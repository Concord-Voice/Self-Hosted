import { afterEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import {
  IDLE_JOIN_GRACE_MS, IDLE_RELEASE_MS, IDLE_SWEEP_INTERVAL_MS, VoiceAuthConnections,
} from '../src/lib/voiceAuthConnection.js';
import type { ConnectionHandle } from '../src/lib/voiceAuthConnection.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';

// VULN-146 idle release. The clock moves, so every signed response is re-signed at
// the current time (the protocol rejects skew over 30 s) and tokens outlive the case.
const boot = '11111111-1111-4111-8111-111111111111';
const user = '22222222-2222-4222-8222-222222222222';
const family = '33333333-3333-4333-8333-333333333333';
const roomId = '44444444-4444-4444-8444-444444444444';
const epoch = 'a'.repeat(32);
const secret = ['idle', 'release', 'test', 'only'].join('-');
const key = Buffer.from(secret);
const start = 1_800_000_000_000;
const token = 'test-idle-token';
const noNative = async () => {};

function deferred<T>() {
  let resolve: (value: T) => void = () => { throw new Error('Uninitialized deferred'); };
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
async function settledTurn() { await new Promise<void>(resolve => setImmediate(resolve)); }

interface Exchange { request: protocol.Request }
function fixture() {
  let now = start;
  const calls: Exchange[] = [];
  const response = (exchange: Exchange, unsigned = false): Response => {
    const request = exchange.request;
    const register = request.connectionRegister();
    const room = request.roomRegister();
    const provisional = request.provisionalClose();
    const target = register ?? room;
    const binding = target ? { userId: user, familyId: family, target: {
      connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
      socketId: target.socketId, credentialEpoch: target.credentialEpoch,
    } } : undefined;
    const payload: protocol.ResponsePayload = register && binding
      ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
      : room && binding ? { schema: 'concord.auth-room-binding', version: 1,
        voiceEnforcementProtocol: 4, binding, room: {
          sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind,
        } }
        : provisional ? { ...provisional, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' }
          : null;
    const signed = protocol.signHttpResponse(request, payload, String(Math.floor(now / 1000)), key);
    return new Response(signed.body.length ? Buffer.from(signed.body) : null, {
      status: signed.status, headers: unsigned ? [] : signed.headers.map(({ name, value }) => [name, value]),
    });
  };
  let answer: (exchange: Exchange) => Response | Promise<Response> = exchange => response(exchange);
  const boundary = vi.fn<typeof fetch>(async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
      : path.endsWith('/auth-connections/release') ? 'connection_release'
        : path.endsWith('/provisional-close') ? 'provisional_close'
          : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
    const body = init?.body;
    if (typeof body !== 'string' && !(body instanceof Uint8Array)) throw new Error('Missing raw request');
    const request = protocol.decodeHttpRequest(endpoint, {
      method: init?.method ?? '', path, body: Buffer.from(body),
      headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })),
    });
    protocol.verifyHttpRequest(request, key, BigInt(Math.floor(now / 1000)));
    const exchange = { request };
    calls.push(exchange);
    return answer(exchange);
  });
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret,
    controlPlaneUrl: 'https://control.test.invalid', now: () => now, fetch: boundary });
  const disconnects = new Map<string, ReturnType<typeof vi.fn>>();
  const reserve = (socketId: string) => {
    const disconnect = vi.fn(); disconnects.set(socketId, disconnect);
    return owners.reserve({ socketId, userId: user, credentialEpoch: epoch, token,
      tokenExpiresAt: start + 3_600_000, disconnect });
  };
  const session = (owner: ConnectionHandle, index: number): VoiceEnforcementSession => ({
    sessionGeneration: `${index.toString(16).padStart(8, '0')}-5555-4555-8555-555555555555`,
    connectionGeneration: owners.snapshot(owner).target.connectionGeneration,
    nodeBootId: boot, roomId, roomKind: 'channel', userId: user, credentialEpoch: epoch,
    socketId: owners.snapshot(owner).target.socketId,
  });
  const released = (endpoint: protocol.Endpoint) => calls.filter(call => call.request.endpoint() === endpoint);
  return { owners, reserve, session, calls, disconnects, response, released,
    advance(ms: number) { now += ms; },
    answer(handler: typeof answer) { answer = handler; } };
}

afterEach(() => { vi.restoreAllMocks(); });

