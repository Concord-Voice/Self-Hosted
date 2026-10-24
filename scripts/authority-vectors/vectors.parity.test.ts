// @vitest-environment node
// DoR §3.1 cross-language check, Node side: every committed vector verifies
// under the second implementation (precedent: stepUpPurpose.parity.test.ts).
// This file lives under scripts/ (private): a test under client/ that read
// scripts/ would ENOENT in the public mirror's frontend CI.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import * as gen from './gen.mjs';

type Vec = { name: string; type: string; bytes_b64?: string; input?: Record<string, any>; expect: Record<string, any>;
  [k: string]: any };

const file = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8'));
const vectors: Vec[] = file.vectors;
const b = (s: string) => gen.b64d(s);
const ok = (v: Vec) => v.expect.reason === 'ok';

async function settles(p: Promise<unknown>): Promise<boolean> {
  try {
    await p;
    return true;
  } catch (e) {
    if (!(e instanceof gen.Malformed)) throw e;
    return false;
  }
}

const PRIM: Record<string, (v: Vec) => Promise<void>> = {
  'p384-point': async (v) => expect(await settles(gen.importP384(b(v.input!.point_b64)))).toBe(ok(v)),
  'p384-sig': async (v) =>
    expect(await gen.verifyP384(b(v.input!.point_b64), b(v.input!.msg_b64), b(v.input!.sig_b64))).toBe(ok(v)),
  'pss-sig': async (v) =>
    expect(await gen.verifyLegacyPSS(b(v.input!.spki_b64), b(v.input!.msg_b64), b(v.input!.sig_b64))).toBe(ok(v)),
  'ek-spki': async (v) => {
    expect(await settles(gen.checkEkSpkiWebCrypto(b(v.input!.spki_b64)))).toBe(ok(v));
    expect(await settles((async () => gen.checkEkSpkiTemplate(b(v.input!.spki_b64)))())).toBe(ok(v));
  },
  'device-id': async (v) => expect(await gen.deviceId(b(v.input!.dsk_b64))).toBe(v.expect.device_id),
  'safety-number': async (v) =>
    expect(await gen.safetyNumber(v.input!.account_id, b(v.input!.pin_digest_b64))).toBe(v.expect.safety_number),
  'age-claim-v2': async (v) => {
    const t = gen.ageClaimV2Bytes(v.input);
    expect(t ? gen.b64e(t) : null).toBe(ok(v) ? v.expect.text_b64 : null);
  },
  'session-bind-v1': async (v) => {
    const t = gen.sessionBindBytes(v.input);
    expect(t ? gen.b64e(t) : null).toBe(ok(v) ? v.expect.text_b64 : null);
  },
  'credential-epoch-digest': async (v) => {
    const d = await gen.credentialEpochDigest(v.input!.epoch, v.input!.epoch_valid);
    expect(d ? gen.b64e(d) : '').toBe(v.expect.digest_b64);
  },
  'population-digest': async (v) => {
    const recs = v.input!.records.map((r: any) => ({ account_id: r.account_id, legacy_ek_digest: b(r.legacy_ek_digest_b64) }));
    if (!ok(v)) return expect(await settles(gen.populationDigest(recs))).toBe(false);
    expect(gen.b64e(await gen.populationDigest(recs))).toBe(v.expect.digest_b64);
  },
  'tls-pin': async (v) => expect(gen.b64e(await gen.tlsPin(b(v.input!.point_b64)))).toBe(v.expect.digest_b64),
  'layout-fields': async (v) => {
    const fields = gen.LAYOUTS[v.input!.layout as string];
    expect(fields, `unknown layout ${v.input!.layout}`).toBeDefined();
    expect(fields.map(([name]: [string]) => name)).toEqual(v.input!.fields);
  },
  'device-head': async (v) => {
    const dh = b(v.bytes_b64!);
    const ch = b(v.input!.chain_head_b64);
    // The pairing check below is only about the digest. A negative must not pass
    // because the chain-head it travels with is itself broken.
    expect(await settles(gen.decode('chain-head', ch)), 'the paired chain-head decodes').toBe(true);
    let head: any;
    let paired = false;
    try {
      head = await gen.decode('device-head', dh);
      paired = gen.equalBytes(head.chain_head_digest, await gen.sha384(ch));
    } catch (e) {
      if (!(e instanceof gen.Malformed)) throw e;
    }
    expect(paired).toBe(ok(v));
    if (ok(v)) expect(gen.b64e(gen.encode('device-head', head))).toBe(v.expect.reencoded_b64);
  },
};

