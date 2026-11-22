import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { selectCryptoScheme } from '@/renderer/workers/e2eeProtocol';

const {
  mockDecryptFrame,
  mockEncryptFrame,
  mockInitFromKey,
  mockSetKeyVersion,
  mockAddDecryptKeyDirectV3,
} = vi.hoisted(() => ({
  mockDecryptFrame: vi.fn(),
  mockEncryptFrame: vi.fn(),
  mockInitFromKey: vi.fn(),
  mockSetKeyVersion: vi.fn(),
  mockAddDecryptKeyDirectV3: vi.fn(),
}));

vi.mock('@/renderer/services/e2ee/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 6,
  MediaEncryption: class {
    initFromKey = mockInitFromKey;
    setKeyVersion = mockSetKeyVersion;
    addDecryptKeyDirectV3 = mockAddDecryptKeyDirectV3;
    rotateKeys = vi.fn().mockResolvedValue(undefined);
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    encryptFrame = mockEncryptFrame;
    decryptFrame = mockDecryptFrame;
    destroy = vi.fn();
  },
}));

describe('e2eeWorker codec dispatch', () => {
  it('selects known codec schemes and refuses absent or unknown codecs', () => {
    expect(selectCryptoScheme('h264')).toBe('nal-aware');
    expect(selectCryptoScheme('av1')).toBe('per-obu');
    expect(selectCryptoScheme('vp8')).toBe('whole-frame');
    expect(selectCryptoScheme('vp9')).toBe('whole-frame');
    expect(selectCryptoScheme('opus')).toBe('whole-frame');
    expect(selectCryptoScheme(undefined)).toBeUndefined();
    expect(selectCryptoScheme('h265' as never)).toBeUndefined();
  });
});

describe('e2eeWorker keyframe recovery', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let rtctransformListener: ((event: { transformer: unknown }) => void) | undefined;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    mockEncryptFrame.mockResolvedValue(undefined);
    mockDecryptFrame.mockResolvedValue(undefined);
    postMessage = vi.fn();
    rtctransformListener = undefined;
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn((event: string, listener: EventListener) => {
        if (event === 'rtctransform') {
          rtctransformListener = listener as unknown as (event: { transformer: unknown }) => void;
        }
      }),
    });
    await import('@/renderer/workers/e2eeWorker');
  });

  it('binds init version and installs decrypt keys by the complete tuple', () => {
    const onmessage = (
      self as unknown as { onmessage: ((event: { data: unknown }) => void) | null }
    ).onmessage;
    expect(onmessage).toBeTypeOf('function');
    if (!onmessage) throw new Error('worker message handler missing');
    const key = {} as CryptoKey;
    onmessage({ data: { type: 'init', encryptKey: key, currentKeyId: 3, keyVersion: 7 } });
    expect(mockInitFromKey).toHaveBeenCalledWith(key, 3);
    expect(mockSetKeyVersion).toHaveBeenCalledWith(7);
    onmessage({
      data: { type: 'addDecryptKey', senderUserId: 'sender', keyVersion: 7, keyId: 3, key },
    });
    expect(mockAddDecryptKeyDirectV3).toHaveBeenCalledWith('sender', 7, 3, key);
  });

  it('passes h264 to the shared MediaEncryption encrypt implementation', async () => {
    const frame = { type: 'key', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];
    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(frame);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'encrypt', codecFamily: 'h264' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() => expect(mockEncryptFrame).toHaveBeenCalledWith(frame, 'h264'));
    await vi.waitFor(() => expect(written).toEqual([frame]));
  });

  it('passes h264 to the shared MediaEncryption decrypt implementation', async () => {
    const frame = { type: 'key', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];
    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(frame);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1', codecFamily: 'h264' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() =>
      expect(mockDecryptFrame).toHaveBeenCalledWith(frame, 'sender-1', 'h264')
    );
    await vi.waitFor(() => expect(written).toEqual([frame]));
  });

  it('posts requestKeyframe when video decrypt recovers after dropped frames', async () => {
    const dropped = { type: 'key', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const recovered = { type: 'delta', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];

    mockDecryptFrame
      .mockRejectedValueOnce(new DOMException('', 'OperationError'))
      .mockResolvedValueOnce(undefined);

    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(dropped);
        controller.enqueue(recovered);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() => {
      expect(postMessage).toHaveBeenCalledWith({
        type: 'requestKeyframe',
        senderUserId: 'sender-1',
      });
    });
    expect(written).toEqual([recovered]);
  });
});

