// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import * as gen from './gen.mjs';

type Vec = { name: string; type: string; expect: { reason: string } };

// The DoR §3.1 codec vectors that A1 owns, by name.
const REQUIRED = [
  'neg/status/u-2^53', 'neg/status/u-1.0', 'neg/status/u-1e0', 'neg/status/bom', 'neg/status/space',
  'neg/status/escape', 'neg/status/non-ascii', 'neg/status/invalid-utf8',
  'neg/anchors/cap-plus-one', 'neg/signed/cap-plus-one', 'neg/device-set/cap-plus-one', 'neg/device-wrap/cap-plus-one',
  'neg/device-intent/cap-plus-one', 'neg/device-bundle/cap-plus-one', 'neg/device-history/cap-plus-one',
  'neg/recovery-bind/cap-plus-one', 'neg/relay-recover/cap-plus-one', 'neg/device-result/cap-plus-one',
  'neg/device-secrets/cap-plus-one',
  'neg/delegation/a-unsorted', 'neg/delegation/a-duplicate', 'neg/delegation/a-nested-array',
  'neg/query/base64-QR', 'neg/query/base64-trailing-bits', 'neg/query/base64-missing-padding', 'neg/query/base64-newline',
  'neg/anchors/point-compressed', 'neg/anchors/point-off-curve', 'neg/anchors/point-hybrid',
  'prim/p384-point/hybrid-06', 'prim/p384-point/hybrid-07', 'prim/p384-point/compressed', 'prim/p384-point/off-curve',
  'neg/bundle/sig-95', 'neg/bundle/sig-97', 'neg/bundle/sig-der', 'neg/bundle/pss-511',
  'prim/p384-sig/95', 'prim/p384-sig/97', 'prim/p384-sig/der', 'prim/pss-sig/511',
  'prim/p384-sig/high-s', 'prim/p384-sig/r-zero', 'prim/p384-sig/s-zero', 'prim/p384-sig/r-eq-n', 'prim/p384-sig/s-eq-n',
  'neg/ek-binding/spki-2048-bit', 'neg/ek-binding/spki-e-3', 'neg/ek-binding/spki-e-65539',
  'neg/ek-binding/spki-pss-oid', 'neg/ek-binding/spki-absent-null', 'neg/ek-binding/spki-trailing-bytes',
  'neg/intent/request-id-v1', 'neg/legacy-ek-pop/age-claim-text', 'neg/query/age-claim-text', 'neg/sig/session-bind-text',
  'neg/signed/type-not-msg2', 'neg/signed/delegation-sig-b-empty', 'neg/signed/succession-sig-b-empty',
  'neg/signed/status-sig-b-present', 'neg/signed/device-status-sig-b-present',
  'pos/delegation/max', 'pos/bundle/max', 'pos/recovery-bind/max', 'pos/relay-recover/max', 'pos/device-set/max',
  'pos/device-set/dev-319', 'neg/device-set/dev-320', 'neg/device-set/dev-id-mismatch',
  'neg/device-set/key-without-digest', 'neg/device-set/unbind-without-bound-key', 'neg/device-set/refresh-to-bound-key',
  'neg/device-set/pending-in-active', 'neg/device-set/duplicate-id',
  'pos/device-set/bound-no-slot', 'neg/device-set/pending-same-id-other-maturity',
  'neg/device-set/pending-same-id-other-dkx',
  'pos/cp-evidence/all-14-factors', 'neg/cp-evidence/backup-email-recovery', 'neg/cp-evidence/duplicate-factor',
  'pos/recovery-bind/bind', 'pos/recovery-bind/cancel', 'pos/recovery-bind/unbind',
  'neg/recovery-bind/escrow-equals-recovery-key', 'neg/recovery-bind/escrow-off-curve',
  'pos/relay-recover/form-r', 'pos/relay-recover/form-c', 'pos/relay-recover/form-b', 'neg/relay-recover/c-plus-ek-binding',
  'pos/query-resp/failure-form',
  'pos/chain-head/genesis', 'pos/device-head/genesis-monitor', 'pos/device-head/genesis-own',
  'pos/chain-head/records-at-caps', 'pos/device-head/records-at-caps', 'neg/chain-head/seq-record-over',
  'neg/chain-head/root-record-over', 'neg/chain-head/recover-record-over', 'neg/device-head/admitted-record-over',
  'neg/device-head/secrets-record-over', 'neg/device-head/chain-head-digest-mismatch',
  'prim/device-id/a', 'prim/safety-number/a', 'prim/age-claim-v2/valid', 'prim/session-bind-v1/valid',
  'prim/credential-epoch-digest/null', 'prim/credential-epoch-digest/empty', 'prim/credential-epoch-digest/value',
  'prim/population-digest/three', 'prim/tls-pin/a', 'prim/ek-spki/valid',
  'prim/ek-spki/bitstring-unused-bits', 'prim/ek-spki/modulus-4097',
];

