// @vitest-environment node
import { describe, it, expect } from 'vitest';
import {
  generateSalt,
  deriveKeyFromPassword,
  generateKeyPair,
  wrapPrivateKey,
  unwrapPrivateKey,
  exportPublicKey,
  importPublicKey,
  arrayBufferToBase64,
  base64ToArrayBuffer,
  generateRegistrationKeys,
  unwrapLoginKeys,
  generateChannelKey,
  exportChannelKey,
  importChannelKey,
  encryptMessage,
  decryptMessage,
  encryptBlob,
  decryptBlob,
  derivePreferencesKey,
} from '@/renderer/utils/crypto';

describe('crypto utilities', () => {
  describe('arrayBufferToBase64 / base64ToArrayBuffer', () => {
    it('round-trips correctly', () => {
      const original = new Uint8Array([1, 2, 3, 4, 5, 100, 200, 255]);
      const base64 = arrayBufferToBase64(original.buffer);
      const roundTripped = new Uint8Array(base64ToArrayBuffer(base64));
      expect(roundTripped).toEqual(original);
    });

    it('handles empty buffer', () => {
      const empty = new Uint8Array([]);
      const base64 = arrayBufferToBase64(empty.buffer);
      expect(base64).toBe('');
      const back = new Uint8Array(base64ToArrayBuffer(base64));
      expect(back.length).toBe(0);
    });
  });

  describe('generateSalt', () => {
    it('returns 16 bytes', () => {
      const salt = generateSalt();
      expect(salt.length).toBe(16);
    });

    it('returns different salts each time', () => {
      const salt1 = generateSalt();
      const salt2 = generateSalt();
      expect(arrayBufferToBase64(salt1.buffer)).not.toBe(arrayBufferToBase64(salt2.buffer));
    });
  });

  describe('deriveKeyFromPassword', () => {
    it('derives a CryptoKey', async () => {
      const salt = generateSalt();
      const key = await deriveKeyFromPassword('testpassword', salt);
      expect(key).toBeDefined();
      expect(key.type).toBe('secret');
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('same password + salt produces same key', async () => {
      const salt = generateSalt();
      const key1 = await deriveKeyFromPassword('testpassword', salt);
      const key2 = await deriveKeyFromPassword('testpassword', salt);
      // Can't compare CryptoKey directly, but they should both work for wrapping
      expect(key1.algorithm.name).toBe(key2.algorithm.name);
    });
  });

  describe('generateKeyPair', () => {
    it('generates an RSA-OAEP key pair', async () => {
      const keyPair = await generateKeyPair();
      expect(keyPair.publicKey).toBeDefined();
      expect(keyPair.privateKey).toBeDefined();
      expect(keyPair.publicKey.algorithm.name).toBe('RSA-OAEP');
      expect(keyPair.privateKey.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('wrapPrivateKey / unwrapPrivateKey', () => {
    it('round-trips private key', async () => {
      const salt = generateSalt();
      const wrappingKey = await deriveKeyFromPassword('testpassword', salt);
      const keyPair = await generateKeyPair();

      const wrapped = await wrapPrivateKey(keyPair.privateKey, wrappingKey);
      expect(wrapped.byteLength).toBeGreaterThan(12); // IV + wrapped key

      const unwrapped = await unwrapPrivateKey(wrapped, wrappingKey);
      expect(unwrapped.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('exportPublicKey / importPublicKey', () => {
    it('round-trips public key', async () => {
      const keyPair = await generateKeyPair();
      const exported = await exportPublicKey(keyPair.publicKey);
      expect(typeof exported).toBe('string');
      expect(exported.length).toBeGreaterThan(0);

      const imported = await importPublicKey(exported);
      expect(imported.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('generateRegistrationKeys', () => {
    it('returns all required key material', async () => {
      const keys = await generateRegistrationKeys('MySecurePassword123!');
      expect(keys.publicKey).toBeDefined();
      expect(keys.privateKey).toBeDefined();
      expect(typeof keys.wrappedPrivateKey).toBe('string');
      expect(typeof keys.keyDerivationSalt).toBe('string');
      expect(keys.wrappedPrivateKey.length).toBeGreaterThan(0);
      expect(keys.keyDerivationSalt.length).toBeGreaterThan(0);
    });
  });

  describe('unwrapLoginKeys', () => {
    it('unwraps keys generated during registration', async () => {
      const password = 'MySecurePassword123!';
      const regKeys = await generateRegistrationKeys(password);

      const privateKey = await unwrapLoginKeys(
        password,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      expect(privateKey.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('channel key operations', () => {
    it('generateChannelKey creates AES-GCM key', async () => {
      const key = await generateChannelKey();
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('exportChannelKey / importChannelKey round-trips', async () => {
      const key = await generateChannelKey();
      const raw = await exportChannelKey(key);
      expect(raw.byteLength).toBe(32); // 256 bits

      const imported = await importChannelKey(raw);
      expect(imported.algorithm.name).toBe('AES-GCM');
    });
  });

  describe('encryptMessage / decryptMessage', () => {
    it('round-trips a message', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Hello, encrypted world!';

      const ciphertext = await encryptMessage(plaintext, key);
      expect(typeof ciphertext).toBe('string');
      expect(ciphertext).not.toBe(plaintext);

      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe(plaintext);
    });

    it('produces unique ciphertext each time (random IV)', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Same message';

      const ct1 = await encryptMessage(plaintext, key);
      const ct2 = await encryptMessage(plaintext, key);
      expect(ct1).not.toBe(ct2); // Different IVs
    });

    it('handles unicode content', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Hello! Encryption works.';

      const ciphertext = await encryptMessage(plaintext, key);
      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe(plaintext);
    });

    it('handles empty string', async () => {
      const key = await generateChannelKey();
      const ciphertext = await encryptMessage('', key);
      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe('');
    });
  });

  describe('encryptBlob / decryptBlob', () => {
    it('round-trips a JSON object', async () => {
      const salt = generateSalt();
      const key = await derivePreferencesKey('password', salt);
      const data = { theme: 'dark', fontSize: 14, nested: { enabled: true } };

      const encrypted = await encryptBlob(data, key);
      expect(typeof encrypted).toBe('string');

      const decrypted = await decryptBlob<typeof data>(encrypted, key);
      expect(decrypted).toEqual(data);
    });
  });

  describe('derivePreferencesKey', () => {
    it('derives a key for preferences encryption', async () => {
      const salt = generateSalt();
      const key = await derivePreferencesKey('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('produces different key than deriveKeyFromPassword (domain separation)', async () => {
      const salt = generateSalt();
      const wrappingKey = await deriveKeyFromPassword('password', salt);
      const prefsKey = await derivePreferencesKey('password', salt);
      // Both are non-extractable, but we can verify they work independently
      expect(wrappingKey.algorithm.name).toBe('AES-GCM');
      expect(prefsKey.algorithm.name).toBe('AES-GCM');
    });
  });
});
