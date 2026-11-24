/**
 * VULN-159: a socket displaced by a second same-account join must be
 * disconnected for every room kind, so it stops receiving room broadcasts.
 *
 * Real pieces: a Socket.IO 4.8 server on a loopback HTTP server, raw
 * Engine.IO v4 WebSocket clients (`ws`, already installed through engine.io),
 * a real RoomManager over mocked mediasoup, and the production commit
 * functions. The handshake middleware is replaced by a query `uid`, because
 * identity is not what this finding is about.
 */
import { createServer, type Server as HttpServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Server, type Socket } from 'socket.io';
import WebSocket from 'ws';
import './mocks/logger.js';
import { logger } from '../src/lib/logger.js';
import { createMockRouter, createMockWorker } from './mocks/mediasoup.js';
import {
  FREE_MEDIA_ENTITLEMENT,
  RoomManager,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
} from '../src/lib/roomManager.js';
import type { JoinRoomResult } from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import type { AuthenticatedSocketData } from '../src/middleware/auth.js';
import {
  commitChannelAdmission,
  commitDMAdmission,
  evictDisplacedSocket,
  rollbackSocketMembership,
} from '../src/lib/roomAdmissionCommit.js';
import type { RoomJoinValue } from '../src/lib/roomAdmissionCommit.js';

const DM_ROOM = 'dm-3c1a7e2b-0f4d-4e8a-9b6c-5d4e3f2a1b0c';
const CHANNEL_ROOM = '6f1c2d3e-4b5a-4c6d-8e7f-901234567890';
const CALL = '24070000-0000-4000-8000-000000000159';
const ALICE = 'a11ce000-0000-4000-8000-000000000001';
const BOB = 'b0b00000-0000-4000-8000-000000000002';
const GEN_FIRST = '10000000-0000-4000-8000-000000000001';
const GEN_SECOND = '20000000-0000-4000-8000-000000000002';
const GEN_BOB = '30000000-0000-4000-8000-000000000003';

type Kind = 'dm' | 'channel';
const KINDS: Array<{ kind: Kind; roomId: string }> = [
  { kind: 'dm', roomId: DM_ROOM },
  { kind: 'channel', roomId: CHANNEL_ROOM },
];

function mockMediasoup(): MediasoupService {
  const router = createMockRouter();
  const worker = createMockWorker();
  return {
    getOrCreateRouter: vi.fn().mockResolvedValue(router),
    getWebRtcServer: vi.fn().mockReturnValue({ closed: false }),
    removeRouter: vi.fn(),
    captureNativeTermination: vi.fn((r: { id: string }) => ({ worker, router: r, routerId: r.id })),
    confirmNativeTermination: vi.fn().mockResolvedValue(undefined),
    confirmAllRoutersClosed: vi.fn().mockResolvedValue(undefined),
  } as unknown as MediasoupService;
}

/** Minimal Socket.IO v4 client over a raw Engine.IO WebSocket. */
class RawClient {
  readonly closed: Promise<void>;
  private readonly ws: WebSocket;
  private readonly ready: Promise<string>;

  constructor(url: string, userId: string) {
    this.ws = new WebSocket(`${url}/socket.io/?EIO=4&transport=websocket&uid=${userId}`);
    this.ready = new Promise((resolve, reject) => {
      this.ws.on('error', reject);
      this.ws.on('message', (raw) => {
        const frame = raw.toString();
        if (frame.startsWith('0')) this.ws.send('40');
        else if (frame === '2') this.ws.send('3');
        else if (frame.startsWith('40')) resolve((JSON.parse(frame.slice(2)) as { sid: string }).sid);
      });
    });
    this.closed = new Promise((resolve) => this.ws.on('close', () => resolve()));
  }

  open(): Promise<string> {
    return this.ready;
  }

  close(): void {
    this.ws.close();
  }
}

function within<T>(promise: Promise<T>, label: string, ms = 2_000): Promise<T> {
  return Promise.race([
    promise,
    new Promise<T>((_, reject) => setTimeout(() => reject(new Error(`timed out: ${label}`)), ms)),
  ]);
}

