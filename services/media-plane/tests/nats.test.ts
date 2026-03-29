import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    natsUrl: 'nats://localhost:4222',
  },
}));

// Build mock NATS connection — vi.hoisted ensures these exist before vi.mock runs
const { mockNc, mockConnect, mockEncode } = vi.hoisted(() => {
  const mockNc = {
    publish: vi.fn(),
    drain: vi.fn().mockResolvedValue(undefined),
    status: vi.fn(() => ({
      async *[Symbol.asyncIterator]() {
        yield { type: 'update', data: {} };
      },
    })),
  };
  const mockConnect = vi.fn().mockResolvedValue(mockNc);
  const mockEncode = vi.fn((data: unknown) => JSON.stringify(data));
  return { mockNc, mockConnect, mockEncode };
});

vi.mock('nats', () => ({
  connect: (...args: any[]) => mockConnect(...args),
  JSONCodec: () => ({ encode: mockEncode }),
}));

import { NatsService } from '../src/lib/nats.js';

describe('NatsService', () => {
  let service: NatsService;

  beforeEach(() => {
    service = new NatsService();
  });

  describe('connect', () => {
    it('connects with correct URL and reconnect options', async () => {
      await service.connect();

      expect(mockConnect).toHaveBeenCalledWith(
        expect.objectContaining({
          servers: 'nats://localhost:4222',
          name: 'media-plane',
          reconnect: true,
          maxReconnectAttempts: -1,
          reconnectTimeWait: 2000,
        })
      );
    });

    it('throws on connection failure', async () => {
      mockConnect.mockRejectedValueOnce(new Error('ECONNREFUSED'));
      await expect(service.connect()).rejects.toThrow('ECONNREFUSED');
    });
  });

  describe('publish', () => {
    it('encodes data and publishes to subject', async () => {
      await service.connect();

      service.publish('voice.joined', { channelId: 'ch-1', userId: 'u-1' });

      expect(mockEncode).toHaveBeenCalledWith({ channelId: 'ch-1', userId: 'u-1' });
      expect(mockNc.publish).toHaveBeenCalledWith(
        'voice.joined',
        expect.anything()
      );
    });

    it('drops message and logs warning when not connected', () => {
      // Don't call connect — nc stays null
      service.publish('voice.joined', { channelId: 'ch-1' });

      expect(mockNc.publish).not.toHaveBeenCalled();
    });

    it('catches publish errors without throwing', async () => {
      await service.connect();
      mockNc.publish.mockImplementationOnce(() => {
        throw new Error('publish error');
      });

      // Should not throw
      expect(() => service.publish('voice.joined', {})).not.toThrow();
    });
  });

  describe('createRoomEventHandler', () => {
    beforeEach(async () => {
      await service.connect();
    });

    it('maps user-joined to voice.joined with correct payload', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'user-joined',
        roomId: 'ch-1',
        userId: 'u-1',
        username: 'alice',
        displayName: 'Alice',
      });

      expect(mockNc.publish).toHaveBeenCalled();
      const encodedData = mockEncode.mock.calls.at(-1)?.[0];
      expect(encodedData).toMatchObject({
        channelId: 'ch-1',
        userId: 'u-1',
        username: 'alice',
        displayName: 'Alice',
      });
      expect(encodedData).toHaveProperty('timestamp');
    });

    it('maps user-left to voice.left', () => {
      const handler = service.createRoomEventHandler();

      handler({ type: 'user-left', roomId: 'ch-1', userId: 'u-1' });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.left');
    });

    it('maps room-empty to voice.room_empty', () => {
      const handler = service.createRoomEventHandler();

      handler({ type: 'room-empty', roomId: 'ch-1' });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.room_empty');
    });

    it('maps producer-added to voice.producer_added', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'producer-added',
        roomId: 'ch-1',
        userId: 'u-1',
        producerId: 'p-1',
        kind: 'audio',
        source: 'mic',
      });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.producer_added');
      const payload = mockEncode.mock.calls.at(-1)?.[0];
      expect(payload).toMatchObject({
        producerId: 'p-1',
        kind: 'audio',
        source: 'mic',
      });
    });

    it('maps producer-removed to voice.producer_removed', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'producer-removed',
        roomId: 'ch-1',
        userId: 'u-1',
        producerId: 'p-1',
        kind: 'video',
        source: 'camera',
      });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.producer_removed');
    });

    it('does NOT publish active-speaker events to NATS', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'active-speaker',
        roomId: 'ch-1',
        userId: 'u-1',
        volume: -30,
      });

      // active-speaker is handled locally via Socket.IO, not NATS
      expect(mockNc.publish).not.toHaveBeenCalled();
    });
  });

  describe('close', () => {
    it('calls drain on the NATS connection', async () => {
      await service.connect();
      await service.close();
      expect(mockNc.drain).toHaveBeenCalled();
    });
  });
});