describe('e2eeWorker key-miss observability', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let rtctransformListener: ((event: { transformer: unknown }) => void) | undefined;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    mockEncryptFrame.mockResolvedValue(undefined);
    postMessage = vi.fn();
    rtctransformListener = undefined;
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn((event: string, listener: EventListener) => {
        if (event === 'rtctransform') {
          rtctransformListener = listener as unknown as (event: { transformer: unknown }) => void;
        }
      }),
    });
    await import('@/renderer/workers/e2eeWorker');
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  /**
   * A FrameKeyMissError used to drop the frame in total silence — no success
   * log, no drop warning — so a receiver missing every key looked identical in
   * the console to one decrypting cleanly. That cost two investigations (#1742
   * hid in the passthrough branch, this one in the key-miss branch).
   */
  it('logs a warning naming the wanted (keyVersion, keyId) on a key miss', async () => {
    const miss = Object.assign(new Error('E2EE: no decrypt key for sender=sender-1 v=1 keyId=7'), {
      name: 'FrameKeyMissError',
      senderUserId: 'sender-1',
      keyVersion: 1,
      keyId: 7,
    });
    mockDecryptFrame.mockRejectedValue(miss);

    const frame = { type: 'key', data: new ArrayBuffer(64) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];
    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(frame);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() => {
      expect(postMessage).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'log',
          level: 'warn',
          message: expect.stringContaining('no key for sender-1'),
          data: expect.objectContaining({ wantKeyVersion: 1, wantKeyId: 7 }),
        })
      );
    });

    // Still fail-closed: an undecryptable frame is never forwarded to the decoder.
    expect(written).toEqual([]);
  });
});

describe('e2eeWorker decrypt entered-frame counter (bypass probe)', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let rtctransformListener: ((event: { transformer: unknown }) => void) | undefined;
  let workerOnMessage: ((event: { data: unknown }) => void) | null;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    postMessage = vi.fn();
    rtctransformListener = undefined;
    const stub = {
      postMessage,
      onmessage: null as ((event: { data: unknown }) => void) | null,
      addEventListener: vi.fn((event: string, listener: EventListener) => {
        if (event === 'rtctransform') {
          rtctransformListener = listener as unknown as (event: { transformer: unknown }) => void;
        }
      }),
    };
    vi.stubGlobal('self', stub);
    await import('@/renderer/workers/e2eeWorker');
    workerOnMessage = stub.onmessage;
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  /**
   * The bypass probe pairs receiver.getStats() with this counter: packets
   * arriving while entered === 0 is the signature of ciphertext reaching the
   * decoder around the transform (2026-08-21 incident). The counter must tick
   * on ENTRY — before any decrypt outcome — so a failing decrypt still counts.
   */
  it('counts entries per probeId and answers queryDecryptStats', async () => {
    const miss = Object.assign(new Error('no key'), {
      name: 'FrameKeyMissError',
      senderUserId: 'sender-1',
      keyVersion: 1,
      keyId: 3,
    });
    mockDecryptFrame.mockRejectedValue(miss);

    const frames = [
      { type: 'key', data: new ArrayBuffer(64) },
      { type: 'delta', data: new ArrayBuffer(64) },
    ] as RTCEncodedVideoFrame[];
    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        for (const f of frames) controller.enqueue(f);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({ write() {} });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1', probeId: 'consumer-42' },
        readable,
        writable,
      },
    });

    const { decryptEnteredCounts } = await import('@/renderer/workers/e2eeWorker');
    await vi.waitFor(() => {
      expect(decryptEnteredCounts.get('consumer-42')).toBe(2);
    });

    expect(workerOnMessage).toBeTypeOf('function');
    workerOnMessage!({ data: { type: 'queryDecryptStats', probeId: 'consumer-42' } });
    expect(postMessage).toHaveBeenCalledWith({
      type: 'decryptStats',
      probeId: 'consumer-42',
      entered: 2,
    });

    // An unknown probeId answers 0, never throws.
    workerOnMessage!({ data: { type: 'queryDecryptStats', probeId: 'nope' } });
    expect(postMessage).toHaveBeenCalledWith({ type: 'decryptStats', probeId: 'nope', entered: 0 });
  });

  it('caps the counter map and evicts the oldest id', async () => {
    const { decryptEnteredCounts, recordDecryptEntry } =
      await import('@/renderer/workers/e2eeWorker');
    decryptEnteredCounts.clear();
    for (let i = 0; i < 300; i++) recordDecryptEntry(`c${i}`);
    expect(decryptEnteredCounts.size).toBeLessThanOrEqual(256);
    expect(decryptEnteredCounts.has('c0')).toBe(false); // oldest evicted
    expect(decryptEnteredCounts.has('c299')).toBe(true);
  });
});