describe('room admission commit evicts the displaced socket (VULN-159)', () => {
  let http: HttpServer;
  let io: Server;
  let rm: RoomManager;
  let url: string;
  const clients: RawClient[] = [];

  beforeEach(async () => {
    rm = new RoomManager(mockMediasoup());
    http = createServer();
    io = new Server(http, { transports: ['websocket'] });
    io.use((socket, next) => {
      socket.data.userId = String(socket.handshake.query.uid);
      next();
    });
    await new Promise<void>((resolve) => http.listen(0, '127.0.0.1', () => resolve()));
    url = `ws://127.0.0.1:${(http.address() as AddressInfo).port}`;
  });

  afterEach(async () => {
    for (const client of clients.splice(0)) client.close();
    io.close();
    await new Promise<void>((resolve) => http.close(() => resolve()));
  });

  async function connect(userId: string): Promise<{ client: RawClient; socket: Socket }> {
    const client = new RawClient(url, userId);
    clients.push(client);
    const sid = await within(client.open(), 'socket connect');
    const socket = io.sockets.sockets.get(sid);
    if (!socket) throw new Error('server socket missing for connected client');
    return { client, socket };
  }

  function dataOf(socket: Socket): AuthenticatedSocketData {
    return socket.data as AuthenticatedSocketData;
  }

  function membership(socket: Socket, roomId: string): () => void {
    return () => {
      socket.join(roomId);
      dataOf(socket).roomId = roomId;
    };
  }

  function identityOf(socket: Socket) {
    return { username: dataOf(socket).userId.slice(0, 5) };
  }

  async function stage(kind: Kind, roomId: string, socket: Socket, generation: string): Promise<void> {
    await rm.joinRoom(roomId, dataOf(socket).userId, socket.id, identityOf(socket), undefined, {
      credentialEpoch: '',
      voiceEnforcementSessionGeneration: generation,
      entitlement: { ...FREE_MEDIA_ENTITLEMENT },
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      ...(kind === 'dm'
        ? { roomContext: { roomKind: 'dm' as const, callId: CALL } }
        : {
            roomContext: { roomKind: 'channel' as const, ownerTier: 'free' },
            permissions: 0n,
            deferChannelPromotion: true,
          }),
    });
  }

  function valueFor(kind: Kind, socket: Socket): RoomJoinValue {
    const identity = identityOf(socket);
    const shared = {
      identity,
      entitlement: { ...FREE_MEDIA_ENTITLEMENT },
      serverMuted: false,
      serverDeafened: false,
    };
    return {
      authorizedCallId: kind === 'dm' ? CALL : undefined,
      identity,
      promotion: kind === 'dm' ? { ...shared, callId: CALL } : { ...shared, ownerTier: 'free', permissions: 0n },
      result: undefined as unknown as JoinRoomResult,
    };
  }

  function commit(kind: Kind, roomId: string, socket: Socket, commitMembership?: () => void): RoomJoinValue {
    const run = kind === 'dm' ? commitDMAdmission : commitChannelAdmission;
    return run(rm, socket, dataOf(socket), roomId, valueFor(kind, socket), commitMembership ?? membership(socket, roomId));
  }

  async function admit(kind: Kind, roomId: string, socket: Socket, generation: string): Promise<RoomJoinValue> {
    await stage(kind, roomId, socket, generation);
    return commit(kind, roomId, socket);
  }

  function inRoom(roomId: string, socket: Socket): boolean {
    return io.sockets.adapter.rooms.get(roomId)?.has(socket.id) ?? false;
  }

  /** Alice's first socket and Bob are admitted; Alice's second socket is connected. */
  async function displacementFixture(kind: Kind, roomId: string) {
    const first = await connect(ALICE);
    await admit(kind, roomId, first.socket, GEN_FIRST);
    const bob = await connect(BOB);
    await admit(kind, roomId, bob.socket, GEN_BOB);
    const second = await connect(ALICE);
    // Preconditions: the fixture reached the state the eviction acts on.
    expect(rm.getParticipant(roomId, ALICE)?.socketId).toBe(first.socket.id);
    expect(inRoom(roomId, first.socket)).toBe(true);
    expect(first.socket.connected).toBe(true);
    return { first, bob, second };
  }

  it.each(KINDS)(
    'a second $kind join disconnects the first socket and removes it from adapter.rooms',
    async ({ kind, roomId }) => {
      const { first, bob, second } = await displacementFixture(kind, roomId);

      await admit(kind, roomId, second.socket, GEN_SECOND);

      expect(first.socket.connected, 'displaced socket must be disconnected').toBe(false);
      expect(inRoom(roomId, first.socket), 'displaced socket must leave the Socket.IO room').toBe(false);
      expect(rm.getParticipant(roomId, ALICE)?.socketId).toBe(second.socket.id);
      expect(inRoom(roomId, second.socket)).toBe(true);
      expect(inRoom(roomId, bob.socket)).toBe(true);
      expect(bob.socket.connected).toBe(true);
      // disconnect(true) closes the underlying connection, not only the namespace.
      await within(first.client.closed, 'displaced transport close');
    }
  );

  it.each(KINDS)('a failed $kind promotion evicts nothing', async ({ kind, roomId }) => {
    const { first, second } = await displacementFixture(kind, roomId);
    await stage(kind, roomId, second.socket, GEN_SECOND);

    expect(() =>
      commit(kind, roomId, second.socket, () => {
        second.socket.join(roomId);
        dataOf(second.socket).roomId = roomId;
        throw new Error('adapter failure');
      })
    ).toThrow('adapter failure');

    expect(first.socket.connected).toBe(true);
    expect(inRoom(roomId, first.socket)).toBe(true);
    expect(rm.getParticipant(roomId, ALICE)?.socketId).toBe(first.socket.id);
    // The failed successor is rolled back.
    expect(dataOf(second.socket).roomId).toBeUndefined();
    expect(inRoom(roomId, second.socket)).toBe(false);
  });

  it('a same-socket DM re-commit does not evict itself', async () => {
    const first = await connect(ALICE);
    await admit('dm', DM_ROOM, first.socket, GEN_FIRST);
    const bob = await connect(BOB);
    await admit('dm', DM_ROOM, bob.socket, GEN_BOB);

    // Idempotent replay for the already-promoted socket and call.
    commit('dm', DM_ROOM, first.socket);

    expect(first.socket.connected).toBe(true);
    expect(inRoom(DM_ROOM, first.socket)).toBe(true);
    expect(rm.getParticipant(DM_ROOM, ALICE)?.socketId).toBe(first.socket.id);
  });

  it.each(KINDS)(
    "the displaced socket's cleanup keeps the successor's $kind participant, session generation and room membership",
    async ({ kind, roomId }) => {
      const { first, second } = await displacementFixture(kind, roomId);
      // The displaced socket's own disconnect handler ends in the socket-owned
      // RoomManager leave; it runs synchronously inside disconnect(true).
      const cleanups: Array<Promise<boolean>> = [];
      first.socket.on('disconnect', () => {
        cleanups.push(rm.leaveRoomIfSocketOwned(roomId, ALICE, first.socket.id, GEN_FIRST));
      });

      await admit(kind, roomId, second.socket, GEN_SECOND);

      expect(cleanups, 'the displaced socket must run its own cleanup').toHaveLength(1);
      await expect(cleanups[0]).resolves.toBe(false);
      const participant = rm.getParticipant(roomId, ALICE);
      expect(participant?.socketId).toBe(second.socket.id);
      expect(participant?.voiceEnforcementSessionGeneration).toBe(GEN_SECOND);
      expect(inRoom(roomId, second.socket)).toBe(true);
      expect(dataOf(second.socket).roomId).toBe(roomId);
    }
  );

  it.each(KINDS)(
    'a throwing $kind disconnect is logged with ids only and the promotion stands',
    async ({ kind, roomId }) => {
      const { first, second } = await displacementFixture(kind, roomId);
      first.socket.on('disconnect', () => {
        throw new Error('listener exploded with client text');
      });
      await stage(kind, roomId, second.socket, GEN_SECOND);

      expect(() => commit(kind, roomId, second.socket)).not.toThrow();

      expect(logger.error).toHaveBeenCalledWith('Failed to disconnect displaced socket', {
        roomId,
        displacedSocketId: first.socket.id,
      });
      for (const call of vi.mocked(logger.error).mock.calls) {
        expect(JSON.stringify(call)).not.toContain('exploded');
        expect(call.some((arg) => arg instanceof Error)).toBe(false);
      }
      expect(rm.getParticipant(roomId, ALICE)?.socketId).toBe(second.socket.id);
      expect(inRoom(roomId, second.socket)).toBe(true);
      expect(dataOf(second.socket).roomId).toBe(roomId);
    }
  );
});

