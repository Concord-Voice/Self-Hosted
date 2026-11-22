// @vitest-environment node
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { parseAv1Obus as parseAv1ObusForTest } from '@/renderer/services/e2ee/av1ObuParser';
import {
  MEDIA_FRAME_MINI_HEADER_SIZE,
  decodeMediaFrameMiniHeader as decodeMediaFrameMiniHeaderForTest,
} from '@/renderer/services/e2ee/mediaFrameMiniHeader';
import {
  findH264SlicePrefixLength,
  parseH264AnnexB,
  stuffH264Bytes,
  unstuffH264Bytes,
} from '@/renderer/services/e2ee/h264NalUnit';

// MediaEncryption is the default export-less class — import directly
let MediaEncryption: typeof import('@/renderer/services/e2ee/mediaEncryption').MediaEncryption;
let deriveFrameKey: typeof import('@/renderer/services/e2ee/mediaEncryption').deriveFrameKey;
let ratchetKey: typeof import('@/renderer/services/e2ee/mediaEncryption').ratchetKey;
let FrameKeyMissError: typeof import('@/renderer/services/e2ee/mediaEncryption').FrameKeyMissError;

beforeEach(async () => {
  const mod = await import('@/renderer/services/e2ee/mediaEncryption');
  MediaEncryption = mod.MediaEncryption;
  deriveFrameKey = mod.deriveFrameKey;
  ratchetKey = mod.ratchetKey;
  FrameKeyMissError = mod.FrameKeyMissError;
});

/** Helper: generate a test AES-256 key (simulates a channel CSK) */
async function generateTestCSK(): Promise<CryptoKey> {
  return crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, ['encrypt', 'decrypt']);
}

/** Helper: build a fake RTCEncodedAudioFrame */
function fakeAudioFrame(size: number): RTCEncodedAudioFrame {
  const buf = new ArrayBuffer(size);
  const view = new Uint8Array(buf);
  // Fill with recognizable pattern (Opus TOC byte + payload)
  for (let i = 0; i < size; i++) view[i] = i & 0xff;
  return { data: buf } as unknown as RTCEncodedAudioFrame;
}

/** Helper: build a fake RTCEncodedVideoFrame */
function fakeVideoFrame(size: number): RTCEncodedVideoFrame {
  const buf = new ArrayBuffer(size);
  const view = new Uint8Array(buf);
  for (let i = 0; i < size; i++) view[i] = (i + 0x10) & 0xff;
  return { data: buf, type: 'delta' } as unknown as RTCEncodedVideoFrame;
}

function concatH264(...parts: Uint8Array[]): Uint8Array<ArrayBuffer> {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const result = new Uint8Array(new ArrayBuffer(total));
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}

function h264StartCode(length: 3 | 4): Uint8Array {
  return length === 3
    ? new Uint8Array([0x00, 0x00, 0x01])
    : new Uint8Array([0x00, 0x00, 0x00, 0x01]);
}

function h264Nal(header: number, payload: readonly number[], delimiter: 3 | 4): Uint8Array {
  return concatH264(h264StartCode(delimiter), new Uint8Array([header, ...payload]));
}

const FIRST_SLICE_BODY = new Uint8Array([0x91, 0xa2, 0xb3, 0xc4, 0xd5, 0xe6, 0xf7, 0x88]);
const LATER_SLICE_BODY = new Uint8Array([0x8f, 0x7e, 0x6d, 0x5c, 0x4b, 0x3a, 0x29, 0x18]);

function buildH264AccessUnit(firstSliceDelimiter: 3 | 4 = 4): RTCEncodedVideoFrame {
  const otherDelimiter = firstSliceDelimiter === 4 ? 3 : 4;
  const data = concatH264(
    h264Nal(0x09, [0xf0], 4),
    h264Nal(0x67, [0x42, 0x80, 0x1f], 3),
    h264Nal(0x68, [0xce, 0x3c, 0x80], 4),
    h264Nal(0x06, [0x55, 0x66, 0x77], 3),
    // 0xb8 is the complete raw EBSP byte covering ue(v) values [0, 2, 0].
    h264Nal(0x65, [0xb8, ...FIRST_SLICE_BODY], firstSliceDelimiter),
    h264Nal(0x41, [0xb8, ...LATER_SLICE_BODY], otherDelimiter),
    h264Nal(0x06, [0xaa, 0xbb, 0xcc, 0xdd], 3)
  );
  return { data: data.buffer, type: 'key' } as unknown as RTCEncodedVideoFrame;
}

function firstH264SliceBoundary(data: Uint8Array): {
  clearEnd: number;
  startCodeOffset: number;
  startCodeLength: 3 | 4;
} {
  const units = parseH264AnnexB(data);
  const firstSlice = units?.find((unit) => unit.kind === 'slice');
  if (!firstSlice) throw new Error('test access unit has no slice');
  const prefixLength = findH264SlicePrefixLength(
    data.subarray(firstSlice.nalOffset + 1, firstSlice.nalOffset + firstSlice.nalLength)
  );
  if (prefixLength === null) throw new Error('test access unit has no slice prefix');
  return {
    clearEnd: firstSlice.nalOffset + 1 + prefixLength,
    startCodeOffset: firstSlice.startCodeOffset,
    startCodeLength: firstSlice.startCodeLength,
  };
}

function containsBytes(haystack: Uint8Array, needle: Uint8Array): boolean {
  for (let offset = 0; offset + needle.length <= haystack.length; offset++) {
    if (needle.every((byte, index) => haystack[offset + index] === byte)) return true;
  }
  return false;
}

function requireH264Bytes(value: Uint8Array | null): Uint8Array {
  if (value === null) throw new Error('expected H.264 byte result');
  return value;
}