describe('idle voice identity release', () => {
  it('exports the idle limits', () => {
    expect([IDLE_RELEASE_MS, IDLE_JOIN_GRACE_MS, IDLE_SWEEP_INTERVAL_MS]).toEqual([30_000, 30_000, 5_000]);
  });

  it('at 30 s terminateIdle goes through terminate() and retainedIdentityCount drops only after the signed release', async () => {
    const f = fixture();
    const registered = f.reserve('idle-registered'); await f.owners.register(registered);
    const provisional = f.reserve('idle-provisional');
    const release = deferred<void>();
    f.answer(exchange => exchange.request.endpoint() === 'connection_release'
      ? release.promise.then(() => f.response(exchange)) : f.response(exchange));
    const terminate = vi.spyOn(f.owners, 'terminate');
    f.advance(29_999); f.owners.terminateIdle();
    expect(terminate).not.toHaveBeenCalled();
    expect(() => f.owners.assertProductive(registered)).not.toThrow();
    f.advance(1); f.owners.terminateIdle();
    expect(terminate.mock.calls).toEqual([[registered], [provisional]]);
    expect(f.disconnects.get('idle-registered')).toHaveBeenCalledOnce();
    expect(f.disconnects.get('idle-provisional')).toHaveBeenCalledOnce();
    expect(f.owners.snapshot(registered).state).toBe('terminal');
    await settledTurn();
    expect(f.released('provisional_close')).toHaveLength(1);
    expect(f.released('connection_release')).toHaveLength(1);
    expect(f.owners.retainedIdentityCount()).toBe(1);
    release.resolve(); await f.owners.terminate(registered);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('a room in flight suspends release below 60 s and not at 60 s', async () => {
    const f = fixture();
    const owner = f.reserve('joining'); await f.owners.register(owner);
    // Reserved and never attached: the join is still in flight.
    f.owners.reserveRoom(owner, f.session(owner, 1), noNative);
    f.advance(30_000); f.owners.terminateIdle();
    expect(f.disconnects.get('joining')).not.toHaveBeenCalled();
    f.advance(29_999); f.owners.terminateIdle();
    expect(f.disconnects.get('joining')).not.toHaveBeenCalled();
    f.advance(1); f.owners.terminateIdle();
    expect(f.disconnects.get('joining')).toHaveBeenCalledOnce();
    await f.owners.terminate(owner);
    expect(f.released('room_release')).toHaveLength(1);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('a failed join does not reset the clock', async () => {
    const f = fixture();
    const owner = f.reserve('failed-join'); await f.owners.register(owner);
    f.advance(20_000);
    const room = f.session(owner, 1);
    f.owners.reserveRoom(owner, room, noNative);
    f.answer(exchange => f.response(exchange, exchange.request.endpoint() === 'room_register'));
    await expect(f.owners.attachRoom(owner, room, token)).rejects.toThrow();
    f.answer(exchange => f.response(exchange));
    await f.owners.releaseRoom(room);
    expect(f.owners.retainedIdentityCount()).toBe(1);
    f.advance(9_999); f.owners.terminateIdle();
    expect(f.disconnects.get('failed-join')).not.toHaveBeenCalled();
    f.advance(1); f.owners.terminateIdle();
    expect(f.disconnects.get('failed-join')).toHaveBeenCalledOnce();
  });

  it("an overdue connection's reserveRoom throws Voice connection idle", async () => {
    const f = fixture();
    const owner = f.reserve('overdue'); await f.owners.register(owner);
    f.advance(29_999);
    expect(() => f.owners.reserveRoom(owner, f.session(owner, 1), noNative)).not.toThrow();
    f.advance(1);
    expect(() => f.owners.reserveRoom(owner, f.session(owner, 2), noNative)).toThrow(/^Voice connection idle$/);
    expect(f.owners.retainedIdentityCount()).toBe(2);
  });

  it('the clock restarts when the last attached room is released', async () => {
    const f = fixture();
    const owner = f.reserve('attached'); await f.owners.register(owner);
    const first = f.session(owner, 1);
    const second = f.session(owner, 2);
    for (const room of [first, second]) {
      f.owners.reserveRoom(owner, room, noNative);
      await f.owners.attachRoom(owner, room, token);
    }
    let failSecond = true;
    f.answer(exchange => {
      if (failSecond && exchange.request.roomRelease()?.room.sessionGeneration === second.sessionGeneration)
        throw new Error('Room release unavailable');
      return f.response(exchange);
    });
    f.advance(100_000);
    await f.owners.releaseRoom(first);
    // Another attached room remains, so the first release starts no clock.
    f.advance(100_000); f.owners.terminateIdle();
    expect(f.disconnects.get('attached')).not.toHaveBeenCalled();
    void f.owners.releaseRoom(second);
    await settledTurn();
    // A retry of the same release does not restart the clock.
    f.advance(20_000); void f.owners.releaseRoom(second);
    f.advance(9_999); f.owners.terminateIdle();
    expect(f.disconnects.get('attached')).not.toHaveBeenCalled();
    f.advance(1); f.owners.terminateIdle();
    expect(f.disconnects.get('attached')).toHaveBeenCalledOnce();
    failSecond = false;
    await f.owners.retryPending(); await f.owners.terminate(owner);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('never idles a connection with an attached room', async () => {
    const f = fixture();
    const owner = f.reserve('in-room'); await f.owners.register(owner);
    f.advance(10_000);
    const room = f.session(owner, 1);
    f.owners.reserveRoom(owner, room, noNative);
    await f.owners.attachRoom(owner, room, token);
    f.advance(600_000); f.owners.terminateIdle();
    expect(f.disconnects.get('in-room')).not.toHaveBeenCalled();
    expect(() => f.owners.assertRoomProductive(room.sessionGeneration)).not.toThrow();
  });

  it('does nothing while draining', async () => {
    const f = fixture();
    const close = deferred<void>();
    f.answer(exchange => exchange.request.provisionalClose()?.target.socketId === 'draining'
      ? close.promise.then(() => f.response(exchange)) : f.response(exchange));
    f.reserve('draining');
    const drain = f.owners.terminateAll();
    // Reserved after the drain captured its set, so only the sweep could release it.
    f.reserve('late');
    f.advance(30_000); f.owners.terminateIdle();
    expect(f.disconnects.get('late')).not.toHaveBeenCalled();
    close.resolve(); await drain;
    f.owners.terminateIdle();
    expect(f.disconnects.get('late')).toHaveBeenCalledOnce();
  });

  it('a synchronous re-entrant disconnect joins the existing terminal task', async () => {
    const f = fixture();
    const owner = f.reserve('reentrant'); await f.owners.register(owner);
    let nested: Promise<void> | undefined;
    f.disconnects.get('reentrant')?.mockImplementation(() => { nested = f.owners.terminate(owner); });
    f.advance(30_000); f.owners.terminateIdle();
    expect(f.disconnects.get('reentrant')).toHaveBeenCalledOnce();
    expect(nested).toBeDefined();
    expect(f.owners.terminate(owner)).toBe(nested);
    await nested;
    expect(f.released('connection_release')).toHaveLength(1);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });
});
