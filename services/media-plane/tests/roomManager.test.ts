import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  createMockRouter,
  createMockTransport,
  createMockProducer,
  createMockConsumer,
  createRtpCapabilities,
  createRtpParameters,
} from './mocks/mediasoup.js';
import './mocks/logger.js';

// Mock mediasoup native module
vi.mock('mediasoup', () => ({
  createWorker: vi.fn(),
}));

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    mediasoup: {
      webRtcTransport: {
        listenIps: [{ ip: '0.0.0.0', announcedIp: '127.0.0.1' }],
        enableUdp: true,
        enableTcp: true,
        preferUdp: true,
        initialAvailableOutgoingBitrate: 1_000_000,
        maxIncomingBitrate: 50_000_000,
      },
    },
    audioLevelObserver: {
      maxEntries: 1,
      threshold: -60,
      interval: 300,
    },
  },
}));

import { RoomManager } from '../src/lib/roomManager.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockMediasoupService(router = createMockRouter()) {
  return {
    getOrCreateRouter: vi.fn().mockResolvedValue(router),
    removeRouter: vi.fn(),
  };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('RoomManager', () => {
  let manager: RoomManager;
  let mockRouter: ReturnType<typeof createMockRouter>;
  let mockMediasoup: ReturnType<typeof createMockMediasoupService>;

  beforeEach(() => {
    mockRouter = createMockRouter();
    mockMediasoup = createMockMediasoupService(mockRouter);
    manager = new RoomManager(mockMediasoup as any);
  });

  // ── Room lifecycle ──────────────────────────────────────────────────

  describe('joinRoom', () => {
    it('creates a new room and returns rtpCapabilities', async () => {
      const result = await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      expect(result.rtpCapabilities).toBeDefined();
      expect(result.existingProducers).toEqual([]);
      expect(result.participants).toHaveLength(1);
      expect(result.participants[0].userId).toBe('u-1');
    });

    it('reuses existing room on second join', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      expect(mockMediasoup.getOrCreateRouter).toHaveBeenCalledTimes(1);
      expect(result.participants).toHaveLength(2);
    });

    it('discards stale room when router is closed', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      mockRouter.closed = true;

      // Second join should detect closed router and recreate
      const newRouter = createMockRouter();
      mockMediasoup.getOrCreateRouter.mockResolvedValue(newRouter);

      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result.rtpCapabilities).toBe(newRouter.rtpCapabilities);
    });

    it('returns existing producers from other participants', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      // Add a producer for u-1
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);
      await manager.produce('room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic');

      // u-2 joins and should see u-1's producer
      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result.existingProducers).toHaveLength(1);
      expect(result.existingProducers[0].userId).toBe('u-1');
    });
  });

  describe('joinRoom — reconnection', () => {
    it('cleans up old session when same userId rejoins', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-old', 'alice');
      const result = await manager.joinRoom('room-1', 'u-1', 'sock-new', 'alice');

      // Should have exactly 1 participant (the new session)
      expect(result.participants).toHaveLength(1);
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.socketId).toBe('sock-new');
    });
  });

  describe('leaveRoom', () => {
    it('removes participant and emits user-left event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.participants.size).toBe(1);

      const leftEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'user-left');
      expect(leftEvent).toBeDefined();
      expect(leftEvent![0].userId).toBe('u-1');
    });

    it('closes room and emits room-empty when last participant leaves', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.leaveRoom('room-1', 'u-1');

      expect(manager.getRoom('room-1')).toBeUndefined();
      const emptyEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'room-empty');
      expect(emptyEvent).toBeDefined();
    });

    it('cleans up transports, producers, and consumers', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      // Create send transport + producer
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      await manager.produce('room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic');

      await manager.leaveRoom('room-1', 'u-1');

      expect(producer.close).toHaveBeenCalled();
      expect(transport.close).toHaveBeenCalled();
    });

    it('is a no-op for non-existent room or participant', async () => {
      await manager.leaveRoom('nonexistent', 'u-1');
      expect(manager.getRoom('nonexistent')).toBeUndefined();
    });
  });

  // ── Transport management ────────────────────────────────────────────

  describe('createTransport', () => {
    beforeEach(async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
    });

    it('creates a send transport', async () => {
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      const result = await manager.createTransport('room-1', 'u-1', 'send');

      expect(result.id).toBe(transport.id);
      expect(result.iceParameters).toBeDefined();
      expect(result.dtlsParameters).toBeDefined();
    });

    it('creates a recv transport and adds to recvTransports map', async () => {
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      const result = await manager.createTransport('room-1', 'u-1', 'recv');

      expect(result.id).toBe(transport.id);
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.recvTransports.has(transport.id)).toBe(true);
    });

    it('replaces old send transport if one exists', async () => {
      const oldTransport = createMockTransport();
      const newTransport = createMockTransport();
      mockRouter.createWebRtcTransport
        .mockResolvedValueOnce(oldTransport)
        .mockResolvedValueOnce(newTransport);

      await manager.createTransport('room-1', 'u-1', 'send');
      await manager.createTransport('room-1', 'u-1', 'send');

      expect(oldTransport.close).toHaveBeenCalled();
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.sendTransport?.id).toBe(newTransport.id);
    });

    it('throws if room not found', async () => {
      await expect(
        manager.createTransport('nonexistent', 'u-1', 'send')
      ).rejects.toThrow('Room not found');
    });

    it('throws if participant not found', async () => {
      await expect(
        manager.createTransport('room-1', 'unknown', 'send')
      ).rejects.toThrow('Participant not found');
    });
  });

  describe('connectTransport', () => {
    it('calls transport.connect with dtlsParameters', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const dtls = { fingerprints: [], role: 'auto' as const };
      await manager.connectTransport('room-1', 'u-1', transport.id, dtls as any);

      expect(transport.connect).toHaveBeenCalledWith({ dtlsParameters: dtls });
    });

    it('throws if transport not found', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await expect(
        manager.connectTransport('room-1', 'u-1', 'bad-id', {} as any)
      ).rejects.toThrow('Transport not found');
    });
  });

  // ── Producer management ─────────────────────────────────────────────

  describe('produce', () => {
    let transport: ReturnType<typeof createMockTransport>;

    beforeEach(async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');
    });

    it('creates producer and emits producer-added event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);

      const info = await manager.produce(
        'room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic'
      );

      expect(info.producerId).toBe(producer.id);
      expect(info.source).toBe('mic');
      const addedEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'producer-added');
      expect(addedEvent).toBeDefined();
    });

    it('adds mic audio producer to AudioLevelObserver', async () => {
      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);

      await manager.produce(
        'room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic'
      );

      expect(mockRouter._audioLevelObserver.addProducer).toHaveBeenCalledWith({
        producerId: producer.id,
      });
    });

    it('does NOT add screen-audio to AudioLevelObserver', async () => {
      // First add a screen producer (required for screen-audio)
      const screenProducer = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(screenProducer);
      await manager.produce(
        'room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'screen'
      );

      mockRouter._audioLevelObserver.addProducer.mockClear();

      const screenAudioProducer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(screenAudioProducer);
      await manager.produce(
        'room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'screen-audio'
      );

      expect(mockRouter._audioLevelObserver.addProducer).not.toHaveBeenCalled();
    });

    it('enforces camera limit (max 25)', async () => {
      // Add 25 camera producers across multiple participants
      for (let i = 0; i < 25; i++) {
        const uid = `u-cam-${i}`;
        await manager.joinRoom('room-1', uid, `sock-${i + 100}`, `user${i}`);
        const t = createMockTransport();
        mockRouter.createWebRtcTransport.mockResolvedValueOnce(t);
        await manager.createTransport('room-1', uid, 'send');
        const p = createMockProducer({ kind: 'video' });
        t.produce.mockResolvedValueOnce(p);
        await manager.produce('room-1', uid, t.id, 'video', createRtpParameters() as any, 'camera');
      }

      // 26th should fail
      const p = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(p);
      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'camera')
      ).rejects.toThrow('Video participant limit reached (max 25)');
    });

    it('enforces screen share limit (max 5)', async () => {
      for (let i = 0; i < 5; i++) {
        const uid = `u-scr-${i}`;
        await manager.joinRoom('room-1', uid, `sock-${i + 200}`, `user${i}`);
        const t = createMockTransport();
        mockRouter.createWebRtcTransport.mockResolvedValueOnce(t);
        await manager.createTransport('room-1', uid, 'send');
        const p = createMockProducer({ kind: 'video' });
        t.produce.mockResolvedValueOnce(p);
        await manager.produce('room-1', uid, t.id, 'video', createRtpParameters() as any, 'screen');
      }

      const p = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(p);
      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'screen')
      ).rejects.toThrow('Screen share limit reached (max 5)');
    });

    it('rejects screen-audio when kind is not audio', async () => {
      // Add screen producer first
      const sp = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(sp);
      await manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'screen');

      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'screen-audio')
      ).rejects.toThrow('screen-audio source must be audio kind');
    });

    it('rejects screen-audio when no screen producer exists', async () => {
      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'screen-audio')
      ).rejects.toThrow('screen-audio requires an active screen producer');
    });

    it('rejects duplicate screen-audio', async () => {
      // Screen producer
      const sp = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(sp);
      await manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'screen');

      // First screen-audio
      const sa1 = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(sa1);
      await manager.produce('room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'screen-audio');

      // Second screen-audio should fail
      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'screen-audio')
      ).rejects.toThrow('Only one active screen-audio producer allowed');
    });

    it('throws if send transport does not match', async () => {
      await expect(
        manager.produce('room-1', 'u-1', 'wrong-transport-id', 'audio', createRtpParameters() as any, 'mic')
      ).rejects.toThrow('Send transport not found or mismatch');
    });
  });

  describe('pauseProducer / resumeProducer', () => {
    it('pauses and resumes a producer', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      const info = await manager.produce(
        'room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic'
      );

      await manager.pauseProducer('room-1', 'u-1', info.producerId);
      expect(producer.pause).toHaveBeenCalled();

      await manager.resumeProducer('room-1', 'u-1', info.producerId);
      expect(producer.resume).toHaveBeenCalled();
    });

    it('throws if producer not found', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await expect(
        manager.pauseProducer('room-1', 'u-1', 'bad-id')
      ).rejects.toThrow('Producer not found');
    });
  });

  describe('closeProducer', () => {
    it('closes producer and emits producer-removed event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      const info = await manager.produce(
        'room-1', 'u-1', transport.id, 'audio', createRtpParameters() as any, 'mic'
      );

      const source = await manager.closeProducer('room-1', 'u-1', info.producerId);

      expect(source).toBe('mic');
      expect(producer.close).toHaveBeenCalled();
      const removedEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'producer-removed');
      expect(removedEvent).toBeDefined();
    });

    it('returns null for non-existent room', async () => {
      const result = await manager.closeProducer('nonexistent', 'u-1', 'p-1');
      expect(result).toBeNull();
    });
  });

  // ── Consumer management ─────────────────────────────────────────────

  describe('consume', () => {
    let producerInfo: { producerId: string };

    beforeEach(async () => {
      // u-1 joins and produces
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const sendTransport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(sendTransport);
      await manager.createTransport('room-1', 'u-1', 'send');
      const producer = createMockProducer({ kind: 'audio' });
      sendTransport.produce.mockResolvedValueOnce(producer);
      producerInfo = await manager.produce(
        'room-1', 'u-1', sendTransport.id, 'audio', createRtpParameters() as any, 'mic'
      );

      // u-2 joins with rtpCapabilities and creates recv transport
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob', undefined, undefined, createRtpCapabilities() as any);
      const recvTransport = createMockTransport();
      const consumer = createMockConsumer({ producerId: producerInfo.producerId, kind: 'audio' });
      recvTransport.consume.mockResolvedValue(consumer);
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await manager.createTransport('room-1', 'u-2', 'recv');
    });

    it('creates consumer on recv transport (starts paused)', async () => {
      const result = await manager.consume('room-1', 'u-2', producerInfo.producerId);

      expect(result).not.toBeNull();
      expect(result!.producerUserId).toBe('u-1');
      expect(result!.source).toBe('mic');
    });

    it('returns null when router.canConsume returns false', async () => {
      mockRouter.canConsume.mockReturnValueOnce(false);

      const result = await manager.consume('room-1', 'u-2', producerInfo.producerId);
      expect(result).toBeNull();
    });

    it('throws when no recv transport exists', async () => {
      // u-3 joins without creating recv transport
      await manager.joinRoom('room-1', 'u-3', 'sock-3', 'charlie', undefined, undefined, createRtpCapabilities() as any);

      await expect(
        manager.consume('room-1', 'u-3', producerInfo.producerId)
      ).rejects.toThrow('No receive transport');
    });

    it('throws when rtpCapabilities not set', async () => {
      // u-4 joins WITHOUT rtpCapabilities
      await manager.joinRoom('room-1', 'u-4', 'sock-4', 'dave');
      const recvT = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvT);
      await manager.createTransport('room-1', 'u-4', 'recv');

      await expect(
        manager.consume('room-1', 'u-4', producerInfo.producerId)
      ).rejects.toThrow('RTP capabilities not set');
    });
  });

  describe('resumeConsumer / pauseConsumer / closeConsumer', () => {
    it('resumes a consumer', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const recvTransport = createMockTransport();
      const consumer = createMockConsumer();
      recvTransport.consume.mockResolvedValue(consumer);
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await manager.createTransport('room-1', 'u-1', 'recv');

      // Manually add the consumer to the participant
      const participant = manager.getParticipant('room-1', 'u-1')!;
      participant.consumers.set(consumer.id, consumer as any);

      await manager.resumeConsumer('room-1', 'u-1', consumer.id);
      expect(consumer.resume).toHaveBeenCalled();
    });

    it('pauses a consumer', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer();
      participant.consumers.set(consumer.id, consumer as any);

      await manager.pauseConsumer('room-1', 'u-1', consumer.id);
      expect(consumer.pause).toHaveBeenCalled();
    });

    it('closes a consumer and removes from map', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer();
      participant.consumers.set(consumer.id, consumer as any);

      const result = manager.closeConsumer('room-1', 'u-1', consumer.id);
      expect(result).toBe(true);
      expect(consumer.close).toHaveBeenCalled();
      expect(participant.consumers.has(consumer.id)).toBe(false);
    });

    it('returns false for non-existent consumer', () => {
      const result = manager.closeConsumer('room-1', 'u-1', 'bad-id');
      expect(result).toBe(false);
    });
  });

  // ── Codec floor ─────────────────────────────────────────────────────

  describe('computeCodecFloor', () => {
    it('returns null for non-existent room', () => {
      expect(manager.computeCodecFloor('nonexistent')).toBeNull();
    });

    it('returns null when fewer than 2 participants have capabilities', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      expect(manager.computeCodecFloor('room-1')).toBeNull();
    });

    it('returns intersection of video codecs', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice', undefined, undefined,
        createRtpCapabilities(['video/VP8', 'video/VP9', 'video/H264']) as any);
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob', undefined, undefined,
        createRtpCapabilities(['video/VP8', 'video/H264']) as any);

      const floor = manager.computeCodecFloor('room-1');

      expect(floor).toContain('video/vp8');
      expect(floor).toContain('video/h264');
      expect(floor).not.toContain('video/vp9');
    });

    it('normalizes mimeType to lowercase', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice', undefined, undefined,
        createRtpCapabilities(['video/VP8']) as any);
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob', undefined, undefined,
        createRtpCapabilities(['video/VP8']) as any);

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });

    it('skips participants with null rtpCapabilities', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice', undefined, undefined,
        createRtpCapabilities(['video/VP8']) as any);
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob'); // no capabilities
      await manager.joinRoom('room-1', 'u-3', 'sock-3', 'charlie', undefined, undefined,
        createRtpCapabilities(['video/VP8', 'video/VP9']) as any);

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });
  });

  // ── E2EE epoch ──────────────────────────────────────────────────────

  describe('E2EE epoch', () => {
    it('increments epoch on join when room is encrypted', async () => {
      const result1 = await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice', undefined, undefined, undefined, true);
      expect(result1.e2eeEpoch).toBe(1);

      const result2 = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result2.e2eeEpoch).toBe(2); // encrypted flag already set on room
    });

    it('increments epoch on leave for forward secrecy', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice', undefined, undefined, undefined, true);
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.e2eeEpoch).toBe(3); // 1 (join u-1) + 1 (join u-2) + 1 (leave u-1)
    });
  });

  // ── Query methods ───────────────────────────────────────────────────

  describe('query methods', () => {
    it('findRoomBySocketId returns correct roomId and userId', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const found = manager.findRoomBySocketId('sock-1');
      expect(found).toEqual({ roomId: 'room-1', userId: 'u-1' });
    });

    it('findRoomBySocketId returns null for unknown socket', () => {
      expect(manager.findRoomBySocketId('unknown')).toBeNull();
    });

    it('getRoomSocketIds returns all sockets in room', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      const ids = manager.getRoomSocketIds('room-1');
      expect(ids).toContain('sock-1');
      expect(ids).toContain('sock-2');
    });

    it('getStats returns correct aggregate counts', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

      const stats = manager.getStats();
      expect(stats.activeRooms).toBe(2);
      expect(stats.totalParticipants).toBe(2);
      expect(stats.totalProducers).toBe(0);
      expect(stats.totalConsumers).toBe(0);
    });

    it('getActiveRoomIds returns all room IDs', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

      expect(manager.getActiveRoomIds()).toEqual(['room-1', 'room-2']);
    });
  });

  // ── Event system ────────────────────────────────────────────────────

  describe('event system', () => {
    it('calls all registered handlers', async () => {
      const handler1 = vi.fn();
      const handler2 = vi.fn();
      manager.onEvent(handler1);
      manager.onEvent(handler2);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      expect(handler1).toHaveBeenCalled();
      expect(handler2).toHaveBeenCalled();
    });

    it('catches handler errors without breaking other handlers', async () => {
      const badHandler = vi.fn(() => { throw new Error('boom'); });
      const goodHandler = vi.fn();
      manager.onEvent(badHandler);
      manager.onEvent(goodHandler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      expect(badHandler).toHaveBeenCalled();
      expect(goodHandler).toHaveBeenCalled();
    });
  });

  // ── updateRtpCapabilities ───────────────────────────────────────────

  describe('updateRtpCapabilities', () => {
    it('updates participant capabilities', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const caps = createRtpCapabilities();

      manager.updateRtpCapabilities('room-1', 'u-1', caps as any);

      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.rtpCapabilities).toBe(caps);
    });

    it('is a no-op for non-existent room', () => {
      manager.updateRtpCapabilities('nonexistent', 'u-1', {} as any);
      expect(manager.getParticipant('nonexistent', 'u-1')).toBeUndefined();
    });
  });

  // ── closeAll ────────────────────────────────────────────────────────

  describe('closeAll', () => {
    it('closes all rooms', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

      await manager.closeAll();

      expect(manager.getActiveRoomIds()).toEqual([]);
    });
  });
});