describe('MediaEncryption', () => {
  describe('encrypt/decrypt round-trip', () => {
    it('round-trips an audio frame', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      const frame = fakeAudioFrame(50);
      // Copy original data for comparison
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // Frame should be larger (payload + trailer overhead)
      expect(frame.data.byteLength).toBeGreaterThan(50);

      // Magic trailer should be present
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 1]).toBe(0xad);
      expect(encrypted[encrypted.length - 2]).toBe(0xde);

      // headerBytes field should be 1 (audio) — v6 trailer position -23
      expect(encrypted[encrypted.length - 23]).toBe(1);

      // Decrypt
      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');

      // Should match original
      const decrypted = new Uint8Array(frame.data);
      expect(decrypted).toEqual(originalData);
    });

    it('round-trips a video frame', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      const frame = fakeVideoFrame(200);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'vp9');
      expect(frame.data.byteLength).toBeGreaterThan(200);

      // headerBytes field should be 2 (video) — v6 trailer position -23
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 23]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('round-trips after key rotation (epoch > 0)', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      // Rotate both sides to epoch 2
      await sender.rotateKeys();
      await sender.rotateKeys();
      await receiver.rotateKeys();
      await receiver.rotateKeys();

      const frame = fakeAudioFrame(40);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // keyId in trailer should be 2 — v6 keyId is 2B BE, low byte at -21
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 21]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('serializes overlapping key rotations so epochs are not lost', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      await Promise.all([sender.rotateKeys(), sender.rotateKeys()]);
      expect(sender.getCurrentKeyId()).toBe(2);

      await receiver.catchUpToEpoch(2);

      const frame = fakeAudioFrame(40);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      const encrypted = new Uint8Array(frame.data);
      // keyId low byte at v6 position -21
      expect(encrypted[encrypted.length - 21]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('keeps ciphertext and trailer key identity atomic when rotation/rebase overlaps encryption', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id', 0);
      await receiver.addDecryptKeyAtEpoch(csk, 'sender-user-id', 1);

      const frame = fakeVideoFrame(200);
      const originalData = new Uint8Array(frame.data).slice();

      const originalEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
      let signalEncryptStarted: (() => void) | null = null;
      let releaseEncrypt: (() => void) | null = null;
      const encryptStarted = new Promise<void>((resolve) => {
        signalEncryptStarted = resolve;
      });
      const encryptGate = new Promise<void>((resolve) => {
        releaseEncrypt = resolve;
      });

      const encryptSpy = vi
        .spyOn(crypto.subtle, 'encrypt')
        .mockImplementation(async (algorithm, key, data) => {
          if (!signalEncryptStarted) throw new Error('test signal not initialized');
          signalEncryptStarted();
          await encryptGate;
          return originalEncrypt(algorithm, key, data);
        });

      try {
        const encryptPromise = sender.encryptFrame(frame, 'vp9');
        await encryptStarted;

        await sender.rotateKeys();
        expect(sender.getCurrentKeyId()).toBe(1);
        sender.setKeyVersion(9);

        if (!releaseEncrypt) throw new Error('test release not initialized');
        releaseEncrypt();
        await encryptPromise;

        const encrypted = new Uint8Array(frame.data);
        // keyId low byte at v6 position -21
        expect(encrypted[encrypted.length - 21]).toBe(0);
        // keyVersion starts at v6 position -20 and must match the key captured
        // before the deferred WebCrypto operation, not the concurrent rebase.
        expect(new DataView(encrypted.buffer).getUint32(encrypted.length - 20, false)).toBe(0);

        await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
        expect(new Uint8Array(frame.data)).toEqual(originalData);
      } finally {
        encryptSpy.mockRestore();
      }
    });
  });

  describe('rejection paths', () => {
    it('rejects non-empty frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      const frame = fakeAudioFrame(50);

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'unencrypted media frame received'
      );
    });

    it('passes through empty frames', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(0);
      const originalData = new Uint8Array(frame.data).slice();

      await me.decryptFrame(frame, 'sender-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('rejects non-empty too-small frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(10);

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'unencrypted media frame received'
      );
    });

    it('rejects too-small frames that carry the E2EE magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 10 bytes is below the v6 minimum (trailer 23 + GCM overhead 17).
      const buf = new ArrayBuffer(10);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[8] = 0xde;
      view[9] = 0xad;
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('rejects frames with magic trailer but too-small ciphertext', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      // 39 bytes: below v6 minimum (trailer 23 + GCM overhead 17 = 40),
      // so the "too small" guard fires before the version check.
      const buf = new ArrayBuffer(39);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[37] = 0xde; // magic
      view[38] = 0xad;
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('rejects frames with invalid headerBytes in trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 50-byte frame with magic trailer, valid v6 version/scheme, but headerBytes=0.
      // v6 layout: magic[-2,-1], scheme[-3], version[-4], keyVersion[-20,-17],
      // keyId[-22,-21], headerBytes[-23].
      const buf = new ArrayBuffer(50);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[48] = 0xde;
      view[49] = 0xad;
      view[50 - 4] = 6;
      view[50 - 3] = 1; // Opus scheme
      view[50 - 23] = 0; // headerBytes = 0 (invalid, must be 1-10)
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;

      await expect(me.decryptFrame(frame, 'sender-id', 'opus')).rejects.toThrow(
        'malformed encrypted frame'
      );
    });

    it('throws when no decrypt key is available', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      // Intentionally NOT adding decrypt key for sender

      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame, 'opus');

      await expect(receiver.decryptFrame(frame, 'sender-user-id', 'opus')).rejects.toThrow(
        /no decrypt key/
      );
    });
  });

  describe('self-healing ratchet', () => {
    it('ratchets forward to decrypt frames at a higher epoch', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      // Sender rotates to epoch 3, receiver stays at epoch 0
      await sender.rotateKeys();
      await sender.rotateKeys();
      await sender.rotateKeys();

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // Receiver should self-heal by ratcheting from epoch 0 → 3
      await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('bounds repeated forged keyId+10 frames, then a directly provisioned key recovers', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();
      await sender.init(csk, 'sender');
      await receiver.init(csk, 'receiver');
      await receiver.addDecryptKey(csk, 'sender');
      for (let i = 0; i < 10; i++) await sender.rotateKeys();
      const valid = fakeAudioFrame(50);
      await sender.encryptFrame(valid, 'opus');
      const wire = new Uint8Array(valid.data).slice();
      const forged = wire.slice();
      forged[0] ^= 0x80;
      const derive = vi.spyOn(crypto.subtle, 'deriveKey');
      vi.useFakeTimers();
      try {
        vi.setSystemTime(0);
        const first = { data: forged.slice().buffer } as RTCEncodedAudioFrame;
        await expect(receiver.decryptFrame(first, 'sender', 'opus')).rejects.not.toBeInstanceOf(
          FrameKeyMissError
        );
        const throttledValid = { data: wire.slice().buffer } as RTCEncodedAudioFrame;
        await expect(receiver.decryptFrame(throttledValid, 'sender', 'opus')).rejects.toMatchObject(
          {
            name: 'FrameKeyMissError',
            senderUserId: 'sender',
            keyVersion: 0,
            keyId: 10,
            message: expect.stringContaining('ratchet work throttled'),
          }
        );
        for (let i = 0; i < 3; i++) {
          const frame = { data: forged.slice().buffer } as RTCEncodedAudioFrame;
          await expect(receiver.decryptFrame(frame, 'sender', 'opus')).rejects.toBeInstanceOf(
            FrameKeyMissError
          );
        }
        expect(derive).toHaveBeenCalledTimes(10);
        let provisioned = await deriveFrameKey(csk, 'sender');
        for (let i = 0; i < 10; i++) provisioned = await ratchetKey(provisioned);
        receiver.addDecryptKeyDirectV3('sender', 0, 10, provisioned);
        const recovered = { data: wire.buffer } as RTCEncodedAudioFrame;
        await expect(receiver.decryptFrame(recovered, 'sender', 'opus')).resolves.toBeUndefined();
      } finally {
        derive.mockRestore();
        vi.useRealTimers();
      }
    });

    it('enforces sender and global ratchet budgets, then permits an expired exact-key retry', async () => {
      const csk = await generateTestCSK();
      const receiver = new MediaEncryption();
      await receiver.init(csk, 'receiver');
      const internal = receiver as unknown as {
        deriveRatchetedKey(
          sender: string,
          version: number,
          keyId: number
        ): Promise<CryptoKey | null>;
      };
      vi.useFakeTimers();
      try {
        vi.setSystemTime(0);
        const base = await deriveFrameKey(csk, 'sender');
        receiver.addDecryptKeyDirectV3('sender', 0, 0, base);
        await expect(internal.deriveRatchetedKey('sender', 0, 10)).resolves.toBeTruthy();
        await expect(internal.deriveRatchetedKey('sender', 0, 9)).rejects.toMatchObject({
          name: 'FrameKeyMissError',
          senderUserId: 'sender',
          keyVersion: 0,
          keyId: 9,
          message: expect.stringContaining('budget'),
        });
        vi.advanceTimersByTime(15_001);
        await expect(internal.deriveRatchetedKey('sender', 0, 10)).resolves.toBeTruthy();
        vi.advanceTimersByTime(15_001);
        for (let i = 0; i < 6; i++) {
          const id = `global-${i}`;
          receiver.addDecryptKeyDirectV3(id, 0, 0, base);
          await expect(internal.deriveRatchetedKey(id, 0, 10)).resolves.toBeTruthy();
        }
        receiver.addDecryptKeyDirectV3('global-overflow', 0, 0, base);
        await expect(internal.deriveRatchetedKey('global-overflow', 0, 10)).rejects.toMatchObject({
          name: 'FrameKeyMissError',
          senderUserId: 'global-overflow',
          keyVersion: 0,
          keyId: 10,
          message: expect.stringContaining('budget'),
        });
      } finally {
        vi.useRealTimers();
      }
    });

    it('keeps an authority-current decrypt key usable after an hour', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();
      vi.useFakeTimers();
      try {
        vi.setSystemTime(0);
        await sender.init(csk, 'sender');
        await receiver.init(csk, 'receiver');
        sender.setKeyVersion(1);
        receiver.setKeyVersion(1);
        await receiver.addDecryptKeyAtVersion(csk, 'sender', 1, 0);
        vi.advanceTimersByTime(3_660_000);
        const frame = fakeAudioFrame(50);
        const original = new Uint8Array(frame.data).slice();
        await sender.encryptFrame(frame, 'opus');
        await expect(receiver.decryptFrame(frame, 'sender', 'opus')).resolves.toBeUndefined();
        expect(new Uint8Array(frame.data)).toEqual(original);
      } finally {
        vi.useRealTimers();
      }
    });

    it('does not extend an older key version by decrypting overlap media', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();
      vi.useFakeTimers();
      try {
        vi.setSystemTime(0);
        await sender.init(csk, 'sender');
        await receiver.init(csk, 'receiver');
        await receiver.addDecryptKeyAtVersion(csk, 'sender', 0, 0);
        const first = fakeAudioFrame(50);
        const second = fakeAudioFrame(50);
        await sender.encryptFrame(first, 'opus');
        await sender.encryptFrame(second, 'opus');
        receiver.setKeyVersion(1);
        vi.advanceTimersByTime(9_000);
        await expect(receiver.decryptFrame(first, 'sender', 'opus')).resolves.toBeUndefined();
        vi.advanceTimersByTime(2_000);
        await expect(receiver.decryptFrame(second, 'sender', 'opus')).rejects.toBeInstanceOf(
          FrameKeyMissError
        );
        await expect(receiver.addDecryptKeyAtVersion(csk, 'sender', 0, 0)).rejects.toThrow(
          /retired decrypt key version/
        );
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe('Worker-path APIs (initFromKey / addDecryptKeyDirect / setCurrentKeyId)', () => {
    it('derives and ratchets 256-bit AES-GCM frame keys', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-a');
      const ratcheted = await ratchetKey(key);

      expect(key.algorithm.name).toBe('AES-GCM');
      expect(key.algorithm).toHaveProperty('length', 256);
      expect(ratcheted.algorithm.name).toBe('AES-GCM');
      expect(ratcheted.algorithm).toHaveProperty('length', 256);
    });

    it('initFromKey enables encryptFrame and sets currentKeyId', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-a');

      const enc = new MediaEncryption();
      enc.initFromKey(key, 5);

      expect(enc.getCurrentKeyId()).toBe(5);

      // Should be able to encrypt
      const frame = fakeAudioFrame(50);
      await enc.encryptFrame(frame, 'opus');

      // Encrypted frame should be larger and have the magic trailer
      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(50);
      expect(data[data.length - 1]).toBe(0xad);
      expect(data[data.length - 2]).toBe(0xde);

      // keyId in trailer should be 5 — v6 keyId is 2B BE, low byte at -21
      expect(data[data.length - 21]).toBe(5);
    });

    it('addDecryptKeyDirect allows decryptFrame for matching keyId', async () => {
      const csk = await generateTestCSK();
      const senderKey = await deriveFrameKey(csk, 'sender-x');

      // Sender uses initFromKey (Worker path)
      const sender = new MediaEncryption();
      sender.initFromKey(senderKey, 0);

      // Receiver uses addDecryptKeyDirect (Worker path)
      const receiver = new MediaEncryption();
      const receiverOwnKey = await deriveFrameKey(csk, 'receiver-y');
      receiver.initFromKey(receiverOwnKey, 0);
      receiver.addDecryptKeyDirect('sender-x', 0, senderKey);

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      await receiver.decryptFrame(frame, 'sender-x', 'opus');

      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('addDecryptKeyDirect at a ratcheted epoch decrypts correctly', async () => {
      const csk = await generateTestCSK();

      // Sender at epoch 2
      const sender = new MediaEncryption();
      await sender.init(csk, 'sender-z');
      await sender.rotateKeys();
      await sender.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(2);

      // Receiver derives sender key and pre-ratchets to epoch 2, then adds directly
      let senderKey = await deriveFrameKey(csk, 'sender-z');
      senderKey = await ratchetKey(senderKey);
      senderKey = await ratchetKey(senderKey);

      const receiver = new MediaEncryption();
      const recvKey = await deriveFrameKey(csk, 'receiver-w');
      receiver.initFromKey(recvKey, 0);
      receiver.addDecryptKeyDirect('sender-z', 2, senderKey);

      const frame = fakeAudioFrame(60);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');
      await receiver.decryptFrame(frame, 'sender-z', 'opus');

      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('setCurrentKeyId updates epoch without nulling encryptKey', async () => {
      const csk = await generateTestCSK();
      const key = await deriveFrameKey(csk, 'user-b');

      const enc = new MediaEncryption();
      enc.initFromKey(key, 0);

      // Advance epoch via setCurrentKeyId
      enc.setCurrentKeyId(7);
      expect(enc.getCurrentKeyId()).toBe(7);

      // encryptFrame should still work (key not nulled)
      const frame = fakeAudioFrame(40);
      await enc.encryptFrame(frame, 'opus');

      const data = new Uint8Array(frame.data);
      expect(data.length).toBeGreaterThan(40);
      // keyId in trailer should be 7 — v6 keyId is 2B BE, low byte at -21
      expect(data[data.length - 21]).toBe(7);
    });

    it('full Worker-path round-trip: initFromKey + addDecryptKeyDirect + rotation', async () => {
      const csk = await generateTestCSK();
      const senderKey = await deriveFrameKey(csk, 'alice');
      const receiverKey = await deriveFrameKey(csk, 'bob');

      const sender = new MediaEncryption();
      sender.initFromKey(senderKey, 0);

      const receiver = new MediaEncryption();
      receiver.initFromKey(receiverKey, 0);
      receiver.addDecryptKeyDirect('alice', 0, senderKey);

      // Both rotate to epoch 1 (simulating Worker receiving rotateKeys message)
      await sender.rotateKeys();
      await receiver.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(1);
      expect(receiver.getCurrentKeyId()).toBe(1);

      const frame = fakeAudioFrame(80);
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'opus');

      // keyId should be 1 — v6 keyId is 2B BE, low byte at -21
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 21]).toBe(1);

      await receiver.decryptFrame(frame, 'alice', 'opus');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('debouncedRotateKeys', () => {
    it('rotates after debounce delay', async () => {
      const { vi } = await import('vitest');
      vi.useFakeTimers();
      try {
        const csk = await generateTestCSK();
        const enc = new MediaEncryption();
        await enc.init(csk, 'user-1');
        expect(enc.getCurrentKeyId()).toBe(0);

        enc.debouncedRotateKeys();

        // Not yet rotated
        expect(enc.getCurrentKeyId()).toBe(0);

        // Advance past 2s debounce
        await vi.advanceTimersByTimeAsync(2500);

        await vi.waitFor(() => {
          expect(enc.getCurrentKeyId()).toBe(1);
        });
      } finally {
        vi.useRealTimers();
      }
    });

    it('coalesces rapid calls into single rotation', async () => {
      const { vi } = await import('vitest');
      vi.useFakeTimers();
      try {
        const csk = await generateTestCSK();
        const enc = new MediaEncryption();
        await enc.init(csk, 'user-1');

        // Fire 5 rapid calls
        for (let i = 0; i < 5; i++) {
          enc.debouncedRotateKeys();
        }

        await vi.advanceTimersByTimeAsync(2500);

        // Should have rotated exactly once (not 5 times)
        await vi.waitFor(() => {
          expect(enc.getCurrentKeyId()).toBe(1);
        });
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe('catchUpToEpoch', () => {
    it('catches up from epoch 0 to target', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');
      expect(enc.getCurrentKeyId()).toBe(0);

      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);
    });

    it('does not overshoot when duplicate catch-up requests overlap', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await Promise.all([enc.catchUpToEpoch(2), enc.catchUpToEpoch(2)]);

      expect(enc.getCurrentKeyId()).toBe(2);
    });

    it('rejects instead of spinning when catch-up cannot advance', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');
      enc.destroy();

      await expect(enc.catchUpToEpoch(1)).rejects.toThrow('E2EE epoch catch-up stalled');
      expect(enc.getCurrentKeyId()).toBe(0);
    });

    it('no-op when already at target', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);

      // Calling again with same target should be no-op
      await enc.catchUpToEpoch(3);
      expect(enc.getCurrentKeyId()).toBe(3);

      // Calling with lower target should also be no-op
      await enc.catchUpToEpoch(1);
      expect(enc.getCurrentKeyId()).toBe(3);
    });

    it('rejects gap > 100', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await expect(enc.catchUpToEpoch(101)).rejects.toThrow('E2EE epoch gap too large');
      expect(enc.getCurrentKeyId()).toBe(0);
    });
  });

  describe('destroy', () => {
    it.each(['opus', 'h264', 'av1'] as const)(
      'drops a %s frame whose encryption finishes after teardown',
      async (codec) => {
        const csk = await generateTestCSK();
        const enc = new MediaEncryption();
        await enc.init(csk, 'sender');
        const frame =
          codec === 'opus'
            ? fakeAudioFrame(40)
            : codec === 'h264'
              ? buildH264AccessUnit()
              : ({
                  data: new Uint8Array([0x0a, 0x02, 0xaa, 0xbb, 0x32, 0x03, 1, 2, 3]).buffer,
                  type: 'key',
                } as unknown as RTCEncodedVideoFrame);
        const original = frame.data;
        const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
        let entered: () => void = () => {};
        let release: () => void = () => {};
        const started = new Promise<void>((resolve) => {
          entered = resolve;
        });
        const blocked = new Promise<void>((resolve) => {
          release = resolve;
        });
        const spy = vi.spyOn(crypto.subtle, 'encrypt').mockImplementation(async (...args) => {
          const ciphertext = await encrypt(...args);
          entered();
          await blocked;
          return ciphertext;
        });
        try {
          const pending = enc.encryptFrame(frame, codec);
          await started;
          enc.destroy();
          release();
          await expect(pending).rejects.toThrow('media encryption destroyed');
          expect(frame.data).toBe(original);
        } finally {
          spy.mockRestore();
        }
      }
    );

    it.each(['opus', 'h264', 'av1'] as const)(
      'drops a %s frame whose decryption finishes after teardown',
      async (codec) => {
        const csk = await generateTestCSK();
        const sender = new MediaEncryption();
        const receiver = new MediaEncryption();
        await sender.init(csk, 'sender');
        await receiver.init(csk, 'receiver');
        await receiver.addDecryptKey(csk, 'sender');
        const frame =
          codec === 'opus'
            ? fakeAudioFrame(40)
            : codec === 'h264'
              ? buildH264AccessUnit()
              : ({
                  data: new Uint8Array([0x0a, 0x02, 0xaa, 0xbb, 0x32, 0x03, 1, 2, 3]).buffer,
                  type: 'key',
                } as unknown as RTCEncodedVideoFrame);
        await sender.encryptFrame(frame, codec);
        const encrypted = frame.data;
        const decrypt = crypto.subtle.decrypt.bind(crypto.subtle);
        let entered: () => void = () => {};
        let release: () => void = () => {};
        const started = new Promise<void>((resolve) => {
          entered = resolve;
        });
        const blocked = new Promise<void>((resolve) => {
          release = resolve;
        });
        const spy = vi.spyOn(crypto.subtle, 'decrypt').mockImplementation(async (...args) => {
          const plaintext = await decrypt(...args);
          entered();
          await blocked;
          return plaintext;
        });
        try {
          const pending = receiver.decryptFrame(frame, 'sender', codec);
          await started;
          receiver.destroy();
          release();
          await expect(pending).rejects.toThrow('media encryption destroyed');
          expect(frame.data).toBe(encrypted);
        } finally {
          spy.mockRestore();
        }
      }
    );

    it('does not let a pending decrypt-key derivation restore state after teardown', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'receiver');
      const pending = enc.addDecryptKeyAtVersion(csk, 'sender', 1, 1);
      enc.destroy();
      await expect(pending).rejects.toThrow('media encryption destroyed');
      expect((enc as unknown as { decryptKeys: Map<string, unknown> }).decryptKeys.size).toBe(0);
    });

    it('clears state and prevents further encryption', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      // Encryption works before destroy
      const frame = fakeAudioFrame(40);
      await enc.encryptFrame(frame, 'opus');

      enc.destroy();

      // Encrypt should fail after destroy
      const frame2 = fakeAudioFrame(40);
      await expect(enc.encryptFrame(frame2, 'opus')).rejects.toThrow('media encryption destroyed');
    });

    it('is idempotent', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      enc.destroy();
      enc.destroy(); // Should not throw
      expect(enc.getCurrentKeyId()).toBe(0);
    });
  });

  describe('addDecryptKeyAtEpoch', () => {
    it('pre-ratchets key to target epoch', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender');
      await receiver.init(csk, 'receiver');

      // Rotate sender to epoch 3
      await sender.rotateKeys();
      await sender.rotateKeys();
      await sender.rotateKeys();
      expect(sender.getCurrentKeyId()).toBe(3);

      // Pre-ratchet receiver's decrypt key for sender to epoch 3
      await receiver.addDecryptKeyAtEpoch(csk, 'sender', 3);

      // Sender encrypts at epoch 3
      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame, 'opus');

      // Receiver should decrypt successfully
      await receiver.decryptFrame(frame, 'sender', 'opus');
    });

    it('rejects target epoch > 100', async () => {
      const csk = await generateTestCSK();
      const enc = new MediaEncryption();
      await enc.init(csk, 'user-1');

      await expect(enc.addDecryptKeyAtEpoch(csk, 'sender', 101)).rejects.toThrow(
        'E2EE epoch gap too large'
      );
    });
  });
});

