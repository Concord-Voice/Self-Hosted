// @vitest-environment node
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, it, expect, vi } from 'vitest';
import * as gen from './gen.mjs';

const { subtle } = globalThis.crypto;

async function point(): Promise<Uint8Array> {
  const kp = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-384' }, true, ['sign', 'verify']);
  return new Uint8Array(await subtle.exportKey('raw', kp.publicKey));
}

const id = () => crypto.randomUUID();
const bytes = (n: number) => crypto.getRandomValues(new Uint8Array(n));

async function sampleIntent() {
  return {
    realm_id: id(), account_id: id(), request_id: id(), op: 'rotate_root', expires_at_ms: 1_700_000_660_000,
    expected_seq: 4, expected_recovery_gen: 1, expected_head_digest: bytes(48), predecessor_root_digest: bytes(48),
    candidate_root: await point(), ek_binding_digest: bytes(48), legacy_ek_digest: new Uint8Array(0),
    device_intent_digest: bytes(48), recovery_bind_digest: new Uint8Array(0),
  };
}

const text = (b: Uint8Array) => Buffer.from(b).toString('latin1');
const raw = (s: string) => new Uint8Array(Buffer.from(s, 'latin1'));

describe('gen.mjs codec (independent implementation of DoR §2.3–§2.6)', () => {
  it('round-trips an intent byte for byte', async () => {
    const b = gen.encode('intent', await sampleIntent());
    const v = await gen.decode('intent', b);
    expect(gen.equalBytes(gen.encode('intent', v), b)).toBe(true);
  });

  it('refuses every EN-level corruption as malformed', async () => {
    const t = text(gen.encode('intent', await sampleIntent()));
    const cases: Record<string, string> = {
      '2^53': t.replace(',1700000660000,', ',9007199254740992,'),
      '1.0': t.replace(',4,1,', ',4.0,1,'),
      '1e0': t.replace(',4,1,', ',4e0,1,'),
      BOM: '\xef\xbb\xbf' + t,
      space: t.replace(',1,"intent"', ', 1,"intent"'),
      escape: t.replace('"rotate_root"', '"rotate\\u005froot"'),
      'non-ASCII': t.replace('"rotate_root"', '"rotat\xc3\xa9"'),
      'invalid UTF-8': t.replace('"rotate_root"', '"rotate\xff"'),
      'version 2': t.replace('",1,"intent"', '",2,"intent"'),
      'extra element': t.slice(0, -1) + ',""]',
      'missing element': t.slice(0, t.lastIndexOf(',')) + ']',
      'private op': t.replace('"rotate_root"', '"add_device"'),
      'nested array': t.replace('"rotate_root"', '["rotate_root"]'),
    };
    for (const [name, s] of Object.entries(cases)) {
      await expect(gen.decode('intent', raw(s)), name).rejects.toBeInstanceOf(gen.Malformed);
    }
  });

  it('refuses a hybrid point at the Web Crypto prefix check, before importKey', async () => {
    const p = await point();
    const hybrid = p.slice();
    hybrid[0] = 0x06 | (p[96] & 1);
    await expect(gen.importP384(p)).resolves.toBeDefined();
    await expect(gen.importP384(hybrid)).rejects.toThrow(/point_prefix/);
    expect(() => gen.checkPoint(hybrid)).toThrow(gen.Malformed);
    const off = p.slice();
    off[96] ^= 1;
    expect(() => gen.checkPoint(off)).toThrow(gen.Malformed);
  });

  // checkPoint is hand-written field arithmetic because encode is synchronous and Web
  // Crypto is not. Its verdict must equal Web Crypto's on every point form, so a wrong
  // constant or reduction cannot give the generator a different acceptance boundary.
  it('agrees with Web Crypto on P-384 point validity', async () => {
    const P = 2n ** 384n - 2n ** 128n - 2n ** 96n + 2n ** 32n - 1n;
    const be = (n: bigint) => Uint8Array.from(Buffer.from(n.toString(16).padStart(96, '0'), 'hex'));
    const withXY = (x: bigint, y: bigint) => Uint8Array.from([0x04, ...be(x), ...be(y)]);
    const xy = (b: Uint8Array) => [b.subarray(1, 49), b.subarray(49)].map((h) => BigInt('0x' + Buffer.from(h).toString('hex')));
    const cases: Uint8Array[] = [];
    for (let i = 0; i < 32; i++) {
      const p = await point();
      const [x, y] = xy(p);
      cases.push(p);                       // on the curve
      cases.push(withXY(x, P - y));        // its negation: also on the curve
      const off = p.slice();
      off[96] ^= 1 << (i % 8);             // y perturbed: off the curve
      cases.push(off);
      cases.push(withXY(P + BigInt(i), y)); // x ≥ p: out of the field, still 48 bytes
    }
    cases.push(withXY(0n, 0n), withXY(P, 0n), withXY(0n, P), withXY(P - 1n, P - 1n));
    let accepted = 0;
    for (const c of cases) {
      expect(c).toHaveLength(97);
      const crypto = await gen.importP384(c).then(() => true, () => false);
      const ours = (() => { try { gen.checkPoint(c); return true; } catch { return false; } })();
      expect(ours, Buffer.from(c).toString('hex').slice(0, 24)).toBe(crypto);
      if (crypto) accepted++;
    }
    expect(accepted).toBe(64); // 32 points and their 32 negations; every other case is refused by both
  });

  it('agrees with itself on EK SPKI: template and Web Crypto procedure', async () => {
    const kp = await subtle.generateKey(
      { name: 'RSA-OAEP', modulusLength: 4096, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
      true, ['encrypt', 'decrypt']);
    const spki = new Uint8Array(await subtle.exportKey('spki', kp.publicKey));
    expect(() => gen.checkEkSpkiTemplate(spki)).not.toThrow();
    await expect(gen.checkEkSpkiWebCrypto(spki)).resolves.toBeUndefined();
    const e65539 = spki.slice();
    e65539[549] = 0x03;
    expect(() => gen.checkEkSpkiTemplate(e65539)).toThrow(gen.Malformed);
    await expect(gen.checkEkSpkiWebCrypto(e65539)).rejects.toBeInstanceOf(gen.Malformed);
  }, 30_000);

  it('computes the DoR worst cases independently of Go', () => {
    expect(gen.maxLen('bundle')).toBe(24388);
    expect(gen.maxLen('submit')).toBe(14963);
    expect(gen.maxLen('device-submit')).toBe(7681);
    expect(gen.maxLen('import-item')).toBe(56038);
    expect(gen.maxLen('device-set')).toBe(4087);
    expect(gen.maxLen('device-bundle')).toBe(7812);
    expect(gen.maxLen('device-history')).toBe(174921);
    expect(gen.maxLen('recovery-bind')).toBe(678);
    expect(gen.maxLen('relay-recover')).toBe(10278);
    expect(gen.maxLen('device-secrets')).toBe(3866);
    expect(Object.keys(gen.LAYOUTS)).toHaveLength(46);
    expect(gen.FACTORS).toHaveLength(14);
  });

  it('refuses non-boolean age-claim flags, which Go cannot express (%t)', () => {
    const claim = {
      user_id: crypto.randomUUID(), valid_age: true, nsfw_auth: false, jurisdiction_obligation: 2,
      nonce: 'a'.repeat(64), timestamp: 1, device_id: crypto.randomUUID(), client_version: '1.0.0',
    };
    expect(gen.ageClaimV2Bytes(claim)).not.toBeNull();
    for (const bad of [{ valid_age: 1 }, { valid_age: 'true' }, { nsfw_auth: 'false' }, { nsfw_auth: 0 }, { nsfw_auth: null }]) {
      expect(gen.ageClaimV2Bytes({ ...claim, ...bad }), JSON.stringify(bad)).toBeNull();
    }
  });

  it('formats the safety number as 12 space-separated groups', async () => {
    const s = await gen.safetyNumber(id(), bytes(48));
    expect(s).toMatch(/^[0-9]{5}( [0-9]{5}){11}$/);
    expect(await gen.safetyNumber('NOT-A-UUID', bytes(48))).toBe('');
  });
});

// A dev string: DeviceID(dsk) '.' dsk '.' dkx '.' mature_at_ms.
async function deviceKeys() {
  return { dsk: await point(), dkx: await point() };
}

async function dev(matureAtMs: number, keys?: { dsk: Uint8Array; dkx: Uint8Array }): Promise<string> {
  const { dsk, dkx } = keys ?? (await deviceKeys());
  return `${await gen.deviceId(dsk)}.${gen.b64e(dsk)}.${gen.b64e(dkx)}.${matureAtMs}`;
}

function sampleDeviceSet(active: string[]) {
  return {
    realm_id: id(), account_id: id(), active, pending: '', recovery_key: new Uint8Array(0),
    recovery_bind_digest: new Uint8Array(0), replacement_key: new Uint8Array(0),
    replacement_bind_digest: new Uint8Array(0), replacement_nb_ms: 0,
  };
}

describe('gen.mjs device-set active ids (DoR §2.5: IDs are pairwise distinct, V-10)', () => {
  it('still decodes a valid two-device set', async () => {
    const a = await dev(5);
    const b = await dev(6);
    const encoded = gen.encode('device-set', sampleDeviceSet([b, a]));
    const v = await gen.decode('device-set', encoded);
    expect(v.active).toHaveLength(2);
    expect(gen.equalBytes(gen.encode('device-set', v), encoded)).toBe(true);
  });

  it('refuses two active entries that share a device_id and differ in mature_at_ms', async () => {
    const keys = await deviceKeys();
    const five = await dev(5, keys);
    const six = await dev(6, keys);
    // Strictly ascending as strings (the last part differs: 5 < 6), so only the id rule can refuse it.
    expect(five < six).toBe(true);
    expect(five.slice(0, 36)).toBe(six.slice(0, 36));
    const one = text(gen.encode('device-set', sampleDeviceSet([five])));
    const both = one.replace(`["${five}"]`, `["${five}","${six}"]`);
    expect(both).not.toBe(one);
    await expect(gen.decode('device-set', raw(both))).rejects.toBeInstanceOf(gen.Malformed);
  });

  it('refuses the same pair at encode, whatever order the caller lists them in', async () => {
    const keys = await deviceKeys();
    const five = await dev(5, keys);
    const six = await dev(6, keys);
    const other = await dev(7);
    for (const active of [[five, six], [six, five], [five, other, six]]) {
      expect(() => gen.encode('device-set', sampleDeviceSet(active)), `${active.length} entries`)
        .toThrow(gen.Malformed);
    }
  });
});

describe('gen.mjs CLI entry guard', () => {
  // Node real-paths import.meta.url for the main module but not process.argv[1], so a
  // guard that compares the two verbatim never runs main() when the invocation path holds
  // a symlinked directory. Exit 0 alone cannot tell "ran and passed" from "never ran", so
  // the drifted fixture below must exit 1 with the drift message, which only a run produces.
  it('runs --check when invoked through a symlinked directory', async () => {
    const root = realpathSync(mkdtempSync(join(tmpdir(), 'gen-guard-')));
    try {
      const genDir = join(root, 'real', 'scripts', 'authority-vectors');
      const vecDir = join(root, 'real', 'services', 'control-plane', 'internal', 'authoritycontract', 'testdata');
      mkdirSync(genDir, { recursive: true });
      mkdirSync(vecDir, { recursive: true });
      copyFileSync(gen.GENERATOR_PATH, join(genDir, 'gen.mjs'));
      symlinkSync(genDir, join(root, 'lnk'));
      const vectors = join(vecDir, 'vectors.json');
      const dirs = [genDir, join(root, 'lnk')]; // the real path (control), then the symlinked path
      const check = (dir: string) => spawnSync(process.execPath, [join(dir, 'gen.mjs'), '--check'], { encoding: 'utf8' });

      writeFileSync(vectors, JSON.stringify({ generator_sha384: '0'.repeat(96) }));
      for (const dir of dirs) {
        const r = check(dir);
        expect(r.status, dir).toBe(1);
        expect(r.stderr, dir).toContain('vectors.json was written by another gen.mjs');
      }

      const written = await gen.renderVectorFile([{ name: 'n', type: 't' }]);
      writeFileSync(vectors, written);
      for (const dir of dirs) {
        const r = check(dir);
        expect(r.status, dir).toBe(0);
        expect(r.stderr, dir).toBe('');
      }

      // A hand edit that keeps generator_sha384 fails on the content digest.
      writeFileSync(vectors, written.replace('"name":"n"', '"name":"m"'));
      for (const dir of dirs) {
        const r = check(dir);
        expect(r.status, dir).toBe(1);
        expect(r.stderr, dir).toContain('vectors.json was edited after gen.mjs wrote it');
      }

      // A vector inserted on the header line sits outside the digest; the header check refuses it.
      writeFileSync(vectors, written.replace('"vectors":[\n', '"vectors":[{"name":"x","type":"t"},\n'));
      for (const dir of dirs) {
        const r = check(dir);
        expect(r.status, dir).toBe(1);
        expect(r.stderr, dir).toContain('header line is not the form gen.mjs writes');
      }
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});

// The post-merge public-mirror gate runs gitleaks 8.30.1 over vectors.json. mirrorUnsafe is
// the generator's port of its generic-api-key rule and base64 decoder (gen.mjs section 5);
// these cases pin what that port was measured to do against the real binary.
describe('gen.mjs public-mirror safety (mirrorUnsafe)', () => {
  // Built at run time: a literal high-entropy value next to a keyword would trip this
  // repository's own secret scanners.
  const secret = Buffer.from(Array.from({ length: 48 }, (_, i) => (i * 37 + 11) % 256)).toString('base64');
  const pair = (keyword: string, value: string) => JSON.stringify({ x: [`abcdef${keyword}`, value] });
  const b64 = (s: string | Buffer) => Buffer.from(s as string, 'latin1').toString('base64');
  const wrap = (s: string, times: number) => {
    for (let i = 0; i < times; i++) s = b64(s);
    return s;
  };
  const asVector = (bytesB64: string) =>
    `{"name":"pos/x/typical","type":"x","bytes_b64":"${bytesB64}","expect":{"reason":"ok","verdict":"ok"}}`;

  it('flags a value ending in a keyword followed by a high-entropy value', () => {
    expect(gen.mirrorUnsafe(pair('Api', secret))).toBe(true);
    expect(gen.mirrorUnsafe(pair('key', secret))).toBe(true);
    expect(gen.mirrorUnsafe(pair('SECRET', secret))).toBe(true);
    // The entropy threshold from above: 12 distinct characters is log2(12) = 3.58 bits.
    expect(gen.mirrorUnsafe(pair('Api', 'Zk3pQ9xLm2Vw')), '12 distinct characters, 3.58 bits').toBe(true);
  });

  it('flags the same pair inside a base64-encoded message string, to gitleaks\' decode depth', () => {
    const text = pair('Api', secret);
    expect(gen.mirrorUnsafe(asVector(b64(text)))).toBe(true);
    expect(gen.mirrorUnsafe(asVector(wrap(text, 5)))).toBe(true); // --max-decode-depth 5
    expect(gen.mirrorUnsafe(asVector(wrap(text, 6)))).toBe(false); // measured: 8.30.1 stops here
  });

  it('does not flag what the rule does not match', () => {
    // Each of the next two holds a digit, so the identifier allowlist does not apply and
    // only the entropy check can clear them.
    expect(gen.mirrorUnsafe(pair('Api', 'a1'.repeat(24))), 'entropy 1 bit').toBe(false);
    expect(gen.mirrorUnsafe(pair('Api', 'Zk3pQ9xLm2V')), '11 distinct characters, 3.46 bits: just under the threshold').toBe(false);
    // The rule's 10- and 12-character minimums cannot be isolated from the entropy check:
    // a value under 12 characters cannot reach 3.5 bits (log2(11) < 3.5), so there is no
    // value only the length rule could clear.
    expect(gen.mirrorUnsafe(pair('', secret)), 'no keyword').toBe(false);
    expect(gen.mirrorUnsafe(pair('aPI', secret)), 'the keyword spelling is case-sensitive for api').toBe(false);
    expect(gen.mirrorUnsafe('{"fields":["rsk_public_keys","cak_public_keys"]}'), 'identifier-shaped secret').toBe(false);
  });

  it('decodes only text whose bytes are all in 9..126, as gitleaks does', () => {
    const text = pair('Api', secret);
    expect(gen.mirrorUnsafe(asVector(b64(text + '\x01'))), 'control byte').toBe(false);
    expect(gen.mirrorUnsafe(asVector(b64(Buffer.from(text + ' caf\u00e9', 'utf8')))), 'non-ASCII').toBe(false);
    expect(gen.mirrorUnsafe(asVector(b64(text + '\n\tz'))), 'tab and newline decode').toBe(true);
  });

  it('does not flag the committed vectors.json', () => {
    expect(gen.mirrorUnsafe(readFileSync(gen.VECTORS_PATH, 'utf8'))).toBe(false);
  }, 60_000);

});

// The wiring of the guard: the loop has to consult the predicate and has to start every
// draw from scratch. Both are driven with an injected predicate and neither writes
// VECTORS_PATH (buildSafeVectorFile only builds; main() writes).
describe('gen.mjs mirror-gate guard wiring (buildSafeVectorFile)', () => {
  const quiet = () => vi.spyOn(console, 'error').mockImplementation(() => {});

  it('gives up with a clear error after exactly maxDraws flagged draws', async () => {
    const log = quiet();
    try {
      let calls = 0;
      const always = () => {
        calls++;
        return true;
      };
      await expect(gen.buildSafeVectorFile(2, always)).rejects.toThrow(/2 draws in a row would trip the public-mirror secret scan/);
      expect(calls).toBe(2);
      expect(log).toHaveBeenCalledTimes(2);
    } finally {
      log.mockRestore();
    }
  }, 120_000);

  it('returns the first draw the predicate passes, and that redraw reuses no key material', async () => {
    const log = quiet();
    try {
      const seen: string[] = [];
      const flagFirst = (text: string) => {
        seen.push(text);
        return seen.length === 1;
      };
      const out = await gen.buildSafeVectorFile(3, flagFirst);
      expect(out.draws).toBe(2);
      expect(out.body).toBe(seen[1]);
      expect(seen[0]).not.toBe(seen[1]);

      // Fresh draws differ anyway (digests are random), so compare what resetDraw() resets:
      // the P-384 pool and the RSA EK key. Draw one's points and SPKIs must not come back.
      const keyMaterial = (text: string) => {
        const out = new Set<string>();
        for (const v of JSON.parse(text).vectors) {
          for (const k of ['point_b64', 'spki_b64']) if (v.input?.[k]) out.add(v.input[k]);
        }
        return out;
      };
      const [first, second] = seen.map(keyMaterial);
      expect(first.size).toBeGreaterThan(10);
      expect([...first].filter((k) => second.has(k))).toEqual([]);
    } finally {
      log.mockRestore();
    }
  }, 120_000);
});