describe('evictDisplacedSocket (VULN-159)', () => {
  function fakeSocket(onDisconnect?: () => void) {
    return { disconnect: vi.fn((_close?: boolean) => onDisconnect?.()) };
  }

  it('disconnects the displaced socket with close=true and reports it', () => {
    const displaced = fakeSocket();
    const sockets = new Map([['old', displaced]]);
    expect(evictDisplacedSocket(sockets, 'old', 'new', { roomId: DM_ROOM })).toBe(true);
    expect(displaced.disconnect).toHaveBeenCalledExactlyOnceWith(true);
  });

  it('does nothing without a displaced socket, for the current socket, or for a socket already gone', () => {
    const current = fakeSocket();
    const sockets = new Map([['same', current]]);
    expect(evictDisplacedSocket(sockets, undefined, 'same', { roomId: DM_ROOM })).toBe(false);
    expect(evictDisplacedSocket(sockets, 'same', 'same', { roomId: DM_ROOM })).toBe(false);
    expect(evictDisplacedSocket(sockets, 'gone', 'same', { roomId: DM_ROOM })).toBe(false);
    expect(current.disconnect).not.toHaveBeenCalled();
  });

  it('never throws and logs only ids when disconnect throws', () => {
    const displaced = fakeSocket(() => {
      throw new Error('boom with client text');
    });
    const sockets = new Map([['old', displaced]]);
    expect(evictDisplacedSocket(sockets, 'old', 'new', { roomId: DM_ROOM })).toBe(false);
    expect(logger.error).toHaveBeenCalledExactlyOnceWith('Failed to disconnect displaced socket', {
      roomId: DM_ROOM,
      displacedSocketId: 'old',
    });
  });
});

describe('rollbackSocketMembership', () => {
  it('clears the room id and leaves the room, logging a failed leave without throwing', () => {
    const leave = vi.fn(() => {
      throw new Error('adapter failure');
    });
    const data = { userId: ALICE, roomId: DM_ROOM } as AuthenticatedSocketData;
    expect(() => rollbackSocketMembership({ leave } as unknown as Socket, data, DM_ROOM)).not.toThrow();
    expect(data.roomId).toBeUndefined();
    expect(leave).toHaveBeenCalledWith(DM_ROOM);
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to roll back Socket.IO room membership',
      expect.objectContaining({ roomId: DM_ROOM, userId: ALICE })
    );
  });
});