// Regression for #1742: an empty (0-byte) DTX frame used to encrypt to exactly
// 32 bytes, which the decrypt `< 33` too-small guard misclassified as
// unencrypted and fed to the Opus decoder undeciphered — the receiver-side
// garble-during-silence. The fix passes empty frames through unchanged.
describe('MediaEncryption — #1742 empty DTX frame passthrough', () => {
  it('passes an empty (0-byte) frame through unchanged on encrypt (never the 32-byte blob)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const frame = fakeAudioFrame(0);
    await sender.encryptFrame(frame, 'opus');

    // Must stay 0 bytes — the pre-fix bug produced exactly 32 bytes here.
    expect(frame.data.byteLength).toBe(0);
  });

  it('round-trips an empty frame as empty (decoder sees DTX silence, not garble)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(0);
    await sender.encryptFrame(frame, 'opus');
    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');

    expect(frame.data.byteLength).toBe(0);
  });

  it('still round-trips a 1-byte audio frame (boundary just above empty)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(1);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'opus');
    // 1-byte input encrypts to 39 bytes (the v5 guard exactly: 1+16+22) —
    // must decrypt, not pass through.
    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('stores the ACTUAL header length in the trailer (closes the sub-header video boundary, H5)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    // A 1-byte video frame: getUnencryptedBytes()=2 but only 1 byte exists, so
    // the trailer must record the actual header length (1), not the static 2 —
    // else the decrypt header/ciphertext split overruns and the frame is
    // mis-passed-through instead of decrypted.
    const frame = fakeVideoFrame(1);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'vp9');

    const enc = new Uint8Array(frame.data);
    expect(enc[enc.length - 23]).toBe(1); // headerBytes field = actual header length (v6 -23)

    await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('uses a fully random 96-bit GCM IV for encrypted frames', async () => {
    // Security invariant: empty-frame early return must stay above IV
    // generation, and real encrypted frames request the full 96-bit GCM nonce
    // from WebCrypto rather than combining a counter prefix with random suffix.
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    const randomSpy = vi.spyOn(crypto, 'getRandomValues');

    const empty = fakeAudioFrame(0);
    await sender.encryptFrame(empty, 'opus'); // must not advance the counter
    expect(empty.data.byteLength).toBe(0);
    expect(randomSpy).not.toHaveBeenCalled();

    const real = fakeAudioFrame(50);
    await sender.encryptFrame(real, 'opus');
    expect(randomSpy).toHaveBeenCalledTimes(1);
    expect((randomSpy.mock.calls[0][0] as Uint8Array).byteLength).toBe(12);
    randomSpy.mockRestore();
  });
});

