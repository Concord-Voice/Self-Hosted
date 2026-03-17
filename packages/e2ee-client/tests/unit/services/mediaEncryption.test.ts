// @vitest-environment node
import { describe, it, expect, beforeEach } from 'vitest';

// MediaEncryption is the default export-less class — import directly
let MediaEncryption: typeof import('@/renderer/services/mediaEncryption').MediaEncryption;

beforeEach(async () => {
  const mod = await import('@/renderer/services/mediaEncryption');
  MediaEncryption = mod.MediaEncryption;
});

/** Helper: generate a test AES-256 key (simulates a channel CSK) */
async function generateTestCSK(): Promise<CryptoKey> {
  return crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
    'encrypt',
    'decrypt',
  ]);
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

describe('MediaEncryption', () => {
  describe('encrypt/decrypt round-trip', () => {
    it('round-trips an audio frame', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      await receiver.addDecryptKey(csk, 'sender-user-id');

      const original = new Uint8Array(fakeAudioFrame(50).data).slice();
      const frame = fakeAudioFrame(50);
      // Copy original data for comparison
      const originalData = new Uint8Array(frame.data).slice();

      await sender.encryptFrame(frame);

      // Frame should be larger (payload + trailer overhead)
      expect(frame.data.byteLength).toBeGreaterThan(50);

      // Magic trailer should be present
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 1]).toBe(0xad);
      expect(encrypted[encrypted.length - 2]).toBe(0xde);

      // headerBytes field should be 1 (audio)
      expect(encrypted[encrypted.length - 16]).toBe(1);

      // Decrypt
      await receiver.decryptFrame(frame, 'sender-user-id');

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

      await sender.encryptFrame(frame);
      expect(frame.data.byteLength).toBeGreaterThan(200);

      // headerBytes field should be 2 (video)
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 16]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id');
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

      await sender.encryptFrame(frame);

      // keyId in trailer should be 2
      const encrypted = new Uint8Array(frame.data);
      expect(encrypted[encrypted.length - 15]).toBe(2);

      await receiver.decryptFrame(frame, 'sender-user-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });

  describe('rejection paths', () => {
    it('passes through frames without magic trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      const frame = fakeAudioFrame(50);
      const originalData = new Uint8Array(frame.data).slice();

      // No encryption → no magic trailer → should pass through unchanged
      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames that are too small', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      const frame = fakeAudioFrame(10);
      const originalData = new Uint8Array(frame.data).slice();

      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames with coincidental 0xDEAD but too small for valid ciphertext', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');
      await me.addDecryptKey(csk, 'sender-id');

      // 33 bytes = minimum size to pass the first check, but with headerBytes=2
      // the ciphertext would be 33 - 16 (trailer) - 2 (header) = 15 bytes < 16
      const buf = new ArrayBuffer(33);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[31] = 0xde; // magic
      view[32] = 0xad;
      view[17] = 2; // headerBytes = 2 at position length-16
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;
      const originalData = new Uint8Array(buf).slice();

      // Should pass through: ciphertext is 15 bytes < 16 (GCM auth tag minimum)
      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('passes through frames with invalid headerBytes in trailer', async () => {
      const csk = await generateTestCSK();
      const me = new MediaEncryption();
      await me.init(csk, 'user-id');

      // 50-byte frame with magic trailer but headerBytes=0 (invalid)
      const buf = new ArrayBuffer(50);
      const view = new Uint8Array(buf);
      view.fill(0x42);
      view[48] = 0xde;
      view[49] = 0xad;
      view[34] = 0; // headerBytes = 0 (invalid, must be 1-10)
      const frame = { data: buf } as unknown as RTCEncodedAudioFrame;
      const originalData = new Uint8Array(buf).slice();

      await me.decryptFrame(frame, 'sender-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });

    it('throws when no decrypt key is available', async () => {
      const csk = await generateTestCSK();
      const sender = new MediaEncryption();
      const receiver = new MediaEncryption();

      await sender.init(csk, 'sender-user-id');
      await receiver.init(csk, 'receiver-user-id');
      // Intentionally NOT adding decrypt key for sender

      const frame = fakeAudioFrame(50);
      await sender.encryptFrame(frame);

      await expect(receiver.decryptFrame(frame, 'sender-user-id')).rejects.toThrow(
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

      await sender.encryptFrame(frame);

      // Receiver should self-heal by ratcheting from epoch 0 → 3
      await receiver.decryptFrame(frame, 'sender-user-id');
      expect(new Uint8Array(frame.data)).toEqual(originalData);
    });
  });
});