// PR-A2: the codec layer of every verification vector (plan, TS-side
// decision). Reasons are #2628's to verify, not this file's.
async function decodes(tag: string, s?: string) {
  if (!s) return null;
  const bytes = b(s);
  const values = await gen.decode(tag, bytes);
  expect(gen.equalBytes(gen.encode(tag, values), bytes), `${tag} re-encodes`).toBe(true);
  return values;
}

async function bundleParts(s: string) {
  const v = await decodes('bundle', s);
  for (const [n, t] of [['intent', 'intent'], ['result', 'result'], ['cp_authz', 'cp-authz'], ['ek_binding', 'ek-binding'],
    ['recovery_bind', 'recovery-bind'], ['held_status', 'signed']] as const) {
    if (v[n].length) await decodes(t, gen.b64e(v[n]));
  }
}

async function deviceBundleParts(s: string) {
  const v = await decodes('device-bundle', s);
  for (const [n, t] of [['device_intent', 'device-intent'], ['recovery_bind', 'recovery-bind'], ['cp_authz', 'cp-authz'],
    ['device_result', 'device-result']] as const) {
    if (v[n].length) await decodes(t, gen.b64e(v[n]));
  }
}

const PRIMARY_TAG: Record<string, string> = { chain: 'signed', 'device-ledger': 'signed', 'device-wrap-open': 'device-wrap',
  'apply-device-op': 'device-set', normalize: 'device-set' };
const P1_TAG: Record<string, string> = { status: 'signed', history: 'history', shape: 'bundle', 'bind-shape': 'recovery-bind' };

async function verificationVector(v: Vec) {
  expect(gen.VERDICT[v.expect.reason], `reason ${v.expect.reason}`).toBeDefined();
  expect(v.expect.verdict).toBe(gen.VERDICT[v.expect.reason]);
  await decodes('chain-head', v.expect.head_b64);
  await decodes('device-set', v.expect.set_b64);
  if (v.expect.device_head_b64) await decodes('device-head', v.expect.device_head_b64);
  if (v.expect.reason === 'malformed') return;
  await decodes('anchors', v.anchors);
  for (const s of v.trust ?? []) await decodes('signed', s);
  await decodes('chain-head', v.prev_head);
  for (const s of v.bundles ?? []) await bundleParts(s);
  await decodes('signed', v.fix);
  for (const s of v.device_bundles ?? []) await deviceBundleParts(s);
  await decodes('signed', v.device_status);
  for (const s of v.statuses ?? []) await decodes('signed', s);
  if (v.prev_device_head) {
    const dh = await decodes('device-head', v.prev_device_head);
    expect(gen.equalBytes(dh.chain_head_digest, await gen.sha384(b(v.prev_head)))).toBe(true);
  }
  if (v.input?.bind_b64) await decodes('recovery-bind', v.input.bind_b64);
  if (v.input?.new_dev) await gen.parseDev(v.input.new_dev);
  const okish = v.expect.reason === 'ok' || v.expect.reason === 'ok_accepted_pin';
  if (!v.bytes_b64 || (!okish && ['device-wrap-open', 'pattern', 'for-op'].includes(v.type))) return;
  if (v.type === 'bundle-verify') return bundleParts(v.bytes_b64);
  if (v.type === 'p1') {
    await decodes(P1_TAG[v.input!.mode], v.bytes_b64);
    return decodes(P1_TAG[v.input!.mode], v.input!.other_b64);
  }
  await decodes(PRIMARY_TAG[v.type] ?? v.input!.tag, v.bytes_b64);
}

for (const t of gen.A2_TYPES) PRIM[t] = verificationVector;

// A vector whose type has no PRIM handler is a layout vector. An unregistered type
// must fail here: decode() throws Malformed('unknown layout') for it, settles() reads
// that as a refusal, and a non-ok vector would then pass for the wrong reason.
async function layout(v: Vec) {
  expect(Object.hasOwn(gen.LAYOUTS, v.type), `${v.type}: no PRIM handler and no layout`).toBe(true);
  const bytes = b(v.bytes_b64!);
  if (!ok(v)) return expect(await settles(gen.decode(v.type, bytes))).toBe(false);
  const values = await gen.decode(v.type, bytes);
  expect(gen.b64e(gen.encode(v.type, values))).toBe(v.expect.reencoded_b64);
}

describe('authority vectors verify under the Node implementation', () => {
  it('were written by this gen.mjs (drift)', async () => {
    expect(file.generator_sha384).toBe(await gen.generatorDigest());
  });
  for (const v of vectors) {
    it(v.name, async () => {
      // Own keys only: PRIM[v.type] alone would resolve 'toString' or 'constructor'.
      await (Object.hasOwn(PRIM, v.type) ? PRIM[v.type] : layout)(v);
    }, 30_000);
  }
});