describe('channel CSK rotation desync (#1878)', () => {
  it('baseline: shared CSK round-trips', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKey(csk, 'alice', 0);

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('a CSK rotation is deterministic when the frame carries the version (#1878 fixed)', async () => {
    const cskOld = await generateTestCSK();
    const cskNew = await generateTestCSK();

    const sender = new MediaEncryption();
    sender.setKeyVersion(2); // sender re-based onto NEW CSK at version 2
    await sender.init(cskNew, 'alice');

    const receiver = new MediaEncryption();
    // Receiver holds BOTH versions (v1 old + v2 new) — the 3-part map keeps them distinct.
    await receiver.addDecryptKeyAtVersion(cskOld, 'alice', 1, 0);
    await receiver.addDecryptKeyAtVersion(cskNew, 'alice', 2, 0);

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // Frame stamps version 2 → receiver selects the v2 key deterministically.
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });
});

describe('version-keyed frame trailer (introduced in v3 by #1878)', () => {
  it('round-trips the current frame format carrying (keyVersion, keyId)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(7); // new API: bind encrypt keyVersion
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 7, 0); // new API: (csk, sender, version, keyId)

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // v6 whole-frame trailer is 23 bytes; magic is still last 2.
    const bytes = new Uint8Array(frame.data);
    expect(bytes.at(-1)).toBe(0xad);
    expect(bytes.at(-2)).toBe(0xde);

    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('never decrypts a v(N) frame with a v(M) key (version isolation)', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(2);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 3, 0); // wrong version held

    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    // Map key is senderId:2:0 (from frame); receiver holds senderId:3:0 → miss.
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).rejects.toThrow(/no decrypt key/);
  });

  it('keyVersion boundary: a large version (e.g. 65537) survives the 4-byte BE field', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(65537);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption();
    await receiver.addDecryptKeyAtVersion(csk, 'alice', 65537, 0);
    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');
    await expect(receiver.decryptFrame(frame, 'alice', 'vp9')).resolves.toBeUndefined();
  });

  it('a decrypt miss throws FrameKeyMissError with (keyVersion,keyId), not OperationError', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    sender.setKeyVersion(9);
    await sender.init(csk, 'alice');
    const receiver = new MediaEncryption(); // holds NO key
    const frame = fakeVideoFrame(200);
    await sender.encryptFrame(frame, 'vp9');

    let caught: unknown;
    try {
      await receiver.decryptFrame(frame, 'alice', 'vp9');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(FrameKeyMissError);
    expect(caught).toMatchObject({
      name: 'FrameKeyMissError',
      senderUserId: 'alice',
      keyVersion: 9,
      keyId: 0,
    });
    // Message still contains "no decrypt key" so the existing regex tests pass.
    expect((caught as Error).message).toMatch(/no decrypt key/);
  });
});

