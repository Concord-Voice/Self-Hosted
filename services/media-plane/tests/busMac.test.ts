/**
 * Bus MAC (TS half). Every row of the shared vector file is replayed here
 * against `src/lib/busMac.ts`; the Go suite (`pkg/busmac/busmac_test.go`)
 * generates the file and replays the same rows, so the two modules agree on
 * every MAC, every envelope byte and every refusal code.
 */
import { createHash, createHmac } from 'node:crypto';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it, vi } from 'vitest';
import type * as RegistryModule from '../src/lib/busMacRegistry.js';

// The shipped registry holds families only. Like the Go suite's TestMain, add
// the vectors' test raw purpose so the raw rows replay here too.
vi.mock('../src/lib/busMacRegistry.js', async () => {
  const actual = await vi.importActual<typeof RegistryModule>('../src/lib/busMacRegistry.js');
  return {
    ...actual,
    BUS_MAC_PURPOSES: new Map([...actual.BUS_MAC_PURPOSES, ['test>test/raw', 'raw']]),
  };
});

import {
  BusMacError,
  FAMILY_LIFECYCLE,
  FAMILY_TELEMETRY,
  MAX_FUTURE_SKEW_MS,
  MAX_PAST_SKEW_MS,
  MIN_KEY_BYTES,
  isRegisteredFamily,
  parseKeyring,
} from '../src/lib/busMac.js';
import { BUS_MAC_PURPOSES } from '../src/lib/busMacRegistry.js';

interface ParseRow {
  name: string;
  spec: string;
  active_kid?: string;
  error?: string;
}
interface MacRow {
  name: string;
  keyring: string;
  family: string;
  subject: string;
  ts_ms: number;
  body: string;
  kid: string;
  mac_hex: string;
}
interface RawRow {
  name: string;
  keyring: string;
  purpose: string;
  lines: string[];
  kid: string;
  mac_hex: string;
}
interface SignRow {
  name: string;
  keyring: string;
  family: string;
  subject: string;
  body: string;
  now_ms: number;
  kid: string;
  mac_hex: string;
  envelope: string;
}
interface OpenRow {
  name: string;
  group: string;
  keyring: string;
  family: string;
  subject: string;
  envelope: string;
  received_at_ms: number;
  body_hex?: string;
  error?: string;
}
interface FieldRow {
  name: string;
  group: string;
  op: 'mac' | 'raw_mac' | 'sign' | 'open' | 'verify_mac' | 'raw_verify';
  keyring: string;
  family: string;
  kid: string;
  subject: string;
  lines: string[] | null;
  ts_ms: number;
  body: string;
  envelope: string;
  received_at_ms: number;
  mac_hex: string;
  error: string;
}
interface Vectors {
  families: string[];
  test_raw_purpose: string;
  parse_keyring: ParseRow[];
  mac: MacRow[];
  raw: RawRow[];
  sign: SignRow[];
  open: OpenRow[];
  fields: FieldRow[];
}

const vectors = JSON.parse(
  readFileSync(
    path.resolve(import.meta.dirname, '../../control-plane/pkg/busmac/testdata/vectors.json'),
    'utf8'
  )
) as Vectors;

const utf8 = (s: string): Buffer => Buffer.from(s, 'utf8');
const fromHex = (s: string): Buffer => Buffer.from(s, 'hex');

/** The refusal code `fn` throws, or '' when it returns. */
function codeOf(fn: () => unknown): string {
  try {
    fn();
    return '';
  } catch (err) {
    if (err instanceof BusMacError) return err.code;
    throw err;
  }
}

/** The spec §5.3 formula, written out independently of the module. */
function refMac(root: Buffer, purpose: string, kid: string, lines: string[]): Buffer {
  const subkey = createHmac('sha256', root).update(`concord-bus-mac/v1/${purpose}`).digest();
  return createHmac('sha256', subkey)
    .update(['concord-bus-mac/v1', purpose, kid, lines.join('\n')].join('\n'))
    .digest();
}

