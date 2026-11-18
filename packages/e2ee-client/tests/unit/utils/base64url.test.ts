import { describe, it, expect } from 'vitest';
import { base64urlToBuffer, bufferToBase64url } from '@/renderer/utils/crypto/base64url';

const bytes = (...values: number[]): ArrayBuffer => new Uint8Array(values).buffer;

describe('base64urlToBuffer', () => {
  it('decodes an unpadded base64url string', () => {
    // 'Mg' → 0x32 ('2'), needs two padding chars internally
    expect(new Uint8Array(base64urlToBuffer('Mg'))).toEqual(new Uint8Array([0x32]));
  });

  it('decodes URL-safe characters (- and _) as + and /', () => {
    // 0xfb 0xff encodes to '-_8' in base64url ('+/8' in standard base64)
    expect(new Uint8Array(base64urlToBuffer('-_8'))).toEqual(new Uint8Array([0xfb, 0xff]));
  });

  it('decodes a length that requires no padding', () => {
    expect(new Uint8Array(base64urlToBuffer('AAECAw'))).toEqual(new Uint8Array([0, 1, 2, 3]));
  });

  it('decodes the empty string to an empty buffer', () => {
    expect(base64urlToBuffer('').byteLength).toBe(0);
  });

  it('throws on input that is not base64 at all', () => {
    expect(() => base64urlToBuffer('!!!!')).toThrow();
  });
});

describe('bufferToBase64url', () => {
  it('encodes without padding characters', () => {
    expect(bufferToBase64url(bytes(0x32))).toBe('Mg');
  });

  it('uses URL-safe characters instead of + and /', () => {
    expect(bufferToBase64url(bytes(0xfb, 0xff))).toBe('-_8');
  });

  it('encodes the empty buffer to the empty string', () => {
    expect(bufferToBase64url(bytes())).toBe('');
  });
});

describe('round-trip', () => {
  it('buffer → base64url → buffer is identity across all byte values', () => {
    const all = new Uint8Array(256).map((_, i) => i);
    const encoded = bufferToBase64url(all.buffer);
    expect(encoded).not.toMatch(/[+/=]/);
    expect(new Uint8Array(base64urlToBuffer(encoded))).toEqual(all);
  });

  it('round-trips every remainder length (1..4 bytes)', () => {
    for (let len = 1; len <= 4; len++) {
      const buf = new Uint8Array(Array.from({ length: len }, (_, i) => 0xf0 + i)).buffer;
      expect(new Uint8Array(base64urlToBuffer(bufferToBase64url(buf)))).toEqual(
        new Uint8Array(buf)
      );
    }
  });
});

describe('bufferToBase64url padding trim', () => {
  // Reference: the previous regex-based expression, kept here to pin byte-identical output.
  const legacy = (buffer: ArrayBuffer): string => {
    let binary = '';
    for (const byte of new Uint8Array(buffer)) binary += String.fromCodePoint(byte);
    return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replaceAll(/=+$/g, '');
  };

  it.each([
    ['empty buffer', bytes(), ''],
    ['0 padding chars', bytes(1, 2, 3), 'AQID'],
    ['1 padding char', bytes(1, 2), 'AQI'],
    ['2 padding chars', bytes(1), 'AQ'],
    ['+ and / mapped to - and _', bytes(0xfb, 0xef, 0xbf), '---_'],
  ])('%s', (_name, buffer, expected) => {
    expect(bufferToBase64url(buffer)).toBe(expected);
    expect(bufferToBase64url(buffer)).toBe(legacy(buffer));
  });

  it('matches the legacy regex output for every length 0..64 across sampled byte patterns', () => {
    const all = new Uint8Array(256).map((_, i) => i);
    for (let len = 0; len <= 64; len++) {
      for (let offset = 0; offset < 256; offset += 37) {
        const slice = new Uint8Array(len).map((_, i) => all[(offset + i * 7) % 256]);
        expect(bufferToBase64url(slice.buffer)).toBe(legacy(slice.buffer));
      }
    }
  });
});