describe('e2eeWorker requestFrameKey retry policy (#1885)', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let requestFrameKeyOnce: (s: string, v: number, k: number) => void;
  let frameKeyRequests: Map<string, { lastAttempt: number; attempts: number }>;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.useFakeTimers();
    vi.setSystemTime(0);
    postMessage = vi.fn();
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn(),
    });
    const mod = await import('@/renderer/workers/e2eeWorker');
    requestFrameKeyOnce = mod.requestFrameKeyOnce;
    frameKeyRequests = mod.frameKeyRequests;
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const countRequests = () =>
    postMessage.mock.calls.filter((c) => (c[0] as { type?: string }).type === 'requestFrameKey')
      .length;

  it('retries in a bounded burst then pauses (never loops forever)', () => {
    // 20 frames arrive for the same missing key, each spaced past the backoff.
    for (let i = 0; i < 20; i++) {
      requestFrameKeyOnce('sender-1', 3, 0);
      vi.setSystemTime((i + 1) * 400); // > FRAME_KEY_BACKOFF_MS (350) each step
    }
    expect(countRequests()).toBe(8); // capped at FRAME_KEY_BURST_CAP — no infinite loop
  });

  it('resumes after the idle-reset window so a slow-published key recovers (Gitar #3)', () => {
    for (let i = 0; i < 10; i++) {
      requestFrameKeyOnce('sender-1', 3, 0);
      vi.setSystemTime((i + 1) * 400);
    }
    expect(countRequests()).toBe(8); // burst exhausted, paused

    // A pending-404 key published slower than the burst window: after the idle
    // reset, the next frame resumes requesting instead of staying black forever.
    vi.setSystemTime(10 * 400 + 16_000); // > FRAME_KEY_RETRY_RESET_MS (15000) since last request
    requestFrameKeyOnce('sender-1', 3, 0);
    expect(countRequests()).toBe(9); // recovered: a fresh request went out
  });

  it('suppresses repeated misses inside the backoff window (no request spam)', () => {
    requestFrameKeyOnce('sender-1', 3, 0); // 1 request
    vi.setSystemTime(100); // < 350ms backoff
    requestFrameKeyOnce('sender-1', 3, 0); // suppressed
    requestFrameKeyOnce('sender-1', 3, 0); // suppressed
    expect(countRequests()).toBe(1);
  });

  it('limits a churn of attacker-chosen key ids by authenticated sender, not key id', () => {
    // A malicious SFU can vary keyVersion/keyId every frame. The request limiter
    // must collapse that churn to one sender budget instead of sending one request
    // per attacker-controlled tuple.
    for (let i = 0; i < 30; i++) {
      requestFrameKeyOnce('sender-1', i, i);
      vi.setSystemTime((i + 1) * 400);
    }
    expect(countRequests()).toBeLessThanOrEqual(8);
  });

  it('caps aggregate key requests across many authenticated senders', () => {
    for (let i = 0; i < 100; i++) {
      requestFrameKeyOnce(`sender-${i}`, 1, 1);
      vi.setSystemTime(i + 1);
    }
    // The v6 worker budget is global as well as per-sender, so a room-wide
    // attacker cannot turn one malformed frame per participant into unbounded IPC.
    expect(countRequests()).toBeLessThanOrEqual(64);
  });

  it('caps the tracking map against a sender churning unique keys (DoS bound)', () => {
    // Seed at capacity: the independent sender/global emission budget would
    // otherwise stop this fixture at eight entries before it tests eviction.
    for (let i = 0; i < 512; i++) {
      frameKeyRequests.set(`stale:${i}:0`, { lastAttempt: -400, attempts: 1 });
    }
    requestFrameKeyOnce('sender-1', 99, 0);
    expect(frameKeyRequests.size).toBe(512);
    expect(frameKeyRequests.has('stale:0:0')).toBe(false);
    expect(frameKeyRequests.has('sender-1:99:0')).toBe(true);
  });

  it('LRU eviction keeps an actively-touched key while churn evicts stale ones (Gitar #1885)', () => {
    const keepAlive = 'sender-1:9999:0';
    frameKeyRequests.set(keepAlive, { lastAttempt: -400, attempts: 1 });
    for (let i = 0; i < 510; i++) {
      frameKeyRequests.set(`stale:${i}:0`, { lastAttempt: -400, attempts: 1 });
    }
    // Re-touch A under the real request function, then force capacity eviction.
    requestFrameKeyOnce('sender-1', 9999, 0);
    requestFrameKeyOnce('sender-2', 1, 0);
    requestFrameKeyOnce('sender-3', 1, 0);
    expect(frameKeyRequests.has(keepAlive)).toBe(true);
    expect(frameKeyRequests.has('stale:0:0')).toBe(false);
    expect(frameKeyRequests.size).toBe(512);
  });
});
