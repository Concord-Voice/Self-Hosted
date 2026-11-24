// @vitest-environment node
import { readFileSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { describe, expect, it, vi } from 'vitest';
import {
  encodeNonmediaContext,
  parseNonmediaContext,
  type NonmediaContext,
} from '../../../src/renderer/utils/crypto/nonmediaContext';
import {
  channelKeyCapsuleLabel,
  openNonmediaTextEnvelope,
  type NonmediaTextFormat,
} from '../../../src/renderer/utils/crypto/nonmediaEnvelope';

const bytes = readFileSync(
  new URL('../../../../../docs/design/nonmedia-e2ee-v1-vectors.json', import.meta.url)
);
expect(createHash('sha384').update(bytes).digest('hex')).toBe(
  '4a16bb107203d840152ce432bcbeda286c7ecb79b2b519713eebb2d3701589433f54fad511d0e52b439ea230d49833e2'
);
const vectors = JSON.parse(bytes.toString('utf8')).vectors as Array<{
  name: string;
  input: Record<string, string>;
  expect: { verdict: string; outputs?: Record<string, string> };
}>;
const vector = (name: string) => {
  const item = vectors.find((entry) => entry.name === name);
  if (!item) throw new Error(`missing corpus vector: ${name}`);
  return item;
};
const base64 = (text: string) => new Uint8Array(Buffer.from(text, 'base64'));
const b64Text = (text: string) => Buffer.from(text, 'base64').toString('ascii');

describe('dormant ADR-0051 reader', () => {
  it.each([
    ['message-v1', 'm1'],
    ['blob-v1', 'b1'],
    ['account-ek-wrap-v1', 'ek1'],
    ['recovery-wrap-v1', 'rw1-private'],
    ['recovery-prefs-wrap-v1', 'rw1-preferences'],
  ] as const)(
    'opens the pinned %s ciphertext only under its expected context',
    async (tag, format) => {
      const sample = vector(`aead/decrypt/${tag}/base`);
      const expected = parseNonmediaContext(base64(sample.input.expected_context_b64));
      expect(expected.tag).toBe(tag);
      const key = await webcrypto.subtle.importKey(
        'raw',
        base64(sample.input.key_b64),
        'AES-GCM',
        false,
        ['decrypt']
      );
      const envelope = b64Text(sample.input.envelope_b64);
      const plain = await openNonmediaTextEnvelope(format, envelope, key, expected as never);
      expect(Buffer.from(plain)).toEqual(
        Buffer.from(sample.expect.outputs?.plaintext_b64 ?? '', 'base64')
      );
      const altered = { ...expected, fields: [...expected.fields] } as NonmediaContext;
      (altered.fields as unknown[])[0] = '22222222-2222-4222-8222-222222222222';
      await expect(
        openNonmediaTextEnvelope(format, envelope, key, altered as never)
      ).rejects.toThrow();
      await expect(
        openNonmediaTextEnvelope(
          format,
          envelope.replace(/^[^:]+:/, 'wrong:'),
          key,
          expected as never
        )
      ).rejects.toThrow();
    }
  );

  it('does not choose the other recovery context for the shared rw1 prefix', async () => {
    const sample = vector('aead/decrypt/recovery-wrap-v1/base');
    const privateContext = parseNonmediaContext(base64(sample.input.expected_context_b64));
    const prefs = vector('aead/decrypt/recovery-prefs-wrap-v1/base');
    const prefsContext = parseNonmediaContext(base64(prefs.input.expected_context_b64));
    const key = await webcrypto.subtle.importKey(
      'raw',
      base64(sample.input.key_b64),
      'AES-GCM',
      false,
      ['decrypt']
    );
    await expect(
      openNonmediaTextEnvelope(
        'rw1-preferences',
        b64Text(sample.input.envelope_b64),
        key,
        prefsContext as never
      )
    ).rejects.toThrow();
    await expect(
      openNonmediaTextEnvelope(
        'rw1-private',
        b64Text(sample.input.envelope_b64),
        key,
        privateContext as never
      )
    ).resolves.toBeDefined();
  });

  it('rejects noncanonical or unregistered context before use', () => {
    const sample = vector('aead/decrypt/message-v1/base');
    const canonical = base64(sample.input.expected_context_b64);
    const context = parseNonmediaContext(canonical);
    expect(encodeNonmediaContext(context)).toEqual(canonical);
    expect(() =>
      parseNonmediaContext(
        new TextEncoder().encode(new TextDecoder().decode(canonical).replace(',1,', ', 1,'))
      )
    ).toThrow();
    expect(() => encodeNonmediaContext({ ...context, tag: 'unregistered' } as never)).toThrow();
  });

  it('rejects every pinned malformed or invalid context parse case', () => {
    const rejected = vectors.filter(
      (entry) => entry.name.startsWith('context/parse/') && entry.expect.verdict === 'reject'
    );
    expect(rejected.length).toBeGreaterThanOrEqual(8);
    for (const entry of rejected) {
      expect(() => parseNonmediaContext(base64(entry.input.context_b64)), entry.name).toThrow();
    }
  });

  it('rejects an oversized nonce before base64 decoding', () => {
    const sample = vector('context/encode/attachment-v4/base');
    const context = parseNonmediaContext(base64(sample.expect.outputs?.context_b64 ?? ''));
    const oversized = { ...context, fields: [...context.fields] } as NonmediaContext;
    (oversized.fields as unknown[])[6] = 'AAAA'.repeat(4096);
    const atobSpy = vi.spyOn(globalThis, 'atob');
    try {
      expect(() => encodeNonmediaContext(oversized)).toThrow();
      expect(atobSpy).not.toHaveBeenCalled();
    } finally {
      atobSpy.mockRestore();
    }
  });

  it('parses every pinned accepted context without changing its bytes', () => {
    const accepted = vectors.filter(
      (entry) => entry.name.startsWith('context/encode/') && entry.expect.verdict === 'accept'
    );
    expect(accepted.length).toBeGreaterThanOrEqual(7);
    for (const entry of accepted) {
      const source = base64(entry.expect.outputs?.context_b64 ?? '');
      expect(encodeNonmediaContext(parseNonmediaContext(source)), entry.name).toEqual(source);
    }
  });

  it('derives the capsule label from validated context bytes', () => {
    const sample = vector('rsa-label/derive/channel-key-capsule-v1/base');
    const context = {
      tag: 'channel-key-capsule-v1',
      fields: sample.input.context_fields,
    } as unknown as Extract<NonmediaContext, { tag: 'channel-key-capsule-v1' }>;
    expect(channelKeyCapsuleLabel(context)).toEqual(base64(sample.expect.outputs?.label_b64 ?? ''));
  });

  it('binds valid alternate indexes and totals on a full-size attachment chunk', async () => {
    const key = await webcrypto.subtle.importKey(
      'raw',
      new Uint8Array(32).fill(23),
      'AES-GCM',
      false,
      ['encrypt', 'decrypt']
    );
    const iv = new Uint8Array(12).fill(17); // Fixed test vector only; no production writer is added.
    const context: Extract<NonmediaContext, { tag: 'attachment-v4' }> = {
      tag: 'attachment-v4',
      fields: [
        '11111111-1111-4111-8111-111111111111',
        'channel',
        '55555555-5555-4555-8555-555555555555',
        '66666666-6666-4666-8666-666666666666',
        '77777777-7777-4777-8777-777777777777',
        1,
        'AQEBAQEBAQEBAQEBAQEBAQ==',
        0,
        1,
        3,
        8388608,
      ],
    };
    const plaintext = new Uint8Array(8388608);
    const ciphertext = await webcrypto.subtle.encrypt(
      { name: 'AES-GCM', iv, additionalData: encodeNonmediaContext(context), tagLength: 128 },
      key,
      plaintext
    );
    await expect(
      webcrypto.subtle.decrypt(
        { name: 'AES-GCM', iv, additionalData: encodeNonmediaContext(context), tagLength: 128 },
        key,
        ciphertext
      )
    ).resolves.toHaveProperty('byteLength', 8388608);
    for (const changed of [
      { ...context, fields: [...context.fields.slice(0, 8), 2, 3, 8388608] },
      { ...context, fields: [...context.fields.slice(0, 8), 1, 4, 8388608] },
    ] as NonmediaContext[]) {
      await expect(
        webcrypto.subtle.decrypt(
          { name: 'AES-GCM', iv, additionalData: encodeNonmediaContext(changed), tagLength: 128 },
          key,
          ciphertext
        )
      ).rejects.toThrow();
    }
  });
});
