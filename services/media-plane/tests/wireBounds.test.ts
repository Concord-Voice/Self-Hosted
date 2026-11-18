import { describe, expect, it } from 'vitest';
import {
  HANDSHAKE_USERNAME_MAX_CHARS,
  WIRE_ID_PATTERN,
  isBoundedWireId,
  parseConsumeRequest,
} from '../src/lib/wireBounds.js';

// A real mediasoup id shape: a UUID.
const PRODUCER_ID = '5f0c8a3e-7b21-4d9a-9c4e-2a6b1d3f8e07';
const TRANSPORT_ID = '0b9e7d1c-3a52-4f68-8e1d-7c2b4a9f6e13';

describe('wire id bounds (VULN-157)', () => {
  it('pins the reviewed pattern and handshake bound', () => {
    expect(WIRE_ID_PATTERN.source).toBe('^[A-Za-z0-9_-]{1,64}$');
    expect(WIRE_ID_PATTERN.flags).toBe('');
    expect(HANDSHAKE_USERNAME_MAX_CHARS).toBe(64);
  });

  it('accepts a 64-char id and rejects 65 chars, empty, non-string and control characters', () => {
    expect(isBoundedWireId(PRODUCER_ID)).toBe(true);
    expect(isBoundedWireId('a'.repeat(64))).toBe(true);
    expect(isBoundedWireId('a_B-9')).toBe(true);
    expect(isBoundedWireId('a'.repeat(65))).toBe(false);
    expect(isBoundedWireId('')).toBe(false);
    for (const value of [undefined, null, 42, true, {}, [], ['id'], 10n]) {
      expect(isBoundedWireId(value)).toBe(false);
    }
    for (const value of ['id\n', '\nid', 'id\u0000', 'id\r\nInjected', 'i d', 'id ', 'idé', 'id😀']) {
      expect(isBoundedWireId(value)).toBe(false);
    }
  });
});

describe('parseConsumeRequest (VULN-157)', () => {
  it('accepts transportId undefined', () => {
    expect(parseConsumeRequest({ producerId: 'p1' })).toEqual({ producerId: 'p1' });
    expect(parseConsumeRequest({ producerId: PRODUCER_ID, transportId: undefined })).toEqual({
      producerId: PRODUCER_ID,
    });
  });

  it('accepts a bounded transportId and drops unknown fields', () => {
    expect(parseConsumeRequest({ producerId: PRODUCER_ID, transportId: TRANSPORT_ID, extra: 'x'.repeat(1000) })).toEqual({
      producerId: PRODUCER_ID,
      transportId: TRANSPORT_ID,
    });
  });

  it('refuses null, arrays, numbers and a null transportId', () => {
    for (const payload of [null, undefined, [], [PRODUCER_ID], 42, 'p1', true]) {
      expect(parseConsumeRequest(payload)).toBeUndefined();
    }
    expect(parseConsumeRequest({ producerId: PRODUCER_ID, transportId: null })).toBeUndefined();
    expect(parseConsumeRequest({ producerId: PRODUCER_ID, transportId: 'x'.repeat(65) })).toBeUndefined();
    expect(parseConsumeRequest({ producerId: PRODUCER_ID, transportId: 7 })).toBeUndefined();
  });

  it('refuses a numeric or 65-char producerId', () => {
    expect(parseConsumeRequest({ producerId: 12345 })).toBeUndefined();
    expect(parseConsumeRequest({ producerId: 'p'.repeat(65) })).toBeUndefined();
    expect(parseConsumeRequest({ producerId: '' })).toBeUndefined();
    expect(parseConsumeRequest({})).toBeUndefined();
    expect(parseConsumeRequest({ producerId: 'p1\nforged log line' })).toBeUndefined();
  });
});