describe('v6 whole-frame (VP9/VP8/Opus authenticated, version-stamped)', () => {
  it('advertises crypto version 6', async () => {
    const mod = await import('@/renderer/services/e2ee/mediaEncryption');
    expect(mod.MEDIA_E2EE_FRAME_CRYPTO_VERSION).toBe(6);
  });

  it('round-trips an audio frame under v6 with version marker and codec scheme in trailer', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(50);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'opus');

    const enc = new Uint8Array(frame.data);
    // v6 trailer: ...[version:1 (=6)][scheme:1 (=opus)][magic:2].
    expect(enc[enc.length - 1]).toBe(0xad);
    expect(enc[enc.length - 2]).toBe(0xde);
    expect(enc[enc.length - 4]).toBe(6); // version marker
    expect(enc[enc.length - 3]).toBe(1); // Opus scheme

    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('round-trips a VP9 video frame under v6', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeVideoFrame(200);
    const original = new Uint8Array(frame.data).slice();
    await sender.encryptFrame(frame, 'vp9');
    await receiver.decryptFrame(frame, 'sender-user-id', 'vp9');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('rejects a whole-frame trailer carrying the v5 marker', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const frame = fakeAudioFrame(50);
    await sender.encryptFrame(frame, 'opus');
    const wire = new Uint8Array(frame.data);
    wire[wire.length - 4] = 5;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'opus')).rejects.toThrow(
      'unexpected frame crypto version'
    );
  });
});

describe('v6 authenticated whole-frame metadata', () => {
  async function setupPair(): Promise<{
    sender: InstanceType<typeof MediaEncryption>;
    receiver: InstanceType<typeof MediaEncryption>;
  }> {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    return { sender, receiver };
  }

  it('stamps v6 and the Opus scheme in a 23-byte trailer', async () => {
    const { sender, receiver } = await setupPair();
    const frame = fakeAudioFrame(50);
    const original = new Uint8Array(frame.data).slice();

    await sender.encryptFrame(frame, 'opus');
    const wire = new Uint8Array(frame.data);
    expect(wire[wire.length - 23]).toBe(1); // headerBytes
    expect(wire[wire.length - 4]).toBe(6); // version
    expect(wire[wire.length - 3]).toBe(1); // Opus scheme

    await receiver.decryptFrame(frame, 'sender-user-id', 'opus');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it('rejects an inherited object key used as an untrusted codec before mutating the frame', async () => {
    const { sender } = await setupPair();
    const frame = fakeAudioFrame(50);
    const original = new Uint8Array(frame.data).slice();

    await expect(sender.encryptFrame(frame, 'toString' as never)).rejects.toThrow();
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it.each([
    ['opus', fakeAudioFrame],
    ['vp8', fakeVideoFrame],
    ['vp9', fakeVideoFrame],
  ] as const)('drops a tampered clear prefix for %s', async (codec, makeFrame) => {
    const { sender, receiver } = await setupPair();
    const frame = makeFrame(50);
    await sender.encryptFrame(frame, codec);

    const wire = new Uint8Array(frame.data);
    wire[0] ^= 0x01; // Opus TOC or the video payload descriptor
    await expect(receiver.decryptFrame(frame, 'sender-user-id', codec)).rejects.toThrow();
  });

  it('drops a trailer-field mutation and a valid-tag frame sent to the wrong codec branch', async () => {
    const { sender, receiver } = await setupPair();
    const frame = fakeVideoFrame(50);
    await sender.encryptFrame(frame, 'vp8');
    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'vp9')).rejects.toThrow();

    await sender.encryptFrame(frame, 'vp8');
    const wire = new Uint8Array(frame.data);
    wire[wire.length - 3] ^= 0x01; // authenticated scheme byte
    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'vp8')).rejects.toThrow();
  });

  it('rejects a v5 trailer after the v6 cutover', async () => {
    const { sender, receiver } = await setupPair();
    const frame = fakeAudioFrame(50);
    await sender.encryptFrame(frame, 'opus');
    const wire = new Uint8Array(frame.data);
    wire[wire.length - 4] = 5;
    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'opus')).rejects.toThrow(
      /unexpected frame crypto version/
    );
  });
});