describe('busMac shared vectors', () => {
  it('registers exactly the families the Go registry ships', () => {
    const families = [...BUS_MAC_PURPOSES]
      .filter(([, kind]) => kind === 'family')
      .map(([name]) => name)
      .sort();
    expect(families).toEqual(vectors.families);
    expect([FAMILY_LIFECYCLE, FAMILY_TELEMETRY].sort()).toEqual(vectors.families);
    expect(isRegisteredFamily(FAMILY_LIFECYCLE)).toBe(true);
    expect(isRegisteredFamily(vectors.test_raw_purpose)).toBe(false);
    expect(isRegisteredFamily('x')).toBe(false);
  });

  it('pins the constants the Go module uses', () => {
    expect(MIN_KEY_BYTES).toBe(32);
    expect(MAX_PAST_SKEW_MS).toBe(60_000);
    expect(MAX_FUTURE_SKEW_MS).toBe(5_000);
  });

  it.each(vectors.parse_keyring)('parse_keyring/$name', (row) => {
    expect(codeOf(() => parseKeyring(row.spec))).toBe(row.error ?? '');
    if (row.error === undefined) {
      expect(parseKeyring(row.spec).activeKid()).toBe(row.active_kid);
    }
  });

  it.each(vectors.mac)('mac/$name', (row) => {
    const ring = parseKeyring(row.keyring);
    const { kid, mac } = ring.mac(row.family, row.subject, row.ts_ms, utf8(row.body));
    expect(kid).toBe(row.kid);
    expect(mac.toString('hex')).toBe(row.mac_hex);
    expect(ring.verifyMac(row.family, row.kid, row.subject, row.ts_ms, utf8(row.body), mac)).toBe(
      true
    );
  });

  it.each(vectors.raw)('raw/$name', (row) => {
    const ring = parseKeyring(row.keyring);
    const { kid, mac } = ring.rawMac(row.purpose, row.lines);
    expect(kid).toBe(row.kid);
    expect(mac.toString('hex')).toBe(row.mac_hex);
    expect(ring.rawVerify(row.purpose, row.kid, row.lines, mac)).toBe(true);
  });

  it.each(vectors.sign)('sign/$name', (row) => {
    const ring = parseKeyring(row.keyring);
    const envelope = ring.sign(row.family, row.subject, row.body, row.now_ms);
    expect(envelope).toBe(row.envelope);
    expect(JSON.parse(envelope)).toEqual({
      v: 1,
      kid: row.kid,
      ts: row.now_ms,
      mac: fromHex(row.mac_hex).toString('base64url'),
      body: row.body,
    });
    expect(ring.open(row.family, row.subject, envelope, row.now_ms)).toBe(row.body);
  });

  it.each(vectors.open)('open/$group/$name', (row) => {
    const ring = parseKeyring(row.keyring);
    let body = '';
    const code = codeOf(() => {
      body = ring.open(row.family, row.subject, row.envelope, row.received_at_ms);
    });
    expect(code).toBe(row.error ?? '');
    if (row.error === undefined) {
      expect(utf8(body).toString('hex')).toBe(row.body_hex);
    }
  });

  it.each(vectors.fields)('field/$group/$name', (row) => {
    const ring = parseKeyring(row.keyring);
    const lines = row.lines ?? [];
    switch (row.op) {
      case 'mac':
        expect(codeOf(() => ring.mac(row.family, row.subject, row.ts_ms, utf8(row.body)))).toBe(
          row.error
        );
        break;
      case 'raw_mac':
        expect(codeOf(() => ring.rawMac(row.family, lines))).toBe(row.error);
        if (row.error === '') {
          const { kid, mac } = ring.rawMac(row.family, lines);
          expect(kid).toBe('a');
          expect(ring.rawVerify(row.family, kid, lines, mac)).toBe(true);
        }
        break;
      case 'sign':
        expect(codeOf(() => ring.sign(row.family, row.subject, row.body, row.ts_ms))).toBe(
          row.error
        );
        break;
      case 'open':
        expect(
          codeOf(() => ring.open(row.family, row.subject, row.envelope, row.received_at_ms))
        ).toBe(row.error);
        break;
      case 'verify_mac':
        expect(
          ring.verifyMac(
            row.family,
            row.kid,
            row.subject,
            row.ts_ms,
            utf8(row.body),
            fromHex(row.mac_hex)
          )
        ).toBe(false);
        break;
      case 'raw_verify':
        expect(ring.rawVerify(row.family, row.kid, lines, fromHex(row.mac_hex))).toBe(false);
        break;
    }
  });
});