// The DoR §3.1 verification vectors that A2 owns, by name and vector type: at
// least one of every A2 type (gen.A2_TYPES) and of every family the A2
// builders emit. Names are the as-built ones in vectors.json.
const A2_REQUIRED: Record<string, string> = {
  // Trust: T3, T4 halts, the T6–T8 windows, T7 supersession, T8 promotion.
  'trust/t4/succession-pair/ab': 'chain',
  'trust/t3/a-only': 'chain',
  'trust/window/t6-at-not-after': 'chain',
  'trust/window/t7-at-not-after': 'bundle-verify',
  'trust/t7/superseded-past-valid-until': 'chain',
  'trust/t8/standby-promotion': 'bundle-verify',
  // Pins and the §2.13 state verdicts.
  'pin/accepted': 'chain',
  'pin/acceptance-needs-a-fresh-status': 'chain',
  'pin/revoked': 'chain',
  'd136/ek-rollback-status': 'chain',
  'd136/held-pending-anb-never': 'chain',
  'p1e/device-status-passed-as-status': 'chain',
  'sets/operator-suspend-unsuspend-from-su': 'chain',
  'status/pending-from-active': 'chain',
  'status/pending-from-suspended': 'chain',
  'status/pending-erase': 'chain',
  'status/root-revoked': 'chain',
  'row/02/pop-pending': 'chain',
  'row/02/pop-disputed': 'chain',
  'row/03/no-pop-recovery': 'chain',
  // The §2.9 rows, the operator signature, D-182.
  'row/01/enroll-new': 'bundle-verify',
  'row/19/erase-operator': 'bundle-verify',
  'operator/unlisted-key': 'bundle-verify',
  'neg/d182/replace-claims-recovery-from-unverified': 'bundle-verify',
  // R3, R4 and the persisted head.
  'r3/row-16/h-pend-unique': 'bundle-verify',
  'r4/fix-refuses-h-act': 'bundle-verify',
  'head/p-suspended-activation-has-no-bind-root': 'chain',
  // The binding proof and rows 20–21.
  'row/20/from-pr': 'bundle-verify',
  'neg/proof/6-key-burned-by-an-activated-recover': 'bundle-verify',
  'row/21/cancel-by-the-pending-recovery-key': 'bundle-verify',
  // Restore, row 22.
  'row/22/from-held-pr-active': 'bundle-verify',
  'neg/row-22/activation-fix': 'bundle-verify',
  // R4's fix retention, one vector per arm of the rule (red-team RT-1, RT-1b),
  // the trust's realm (review B-SF1) and the first-sight pin past genesis (RT-2).
  'r4/retain/activation-then-held': 'chain',
  'r4/retain/activation-then-later-activation': 'chain',
  'r4/retain/activation-then-earlier-activation': 'chain',
  'r4/retain/held-then-earlier-activation': 'chain',
  'r4/retain/pending-then-newer-held': 'chain',
  'r4/retain/pending-then-older-pending': 'chain',
  'realm/status-names-another-realm': 'chain',
  'pin/first-sight-pin-past-genesis': 'chain',
  // Device rules, normalization, published-intent positions, envelope patterns.
  'apply/scenario/00-add_device': 'apply-device-op',
  'normalize/matrix/11111': 'normalize',
  'for-op/intent/00-enroll_new-user': 'for-op',
  'for-op/intent/00-enroll_new-user/flip-legacy_ek_digest': 'for-op',
  'for-op/recovery-bind/02-bind': 'for-op',
  'pattern/relay-recover/form-r': 'pattern',
  // The device ledger and P1.
  'ledger/c4-4-completion-and-activation-tie': 'device-ledger',
  'ledger/row-18-erase-keeps-the-pending-device-and-the-slot': 'device-ledger',
  'ledger/fork': 'device-ledger',
  'ledger/add-device-built-on-an-earlier-dseq': 'device-ledger',
  'ledger/add-device-built-on-another-device-head': 'device-ledger',
  'p1/a-b/add-device-leaves-the-status-unchanged': 'p1',
  'p1/f/bind-recovery-leaves-the-status-unchanged': 'p1',
  // Device wraps.
  'wrap/head-from-verified-ledger': 'device-ledger',
  'wrap/current': 'device-wrap-open',
  'wrap/pkcs8-publickey-field-lies': 'device-wrap-open',
};