describe('AV1 per-OBU (v5, v4-compatible mini-header layout)', () => {
  /** Build a single low-overhead OBU: header + optional ext + leb128 size + payload. */
  function obu(opts: {
    type: number;
    payload: Uint8Array;
    ext?: number; // present → extension_flag=1, this is the extension byte
    hasSize?: boolean; // default true
  }): Uint8Array {
    const { type, payload, ext, hasSize = true } = opts;
    const header =
      ((type & 0x0f) << 3) | ((ext !== undefined ? 1 : 0) << 2) | ((hasSize ? 1 : 0) << 1);
    const head: number[] = [header];
    if (ext !== undefined) head.push(ext & 0xff);
    const size: number[] = [];
    if (hasSize) {
      let v = payload.length;
      do {
        let b = v & 0x7f;
        v >>>= 7;
        if (v) b |= 0x80;
        size.push(b);
      } while (v);
    }
    return new Uint8Array([...head, ...size, ...payload]);
  }

  function concat(...parts: Uint8Array[]): Uint8Array {
    const total = parts.reduce((n, p) => n + p.length, 0);
    const out = new Uint8Array(total);
    let o = 0;
    for (const p of parts) {
      out.set(p, o);
      o += p.length;
    }
    return out;
  }

  function wireObuChunks(data: Uint8Array): Uint8Array[] {
    const parsed = parseAv1ObusForTest(data);
    if (parsed === null) throw new Error('expected parseable AV1 wire frame');
    let start = 0;
    return parsed.map((entry) => {
      const end = entry.payloadOffset + entry.payloadLen;
      const chunk = data.slice(start, end);
      start = end;
      return chunk;
    });
  }

  // Build a valid-ish AV1 temporal unit: TD + SEQUENCE_HEADER + FRAME(6) + TILE_GROUP(4).
  function buildAv1Frame(framePayload: Uint8Array, tgPayload: Uint8Array): RTCEncodedVideoFrame {
    const td = obu({ type: 2, payload: new Uint8Array(0) });
    const seq = obu({ type: 1, payload: new Uint8Array([0xaa, 0xbb]) });
    const frame = obu({ type: 6, payload: framePayload });
    const tg = obu({ type: 4, payload: tgPayload });
    const data = concat(td, seq, frame, tg);
    return {
      data: data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength),
      type: 'key',
    } as unknown as RTCEncodedVideoFrame;
  }

  it('encrypts only FRAME/TILE_GROUP payloads and leaves structure cleartext', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const framePayload = new Uint8Array([1, 2, 3, 4, 5, 6]);
    const tgPayload = new Uint8Array([7, 7, 7, 7]);
    const frame = buildAv1Frame(framePayload, tgPayload);
    await sender.encryptFrame(frame, 'av1');

    // The transport-normalized empty leading TD is removed before the v6 manifest.
    const enc = new Uint8Array(frame.data);
    expect(enc[0] & 0x80).toBe(0); // forbidden bit clear
    expect((enc[0] >>> 3) & 0x0f).toBe(1); // first retained OBU is the sequence header

    // Each encrypted tile-data OBU payload now starts with the 0xDEAD mini-header magic.
    // (Parse to find them rather than asserting a fixed offset.)
    const obus = parseAv1ObusForTest(enc);
    expect(obus).not.toBeNull();
    const tileData = obus!.filter((o) => o.obuType === 6 || o.obuType === 4);
    expect(tileData).toHaveLength(2);
    for (const o of tileData) {
      expect(enc[o.payloadOffset]).toBe(0xde);
      expect(enc[o.payloadOffset + 1]).toBe(0xad);
    }
    // The SEQUENCE_HEADER (type 1) payload stays cleartext (0xaa 0xbb).
    const seqObu = obus!.find((o) => o.obuType === 1);
    expect(seqObu).toBeDefined();
    expect(Array.from(enc.slice(seqObu!.payloadOffset, seqObu!.payloadOffset + 2))).toEqual([
      0xaa, 0xbb,
    ]);
  });

  it('round-trips an AV1 frame: decrypt restores exact original payloads', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');

    const framePayload = new Uint8Array([1, 2, 3, 4, 5, 6]);
    const tgPayload = new Uint8Array([7, 8, 9]);
    const frame = buildAv1Frame(framePayload, tgPayload);
    const original = new Uint8Array(frame.data).slice(2); // v6 removes the leading empty TD

    const digest = vi.spyOn(crypto.subtle, 'digest');
    const encrypt = vi.spyOn(crypto.subtle, 'encrypt');
    const decrypt = vi.spyOn(crypto.subtle, 'decrypt');
    await sender.encryptFrame(frame, 'av1');
    expect(new Uint8Array(frame.data)).not.toEqual(original); // structure changed (encrypted payloads)
    await receiver.decryptFrame(frame, 'sender-user-id', 'av1');
    expect(new Uint8Array(frame.data)).toEqual(original); // byte-exact restore
    expect(digest).toHaveBeenCalledTimes(2); // one manifest hash on each side
    const aadLengths = [...encrypt.mock.calls, ...decrypt.mock.calls].map(
      ([algorithm]) => (algorithm as AesGcmParams).additionalData?.byteLength
    );
    expect(aadLengths).toEqual([66, 66, 66, 66]); // fixed size for each protected OBU
    vi.restoreAllMocks();
  });

  it('per-OBU IV uniqueness: 2 encrypted OBUs in one frame get distinct IVs', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');

    const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    await sender.encryptFrame(frame, 'av1');

    const enc = new Uint8Array(frame.data);
    const obus = parseAv1ObusForTest(enc)!.filter((o) => o.obuType === 6 || o.obuType === 4);
    expect(obus).toHaveLength(2);
    const ivs = obus.map((o) => {
      const mh = decodeMediaFrameMiniHeaderForTest(
        enc.slice(o.payloadOffset, o.payloadOffset + 22)
      );
      return Array.from(mh!.iv);
    });
    expect(ivs[0]).not.toEqual(ivs[1]); // each OBU receives a fresh full-random IV
  });

  it('requests a fresh full 96-bit random IV after recreating a session under the same key', async () => {
    const csk = await generateTestCSK();
    const randomSpy = vi.spyOn(crypto, 'getRandomValues');

    for (let session = 0; session < 2; session++) {
      const sender = new MediaEncryption();
      await sender.init(csk, 'sender-user-id');
      const frame = buildAv1Frame(
        new Uint8Array([session + 1, 0x22, 0x33]),
        new Uint8Array([0x44, 0x55])
      );
      await sender.encryptFrame(frame, 'av1');
    }

    // Two encrypted OBUs per frame, across two fresh MediaEncryption sessions.
    // Every AEAD invocation must ask the CSPRNG for the entire 96-bit nonce;
    // counters that reset with the session may not supply a deterministic prefix.
    expect(randomSpy).toHaveBeenCalledTimes(4);
    for (const [buffer] of randomSpy.mock.calls) {
      expect((buffer as Uint8Array).byteLength).toBe(12);
    }
    randomSpy.mockRestore();
  });

  it('leaves the original AV1 frame untouched when a later OBU encryption fails', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    const frame = buildAv1Frame(
      new Uint8Array([0x11, 0x22, 0x33]),
      new Uint8Array([0x44, 0x55, 0x66])
    );
    const original = new Uint8Array(frame.data).slice();
    const realEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
    const encryptSpy = vi.spyOn(crypto.subtle, 'encrypt');
    encryptSpy
      .mockImplementationOnce((algorithm, key, data) => realEncrypt(algorithm, key, data))
      .mockRejectedValueOnce(new DOMException('injected second-OBU failure', 'OperationError'));

    await expect(sender.encryptFrame(frame, 'av1')).rejects.toThrow('injected second-OBU failure');
    expect(new Uint8Array(frame.data)).toEqual(original);
    encryptSpy.mockRestore();
  });

  it('survives a real nonminimal leb128 rewrite of a clear AV1 OBU size', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const frame = buildAv1Frame(new Uint8Array([1, 2, 3, 4]), new Uint8Array([5, 6, 7, 8]));
    const original = new Uint8Array(frame.data).slice(2); // v6 removes the leading empty TD
    await sender.encryptFrame(frame, 'av1');
    const wire = new Uint8Array(frame.data);
    // The first retained OBU is `[0x0a, 0x02, aa, bb]`. Rewrite its one-byte
    // size as the equivalent two-byte leb128 `82 00`, shifting later wire bytes.
    frame.data = concat(wire.slice(0, 1), new Uint8Array([0x82, 0x00]), wire.slice(2)).buffer;
    await receiver.decryptFrame(frame, 'sender-user-id', 'av1');
    expect(new Uint8Array(frame.data)).toEqual(original);
  });

  it.each(['drop', 'add', 'reorder'] as const)(
    'drops an AV1 protected-OBU %s mutation',
    async (mutation) => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();
      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');
      const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
      await sender.encryptFrame(frame, 'av1');
      const chunks = wireObuChunks(new Uint8Array(frame.data));
      expect(chunks).toHaveLength(3);
      frame.data = (
        mutation === 'drop'
          ? concat(chunks[0], chunks[1])
          : mutation === 'add'
            ? concat(...chunks, chunks[2])
            : concat(chunks[0], chunks[2], chunks[1])
      ).buffer;

      await expect(receiver.decryptFrame(frame, 'sender-user-id', 'av1')).rejects.toThrow();
    }
  );

  it('does not mutate the frame or commit a derived key when a later protected OBU tag fails', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    await sender.rotateKeys(); // receiver must derive keyId 1 from its keyId 0 entry
    const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    await sender.encryptFrame(frame, 'av1');
    const tampered = new Uint8Array(frame.data).slice();
    tampered[tampered.length - 1] ^= 0x80;
    frame.data = tampered.buffer;
    const decryptKeys = (receiver as unknown as { decryptKeys: Map<string, unknown> }).decryptKeys;
    const before = decryptKeys.size;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'av1')).rejects.toThrow();
    expect(new Uint8Array(frame.data)).toEqual(tampered);
    expect(decryptKeys.size).toBe(before);
  });

  it('drops a tile-less AV1 frame instead of forwarding unauthenticated decoder input', async () => {
    const csk = await generateTestCSK();
    const receiver = new MediaEncryption();
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const tileless = concat(
      obu({ type: 2, payload: new Uint8Array(0) }),
      obu({ type: 1, payload: new Uint8Array([0xaa, 0xbb]) }),
      obu({ type: 3, payload: new Uint8Array([0x01]) })
    );
    const frame = { data: tileless.buffer, type: 'key' } as unknown as RTCEncodedVideoFrame;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'av1')).rejects.toThrow();
  });

  it('drops a modified clear AV1 structure header before it reaches the decoder', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    await sender.encryptFrame(frame, 'av1');

    const wire = new Uint8Array(frame.data);
    const obus = parseAv1ObusForTest(wire);
    expect(obus).not.toBeNull();
    let start = 0;
    for (const obuEntry of obus ?? []) {
      if (obuEntry.obuType === 1) {
        wire[start] = (wire[start] & 0x87) | (5 << 3); // metadata is disallowed in v6
        break;
      }
      start = obuEntry.payloadOffset + obuEntry.payloadLen;
    }
    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'av1')).rejects.toThrow();
  });

  it('canonicalizes one leading empty temporal delimiter and rejects a duplicate', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const frame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    const expected = new Uint8Array(frame.data).slice(2); // the exact `12 00` is normalized

    await sender.encryptFrame(frame, 'av1');
    await receiver.decryptFrame(frame, 'sender-user-id', 'av1');
    expect(new Uint8Array(frame.data)).toEqual(expected);

    const duplicateFrame = buildAv1Frame(new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6]));
    await sender.encryptFrame(duplicateFrame, 'av1');
    duplicateFrame.data = concat(
      new Uint8Array([0x12, 0x00]),
      new Uint8Array([0x12, 0x00]),
      new Uint8Array(duplicateFrame.data)
    ).buffer;
    await expect(receiver.decryptFrame(duplicateFrame, 'sender-user-id', 'av1')).rejects.toThrow();
  });

  it('drops (throws) a malformed AV1 frame fail-closed', async () => {
    const csk = await generateTestCSK();
    const receiver = new MediaEncryption();
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    const bad = {
      data: new Uint8Array([(6 << 3) | 0b10, 100, 1]).buffer,
      type: 'key',
    } as unknown as RTCEncodedVideoFrame;
    await expect(receiver.decryptFrame(bad, 'sender-user-id', 'av1')).rejects.toThrow();
  });
});