describe('busMac properties the vectors cannot carry', () => {
  const [first] = vectors.mac;
  const ring = parseKeyring(first.keyring);
  const body = utf8(first.body);
  const mac = fromHex(first.mac_hex);
  const verify = (candidate: Buffer): boolean =>
    ring.verifyMac(first.family, first.kid, first.subject, first.ts_ms, body, candidate);

  it('agrees with the spec formula computed independently', () => {
    const root = Buffer.from(first.keyring.split(':')[1], 'base64url');
    const digest = createHash('sha256').update(body).digest('hex');
    expect(
      refMac(root, first.family, first.kid, [first.subject, String(first.ts_ms), digest])
    ).toEqual(mac);
  });

  it('compares the whole MAC', () => {
    const flipped = Buffer.from(mac);
    flipped[flipped.length - 1] ^= 1;
    expect(verify(mac)).toBe(true);
    expect(verify(mac.subarray(0, 16))).toBe(false);
    expect(verify(Buffer.concat([mac, Buffer.alloc(1)]))).toBe(false);
    expect(verify(flipped)).toBe(false);
    expect(verify(Buffer.alloc(0))).toBe(false);
  });

  // Go refuses a body that is not UTF-8; a TS string's equivalent is a lone
  // surrogate, which would hash as EF BF BD while the envelope carried \ud800.
  it('refuses a body with a lone surrogate when signing', () => {
    const lone = `{"s":"${String.fromCharCode(0xd800)}"}`;
    expect(codeOf(() => ring.sign(first.family, first.subject, lone, first.ts_ms))).toBe(
      'bad_field'
    );
  });

  it('refuses an envelope string carrying a raw lone surrogate', () => {
    const envelope = ring
      .sign(first.family, first.subject, first.body, first.ts_ms)
      .replace('c1', `c${String.fromCharCode(0xdc00)}`);
    expect(codeOf(() => ring.open(first.family, first.subject, envelope, first.ts_ms))).toBe(
      'bad_envelope'
    );
  });

  it('refuses a lone surrogate in a raw line', () => {
    expect(codeOf(() => ring.rawMac(vectors.test_raw_purpose, [String.fromCharCode(0xd800)]))).toBe(
      'bad_field'
    );
  });

  it('treats a non-finite receipt time as out of window', () => {
    const envelope = ring.sign(first.family, first.subject, first.body, first.ts_ms);
    for (const receivedAt of [Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(codeOf(() => ring.open(first.family, first.subject, envelope, receivedAt))).toBe(
        'skew'
      );
    }
  });

  it('keeps key material out of keyring refusals', () => {
    const short = parseKeyring(first.keyring);
    expect(short.activeKid()).toBe('a');
    const encoded = first.keyring.split(':')[1];
    for (const spec of [
      `a:${encoded.slice(0, 40)}`,
      `${first.keyring},${first.keyring}`,
      ` ${first.keyring}`,
    ]) {
      try {
        parseKeyring(spec);
        expect.unreachable('spec should be refused');
      } catch (err) {
        expect(err).toBeInstanceOf(BusMacError);
        expect(String(err)).not.toContain(encoded.slice(0, 8));
      }
    }
  });
});