// The Reasons the verifier never returns (plan A1): the caller's own. Go's
// TestEveryReasonHasAVector exempts exactly these two.
const EXEMPT_REASONS = new Set(['network', 'sender_side']);

// missingReasons lists, sorted, every Reason the verifier can return that no
// vector expects (DoR §3.1: "every Reason in §2.12 has at least one vector").
function missingReasons(vectors: Vec[]): string[] {
  const seen = new Set(vectors.map((v) => v.expect.reason));
  return Object.keys(gen.VERDICT)
    .filter((r) => !EXEMPT_REASONS.has(r) && !seen.has(r))
    .sort();
}

// Go's Reason strings, read from reason.go: the closed set
// TestEveryReasonHasAVector iterates (AllReasons).
function goReasons(): string[] {
  const src = readFileSync(
    resolve(dirname(gen.GENERATOR_PATH), '../../services/control-plane/internal/authoritycontract/reason.go'),
    'utf8',
  );
  return [...src.matchAll(/\bReason\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]).sort();
}

describe('vectors.json coverage (DoR §3.1, AC7)', () => {
  it('exists and was written by this gen.mjs', async () => {
    expect(existsSync(gen.VECTORS_PATH)).toBe(true);
    const text = readFileSync(gen.VECTORS_PATH, 'utf8');
    const file = JSON.parse(text);
    expect(file.generator_sha384).toBe(await gen.generatorDigest());
    expect(text.slice(0, text.indexOf('\n'))).toMatch(gen.HEADER_LINE);
    expect(file.vectors_sha384).toBe(await gen.vectorsDigest(text.slice(text.indexOf('\n') + 1)));
  });

  it('has unique names, every required vector, and both polarities for every layout', () => {
    const vectors: Vec[] = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8')).vectors;
    const names = new Set(vectors.map((v) => v.name));
    expect(names.size).toBe(vectors.length);
    for (const n of REQUIRED) expect(names.has(n), n).toBe(true);
    for (const tag of Object.keys(gen.LAYOUTS)) {
      expect(vectors.some((v) => v.type === tag && v.expect.reason === 'ok'), `${tag} positive`).toBe(true);
      expect(vectors.some((v) => v.type === tag && v.expect.reason === 'malformed'), `${tag} negative`).toBe(true);
    }
  });

  it('pins the ordered position names of every layout, once each', () => {
    const vectors: Vec[] = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8')).vectors;
    const fieldVectors = vectors.filter((v) => v.type === 'layout-fields');
    for (const tag of Object.keys(gen.LAYOUTS)) {
      expect(fieldVectors.filter((v) => v.name === `prim/layout-fields/${tag}`), `${tag} layout-fields`).toHaveLength(1);
    }
    expect(fieldVectors).toHaveLength(Object.keys(gen.LAYOUTS).length);
  });

  it('has every A2 required vector, with its type, and one of every A2 type', () => {
    const vectors: Vec[] = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8')).vectors;
    const types = new Map(vectors.map((v) => [v.name, v.type]));
    const wrong = Object.entries(A2_REQUIRED).filter(([n, t]) => types.get(n) !== t);
    expect(wrong.map(([n, t]) => `${n} (${t}): ${types.get(n) ?? 'missing'}`)).toEqual([]);
    expect(gen.A2_TYPES.filter((t) => !Object.values(A2_REQUIRED).includes(t))).toEqual([]);
  });

  it("has the generator's Reason set equal to Go's", () => {
    expect(Object.keys(gen.VERDICT).sort()).toEqual(goReasons());
  });

  it('has a vector for every Reason the verifier can return', () => {
    const vectors: Vec[] = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8')).vectors;
    expect(missingReasons(vectors)).toEqual([]);
  });

  it('names exactly the Reason whose vectors are gone, and never an exempt one', () => {
    const vectors: Vec[] = JSON.parse(readFileSync(gen.VECTORS_PATH, 'utf8')).vectors;
    for (const r of Object.keys(gen.VERDICT).filter((x) => !EXEMPT_REASONS.has(x))) {
      expect(missingReasons(vectors.filter((v) => v.expect.reason !== r)), r).toEqual([r]);
    }
    const none = missingReasons([]);
    expect(none).toHaveLength(Object.keys(gen.VERDICT).length - EXEMPT_REASONS.size);
    for (const r of EXEMPT_REASONS) expect(none).not.toContain(r);
  });
});