describe('H.264 Annex-B encrypted suffix (v5)', () => {
  async function setupH264Pair(): Promise<{
    sender: InstanceType<typeof MediaEncryption>;
    receiver: InstanceType<typeof MediaEncryption>;
  }> {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    await receiver.addDecryptKey(csk, 'sender-user-id');
    return { sender, receiver };
  }

  it('uses one AEAD for the complete access unit and hides the first slice body plus later NALs', async () => {
    const { sender } = await setupH264Pair();
    const frame = buildH264AccessUnit(4);
    const original = new Uint8Array(frame.data).slice();
    const { clearEnd } = firstH264SliceBoundary(original);
    const encryptSpy = vi.spyOn(crypto.subtle, 'encrypt');

    try {
      await sender.encryptFrame(frame, 'h264');
      expect(encryptSpy).toHaveBeenCalledTimes(1);
    } finally {
      encryptSpy.mockRestore();
    }

    const encrypted = new Uint8Array(frame.data);
    expect(encrypted.slice(0, clearEnd)).toEqual(original.slice(0, clearEnd));
    expect(containsBytes(encrypted, FIRST_SLICE_BODY)).toBe(false);
    expect(containsBytes(encrypted, LATER_SLICE_BODY)).toBe(false);

    const parsedWire = parseH264AnnexB(encrypted);
    expect(parsedWire?.filter((unit) => unit.kind === 'slice')).toHaveLength(1);
  });

  it.each([3, 4] as const)(
    'round-trips a multi-NAL, multi-slice access unit whose first VCL delimiter is %i bytes',
    async (delimiterLength) => {
      const { sender, receiver } = await setupH264Pair();
      const frame = buildH264AccessUnit(delimiterLength);
      const original = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame, 'h264');
      await receiver.decryptFrame(frame, 'sender-user-id', 'h264');

      expect(new Uint8Array(frame.data)).toEqual(original);
    }
  );

  it('drops a first-VCL delimiter rewrite because the complete clear prefix is authenticated', async () => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit(4);
    const original = new Uint8Array(frame.data).slice();
    const originalBoundary = firstH264SliceBoundary(original);
    expect(originalBoundary.startCodeLength).toBe(4);

    await sender.encryptFrame(frame, 'h264');
    const encrypted = new Uint8Array(frame.data);
    const encryptedBoundary = firstH264SliceBoundary(encrypted);
    frame.data = concatH264(
      encrypted.slice(0, encryptedBoundary.startCodeOffset),
      encrypted.slice(encryptedBoundary.startCodeOffset + 1)
    ).buffer;
    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it('authenticates the clear first-slice prefix as AAD', async () => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');

    const tampered = new Uint8Array(frame.data).slice();
    const { clearEnd } = firstH264SliceBoundary(tampered);
    tampered[clearEnd - 1] ^= 0x01;
    frame.data = tampered.buffer;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it('authenticates every pre-VCL Annex-B byte, including AUD and SPS', async () => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');
    const tampered = new Uint8Array(frame.data).slice();
    tampered[5] ^= 0x01; // AUD payload; v5 excluded it from AAD.
    frame.data = tampered.buffer;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it('rejects ciphertext tampering after valid re-stuffing', async () => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');

    const encrypted = new Uint8Array(frame.data);
    const { clearEnd } = firstH264SliceBoundary(encrypted);
    const region = requireH264Bytes(unstuffH264Bytes(encrypted.subarray(clearEnd), 0));
    region[MEDIA_FRAME_MINI_HEADER_SIZE] ^= 0x80;
    const restuffed = requireH264Bytes(stuffH264Bytes(region, 0));
    frame.data = concatH264(encrypted.slice(0, clearEnd), restuffed).buffer;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it.each([
    ['version', 20, 5],
    ['scheme', 21, 4],
  ] as const)('rejects a H.264 mini-header %s mutation', async (_name, offset, value) => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');
    const encrypted = new Uint8Array(frame.data);
    const { clearEnd } = firstH264SliceBoundary(encrypted);
    const region = requireH264Bytes(unstuffH264Bytes(encrypted.subarray(clearEnd), 0));
    region[offset] = value;
    const restuffed = requireH264Bytes(stuffH264Bytes(region, 0));
    frame.data = concatH264(encrypted.slice(0, clearEnd), restuffed).buffer;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it('uses a fresh random 96-bit IV for each complete access unit', async () => {
    const { sender } = await setupH264Pair();
    const frames = [buildH264AccessUnit(), buildH264AccessUnit()];
    const ivs: number[][] = [];

    for (const frame of frames) {
      await sender.encryptFrame(frame, 'h264');
      const encrypted = new Uint8Array(frame.data);
      const { clearEnd } = firstH264SliceBoundary(encrypted);
      const region = requireH264Bytes(unstuffH264Bytes(encrypted.subarray(clearEnd), 0));
      const mini = decodeMediaFrameMiniHeaderForTest(region);
      expect(mini).not.toBeNull();
      expect(mini?.iv).toHaveLength(12);
      ivs.push(Array.from(mini?.iv ?? []));
    }

    expect(ivs[0]).not.toEqual(ivs[1]);
  });

  it('fails closed on empty, malformed, no-VCL, and unsupported-VCL access units', async () => {
    const { sender, receiver } = await setupH264Pair();
    const empty = { data: new ArrayBuffer(0), type: 'delta' } as RTCEncodedVideoFrame;
    const malformed = {
      data: new Uint8Array([0x65, 0xb8, 0x91]).buffer,
      type: 'key',
    } as RTCEncodedVideoFrame;
    const noVcl = {
      data: concatH264(h264Nal(0x67, [0x42, 0x80], 4), h264Nal(0x68, [0xce, 0x80], 3)).buffer,
      type: 'key',
    } as RTCEncodedVideoFrame;
    const unsupportedVcl = {
      data: h264Nal(0x74, [0xb8, 0x91], 4).buffer,
      type: 'key',
    } as RTCEncodedVideoFrame;

    await expect(sender.encryptFrame(empty, 'h264')).rejects.toThrow();
    await expect(receiver.decryptFrame(empty, 'sender-user-id', 'h264')).rejects.toThrow();
    await expect(sender.encryptFrame(malformed, 'h264')).rejects.toThrow();
    await expect(receiver.decryptFrame(malformed, 'sender-user-id', 'h264')).rejects.toThrow();
    await expect(sender.encryptFrame(noVcl, 'h264')).rejects.toThrow();
    await expect(receiver.decryptFrame(noVcl, 'sender-user-id', 'h264')).rejects.toThrow();
    await expect(sender.encryptFrame(unsupportedVcl, 'h264')).rejects.toThrow();
    await expect(receiver.decryptFrame(unsupportedVcl, 'sender-user-id', 'h264')).rejects.toThrow();
  });

  it('rejects an unencrypted H.264 access unit with no media mini-header', async () => {
    const { receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow(
      /missing media mini-header/
    );
  });

  it('rejects malformed terminal stuffing', async () => {
    const { sender, receiver } = await setupH264Pair();
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');
    const encrypted = new Uint8Array(frame.data);
    const { clearEnd } = firstH264SliceBoundary(encrypted);
    frame.data = concatH264(
      encrypted.slice(0, clearEnd),
      new Uint8Array([0x00, 0x00, 0x02])
    ).buffer;

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toThrow(
      /stuffing/
    );
  });

  it('reports a typed key miss from the H.264 mini-header', async () => {
    const csk = await generateTestCSK();
    const sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await sender.init(csk, 'sender-user-id');
    await receiver.init(csk, 'receiver-user-id');
    const frame = buildH264AccessUnit();
    await sender.encryptFrame(frame, 'h264');

    await expect(receiver.decryptFrame(frame, 'sender-user-id', 'h264')).rejects.toBeInstanceOf(
      FrameKeyMissError
    );
  });
});

// ---------------------------------------------------------------------------
// Task 10: mixed-codec room (per-sender codec isolation, spec §6.2)
// ---------------------------------------------------------------------------

describe('mixed-codec room (per-sender codec isolation)', () => {
  it('one receiver decrypts an AV1 sender and a VP9 sender with the correct scheme each', async () => {
    const csk = await generateTestCSK();
    const av1Sender = new MediaEncryption();
    const vp9Sender = new MediaEncryption();
    const receiver = new MediaEncryption();
    await av1Sender.init(csk, 'alice'); // AV1 publisher
    await vp9Sender.init(csk, 'bob'); // VP9 publisher
    await receiver.init(csk, 'me');
    await receiver.addDecryptKey(csk, 'alice');
    await receiver.addDecryptKey(csk, 'bob');

    // AV1 frame from alice: TD (type 2, size 0) + FRAME (type 6, payload [1,2,3])
    const av1Data = new Uint8Array([(2 << 3) | 0b10, 0, (6 << 3) | 0b10, 3, 1, 2, 3]);
    const av1Frame = { data: av1Data.buffer, type: 'key' } as unknown as RTCEncodedVideoFrame;
    const av1Orig = new Uint8Array(av1Frame.data).slice(2); // v6 removes the leading empty TD
    await av1Sender.encryptFrame(av1Frame, 'av1');

    // VP9 whole-frame from bob
    const vp9Frame = fakeVideoFrame(120);
    const vp9Orig = new Uint8Array(vp9Frame.data).slice();
    await vp9Sender.encryptFrame(vp9Frame, 'vp9');

    await receiver.decryptFrame(av1Frame, 'alice', 'av1');
    await receiver.decryptFrame(vp9Frame, 'bob', 'vp9');

    expect(new Uint8Array(av1Frame.data)).toEqual(av1Orig);
    expect(new Uint8Array(vp9Frame.data)).toEqual(vp9Orig);
  });
});
