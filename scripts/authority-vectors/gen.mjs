#!/usr/bin/env node
// gen.mjs — the #2553 account-key authority contract, second implementation.
//
// An independent Node implementation of the codec (EN1–EN6), every layout of
// the design of record's §2.5–§2.6 and §2.12 persisted heads, and the §2.4
// primitives. It writes
//   services/control-plane/internal/authoritycontract/testdata/vectors.json
// which Go (vectors_test.go) and vitest (vectors.parity.test.ts) verify.
// CI never runs it to generate; it only verifies.
//
// Dependency-free: node:fs, node:path and node:url for files, and the
// webcrypto globalThis.crypto for every hash, key and signature.
//   generate: node scripts/authority-vectors/gen.mjs
//   drift:    node scripts/authority-vectors/gen.mjs --check

import { readFileSync, realpathSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const { subtle } = globalThis.crypto;
export const GENERATOR_PATH = fileURLToPath(import.meta.url);
export const VECTORS_PATH = resolve(dirname(GENERATOR_PATH),
  '../../services/control-plane/internal/authoritycontract/testdata/vectors.json');

// ── 1. EN1–EN6 ────────────────────────────────────────────────────────────

export const PREFIX = 'concord-account-authority';
export const MAX_SAFE = 9007199254740991;

export class Malformed extends Error {
  constructor(detail) {
    super(`malformed: ${detail}`);
    this.reason = 'malformed';
  }
}

const S_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/+=-';
const S_SET = new Set(S_ALPHABET);
const EN2 = new Set([...S_ALPHABET, '"', ',', '[', ']'].map((c) => c.charCodeAt(0)));
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const B64_RE = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

export const isUUID = (s) => UUID_RE.test(s);
export const isUUIDv4 = (s) => isUUID(s) && s[14] === '4' && '89ab'.includes(s[19]);
export const utf8 = (s) => new TextEncoder().encode(s);
export const b64e = (b) => Buffer.from(b).toString('base64');
export const equalBytes = (a, b) => a.length === b.length && a.every((x, i) => x === b[i]);

export function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

// Strict base64 (RFC 4648 §3.5) plus EN5's value-level re-encode comparison.
export function b64d(s) {
  if (!B64_RE.test(s)) throw new Malformed('base64 form');
  const b = Buffer.from(s, 'base64');
  if (b.toString('base64') !== s) throw new Malformed('base64 canonical');
  return new Uint8Array(b);
}

export function parseU(s) {
  if (!/^(0|[1-9][0-9]*)$/.test(s) || s.length > 16 || BigInt(s) > BigInt(MAX_SAFE)) throw new Malformed('u');
  return Number(s);
}

// Kinds (EN3). `chk` names an extra rule; `hold` names the layout a nested
// position carries (used only to build typical vectors; the codec treats
// nested positions as opaque bytes).
const S = (max, chk) => ({ t: 's', max, chk });
const S36 = { t: 's36' };
const S36V4 = { t: 's36v4' };
const U = { t: 'u', min: 0, max: MAX_SAFE };
const U1 = { t: 'u', min: 1, max: MAX_SAFE };
const U01 = { t: 'u', min: 0, max: 1 };
const B = (n, chk) => ({ t: 'b', sizes: [n], chk });
const BONE = (...sizes) => ({ t: 'b', sizes });
const BUP = (n, hold) => ({ t: 'b', upTo: n, hold });
const E = (...vals) => ({ t: 'e', vals });
const A = (elem, min, max) => ({ t: 'a', elem, min, max });
const L = (elem, min, max) => ({ t: 'l', elem, min, max });
const opt = (k) => ({ ...k, empty: true });
const INF = Infinity;
const P97 = B(97, 'point');
const EKSPKI = B(550, 'ekspki');
const DEV = S(319, 'dev');
const D48 = B(48);
const D48o = opt(B(48));
const SIG = B(96);
const SIGo = opt(B(96));

// ── 2. Closed sets and layouts (DoR §2.5, §2.6, §2.12, §2.22, §3.3) ────────

const PUBLISHED_OPS = ['cancel_pending', 'enroll_existing', 'enroll_new', 'erase', 'recover', 'replace_root',
  'revoke_root', 'rotate_ek', 'rotate_root', 'suspend', 'unsuspend'];
const DEVICE_OPS = ['add_device', 'bind_recovery', 'cancel_recovery_bind', 'enroll_existing', 'enroll_new',
  'recover', 'replace_root', 'rotate_ek', 'rotate_root', 'unsuspend'];
const SCOPE = [...PUBLISHED_OPS, 'add_device', 'bind_recovery', 'cancel_recovery_bind', 'status'];
const STATES = ['absent', 'active', 'suspended', 'pending', 'disputed', 'erased'];
const PRIOR = ['', 'absent', 'active', 'suspended'];
const ASSURANCE = ['', 'initial', 'continuity', 'recovery', 'unverified'];
const SEGMENT = ['initial', 'continuity', 'recovery', 'unverified'];
const SUSPENDERS = ['operator', 'user'];
const ACTORS = ['operator', 'restore', 'user'];
const ROLES = ['actor-device', 'candidate', 'current-root', 'new-device', 'operator', 'recovery-key'];
export const FACTORS = ['admin-webauthn', 'backup-code', 'device-key', 'email', 'email-backup', 'erasure-list',
  'passkey', 'password', 'recovery-key', 'registration', 'session', 'sso', 'totp', 'webauthn'];
const BIND_KINDS = ['bind', 'cancel', 'unbind'];
const BIND_WAITS = ['', 'delta', 'now', 'replace'];
const SIGNED_TYPES = ['delegation', 'device-status', 'status', 'succession'];
const SECRETS_OPS = ['add_device', 'rotate_ek', 'rotate_root'];
const REFUSALS = ['authz_out_of_window', 'device_key_bound', 'device_limit', 'device_too_new', 'device_unknown',
  'ek_generation', 'ek_mismatch', 'ek_reused', 'expired', 'expiry_out_of_range', 'held', 'in_population',
  'insufficient_authorization', 'legacy_mismatch', 'malformed_for_op', 'policy_disabled', 'predecessor_mismatch',
  'recovery_key_bound', 'recovery_key_unbound', 'root_bound_elsewhere', 'root_revoked', 'root_reused',
  'stale_device_seq', 'stale_head', 'stale_recovery_gen', 'stale_seq', 'wrong_state'];
const ERRORS = ['malformed', 'bad_signature', 'bad_authz', 'request_conflict', 'too_large', 'store', 'signer',
  'clock', 'reconcile', 'not_sealed', 'busy', 'not_ready', 'not_leader', 'replication', 'disk'];

export const LAYOUTS = {
  intent: [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['op', E(...PUBLISHED_OPS)],
    ['expires_at_ms', U1], ['expected_seq', U], ['expected_recovery_gen', U], ['expected_head_digest', D48o],
    ['predecessor_root_digest', D48o], ['candidate_root', opt(P97)], ['ek_binding_digest', D48o],
    ['legacy_ek_digest', D48o], ['device_intent_digest', D48o], ['recovery_bind_digest', D48o]],
  sig: [['role', E(...ROLES)], ['digest', D48]],
  'legacy-ek-pop': [['realm_id', S36], ['account_id', S36], ['intent_digest', D48], ['legacy_ek_digest', D48]],
  'ek-binding': [['realm_id', S36], ['account_id', S36], ['root_digest', D48], ['ek_generation', U1],
    ['ek_alg', E('rsa-oaep-4096-sha256')], ['ek_public_key', EKSPKI]],
  'cp-authz': [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['intent_digest', D48],
    ['actor', E(...ACTORS)], ['asserted_at_ms', U1], ['evidence_digest', D48]],
  'cp-evidence': [['request_id', S36V4], ['factors', A(E(...FACTORS), 0, 14)], ['mfa_enrolled', U01],
    ['password_login', U01], ['recovery_window_clear', U01], ['operator_assisted', U01],
    ['credential_epoch_digest', D48o], ['evidence_salt', B(32)]],
  result: [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['intent_digest', D48],
    ['cp_authz_digest', D48], ['op', E(...PUBLISHED_OPS)], ['seq', U1], ['state', E(...STATES)],
    ['prior_state', E(...PRIOR)], ['suspended_by', A(E(...SUSPENDERS), 0, 2)], ['active_root', opt(P97)],
    ['root_revoked', U01], ['ek_binding_digest', D48o], ['pending_root', opt(P97)],
    ['pending_ek_binding_digest', D48o], ['legacy_ek_digest', D48o], ['legacy_pin_revoked', U01],
    ['prev_entry_digest', D48o], ['recovery_gen', U], ['activation_not_before_ms', U],
    ['assurance', E(...ASSURANCE)], ['issued_at_ms', U], ['delegation_gen', U], ['delegation_serial', U1]],
  status: [['realm_id', S36], ['account_id', S36], ['seq', U], ['head_entry_digest', D48o], ['state', E(...STATES)],
    ['prior_state', E(...PRIOR)], ['suspended_by', A(E(...SUSPENDERS), 0, 2)], ['active_root_digest', D48o],
    ['root_revoked', U01], ['ek_binding_digest', D48o], ['pending_root_digest', D48o],
    ['pending_ek_binding_digest', D48o], ['legacy_ek_digest', D48o], ['legacy_pin_revoked', U01],
    ['activation_not_before_ms', U], ['held', U01], ['as_of_ms', U], ['valid_until_ms', U],
    ['delegation_gen', U], ['delegation_serial', U1]],
  query: [['realm_id', S36], ['account_id', S36], ['signer_digest', D48], ['issued_at_ms', U],
    ['nonce', B(16)], ['after_dseq', U]],
  delegation: [['realm_id', S36], ['serial', U1], ['min_valid_serial', U], ['product_root_gen', U],
    ['rsk_public_keys', A(P97, 1, 4)], ['cak_public_keys', A(P97, 1, 4)], ['operator_public_keys', A(P97, 0, 4)],
    ['authority_tls_keys', A(P97, 1, 8)], ['cp_tls_pins', A(D48, 1, 8)], ['not_before_ms', U],
    ['not_after_ms', U], ['scope', A(E(...SCOPE), 1, 15)], ['accepted_versions', A(S(4, 'version'), 1, 8)],
    ['tau_ms', U], ['delta_ms', U], ['g_ms', U], ['cutover_at_ms', U]],
  succession: [['realm_id', S36], ['prev_root_gen', U], ['new_root_gen', U], ['new_primary_a', P97],
    ['new_primary_b', P97], ['new_recovery_a', P97], ['new_recovery_b', P97]],
  anchors: [['realm_id', S36], ['primary_a', P97], ['primary_b', P97], ['recovery_a', P97], ['recovery_b', P97]],
  signed: [['type', E(...SIGNED_TYPES)], ['msg', BUP(4096)], ['sig_a', SIG], ['sig_b', SIGo]],
  statuses: [['items', L(BUP(6144, 'signed'), 0, 500)]],
  trust: [['items', L(BUP(6144, 'signed'), 1, 256)]],
  bundle: [['intent', BUP(2048, 'intent')], ['sig_candidate', SIGo], ['sig_current_root', SIGo],
    ['sig_operator', SIGo], ['sig_recovery', SIGo], ['legacy_pop_sig', opt(B(512))], ['legacy_spki', opt(EKSPKI)],
    ['ek_binding', opt(BUP(2048, 'ek-binding'))], ['ek_binding_sig', SIGo],
    ['recovery_bind', opt(BUP(1024, 'recovery-bind'))], ['recovery_bind_sig', SIGo],
    ['cp_authz', BUP(1024, 'cp-authz')], ['cp_authz_sig', SIGo], ['result', BUP(4096, 'result')],
    ['result_sig', SIG], ['held_status', opt(BUP(6144, 'signed'))]],
  history: [['account_id', S36], ['bundles', L(BUP(32768, 'bundle'), 0, 200)]],
  relay: [['intent', BUP(2048, 'intent')], ['sig_current_root', SIGo], ['legacy_pop_sig', opt(B(512))],
    ['legacy_spki', opt(EKSPKI)]],
  'query-req': [['query', BUP(1024, 'query')], ['query_sig', BONE(96, 512)], ['legacy_spki', opt(EKSPKI)]],
  'query-resp': [['status', opt(BUP(6144, 'signed'))], ['bundles', L(BUP(32768, 'bundle'), 0, 16)],
    ['device_status', opt(BUP(2048, 'signed'))], ['device_bundles', L(BUP(8192, 'device-bundle'), 0, 16)]],
  'device-set': [['realm_id', S36], ['account_id', S36], ['active', A(DEV, 0, 10)], ['pending', opt(DEV)],
    ['recovery_key', opt(P97)], ['recovery_bind_digest', D48o], ['replacement_key', opt(P97)],
    ['replacement_bind_digest', D48o], ['replacement_nb_ms', U]],
  'device-intent': [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['op', E(...DEVICE_OPS)],
    ['salt', B(32)], ['expires_at_ms', U], ['expected_seq', U], ['expected_head_digest', D48o],
    ['expected_dseq', U], ['expected_device_head', D48o], ['actor_device_id', opt(S36V4)],
    ['new_device_id', opt(S36V4)], ['new_device_dsk', opt(P97)], ['new_device_dkx', opt(P97)],
    ['removed_device_ids', A(S36V4, 0, 9)], ['recovery_bind_digest', D48o], ['bind_wait', E(...BIND_WAITS)]],
  'device-result': [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['device_intent_digest', D48],
    ['cp_authz_digest', D48o], ['op', E(...DEVICE_OPS)], ['dseq', U1], ['prev_device_entry_digest', D48o],
    ['chain_seq', U1], ['chain_entry_digest', D48], ['device_set_digest', D48], ['issued_at_ms', U],
    ['delegation_gen', U], ['delegation_serial', U1]],
  'device-status': [['realm_id', S36], ['account_id', S36], ['chain_seq', U], ['chain_head_digest', D48o],
    ['dseq', U], ['device_head_digest', D48o], ['device_set_digest', D48], ['as_of_ms', U],
    ['valid_until_ms', U], ['delegation_gen', U], ['delegation_serial', U1]],
  'device-bundle': [['device_intent', opt(BUP(2048, 'device-intent'))], ['sig_root', SIGo], ['sig_actor', SIGo],
    ['sig_new_device', SIGo], ['recovery_bind', opt(BUP(1024, 'recovery-bind'))], ['sig_recovery', SIGo],
    ['sig_recovery_prior', SIGo], ['cp_authz', opt(BUP(1024, 'cp-authz'))], ['cp_authz_sig', SIGo],
    ['device_result', BUP(1024, 'device-result')], ['device_result_sig', SIG]],
  'device-history': [['account_id', S36], ['bundles', L(BUP(8192, 'device-bundle'), 0, 16)]],
  'recovery-bind': [['realm_id', S36], ['account_id', S36], ['request_id', S36V4], ['kind', E(...BIND_KINDS)],
    ['expires_at_ms', U], ['recovery_key', opt(P97)], ['target_digest', D48o], ['chain_seq', U],
    ['chain_entry_digest', D48o], ['binding_root_digest', D48o], ['escrow_public_key', opt(P97)]],
  'device-wrap': [['realm_id', S36], ['account_id', S36], ['recipient_device_id', S36V4],
    ['device_entry_digest', D48], ['epk', P97], ['nonce', B(12)], ['ct', BUP(4096)]],
  'device-secrets': [['root_pkcs8', opt(BUP(256))], ['ek_pkcs8', BUP(2600)]],
  'relay-recover': [['intent', opt(BUP(2048, 'intent'))], ['sig_candidate', SIGo], ['sig_recovery', SIG],
    ['ek_binding', opt(BUP(2048, 'ek-binding'))], ['ek_binding_sig', SIGo],
    ['recovery_bind', opt(BUP(1024, 'recovery-bind'))], ['recovery_bind_sig', SIGo],
    ['device_intent', opt(BUP(2048, 'device-intent'))], ['sig_new_device', SIGo]],
  submit: [['intent', BUP(2048, 'intent')], ['sig_candidate', SIGo], ['sig_current_root', SIGo],
    ['sig_operator', SIGo], ['sig_recovery', SIGo], ['legacy_pop_sig', opt(B(512))], ['legacy_spki', opt(EKSPKI)],
    ['ek_binding', opt(BUP(2048, 'ek-binding'))], ['ek_binding_sig', SIGo],
    ['recovery_bind', opt(BUP(1024, 'recovery-bind'))], ['recovery_bind_sig', SIGo],
    ['cp_authz', BUP(1024, 'cp-authz')], ['cp_authz_sig', SIG], ['cp_evidence', BUP(1024, 'cp-evidence')],
    ['device_intent', opt(BUP(2048, 'device-intent'))], ['sig_actor', SIGo], ['sig_new_device', SIGo]],
  'import-item': [['bundle', opt(BUP(32768, 'bundle'))], ['device_bundle', opt(BUP(8192, 'device-bundle'))],
    ['cp_evidence', opt(BUP(1024, 'cp-evidence'))]],
  'import-batch': [['items', L(BUP(56320, 'import-item'), 1, 64)]],
  'legacy-batch': [['rows', A(S(101, 'legacyRow'), 1, 8192)]],
  committed: [['result', BUP(4096, 'result')], ['result_sig', SIG],
    ['device_result', opt(BUP(1024, 'device-result'))], ['device_result_sig', SIGo]],
  refused: [['request_id', S36V4], ['code', E(...REFUSALS)]],
  error: [['code', E(...ERRORS)]],
  head: [['status', BUP(6144, 'signed')], ['latest_bundle', opt(BUP(32768, 'bundle'))],
    ['device_status', BUP(2048, 'signed')]],
  'status-req': [['account_ids', A(S36, 1, 500)]],
  'hold-req': [['bundles', L(BUP(32768, 'bundle'), 1, 16)]],
  'pending-heads': [['account_ids', A(S36, 0, 500)], ['next', opt(S36)]],
  signer: [['realm_id', S36], ['product_root_gen', U], ['delegation_serial', U1], ['restore_epoch', U],
    ['reconciled', U01], ['max_commit_id', U], ['held_count', U], ['legacy_sealed', U01],
    ['population_digest', D48o], ['tick_age_s', U], ['cancel_unavailable_ms', U], ['extension_cap_ms', U],
    ['n_cancel_ms', U], ['v_ms', U], ['role', E('leader', 'standby')], ['rsk_digest', D48],
    ['disk_ready', U01], ['replication_required', U01], ['replication_ready', U01], ['archive_ok', U01],
    ['clock_synced', U01], ['submit_layouts', A(S(16), 1, 8)], ['trust', BUP(2097152, 'trust')]],
  'device-submit': [['device_intent', opt(BUP(2048, 'device-intent'))], ['sig_root', SIGo], ['sig_actor', SIGo],
    ['sig_new_device', SIGo], ['recovery_bind', opt(BUP(1024, 'recovery-bind'))], ['sig_recovery', SIGo],
    ['sig_recovery_prior', SIGo], ['cp_authz', BUP(1024, 'cp-authz')], ['cp_authz_sig', SIG],
    ['cp_evidence', BUP(1024, 'cp-evidence')]],
  'device-committed': [['device_bundle', BUP(8192, 'device-bundle')]],
  'chain-head': [['realm_id', S36], ['account_id', S36], ['head_result', opt(BUP(4096, 'result'))],
    ['fix', opt(BUP(6144, 'signed'))], ['active_ek_spki', opt(EKSPKI)], ['active_ek_generation', U],
    ['pending_ek_spki', opt(EKSPKI)], ['pending_ek_generation', U], ['pending_recovery_key', opt(P97)],
    ['seq_index', L(S(129, 'seqRec'), 0, INF)], ['root_index', A(S(208, 'rootRec'), 0, INF)],
    ['ek_index', A(D48, 0, INF)], ['recover_index', A(S(81, 'recoverRec'), 0, INF)]],
  'device-head': [['realm_id', S36], ['account_id', S36], ['self_device_id', opt(S36V4)],
    ['last_device_result', opt(BUP(1024, 'device-result'))], ['device_set', opt(BUP(4608, 'device-set'))],
    ['chain_head_digest', D48], ['admitted_by', A(S(73, 'admittedRec'), 0, 11)],
    ['secrets', L(S(946, 'secretsRec'), 0, INF)]],
};

export const CAPS = {
  anchors: 1024, signed: 6144, 'device-set': 4608, 'device-wrap': 6144, 'device-intent': 2048,
  'device-bundle': 8192, 'device-history': 180224, 'recovery-bind': 1024, 'relay-recover': 10278,
  'device-result': 1024, 'device-secrets': 4080, trust: 2097152, 'chain-head': 16777216, 'device-head': 16777216,
};

const b64Len = (n) => Math.ceil(n / 3) * 4;
function kmax(k) {
  switch (k.t) {
    case 's': return k.max + 2;
    case 's36': case 's36v4': return 38;
    case 'u': return String(k.max).length;
    case 'b': return b64Len(k.upTo ?? Math.max(...k.sizes)) + 2;
    case 'e': return Math.max(...k.vals.map((v) => v.length)) + 2;
    default:
      if (k.max === INF) return INF;
      return k.max === 0 ? 2 : 2 + k.max * kmax(k.elem) + k.max - 1;
  }
}

export function maxLen(tag) {
  let n = `["${PREFIX}",1,"${tag}"]`.length;
  for (const [, k] of LAYOUTS[tag]) n += 1 + kmax(k);
  return n;
}
export const capOf = (tag) => CAPS[tag] ?? maxLen(tag);

const parts = (s, n) => {
  const p = s.split('.');
  if (p.length !== n) throw new Malformed('record parts');
  return p;
};
const exact = (s, n) => {
  if (b64d(s).length !== n) throw new Malformed('record digest');
};

// Synchronous extra string rules. `dev` also needs DeviceID, which is async
// (webcrypto); decode checks it, encode checks the shape.
const STRCHK = {
  version: (s) => { if (!/^[1-9][0-9]{0,3}$/.test(s)) throw new Malformed('version'); },
  legacyRow: (s) => { const p = parts(s, 2); if (!isUUID(p[0])) throw new Malformed('legacy row'); exact(p[1], 48); },
  seqRec: (s) => { const p = parts(s, 2); exact(p[0], 48); if (p[1] !== '') exact(p[1], 48); },
  rootRec: (s) => {
    const p = parts(s, 3);
    exact(p[0], 48);
    checkPoint(b64d(p[1]));
    if (!SEGMENT.includes(p[2])) throw new Malformed('root record assurance');
  },
  recoverRec: (s) => { const p = parts(s, 2); parseU(p[0]); exact(p[1], 48); },
  admittedRec: (s) => {
    const p = parts(s, 2);
    if (!isUUIDv4(p[0]) || (p[1] !== '' && !isUUIDv4(p[1]))) throw new Malformed('admitted record');
  },
  secretsRec: (s) => {
    const p = parts(s, 4);
    exact(p[0], 48);
    if (!SECRETS_OPS.includes(p[1])) throw new Malformed('secrets record op');
    if (p[2] !== '') checkPoint(b64d(p[2]));
    checkEkSpkiTemplate(b64d(p[3]));
  },
  dev: (s) => {
    const p = s.split('.');
    if (p.length !== 4 || !isUUIDv4(p[0])) throw new Malformed('dev');
    checkPoint(b64d(p[1]));
    checkPoint(b64d(p[2]));
    parseU(p[3]);
  },
};

function checkStr(k, s) {
  if (typeof s !== 'string') throw new Malformed('not a string');
  if (s === '') {
    if (k.empty || (k.t === 'e' && k.vals.includes(''))) return;
    throw new Malformed('empty');
  }
  if (k.t === 's') {
    if (s.length > k.max || ![...s].every((c) => S_SET.has(c))) throw new Malformed('s');
    if (k.chk) STRCHK[k.chk](s);
  } else if (k.t === 's36') {
    if (!isUUID(s)) throw new Malformed('s36');
  } else if (k.t === 's36v4') {
    if (!isUUIDv4(s)) throw new Malformed('s36v4');
  } else if (k.t === 'e') {
    if (!k.vals.includes(s)) throw new Malformed('enum');
  } else {
    throw new Malformed('not a string kind');
  }
}

function checkBin(k, b) {
  const ok = k.sizes ? k.sizes.includes(b.length) : b.length >= 1 && b.length <= k.upTo;
  if (!ok) throw new Malformed('b size');
  if (k.chk === 'point') checkPoint(b);
  if (k.chk === 'ekspki') checkEkSpkiTemplate(b);
}

const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

function writeValue(k, x, name) {
  switch (k.t) {
    case 's': case 's36': case 's36v4': case 'e':
      checkStr(k, x);
      return `"${x}"`;
    case 'u':
      if (!Number.isSafeInteger(x) || x < k.min || x > k.max) throw new Malformed(`${name} u`);
      return String(x);
    case 'b':
      if (x.length === 0) {
        if (!k.empty) throw new Malformed(`${name} empty`);
        return '""';
      }
      checkBin(k, x);
      return `"${b64e(x)}"`;
    default: {
      if (x.length < k.min || x.length > k.max) throw new Malformed(`${name} count`);
      const items = x.map((it) => {
        if (it.length === 0) throw new Malformed(`${name} empty item`);
        if (k.elem.t === 'b') {
          checkBin(k.elem, it);
          return b64e(it);
        }
        checkStr(k.elem, it);
        return it;
      });
      if (k.t === 'a') {
        items.sort(cmp);
        for (let i = 1; i < items.length; i++) if (items[i] === items[i - 1]) throw new Malformed(`${name} duplicate`);
      }
      return `[${items.map((s) => `"${s}"`).join(',')}]`;
    }
  }
}

// Cross-position rules the design of record names malformed and that read
// only the message's own positions (the plan's A1 list).
const CROSS = {
  signed: (v) => {
    const want = utf8(`["${PREFIX}",1,"${v.type}",`);
    if (!equalBytes(v.msg.subarray(0, want.length), want)) throw new Malformed('signed type is not msg[2]');
    const dual = v.type === 'delegation' || v.type === 'succession';
    if (dual && v.sig_b.length === 0) throw new Malformed('missing second signature');
    if (!dual && v.sig_b.length !== 0) throw new Malformed('unexpected second signature');
  },
  'relay-recover': (v) => {
    const has = (n) => v[n].length > 0;
    const rest = ['sig_candidate', 'ek_binding', 'ek_binding_sig', 'recovery_bind_sig', 'device_intent', 'sig_new_device'].some(has);
    const formR = ['intent', 'sig_candidate', 'ek_binding', 'ek_binding_sig', 'recovery_bind', 'recovery_bind_sig',
      'device_intent', 'sig_new_device'].every(has);
    const formC = has('intent') && !has('recovery_bind') && !rest;
    const formB = !has('intent') && has('recovery_bind') && !rest;
    if (!formR && !formC && !formB) throw new Malformed('relay-recover form');
  },
  'device-set': (v) => {
    if (v.pending !== '') {
      const prefix = v.pending.slice(0, v.pending.indexOf('.') + 1);
      if (v.active.some((d) => d.startsWith(prefix))) throw new Malformed('pending is in active');
    }
    // §2.5: "IDs are pairwise distinct". Strict ascending order does not imply it
    // (id.dsk.dkx.5 < id.dsk.dkx.6), so compare the 36-character ids directly.
    const ids = v.active.map((d) => d.slice(0, 36)).sort(cmp);
    for (let i = 1; i < ids.length; i++) if (ids[i] === ids[i - 1]) throw new Malformed('duplicate device id');
    const bound = v.recovery_key.length > 0;
    const slot = v.replacement_bind_digest.length > 0;
    const hasKey = v.replacement_key.length > 0;
    if (bound !== v.recovery_bind_digest.length > 0) throw new Malformed('recovery key and digest');
    if (slot !== (v.replacement_nb_ms !== 0)) throw new Malformed('slot digest and nb');
    if (!slot && hasKey) throw new Malformed('slot key without digest');
    if (slot && !hasKey && !bound) throw new Malformed('pending unbind without a bound key');
    if (hasKey && equalBytes(v.replacement_key, v.recovery_key)) throw new Malformed('refresh to the bound key');
  },
  'recovery-bind': (v) => {
    if (v.escrow_public_key.length > 0 && equalBytes(v.escrow_public_key, v.recovery_key)) {
      throw new Malformed('escrow key equals the recovery key');
    }
  },
};

// encode is EN6: it validates every position (dev shape only) and writes.
export function encode(tag, v) {
  const fields = LAYOUTS[tag];
  if (!fields) throw new Malformed('unknown layout');
  let out = `["${PREFIX}",1,"${tag}"`;
  for (const [name, k] of fields) out += ',' + writeValue(k, v[name], name);
  out += ']';
  CROSS[tag]?.(v);
  if (out.length > capOf(tag)) throw new Malformed('length');
  return utf8(out);
}

async function readValue(k, x) {
  switch (k.t) {
    case 's': case 's36': case 's36v4': case 'e':
      checkStr(k, x);
      if (k.chk === 'dev' && x !== '') await parseDev(x);
      return x;
    case 'u':
      if (!Number.isSafeInteger(x) || x < k.min || x > k.max) throw new Malformed('u');
      return x;
    case 'b': {
      if (typeof x !== 'string') throw new Malformed('not a string');
      if (x === '') {
        if (!k.empty) throw new Malformed('empty');
        return new Uint8Array(0);
      }
      const b = b64d(x);
      checkBin(k, b);
      return b;
    }
    default: {
      if (!Array.isArray(x) || x.length < k.min || x.length > k.max) throw new Malformed('items');
      const out = [];
      for (let i = 0; i < x.length; i++) {
        if (typeof x[i] !== 'string' || x[i] === '') throw new Malformed('array item');
        if (k.t === 'a' && i > 0 && !(x[i - 1] < x[i])) throw new Malformed('not strictly ascending');
        out.push(await readValue(k.elem, x[i]));
      }
      return out;
    }
  }
}

// decode is EN5 (JS form: JSON.parse plus Number.isSafeInteger).
export async function decode(tag, bytes, limit) {
  const fields = LAYOUTS[tag];
  if (!fields) throw new Malformed('unknown layout');
  if (bytes.length > (limit ?? capOf(tag))) throw new Malformed('length');            // 1
  for (const c of bytes) if (!EN2.has(c)) throw new Malformed('EN2');                  // 2
  let arr;
  try {
    arr = JSON.parse(Buffer.from(bytes).toString('latin1'));                            // 3
  } catch {
    throw new Malformed('json');
  }
  if (!Array.isArray(arr) || arr.length !== 3 + fields.length) throw new Malformed('count'); // 4
  if (arr[0] !== PREFIX || arr[1] !== 1 || arr[2] !== tag) throw new Malformed('prefix, version or type');
  const v = {};
  for (let i = 0; i < fields.length; i++) v[fields[i][0]] = await readValue(fields[i][1], arr[3 + i]); // 5
  if (!equalBytes(encode(tag, v), bytes)) throw new Malformed('re-encode');             // 6 (+ CROSS)
  return v;
}

// ── 3. §2.4 primitives and canonical texts ────────────────────────────────

const P384_P = 2n ** 384n - 2n ** 128n - 2n ** 96n + 2n ** 32n - 1n;
const P384_B = 0xb3312fa7e23ee7e4988e056be3f82d19181d9c6efe8141120314088f5013875ac656398d8a2ed19d2a85c8edd3ec2aefn;
const big = (b) => BigInt('0x' + Buffer.from(b).toString('hex'));
const modp = (a) => ((a % P384_P) + P384_P) % P384_P;

// checkPoint is the codec's own curve check (y² = x³ − 3x + b over P-384).
export function checkPoint(b) {
  if (b.length !== 97 || b[0] !== 0x04) throw new Malformed('point form');
  const x = big(b.subarray(1, 49));
  const y = big(b.subarray(49));
  if (x >= P384_P || y >= P384_P) throw new Malformed('point range');
  if (modp(y * y) !== modp(x * x * x - 3n * x + P384_B)) throw new Malformed('point not on curve');
}

// The canonical 550-byte SPKI of an RSA-4096 key with e = 65537 has a fixed
// 33-byte header and 5-byte trailer around a 512-byte modulus with its top bit set.
const EK_HDR = Buffer.from('30820222300d06092a864886f70d01010105000382020f003082020a0282020100', 'hex');
const EK_TRL = Buffer.from('0203010001', 'hex');
export function checkEkSpkiTemplate(b) {
  if (b.length !== 550 || !equalBytes(b.subarray(0, 33), EK_HDR) || !equalBytes(b.subarray(545), EK_TRL) ||
      (b[33] & 0x80) === 0) {
    throw new Malformed('ek spki');
  }
}

// checkEkSpkiWebCrypto is the design of record's Web Crypto procedure (§2.4).
export async function checkEkSpkiWebCrypto(b) {
  if (b.length !== 550) throw new Malformed('ek spki length');
  let key;
  try {
    key = await subtle.importKey('spki', b, { name: 'RSA-OAEP', hash: 'SHA-256' }, true, ['encrypt']);
  } catch {
    throw new Malformed('ek spki parse');
  }
  if (key.algorithm.modulusLength !== 4096) throw new Malformed('ek modulus');
  if (!equalBytes(key.algorithm.publicExponent, new Uint8Array([1, 0, 1]))) throw new Malformed('ek exponent');
  if (!equalBytes(new Uint8Array(await subtle.exportKey('spki', key)), b)) throw new Malformed('ek spki not canonical');
}

export const sha384 = async (b) => new Uint8Array(await subtle.digest('SHA-384', b));

// importP384 checks len == 97 and the 0x04 prefix BEFORE importKey: Node
// accepts a correct-parity 0x06/0x07 hybrid point (measured, v26.7.0).
export async function importP384(b) {
  if (b.length !== 97 || b[0] !== 0x04) throw new Malformed('point_prefix');
  try {
    return await subtle.importKey('raw', b, { name: 'ECDSA', namedCurve: 'P-384' }, true, ['verify']);
  } catch {
    throw new Malformed('point');
  }
}

export async function verifyP384(point, msg, sig) {
  let key;
  try {
    key = await importP384(point);
  } catch {
    return false;
  }
  if (sig.length !== 96) return false;
  return subtle.verify({ name: 'ECDSA', hash: 'SHA-384' }, key, sig, msg);
}

export async function verifyLegacyPSS(spki, msg, sig) {
  try {
    await checkEkSpkiWebCrypto(spki);
  } catch {
    return false;
  }
  if (sig.length !== 512) return false;
  const key = await subtle.importKey('spki', spki, { name: 'RSA-PSS', hash: 'SHA-256' }, false, ['verify']);
  return subtle.verify({ name: 'RSA-PSS', saltLength: 32 }, key, sig, msg);
}

export async function deviceId(dsk) {
  const h = await sha384(concat(utf8('concord-device-id/v1'), [0], dsk));
  const b = h.slice(0, 16);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const x = Buffer.from(b).toString('hex');
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20)}`;
}

export async function parseDev(s) {
  STRCHK.dev(s);
  if (s.length > 319) throw new Malformed('dev length');
  const [id, dskB64, dkxB64, mature] = s.split('.');
  const dsk = b64d(dskB64);
  if ((await deviceId(dsk)) !== id) throw new Malformed('dev id is not DeviceID(dsk)');
  return { id, dsk, dkx: b64d(dkxB64), matureAtMs: parseU(mature) };
}

export async function safetyNumber(accountId, pinDigest) {
  if (!isUUID(accountId) || pinDigest.length !== 48) return '';
  const h = new Uint8Array(await subtle.digest('SHA-512',
    concat(utf8('concord-safety-number/v1'), [0], utf8(accountId), pinDigest)));
  const groups = [];
  for (let i = 0; i < 12; i++) {
    let v = 0;
    for (const c of h.subarray(5 * i, 5 * i + 5)) v = v * 256 + c;
    groups.push(String(v % 100000).padStart(5, '0'));
  }
  return groups.join(' ');
}

const HEX64 = /^[0-9a-f]{64}$/;
const CLIENT_V = /^[0-9A-Za-z.+-]{1,32}$/;

export function ageClaimV2Bytes(c) {
  // valid_age and nsfw_auth are Go's %t: only a real boolean formats as true or false.
  if (typeof c.valid_age !== 'boolean' || typeof c.nsfw_auth !== 'boolean' ||
      !isUUID(c.user_id) || !isUUIDv4(c.device_id) || !HEX64.test(c.nonce) || !CLIENT_V.test(c.client_version) ||
      !Number.isInteger(c.jurisdiction_obligation) || c.jurisdiction_obligation < 0 || c.jurisdiction_obligation > 2 ||
      !Number.isSafeInteger(c.timestamp) || c.timestamp <= 0) {
    return null;
  }
  return utf8(`age-claim/v2\ncanonical_version=2\nuser_id=${c.user_id}\nvalid_age=${c.valid_age}\n` +
    `nsfw_auth=${c.nsfw_auth}\njurisdiction_obligation=${c.jurisdiction_obligation}\nnonce=${c.nonce}\n` +
    `timestamp=${c.timestamp}\ndevice_id=${c.device_id}\nclient_version=${c.client_version}`);
}

export function sessionBindBytes(b) {
  if (![b.realm_id, b.user_id, b.session_id].every(isUUID) || !isUUIDv4(b.device_id) || !HEX64.test(b.challenge) ||
      !Number.isSafeInteger(b.issued_at_ms) || b.issued_at_ms <= 0) {
    return null;
  }
  return utf8(`concord-session-bind/v1\nrealm_id=${b.realm_id}\nuser_id=${b.user_id}\ndevice_id=${b.device_id}\n` +
    `session_id=${b.session_id}\nchallenge=${b.challenge}\nissued_at_ms=${b.issued_at_ms}`);
}

export async function credentialEpochDigest(epoch, valid) {
  return !valid || epoch === '' ? null : sha384(utf8(epoch));
}

export async function populationDigest(records) {
  const sorted = [...records].sort((a, b) => cmp(a.account_id, b.account_id));
  for (let i = 0; i < sorted.length; i++) {
    if (!isUUID(sorted[i].account_id) || sorted[i].legacy_ek_digest.length !== 48) throw new Malformed('population record');
    if (i > 0 && sorted[i - 1].account_id === sorted[i].account_id) throw new Malformed('duplicate account');
  }
  return sha384(concat(...sorted.flatMap((r) => [utf8(r.account_id), r.legacy_ek_digest])));
}

export async function tlsPin(point) {
  const key = await importP384(point);
  return sha384(new Uint8Array(await subtle.exportKey('spki', key)));
}

export async function generatorDigest() {
  return Buffer.from(await sha384(readFileSync(GENERATOR_PATH))).toString('hex');
}

// vectorsDigest is SHA-384 over every byte after the file's header line, which
// is where the vectors are. Keys are drawn fresh each run, so the file cannot be
// regenerated and compared; this records what the generator wrote instead. It
// catches an edit made without running the generator. It is not tamper-proof:
// whoever edits the file can recompute it.
// HEADER_LINE is the exact first line renderVectorFile writes. The digest covers
// only what follows it, so the line must hold nothing else: a vector inserted
// after "vectors":[ on this line would otherwise sit outside the commitment.
export const HEADER_LINE = /^\{"generator_sha384":"[0-9a-f]{96}","vectors_sha384":"[0-9a-f]{96}","vectors":\[$/;

export async function vectorsDigest(body) {
  return Buffer.from(await sha384(Buffer.from(body, 'utf8'))).toString('hex');
}

// checkDrift is the pre-commit hook: generator_sha384 must be SHA-384(gen.mjs),
// and vectors_sha384 must match the vectors the file holds.
export async function checkDrift(path = VECTORS_PATH) {
  const text = readFileSync(path, 'utf8');
  const file = JSON.parse(text);
  const want = await generatorDigest();
  if (file.generator_sha384 !== want) {
    throw new Error(`vectors.json was written by another gen.mjs (file ${file.generator_sha384}, generator ${want}); ` +
      'run: node scripts/authority-vectors/gen.mjs');
  }
  const newline = text.indexOf('\n');
  if (newline < 0 || !HEADER_LINE.test(text.slice(0, newline))) {
    throw new Error('vectors.json header line is not the form gen.mjs writes; run: node scripts/authority-vectors/gen.mjs');
  }
  const content = await vectorsDigest(text.slice(newline + 1));
  if (file.vectors_sha384 !== content) {
    throw new Error(`vectors.json was edited after gen.mjs wrote it (recorded ${file.vectors_sha384}, content ${content}); ` +
      'run: node scripts/authority-vectors/gen.mjs');
  }
}

// ── 4. Vectors (DoR §3.1: the codec and primitive part; PR-A2 appends to BUILDERS) ──

const BUDGET = 128 * 1024;
const CAP_VECTOR_BUDGET = 256 * 1024;
const POOL = 32;
const E0 = new Uint8Array(0);
const TYPICAL = { count: 'typical', item: 'typical' };
const OK = (reenc) => ({ reason: 'ok', verdict: 'ok', ...(reenc ? { reencoded_b64: b64e(reenc) } : {}) });
const BAD = { reason: 'malformed', verdict: 'withhold' };
const BADSIG = { reason: 'bad_signature', verdict: 'withhold' };
const B64CH = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const text = (b) => Buffer.from(b).toString('latin1');
const raw = (s) => new Uint8Array(Buffer.from(s, 'latin1'));
export const RUNTIME_ONLY = [];

function rnd(n) {
  const out = new Uint8Array(n);
  for (let o = 0; o < n; o += 65536) crypto.getRandomValues(out.subarray(o, Math.min(n, o + 65536)));
  return out;
}

const keys = [];
async function ecKey(i) {
  while (keys.length <= i) keys.push(await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-384' }, true, ['sign', 'verify']));
  return keys[i];
}
const pointAt = async (i) => new Uint8Array(await subtle.exportKey('raw', (await ecKey(i % POOL)).publicKey));
let pointSeq = 0;
const nextPoint = () => pointAt(pointSeq++);
async function pointWithParity(par) {
  for (let i = 0; ; i++) {
    const p = await pointAt(i);
    if ((p[96] & 1) === par) return p;
  }
}

let ekCache;
async function ek() {
  if (!ekCache) {
    const kp = await subtle.generateKey({ name: 'RSA-OAEP', modulusLength: 4096, publicExponent: new Uint8Array([1, 0, 1]),
      hash: 'SHA-256' }, true, ['encrypt', 'decrypt']);
    ekCache = { kp, spki: new Uint8Array(await subtle.exportKey('spki', kp.publicKey)) };
  }
  return ekCache;
}

async function devString(mature) {
  const dsk = await nextPoint();
  const dkx = await nextPoint();
  return `${await deviceId(dsk)}.${b64e(dsk)}.${b64e(dkx)}.${mature}`;
}

// Samplers for the string kinds that carry a rule (all produce distinct items by i).
const GEN = {
  dev: (max) => devString(max ? MAX_SAFE : 1),
  version: async (max, i) => String(max ? 1000 + i : 1 + i),
  legacyRow: async (_max, i) => `${String(i).padStart(8, '0')}-0000-4000-8000-${String(i).padStart(12, '0')}.${b64e(rnd(48))}`,
  seqRec: async (max) => `${b64e(rnd(48))}.${max ? b64e(rnd(48)) : ''}`,
  rootRec: async (max) => `${b64e(rnd(48))}.${b64e(await nextPoint())}.${max ? 'continuity' : 'initial'}`,
  recoverRec: async (max, i) => `${max ? MAX_SAFE - i : 1 + i}.${b64e(rnd(48))}`,
  admittedRec: async (max) => `${crypto.randomUUID()}.${max ? crypto.randomUUID() : ''}`,
  secretsRec: async (max) =>
    `${b64e(rnd(48))}.${max ? 'rotate_root' : 'rotate_ek'}.${max ? b64e(await nextPoint()) : ''}.${b64e((await ek()).spki)}`,
};

async function sampleScalar(k, mode, i) {
  const max = mode === 'max';
  if (k.chk && GEN[k.chk]) return GEN[k.chk](max, i);
  switch (k.t) {
    case 's': return max ? String(i).padStart(k.max, 'a') : `layout/${i}`;
    case 's36': case 's36v4': return crypto.randomUUID();
    case 'u': return max ? k.max : Math.max(k.min, 1);
    case 'e': {
      const vs = k.vals.filter((x) => x !== '');
      return max ? vs.reduce((a, b) => (b.length > a.length ? b : a)) : vs[0];
    }
    case 'b': {
      if (k.chk === 'point') return nextPoint();
      if (k.chk === 'ekspki') return (await ek()).spki;
      const cap = k.upTo ?? Math.max(...k.sizes);
      if (max) return rnd(cap);
      if (mode === 'typical' && k.hold) return typicalBytes(k.hold);
      return rnd(k.sizes ? k.sizes[0] : mode === 'typical' ? Math.min(cap, 16) : 1);
    }
    default: throw new Error(`no sampler for ${k.t}`);
  }
}

async function sampleField(k, opts) {
  if (k.t !== 'a' && k.t !== 'l') return sampleScalar(k, opts.item === 'max' && kmax(k) > BUDGET ? 'min' : opts.item, 0);
  let n = opts.count === 'max' ? k.max : opts.count === 'one' ? 1 : Math.max(1, k.min);
  if (n === INF) n = 1;
  n = Math.min(Math.max(n, k.min), k.max);
  if (k.elem.t === 'e') return k.elem.vals.filter((x) => x !== '').slice(0, n);
  const items = [];
  for (let i = 0; i < n; i++) items.push(await sampleScalar(k.elem, opts.item, i));
  return items;
}

// FIXUP makes typical values meaningful and keeps the A1 cross rules true.
const FIXUP = {
  signed: async (v, opts) => {
    if (opts.item === 'max') {
      v.type = 'delegation';
      const head = utf8(`["${PREFIX}",1,"delegation",`);
      v.msg = concat(head, rnd(4096 - head.length));
    } else {
      v.type = 'status';
      v.msg = await typicalBytes('status');
      v.sig_b = E0;
    }
  },
  'relay-recover': async (v, opts) => {
    if (opts.count !== 'typical') return;
    for (const n of ['intent', 'sig_candidate', 'ek_binding', 'ek_binding_sig', 'recovery_bind_sig', 'device_intent', 'sig_new_device']) v[n] = E0;
  },
  'recovery-bind': async (v) => {
    if (v.kind === 'bind') {
      v.target_digest = E0;
      v.expires_at_ms = 0;
    }
  },
  'cp-evidence': async (v, opts) => {
    if (opts.count === 'typical') v.factors = ['password', 'totp'];
  },
  // The public-mirror gate (scripts/public-mirror/gitleaks-gate.toml) decodes the
  // base64 in vectors.json and scans the decoded layout text with gitleaks'
  // `generic-api-key` rule. That rule fires on a high-entropy value right after a
  // tag that contains a credential keyword: "cp-authz" ("auth") followed by its
  // random realm_id UUID, and "device-secrets" ("secret") followed by its random
  // key bytes. Nothing here is a credential, so give just those positions
  // low-entropy values: same kind, same length, only the byte values change.
  'cp-authz': async (v) => {
    v.realm_id = '11111111-1111-4111-8111-111111111111';
  },
  'device-secrets': async (v) => {
    for (const n of ['root_pkcs8', 'ek_pkcs8']) v[n] = new Uint8Array(v[n].length);
  },
};

export async function buildValues(tag, opts) {
  const v = {};
  for (const [name, k] of LAYOUTS[tag]) v[name] = await sampleField(k, opts);
  await FIXUP[tag]?.(v, opts);
  return v;
}

const typicalCache = new Map();
async function typicalBytes(tag) {
  if (!typicalCache.has(tag)) typicalCache.set(tag, encode(tag, await buildValues(tag, TYPICAL)));
  return typicalCache.get(tag);
}

// encodeRaw writes `overrides` (raw JSON tokens) in place of some positions and
// skips the cross rules: it builds negative vectors.
function encodeRaw(tag, v, overrides) {
  let out = `["${PREFIX}",1,"${tag}"`;
  for (const [name, k] of LAYOUTS[tag]) out += ',' + (name in overrides ? overrides[name] : writeValue(k, v[name], name));
  return raw(out + ']');
}

async function derSig() {
  const p1363 = new Uint8Array(await subtle.sign({ name: 'ECDSA', hash: 'SHA-384' }, (await ecKey(0)).privateKey, utf8('der')));
  const int = (x) => {
    let i = 0;
    while (i < x.length - 1 && x[i] === 0) i++;
    let v = x.subarray(i);
    if (v[0] & 0x80) v = concat([0], v);
    return concat([0x02, v.length], v);
  };
  const body = concat(int(p1363.subarray(0, 48)), int(p1363.subarray(48)));
  return concat([0x30, body.length], body);
}

let badSpkiCache;
async function badSpkis() {
  if (badSpkiCache) return badSpkiCache;
  const { spki } = await ek();
  const rsa = async (bits, e) => new Uint8Array(await subtle.exportKey('spki', (await subtle.generateKey(
    { name: 'RSA-OAEP', modulusLength: bits, publicExponent: new Uint8Array(e), hash: 'SHA-256' }, true,
    ['encrypt', 'decrypt'])).publicKey));
  const e65539 = spki.slice();
  e65539[549] = 0x03;
  const pssOid = spki.slice();
  pssOid[16] = 0x0a;
  badSpkiCache = [['2048-bit', await rsa(2048, [1, 0, 1])], ['e-3', await rsa(4096, [3])], ['e-65539', e65539],
    ['pss-oid', pssOid], ['absent-null', concat([0x30, 0x82, 0x02, 0x20, 0x30, 0x0b], spki.subarray(6, 17), spki.subarray(19))],
    ['trailing-bytes', concat(spki, [0, 0])]];
  return badSpkiCache;
}

// A negative vector longer than its layout's cap is refused at EN5 step 1 (length)
// and never reaches the rule its name gives, and neg() cannot tell, because any
// Malformed passes. So neg() fails generation for such a vector unless its name ends
// in `cap-plus-one` (length IS the rule there) or it is listed here with the reason
// the length check is the only one it can reach. An entry that is not over its cap
// also fails: a stale entry would hide the next vector that is.
const LENGTH_BOUND = {
  'neg/legacy-ek-pop/extra-element':
    'fixed-width layout: its typical vector already equals maxLen, so one more element can only fail the length check',
  'neg/legacy-ek-pop/age-claim-text': 'the age-claim/v2 text (295 B) exceeds the 259-byte cap, so EN5 step 1 refuses it before EN2 can see the LF',
  'neg/query/age-claim-text': 'the age-claim/v2 text (295 B) exceeds the 245-byte cap, so EN5 step 1 refuses it before EN2 can see the LF',
  'neg/sig/session-bind-text': 'the session-bind/v1 text (311 B) exceeds the 119-byte cap, so EN5 step 1 refuses it before EN2 can see the LF',
  'neg/sig/a-legacy-ek-pop': 'a whole legacy-ek-pop (259 B) exceeds sig\'s 119-byte cap; neg/sig/wrong-type is the vector that reaches the type check',
};

function makeCtx() {
  const out = [];
  const names = new Set();
  const push = (v) => {
    if (names.has(v.name)) throw new Error(`duplicate vector ${v.name}`);
    names.add(v.name);
    out.push(v);
  };
  const pos = async (name, type, bytes) => {
    await decode(type, bytes); // self-check: this implementation accepts it
    push({ name, type, bytes_b64: b64e(bytes), expect: OK(bytes) });
  };
  const neg = async (name, type, bytes) => {
    const over = bytes.length > capOf(type);
    const declared = name.endsWith('cap-plus-one') || Object.hasOwn(LENGTH_BOUND, name);
    if (over && !declared) throw new Error(`self-check: negative vector ${name} is ${bytes.length} bytes, over the ${capOf(type)}-byte cap of ${type}: it is refused by length, not by its own rule`);
    if (!over && declared) throw new Error(`self-check: negative vector ${name} is declared length-bound but fits the ${capOf(type)}-byte cap of ${type}`);
    try {
      await decode(type, bytes);
    } catch (e) {
      if (!(e instanceof Malformed)) throw e;
      push({ name, type, bytes_b64: b64e(bytes), expect: BAD });
      return;
    }
    throw new Error(`self-check: negative vector ${name} decoded`);
  };
  return { out, push, pos, neg, typical: {} };
}

async function maxVectors(tag) {
  if (maxLen(tag) <= BUDGET) return [['max', encode(tag, await buildValues(tag, { count: 'max', item: 'max' }))]];
  const out = [];
  for (const [variant, opts] of [['max-count', { count: 'max', item: 'min' }], ['max-item', { count: 'one', item: 'max' }]]) {
    const b = encode(tag, await buildValues(tag, opts));
    if (b.length <= BUDGET) {
      out.push([variant, b]);
    } else {
      await decode(tag, b); // checked here, never committed
      RUNTIME_ONLY.push(`${tag}/${variant}`);
    }
  }
  return out;
}

async function emptiesVector(tag) {
  const v = await buildValues(tag, TYPICAL);
  let changed = false;
  for (const [name, k] of LAYOUTS[tag]) {
    if (k.empty) {
      v[name] = k.t === 'b' ? E0 : '';
      changed = true;
    } else if ((k.t === 'a' || k.t === 'l') && k.min === 0 && v[name].length > 0) {
      v[name] = [];
      changed = true;
    }
  }
  if (tag === 'device-set') v.replacement_nb_ms = 0;
  if (!changed) return null;
  try {
    const b = encode(tag, v);
    await decode(tag, b);
    return b;
  } catch (e) {
    if (e instanceof Malformed) return null;
    throw e;
  }
}

async function everyLayout(ctx) {
  for (const tag of Object.keys(LAYOUTS).filter((t) => t !== 'chain-head' && t !== 'device-head')) {
    const t = await typicalBytes(tag);
    ctx.typical[tag] = t;
    await ctx.pos(`pos/${tag}/typical`, tag, t);
    const s = text(t);
    // sig's typical role is its longest, leaving no room for a fourth element under
    // the cap; role operator frees 4 bytes, so the extra element reaches the count check.
    const x = tag === 'sig' ? text(encode('sig', { ...(await buildValues('sig', TYPICAL)), role: 'operator' })) : s;
    await ctx.neg(`neg/${tag}/extra-element`, tag, raw(x.slice(0, -1) + ',""]'));
    await ctx.neg(`neg/${tag}/missing-element`, tag, raw(s.slice(0, s.lastIndexOf(',')) + ']'));
    // sig and legacy-ek-pop are at their cap, so a longer tag would only fail the length
    // check; a same-length tag reaches the prefix, version and type check.
    const wrong = { sig: 'siq', 'legacy-ek-pop': 'legacy-ek-poq' }[tag] ?? `${tag}x`;
    await ctx.neg(`neg/${tag}/wrong-type`, tag, raw(s.replace(`,"${tag}",`, `,"${wrong}",`)));
    for (const [variant, b] of await maxVectors(tag)) await ctx.pos(`pos/${tag}/${variant}`, tag, b);
    const e = await emptiesVector(tag);
    if (e) await ctx.pos(`pos/${tag}/empties`, tag, e);
  }
  for (const [tag, cap] of Object.entries(CAPS)) {
    if (cap + 1 <= CAP_VECTOR_BUDGET) await ctx.neg(`neg/${tag}/cap-plus-one`, tag, new Uint8Array(cap + 1).fill(65));
  }
}

async function parsing(ctx) {
  const sv = await buildValues('status', TYPICAL);
  for (const [n, tok] of [['u-2^53', '9007199254740992'], ['u-1.0', '1.0'], ['u-1e0', '1e0'], ['u-negative', '-1'],
    ['u-string', '"1"'], ['u-bool', 'true'], ['u-null', 'null'], ['u-nested-array', '[1]']]) {
    await ctx.neg(`neg/status/${n}`, 'status', encodeRaw('status', sv, { as_of_ms: tok }));
  }
  const st = text(encode('status', sv));
  await ctx.neg('neg/status/bom', 'status', concat([0xef, 0xbb, 0xbf], raw(st)));
  await ctx.neg('neg/status/space', 'status', raw(st.replace('",1,"status"', '", 1,"status"')));
  await ctx.neg('neg/status/escape', 'status', encodeRaw('status', sv, { realm_id: '"\\u0061"' }));
  await ctx.neg('neg/status/non-ascii', 'status', encodeRaw('status', sv, { realm_id: '"\xc3\xa9"' }));
  await ctx.neg('neg/status/invalid-utf8', 'status', encodeRaw('status', sv, { realm_id: '"\xff"' }));
  await ctx.neg('neg/status/version-2', 'status', raw(st.replace('",1,"status"', '",2,"status"')));
  await ctx.neg('neg/status/version-1.0', 'status', raw(st.replace('",1,"status"', '",1.0,"status"')));
  await ctx.neg('neg/status/prefix', 'status', raw(st.replace(`"${PREFIX}"`, '"concord-account-authorit"')));
  await ctx.neg('neg/status/nested-array-at-s', 'status', encodeRaw('status', sv, { realm_id: `["${sv.realm_id}"]` }));
  await ctx.neg('neg/status/object', 'status', raw('{"a":1}'));
  await ctx.neg('neg/status/trailing-bytes', 'status', raw(st + ']'));
}

async function arraysAndEvidence(ctx) {
  const dv = await buildValues('delegation', { count: 'max', item: 'typical' });
  const dt = text(encode('delegation', dv));
  const cak = JSON.parse(dt)[8];
  await ctx.neg('neg/delegation/a-unsorted', 'delegation', raw(dt.replace(`"${cak[0]}","${cak[1]}"`, `"${cak[1]}","${cak[0]}"`)));
  await ctx.neg('neg/delegation/a-duplicate', 'delegation', raw(dt.replace(`"${cak[0]}","${cak[1]}"`, `"${cak[0]}","${cak[0]}"`)));
  await ctx.neg('neg/delegation/a-nested-array', 'delegation', raw(dt.replace(`"${cak[0]}"`, `["${cak[0]}"]`)));
  await ctx.neg('neg/delegation/a-over-count', 'delegation', encodeRaw('delegation', dv,
    { cak_public_keys: `[${[...cak, b64e(await nextPoint())].sort(cmp).map((s) => `"${s}"`).join(',')}]` }));
  await ctx.neg('neg/delegation/version-0', 'delegation', encodeRaw('delegation', dv, { accepted_versions: '["0"]' }));
  await ctx.neg('neg/delegation/version-10000', 'delegation', encodeRaw('delegation', dv, { accepted_versions: '["10000"]' }));
  await ctx.neg('neg/delegation/scope-device-status', 'delegation', encodeRaw('delegation', dv, { scope: '["device-status"]' }));
  const ev = await buildValues('cp-evidence', TYPICAL);
  await ctx.pos('pos/cp-evidence/all-14-factors', 'cp-evidence', encode('cp-evidence', { ...ev, factors: [...FACTORS] }));
  await ctx.neg('neg/cp-evidence/backup-email-recovery', 'cp-evidence', encodeRaw('cp-evidence', ev, { factors: '["backup-email-recovery"]' }));
  await ctx.neg('neg/cp-evidence/duplicate-factor', 'cp-evidence', encodeRaw('cp-evidence', ev, { factors: '["password","password"]' }));
  await ctx.neg('neg/cp-evidence/unsorted-factors', 'cp-evidence', encodeRaw('cp-evidence', ev, { factors: '["totp","password"]' }));
}

async function base64AndKeys(ctx) {
  const qv = await buildValues('query', TYPICAL);
  const nonce = b64e(qv.nonce);
  const strictBad = nonce.slice(0, 21) + B64CH[B64CH.indexOf(nonce[21]) | 1] + '==';
  await ctx.neg('neg/query/base64-QR', 'query', encodeRaw('query', qv, { nonce: '"QR=="' }));
  await ctx.neg('neg/query/base64-trailing-bits', 'query', encodeRaw('query', qv, { nonce: `"${strictBad}"` }));
  await ctx.neg('neg/query/base64-missing-padding', 'query', encodeRaw('query', qv, { nonce: `"${nonce.slice(0, 22)}"` }));
  await ctx.neg('neg/query/base64-newline', 'query', encodeRaw('query', qv, { nonce: `"${nonce.slice(0, 12)}\n${nonce.slice(12)}"` }));

  const av = await buildValues('anchors', TYPICAL);
  const p = av.primary_a;
  const hybrid = p.slice();
  hybrid[0] = 0x06 | (p[96] & 1);
  const off = p.slice();
  off[96] ^= 1;
  for (const [n, b] of [['compressed', concat([0x02 | (p[96] & 1)], p.subarray(1, 49))], ['off-curve', off],
    ['hybrid', hybrid], ['infinity', new Uint8Array([0])], ['short', p.subarray(0, 96)]]) {
    await ctx.neg(`neg/anchors/point-${n}`, 'anchors', encodeRaw('anchors', av, { primary_a: `"${b64e(b)}"` }));
  }

  const bv = await buildValues('bundle', TYPICAL);
  await ctx.neg('neg/bundle/sig-95', 'bundle', encodeRaw('bundle', bv, { result_sig: `"${b64e(rnd(95))}"` }));
  await ctx.neg('neg/bundle/sig-97', 'bundle', encodeRaw('bundle', bv, { result_sig: `"${b64e(rnd(97))}"` }));
  await ctx.neg('neg/bundle/sig-der', 'bundle', encodeRaw('bundle', bv, { result_sig: `"${b64e(await derSig())}"` }));
  await ctx.neg('neg/bundle/pss-511', 'bundle', encodeRaw('bundle', bv, { legacy_pop_sig: `"${b64e(rnd(511))}"` }));

  const ekv = await buildValues('ek-binding', TYPICAL);
  for (const [n, b] of await badSpkis()) {
    await ctx.neg(`neg/ek-binding/spki-${n}`, 'ek-binding', encodeRaw('ek-binding', ekv, { ek_public_key: `"${b64e(b)}"` }));
  }

  const iv = await buildValues('intent', TYPICAL);
  await ctx.neg('neg/intent/request-id-v1', 'intent', encodeRaw('intent', iv, { request_id: '"6ba7b810-9dad-11d1-80b4-00c04fd430c8"' }));
  await ctx.neg('neg/intent/realm-uppercase', 'intent', encodeRaw('intent', iv, { realm_id: `"${iv.realm_id.toUpperCase()}"` }));
  await ctx.neg('neg/intent/private-op', 'intent', encodeRaw('intent', iv, { op: '"add_device"' }));
  await ctx.neg('neg/intent/expires-zero', 'intent', encodeRaw('intent', iv, { expires_at_ms: '0' }));
  await ctx.neg('neg/sig/role-admin', 'sig', encodeRaw('sig', await buildValues('sig', TYPICAL), { role: '"admin"' }));
  await ctx.neg('neg/sig/a-legacy-ek-pop', 'sig', ctx.typical['legacy-ek-pop']);
}

const AGE = () => ({ user_id: crypto.randomUUID(), valid_age: true, nsfw_auth: false, jurisdiction_obligation: 2,
  nonce: Buffer.from(rnd(32)).toString('hex'), timestamp: 1700000000, device_id: crypto.randomUUID(), client_version: '0.2.48' });
const BIND = () => ({ realm_id: crypto.randomUUID(), user_id: crypto.randomUUID(), device_id: crypto.randomUUID(),
  session_id: crypto.randomUUID(), challenge: Buffer.from(rnd(32)).toString('hex'), issued_at_ms: 1700000000123 });

async function signedAndTexts(ctx) {
  const sgn = (type, msg, dual) => ({ type, msg, sig_a: rnd(96), sig_b: dual ? rnd(96) : E0 });
  const t = ctx.typical;
  const sigB = `"${b64e(rnd(96))}"`;
  await ctx.pos('pos/signed/delegation', 'signed', encode('signed', sgn('delegation', t.delegation, true)));
  await ctx.pos('pos/signed/succession', 'signed', encode('signed', sgn('succession', t.succession, true)));
  await ctx.pos('pos/signed/device-status', 'signed', encode('signed', sgn('device-status', t['device-status'], false)));
  await ctx.neg('neg/signed/delegation-sig-b-empty', 'signed', encodeRaw('signed', sgn('delegation', t.delegation, true), { sig_b: '""' }));
  await ctx.neg('neg/signed/succession-sig-b-empty', 'signed', encodeRaw('signed', sgn('succession', t.succession, true), { sig_b: '""' }));
  await ctx.neg('neg/signed/status-sig-b-present', 'signed', encodeRaw('signed', sgn('status', t.status, false), { sig_b: sigB }));
  await ctx.neg('neg/signed/device-status-sig-b-present', 'signed',
    encodeRaw('signed', sgn('device-status', t['device-status'], false), { sig_b: sigB }));
  await ctx.neg('neg/signed/type-not-msg2', 'signed', encodeRaw('signed', sgn('status', t.status, false), { type: '"delegation"', sig_b: sigB }));
  await ctx.neg('neg/signed/type-result', 'signed', encodeRaw('signed', sgn('status', t.status, false),
    { type: '"result"', msg: `"${b64e(t.result)}"` }));
  const age = ageClaimV2Bytes(AGE());
  const bind = sessionBindBytes(BIND());
  await ctx.neg('neg/legacy-ek-pop/age-claim-text', 'legacy-ek-pop', age);
  await ctx.neg('neg/query/age-claim-text', 'query', age);
  await ctx.neg('neg/sig/session-bind-text', 'sig', bind);
  await ctx.pos('pos/query-resp/failure-form', 'query-resp', utf8(`["${PREFIX}",1,"query-resp","",[],"",[]]`));
}

async function ledger(ctx) {
  const dsv = await buildValues('device-set', TYPICAL);
  dsv.active = [await devString(MAX_SAFE)];
  await ctx.pos('pos/device-set/dev-319', 'device-set', encode('device-set', dsv));
  const [id, dk, dx] = dsv.active[0].split('.');
  await ctx.neg('neg/device-set/dev-320', 'device-set', encodeRaw('device-set', dsv, { active: `["${id}.${dk}.${dx}.10000000000000000"]` }));
  await ctx.neg('neg/device-set/dev-id-mismatch', 'device-set', encodeRaw('device-set', dsv, { active: `["${crypto.randomUUID()}.${dk}.${dx}.1"]` }));
  // §2.5 "IDs are pairwise distinct": two entries with one device id and one dsk,
  // differing only in mature_at_ms, are strictly ascending as strings (…5 < …6)
  // and each parses, so only the pairwise-distinct rule can refuse them.
  await ctx.neg('neg/device-set/duplicate-id', 'device-set',
    encodeRaw('device-set', { ...dsv, active: [`${id}.${dk}.${dx}.5`, `${id}.${dk}.${dx}.6`] }, {}));
  const rk = await nextPoint();
  const rk2 = await nextPoint();
  const d1 = rnd(48);
  const d2 = rnd(48);
  const base = { recovery_key: E0, recovery_bind_digest: E0, replacement_key: E0, replacement_bind_digest: E0, replacement_nb_ms: 0 };
  for (const [n, o] of [['pending-refresh', { recovery_key: rk, recovery_bind_digest: d1, replacement_key: rk2, replacement_bind_digest: d2, replacement_nb_ms: 5 }],
    ['pending-unbind', { recovery_key: rk, recovery_bind_digest: d1, replacement_bind_digest: d2, replacement_nb_ms: 5 }],
    ['pending-first-bind', { replacement_key: rk2, replacement_bind_digest: d2, replacement_nb_ms: 5 }],
    // The commonest state after a bind: a bound key, an empty replacement slot.
    ['bound-no-slot', { recovery_key: rk, recovery_bind_digest: d1 }]]) {
    await ctx.pos(`pos/device-set/${n}`, 'device-set', encode('device-set', { ...dsv, ...base, ...o }));
  }
  for (const [n, o] of [['key-without-digest', { recovery_key: rk }], ['digest-without-key', { recovery_bind_digest: d1 }],
    ['slot-digest-without-nb', { replacement_key: rk2, replacement_bind_digest: d2 }], ['nb-without-slot-digest', { replacement_nb_ms: 5 }],
    ['slot-key-without-digest', { replacement_key: rk2 }], ['unbind-without-bound-key', { replacement_bind_digest: d2, replacement_nb_ms: 5 }],
    ['refresh-to-bound-key', { recovery_key: rk, recovery_bind_digest: d1, replacement_key: rk, replacement_bind_digest: d2, replacement_nb_ms: 5 }],
    ['pending-in-active', { pending: dsv.active[0] }]]) {
    await ctx.neg(`neg/device-set/${n}`, 'device-set', encodeRaw('device-set', { ...dsv, ...base, ...o }, {}));
  }
  // §2.5 "pending is not in active" is by device id, not by the whole `dev` string.
  // The same device at another maturity, or with another (valid) dkx, is a different
  // string but the same device. Each pending below differs from active[0] in exactly
  // one position, so it is not a duplicate in active, it parses, and only the by-id
  // rule can refuse it.
  const activeMature = dsv.active[0].split('.')[3];
  let otherDkx = b64e(await nextPoint());
  while (otherDkx === dx) otherDkx = b64e(await nextPoint());
  await ctx.neg('neg/device-set/pending-same-id-other-maturity', 'device-set',
    encodeRaw('device-set', { ...dsv, ...base, pending: `${id}.${dk}.${dx}.1` }, {}));
  await ctx.neg('neg/device-set/pending-same-id-other-dkx', 'device-set',
    encodeRaw('device-set', { ...dsv, ...base, pending: `${id}.${dk}.${otherDkx}.${activeMature}` }, {}));

  const rb = await buildValues('recovery-bind', TYPICAL);
  const bindB = { ...rb, kind: 'bind', expires_at_ms: 0, target_digest: E0 };
  const cancelB = { ...rb, kind: 'cancel', expires_at_ms: 1700000660000, chain_seq: 0, chain_entry_digest: E0,
    binding_root_digest: E0, escrow_public_key: E0 };
  const unbindB = { ...rb, kind: 'unbind', recovery_key: E0, target_digest: E0, escrow_public_key: E0, expires_at_ms: 0 };
  await ctx.pos('pos/recovery-bind/bind', 'recovery-bind', encode('recovery-bind', bindB));
  await ctx.pos('pos/recovery-bind/cancel', 'recovery-bind', encode('recovery-bind', cancelB));
  await ctx.pos('pos/recovery-bind/unbind', 'recovery-bind', encode('recovery-bind', unbindB));
  const off = bindB.escrow_public_key.slice();
  off[96] ^= 1;
  await ctx.neg('neg/recovery-bind/escrow-equals-recovery-key', 'recovery-bind',
    encodeRaw('recovery-bind', { ...bindB, escrow_public_key: bindB.recovery_key }, {}));
  await ctx.neg('neg/recovery-bind/escrow-off-curve', 'recovery-bind', encodeRaw('recovery-bind', bindB, { escrow_public_key: `"${b64e(off)}"` }));
  await ctx.neg('neg/recovery-bind/kind-rebind', 'recovery-bind', encodeRaw('recovery-bind', bindB, { kind: '"rebind"' }));

  const full = { intent: encode('intent', { ...(await buildValues('intent', TYPICAL)), op: 'recover' }), sig_candidate: rnd(96),
    sig_recovery: rnd(96), ek_binding: ctx.typical['ek-binding'], ek_binding_sig: rnd(96),
    recovery_bind: encode('recovery-bind', bindB), recovery_bind_sig: rnd(96), device_intent: ctx.typical['device-intent'],
    sig_new_device: rnd(96) };
  const only = (keep, o = {}) => ({ ...Object.fromEntries(Object.entries(full).map(([k, x]) => [k, keep.includes(k) ? x : E0])), ...o });
  const cancelIntent = encode('intent', { ...(await buildValues('intent', TYPICAL)), op: 'cancel_pending' });
  await ctx.pos('pos/relay-recover/form-r', 'relay-recover', encode('relay-recover', full));
  await ctx.pos('pos/relay-recover/form-c', 'relay-recover', encode('relay-recover', only(['sig_recovery'], { intent: cancelIntent })));
  await ctx.pos('pos/relay-recover/form-b', 'relay-recover',
    encode('relay-recover', only(['sig_recovery'], { recovery_bind: encode('recovery-bind', cancelB) })));
  await ctx.neg('neg/relay-recover/c-plus-ek-binding', 'relay-recover',
    encodeRaw('relay-recover', only(['sig_recovery', 'ek_binding'], { intent: cancelIntent }), {}));
  await ctx.neg('neg/relay-recover/sig-recovery-only', 'relay-recover', encodeRaw('relay-recover', only(['sig_recovery']), {}));
  await ctx.neg('neg/relay-recover/r-without-device-intent', 'relay-recover', encodeRaw('relay-recover', { ...full, device_intent: E0 }, {}));
}

async function heads(ctx) {
  const realm = crypto.randomUUID();
  const account = crypto.randomUUID();
  const chain = (o) => encode('chain-head', { realm_id: realm, account_id: account, head_result: E0, fix: E0,
    active_ek_spki: E0, active_ek_generation: 0, pending_ek_spki: E0, pending_ek_generation: 0, pending_recovery_key: E0,
    seq_index: [], root_index: [], ek_index: [], recover_index: [], ...o });
  const devHead = async (ch, o) => encodeRaw('device-head', { realm_id: realm, account_id: account, self_device_id: '',
    last_device_result: E0, device_set: E0, chain_head_digest: await sha384(ch), admitted_by: [], secrets: [], ...o }, {});
  const pushHead = async (name, dh, ch, ok) => {
    let accepted = true;
    try {
      const v = await decode('device-head', dh);
      await decode('chain-head', ch);
      accepted = equalBytes(v.chain_head_digest, await sha384(ch));
    } catch (e) {
      if (!(e instanceof Malformed)) throw e;
      accepted = false;
    }
    if (accepted !== ok) throw new Error(`self-check: ${name}`);
    ctx.push({ name, type: 'device-head', bytes_b64: b64e(dh), input: { chain_head_b64: b64e(ch) }, expect: ok ? OK(dh) : BAD });
  };

  const genesis = chain({});
  await ctx.pos('pos/chain-head/genesis', 'chain-head', genesis);
  await pushHead('pos/device-head/genesis-monitor', await devHead(genesis, {}), genesis, true);
  await pushHead('pos/device-head/genesis-own', await devHead(genesis, { self_device_id: crypto.randomUUID() }), genesis, true);

  const root = await nextPoint();
  const { spki } = await ek();
  const seqRec = `${b64e(rnd(48))}.${b64e(rnd(48))}`;
  const rootRec = `${b64e(rnd(48))}.${b64e(root)}.continuity`;
  const recRec = `${MAX_SAFE}.${b64e(rnd(48))}`;
  const caps = chain({ head_result: ctx.typical.result, fix: ctx.typical.signed, active_ek_spki: spki, active_ek_generation: 3,
    pending_ek_spki: spki, pending_ek_generation: 4, pending_recovery_key: await nextPoint(),
    seq_index: [`${b64e(rnd(48))}.`, seqRec], root_index: [rootRec], ek_index: [rnd(48)],
    recover_index: [recRec, `10.${b64e(rnd(48))}`] });
  await ctx.pos('pos/chain-head/records-at-caps', 'chain-head', caps);
  const ct = text(caps);
  await ctx.neg('neg/chain-head/seq-record-over', 'chain-head', raw(ct.replace(`"${seqRec}"`, `"${seqRec}A"`)));
  await ctx.neg('neg/chain-head/root-record-over', 'chain-head', raw(ct.replace(`"${rootRec}"`, `"${rootRec}A"`)));
  await ctx.neg('neg/chain-head/recover-record-over', 'chain-head', raw(ct.replace(`"${recRec}"`, `"${recRec}A"`)));

  const adm = `${crypto.randomUUID()}.${crypto.randomUUID()}`;
  const sec = `${b64e(rnd(48))}.rotate_root.${b64e(root)}.${b64e(spki)}`;
  const capsDh = await devHead(caps, { self_device_id: crypto.randomUUID(), last_device_result: ctx.typical['device-result'],
    device_set: ctx.typical['device-set'], admitted_by: [adm, `${crypto.randomUUID()}.`],
    secrets: [sec, `${b64e(rnd(48))}.rotate_ek..${b64e(spki)}`] });
  await pushHead('pos/device-head/records-at-caps', capsDh, caps, true);
  const dt = text(capsDh);
  await pushHead('neg/device-head/admitted-record-over', raw(dt.replace(`"${adm}"`, `"${adm}A"`)), caps, false);
  await pushHead('neg/device-head/secrets-record-over', raw(dt.replace(`"${sec}"`, `"${sec}A"`)), caps, false);
  await pushHead('neg/device-head/chain-head-digest-mismatch', await devHead(genesis, {}), caps, false);
  await pushHead('neg/device-head/extra-element', raw(dt.slice(0, -1) + ',""]'), caps, false);
}

async function primitives(ctx) {
  const prim = (name, type, input, expect) => ctx.push({ name, type, input, expect });
  const even = await pointWithParity(0);
  const odd = await pointWithParity(1);
  const h06 = even.slice();
  h06[0] = 0x06;
  const h07 = odd.slice();
  h07[0] = 0x07;
  const off = even.slice();
  off[96] ^= 1;
  for (const [n, b, ok] of [['valid', even, true], ['hybrid-06', h06, false], ['hybrid-07', h07, false], ['off-curve', off, false],
    ['compressed', concat([0x02], even.subarray(1, 49)), false], ['short', even.subarray(0, 96), false]]) {
    let accepted = true;
    try {
      await importP384(b);
    } catch {
      accepted = false;
    }
    if (accepted !== ok) throw new Error(`self-check: p384-point/${n}`);
    prim(`prim/p384-point/${n}`, 'p384-point', { point_b64: b64e(b) }, ok ? OK() : BAD);
  }

  const k = await ecKey(1);
  const pt = await pointAt(1);
  const msg = rnd(64);
  const sig = new Uint8Array(await subtle.sign({ name: 'ECDSA', hash: 'SHA-384' }, k.privateKey, msg));
  const hybridPt = pt.slice();
  hybridPt[0] = 0x06 | (pt[96] & 1);
  for (const [n, p, m, s, ok] of [['valid', pt, msg, sig, true], ['wrong-msg', pt, rnd(64), sig, false],
    ['95', pt, msg, sig.subarray(0, 95), false], ['97', pt, msg, concat(sig, [0]), false],
    ['der', await pointAt(0), utf8('der'), await derSig(), false], ['hybrid-point', hybridPt, msg, sig, false]]) {
    if ((await verifyP384(p, m, s)) !== ok) throw new Error(`self-check: p384-sig/${n}`);
    prim(`prim/p384-sig/${n}`, 'p384-sig', { point_b64: b64e(p), msg_b64: b64e(m), sig_b64: b64e(s) }, ok ? OK() : BADSIG);
  }
  // P1363 scalar range: r and s must lie in [1, n-1], and a high s is accepted (no low-s rule).
  const n384 = big(Buffer.from('ffffffffffffffffffffffffffffffffffffffffffffffffc7634d81f4372ddf581a0db248b0a77aecec196accc52973', 'hex'));
  const be48 = (x) => new Uint8Array(Buffer.from(x.toString(16).padStart(96, '0'), 'hex'));
  const [sr, ss] = [sig.subarray(0, 48), big(sig.subarray(48))];
  const highS = be48(ss > n384 / 2n ? ss : n384 - ss);
  for (const [n, s, ok] of [['high-s', concat(sr, highS), true], ['r-zero', concat(be48(0n), be48(ss)), false],
    ['s-zero', concat(sr, be48(0n)), false], ['r-eq-n', concat(be48(n384), be48(ss)), false], ['s-eq-n', concat(sr, be48(n384)), false]]) {
    if ((await verifyP384(pt, msg, s)) !== ok) throw new Error(`self-check: p384-sig/${n}`);
    prim(`prim/p384-sig/${n}`, 'p384-sig', { point_b64: b64e(pt), msg_b64: b64e(msg), sig_b64: b64e(s) }, ok ? OK() : BADSIG);
  }

  const { kp, spki } = await ek();
  const pss = await subtle.importKey('pkcs8', new Uint8Array(await subtle.exportKey('pkcs8', kp.privateKey)),
    { name: 'RSA-PSS', hash: 'SHA-256' }, false, ['sign']); // in memory only, never written
  const pm = rnd(64);
  const ps = new Uint8Array(await subtle.sign({ name: 'RSA-PSS', saltLength: 32 }, pss, pm));
  const ps20 = new Uint8Array(await subtle.sign({ name: 'RSA-PSS', saltLength: 20 }, pss, pm));
  for (const [n, m, s, ok] of [['valid', pm, ps, true], ['wrong-msg', rnd(64), ps, false], ['511', pm, ps.subarray(0, 511), false],
    ['salt-20', pm, ps20, false]]) {
    if ((await verifyLegacyPSS(spki, m, s)) !== ok) throw new Error(`self-check: pss-sig/${n}`);
    prim(`prim/pss-sig/${n}`, 'pss-sig', { spki_b64: b64e(spki), msg_b64: b64e(m), sig_b64: b64e(s) }, ok ? OK() : BADSIG);
  }

  // Two shapes that keep the 550-byte length. Go parses the first as the same key (x509
  // right-aligns the bit string), so only the re-marshal comparison refuses it; only a
  // modulus-size check refuses the second.
  const spkiAt = (at, want) => {
    if (!equalBytes(spki.subarray(at, at + want.length), new Uint8Array(want))) throw new Error(`self-check: spki[${at}..] is not ${want}`);
  };
  spkiAt(19, [0x03, 0x82, 0x02, 0x0f, 0x00, 0x30]);
  const unusedBits = spki.slice();
  unusedBits[23] = 0x01;
  for (let i = 24; i < 550; i++) unusedBits[i] = ((spki[i] << 1) | (i < 549 ? spki[i + 1] >> 7 : 0)) & 0xff;
  spkiAt(28, [0x02, 0x82, 0x02, 0x01, 0x00]);
  const modulus4097 = spki.slice();
  modulus4097[32] = 0x01;
  for (const [n, b] of [['valid', spki], ...(await badSpkis()), ['bitstring-unused-bits', unusedBits], ['modulus-4097', modulus4097]]) {
    const ok = n === 'valid';
    let web = true;
    try {
      await checkEkSpkiWebCrypto(b);
    } catch {
      web = false;
    }
    let tpl = true;
    try {
      checkEkSpkiTemplate(b);
    } catch {
      tpl = false;
    }
    if (web !== ok || tpl !== ok) throw new Error(`self-check: ek-spki/${n}`);
    prim(`prim/ek-spki/${n}`, 'ek-spki', { spki_b64: b64e(b) }, ok ? OK() : BAD);
  }

  for (const n of ['a', 'b']) {
    const dsk = await nextPoint();
    prim(`prim/device-id/${n}`, 'device-id', { dsk_b64: b64e(dsk) }, { ...OK(), device_id: await deviceId(dsk) });
    const acct = crypto.randomUUID();
    const pin = rnd(48);
    prim(`prim/safety-number/${n}`, 'safety-number', { account_id: acct, pin_digest_b64: b64e(pin) },
      { ...OK(), safety_number: await safetyNumber(acct, pin) });
  }
  prim('prim/safety-number/uppercase-account', 'safety-number', { account_id: crypto.randomUUID().toUpperCase(),
    pin_digest_b64: b64e(rnd(48)) }, { ...BAD, safety_number: '' });

  const age = AGE();
  prim('prim/age-claim-v2/valid', 'age-claim-v2', age, { ...OK(), text_b64: b64e(ageClaimV2Bytes(age)) });
  prim('prim/age-claim-v2/nonce-uppercase', 'age-claim-v2', { ...age, nonce: age.nonce.toUpperCase() }, BAD);
  // The timestamp bound is MaxSafeInt in both implementations, as for session-bind's issued_at_ms.
  const ageMax = { ...age, timestamp: Number.MAX_SAFE_INTEGER };
  prim('prim/age-claim-v2/timestamp-max-safe', 'age-claim-v2', ageMax, { ...OK(), text_b64: b64e(ageClaimV2Bytes(ageMax)) });
  prim('prim/age-claim-v2/timestamp-2pow53', 'age-claim-v2', { ...age, timestamp: 2 ** 53 }, BAD);
  const bind = BIND();
  prim('prim/session-bind-v1/valid', 'session-bind-v1', bind, { ...OK(), text_b64: b64e(sessionBindBytes(bind)) });
  prim('prim/session-bind-v1/short-challenge', 'session-bind-v1', { ...bind, challenge: bind.challenge.slice(1) }, BAD);
  const bindMax = { ...bind, issued_at_ms: Number.MAX_SAFE_INTEGER };
  prim('prim/session-bind-v1/issued-at-max-safe', 'session-bind-v1', bindMax, { ...OK(), text_b64: b64e(sessionBindBytes(bindMax)) });
  prim('prim/session-bind-v1/issued-at-2pow53', 'session-bind-v1', { ...bind, issued_at_ms: 2 ** 53 }, BAD);

  prim('prim/credential-epoch-digest/null', 'credential-epoch-digest', { epoch: 'e1', epoch_valid: false }, { ...OK(), digest_b64: '' });
  prim('prim/credential-epoch-digest/empty', 'credential-epoch-digest', { epoch: '', epoch_valid: true }, { ...OK(), digest_b64: '' });
  prim('prim/credential-epoch-digest/value', 'credential-epoch-digest', { epoch: 'e1', epoch_valid: true },
    { ...OK(), digest_b64: b64e(await credentialEpochDigest('e1', true)) });

  const recs = [0, 1, 2].map(() => ({ account_id: crypto.randomUUID(), legacy_ek_digest: rnd(48) }));
  const toInput = (rs) => ({ records: rs.map((r) => ({ account_id: r.account_id, legacy_ek_digest_b64: b64e(r.legacy_ek_digest) })) });
  prim('prim/population-digest/three', 'population-digest', toInput(recs), { ...OK(), digest_b64: b64e(await populationDigest(recs)) });
  prim('prim/population-digest/duplicate', 'population-digest', toInput([recs[0], recs[0]]), BAD);

  const tp = await nextPoint();
  prim('prim/tls-pin/a', 'tls-pin', { point_b64: b64e(tp) }, { ...OK(), digest_b64: b64e(await tlsPin(tp)) });

  // The ordered position names of every layout. A second implementation that decodes
  // and re-encodes through its own table round-trips itself whatever the order of two
  // same-kind rows (primary_a/primary_b, realm_id/account_id), so no byte-level vector
  // can see that swap; the names, in position order, can.
  for (const tag of Object.keys(LAYOUTS)) {
    prim(`prim/layout-fields/${tag}`, 'layout-fields', { layout: tag, fields: LAYOUTS[tag].map(([name]) => name) }, OK());
  }
}

// BUILDERS is ordered; PR-A2 appends its verification builders.
export const BUILDERS = [everyLayout, parsing, arraysAndEvidence, base64AndKeys, signedAndTexts, ledger, heads, primitives];

// resetDraw forgets everything a draw randomised, so the next buildVectors() is a new
// draw rather than the same key pool, EK and typical values placed differently.
function resetDraw() {
  keys.length = 0;
  pointSeq = 0;
  ekCache = undefined;
  badSpkiCache = undefined;
  typicalCache.clear();
  RUNTIME_ONLY.length = 0;
  realmSeq = 0;
  legacyCache = undefined;
  APPLY_LOG = null;
  NORM_LOG = null;
}

export async function buildVectors() {
  resetDraw();
  const ctx = makeCtx();
  for (const step of BUILDERS) await step(ctx);
  return ctx.out;
}

// ── 5. Public-mirror safety ──────────────────────────────────────────────────
//
// vectors.json lives under services/**, which the public mirror publishes, and the
// post-merge gate (scripts/public-mirror/publish-gates.sh) scans the tree with gitleaks
// 8.30.1 (GITLEAKS_VERSION, .github/workflows/publish-public-mirror.yml) and
// scripts/public-mirror/gitleaks-gate.toml (useDefault = true). The vectors are random
// public key material, so a draw can hold a base64 value that ends in a credential
// keyword ("Api", "key", ...) directly before the next quoted base64 value, and gitleaks'
// generic-api-key rule reads that pair as a credential. A tripped gate stops the mirror
// from publishing, and nothing before the merge runs it. mirrorUnsafe predicts the trip
// and main() refuses to write a draw it flags.
//
// A port of what gitleaks 8.30.1 does, read from the binary and measured against it:
//  - The rule. [[rules]] id = "generic-api-key" in the default config embedded in the
//    binary (`strings -n 1 "$(command -v gitleaks)"`; the same regex and entropy are in
//    config/gitleaks.toml of the gitleaks checkout pre-commit caches under
//    ~/.cache/pre-commit). The secret is the first non-empty capture group, and a match is
//    a finding when the Shannon entropy of that secret is above 3.5 (gitleaks skips
//    entropy <= 3.5) and the secret is not made only of letters, '_', '.' and '-' (the
//    rule's first allowlist, `^[a-zA-Z_.-]+$`). The allowlist is ported because it is not
//    optional: the file holds identifier-shaped pairs on purpose (the layout-fields
//    vectors list field names such as "rsk_public_keys","cak_public_keys"), and without it
//    every draw would be flagged.
//  - The decoder. gitleaks scans the text as written, then up to --max-decode-depth
//    (default 5) more times, each time after replacing the runs it can decode with their
//    decoded text. That is how the gate sees inside the base64 messages in bytes_b64. The
//    binary's decoder is not readable from here, so everything below was MEASURED on 8.30.1
//    with crafted files (a pair that is visible only once decoded; four samples of each
//    shape, with a control that differs only in the property named):
//      decoded:     a run of /[\w/+-]{16,}={0,2}/ whose bytes decode to 9..126 when it is
//                   standard-alphabet base64 that is padded or needs no padding, or
//                   URL-safe base64 without padding (or with none needed); a hit wrapped
//                   in base64 up to five times; runs up to 1 MiB (no length limit).
//      not decoded: standard base64 whose padding was stripped and that holds '+' or '/';
//                   URL-safe base64 with padding that holds '-' or '_'; a run with none of
//                   [0-9+/_-] (letters only; the same run with a digit-bearing tail
//                   decodes); any decoded text with a byte outside 9..126 (NUL, 0x01..0x08,
//                   DEL, anything non-ASCII); a hit wrapped six or more times.
//      also there:  hex (either case), percent-encoding and \uXXXX or U+XXXX escapes are
//                   decoded too (not \U0000XXXX, \xXX or &#xXX;).
//    The port models only the base64 decoder, and it differs from the binary in two
//    directions. (1) decodeBase64Runs below uses Buffer, which reads both alphabets with or
//    without padding in any mix and has no [0-9+/_-] rule, so it decodes some runs the
//    binary leaves encoded. That mostly adds text the gate never scans, which flags more.
//    It could hide a hit only where such a run is itself the secret of a pair completed in
//    the same pass, and that needs random key material to decode to printable ASCII (about
//    1e-16 for a 48-byte digest). (2) The hex, percent and unicode decoders are omitted, so
//    a pair hidden in one of those encodings would be missed. Neither difference showed up:
//    over 120 raw draws the real gate tripped on 14 and the port flagged the same 14, and
//    the committed file is flagged by neither. This is a measurement of one binary, not a
//    proof: a gitleaks upgrade (GITLEAKS_VERSION) must re-run the measurements above.
//  - Not ported, because each only ever suppresses a finding, so leaving it out makes the
//    predicate flag more and never less: the rule's other allowlist (a regex over the whole
//    match) and its stopwords, and the allowlists in gitleaks-gate.toml. A flagged draw
//    costs a redraw, nothing else.
// RE2 has the (?-i:[Aa]pi|API) group that JavaScript cannot express inside an /i regex,
// so no flag is used: every keyword is spelled case-insensitively and "Api", "api" and
// "API" stay the only spellings of that one.
const ci = (w) => [...w].map((c) => `[${c.toLowerCase()}${c.toUpperCase()}]`).join('');
const GL_KEYWORDS = [ci('access'), ci('auth'), '(?:Api|api|API)', ci('credential'), ci('creds'), ci('key'),
  `${ci('passw')}(?:${ci('or')})?${ci('d')}`, ci('secret'), ci('token')];
const GL_GENERIC_API_KEY = new RegExp(
  String.raw`[\w.-]{0,50}?(?:${GL_KEYWORDS.join('|')})(?:[ \t\w.-]{0,20})[\s'"]{0,3}` +
  String.raw`(?:=|>|:{1,3}=|\|\||:|=>|\?=|,)[\x60'"\s=]{0,5}` +
  String.raw`([\w.=-]{10,150}|[a-zA-Z0-9][a-zA-Z0-9+/]{11,}={0,3})(?:[\x60'"\s;]|\\[nrNR]|$)`, 'g');
const GL_ENTROPY = 3.5;
const GL_SECRET_ALLOWLIST = /^[a-zA-Z_.-]+$/;
const GL_MAX_DECODE_DEPTH = 5;
const GL_B64_RUN = /[\w/+-]{16,}={0,2}/g;

function shannon(s) {
  const counts = new Map();
  for (const ch of s) counts.set(ch, (counts.get(ch) ?? 0) + 1);
  let h = 0;
  for (const n of counts.values()) {
    const p = n / s.length;
    h -= p * Math.log2(p);
  }
  return h;
}

function genericApiKeyFinding(text) {
  for (const m of text.matchAll(GL_GENERIC_API_KEY)) {
    // 1e-9 only guards float noise at the threshold; it can only flag more.
    if (shannon(m[1]) > GL_ENTROPY - 1e-9 && !GL_SECRET_ALLOWLIST.test(m[1])) return true;
  }
  return false;
}

// Buffer's base64 reader is MORE lenient than the binary's (see section 5's header for
// the shapes 8.30.1 leaves encoded), so this decodes some runs the gate does not.
function decodeBase64Runs(text) {
  return text.replace(GL_B64_RUN, (run) => {
    const bytes = Buffer.from(run, 'base64');
    return bytes.length > 0 && bytes.every((b) => b >= 9 && b <= 126) ? bytes.toString('latin1') : run;
  });
}

// mirrorUnsafe reports whether the public-mirror gate would flag jsonText, the serialized
// vectors file.
export function mirrorUnsafe(jsonText) {
  let text = jsonText;
  for (let pass = 0; pass <= GL_MAX_DECODE_DEPTH; pass++) {
    if (genericApiKeyFinding(text)) return true;
    const next = decodeBase64Runs(text);
    if (next === text) return false;
    text = next;
  }
  return false;
}

export const MAX_DRAWS = 25;

export async function renderVectorFile(vectors) {
  const body = `${vectors.map((v) => JSON.stringify(v)).join(',\n')}\n]}\n`;
  return `{"generator_sha384":"${await generatorDigest()}","vectors_sha384":"${await vectorsDigest(body)}",` +
    `"vectors":[\n${body}`;
}

// buildSafeVectorFile draws until the public-mirror gate would pass the file. It builds
// and returns the text and never writes; main() writes. `unsafe` is injectable so a test
// can drive the loop without waiting for a real flagged draw.
export async function buildSafeVectorFile(maxDraws = MAX_DRAWS, unsafe = mirrorUnsafe) {
  for (let draw = 1; draw <= maxDraws; draw++) {
    const vectors = await buildVectors();
    const body = await renderVectorFile(vectors);
    if (!unsafe(body)) return { body, count: vectors.length, draws: draw };
    console.error(`draw ${draw} of ${maxDraws} would trip the public-mirror secret scan; drawing again`);
  }
  throw new Error(`${maxDraws} draws in a row would trip the public-mirror secret scan (mirrorUnsafe); ` +
    'a rule in the generator, not bad luck, is putting a credential-shaped pair in the file');
}

// ── 6. Verification world: realm, trust and vector helpers (PR-A2) ────────
// An independent authority model written from the design of record's §2.5,
// §2.6, §2.9–§2.12, §2.19 and §2.22, never from the Go code. Builders sign
// real scenarios and record the Reason the DoR assigns; Go verifies them and
// vitest checks their codec layer (#2628 verifies their Reasons in TS).

export const T0 = 1_760_000_000_000;
export const DAY = 86_400_000;
export const DELTA = 259_200_000;
export const GRACE = 900_000;
export const TAU = 600_000;
export const VMS = 259_200_000;
export const HW0 = { gen: 0, serial: 0, min_valid: 0 };

// DoR §2.12 Reason → Verdict (the generator's own copy).
export const VERDICT = Object.fromEntries([
  ...['ok', 'ok_accepted_pin'].map((r) => [r, 'ok']),
  ...['network', 'status_stale_tau', 'superseded_uncovered', 'status_expired', 'chain_behind', 'delegation_unknown',
    'device_behind', 'device_status_stale'].map((r) => [r, 'transient']),
  ...['suspended', 'disputed', 'held', 'erased', 'pending_erase', 'root_revoked', 'pending_from_active',
    'pending_from_suspended', 'pending_from_absent', 'pin_revoked', 'fork', 'hypothesis_conflict', 'field_mismatch',
    'anb_mismatch', 'issued_bounds', 'pin_changed', 'malformed', 'bad_signature', 'cak_unlisted', 'transition_invalid',
    'chain_gap', 'head_mismatch', 'status_future', 'absent_unpinned', 'absent_after_cutover', 'absent_after_enrolled',
    'delegation_window', 'out_of_scope', 'status_superseded', 'realm_mismatch', 'device_status_mismatch', 'device_gap',
    'device_fork', 'device_rule', 'wrap_invalid'].map((r) => [r, 'withhold']),
  ['trust_halt', 'halt'], ['sender_side', 'refuse-op'],
]);

export const A2_TYPES = ['chain', 'bundle-verify', 'device-ledger', 'device-wrap-open', 'apply-device-op', 'normalize',
  'pattern', 'for-op', 'p1'];

export async function wKey() {
  const k = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-384' }, true, ['sign', 'verify']);
  return { k, pt: new Uint8Array(await subtle.exportKey('raw', k.publicKey)) };
}
export const wSign = async (key, msg) =>
  new Uint8Array(await subtle.sign({ name: 'ECDSA', hash: 'SHA-384' }, key.k.privateKey, msg));
// Every root, candidate, operator, device and recovery-key signature is over
// the `sig` message (§2.5): the verifier rebuilds it.
export const signAs = (key, role, digest) => wSign(key, encode('sig', { role, digest }));

// fakeEk is a 550-byte RSA-OAEP-4096 SPKI with a random odd modulus: it passes
// §2.4's three checks and is fresh per call, which every rotation needs (D-243).
// Chains that never decrypt use it; wraps use real keys (ek()).
export function fakeEk() {
  const m = rnd(512);
  m[0] |= 0x80;
  m[511] |= 1;
  return concat(EK_HDR, m, EK_TRL);
}

export const b64s = (b) => (b && b.length ? b64e(b) : '');

export function expectOf(reason, extra = {}) {
  // Own keys only: `in` would admit 'toString' or 'constructor' from the prototype.
  if (!Object.hasOwn(VERDICT, reason)) throw new Error(`generator bug: unknown reason ${reason}`);
  return { reason, verdict: VERDICT[reason], ...extra };
}

// A realm id is distinct per realm and low in entropy, never crypto.randomUUID():
// realm_id is the first position of cp-authz, so a random UUID right after the
// "cp-authz" tag ("auth") is the pair gitleaks' generic-api-key rule flags once the
// public-mirror gate decodes the base64 (section 4's FIXUP['cp-authz'] fixes A1's
// realm_id for the same reason). resetDraw() restarts the sequence.
let realmSeq = 0;

export class Realm {
  static async create(o = {}) {
    const r = new Realm();
    r.id = o.id ?? `11111111-1111-4111-8111-${String(++realmSeq).padStart(12, '0')}`;
    r.gens = [await Realm.genKeys()];
    const g = r.gens[0];
    r.anchors = encode('anchors', { realm_id: r.id, primary_a: g.A.pt, primary_b: g.B.pt, recovery_a: g.RA.pt,
      recovery_b: g.RB.pt });
    r.items = [];
    r.dels = [];
    if (o.delegate !== false) await r.delegate(o.del ?? {});
    return r;
  }

  static async genKeys() {
    return { A: await wKey(), B: await wKey(), RA: await wKey(), RB: await wKey() };
  }

  // delegate signs a delegation (§2.5) with both primaries of its generation.
  // o.detached: sign without recording (for out-of-order and invalid items).
  // o.keys: sign with these primaries (a generation a detached succession made);
  // o.tls, o.pins: the authority TLS key and the cp_tls_pins (§2.4 key separation).
  async delegate(o = {}) {
    const gen = o.gen ?? this.gens.length - 1;
    const serial = o.serial ?? 1 + this.dels.filter((d) => d.v.product_root_gen === gen).length;
    const rsk = o.rsk ?? (await wKey());
    const cak = o.cak ?? (await wKey());
    const ops = o.ops ?? [];
    const v = { realm_id: this.id, serial, min_valid_serial: o.minValid ?? serial, product_root_gen: gen,
      rsk_public_keys: [rsk.pt], cak_public_keys: [cak, ...(o.moreCaks ?? [])].map((k) => k.pt),
      operator_public_keys: ops.map((k) => k.pt), authority_tls_keys: [(o.tls ?? (await wKey())).pt],
      cp_tls_pins: o.pins ?? [rnd(48)],
      not_before_ms: o.nb ?? T0 - DAY, not_after_ms: o.na ?? T0 + 180 * DAY, scope: o.scope ?? [...SCOPE],
      accepted_versions: ['1'], tau_ms: o.tau ?? TAU, delta_ms: o.delta ?? DELTA, g_ms: o.g ?? GRACE,
      cutover_at_ms: o.cutover ?? 0, ...(o.v ?? {}) };
    const msg = encode('delegation', v);
    const g = o.keys ?? this.gens[gen];
    const signed = encode('signed', { type: 'delegation', msg, sig_a: await wSign(o.signA ?? g.A, msg),
      sig_b: await wSign(o.signB ?? g.B, msg) });
    const d = { v, msg, signed, rsk, cak, ops };
    if (!o.detached) {
      this.dels.push(d);
      this.items.push(signed);
    }
    return d;
  }

  // succeed signs a succession (§2.5) with both recovery keys of the top generation.
  async succeed(o = {}) {
    const prev = this.gens.length - 1;
    const next = o.keys ?? (await Realm.genKeys());
    const v = { realm_id: this.id, prev_root_gen: prev, new_root_gen: prev + 1, new_primary_a: next.A.pt,
      new_primary_b: next.B.pt, new_recovery_a: next.RA.pt, new_recovery_b: next.RB.pt, ...(o.v ?? {}) };
    const msg = encode('succession', v);
    const g = this.gens[prev];
    const signed = encode('signed', { type: 'succession', msg, sig_a: await wSign(g.RA, msg),
      sig_b: await wSign(o.signB ?? g.RB, msg) });
    if (!o.detached) {
      this.gens.push(next);
      this.items.push(signed);
    }
    return signed;
  }

  // cur is T10's signing delegation: the highest (gen, serial) recorded.
  get cur() {
    if (!this.dels.length) {
      throw new Error('generator bug: the realm has recorded no delegation (Realm.create({ delegate: false })): ' +
        'call realm.delegate() or pass o.d');
    }
    return this.dels.reduce((a, d) => (d.v.product_root_gen > a.v.product_root_gen ||
      (d.v.product_root_gen === a.v.product_root_gen && d.v.serial > a.v.serial) ? d : a));
  }

  hw(d = this.cur) {
    return { gen: d.v.product_root_gen, serial: d.v.serial, min_valid: d.v.min_valid_serial };
  }

  async signStatus(values, d = this.cur, key) {
    const msg = encode('status', values);
    return encode('signed', { type: 'status', msg, sig_a: await wSign(key ?? d.rsk, msg), sig_b: E0 });
  }
}

// Codec self-check before a vector is written: every component the vector
// carries decodes and re-encodes to the same bytes, unless the vector exists
// to carry a malformed one. It is at least as strict as vectors.parity.test.ts
// (verificationVector), so a vector vitest would refuse is never written.
async function selfBytes(tag, bytes) {
  const v = await decode(tag, bytes);
  if (!equalBytes(encode(tag, v), bytes)) throw new Error(`generator bug: a ${tag} does not re-encode to its bytes`);
  return v;
}

async function selfDecode(tag, s) {
  if (!s) return null;
  return selfBytes(tag, b64d(s));
}

async function selfBundle(s) {
  const v = await selfDecode('bundle', s);
  for (const [n, t] of [['intent', 'intent'], ['result', 'result'], ['cp_authz', 'cp-authz'], ['ek_binding', 'ek-binding'],
    ['recovery_bind', 'recovery-bind'], ['held_status', 'signed']]) {
    if (v[n].length) await selfBytes(t, v[n]);
  }
}

async function selfDeviceBundle(s) {
  const v = await selfDecode('device-bundle', s);
  for (const [n, t] of [['device_intent', 'device-intent'], ['recovery_bind', 'recovery-bind'], ['cp_authz', 'cp-authz'],
    ['device_result', 'device-result']]) {
    if (v[n].length) await selfBytes(t, v[n]);
  }
}

// The primary `bytes_b64` layout per type, and per p1 `input.mode`: the same
// tables vectors.parity.test.ts reads (PRIMARY_TAG, P1_TAG). Other types name
// theirs in `input.tag`.
const PRIMARY_TAG = { chain: 'signed', 'device-ledger': 'signed', 'device-wrap-open': 'device-wrap',
  'apply-device-op': 'device-set', normalize: 'device-set' };
const P1_TAG = { status: 'signed', history: 'history', shape: 'bundle', 'bind-shape': 'recovery-bind' };

async function selfInputs(v) {
  await selfDecode('anchors', v.anchors);
  for (const s of v.trust ?? []) await selfDecode('signed', s);
  await selfDecode('chain-head', v.prev_head);
  for (const s of v.bundles ?? []) await selfBundle(s);
  await selfDecode('signed', v.fix);
  for (const s of v.device_bundles ?? []) await selfDeviceBundle(s);
  await selfDecode('signed', v.device_status);
  for (const s of v.statuses ?? []) await selfDecode('signed', s);
  if (v.prev_device_head) {
    const dh = await selfDecode('device-head', v.prev_device_head);
    if (!v.prev_head || !equalBytes(dh.chain_head_digest, await sha384(b64d(v.prev_head)))) {
      throw new Error(`generator bug: ${v.name}: prev_device_head is not paired with prev_head (D-269)`);
    }
  }
  if (v.input?.bind_b64) await selfDecode('recovery-bind', v.input.bind_b64);
  if (v.input?.new_dev) await parseDev(v.input.new_dev);
  const okish = v.expect.reason === 'ok' || v.expect.reason === 'ok_accepted_pin';
  if (!v.bytes_b64 || (!okish && ['device-wrap-open', 'pattern', 'for-op'].includes(v.type))) return;
  if (v.type === 'bundle-verify') {
    await selfBundle(v.bytes_b64);
    return;
  }
  if (v.type === 'p1') {
    const tag = Object.hasOwn(P1_TAG, v.input?.mode ?? '') ? P1_TAG[v.input.mode] : null;
    if (!tag) throw new Error(`generator bug: ${v.name}: p1 input.mode ${v.input?.mode} is not one of ${Object.keys(P1_TAG)}`);
    await selfDecode(tag, v.bytes_b64);
    await selfDecode(tag, v.input.other_b64);
    return;
  }
  const tag = PRIMARY_TAG[v.type] ?? v.input?.tag;
  if (!tag) throw new Error(`generator bug: ${v.name}: no primary layout for type ${v.type} (input.tag)`);
  await selfDecode(tag, v.bytes_b64);
}

export async function pushA2(ctx, v) {
  if (!A2_TYPES.includes(v.type)) throw new Error(`generator bug: type ${v.type}`);
  // An unknown reason is refused even with no verdict: undefined === undefined.
  if (!Object.hasOwn(VERDICT, v.expect?.reason ?? '')) throw new Error(`generator bug: reason ${v.expect?.reason} of ${v.name}`);
  if (v.expect.verdict !== VERDICT[v.expect.reason]) throw new Error(`generator bug: verdict of ${v.name}`);
  await selfDecode('chain-head', v.expect.head_b64);
  await selfDecode('device-set', v.expect.set_b64);
  await selfDecode('device-head', v.expect.device_head_b64);
  if (v.expect.reason !== 'malformed') await selfInputs(v);
  ctx.push(v);
}

// ── 7. Verification world: one account's published chain (PR-A2) ──────────
// DoR §2.7 (intent positions), §2.9 (rows 1–22, derived activation), §2.11
// (R2), §2.12 ("When the index moves"). Negative vectors use the mut* hooks
// and sign whatever the hook leaves: the generator holds the RSK.

export const RANK = { initial: 3, continuity: 3, recovery: 2, unverified: 1, '': 0 };
export const capA = (a, cap) => (RANK[a] <= RANK[cap] ? a : cap);
const PROPOSES = new Set(['enroll_new', 'enroll_existing', 'rotate_root', 'replace_root', 'recover']);
const EK_OPS = new Set(['enroll_new', 'enroll_existing', 'rotate_root', 'rotate_ek', 'replace_root', 'recover']);
const PRED_OPS = new Set(['rotate_root', 'rotate_ek', 'unsuspend', 'replace_root', 'recover']);
const D15_OPS = new Set(['enroll_new', 'enroll_existing', 'rotate_root', 'rotate_ek', 'replace_root', 'recover']);
const isPD = (s) => s.state === 'pending' || s.state === 'disputed';
const xOf = (s) => (isPD(s) ? s.prior : s.state);
const setOfX = (s) => (xOf(s) === 'suspended' ? s.susp : []);
const sortedSet = (a) => [...new Set(a)].sort();

// resetDraw() forgets it, so a re-draw makes a fresh legacy key from the fresh EK.
let legacyCache;
// legacy is the account's RSA legacy key (PSS signer; pin = SHA-384(SPKI)).
export async function legacy() {
  if (!legacyCache) {
    const { kp, spki } = await ek();
    const pss = await subtle.importKey('pkcs8', new Uint8Array(await subtle.exportKey('pkcs8', kp.privateKey)),
      { name: 'RSA-PSS', hash: 'SHA-256' }, false, ['sign']); // in memory only, never written
    legacyCache = { spki, pss, pin: await sha384(spki) };
  }
  return legacyCache;
}

// activated is §2.9's derived activation of a pending state (no event).
export function activated(s) {
  const drop = { prior: '', anb: 0, pend: null, pendEk: E0, pendGen: 0, recKey: null };
  if (!s.pend) return { ...s, ...drop, state: 'erased', susp: [] }; // Pe(x) → E, the root kept
  let set = [];
  if (s.prior === 'suspended') set = s.rr ? s.susp.filter((a) => a !== 'operator') : s.susp;
  return { ...s, ...drop, root: s.pend, ek: s.pendEk, ekGen: s.pendGen, rr: 0, seg: s.assurance,
    state: set.length ? 'suspended' : 'active', susp: set };
}

function restoreOf(f) {
  return { ...f, state: f.prior, prior: '', susp: f.prior === 'suspended' ? f.susp : [], pend: null, pendEk: E0,
    pendGen: 0, recKey: null };
}

// post is the To column of §2.9 for the row (op, actor, from) selects.
function post(op, actor, f, x) {
  const drop = { pend: null, pendEk: E0, pendGen: 0, recKey: null };
  switch (op) {
    case 'enroll_new':
      return { ...f, ...drop, state: 'active', prior: '', susp: [], root: x.cand, rr: 0, ek: x.ekd, ekGen: x.gen, seg: 'initial' };
    case 'enroll_existing':
    case 'replace_root':
      if (isPD(f)) return { ...f, ...drop, state: 'disputed', susp: setOfX(f) }; // rows 4, 12
      if (x.dispute) return { ...f, ...drop, state: 'disputed', prior: f.state, susp: setOfX(f) };
      return { ...f, state: 'pending', prior: f.state, susp: setOfX(f), pend: x.cand, pendEk: x.ekd, pendGen: x.gen, recKey: null };
    case 'rotate_root':
      return { ...f, root: x.cand, ek: x.ekd, ekGen: x.gen };
    case 'rotate_ek':
      return { ...f, ek: x.ekd, ekGen: x.gen };
    case 'suspend':
      return { ...f, state: 'suspended', susp: sortedSet([...f.susp, actor]) };
    case 'unsuspend': {
      const set = f.susp.filter((a) => a !== actor);
      return { ...f, state: set.length ? 'suspended' : 'active', susp: set };
    }
    case 'cancel_pending':
      return restoreOf(f);
    case 'revoke_root':
      if (xOf(f) === 'absent') return { ...f, ...drop, state: 'absent', prior: '', susp: [], pinRev: 1 };
      return { ...f, ...drop, state: 'suspended', prior: '', susp: sortedSet([...setOfX(f), 'operator']), rr: 1 };
    case 'erase':
      if (actor === 'operator') return { ...f, ...drop, state: 'erased', prior: '', susp: [] };
      return { ...f, ...drop, state: 'pending', prior: xOf(f), susp: setOfX(f) };
    case 'recover':
      return { ...f, state: 'pending', prior: xOf(f), susp: setOfX(f), pend: x.cand, pendEk: x.ekd, pendGen: x.gen, recKey: x.recKey };
  }
  throw new Error(`generator bug: op ${op}`);
}

function deltaRG(op, actor) {
  if (op === 'erase') return actor === 'operator' ? 0 : 1;
  return ['enroll_existing', 'replace_root', 'cancel_pending', 'revoke_root', 'recover'].includes(op) ? 1 : 0;
}

function assuranceFor(op, f, x) {
  switch (op) {
    case 'enroll_new':
      return 'initial';
    case 'enroll_existing':
      if (f.state !== 'absent' || x.dispute) return '';
      return x.pop ? 'continuity' : f.pinRev ? 'unverified' : 'recovery';
    case 'rotate_root':
      return RANK[f.seg] === 3 ? 'continuity' : f.seg;
    case 'replace_root':
      if (isPD(f) || x.dispute) return '';
      return f.rr ? 'unverified' : capA('recovery', f.seg);
    case 'recover':
      return capA('recovery', x.bindSeg ?? 'initial');
  }
  return '';
}

// rootSigned: the head's active root signs the intent (§2.7 last column).
const rootSigned = (op, actor, by) => op === 'rotate_root' || op === 'rotate_ek' ||
  (op === 'unsuspend' && actor === 'user') || (op === 'cancel_pending' && by === 'root');

// fromCode is §2.9's From-state code of a head (DoR:592–604).
export function fromCode(s, pin) {
  switch (s.state) {
    case 'absent':
      if (!pin.length) return 'A0';
      return s.pinRev ? 'Apr' : 'Ap';
    case 'active':
      return 'Ac';
    case 'suspended':
      if (s.rr) return 'Sor';
      if (s.susp.includes('operator')) return 'So';
      if (s.susp.length === 1 && s.susp[0] === 'user') return 'Su';
      break;
    case 'pending':
      return `${{ recover: 'Pr', erase: 'Pe' }[s.op] ?? 'P'}(${s.prior})`;
    case 'disputed':
      return `D(${s.prior})`;
    case 'erased':
      return 'E';
  }
  throw new Error(`generator bug: no From code for ${s.state} [${s.susp}]`);
}

// FROM is the generator's own transcription of §2.9's client-checkable rows
// 1–22 (DoR:609–632), never of Go's table: [row, op, actor, form, From]. The
// form is the signature that picks between rows sharing (op, actor): PoP on
// rows 2 and 3, and the cancel's signer (`by`) on rows 13, 14 and 21; null is
// any. Two conditions a row states outside its From cell are kept with it: row
// 13's root_revoked = 0 (its signature cell) and row 22's "only while held".
// P(x) excludes Pr(x) and Pe(x): the rows that admit them name them.
const isPDCode = (c) => /^[PD]\(/.test(c);
const FROM = [
  [1, 'enroll_new', 'user', null, (c) => c === 'A0'],
  [2, 'enroll_existing', 'user', 'pop', (c) => c === 'Ap'],
  [3, 'enroll_existing', 'user', 'no-pop', (c) => c === 'Ap' || c === 'Apr'],
  [4, 'enroll_existing', 'user', null, (c) => c === 'P(absent)' || c === 'D(absent)'],
  [5, 'rotate_root', 'user', null, (c) => c === 'Ac'],
  [6, 'rotate_ek', 'user', null, (c) => c === 'Ac'],
  [7, 'suspend', 'user', null, (c, s) => ['Ac', 'So', 'Sor'].includes(c) && !s.susp.includes('user')],
  [8, 'suspend', 'operator', null, (c) => c === 'Ac' || c === 'Su'],
  [9, 'unsuspend', 'user', null, (c, s) => s.state === 'suspended' && s.susp.includes('user') && !s.rr],
  [10, 'unsuspend', 'operator', null, (c, s) => s.state === 'suspended' && s.susp.includes('operator') && !s.rr],
  [11, 'replace_root', 'user', null, (c) => ['Ac', 'Su', 'Sor'].includes(c)],
  [12, 'replace_root', 'user', null, (c) => isPDCode(c) && (c.endsWith('(active)') || c.endsWith('(suspended)'))],
  [13, 'cancel_pending', 'user', 'root', (c, s) => (isPDCode(c) || c.startsWith('Pe(')) && !!s.root && !s.rr],
  [14, 'cancel_pending', 'user', 'pop', (c, s, m) => (isPDCode(c) || c.startsWith('Pe(')) && c.endsWith('(absent)') && m.livePin],
  [15, 'revoke_root', 'operator', null, (c) => ['Ac', 'Su', 'So'].includes(c)],
  [16, 'revoke_root', 'operator', null, (c) => isPDCode(c) && !c.endsWith('(absent)')],
  [17, 'revoke_root', 'operator', null, (c) => ['Ap', 'P(absent)', 'D(absent)'].includes(c)],
  [18, 'erase', 'user', null, (c) => !c.startsWith('Pe(') && !c.startsWith('Pr(') && c !== 'E'],
  [19, 'erase', 'operator', null, (c) => !c.startsWith('Pr(') && c !== 'E'],
  [20, 'recover', 'user', null, (c) => c !== 'E'],
  [21, 'cancel_pending', 'user', 'recovery', (c) => c.startsWith('Pr(')],
  [22, 'cancel_pending', 'restore', null, (c, s, m) => (isPDCode(c) || c.startsWith('Pr(')) && m.held],
];

// admit throws unless some row admits (op, actor, form) from the head, or the
// caller opted out for a negative vector (o.inadmissible), which must then
// really be inadmissible: a stale opt-out would hide the next generator bug.
function admit(op, actor, form, s, m, inadmissible) {
  const c = fromCode(s, m.pin);
  const ok = FROM.some(([, o, a, fm, from]) => o === op && a === actor && (fm === null || fm === form) && from(c, s, m));
  const what = `${op}/${actor} from ${c}${form ? ` (${form})` : ''}`;
  if (!ok && !inadmissible) throw new Error(`generator bug: inadmissible ${what}`);
  if (ok && inadmissible) throw new Error(`generator bug: o.inadmissible on the admissible ${what}`);
}

// transact runs one model op on a fork and adopts the fork only when the op
// returns and is not dry, so a dry or a throwing op leaves the live model's
// clock, state, head, index, ledger and private set exactly as they were (the
// index moves only when a result is verified, DoR:867).
async function transact(a, o, run) {
  const w = a.fork();
  const out = await run(w);
  if (!o.dry) Object.assign(a, w);
  return out;
}

const INTENT_WINDOW_MS = 660_000; // DoR §2.3

// insertSorted keeps an a<s> head array in EN3 order in the model, as Go's
// insertStr does: ascending bytewise over the text, not numerically (DoR:865,
// E3-07), so "11.…" precedes "9.…". encode() sorts an a<…> array as it writes,
// so the bytes never depended on this; the model state now matches them.
function insertSorted(xs, s) {
  if (xs.includes(s)) return;
  xs.push(s);
  xs.sort(cmp);
}

async function addRoot(h, rootPt, seg) {
  const d = b64e(await sha384(rootPt));
  if (!h.root_index.some((r) => r.startsWith(`${d}.`))) h.root_index.push(`${d}.${b64e(rootPt)}.${seg}`);
}

// checkRootReuse is the client rule of E2EE review A-B1: a row that installs
// its candidate root (rows 1, 2, 3, 5, 11 and 20; rows 2, 3 and 11 on either
// outcome) is transition_invalid when root_index already records that root,
// because addRoot keeps the old record and its old segment label. The
// authority refuses it first, as root_reused (DoR:683, §3.2.4 step 10), so the
// model never builds one except as a deliberate negative (o.reusedRoot), and a
// stale opt-out throws. Rows 4 and 12 (from P or D) burn their candidate.
async function checkRootReuse(h, op, f, cand, optOut) {
  const installs = op === 'recover' || !isPD(f);
  const d = b64e(await sha384(cand.pt));
  const reused = installs && h.root_index.some((r) => r.startsWith(`${d}.`));
  if (reused && !optOut) throw new Error(`generator bug: ${op} re-installs a root already in root_index (root_reused)`);
  if (!reused && optOut) throw new Error(`generator bug: o.reusedRoot on a ${op} that re-installs no root`);
}

export class Account {
  static async create(realm, o = {}) {
    const a = new Account();
    a.r = realm;
    a.id = crypto.randomUUID();
    a.pin = o.pin ? (await legacy()).pin : E0;
    a.s = { seq: 0, entry: E0, state: 'absent', prior: '', susp: [], root: null, rr: 0, ek: E0, ekGen: 0, pend: null,
      pendEk: E0, pendGen: 0, pinRev: 0, rg: 0, anb: 0, assurance: '', issued: 0, op: '', seg: '', recKey: null };
    a.h = { realm_id: realm.id, account_id: a.id, head_result: E0, fix: E0, active_ek_spki: E0, active_ek_generation: 0,
      pending_ek_spki: E0, pending_ek_generation: 0, pending_recovery_key: E0, seq_index: [], root_index: [],
      ek_index: [], recover_index: [] };
    a.clock = T0;
    a.bundles = [];
    a.results = [];
    a.heads = [encode('chain-head', a.h)];
    a.held = false;
    a.ledgerInit();
    return a;
  }

  headBytes() {
    return encode('chain-head', this.h);
  }

  mem(o = {}) {
    return { pin: o.pin ?? b64s(this.pin), everEnrolled: o.everEnrolled ?? 0, acceptedPin: o.acceptedPin ?? '' };
  }

  // fork copies the model so a builder can run a variant op (keys are shared).
  fork() {
    const c = Object.assign(Object.create(Account.prototype), this);
    c.s = { ...this.s, susp: [...this.s.susp] };
    c.h = { ...this.h, seq_index: [...this.h.seq_index], root_index: [...this.h.root_index],
      ek_index: [...this.h.ek_index], recover_index: [...this.h.recover_index] };
    c.bundles = [...this.bundles];
    c.results = [...this.results];
    c.heads = [...this.heads];
    c.forkLedger(this);
    return c;
  }

  // settle is the authority's Normalize before an op at `issued`: a raw
  // pending head that is not held activates at its anb (no extension here).
  // o.noActivate keeps it raw (an extended `visible`, the H_pend hypothesis).
  async settle(issued, o = {}) {
    if (!o.noActivate && this.s.state === 'pending' && !this.held && issued >= this.s.anb) await this.activate();
  }

  // activate records the activation in the head index (§2.12, E3-06) and
  // moves the model to the derived state.
  async activate() {
    const prev = this.s;
    const n = prev.seq;
    if (prev.pend) {
      const act = activated(prev);
      if (act.state === 'active') {
        this.h.seq_index[n - 1] = `${this.h.seq_index[n - 1].split('.')[0]}.${b64e(await sha384(prev.pend.pt))}`;
      }
      await addRoot(this.h, prev.pend.pt, prev.assurance);
      this.h.active_ek_spki = this.h.pending_ek_spki;
      this.h.active_ek_generation = this.h.pending_ek_generation;
      if (prev.op === 'recover') insertSorted(this.h.recover_index, `${n}.${b64e(await sha384(this.h.pending_recovery_key))}`);
    }
    this.h.pending_ek_spki = E0;
    this.h.pending_ek_generation = 0;
    this.h.pending_recovery_key = E0;
    this.s = activated(prev);
    await this.onActivate(prev);
  }

  // op runs one published op (o.dry: build the bundle only). It runs on a
  // fork (transact), so a dry or throwing op never moves the live model.
  // o.inadmissible opts a negative vector out of the §2.9 From guard (admit).
  // On a recover, o.mutB and o.bindRoot edit its binding proof (recoveryFor),
  // and o.recoverySigner signs its sig_recovery in place of B.recovery_key.
  async op(op, o = {}) {
    return transact(this, o, (w) => w.runOp(op, o));
  }

  async runOp(op, o) {
    const actor = o.actor ?? 'user';
    this.clock = Math.max(this.clock + 60_000, o.at ?? 0);
    const issued = o.at ?? this.clock;
    await this.settle(issued, { noActivate: o.noActivate, noComplete: o.noComplete });
    const f = this.s;
    const d = o.d ?? this.r.cur;
    // Row 22's values (§3.4 clear-held, DoR:1691): asserted_at = issued_at = T and
    // expires_at = T + INTENT_WINDOW_MS; every other op asserts 1 s before issue.
    const restore = actor === 'restore';
    const asserted = o.asserted ?? (restore ? issued : issued - 1_000);
    const by = o.by ?? (op !== 'cancel_pending' || restore ? ''
      : f.op === 'recover' && f.state === 'pending' ? 'recovery' : f.root && xOf(f) !== 'absent' ? 'root' : 'pop');
    const x = { cand: null, ekb: null, ekbBytes: E0, ekd: E0, spki: E0, gen: 0, pop: !!o.pop || by === 'pop',
      dispute: !!o.dispute, recKey: null, bindB: E0, bindSig: E0, bindSeg: o.bindSeg };
    const form = op === 'enroll_existing' ? (x.pop ? 'pop' : 'no-pop') : op === 'cancel_pending' && !restore ? by : null;
    admit(op, actor, form, f, { pin: this.pin, livePin: this.pin.length > 0 && !f.pinRev, held: this.held }, o.inadmissible);
    if (PROPOSES.has(op)) x.cand = o.cand ?? (await wKey());
    if (x.cand) await checkRootReuse(this.h, op, f, x.cand, o.reusedRoot);
    else if (o.reusedRoot) throw new Error(`generator bug: o.reusedRoot on ${op}, which proposes no root`);
    if (op === 'recover') Object.assign(x, await this.recoveryFor(o));
    const ekSigner = op === 'rotate_ek' ? f.root : x.cand;
    if (EK_OPS.has(op)) {
      x.spki = o.spki ?? fakeEk();
      x.gen = f.ekGen + 1;
      x.ekb = { realm_id: this.r.id, account_id: this.id, root_digest: await sha384(ekSigner.pt), ek_generation: x.gen,
        ek_alg: 'rsa-oaep-4096-sha256', ek_public_key: x.spki };
      o.mutEkb?.(x.ekb);
      x.ekbBytes = encode('ek-binding', x.ekb);
      x.ekd = await sha384(x.ekbBytes);
    }
    const intent = { realm_id: this.r.id, account_id: this.id, request_id: crypto.randomUUID(), op,
      expires_at_ms: o.expires ?? asserted + (restore ? INTENT_WINDOW_MS : 600_000), expected_seq: f.seq,
      expected_recovery_gen: f.rg, expected_head_digest: f.entry, predecessor_root_digest: E0, candidate_root: x.cand ? x.cand.pt : E0,
      ek_binding_digest: x.ekd, legacy_ek_digest: x.pop ? this.pin : E0, device_intent_digest: E0,
      recovery_bind_digest: x.bindB.length ? await sha384(x.bindB) : E0 };
    if (PRED_OPS.has(op) || (op === 'cancel_pending' && by === 'root')) intent.predecessor_root_digest = await sha384(f.root.pt);
    x.requestId = intent.request_id;
    const dev = D15_OPS.has(op) || (op === 'unsuspend' && actor === 'user') ? await this.attach(op, actor, f, x, o) : null;
    if (dev) intent.device_intent_digest = dev.D;
    o.mutIntent?.(intent);
    const intentB = encode('intent', intent);
    const intentD = await sha384(intentB);
    const evidence = encode('cp-evidence', { request_id: intent.request_id, factors: ['password'], mfa_enrolled: 0,
      password_login: 1, recovery_window_clear: 1, operator_assisted: 0, credential_epoch_digest: E0, evidence_salt: rnd(32) });
    const authz = { realm_id: this.r.id, account_id: this.id, request_id: intent.request_id, intent_digest: intentD, actor,
      asserted_at_ms: asserted, evidence_digest: await sha384(actor === 'restore' ? o.heldStatus : evidence) };
    o.mutAuthz?.(authz);
    const authzB = encode('cp-authz', authz);
    const p = post(op, actor, f, x);
    const pend = p.state === 'pending';
    const result = { realm_id: this.r.id, account_id: this.id, request_id: intent.request_id, intent_digest: intentD,
      cp_authz_digest: await sha384(authzB), op, seq: f.seq + 1, state: p.state,
      prior_state: isPD(p) ? p.prior : '', suspended_by: p.susp, active_root: p.root ? p.root.pt : E0,
      root_revoked: p.rr, ek_binding_digest: p.ek, pending_root: p.pend ? p.pend.pt : E0,
      pending_ek_binding_digest: p.pendEk, legacy_ek_digest: this.pin, legacy_pin_revoked: p.pinRev,
      prev_entry_digest: f.entry, recovery_gen: f.rg + deltaRG(op, actor),
      activation_not_before_ms: pend ? Math.min(asserted + d.v.delta_ms + d.v.g_ms, MAX_SAFE) : 0,
      assurance: o.assurance ?? assuranceFor(op, f, x), issued_at_ms: issued, delegation_gen: d.v.product_root_gen,
      delegation_serial: d.v.serial };
    o.mutResult?.(result);
    const resultB = encode('result', result);
    const b = { intent: intentB, sig_candidate: E0, sig_current_root: E0, sig_operator: E0, sig_recovery: E0,
      legacy_pop_sig: E0, legacy_spki: E0, ek_binding: x.ekbBytes, ek_binding_sig: x.ekb ? await wSign(ekSigner, x.ekbBytes) : E0,
      recovery_bind: x.bindB, recovery_bind_sig: x.bindSig, cp_authz: authzB,
      cp_authz_sig: actor === 'restore' ? E0 : await wSign(o.cak ?? d.cak, authzB), result: resultB,
      result_sig: await wSign(o.rsk ?? d.rsk, resultB), held_status: actor === 'restore' ? o.heldStatus : E0 };
    if (x.cand) b.sig_candidate = await signAs(x.cand, 'candidate', intentD);
    if (rootSigned(op, actor, by)) b.sig_current_root = await signAs(f.root, 'current-root', intentD);
    if (actor === 'operator') b.sig_operator = await signAs(o.opKey ?? d.ops[0], 'operator', intentD);
    if (op === 'recover') b.sig_recovery = await signAs(o.recoverySigner ?? x.recKey, 'recovery-key', intentD);
    if (by === 'recovery') b.sig_recovery = await signAs(o.recKey ?? f.recKey, 'recovery-key', intentD);
    if (x.pop) {
      const L = await legacy();
      const pop = encode('legacy-ek-pop', { realm_id: this.r.id, account_id: this.id, intent_digest: intentD,
        legacy_ek_digest: this.pin });
      b.legacy_spki = L.spki;
      b.legacy_pop_sig = new Uint8Array(await subtle.sign({ name: 'RSA-PSS', saltLength: 32 }, L.pss, pop));
    }
    await o.mutBundle?.(b);
    const bundle = encode('bundle', b);
    if (o.dry) return bundle;
    const entry = await sha384(resultB);
    this.s = { ...p, seq: result.seq, entry, rg: result.recovery_gen, anb: result.activation_not_before_ms,
      assurance: result.assurance, issued, op };
    await this.appendHead(result, resultB, x.ekb, x.bindB, f);
    this.results.push(result);
    this.bundles.push(bundle);
    this.heads.push(this.headBytes());
    await this.afterResult(op, actor, f, { result, resultB, dev, x, d });
    return bundle;
  }

  // appendHead is §2.12's index move for the new result (decision A9).
  async appendHead(result, resultB, ekb, bindB, f) {
    const h = this.h;
    let bindRoot = '';
    if (result.state === 'active') {
      bindRoot = b64e(await sha384(result.active_root));
      await addRoot(h, result.active_root, result.op === 'enroll_new' ? 'initial' : f.seg);
    }
    h.seq_index.push(`${b64e(await sha384(resultB))}.${bindRoot}`);
    if (ekb) {
      h.ek_index.push(await sha384(ekb.ek_public_key));
      if (result.state === 'active') {
        h.active_ek_spki = ekb.ek_public_key;
        h.active_ek_generation = ekb.ek_generation;
      } else if (result.state === 'pending') {
        h.pending_ek_spki = ekb.ek_public_key;
        h.pending_ek_generation = ekb.ek_generation;
      }
    }
    // pending_ek_spki is the binding at head_result #17 (§2.12 chain-head), so
    // it empties whenever #17 is "": every non-pending result, and a pending
    // erase, which burns a pending candidate (row 18 from P).
    if (!result.pending_ek_binding_digest.length) {
      h.pending_ek_spki = E0;
      h.pending_ek_generation = 0;
    }
    h.pending_recovery_key = result.op === 'recover' && result.state === 'pending'
      ? (await decode('recovery-bind', bindB)).recovery_key : E0;
    h.head_result = resultB;
    h.fix = E0;
  }

  // status signs the effective state at as_of (R5): pending while held or
  // before `visible` (default anb), otherwise the derived activation.
  async status(o = {}) {
    const asOf = o.asOf ?? this.clock + 1_000;
    const d = o.d ?? this.r.cur;
    const held = o.held ?? (this.held ? 1 : 0);
    let s = this.s;
    let anb = 0;
    if (s.state === 'pending') {
      const visible = o.visible ?? s.anb;
      if (held || asOf < visible) anb = held ? MAX_SAFE : visible;
      else s = activated(s);
    }
    const v = { realm_id: this.r.id, account_id: this.id, seq: s.seq, head_entry_digest: s.entry, state: s.state,
      prior_state: isPD(s) ? s.prior : '', suspended_by: s.susp, active_root_digest: s.root ? await sha384(s.root.pt) : E0,
      root_revoked: s.rr, ek_binding_digest: s.ek, pending_root_digest: s.pend ? await sha384(s.pend.pt) : E0,
      pending_ek_binding_digest: s.pendEk, legacy_ek_digest: this.pin, legacy_pin_revoked: s.pinRev,
      activation_not_before_ms: anb, held, as_of_ms: asOf, valid_until_ms: asOf + (o.v ?? VMS),
      delegation_gen: d.v.product_root_gen, delegation_serial: d.v.serial };
    o.mut?.(v);
    return this.r.signStatus(v, d, o.key);
  }
}

// Section 8 replaces these with the private ledger model. Until then an
// attached device part is a well-formed device-intent that co-members never
// see (§2.19 P1): fresh new device, or an arbitrary actor.
Account.prototype.ledgerInit = function () {};
Account.prototype.forkLedger = function () {};
Account.prototype.onActivate = async function () {};
Account.prototype.afterResult = async function () {};
Account.prototype.recoveryFor = async function () {
  throw new Error('generator bug: recover needs section 8');
};
Account.prototype.attach = async function (op, actor, f, x) {
  const proposal = !(op === 'rotate_root' || op === 'rotate_ek' || op === 'unsuspend');
  const nd = proposal ? await wKey() : null;
  const ndx = proposal ? await wKey() : null;
  const di = { realm_id: this.r.id, account_id: this.id, request_id: x.requestId, op, salt: rnd(32), expires_at_ms: 0,
    expected_seq: f.seq, expected_head_digest: f.entry, expected_dseq: proposal ? 0 : 1,
    expected_device_head: proposal ? E0 : rnd(48), actor_device_id: proposal ? '' : crypto.randomUUID(),
    new_device_id: proposal ? await deviceId(nd.pt) : '', new_device_dsk: proposal ? nd.pt : E0,
    new_device_dkx: proposal ? ndx.pt : E0, removed_device_ids: [], recovery_bind_digest: E0, bind_wait: '' };
  const bytes = encode('device-intent', di);
  return { di: bytes, D: await sha384(bytes) };
};

// ── 8. Verification world: the private ledger (PR-A2) ──────────────────────
// DoR §2.5 device messages, §2.9 DR1–DR9, §2.19, §2.22. jsApply and
// jsNormalize are the generator's own DR rules (D-270's second implementation).

export async function newDevice(mature = 0) {
  const dsk = await wKey();
  const kx = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-384' }, true, ['deriveBits']);
  const dkx = { k: kx, pt: new Uint8Array(await subtle.exportKey('raw', kx.publicKey)) };
  return { id: await deviceId(dsk.pt), dsk, dkx, mature };
}
export const entryOf = (dev) => ({ id: dev.id, dsk: dev.dsk.pt, dkx: dev.dkx.pt, mature: dev.mature });
export const devStr = (e) => `${e.id}.${b64e(e.dsk)}.${b64e(e.dkx)}.${e.mature}`;
export const emptyS = () => ({ active: [], pending: null, key: E0, dig: E0, rkey: E0, rdig: E0, nb: 0 });
const cloneS = (S) => ({ ...S, active: [...S.active] });
const noSlot = { rkey: E0, rdig: E0, nb: 0 };

export const encodeS = (realm, account, S) => encode('device-set', { realm_id: realm, account_id: account,
  active: S.active.map(devStr), pending: S.pending ? devStr(S.pending) : '', recovery_key: S.key, recovery_bind_digest: S.dig,
  replacement_key: S.rkey, replacement_bind_digest: S.rdig, replacement_nb_ms: S.nb });

export async function decodeS(bytes) {
  const v = await decode('device-set', bytes);
  const ent = async (s) => {
    const p = await parseDev(s);
    return { id: p.id, dsk: p.dsk, dkx: p.dkx, mature: p.matureAtMs };
  };
  return { active: await Promise.all(v.active.map(ent)), pending: v.pending ? await ent(v.pending) : null,
    key: v.recovery_key, dig: v.recovery_bind_digest, rkey: v.replacement_key, rdig: v.replacement_bind_digest,
    nb: v.replacement_nb_ms };
}

// bindKindOk is §2.22's per-kind position rule.
export function bindKindOk(b) {
  const has = (x) => x.length > 0;
  if (b.kind === 'bind') {
    return b.expires_at_ms === 0 && has(b.recovery_key) && !has(b.target_digest) && b.chain_seq >= 1 &&
      has(b.chain_entry_digest) && has(b.binding_root_digest) && has(b.escrow_public_key);
  }
  if (b.kind === 'unbind') {
    return b.expires_at_ms === 0 && !has(b.recovery_key) && !has(b.target_digest) && b.chain_seq >= 1 &&
      has(b.chain_entry_digest) && has(b.binding_root_digest) && !has(b.escrow_public_key);
  }
  return b.expires_at_ms > 0 && has(b.recovery_key) && b.chain_seq === 0 && !has(b.chain_entry_digest) &&
    !has(b.binding_root_digest) && !has(b.escrow_public_key);
}

const ACTOR_OPS = new Set(['add_device', 'bind_recovery', 'rotate_root', 'rotate_ek', 'unsuspend']);

// bindWaitMs is #19's wait per value. A switch, not an object lookup, so an
// inherited name ('toString', 'constructor') is undefined like any other unknown.
function bindWaitMs(bindWait, delta) {
  switch (bindWait) {
    case '':
    case 'now':
      return 0;
    case 'delta':
      return delta;
    case 'replace':
      return 2_592_000_000;
    default:
      return undefined;
  }
}

async function jsBind(S, B, inp, issued, delta) {
  if (B.kind === 'cancel') return 'wrong_state';
  if (B.chain_seq !== inp.head.seq) return 'stale_seq';
  if (!equalBytes(B.chain_entry_digest, inp.head.entry)) return 'stale_head';
  if (!equalBytes(B.binding_root_digest, inp.head.root)) return 'wrong_state';
  const wait = bindWaitMs(inp.bindWait, delta);
  if (wait === undefined || issued + wait > MAX_SAFE) return 'wrong_state';
  const nb = issued + wait;
  const h = await sha384(inp.bind);
  const bound = S.key.length > 0;
  const slot = S.rdig.length > 0;
  const now = inp.bindWait === 'now';
  if (B.kind === 'unbind') {
    if (!bound) return 'wrong_state';
    if (inp.priorSig) {
      Object.assign(S, { key: E0, dig: E0 }, noSlot);
      return '';
    }
    if (slot) return 'wrong_state';
    Object.assign(S, now ? { key: E0, dig: E0 } : { rkey: E0, rdig: h, nb });
    return '';
  }
  if (inp.priorSig) {
    if (!bound) return 'wrong_state';
    Object.assign(S, { key: B.recovery_key, dig: h }, noSlot);
    return '';
  }
  if (slot) return 'wrong_state';
  Object.assign(S, now ? { key: B.recovery_key, dig: h } : { rkey: B.recovery_key, rdig: h, nb });
  return '';
}

function jsCancel(S, B, inp) {
  if (B.kind !== 'cancel' || !S.rdig.length || (B.target_digest.length && !equalBytes(B.target_digest, S.rdig))) {
    return 'wrong_state';
  }
  const bound = S.key.length > 0;
  if (inp.rootForm) {
    if (bound || !equalBytes(B.recovery_key, S.rkey)) return 'wrong_state';
  } else if (!bound || !equalBytes(B.recovery_key, S.key)) {
    return 'recovery_key_unbound';
  }
  Object.assign(S, noSlot);
  return '';
}

async function jsBody(S, B, inp, issued, delta) {
  switch (inp.op) {
    case 'enroll_new':
      S.active = [{ ...inp.new, mature: issued }];
      S.pending = null;
      return '';
    case 'enroll_existing':
    case 'replace_root':
    case 'recover':
      if (inp.op === 'recover' && (!S.dig.length || !equalBytes(await sha384(inp.bind), S.dig))) return 'recovery_key_unbound';
      if (inp.op === 'recover' && inp.outcome !== 'P') return 'wrong_state';
      if (inp.outcome === 'P') {
        S.pending = { ...inp.new, mature: inp.anb };
        if (inp.op === 'recover') Object.assign(S, noSlot);
      } else if (inp.outcome === 'D') {
        S.pending = null;
      } else {
        return 'wrong_state';
      }
      return '';
    case 'rotate_root':
    case 'rotate_ek':
      S.active = S.active.filter((e) => !(inp.removed ?? []).includes(e.id));
      if (inp.op === 'rotate_root') Object.assign(S, noSlot);
      return '';
    case 'unsuspend':
      return '';
    case 'add_device':
      if (issued + delta > MAX_SAFE) return 'wrong_state';
      S.active.push({ ...inp.new, mature: issued + delta });
      return '';
    case 'bind_recovery':
      return jsBind(S, B, inp, issued, delta);
    case 'cancel_recovery_bind':
      return jsCancel(S, B, inp);
  }
  return 'wrong_state';
}

// jsApply is the generator's ApplyDeviceOp: step 6's malformed_for_op, the
// head gates, DR1, DR4, DR2, then DR5/DR6/DR9 (a), (b), (e).
export async function jsApply(S0, inp, issued, delta) {
  APPLY_LOG?.push({ S0: cloneS(S0), inp: { ...inp, head: { ...inp.head } }, issued, delta });
  const S = cloneS(S0);
  const no = (refusal) => ({ S: null, refusal });
  let B = null;
  if (['bind_recovery', 'cancel_recovery_bind', 'recover'].includes(inp.op)) {
    try {
      B = await decode('recovery-bind', inp.bind);
    } catch {
      // Unreachable through a verified envelope: the bundle and device-bundle
      // codecs refuse an undecodable B as malformed before any device rule
      // runs. The rules are total over their input, so they refuse it rather
      // than throw, with wrong_state: malformed_for_op is step 6's verdict on a
      // decoded B's per-kind positions, and nothing about this B was decoded.
      // Go's preCheck returns the same (D-270: one rule, two implementations).
      return no('wrong_state');
    }
    if (!bindKindOk(B)) return no('malformed_for_op');
    if (inp.op === 'bind_recovery' && inp.priorSig === (inp.bindWait !== '')) return no('malformed_for_op');
  }
  if ((inp.op === 'add_device' || inp.op === 'bind_recovery') && (inp.head.state !== 'active' || inp.head.fu)) return no('wrong_state');
  if (inp.op === 'cancel_recovery_bind' && inp.head.state === 'erased') return no('wrong_state');
  const actorOp = inp.op === 'cancel_recovery_bind' ? !!inp.rootForm : ACTOR_OPS.has(inp.op);
  const at = (id) => S.active.findIndex((e) => e.id === id);
  if (actorOp) {
    if (at(inp.actor) < 0) return no('device_unknown');
    for (const id of inp.removed ?? []) if (id === inp.actor || at(id) < 0) return no('device_unknown');
  }
  if (inp.new && (at(inp.new.id) >= 0 || S.pending?.id === inp.new.id || (await deviceId(inp.new.dsk)) !== inp.new.id)) {
    return no('device_unknown');
  }
  if (actorOp && S.active[at(inp.actor)].mature > issued) return no('device_too_new');
  if (inp.op === 'add_device' && S.active.length >= 10) return no('device_limit');
  const r = await jsBody(S, B, inp, issued, delta);
  if (r) return no(r);
  if (S.rkey.length && equalBytes(S.rkey, S.key)) return no('wrong_state'); // the set codec refuses it
  return { S, refusal: '' };
}

// jsNormalize is DR8 and DR9 (c), in NormalizeDeviceSet's order: clearPending,
// emptySlot, completeSlot, activate, burnRecovery.
export function jsNormalize(S0, f) {
  NORM_LOG?.push({ S0: cloneS(S0), f: { ...f } });
  const S = cloneS(S0);
  if (f.clearPending) S.pending = null;
  if (f.emptySlot) Object.assign(S, noSlot);
  if (f.complete && S.rdig.length) Object.assign(S, S.rkey.length ? { key: S.rkey, dig: S.rdig } : { key: E0, dig: E0 }, noSlot);
  if (f.activate) {
    if (S.pending) {
      S.active = [S.pending];
      S.pending = null;
    }
    Object.assign(S, noSlot);
  }
  if (f.burn) Object.assign(S, { key: E0, dig: E0 });
  return S;
}

Account.prototype.ledgerInit = function () {
  this.L = { S: emptyS(), devs: new Map(), dseq: 0, last: E0, lastResult: E0, admitted: new Map(), secrets: [], self: '',
    dbundles: [], bound: null, slot: null, slotG: GRACE, slotExt: 0, indexAhead: false };
};

Account.prototype.forkLedger = function (src) {
  const L = src.L;
  this.L = { ...L, S: cloneS(L.S), devs: new Map(L.devs), admitted: new Map(L.admitted), secrets: [...L.secrets],
    dbundles: [...L.dbundles] };
};

Account.prototype.headInput = async function () {
  return { state: this.s.state, seq: this.s.seq, entry: this.s.entry, root: this.s.root ? await sha384(this.s.root.pt) : E0, fu: false };
};

Account.prototype.completeSlot = function () {
  this.L.S = jsNormalize(this.L.S, { complete: true });
  this.L.bound = this.L.S.key.length ? this.L.slot : null;
  this.L.slot = null;
};

// installsRoot: a pending head with a pending root, P(x) or Pr(x). Its derived
// activation is the only one that touches the private set: active := {pending},
// the slot emptied, and for Pr the bound key burned (DR8, DR9 (c), (d); §3.2.4
// step 7). A Pe(x) head has no pending root and activates to E with the root,
// the set, the pending device and the slot all kept (DoR:670; DR8 "erase keep[s]
// the set; an erased account keeps it as part of the tombstone"). So a row-18
// erase from P(x) leaves the burned candidate's private device in `pending`,
// and it never becomes active (literal DR8; DoR:634's burn list omits row 18).
const installsRoot = (s) => s.state === 'pending' && !!s.pend;

// settle: the slot completes at nb + g + L.slotExt unless held (DR9 (c)
// completes on any head that is not held). L.slotExt is the authority's own
// extension of that instant, which own devices cannot see: the cancel-
// unavailability accumulator's growth (at most EXTENSION_CAP_MS, D-274) or a
// post-restore floor (D-286); a builder sets it after the slot fills, and a new
// fill resets it. o.noComplete holds one settle back the same way. When a
// new-root activation is also due, the earlier applies first, a tie completing
// first (A10, C4-4). o.noActivate keeps the head raw only.
const settlePublished = Account.prototype.settle;
Account.prototype.settle = async function (issued, o = {}) {
  const S = this.L.S;
  const tc = S.rdig.length && !this.held && !o.noComplete ? S.nb + this.L.slotG + this.L.slotExt : Infinity;
  const ta = installsRoot(this.s) && !this.held && !o.noActivate ? this.s.anb : Infinity;
  if (tc <= issued && tc <= ta) this.completeSlot();
  await settlePublished.call(this, issued, o);
};

Account.prototype.onActivate = async function (prev) {
  if (!prev.pend) return; // Pe(x) → E: nothing private moves
  this.L.S = jsNormalize(this.L.S, { activate: true, burn: prev.op === 'recover' });
  this.L.slot = null;
  if (prev.op === 'recover') this.L.bound = null;
};

// recoveryFor is the binding proof a `recover` carries (§2.22): the bound
// record (o.binding replays an older one), its B and the binding root's
// signature over H(B). o.mutB edits B and re-signs it under the binding root
// (o.bindRoot names another signer), so the proof's own signature stays valid
// and only the edited position can refuse it; intent #16 follows the edited B.
// It builds negative vectors only: a committed recover would keep a pending
// recovery key the model never bound. (o.recoverySigner, which signs the
// bundle's sig_recovery, is read by runOp.)
Account.prototype.recoveryFor = async function (o) {
  const b = o.binding ?? this.L.bound;
  if (!b) throw new Error('generator bug: recover with no bound key');
  let bindB = o.bindB ?? b.BB;
  let bindSig = o.bindSig ?? b.sigRoot;
  if (o.mutB) {
    if (!o.dry) throw new Error('generator bug: o.mutB builds a negative recover (dry only)');
    const B = await decode('recovery-bind', bindB);
    await o.mutB(B);
    bindB = encode('recovery-bind', B);
    bindSig = o.bindSig ?? (await signAs(o.bindRoot ?? b.root, 'current-root', await sha384(bindB)));
  }
  return { recKey: b.key, bindB, bindSig, bindSeg: o.bindSeg ?? b.seg };
};

// actingDevice is the default actor: self while it is active; once a derived
// activation has dropped every old device (DR8), the device it installed.
const actingDevice = (L) => L.devs.get(L.S.active.some((e) => e.id === L.self) ? L.self : L.S.active[0]?.id);

Account.prototype.attach = async function (op, actor, f, x, o) {
  const L = this.L;
  const proposal = !['rotate_root', 'rotate_ek', 'unsuspend'].includes(op);
  const nd = proposal ? (o.newDev ?? (await newDevice())) : null;
  const act = proposal ? null : (o.actorDev ?? actingDevice(L));
  const di = { realm_id: this.r.id, account_id: this.id, request_id: x.requestId, op, salt: rnd(32), expires_at_ms: 0,
    expected_seq: f.seq, expected_head_digest: f.entry, expected_dseq: proposal ? 0 : L.dseq,
    expected_device_head: proposal ? E0 : L.last, actor_device_id: act ? act.id : '', new_device_id: nd ? nd.id : '',
    new_device_dsk: nd ? nd.dsk.pt : E0, new_device_dkx: nd ? nd.dkx.pt : E0, removed_device_ids: o.removed ?? [],
    recovery_bind_digest: E0, bind_wait: '' };
  o.mutDi?.(di);
  const bytes = encode('device-intent', di);
  const D = await sha384(bytes);
  return { di: bytes, D, nd, act, removed: di.removed_device_ids,
    sigNew: nd ? await signAs(nd.dsk, 'new-device', D) : E0,
    sigActor: act ? await signAs(o.actorKey ?? act.dsk, 'actor-device', D) : E0 };
};

Account.prototype.pushEntry = async function (e) {
  const L = this.L;
  const dr = { realm_id: this.r.id, account_id: this.id, request_id: e.requestId, device_intent_digest: e.D,
    cp_authz_digest: e.cpAuthz.length ? await sha384(e.cpAuthz) : E0, op: e.op, dseq: L.dseq + 1,
    prev_device_entry_digest: L.last, chain_seq: e.chainSeq, chain_entry_digest: e.chainEntry,
    device_set_digest: await sha384(encodeS(this.r.id, this.id, L.S)), issued_at_ms: e.issued,
    delegation_gen: e.d.v.product_root_gen, delegation_serial: e.d.v.serial };
  e.mutDr?.(dr);
  const drB = encode('device-result', dr);
  const db = { device_intent: e.di, sig_root: e.sigRoot ?? E0, sig_actor: e.sigActor ?? E0, sig_new_device: e.sigNew ?? E0,
    recovery_bind: e.bind ?? E0, sig_recovery: e.sigRecovery ?? E0, sig_recovery_prior: e.sigPrior ?? E0,
    cp_authz: e.cpAuthz, cp_authz_sig: e.cpAuthzSig, device_result: drB, device_result_sig: await wSign(e.rsk ?? e.d.rsk, drB) };
  e.mutDb?.(db);
  const bytes = encode('device-bundle', db);
  L.dseq += 1;
  L.last = await sha384(drB);
  L.lastResult = drB;
  L.dbundles.push(bytes);
  return bytes;
};

Account.prototype.recordSecrets = function (op, rootPt) {
  if (this.L.self && this.L.S.active.some((e) => e.id === this.L.self)) {
    this.L.secrets.push(`${b64e(this.L.last)}.${op}.${rootPt && rootPt.length ? b64e(rootPt) : ''}.${b64e(this.h.active_ek_spki)}`);
  }
};

// afterResult: an attached entry (DR5/DR6), or the entry-less DR8 change.
Account.prototype.afterResult = async function (op, actor, f, { result, resultB, dev, x, d }) {
  const L = this.L;
  L.indexAhead = false; // a verified result moves the index (§2.12)
  if (dev) {
    const inp = { op, actor: dev.act ? dev.act.id : '', new: dev.nd ? entryOf(dev.nd) : null, removed: dev.removed,
      bind: op === 'recover' ? x.bindB : E0, bindWait: '', priorSig: false, rootForm: false,
      head: { state: f.state, seq: f.seq, entry: f.entry, root: f.root ? await sha384(f.root.pt) : E0, fu: false },
      outcome: result.state === 'pending' ? 'P' : result.state === 'disputed' ? 'D' : '', anb: result.activation_not_before_ms };
    const { S, refusal } = await jsApply(L.S, inp, result.issued_at_ms, d.v.delta_ms);
    if (refusal) throw new Error(`generator bug: attached ${op} refused ${refusal}`);
    L.S = S;
    if (op === 'recover') L.slot = null;
    if (op === 'rotate_root') L.slot = null;
    if (dev.nd && result.state !== 'disputed') {
      L.devs.set(dev.nd.id, dev.nd);
      L.admitted.set(dev.nd.id, '');
      if (op === 'enroll_new' && !L.self) L.self = dev.nd.id;
    }
    await this.pushEntry({ op, di: dev.di, D: dev.D, sigActor: dev.sigActor, sigNew: dev.sigNew, cpAuthz: E0, cpAuthzSig: E0,
      chainSeq: result.seq, chainEntry: await sha384(resultB), issued: result.issued_at_ms, d, requestId: result.request_id });
    if (op === 'rotate_root' || op === 'rotate_ek') this.recordSecrets(op, op === 'rotate_root' ? result.active_root : E0);
    return;
  }
  if (op === 'cancel_pending' || (op === 'revoke_root' && isPD(f))) {
    L.S = jsNormalize(L.S, { clearPending: true, emptySlot: actor === 'restore' });
    if (actor === 'restore') L.slot = null;
  }
};

// privateOp runs on the fork transact handed the private op (see the wrappers
// after cancelRecoveryBind), so its clock and settle never leak from a throw.
async function privateOp(a, o) {
  a.clock = Math.max(a.clock + 60_000, o.at ?? 0);
  const issued = o.at ?? a.clock;
  const raw = a.s.state === 'pending';
  await a.settle(issued, { noActivate: o.noActivate, noComplete: o.noComplete });
  // The model moved its head index for a derived activation that no verified
  // result has yet recorded; a verifier's index does not move (§2.12, A8).
  if (raw && a.s.state !== 'pending') a.L.indexAhead = true;
  return { issued, asserted: o.asserted ?? issued - 1_000, d: o.d ?? a.r.cur };
}

async function authzOver(a, requestId, digest, asserted, d) {
  const cpAuthz = encode('cp-authz', { realm_id: a.r.id, account_id: a.id, request_id: requestId, intent_digest: digest,
    actor: 'user', asserted_at_ms: asserted, evidence_digest: rnd(48) });
  return { cpAuthz, cpAuthzSig: await wSign(d.cak, cpAuthz) };
}

function privateIntent(a, op, requestId, asserted, actorId, extra) {
  return { realm_id: a.r.id, account_id: a.id, request_id: requestId, op, salt: rnd(32), expires_at_ms: asserted + 600_000,
    expected_seq: a.s.seq, expected_head_digest: a.s.entry, expected_dseq: a.L.dseq, expected_device_head: a.L.last,
    actor_device_id: actorId, new_device_id: '', new_device_dsk: E0, new_device_dkx: E0, removed_device_ids: [],
    recovery_bind_digest: E0, bind_wait: '', ...extra };
}

// commitPrivate applies jsApply (o.force keeps an RSK-forged entry the rules
// refuse, for own-device negative vectors) and signs the entry.
async function commitPrivate(a, o, inp, issued, d, entry) {
  const { S, refusal } = await jsApply(a.L.S, inp, issued, d.v.delta_ms);
  if (refusal && !o.force) throw new Error(`generator bug: ${inp.op} refused ${refusal}`);
  if (S) a.L.S = S;
  if (o.forceSet) a.L.S = o.forceSet(a.L.S);
  return a.pushEntry({ ...entry, chainSeq: a.s.seq, chainEntry: a.s.entry, issued, d, rsk: o.rsk, mutDr: o.mutDr, mutDb: o.mutDb });
}

Account.prototype.addDevice = async function (o = {}) {
  const { issued, asserted, d } = await privateOp(this, o);
  const act = o.actorDev ?? actingDevice(this.L);
  const nd = o.newDev ?? (await newDevice());
  const di = privateIntent(this, 'add_device', crypto.randomUUID(), asserted, act.id,
    { new_device_id: nd.id, new_device_dsk: nd.dsk.pt, new_device_dkx: nd.dkx.pt });
  o.mutDi?.(di);
  const diB = encode('device-intent', di);
  const D = await sha384(diB);
  const auth = await authzOver(this, di.request_id, o.authzDigest ?? D, asserted, d);
  const inp = { op: 'add_device', actor: act.id, new: entryOf(nd), removed: [], head: await this.headInput() };
  const bytes = await commitPrivate(this, o, inp, issued, d, { op: 'add_device', di: diB, D, requestId: di.request_id, ...auth,
    sigRoot: await signAs(o.rootKey ?? this.s.root, 'current-root', D), sigActor: await signAs(o.actorKey ?? act.dsk, 'actor-device', D),
    sigNew: await signAs(nd.dsk, 'new-device', D) });
  this.L.devs.set(nd.id, nd);
  this.L.admitted.set(nd.id, act.id);
  if (this.L.self === nd.id) this.recordSecrets('add_device', this.s.root.pt);
  return { bytes, nd };
};

// bindRecovery: o.unbind, o.prior (sig_recovery_prior by the bound key),
// o.wait ('now' | 'delta' | 'replace' | ''), o.key (the key being bound).
Account.prototype.bindRecovery = async function (o = {}) {
  const { issued, asserted, d } = await privateOp(this, o);
  const L = this.L;
  const act = o.actorDev ?? actingDevice(L);
  const key = o.unbind ? null : (o.key ?? (await wKey()));
  const escrow = o.unbind ? null : await wKey();
  const B = { realm_id: this.r.id, account_id: this.id, request_id: crypto.randomUUID(), kind: o.unbind ? 'unbind' : 'bind',
    expires_at_ms: 0, recovery_key: key ? key.pt : E0, target_digest: E0, chain_seq: this.s.seq,
    chain_entry_digest: this.s.entry, binding_root_digest: await sha384(this.s.root.pt), escrow_public_key: escrow ? escrow.pt : E0 };
  o.mutB?.(B);
  const BB = encode('recovery-bind', B);
  const H = await sha384(BB);
  const wait = o.wait ?? (o.prior ? '' : 'now');
  const di = privateIntent(this, 'bind_recovery', B.request_id, asserted, act.id, { recovery_bind_digest: H, bind_wait: wait });
  o.mutDi?.(di);
  const diB = encode('device-intent', di);
  const D = await sha384(diB);
  const auth = await authzOver(this, di.request_id, D, asserted, d);
  const sigRoot = await signAs(o.rootKey ?? this.s.root, 'current-root', H);
  const inp = { op: 'bind_recovery', actor: act.id, bind: BB, bindWait: wait, priorSig: !!o.prior, head: await this.headInput() };
  const seg = this.s.seg;
  const bytes = await commitPrivate(this, o, inp, issued, d, { op: 'bind_recovery', di: diB, D, requestId: di.request_id, ...auth,
    bind: BB, sigRoot, sigActor: await signAs(act.dsk, 'actor-device', D),
    sigRecovery: key ? await signAs(o.recoverySigner ?? key, 'recovery-key', H) : E0,
    sigPrior: o.prior ? await signAs(o.priorKey ?? L.bound.key, 'recovery-key', H) : E0 });
  const rec = { key, BB, sigRoot, seg, root: o.rootKey ?? this.s.root };
  if (equalBytes(L.S.dig, H)) L.bound = rec;
  else if (equalBytes(L.S.rdig, H)) {
    L.slot = rec;
    L.slotG = d.v.g_ms;
    L.slotExt = 0;
  }
  if (!L.S.key.length) L.bound = null;
  if (!L.S.rdig.length) L.slot = null;
  return { bytes, BB, H, key, sigRoot };
};

// cancelRecoveryBind: the sheet form (default; the bound key signs H(B)) or
// the root form (o.root; a mature actor and the current root).
Account.prototype.cancelRecoveryBind = async function (o = {}) {
  const { issued, asserted, d } = await privateOp(this, o);
  const L = this.L;
  const signer = o.signer ?? (o.root ? null : L.bound?.key);
  if (!o.root && !signer) {
    throw new Error('generator bug: a sheet cancel_recovery_bind needs a bound key to sign H(B): ' +
      'bind one first, pass o.signer, or use the root form (o.root)');
  }
  const B = { realm_id: this.r.id, account_id: this.id, request_id: crypto.randomUUID(), kind: 'cancel',
    expires_at_ms: asserted + 600_000, recovery_key: o.keyPt ?? (o.root ? L.S.rkey : L.S.key), target_digest: o.target ?? E0,
    chain_seq: 0, chain_entry_digest: E0, binding_root_digest: E0, escrow_public_key: E0 };
  o.mutB?.(B);
  const BB = encode('recovery-bind', B);
  const H = await sha384(BB);
  let entry;
  let inp;
  if (o.root) {
    const act = o.actorDev ?? actingDevice(L);
    const di = privateIntent(this, 'cancel_recovery_bind', B.request_id, asserted, act.id,
      { expires_at_ms: B.expires_at_ms, recovery_bind_digest: H });
    o.mutDi?.(di);
    const diB = encode('device-intent', di);
    const D = await sha384(diB);
    entry = { op: 'cancel_recovery_bind', di: diB, D, requestId: B.request_id, ...(await authzOver(this, B.request_id, D, asserted, d)),
      bind: BB, sigRoot: await signAs(this.s.root, 'current-root', H), sigActor: await signAs(act.dsk, 'actor-device', D) };
    inp = { op: 'cancel_recovery_bind', actor: act.id, bind: BB, rootForm: true, head: await this.headInput() };
  } else {
    entry = { op: 'cancel_recovery_bind', di: E0, D: H, requestId: B.request_id,
      ...(await authzOver(this, B.request_id, H, asserted, d)), bind: BB, sigRecovery: await signAs(signer, 'recovery-key', H) };
    inp = { op: 'cancel_recovery_bind', actor: '', bind: BB, rootForm: false, head: await this.headInput() };
  }
  const bytes = await commitPrivate(this, o, inp, issued, d, entry);
  if (!L.S.rdig.length) L.slot = null;
  return { bytes, BB, H };
};

// The private ops run like op(): on a fork that is adopted only when the op
// returns (and o.dry is not set), so a refused or throwing private op leaves
// the live model unchanged.
for (const name of ['addDevice', 'bindRecovery', 'cancelRecoveryBind']) {
  const run = Account.prototype[name];
  Account.prototype[name] = function (o = {}) {
    return transact(this, o, (w) => run.call(w, o));
  };
}

// deviceStatus co-signs a device-status with a status (D-241): the effective
// set at as_of — completion if due and not held (or o.slot), then a new-root
// activation if the status shows it, the earlier first. o.visible is the
// activation instant the status was signed with (status()'s o.visible); pass
// the same value to both, or the C4-4 order is judged at anb.
Account.prototype.deviceStatus = async function (statusB, o = {}) {
  const st = await decode('status', (await decode('signed', statusB)).msg);
  const v = { realm_id: this.r.id, account_id: this.id, chain_seq: st.seq, chain_head_digest: st.head_entry_digest,
    dseq: this.L.dseq, device_head_digest: this.L.last,
    device_set_digest: await sha384(encodeS(this.r.id, this.id, this.effectiveSet(st, o).shown)), as_of_ms: st.as_of_ms,
    valid_until_ms: st.valid_until_ms, delegation_gen: st.delegation_gen, delegation_serial: st.delegation_serial };
  o.mut?.(v);
  const msg = encode('device-status', v);
  const d = o.d ?? this.r.dels.find((x) => x.v.product_root_gen === st.delegation_gen && x.v.serial === st.delegation_serial);
  if (!d && !o.key) {
    throw new Error(`generator bug: deviceStatus for a status under delegation ${st.delegation_gen}:${st.delegation_serial}, ` +
      'which the realm never recorded (a detached delegation): pass o.d or o.key');
  }
  return encode('signed', { type: 'device-status', msg, sig_a: await wSign(o.key ?? d.rsk, msg), sig_b: E0 });
};

// effectiveSet returns the set a status shows and the set the own device
// persists for it (no derived activation of the head, §2.12). The slot
// completes at nb + g + L.slotExt, as settle has it. Only a new-root activation
// moves the set or burns the slot (installsRoot); a status showing a Pe head
// erased shows the set as it is.
Account.prototype.effectiveSet = function (st, o = {}) {
  const S = this.L.S;
  const completeAt = S.rdig.length ? S.nb + this.L.slotG + this.L.slotExt : Infinity;
  const due = o.slot ? o.slot === 'completed' : !st.held && st.as_of_ms >= completeAt;
  const act = installsRoot(this.s) && st.state !== 'pending';
  const visible = o.visible ?? this.s.anb;
  const completes = due && !(act && visible < completeAt);
  const persisted = completes ? jsNormalize(S, { complete: true }) : S;
  const shown = act ? jsNormalize(persisted, { activate: true, burn: this.s.op === 'recover' }) : persisted;
  return { persisted, shown };
};

Account.prototype.genesisDeviceHead = async function (self = this.L.self) {
  return encode('device-head', { realm_id: this.r.id, account_id: this.id, self_device_id: self, last_device_result: E0,
    device_set: E0, chain_head_digest: await sha384(this.heads[0]), admitted_by: [], secrets: [] });
};

// statusR4Of reads the positions R4's retention rule compares from a signed
// status this file built. Every such status passed encode, so a JSON read is
// exact (decode is async, and expectedChainHead is not).
function statusR4Of(signedB) {
  const at = (tag, arr, name) => arr[3 + LAYOUTS[tag].findIndex(([n]) => n === name)];
  const sg = JSON.parse(Buffer.from(signedB).toString('latin1'));
  const st = JSON.parse(Buffer.from(b64d(at('signed', sg, 'msg'))).toString('latin1'));
  const f = (name) => at('status', st, name);
  return { seq: f('seq'), entry: f('head_entry_digest'), held: f('held'), anb: f('activation_not_before_ms'),
    asOf: f('as_of_ms') };
}

// retainedFix is R4's retention rule (Go keepsFix, chain.go; red-team RT-1,
// RT-1b; DoR:642, :738, :2106): the fix a raw pending head keeps when statusB,
// verified against it, meets the fix prevB it already holds. An activation
// fix gives way only to an earlier activation (a held or pending status, or a
// later activation, never replaces it); an activation replaces a pending fix
// (RT-1c: that fix's H_pend range is lost); of two pending fixes the newer
// stays; a fix naming another head, or none, gives way. R5 makes a status
// pending exactly when held or as_of < its anb.
function retainedFix(prevB, statusB) {
  if (!prevB?.length) return statusB;
  const o = statusR4Of(prevB);
  const s = statusR4Of(statusB);
  if (o.seq !== s.seq || o.entry !== s.entry) return statusB;
  const act = (x) => !(x.held === 1 || x.asOf < x.anb);
  if (act(o)) return act(s) && s.asOf < o.asOf ? statusB : prevB;
  if (act(s)) return statusB;
  return o.asOf >= s.asOf ? prevB : statusB;
}

// expectedChainHead is the head VerifyChain persists: a raw-pending head
// keeps a verified status as its fix under R4's retention rule (retainedFix).
// o.prevFix is the fix the head the call starts from holds (default the
// model head's own, which a result always empties): a call that verifies a
// result drops it, since it named the previous head (§2.12, V3-10).
Account.prototype.expectedChainHead = function (statusB, o = {}) {
  if (this.L.indexAhead) {
    throw new Error('generator bug: a private op activated a pending head that no verified result records (§2.12, A8)');
  }
  const fix = this.s.state === 'pending' ? retainedFix(o.prevFix ?? this.h.fix, statusB) : E0;
  return encode('chain-head', { ...this.h, fix });
};

// o.chain pairs it with a chain-head the caller supplies: after a private op
// took a raw head's derived activation (L.indexAhead), the verifier's chain-head
// is still the raw head's, which a fork taken before that op holds.
Account.prototype.expectedDeviceHead = async function (statusB, o = {}) {
  const st = await decode('status', (await decode('signed', statusB)).msg);
  const S = this.effectiveSet(st, o).persisted;
  const chain = o.chain ?? this.expectedChainHead(statusB);
  const ids = new Set([...S.active.map((e) => e.id), ...(S.pending ? [S.pending.id] : [])]);
  return encode('device-head', { realm_id: this.r.id, account_id: this.id, self_device_id: o.self ?? this.L.self,
    last_device_result: this.L.lastResult, device_set: this.L.dseq ? encodeS(this.r.id, this.id, S) : E0,
    chain_head_digest: await sha384(chain), admitted_by: [...this.L.admitted].filter(([id]) => ids.has(id)).map(([id, by]) => `${id}.${by}`),
    secrets: this.L.secrets });
};

// ── 9. Verification vectors (PR-A2) ───────────────────────────────────────
// The builders of DoR §3.1's verification vectors, over sections 6–8's model.
// Each records what the design of record assigns (a Reason, a refusal or a
// set); Go verifies them byte for byte and vitest checks their codec layer.

// Builders that run the section 6–8 world through each DoR §3.1 scenario and
// record the Reason the DoR assigns; Go's VerifyChain and VerifyBundle verify
// them (vectors_chain_test.go).

const EPS = 120_000;
const P384_N = 0xffffffffffffffffffffffffffffffffffffffffffffffffc7634d81f4372ddf581a0db248b0a77aecec196accc52973n;

// highS is the other valid signature (r, n − s): EN7 allows high-s (D-222).
export function highS(sig) {
  const s = big(sig.subarray(48));
  const out = sig.slice();
  out.set(Buffer.from((P384_N - s).toString(16).padStart(96, '0'), 'hex'), 48);
  return out;
}

async function chainVec(ctx, name, a, reason, o = {}) {
  const from = o.from ?? 0;
  const status = o.status ?? (await a.status(o.st));
  await pushA2(ctx, { name, type: 'chain', anchors: b64e(o.anchors ?? a.r.anchors), trust: (o.items ?? a.r.items).map(b64e),
    hw: o.hw ?? HW0, prev_head: b64e(o.prevHead ?? a.heads[from]), bundles: (o.bundles ?? a.bundles.slice(from)).map(b64e),
    bytes_b64: b64e(status), fix: '', mem: o.mem ?? a.mem(), now_ms: o.now ?? a.clock + 2_000, use: o.use ?? 'new-secret',
    expect: expectOf(reason, o.head ? { head_b64: b64e(a.expectedChainHead(status, { prevFix: o.prevFix })) } : {}) });
}

async function bundleVec(ctx, name, a, reason, bundle, o = {}) {
  await pushA2(ctx, { name, type: 'bundle-verify', anchors: b64e(a.r.anchors), trust: (o.items ?? a.r.items).map(b64e),
    hw: o.hw ?? HW0, prev_head: b64e(o.prevHead ?? a.heads.at(-1)), bytes_b64: b64e(bundle), fix: b64s(o.fix),
    expect: expectOf(reason, o.headOut ? { head_b64: b64e(o.headOut) } : {}) });
}

// step runs op (or, with o.dry, only builds it) and pushes a bundle-verify
// vector against the head before it. VerifyBundle takes no AccountMemory, so
// its genesis hypothesis is A0 with no pin (DoR §2.12, V3-05: "the seq-0 pin
// is AccountMemory.pin only"; a fix never supplies one): a pinned account's
// first result is a firstStep chain vector, never a step.
async function step(ctx, name, a, op, o = {}, reason = 'ok') {
  if (a.s.seq === 0 && a.pin.length) {
    throw new Error(`generator bug: ${name}: a pinned account's seq-0 result verifies only through VerifyChain (firstStep)`);
  }
  const before = a.heads.at(-1);
  const bundle = await a.op(op, o);
  await bundleVec(ctx, name, a, reason, bundle, { prevHead: before, items: o.items,
    headOut: o.headOut && !o.dry ? a.heads.at(-1) : undefined });
  return bundle;
}

// firstStep is step for a pinned account at genesis: a chain vector from
// genesis under AccountMemory's first-sight pin (the account's own pin,
// a.mem()). Its Reason is VerifyChain's: for a committed op, the status
// verdict of the head the op made (§2.13), with that head checked byte for
// byte; for a dry op, the bundle's own Reason, which VerifyChain reports
// before it reads the status's state.
async function firstStep(ctx, name, a, op, o = {}, reason = 'ok') {
  if (a.s.seq !== 0 || !a.pin.length) throw new Error(`generator bug: ${name}: firstStep is a pinned account's seq-0 step`);
  const bundle = await a.op(op, o);
  await chainVec(ctx, name, a, reason, { bundles: [bundle], head: !o.dry });
  return bundle;
}

async function enrolled(o = {}) {
  const r = o.realm ?? (await Realm.create(o.r ?? {}));
  const a = await Account.create(r, o.a ?? {});
  await a.op('enroll_new');
  return a;
}

// DoR §3.1 "Trust", "Dual control" (verification halves) and "Negative,
// delegation bounds" (T3: the delegation is ignored, so a status under it is
// delegation_unknown; T4: halts in both arrival orders).
async function trustVectors(ctx) {
  const halts = async (name, mk) => {
    const a = await enrolled();
    const [x, y] = await mk(a.r);
    for (const [order, items] of [['ab', [x, y]], ['ba', [y, x]]]) {
      await chainVec(ctx, `trust/t4/${name}/${order}`, a, 'trust_halt', { from: 1, items: [...a.r.items, ...items] });
    }
  };
  await halts('succession-pair', async (r) => [await r.succeed({ detached: true }), await r.succeed({ detached: true })]);
  await halts('delegation-pair', async (r) => [(await r.delegate({ serial: 2, detached: true })).signed,
    (await r.delegate({ serial: 2, tau: 300_000, detached: true })).signed]);
  await halts('cutover-lowered', async (r) => [(await r.delegate({ serial: 2, cutover: T0 + 10, detached: true })).signed,
    (await r.delegate({ serial: 3, cutover: T0 + 5, detached: true })).signed]);

  let a = await enrolled();
  const x2 = await a.r.delegate({ serial: 2, detached: true });
  const y2 = await a.r.delegate({ serial: 2, tau: 300_000, detached: true, signB: await wKey() });
  await chainVec(ctx, 'trust/t4/contradiction-with-one-invalid-signature', a, 'ok',
    { from: 1, items: [...a.r.items, x2.signed, y2.signed], st: { d: x2 } });

  a = await enrolled();
  const r0a = await a.r.delegate({ serial: 2, detached: true });
  const r0b = await a.r.delegate({ serial: 2, tau: 300_000, detached: true });
  await a.r.succeed();
  await a.r.delegate({ gen: 1, serial: 1 });
  await chainVec(ctx, 'trust/t4/retired-generation-contradiction', a, 'ok',
    { from: 1, items: [...a.r.items, r0a.signed, r0b.signed] });
  await chainVec(ctx, 'trust/t4/succession-before-its-delegation', a, 'ok', { from: 1 });
  await chainVec(ctx, 'trust/t7/succession-serial-reset-covered', a, 'ok', { use: 'history' });
  await chainVec(ctx, 'trust/t4/no-delegation-for-current-generation', a, 'trust_halt',
    { from: 1, items: a.r.items.slice(0, 2) });
  // T7: a superseded entry is accepted only when covered by a status that
  // passes T6 and the history rule, now <= valid_until. Under NewSecret,
  // freshness reads tau rather than valid_until, so the cover is the only check
  // that refuses a status past valid_until (now = as_of + 1 s here).
  await chainVec(ctx, 'trust/t7/superseded-covered-at-valid-until', a, 'ok', { st: { v: 1_000 } });
  await chainVec(ctx, 'trust/t7/superseded-past-valid-until', a, 'superseded_uncovered', { st: { v: 999 } });

  // T7 and T8 are existential over the valid delegations retained at one
  // (gen, serial) (T3 retains one below HW; T4 ignores a contradiction inside
  // a retired generation), and R2 reads delta_ms and g_ms from a delegation
  // whose RSK signed the result. Two valid (0, 2) delegations differing in
  // g_ms; gen 0 is retired, so the replace_root under (0, 2) is superseded
  // and the status under (1, 1) covers it.
  const retiredPair = async (rskX, rskY) => {
    const b = await enrolled();
    const x = await b.r.delegate({ serial: 2, rsk: rskX, detached: true });
    const y = await b.r.delegate({ serial: 2, rsk: rskY, g: GRACE + 60_000, detached: true });
    await b.r.succeed();
    await b.r.delegate({ gen: 1, serial: 1 });
    const [first, second] = [x, y].sort((p, q) => Buffer.compare(p.msg, q.msg)); // Go's delegations() order
    return { b, first, second, items: [...b.r.items, x.signed, y.signed] };
  };
  // Both list the RSK; the second in msg-byte order is named, so the first
  // fails R2 and the second admits (a verifier that stops at the first fails).
  const sharedRsk = await wKey();
  let rp = await retiredPair(sharedRsk, sharedRsk);
  await rp.b.op('replace_root', { d: rp.second });
  await chainVec(ctx, 'trust/t7/retired-pair-the-second-signer-admits', rp.b, 'pending_from_active',
    { from: 1, items: rp.items, head: true });
  // Only the first's RSK signed, while the anb follows the second's g_ms: R2
  // holds only under a delegation that did not sign, so none admits it.
  rp = await retiredPair(await wKey(), await wKey());
  await rp.b.op('replace_root', { d: rp.second, rsk: rp.first.rsk });
  await chainVec(ctx, 'trust/t7/retired-pair-g-from-the-signer', rp.b, 'anb_mismatch', { from: 1, items: rp.items });

  a = await enrolled();
  const g = a.r.gens[0];
  const other = await wKey();
  for (const [name, o] of [['a-only', { signB: other }], ['b-only', { signA: other }], ['swapped', { signA: g.B, signB: g.A }],
    ['same-key-twice', { signA: g.A, signB: g.A }]]) {
    const d = await a.r.delegate({ serial: 2, detached: true, ...o });
    await chainVec(ctx, `trust/t3/${name}`, a, 'delegation_unknown', { from: 1, items: [...a.r.items, d.signed], st: { d } });
  }
  for (const [name, so] of [['succession-sig-b-other-key', { signB: other }], ['succession-reuses-a-prior-key', { reuse: true }]]) {
    const b = await enrolled();
    const keys = await Realm.genKeys();
    if (so.reuse) keys.A = b.r.gens[0].A;
    const s = await b.r.succeed({ keys, detached: true, signB: so.signB });
    const d = await b.r.delegate({ gen: 1, serial: 1, keys, detached: true });
    await chainVec(ctx, `trust/t3/${name}`, b, 'delegation_unknown', { from: 1, items: [...b.r.items, s, d.signed], st: { d } });
  }

  a = await enrolled();
  const av = await decode('anchors', a.r.anchors);
  await chainVec(ctx, 'trust/t1/anchors-two-keys-equal', a, 'malformed',
    { from: 1, anchors: encode('anchors', { ...av, recovery_b: av.primary_a }) });

  a = await enrolled();
  const d1 = await decode('signed', a.r.items[0]);
  await chainVec(ctx, 'trust/malleated-signature-pair', a, 'ok',
    { from: 1, items: [a.r.items[0], encode('signed', { ...d1, sig_a: highS(d1.sig_a) })] });

  // g_ms ≤ delta_ms holds in the first case, so only the delta_ms floor refuses it.
  const bounds = [['delta-intent-window-plus-epsilon', { delta: 660_000 + EPS, g: 1_000 }, 'delegation_unknown'],
    ['delta-max-plus-one', { delta: 2_592_000_001 }, 'delegation_unknown'],
    ['lifetime-400-days', { nb: T0 - DAY, na: T0 - DAY + 400 * DAY }, 'ok'],
    ['lifetime-400-days-plus-1-ms', { nb: T0 - DAY, na: T0 - DAY + 400 * DAY + 1 }, 'delegation_unknown']];
  for (const [name, o, reason] of bounds) {
    const b = await enrolled();
    await b.r.delegate({ serial: 2, ...o });
    await chainVec(ctx, `trust/delegation-bounds/${name}`, b, reason, { from: 1 });
  }
  for (const [name, mk] of [['shared-rsk-and-cak', async () => { const k = await wKey(); return { rsk: k, cak: k }; }],
    ['shared-operator-and-tls', async () => { const k = await wKey(); return { ops: [k], tls: k }; }],
    ['tls-point-digest-in-cp-tls-pins', async () => { const k = await wKey(); return { tls: k, pins: [await tlsPin(k.pt)] }; }]]) {
    const b = await enrolled();
    await b.r.delegate({ serial: 2, ...(await mk()) });
    await chainVec(ctx, `trust/key-separation/${name}`, b, 'delegation_unknown', { from: 1 });
  }
  a = await enrolled();
  await a.r.delegate({ serial: 2, rsk: a.r.dels[0].rsk });
  await chainVec(ctx, 'trust/cross-delegation-rsk-reuse-is-not-t3', a, 'ok', { from: 1 });

  // A valid delegation below HW is retained for T7 lookups (D(s) after D(s+1)).
  a = await enrolled();
  await a.r.delegate({ serial: 2 });
  await chainVec(ctx, 'trust/t7/below-hw-delegation-retained', a, 'ok',
    { items: [a.r.items[1], a.r.items[0]], use: 'history' });
  await chainVec(ctx, 'trust/t6/status-under-a-superseded-delegation', a, 'status_superseded',
    { from: 1, st: { d: a.r.dels[0] } });

  // Window edges (§2.10): x = not_before_ms covered, x = not_after_ms not.
  const nb = T0 + DAY;
  const na = T0 + 2 * DAY;
  a = await Account.create(await Realm.create({ del: { nb, na } }));
  await step(ctx, 'trust/window/t7-t8-at-not-before', a, 'enroll_new', { at: nb, asserted: nb });
  await chainVec(ctx, 'trust/window/t6-at-not-before', a, 'ok', { from: 1, st: { asOf: nb }, now: nb + 1_000 });
  await chainVec(ctx, 'trust/window/t6-at-not-after', a, 'delegation_window', { from: 1, st: { asOf: na }, now: na + 1_000 });
  a = await Account.create(await Realm.create({ del: { nb, na } }));
  await step(ctx, 'trust/window/t7-at-not-after', a, 'enroll_new', { at: na, asserted: na - 1_000, dry: true }, 'delegation_window');
  await step(ctx, 'trust/window/t8-at-not-after', a, 'enroll_new', { at: na - 1, asserted: na, dry: true }, 'delegation_window');

  // T8: a standby CAK promoted at D(2); and a CAK of another generation.
  const standby = await wKey();
  const b2 = await Account.create(await Realm.create({ del: { moreCaks: [standby] } }));
  await b2.op('enroll_new');
  await b2.r.delegate({ serial: 2, minValid: 1, cak: standby, moreCaks: [await wKey()] });
  await step(ctx, 'trust/t8/standby-promotion', b2, 'rotate_ek');
  const c = await enrolled();
  await c.r.succeed();
  await c.r.delegate({ gen: 1, serial: 1 });
  await step(ctx, 'trust/t8/same-generation-only', c, 'rotate_ek', { dry: true, cak: c.r.dels[0].cak }, 'cak_unlisted');
}

// DoR §3.1 "Changed pin (LD-28)", D-149, "D-136", P1-e (status half).
async function pinAndStatusVectors(ctx) {
  const r = await Realm.create();
  const a = await Account.create(r, { pin: true });
  const other = b64e(rnd(48));
  const accepted = a.mem({ pin: other, acceptedPin: b64e(a.pin) });
  await chainVec(ctx, 'pin/first-sight', a, 'ok', { mem: a.mem({ pin: '' }) });
  await chainVec(ctx, 'pin/unchanged', a, 'ok');
  await chainVec(ctx, 'pin/changed', a, 'pin_changed', { mem: a.mem({ pin: other }) });
  await chainVec(ctx, 'pin/accepted', a, 'ok_accepted_pin', { mem: accepted });
  await chainVec(ctx, 'pin/stale-acceptance', a, 'pin_changed', { mem: a.mem({ pin: other, acceptedPin: b64e(rnd(48)) }) });
  await chainVec(ctx, 'pin/acceptance-needs-a-fresh-status', a, 'pin_changed',
    { mem: accepted, use: 'history', st: { asOf: a.clock - TAU - 5_000 }, now: a.clock });
  await chainVec(ctx, 'pin/absent-after-enrolled', a, 'absent_after_enrolled', { mem: a.mem({ everEnrolled: 1 }) });
  const cut = await Account.create(await Realm.create({ del: { cutover: T0 } }), { pin: true });
  await chainVec(ctx, 'pin/after-cutover', cut, 'absent_after_cutover', { mem: accepted });
  const opKey = await wKey();
  const rev = await Account.create(await Realm.create({ del: { ops: [opKey] } }), { pin: true });
  await rev.op('revoke_root', { actor: 'operator' });
  // From the verified Apr head: a chain from genesis under the accepted
  // memory's first-sight pin (`other`) would refuse the revoke itself (V3-05).
  await chainVec(ctx, 'pin/revoked', rev, 'pin_revoked', { from: 1, mem: accepted });
  await chainVec(ctx, 'pin/absent-unpinned', await Account.create(r), 'absent_unpinned');
  await chainVec(ctx, 'status/future', a, 'status_future', { now: a.clock + 1_000 - EPS - 1 });
  await chainVec(ctx, 'status/stale-tau', a, 'status_stale_tau', { now: a.clock + TAU + 5_000 });
  await chainVec(ctx, 'status/expired-for-history', a, 'status_expired', { use: 'history', now: a.clock + VMS + 5_000 });
  await chainVec(ctx, 'status/valid-until-above-v-max', a, 'malformed',
    { st: { mut: (v) => { v.valid_until_ms = v.as_of_ms + 259_200_001; } } });

  const e = await enrolled();
  const oldEk = e.s.ek;
  await e.op('rotate_ek');
  await chainVec(ctx, 'd136/ek-rollback-status', e, 'field_mismatch', { from: 2, st: { mut: (v) => { v.ek_binding_digest = oldEk; } } });
  await chainVec(ctx, 'status/chain-behind', e, 'chain_behind', { from: 0, bundles: e.bundles.slice(0, 1) });
  await chainVec(ctx, 'status/fork-lower-seq', e, 'fork', { from: 2, status: await e.status({ mut: (v) => { v.seq = 1; } }) });
  const p = await enrolled();
  await p.op('replace_root');
  await chainVec(ctx, 'd136/status-anb-below-result-anb', p, 'anb_mismatch',
    { from: 2, st: { visible: p.s.anb - 1 } });
  await chainVec(ctx, 'd136/held-pending-anb-never', p, 'held', { from: 2, st: { held: 1 } });
  await chainVec(ctx, 'status/pending-from-active', p, 'pending_from_active', { from: 2 });
  const n = await enrolled();
  await n.op('rotate_ek');
  await n.r.delegate({ serial: 2 });
  await chainVec(ctx, 'd136/status-under-a-newer-delegation-serial', n, 'ok', { from: 2 });
  await chainVec(ctx, 'p1e/device-status-passed-as-status', n, 'malformed', { from: 2, status: await n.deviceStatus(await n.status()) });
}

// The seq-0 pin (DoR §2.12 "the seq-0 pin is AccountMemory.pin only", V3-05;
// §2.13 first-sight pin, D-149; R5 "at seq 0 … the pin is set or \"\"").
// VerifyChain takes AccountMemory's first-sight pin, or, with none, the pin of
// the status it sees for the first time. VerifyBundle takes no AccountMemory,
// so its genesis is A0 with no pin, and its fix keeps only its R4 meaning: a
// verified genesis status passed as the fix supplies no pin. From there a
// result cannot change the pin, "" included, so an unpinned account's first
// result cannot introduce one.
async function seqZeroPinVectors(ctx) {
  const realm = await Realm.create();
  let a = await Account.create(realm, { pin: true });
  const genesisStatus = await a.status();
  await a.op('enroll_existing', { pop: true });
  await chainVec(ctx, 'pin/seq0/chain-under-the-first-sight-pin', a, 'ok');
  // The same bundle through VerifyBundle, with the verified genesis status as
  // its fix: A0 admits no enroll_existing (§2.9 rows 2-3 start from Ap), so a
  // status pin this caller's first sight never judged cannot make a pending
  // head verify as continuity (V3-05).
  await bundleVec(ctx, 'pin/seq0/bundle-fix-supplies-no-pin', a, 'transition_invalid', a.bundles[0],
    { prevHead: a.heads[0], fix: genesisStatus });
  await chainVec(ctx, 'pin/seq0/chain-first-contact-takes-the-status-pin', a, 'ok', { mem: a.mem({ pin: '' }) });
  await chainVec(ctx, 'pin/seq0/chain-under-another-first-sight-pin', a, 'transition_invalid',
    { mem: a.mem({ pin: b64e(rnd(48)) }) });

  a = await Account.create(realm, { pin: true });
  const genesis = a.heads[0];
  const b = await a.op('enroll_existing', { pop: true, dry: true });
  await bundleVec(ctx, 'pin/seq0/bundle-without-a-fix-knows-no-pin', a, 'transition_invalid', b, { prevHead: genesis });
  await bundleVec(ctx, 'pin/seq0/bundle-fix-forged', a, 'bad_signature', b,
    { prevHead: genesis, fix: await a.status({ key: await wKey() }) });
  const stranger = await Account.create(realm, { pin: true });
  await bundleVec(ctx, 'pin/seq0/bundle-fix-for-another-account', a, 'realm_mismatch', b,
    { prevHead: genesis, fix: await stranger.status() });

  const u = await Account.create(realm);
  const injected = await u.op('erase', { dry: true, mutResult: (r) => { r.legacy_ek_digest = rnd(48); } });
  await bundleVec(ctx, 'pin/seq0/injected-pin-on-erase-from-a0', u, 'transition_invalid', injected, { prevHead: u.heads[0] });
  await chainVec(ctx, 'pin/seq0/chain-injected-pin-on-erase-from-a0', u, 'transition_invalid', { bundles: [injected] });
}

// DoR §3.1 "Transitions" (rows 1–19; rows 20–22 are Tasks 17–18), the
// force_unverified client half, "Operator key", "D-142", "D-182", "Sets",
// "Device signatures" (published half) and the PoP digest rule.
async function transitionVectors(ctx) {
  const opKey = await wKey();
  const realm = await Realm.create({ del: { ops: [opKey] } });
  const fresh = (o) => Account.create(realm, o);
  let a = await fresh();
  await step(ctx, 'row/01/enroll-new', a, 'enroll_new');
  // A pinned account's first result verifies only through VerifyChain (V3-05),
  // so rows 2, 3 and 17 from Ap are chain vectors whose Reason is the status
  // verdict of the head they make (§2.13).
  a = await fresh({ pin: true });
  await firstStep(ctx, 'row/02/pop-pending', a, 'enroll_existing', { pop: true }); // continuity pending from absent
  a = await fresh({ pin: true });
  await firstStep(ctx, 'row/02/pop-disputed', a, 'enroll_existing', { pop: true, dispute: true }, 'disputed');
  a = await fresh({ pin: true });
  await firstStep(ctx, 'row/03/no-pop-recovery', a, 'enroll_existing', {}, 'pending_from_absent');
  await step(ctx, 'row/04/competitor', a, 'enroll_existing');
  a = await fresh({ pin: true });
  await a.op('enroll_existing');
  await step(ctx, 'row/04/competitor-with-pop', a, 'enroll_existing', { pop: true });
  a = await fresh({ pin: true });
  await a.op('revoke_root', { actor: 'operator' });
  await step(ctx, 'row/03/from-apr-unverified', a, 'enroll_existing');
  a = await fresh({ pin: true });
  await firstStep(ctx, 'fu/row-03-forced-unverified', a, 'enroll_existing', { assurance: 'unverified' }, 'pending_from_absent');

  a = await enrolled({ realm });
  await step(ctx, 'row/05/rotate-root', a, 'rotate_root');
  await step(ctx, 'row/06/rotate-ek', a, 'rotate_ek');
  await step(ctx, 'row/07/suspend-user', a, 'suspend');
  await step(ctx, 'row/09/unsuspend-user', a, 'unsuspend');
  await step(ctx, 'row/08/suspend-operator', a, 'suspend', { actor: 'operator' });
  await step(ctx, 'row/10/unsuspend-operator', a, 'unsuspend', { actor: 'operator' });
  await step(ctx, 'row/11/replace-pending', a, 'replace_root');
  await step(ctx, 'row/12/competitor', a, 'replace_root');
  await step(ctx, 'row/13/cancel-by-root', a, 'cancel_pending');
  await step(ctx, 'row/11/replace-disputed', a, 'replace_root', { dispute: true });
  await step(ctx, 'row/16/revoke-from-disputed', a, 'revoke_root', { actor: 'operator' });
  a = await enrolled({ realm });
  await step(ctx, 'fu/row-11-forced-unverified', a, 'replace_root', { assurance: 'unverified', dry: true });
  await step(ctx, 'row/15/revoke-from-active', a, 'revoke_root', { actor: 'operator' });
  await step(ctx, 'row/11/replace-from-sor-unverified', a, 'replace_root');
  a = await fresh({ pin: true });
  await firstStep(ctx, 'row/17/revoke-from-ap', a, 'revoke_root', { actor: 'operator' }, 'pin_revoked');
  a = await fresh({ pin: true });
  await a.op('enroll_existing');
  await step(ctx, 'row/14/cancel-by-pop', a, 'cancel_pending');
  a = await enrolled({ realm });
  await step(ctx, 'row/18/erase-user', a, 'erase');
  await step(ctx, 'row/13/cancel-pending-erase', a, 'cancel_pending');
  await step(ctx, 'row/19/erase-operator', a, 'erase', { actor: 'operator' });

  // Sets: suspend(operator) then unsuspend(operator) from Su ends in Su.
  a = await enrolled({ realm });
  await a.op('suspend');
  await a.op('suspend', { actor: 'operator' });
  await a.op('unsuspend', { actor: 'operator' });
  await chainVec(ctx, 'sets/operator-suspend-unsuspend-from-su', a, 'suspended');
  await step(ctx, 'neg/transition/row-07-user-already-in-set', a, 'suspend', { dry: true, inadmissible: true },
    'transition_invalid');
  await a.op('replace_root');
  await step(ctx, 'neg/transition/row-12-set-changed', a, 'replace_root',
    { dry: true, mutResult: (r) => { r.suspended_by = []; } }, 'transition_invalid');
  a = await enrolled({ realm });
  await a.op('suspend', { actor: 'operator' });
  await step(ctx, 'neg/transition/row-11-from-so', a, 'replace_root', { dry: true, inadmissible: true }, 'transition_invalid');

  // Operator key (LD-4, T12): missing, unlisted, wrong delegation, GA form.
  const n = await enrolled({ realm });
  await step(ctx, 'operator/missing-signature', n, 'suspend',
    { actor: 'operator', dry: true, mutBundle: (b) => { b.sig_operator = E0; } }, 'transition_invalid');
  await step(ctx, 'operator/unlisted-key', n, 'suspend', { actor: 'operator', dry: true, opKey: await wKey() }, 'bad_signature');
  const k1 = await wKey();
  const wd = await enrolled({ r: { del: { ops: [k1] } } });
  await wd.r.delegate({ serial: 2, minValid: 1, ops: [await wKey()] });
  await step(ctx, 'operator/wrong-delegation', wd, 'suspend', { actor: 'operator', dry: true, opKey: k1 }, 'bad_signature');
  const ga = await enrolled();
  await step(ctx, 'operator/ga-form-no-operator-keys', ga, 'suspend', { actor: 'operator', dry: true, opKey: k1 }, 'bad_signature');

  // Negative transitions on one enrolled account (dry runs).
  const neg = (name, op, o, reason) => step(ctx, `neg/transition/${name}`, n, op, { ...o, dry: true }, reason);
  await neg('missing-current-root-signature', 'rotate_ek', { mutBundle: (b) => { b.sig_current_root = E0; } }, 'transition_invalid');
  await neg('bad-current-root-signature', 'rotate_ek', { mutBundle: (b) => { b.sig_current_root = rnd(96); } }, 'bad_signature');
  await neg('ek-generation-not-head-plus-one', 'rotate_ek', { mutEkb: (e) => { e.ek_generation += 1; } }, 'transition_invalid');
  // An ek-binding names the chain's realm and account (realm_mismatch, DoR §2.12)
  // and root_digest = its signer (§2.5 ek-binding Rule, in the class of
  // ek_generation = head + 1); each is otherwise self-consistent and signed.
  await neg('ek-binding-another-realm', 'rotate_ek',
    { mutEkb: (e) => { e.realm_id = '11111111-1111-4111-8111-999999999999'; } }, 'realm_mismatch');
  await neg('ek-binding-another-account', 'rotate_ek',
    { mutEkb: (e) => { e.account_id = '22222222-2222-4222-8222-222222222222'; } }, 'realm_mismatch');
  await neg('ek-binding-root-digest-not-the-signer', 'rotate_ek', { mutEkb: (e) => { e.root_digest = rnd(48); } },
    'transition_invalid');
  await neg('predecessor-root-digest-mismatch', 'rotate_ek', { mutIntent: (i) => { i.predecessor_root_digest = rnd(48); } }, 'transition_invalid');
  await neg('expected-head-digest-mismatch', 'rotate_ek', { mutIntent: (i) => { i.expected_head_digest = rnd(48); } }, 'transition_invalid');
  await neg('candidate-root-on-rotate-ek', 'rotate_ek', { mutIntent: (i) => { i.candidate_root = n.s.root.pt; } }, 'transition_invalid');
  await neg('post-state-not-in-the-row', 'rotate_ek', { mutResult: (r) => { r.state = 'suspended'; r.suspended_by = ['user']; } }, 'transition_invalid');
  await neg('recovery-gen', 'rotate_ek', { mutResult: (r) => { r.recovery_gen += 1; } }, 'transition_invalid');
  await neg('seq-skips', 'rotate_ek', { mutResult: (r) => { r.seq += 1; } }, 'chain_gap');
  await neg('prev-entry-digest', 'rotate_ek', { mutResult: (r) => { r.prev_entry_digest = rnd(48); } }, 'head_mismatch');
  await neg('result-signature', 'rotate_ek', { rsk: await wKey() }, 'bad_signature');
  await neg('cak-unlisted', 'rotate_ek', { cak: await wKey() }, 'cak_unlisted');
  await neg('result-account-mismatch', 'rotate_ek', { mutResult: (r) => { r.account_id = crypto.randomUUID(); } }, 'realm_mismatch');
  await neg('issued-below-asserted-minus-epsilon', 'rotate_ek', { at: n.clock + 60_000, asserted: n.clock + 60_000 + EPS + 1 }, 'issued_bounds');
  await neg('issued-decreasing', 'rotate_ek', { at: n.s.issued - 1, asserted: n.s.issued - 1 }, 'issued_bounds');
  await neg('anb-off-by-one', 'replace_root', { mutResult: (r) => { r.activation_not_before_ms += 1; } }, 'anb_mismatch');
  const scoped = await enrolled({ r: { del: { scope: SCOPE.filter((s) => s !== 'rotate_ek') } } });
  await step(ctx, 'neg/transition/op-out-of-scope', scoped, 'rotate_ek', { dry: true }, 'out_of_scope');
  const far = await Account.create(await Realm.create({ del: { nb: MAX_SAFE - 10 * DAY, na: MAX_SAFE - 1 } }));
  far.clock = MAX_SAFE - 259_200_000 - 200_000;
  await far.op('enroll_new');
  await step(ctx, 'neg/delegation-bounds/r2-sum-above-2-53', far, 'replace_root',
    { dry: true, mutResult: (r) => { r.activation_not_before_ms = MAX_SAFE; } }, 'anb_mismatch'); // no encodable anb equals the overflowed sum

  // D-142 and D-182 on an unverified segment.
  const u = await enrolled({ realm });
  await u.op('replace_root', { assurance: 'unverified' });
  await u.op('suspend', { at: u.s.anb + 1 });
  await u.op('unsuspend');
  await step(ctx, 'neg/d142/rotate-claims-continuity', u, 'rotate_root', { dry: true, assurance: 'continuity' }, 'transition_invalid');
  await step(ctx, 'd142/rotate-from-unverified-stays-unverified', u, 'rotate_root', { dry: true });
  await step(ctx, 'neg/d182/replace-claims-recovery-from-unverified', u, 'replace_root', { dry: true, assurance: 'recovery' }, 'transition_invalid');
  await step(ctx, 'd182/replace-from-unverified-stays-unverified', u, 'replace_root', { dry: true });

  // A-B1: a row that installs its candidate root refuses a root root_index
  // already records (transition_invalid; the authority's root_reused, DoR:683).
  // Row 11 from Ac(B) names the enrolment's root A: accepted, A's activation
  // would keep A's old initial record (addRoot) in place of the recovery its
  // own row allows, so a later rotate_root would derive continuity from it.
  const ro = await enrolled({ realm });
  const oldRoot = ro.s.root;
  await ro.op('rotate_root');
  await step(ctx, 'neg/transition/row-11-reinstalls-an-old-root', ro, 'replace_root',
    { dry: true, cand: oldRoot, reusedRoot: true }, 'transition_invalid');

  // Device signatures (published half) and the PoP digest (D-243, E2-5).
  let D;
  await neg('device-intent-root-signature-as-current-root', 'rotate_ek', { mutIntent: (i) => { D = i.device_intent_digest; },
    mutBundle: async (b) => { b.sig_current_root = await signAs(n.s.root, 'current-root', D); } }, 'bad_signature');
  await neg('ek-spki-rebound-in-chain', 'rotate_ek', { spki: n.h.active_ek_spki }, 'transition_invalid');
  const pinned = await fresh({ pin: true });
  await firstStep(ctx, 'neg/transition/ek-spki-is-the-legacy-pin', pinned, 'enroll_existing', { dry: true, spki: (await legacy()).spki }, 'transition_invalid');
  await firstStep(ctx, 'neg/transition/pop-legacy-spki-digest', pinned, 'enroll_existing',
    { dry: true, pop: true, mutBundle: (b) => { b.legacy_spki = fakeEk(); } }, 'transition_invalid');
}

BUILDERS.push(trustVectors, pinAndStatusVectors, seqZeroPinVectors, transitionVectors);

// DoR §3.1 "Replay" (R1, R3, R4, D-137) and "Persisted heads" (D-269, E3-06).
async function replayAndHeadVectors(ctx) {
  const realm = await Realm.create({ del: { ops: [await wKey()] } });
  const pending = async (setup) => {
    const a = setup ? await Account.create(realm, { pin: true }) : await enrolled({ realm });
    await a.op(setup ?? 'replace_root');
    return a;
  };
  let a = await pending();
  await step(ctx, 'r3/before-anb-only-h-pend', a.fork(), 'replace_root', { at: a.s.anb - 1, noActivate: true, headOut: true });
  // Before anb H_act is not formed. The rotate_ek H_act admits at anb + 1
  // (signed by the candidate), re-stamped to issue 1 ms before anb, has the
  // single hypothesis H_pend, from which no row admits rotate_ek, so it gets
  // that check's own Reason (decision A3). Forming H_act early accepts it.
  a = await pending();
  const early = a.s.anb - 1;
  await step(ctx, 'r3/before-anb-h-act-not-formed', a.fork(), 'rotate_ek', { at: a.s.anb + 1, dry: true,
    mutResult: (r) => { r.issued_at_ms = early; } }, 'transition_invalid');
  // At or after anb both hypotheses are formed; the post-state makes exactly one match (D-137).
  for (const [n, op, o] of [[12, 'replace_root', {}], [16, 'revoke_root', { actor: 'operator' }], [18, 'erase', {}],
    [19, 'erase', { actor: 'operator' }]]) {
    a = await pending();
    await step(ctx, `r3/row-${n}/h-pend-unique`, a.fork(), op, { ...o, at: a.s.anb + 1, noActivate: true, headOut: true });
    await step(ctx, `r3/row-${n}/h-act-unique`, a.fork(), op, { ...o, at: a.s.anb + 1, headOut: true });
  }
  a = await pending('enroll_existing');
  await step(ctx, 'r3/p-absent/revoke/h-pend-unique', a.fork(), 'revoke_root', { actor: 'operator', at: a.s.anb + 1, noActivate: true });
  await step(ctx, 'r3/p-absent/revoke/h-act-unique', a.fork(), 'revoke_root', { actor: 'operator', at: a.s.anb + 1 });
  // The RSK relabels the result as a well-formed Su (anb 0 and no pending
  // positions, so R2 and the result's own rules pass): at anb + 1 both
  // hypotheses are formed and neither admits it, which R3 calls
  // hypothesis_conflict ("zero or two matches").
  a = await pending();
  await step(ctx, 'r3/rsk-relabelled-matches-neither', a.fork(), 'replace_root', { at: a.s.anb + 1, dry: true,
    mutResult: (r) => {
      Object.assign(r, { state: 'suspended', prior_state: '', suspended_by: ['user'], pending_root: E0,
        pending_ek_binding_digest: E0, activation_not_before_ms: 0 });
    } }, 'hypothesis_conflict');

  // R4: a retained status that still shows the head held refuses H_act.
  const q = await pending();
  const fix = await q.status({ held: 1, asOf: q.s.anb + 5_000 });
  const before = q.heads.at(-1);
  await bundleVec(ctx, 'r4/fix-refuses-h-act', q, 'hypothesis_conflict',
    await q.fork().op('rotate_ek', { at: q.s.anb + 1_000, dry: true }), { prevHead: before, fix });
  await bundleVec(ctx, 'r4/fix-keeps-h-pend', q, 'ok',
    await q.fork().op('replace_root', { at: q.s.anb + 1_000, noActivate: true, dry: true }), { prevHead: before, fix });

  // R1: issued_at_ms is non-decreasing, so an equal value is admitted.
  const e = await enrolled({ realm });
  await step(ctx, 'r1/issued-equal-to-previous', e, 'rotate_ek', { at: e.s.issued });

  // Persisted heads: the exact chain-head bytes after each move (D-269).
  await chainVec(ctx, 'head/genesis', await Account.create(realm), 'absent_unpinned', { head: true });
  const h = await enrolled({ realm });
  await h.op('rotate_root');
  await h.op('rotate_ek');
  await chainVec(ctx, 'head/after-rotations', h, 'ok', { head: true });
  await h.op('replace_root');
  await chainVec(ctx, 'head/pending', h, 'pending_from_active', { head: true });
  await h.op('rotate_ek', { at: h.s.anb + 1 });
  await chainVec(ctx, 'head/activation-recorded-by-the-next-result', h, 'ok', { head: true });
  const s = await enrolled({ realm });
  await s.op('suspend');
  await s.op('replace_root');
  await s.op('unsuspend', { at: s.s.anb + 1 });
  await chainVec(ctx, 'head/p-suspended-activation-has-no-bind-root', s, 'ok', { head: true });
}

BUILDERS.push(replayAndHeadVectors);

// DoR §3.1 "Recovery key" (published half), with the binding-proof lines of
// "Escrow key", "Unbind" and V2-08: row 20 from every From-state and refused
// from E, row 21, the Pr exclusions of rows 12, 13, 16, 18 and 19, §2.22's
// checks 1–6 and the Δ13 cap (LD-35, D-254). Each negative is built so that
// only the check its name gives refuses it: o.mutB re-signs the edited B
// under the binding root and intent #16 follows it, so the proof's signature
// and check 1's digest still pass.
async function recoveryVectors(ctx) {
  const realm = await Realm.create({ del: { ops: [await wKey()] } });
  const bound = async () => {
    const a = await enrolled({ realm });
    await a.bindRecovery({ wait: 'now' });
    return a;
  };
  const froms = { ac: [], su: [['suspend']], so: [['suspend', { actor: 'operator' }]],
    sor: [['revoke_root', { actor: 'operator' }]], p: [['replace_root']], d: [['replace_root'], ['replace_root']],
    pe: [['erase']], pr: [['recover']] };
  for (const [name, ops] of Object.entries(froms)) {
    const a = await bound();
    for (const [op, o] of ops) await a.op(op, o);
    await step(ctx, `row/20/from-${name}`, a, 'recover', { headOut: true });
  }
  // Row 20 excludes E. A published E (row 19) is one hypothesis, so the From
  // column answers (a Pe head at its anb would also carry H_pend, from which
  // row 20 admits). E has no Pr(x) post-state (x = erased is no prior state),
  // so the result carries the Pr(active) the account held before the erase.
  const e = await bound();
  await e.op('erase', { actor: 'operator' });
  await step(ctx, 'neg/row-20/from-e', e, 'recover',
    { dry: true, inadmissible: true, mutResult: (r) => { r.prior_state = 'active'; } }, 'transition_invalid');

  let a = await bound();
  await a.op('recover');
  await step(ctx, 'row/21/cancel-by-the-pending-recovery-key', a.fork(), 'cancel_pending', { by: 'recovery', headOut: true });
  await step(ctx, 'neg/row-21/other-key', a.fork(), 'cancel_pending', { by: 'recovery', recKey: await wKey(), dry: true }, 'bad_signature');
  for (const [n, op, o] of [[12, 'replace_root', {}], [13, 'cancel_pending', { by: 'root' }],
    [16, 'revoke_root', { actor: 'operator' }], [18, 'erase', {}], [19, 'erase', { actor: 'operator' }]]) {
    await step(ctx, `neg/row-${n}/excludes-pr`, a.fork(), op, { ...o, dry: true, inadmissible: true }, 'transition_invalid');
  }
  a = await bound();
  await a.op('replace_root');
  await a.op('cancel_pending');
  await step(ctx, 'row/20/within-n-cancel-is-pending', a, 'recover', { headOut: true });

  // The proof travels with the key: B was written before two rotations.
  a = await bound();
  await a.op('rotate_root');
  await a.op('rotate_ek');
  await step(ctx, 'row/20/proof-from-an-earlier-chain-position', a, 'recover');

  // Binding-proof negatives (dry: the bound account is unchanged).
  a = await bound();
  const neg = (name, o, reason, acct = a) => step(ctx, `neg/proof/${name}`, acct, 'recover', { ...o, dry: true }, reason);
  const recoverSeq = a.s.seq + 1;
  await neg('1-kind-cancel-is-malformed-v2-04', { mutB: (b) => { b.kind = 'cancel'; } }, 'malformed');
  // Check 1's kind clause. A well-formed cancel B has chain_seq 0 (V2-04), so
  // check 2 refuses it too; a well-formed unbind B names a real position and
  // root and carries no key, so only the kind clause refuses it.
  await neg('1-kind-cancel-well-formed', { mutB: (b) => {
    Object.assign(b, { kind: 'cancel', expires_at_ms: T0 + DAY, chain_seq: 0, chain_entry_digest: E0, binding_root_digest: E0,
      escrow_public_key: E0 });
  } }, 'transition_invalid');
  await neg('1-unbind-b-is-not-a-binding-proof', { mutB: (b) => {
    Object.assign(b, { kind: 'unbind', recovery_key: E0, escrow_public_key: E0 });
  } }, 'transition_invalid');
  await neg('1-digest-is-not-intent-16', { mutIntent: (i) => { i.recovery_bind_digest = rnd(48); } }, 'transition_invalid');
  await neg('2-chain-seq-not-below-the-recover', { mutB: (b) => { b.chain_seq = recoverSeq; } }, 'transition_invalid');
  await neg('2-chain-entry-digest', { mutB: (b) => { b.chain_entry_digest = rnd(48); } }, 'transition_invalid');
  await neg('4-recovery-bind-sig', { mutBundle: (b) => { b.recovery_bind_sig = rnd(96); } }, 'bad_signature');
  await neg('4-sig-recovery-other-key', { recoverySigner: await wKey() }, 'bad_signature');
  await neg('6-realm', { mutB: (b) => { b.account_id = crypto.randomUUID(); } }, 'realm_mismatch');
  await neg('6-realm-id', { mutB: (b) => { b.realm_id = '11111111-1111-4111-8111-ffffffffffff'; } }, 'realm_mismatch');
  // An escrow key equal to B.recovery_key is malformed (LD-53, D-283); encode
  // refuses to write it, so the bundle carries it raw.
  await neg('escrow-key-equals-the-recovery-key', { mutBundle: async (b) => {
    const B = await decode('recovery-bind', b.recovery_bind);
    b.recovery_bind = encodeRaw('recovery-bind', { ...B, escrow_public_key: B.recovery_key }, {});
  } }, 'malformed');

  // Check 3: B names a root the chain made active and that root signs H(B),
  // but it is not the root of the effective state after result B.chain_seq.
  // proofAt points the account's B at result n under root key k.
  const proofAt = async (acct, n, k) => {
    const entry = b64d(acct.h.seq_index[n - 1].split('.')[0]);
    const root = await sha384(k.pt);
    return { bindRoot: k, mutB: (b) => { Object.assign(b, { chain_seq: n, chain_entry_digest: entry, binding_root_digest: root }); } };
  };
  const rot = await enrolled({ realm });
  const rot0 = rot.s.root;
  await rot.op('rotate_root');
  await rot.bindRecovery({ wait: 'now' });
  await neg('3-binding-root-is-not-that-states-root', await proofAt(rot, 2, rot0), 'transition_invalid', rot);
  const pa = await enrolled({ realm });
  const pa0 = pa.s.root;
  await pa.op('replace_root');
  await pa.op('rotate_ek', { at: pa.s.anb + 1 });
  await pa.bindRecovery({ wait: 'now' });
  await neg('3-pre-activation-root-of-a-pending-result', await proofAt(pa, 2, pa0), 'transition_invalid', pa);
  const dp = await enrolled({ realm });
  const dp0 = dp.s.root;
  await dp.op('replace_root', { dispute: true });
  await dp.op('cancel_pending');
  await dp.bindRecovery({ wait: 'now' });
  await neg('3-position-at-a-disputed-result', await proofAt(dp, 2, dp0), 'transition_invalid', dp);
  const pe = await enrolled({ realm });
  const pe0 = pe.s.root;
  await pe.op('erase');
  await pe.op('cancel_pending');
  await pe.bindRecovery({ wait: 'now' });
  await neg('3-position-at-a-pending-erase', await proofAt(pe, 2, pe0), 'transition_invalid', pe);
  // A candidate cancelled under H_pend never became a root, so the root
  // index has no record of it either: both halves of check 3 refuse it.
  const cc = await enrolled({ realm });
  await cc.op('replace_root');
  const cand = cc.s.pend;
  await cc.op('cancel_pending');
  await cc.bindRecovery({ wait: 'now' });
  await neg('3-cancelled-candidate-of-a-pending-result', await proofAt(cc, 2, cand), 'transition_invalid', cc);

  // Check 6: an activated recover burns its key and its chain position. Each
  // clause gets its own vector: a fresh key at a position below the recover,
  // then the burned key bound again at a later position (the authority's
  // key_bindings refuses that bind; a co-member sees only the proof).
  a = await bound();
  const first = a.L.bound; // the bind record; o.binding replays it
  await a.op('recover');
  await a.op('rotate_ek', { at: a.s.anb + 1 });
  await chainVec(ctx, 'head/recover-index-after-activation', a, 'ok', { head: true });
  const fresh = await wKey();
  await step(ctx, 'neg/proof/6-position-below-an-activated-recover', a, 'recover',
    { binding: first, recoverySigner: fresh, mutB: (b) => { b.recovery_key = fresh.pt; }, dry: true }, 'transition_invalid');
  await a.bindRecovery({ wait: 'now', key: first.key });
  await step(ctx, 'neg/proof/6-key-burned-by-an-activated-recover', a, 'recover', { dry: true }, 'transition_invalid');

  // recover_index is ascending bytewise over its text, not numerically
  // (DoR:865, E3-07): recovers activated at seq 9 and then seq 11 persist as
  // ["11.…", "9.…"]. The chain starts from the persisted head at seq 8, so the
  // vector compares the head after both activations byte for byte.
  const two = await bound();
  for (let i = 0; i < 7; i++) await two.op('rotate_ek');
  await two.op('recover'); // seq 9
  await two.op('rotate_ek', { at: two.s.anb + 1 }); // seq 10 records 9's activation
  await two.bindRecovery({ wait: 'now' });
  await two.op('recover'); // seq 11
  await two.op('rotate_ek', { at: two.s.anb + 1 }); // seq 12 records 11's
  const ri = two.h.recover_index;
  if (two.s.seq !== 12 || ri.length !== 2 || !ri[0].startsWith('11.') || !ri[1].startsWith('9.')) {
    throw new Error(`generator bug: recover_index is not ["11.…", "9.…"] at seq 12: ${ri.map((r) => r.split('.')[0])}`);
  }
  await chainVec(ctx, 'head/recover-index-orders-by-text-not-number', two, 'ok', { head: true, from: 8 });

  // Check 5 / Δ13: a key bound in an unverified segment caps recover at
  // unverified; one bound in an initial segment keeps recovery when used from
  // a later unverified segment (the binding segment caps, not the current one).
  const u = await enrolled({ realm });
  await u.op('replace_root', { assurance: 'unverified' });
  await u.op('rotate_ek', { at: u.s.anb + 1 });
  await u.bindRecovery({ wait: 'now' });
  await step(ctx, 'neg/proof/5-cap-claims-recovery', u, 'recover', { assurance: 'recovery', dry: true }, 'transition_invalid');
  await step(ctx, 'row/20/unverified-segment-caps-at-unverified', u, 'recover');
  const th = await bound();
  await th.op('replace_root', { assurance: 'unverified' });
  await th.op('rotate_ek', { at: th.s.anb + 1 });
  await step(ctx, 'row/20/initial-binding-used-from-an-unverified-segment', th, 'recover');
}

BUILDERS.push(recoveryVectors);

// DoR §3.1 "Restore-cancel" (row 22; LD-46, D-268, E3-09): accepted from a
// held P, D, Pr and P(absent) head; refused from Ac and Pe; each of §2.9's
// four client checks refused on its own. A restore asserts and issues at one
// instant T (§3.4 clear-held), so the held_status it carries is signed at T.
async function restoreVectors(ctx) {
  const realm = await Realm.create({ del: { ops: [await wKey()] } });
  const held = async (setup) => {
    const a = await setup();
    a.held = true;
    return a;
  };
  const restore = async (a, at = a.clock + 60_000, o = {}) =>
    ({ actor: 'restore', at, heldStatus: await a.status({ held: 1, asOf: at }), ...o });
  const fromP = () => enrolled({ realm }).then(async (a) => {
    await a.op('replace_root');
    return a;
  });
  const cases = {
    'p-active': fromP,
    'd-suspended-anb-0': async () => {
      const a = await enrolled({ realm });
      await a.op('suspend');
      await a.op('replace_root');
      await a.op('replace_root');
      return a;
    },
    'pr-active': async () => {
      const a = await enrolled({ realm });
      await a.bindRecovery({ wait: 'now' });
      await a.op('recover');
      return a;
    },
    'p-absent-to-ap': async () => {
      const a = await Account.create(realm, { pin: true });
      await a.op('enroll_existing');
      return a;
    },
    'p-absent-revoked-pin-to-apr': async () => {
      const a = await Account.create(realm, { pin: true });
      await a.op('revoke_root', { actor: 'operator' });
      await a.op('enroll_existing');
      return a;
    },
  };
  for (const [name, setup] of Object.entries(cases)) {
    const a = await held(setup);
    await step(ctx, `row/22/from-held-${name}`, a, 'cancel_pending', { ...(await restore(a)), headOut: true });
  }
  // The From column: Ac and Pe are not in row 22's. "Exactly x" of Ac has no
  // prior state, so the result names the from-state itself; the post-state
  // rule refuses that too (no well-formed result isolates the Ac clause).
  // Exactly x of Pe(active) is active, which only the From column refuses.
  const ac = await held(() => enrolled({ realm }));
  await step(ctx, 'neg/row-22/from-active', ac, 'cancel_pending', { ...(await restore(ac)), dry: true, inadmissible: true,
    mutResult: (r) => { r.state = 'active'; } }, 'transition_invalid');
  const pe = await held(async () => {
    const a = await enrolled({ realm });
    await a.op('erase');
    return a;
  });
  await step(ctx, 'neg/row-22/from-pe', pe, 'cancel_pending', { ...(await restore(pe)), dry: true, inadmissible: true },
    'transition_invalid');

  // Checks 1–3, one clause each, on one held P(active) head at one instant.
  const p = await held(fromP);
  const at = p.clock + 60_000;
  const base = await restore(p, at, { dry: true });
  const neg = async (name, o, reason) => step(ctx, `neg/row-22/${name}`, p, 'cancel_pending', { ...base, ...o }, reason);
  const heldAt = (o) => p.status({ held: 1, asOf: at, ...o });
  await neg('not-held', { heldStatus: await p.status({ held: 0, asOf: at }) }, 'transition_invalid');
  await neg('held-status-wrong-as-of', { heldStatus: await p.status({ held: 1, asOf: at - 1 }) }, 'transition_invalid');
  await neg('held-status-names-another-head', { heldStatus: await heldAt({ mut: (v) => { v.seq -= 1; } }) },
    'transition_invalid');
  await neg('held-status-another-account', { heldStatus: await heldAt({ mut: (v) => { v.account_id = crypto.randomUUID(); } }) },
    'transition_invalid');
  await neg('held-status-another-realm',
    { heldStatus: await heldAt({ mut: (v) => { v.realm_id = '11111111-1111-4111-8111-ffffffffffff'; } }) }, 'transition_invalid');
  await neg('held-status-signed-by-a-non-rsk', { heldStatus: await heldAt({ key: await wKey() }) }, 'transition_invalid');
  await neg('evidence-digest', { mutAuthz: (x) => { x.evidence_digest = rnd(48); } }, 'transition_invalid');
  await neg('restore-with-cp-authz-sig-is-malformed', { mutBundle: (b) => { b.cp_authz_sig = rnd(96); } }, 'malformed');
  await neg('restore-without-held-status-is-malformed', { mutBundle: (b) => { b.held_status = E0; } }, 'malformed');
  const hs = await p.status({ held: 1 });
  await step(ctx, 'neg/row-22/held-status-on-a-user-cancel-is-malformed', p, 'cancel_pending',
    { dry: true, mutBundle: (b) => { b.held_status = hs; } }, 'malformed');
  // Check 2's scope clause: the result's delegation lists the RSK that signed
  // held_status, but not `status` in its scope.
  const ns = await held(async () => {
    const a = await enrolled({ r: { del: { scope: SCOPE.filter((s) => s !== 'status') } } });
    await a.op('replace_root');
    return a;
  });
  await step(ctx, 'neg/row-22/held-status-delegation-without-status-scope', ns, 'cancel_pending',
    { ...(await restore(ns)), dry: true }, 'transition_invalid');

  // Check 2's delegation clause (DoR:1320, "signed under a delegation other
  // than the result's"): serial 2 lists serial 1's RSK, the result names
  // serial 2, and held_status names serial 1 under that shared RSK.
  const shared = await Realm.create();
  await shared.delegate({ rsk: shared.cur.rsk });
  const sd = await held(async () => {
    const a = await enrolled({ realm: shared });
    await a.op('replace_root');
    return a;
  });
  const sdAt = sd.clock + 60_000;
  await step(ctx, 'neg/row-22/held-status-names-another-delegation', sd, 'cancel_pending',
    { ...(await restore(sd, sdAt, { heldStatus: await sd.status({ held: 1, asOf: sdAt, d: shared.dels[0] }) })), dry: true },
    'transition_invalid');

  // Check 4 (E3-04, V3-09; DoR:641, :644, :1320): a client holding a fix that
  // names the from-head and shows it activated refuses the restore-cancel at
  // any issued_at; a held_status never overrides it. Without the fix each
  // bundle is accepted (H_pend admits it; H_act is Ac, which row 22 refuses).
  const q = await fromP();
  const qBefore = q.heads.at(-1);
  const qFix = await q.status({ asOf: q.s.anb + 2_000 }); // not held yet: the derived activation
  q.held = true;
  await bundleVec(ctx, 'neg/row-22/activation-fix', q, 'hypothesis_conflict',
    await q.op('cancel_pending', { ...(await restore(q, q.s.anb + 5_000)), dry: true }), { prevHead: qBefore, fix: qFix });
  // N-3: the same refusal when the head already retains the activation fix and
  // the fix argument is a newer held status: keepsFix keeps the activation, so
  // the held status never replaces it (RT-1) on VerifyBundle's path either.
  const k = await fromP();
  const kHead = k.expectedChainHead(await k.status({ asOf: k.s.anb + 2_000 }));
  k.held = true;
  await bundleVec(ctx, 'neg/row-22/activation-fix-kept-over-a-held-fix-argument', k, 'hypothesis_conflict',
    await k.op('cancel_pending', { ...(await restore(k, k.s.anb + 5_000)), dry: true }),
    { prevHead: kHead, fix: await k.status({ held: 1, asOf: k.s.anb + 4_000 }) });
  // Back-dated below the fix's as_of: R4 alone leaves H_pend standing (it
  // fixes H_act for issued ≥ T only), so check 4 is what refuses it.
  const b = await fromP();
  const bBefore = b.heads.at(-1);
  const bFix = await b.status({ asOf: b.s.anb + 5_000 });
  b.held = true;
  await bundleVec(ctx, 'neg/row-22/activation-fix-after-a-back-dated-restore', b, 'hypothesis_conflict',
    await b.op('cancel_pending', { ...(await restore(b, b.s.anb + 2_000)), dry: true }), { prevHead: bBefore, fix: bFix });
  // The same instants with a fix showing the from-head still pending (held):
  // the fix fixes H_pend, which row 22 admits.
  const w = await fromP();
  const wBefore = w.heads.at(-1);
  w.held = true;
  const wFix = await w.status({ asOf: w.s.anb + 5_000 });
  const wBundle = await w.op('cancel_pending', await restore(w, w.s.anb + 2_000));
  await bundleVec(ctx, 'row/22/fix-shows-the-from-head-pending', w, 'ok', wBundle,
    { prevHead: wBefore, fix: wFix, headOut: w.heads.at(-1) });
}

BUILDERS.push(restoreVectors);

// ── 9b. Device-rule parity (D-270, A6) ────────────────────────────────────
// jsApply and jsNormalize log every call while deviceRuleVectors' scenario
// runs, and are silent otherwise; resetDraw() forgets both logs.
let APPLY_LOG = null;
let NORM_LOG = null;

const entryStr = (n) => (n.dsk?.pt ? devStr(entryOf(n)) : devStr(n));
const pad2 = (i) => String(i).padStart(2, '0');
const devInput = (inp, issued, delta) => ({ op: inp.op, actor: inp.actor ?? '', new_dev: inp.new ? entryStr(inp.new) : '',
  removed: inp.removed ?? [], bind_b64: b64s(inp.bind), bind_wait: inp.bindWait ?? '', prior_sig: !!inp.priorSig,
  root_form: !!inp.rootForm, head: { state: inp.head.state, seq: inp.head.seq, entry_digest_b64: b64s(inp.head.entry),
    root_digest_b64: b64s(inp.head.root), force_unverified: !!inp.head.fu },
  outcome: inp.outcome ?? '', proposing_anb_ms: inp.anb ?? 0, issued_at_ms: issued, delta_ms: delta });

// A refusal is device_rule on the account's own devices (the PR-A2 plan's
// rule for authority-function vectors); an admitted op carries refusal "".
async function applyVec(ctx, name, ids, c, inp = c.inp) {
  const { S, refusal } = await jsApply(c.S0, inp, c.issued, c.delta);
  await pushA2(ctx, { name, type: 'apply-device-op', bytes_b64: b64e(encodeS(...ids, c.S0)),
    input: devInput(inp, c.issued, c.delta),
    expect: refusal ? expectOf('device_rule', { refusal })
      : expectOf('ok', { refusal: '', set_b64: b64e(encodeS(...ids, S)) }) });
}

async function normVec(ctx, name, ids, S0, f) {
  await pushA2(ctx, { name, type: 'normalize', bytes_b64: b64e(encodeS(...ids, S0)),
    input: { activate: !!f.activate, burn_recovery: !!f.burn, complete_slot: !!f.complete, clear_pending: !!f.clearPending,
      empty_slot: !!f.emptySlot },
    expect: expectOf('ok', { set_b64: b64e(encodeS(...ids, jsNormalize(S0, f))) }) });
}

// The scenario's own account runs every private op in each of its forms and
// every attached published op but enroll_new and enroll_existing, which two
// more accounts run. Each comment names the rule the call exercises.
async function deviceRuleScenario(a, own) {
  const realm = a.r;
  own(a);
  const { nd: d1 } = await a.addDevice({ at: a.clock + 60_000 });
  const { nd: d2 } = await a.addDevice({ at: a.clock + 60_000 });
  await a.bindRecovery({ wait: 'now' }); // first bind at the now tier: bound at once (DR9 a)
  // active := active \ removed (DR5); DR4 binds the actor, so a removed device need not be mature.
  const rotate = await a.op('rotate_root', { removed: [d1.id, d2.id].sort() });
  await a.op('rotate_ek');
  await a.op('suspend');
  await a.op('unsuspend'); // the user form: an actor device under DR1 and DR4 (LD-40)
  await a.bindRecovery({ wait: 'delta' }); // a root-only refresh fills the slot (DR9 a)
  const sheet = await a.cancelRecoveryBind({}); // the sheet form drops it and keeps the bound key (DR9 b)
  await a.bindRecovery({ wait: 'delta' });
  await a.bindRecovery({ prior: true }); // with the old key: bound at once, the pending refresh dropped (DR9 a)
  await a.bindRecovery({ wait: 'delta' });
  // Issued at the slot's nb + g: settle completes the Δ refresh with no entry
  // (DR9 c), and this root-only refresh then fills the slot for RECOVERY_REPLACE_MS.
  await a.bindRecovery({ wait: 'replace', at: a.L.S.nb + a.L.slotG });
  await a.op('replace_root'); // the slot survives a pending replace_root (DR9 d)
  // The replacement activates at its anb, about 27 d before the slot's nb + g,
  // so the activation applies first and burns the slot; the bound key stays
  // (DR8, LD-52, C4-4).
  await a.op('rotate_ek', { at: a.s.anb + 1 });
  await a.bindRecovery({ wait: 'now' }); // a root-only refresh at the now tier
  const recover = await a.op('recover'); // row 20: H(B) is the bound key's digest (DR9 e)
  const recoverEntry = a.L.dbundles.at(-1);
  const row21 = await a.op('cancel_pending', { by: 'recovery' }); // row 21: pending := "", the key kept (DR8)
  await a.bindRecovery({ unbind: true }); // the root-only unbind at the now tier (LD-49)
  await a.bindRecovery({ wait: 'delta' }); // a pending first bind
  await a.cancelRecoveryBind({ root: true }); // the root form drops it (LD-43)
  await a.bindRecovery({ wait: 'now' });
  await a.bindRecovery({ unbind: true, prior: true }); // the old-key unbind: no key bound, at once (LD-49)

  const full = await Account.create(realm);
  own(full);
  await full.op('enroll_new');
  for (let i = 1; i < 10; i++) await full.addDevice({ at: full.clock + 60_000 }); // MAX_DEVICES active
  await full.fork().addDevice({ at: full.clock + 60_000, force: true }); // DR2's cap: logged, refused, not kept

  const pinned = await Account.create(realm, { pin: true });
  own(pinned);
  await pinned.op('enroll_existing'); // from absent, no PoP: a P outcome, pending := new (DR5)
  return { a, rotate, sheet, recover, recoverEntry, row21 };
}

// DoR §3.1 "Device rules", "Recovery key" (private half), "Normalize" and
// "malformed_for_op": every call the scenario made, then single-field
// mutations of the first call per op; Normalize over the scenario's calls and
// over all 32 flag combinations of the richest set it saw.
async function deviceRuleVectors(ctx) {
  const realm = await Realm.create();
  const a = await Account.create(realm);
  await a.op('enroll_new'); // before the logs start: the first logged call is a's first add_device
  APPLY_LOG = [];
  NORM_LOG = [];
  const owners = [];
  const own = (acc) => owners.push({ id: acc.id, apply: APPLY_LOG.length, norm: NORM_LOG.length });
  const sc = await deviceRuleScenario(a, own);
  const calls = APPLY_LOG;
  const norms = NORM_LOG;
  APPLY_LOG = NORM_LOG = null;
  // Each logged call encodes its set under the account that made it.
  const idsAt = (log, i) => [realm.id, owners.findLast((o) => o[log] <= i).id];

  for (const [i, c] of calls.entries()) await applyVec(ctx, `apply/scenario/${pad2(i)}-${c.inp.op}`, idsAt('apply', i), c);
  const muts = {
    'actor-unknown': (i) => ({ ...i, actor: crypto.randomUUID() }),
    'head-suspended': (i) => ({ ...i, head: { ...i.head, state: 'suspended' } }),
    'head-pending': (i) => ({ ...i, head: { ...i.head, state: 'pending' } }),
    'bind-wait-replace': (i) => ({ ...i, bindWait: 'replace' }),
    'prior-sig-flipped': (i) => ({ ...i, priorSig: !i.priorSig }),
    'root-form-flipped': (i) => ({ ...i, rootForm: !i.rootForm }),
    'outcome-d': (i) => ({ ...i, outcome: 'D' }),
  };
  const firsts = new Map();
  for (const [i, c] of calls.entries()) if (!firsts.has(c.inp.op)) firsts.set(c.inp.op, i);
  for (const [op, i] of firsts) {
    for (const [m, f] of Object.entries(muts)) await applyVec(ctx, `apply/${op}/${m}`, idsAt('apply', i), calls[i], f(calls[i].inp));
  }
  for (const [i, n] of norms.entries()) await normVec(ctx, `normalize/scenario/${pad2(i)}`, idsAt('norm', i), n.S0, n.f);
  // A pending device, a bound key and a slot, so every flag changes something.
  const ri = norms.findLastIndex((n) => n.S0.pending && n.S0.key.length && n.S0.rdig.length);
  if (ri < 0) throw new Error('generator bug: no Normalize call saw a pending device, a bound key and a slot');
  for (let m = 0; m < 32; m++) {
    const f = { activate: !!(m & 1), burn: !!(m & 2), complete: !!(m & 4), clearPending: !!(m & 8), emptySlot: !!(m & 16) };
    await normVec(ctx, `normalize/matrix/${m.toString(2).padStart(5, '0')}`, idsAt('norm', ri), norms[ri].S0, f);
  }
  await recoveryBindForOpVectors(ctx, calls, sc.a);
  await relayRecoverPatternVectors(ctx, sc);
}

// malformed_for_op for recovery-bind (§2.22, V2-04): every B a call carried,
// then its escrow position flipped. A position its kind does not use is
// malformed_for_op at the authority, device_rule on own devices for a private
// op's B, and malformed inside a published recover bundle (decision A5).
async function recoveryBindForOpVectors(ctx, calls, a) {
  for (const [i, c] of calls.entries()) {
    if (!c.inp.bind?.length) continue;
    const B = await decode('recovery-bind', c.inp.bind);
    const where = c.inp.op === 'recover' ? 'malformed' : 'device_rule';
    const vec = (name, bytes, ok) => pushA2(ctx, { name, type: 'for-op', bytes_b64: b64e(bytes), input: { tag: 'recovery-bind' },
      expect: ok ? expectOf('ok', { refusal: '' }) : expectOf(where, { refusal: 'malformed_for_op' }) });
    const name = `for-op/recovery-bind/${pad2(i)}-${B.kind}`;
    await vec(name, c.inp.bind, bindKindOk(B));
    const odd = { ...B, escrow_public_key: B.escrow_public_key.length ? E0 : a.s.root.pt };
    await vec(`${name}/flip-escrow`, encode('recovery-bind', odd), bindKindOk(odd));
  }
}

// The relay-recover envelope rule (§2.6): exactly one of form R (a recover
// intent), C (a cancel_pending intent) and B (a cancel B), by which positions
// are non-empty, each with its own intent op or B kind. Positions come from
// the scenario's real bundles and entries.
async function relayRecoverPatternVectors(ctx, sc) {
  const cancel = await decode('bundle', sc.row21);
  const rot = await decode('bundle', sc.rotate);
  const rec = await decode('bundle', sc.recover);
  const recDev = await decode('device-bundle', sc.recoverEntry);
  const sheet = await decode('device-bundle', sc.sheet.bytes);
  const none = { intent: E0, sig_candidate: E0, sig_recovery: E0, ek_binding: E0, ek_binding_sig: E0, recovery_bind: E0,
    recovery_bind_sig: E0, device_intent: E0, sig_new_device: E0 };
  // encode applies the form rule itself; encodeRaw writes a pattern no form admits.
  const relay = (o) => encode('relay-recover', { ...none, ...o });
  const relayRaw = (o) => encodeRaw('relay-recover', { ...none, ...o }, {});
  const formR = { intent: rec.intent, sig_candidate: rec.sig_candidate, sig_recovery: rec.sig_recovery, ek_binding: rec.ek_binding,
    ek_binding_sig: rec.ek_binding_sig, recovery_bind: rec.recovery_bind, recovery_bind_sig: rec.recovery_bind_sig,
    device_intent: recDev.device_intent, sig_new_device: recDev.sig_new_device };
  const formC = { intent: cancel.intent, sig_recovery: cancel.sig_recovery };
  const formB = { recovery_bind: sheet.recovery_bind, sig_recovery: sheet.sig_recovery };
  for (const [name, bytes, reason] of [
    ['relay-recover/form-r', relay(formR), 'ok'],
    ['relay-recover/form-r-with-a-cancel-intent', relay({ ...formR, intent: cancel.intent }), 'malformed'],
    ['relay-recover/form-c', relay(formC), 'ok'],
    ['relay-recover/form-c-with-a-non-cancel-intent', relay({ ...formC, intent: rot.intent }), 'malformed'],
    ['relay-recover/form-c-plus-a-stray-bind', relayRaw({ ...formC, recovery_bind: rec.recovery_bind }), 'malformed'],
    ['relay-recover/form-b', relay(formB), 'ok'],
    ['relay-recover/form-b-with-a-bind', relay({ ...formB, recovery_bind: rec.recovery_bind }), 'malformed']]) {
    await pushA2(ctx, { name: `pattern/${name}`, type: 'pattern', bytes_b64: b64e(bytes), input: { tag: 'relay-recover' },
      expect: expectOf(reason) });
  }
}

BUILDERS.push(deviceRuleVectors);

// ── 9c. Published-intent positions per op (§2.7, V-18) ────────────────────
// The six positions §2.7 sets per op, in its column order. expected_seq,
// expected_recovery_gen and expected_head_digest are always the head's values
// (§2.7, decision A4): a value rule, never an op-determined position.
const INTENT_POSITIONS = ['candidate_root', 'ek_binding_digest', 'legacy_ek_digest', 'predecessor_root_digest',
  'device_intent_digest', 'recovery_bind_digest'];

// intentUses is section 7's reading of §2.7, the sets runOp fills an intent
// from: the positions a published intent carries for its op, its actor, a PoP
// and a root-signed cancel. Every other position MUST be "".
function intentUses(op, actor, pop, rootCancels) {
  const uses = new Set();
  if (PROPOSES.has(op)) uses.add('candidate_root');
  if (EK_OPS.has(op)) uses.add('ek_binding_digest');
  if (pop && (op === 'enroll_existing' || op === 'cancel_pending')) uses.add('legacy_ek_digest'); // "= pin iff PoP"
  if (PRED_OPS.has(op) || (op === 'cancel_pending' && rootCancels)) uses.add('predecessor_root_digest');
  if (D15_OPS.has(op) || (op === 'unsuspend' && actor === 'user')) uses.add('device_intent_digest');
  if (op === 'recover') uses.add('recovery_bind_digest'); // LD-34
  return uses;
}

// Every §2.7 op in each form that changes its positions — the actor, a PoP,
// and the cancel's signer — published from a From state its §2.9 row admits.
// Dry runs leave the model where it was; committed ops walk it to the next
// From state. Each comment names the row.
async function intentForOpScenario() {
  const realm = await Realm.create({ del: { ops: [await wKey()] } }); // the operator rows sign sig_operator
  const a = await Account.create(realm);
  const out = [await a.op('enroll_new')]; // row 1 from A0
  await a.bindRecovery({ wait: 'now' }); // a bound key for recover (DR9 e)
  for (const [op, actor] of [['rotate_root'], ['rotate_ek'], ['suspend'], ['suspend', 'operator'], ['replace_root'],
    ['revoke_root', 'operator'], ['erase'], ['erase', 'operator'], ['recover']]) {
    out.push(await a.op(op, { actor, dry: true })); // rows 5, 6, 7, 8, 11, 15, 18, 19, 20 from Ac
  }
  await a.op('suspend');
  out.push(await a.op('unsuspend', { dry: true })); // row 9 from Su
  await a.op('unsuspend');
  await a.op('suspend', { actor: 'operator' });
  out.push(await a.op('unsuspend', { actor: 'operator', dry: true })); // row 10 from So
  await a.op('unsuspend', { actor: 'operator' });
  await a.op('replace_root');
  out.push(await a.op('cancel_pending', { dry: true })); // row 13 from P(active): the root cancels
  const held = a.fork();
  held.held = true;
  out.push(await held.op('cancel_pending', { actor: 'restore', heldStatus: await held.status(), dry: true })); // row 22
  await a.op('cancel_pending');
  await a.op('recover');
  out.push(await a.op('cancel_pending', { dry: true })); // row 21 from Pr(active)
  const p = await Account.create(realm, { pin: true });
  out.push(await p.op('enroll_existing', { dry: true })); // row 3 from Ap
  out.push(await p.op('enroll_existing', { pop: true, dry: true })); // row 2 from Ap
  await p.op('enroll_existing');
  out.push(await p.op('cancel_pending', { dry: true })); // row 14 from P(absent): the PoP cancels
  return out;
}

// The form a bundle publishes, read from its own positions.
function intentForm(op, actor, b, pop) {
  if (op === 'enroll_existing') return pop ? 'pop' : 'no-pop';
  if (op !== 'cancel_pending' || actor !== 'user') return '';
  if (b.sig_current_root.length) return 'root';
  return pop ? 'pop' : 'recovery';
}

// DoR §3.1 "malformed_for_op" for the published intent: each form's intent as
// the model published it (admitted), then every position its op does not use
// made non-empty. The authority records malformed_for_op; a co-member reads a
// committed intent no row admits as transition_invalid (decision A5).
async function intentForOpVectors(ctx) {
  const labels = new Set();
  for (const [i, bb] of (await intentForOpScenario()).entries()) {
    const b = await decode('bundle', bb);
    const { actor } = await decode('cp-authz', b.cp_authz);
    const intent = await decode('intent', b.intent);
    const pin = (await decode('result', b.result)).legacy_ek_digest;
    const pop = b.legacy_pop_sig.length > 0;
    const rootCancels = intent.op === 'cancel_pending' && b.sig_current_root.length > 0;
    const uses = intentUses(intent.op, actor, pop, rootCancels);
    const carried = INTENT_POSITIONS.filter((p) => intent[p].length).join();
    if (carried !== INTENT_POSITIONS.filter((p) => uses.has(p)).join()) {
      throw new Error(`generator bug: a published ${intent.op}/${actor} intent carries [${carried}], §2.7 reads [${[...uses]}]`);
    }
    const label = [intent.op, actor, intentForm(intent.op, actor, b, pop)].filter(Boolean).join('-');
    if (labels.has(label)) throw new Error(`generator bug: the intent scenario publishes ${label} twice`);
    labels.add(label);
    const name = `for-op/intent/${pad2(i)}-${label}`;
    const input = { tag: 'intent', actor, pop, root_cancels: rootCancels };
    await pushA2(ctx, { name, type: 'for-op', bytes_b64: b64e(b.intent), input, expect: expectOf('ok', { refusal: '' }) });
    for (const pos of INTENT_POSITIONS.filter((p) => !uses.has(p))) {
      // A value the position could hold: a real point, the account's own pin.
      const v = pos === 'candidate_root' ? (await wKey()).pt : pos === 'legacy_ek_digest' && pin.length ? pin : rnd(48);
      await pushA2(ctx, { name: `${name}/flip-${pos}`, type: 'for-op', bytes_b64: b64e(encode('intent', { ...intent, [pos]: v })),
        input, expect: expectOf('transition_invalid', { refusal: 'malformed_for_op' }) });
    }
  }
  const missing = PUBLISHED_OPS.filter((op) => ![...labels].some((l) => l.split('-')[0] === op));
  if (missing.length) throw new Error(`generator bug: the intent scenario publishes no ${missing}`);
}

BUILDERS.push(intentForOpVectors);

// ── 9d. Device ledger, P1 and slot burning (PR-A2) ────────────────────────
// DoR §3.1 "Device ledger", "P1" and "Recovery key" (completion and slot
// burning): DR3, DR7 (D-272), D-240 (an attached device part is the one intent
// #15 commits to), DR8 (a Pe activation moves nothing private, so row 18's
// erase keeps its pending device in the tombstone), DR9 (c) and C4-4 (decision
// A10: a tie completes first). A vector whose chain verified carries the heads
// VerifyDeviceLedger persists (o.heads; by default only for ok).
async function ledgerVec(ctx, name, a, reason, o = {}) {
  const status = o.status ?? (await a.status(o.st));
  const ds = o.deviceStatus ?? (await a.deviceStatus(status, o.ds));
  const dbs = o.dbundles ?? a.L.dbundles;
  const heads = o.heads ?? reason === 'ok';
  await pushA2(ctx, { name, type: 'device-ledger', anchors: b64e(a.r.anchors), trust: a.r.items.map(b64e), hw: HW0,
    prev_head: b64e(a.heads[0]), prev_device_head: b64e(await a.genesisDeviceHead(a.L.self)), bundles: a.bundles.map(b64e),
    device_bundles: dbs.map(b64e), bytes_b64: b64e(status), device_status: b64e(ds), mem: a.mem(), now_ms: o.now ?? a.clock + 2_000,
    use: o.use ?? 'new-secret', expect: expectOf(reason, heads ? { head_b64: b64e(a.expectedChainHead(status)),
      device_head_b64: b64e(await a.expectedDeviceHead(status)) } : {}) });
}

// reattachLast replaces the account's last entry, an attached one, with the same
// entry over a fresh device-intent (a new salt): the device-result names the new
// D, every private signature verifies over it, and the RSK re-signs, so only the
// published intent's #15 still names the old D (D-240).
async function reattachLast(a) {
  const L = a.L;
  const db = await decode('device-bundle', L.dbundles.at(-1));
  const di = await decode('device-intent', db.device_intent);
  const diB = encode('device-intent', { ...di, salt: rnd(32) });
  const D = await sha384(diB);
  const dr = encode('device-result', { ...(await decode('device-result', db.device_result)), device_intent_digest: D });
  const nd = di.new_device_id ? L.devs.get(di.new_device_id) : null;
  const act = di.actor_device_id ? L.devs.get(di.actor_device_id) : null;
  L.dbundles = [...L.dbundles.slice(0, -1), encode('device-bundle', { ...db, device_intent: diB, device_result: dr,
    device_result_sig: await wSign(a.r.cur.rsk, dr), sig_new_device: nd ? await signAs(nd.dsk, 'new-device', D) : E0,
    sig_actor: act ? await signAs(act.dsk, 'actor-device', D) : E0 })];
  L.last = await sha384(dr);
  L.lastResult = dr;
}

async function ledgerVectors(ctx) {
  const realm = await Realm.create({ del: { ops: [await wKey()] } });
  const a = await enrolled({ realm });
  await ledgerVec(ctx, 'ledger/genesis-device', a, 'ok');
  const firstRoot = a.s.root; // the root a back-dated entry would sign under (D-272)
  const { nd: d1 } = await a.addDevice({ at: a.clock + 60_000 });
  await a.addDevice({ at: a.clock + 60_000 });
  await a.op('rotate_root');
  await a.op('rotate_ek');
  const lastAttached = a.fork(); // its last entry is rotate_ek's attached entry
  await a.bindRecovery({ wait: 'now' });
  await ledgerVec(ctx, 'ledger/devices-rotations-and-a-bind', a, 'ok');
  // DoR:919, :1120, :1778: a device-status older than τ is device_status_stale
  // for every Use. History use relaxes the chain status to V, never the
  // private ledger; otherwise the CP could withhold a removal for up to V.
  // Exactly τ old is the control.
  const hist = await a.status();
  const histAsOf = statusR4Of(hist).asOf;
  await ledgerVec(ctx, 'ledger/history-device-status-tau-old', a, 'ok', { status: hist, use: 'history', now: histAsOf + TAU });
  await ledgerVec(ctx, 'ledger/history-device-status-older-than-tau', a, 'device_status_stale',
    { status: hist, use: 'history', now: histAsOf + TAU + 1_000 });
  const n = a.L.dbundles.length;
  await ledgerVec(ctx, 'ledger/missing-entry', a, 'device_gap', { dbundles: a.L.dbundles.filter((_, i) => i !== 1) });
  await ledgerVec(ctx, 'ledger/missing-attached-entry', a, 'device_gap',
    { dbundles: a.L.dbundles.filter((_, i) => i !== n - 2) }); // the rotate_ek part
  // The same, with no entry after it: dseq stays contiguous, so only DR7's
  // "exactly one attached entry" refuses it (a walk that skipped it would end
  // behind the device-status instead).
  await ledgerVec(ctx, 'ledger/missing-last-attached-entry', lastAttached, 'device_gap',
    { dbundles: lastAttached.L.dbundles.slice(0, -1) });
  await ledgerVec(ctx, 'ledger/reordered', a, 'device_gap',
    { dbundles: [a.L.dbundles[1], a.L.dbundles[0], ...a.L.dbundles.slice(2)] });
  await ledgerVec(ctx, 'ledger/device-status-another-head', a, 'device_status_mismatch',
    { ds: { mut: (v) => { v.chain_head_digest = rnd(48); } } });
  await ledgerVec(ctx, 'ledger/device-status-another-as-of', a, 'device_status_mismatch',
    { ds: { mut: (v) => { v.as_of_ms += 1; } } });
  await ledgerVec(ctx, 'ledger/device-status-older-as-of', a, 'device_status_stale',
    { ds: { mut: (v) => { v.as_of_ms -= 1; } } });
  await ledgerVec(ctx, 'ledger/device-status-set-digest', a, 'device_status_mismatch',
    { ds: { mut: (v) => { v.device_set_digest = rnd(48); } } });
  await ledgerVec(ctx, 'ledger/fork', a, 'device_fork', { ds: { mut: (v) => { v.device_head_digest = rnd(48); } } });
  await ledgerVec(ctx, 'ledger/behind', a, 'device_behind', { ds: { mut: (v) => { v.dseq += 1; } } });

  // DR3 and DR7 through the model's own hooks (o.mutDr, o.mutDb, o.mutDi, o.rootKey).
  let b = a.fork();
  await b.addDevice({ at: b.clock + 60_000, mutDb: (db) => { db.sig_actor = rnd(96); } });
  await ledgerVec(ctx, 'ledger/actor-signature', b, 'bad_signature');
  b = a.fork();
  await b.addDevice({ at: b.clock + 60_000, mutDb: (db) => { db.sig_root = E0; } });
  await ledgerVec(ctx, 'ledger/add-device-without-sig-root', b, 'device_rule');
  // An RSK holding the first root back-dates an admission to seq 1, where that
  // root was active, and the device-intent names that head too: every signature
  // verifies and every position agrees, so only D-272's order refuses it.
  b = a.fork();
  const early = await sha384((await decode('bundle', a.bundles[0])).result);
  await b.addDevice({ at: b.clock + 60_000, rootKey: firstRoot,
    mutDi: (di) => { di.expected_seq = 1; di.expected_head_digest = early; },
    mutDr: (dr) => { dr.chain_seq = 1; dr.chain_entry_digest = early; } });
  await ledgerVec(ctx, 'ledger/chain-seq-back-dated-below-an-attached-entry', b, 'device_rule');
  // V2-04: an add_device device-intent naming a removed device (#17 is [] on it).
  b = a.fork();
  await b.addDevice({ at: b.clock + 60_000, mutDi: (di) => { di.removed_device_ids = [d1.id]; } });
  await ledgerVec(ctx, 'ledger/add-device-uses-a-position-it-does-not', b, 'device_rule');
  // DoR:413-414, :1305: an op with an actor device built on another private head
  // than the one its entry lands on, as a replayed device-intent is, is a device
  // rule failure on own devices (the authority's stale_device_seq, DoR:1449).
  if (a.L.dseq < 2) throw new Error('the stale-position vectors need dseq >= 2');
  b = a.fork();
  await b.addDevice({ at: b.clock + 60_000, mutDi: (di) => { di.expected_dseq -= 1; } });
  await ledgerVec(ctx, 'ledger/add-device-built-on-an-earlier-dseq', b, 'device_rule');
  b = a.fork();
  await b.addDevice({ at: b.clock + 60_000, mutDi: (di) => { di.expected_device_head = rnd(48); } });
  await ledgerVec(ctx, 'ledger/add-device-built-on-another-device-head', b, 'device_rule');
  // D-240: the enrolment's device part swapped for another device-intent.
  const sw = await enrolled({ realm });
  await reattachLast(sw);
  await ledgerVec(ctx, 'ledger/attached-entry-another-device-intent', sw, 'device_gap');
  // §2.5 device-result #14: a private entry's issued_at keeps R1's bounds against its cp-authz.
  const late = (await enrolled({ realm })).fork();
  await late.addDevice({ at: late.clock + 60_000, mutDr: (dr) => { dr.issued_at_ms += 3_600_000; } });
  await ledgerVec(ctx, 'ledger/add-device-issued-past-its-authorization', late, 'issued_bounds');
  // T7: an entry under a superseded delegation stands only under a status that
  // passes the history rule (now ≤ valid_until).
  const renewed = await Realm.create();
  const old = renewed.cur;
  await renewed.delegate();
  const r = await enrolled({ realm: renewed });
  await r.addDevice({ at: r.clock + 60_000, d: old });
  await ledgerVec(ctx, 'ledger/entry-under-a-superseded-delegation-covered', r, 'ok');
  await ledgerVec(ctx, 'ledger/entry-under-a-superseded-delegation-uncovered', r, 'superseded_uncovered',
    { st: { v: 500 } });

  // Slot burning and C4-4: a delta-wait bind leaves a slot; a new-root
  // activation empties it (LD-52); completion first on a tie (A10).
  const s = await enrolled({ realm });
  await s.bindRecovery({ wait: 'now' });
  await s.bindRecovery({ wait: 'delta' });
  await ledgerVec(ctx, 'ledger/slot-pending', s, 'ok');
  const done = s.fork();
  done.clock = done.L.S.nb + done.L.slotG;
  await ledgerVec(ctx, 'ledger/slot-completed-by-the-device-status', done, 'ok');
  // asserted = issued − 1 s, so anb = issued − 1 s + Δ + g: issuing 1 s later
  // than nb + g − Δ − g puts anb exactly at the slot's completion instant.
  const race = s.fork();
  const tc = race.L.S.nb + race.L.slotG;
  await race.op('replace_root', { at: tc - race.r.cur.v.delta_ms - race.r.cur.v.g_ms + 1_000 });
  if (race.s.anb !== tc) throw new Error(`generator bug: the C4-4 tie is not a tie (anb ${race.s.anb}, completion ${tc})`);
  await race.op('rotate_ek', { at: race.s.anb + 1 });
  await ledgerVec(ctx, 'ledger/c4-4-completion-and-activation-tie', race, 'ok');
  // DR8: a new-root activation makes the pending device the only active one.
  const act = await enrolled({ realm });
  await act.op('replace_root');
  await act.op('rotate_ek', { at: act.s.anb + 1 });
  await ledgerVec(ctx, 'ledger/replace-root-activation-drops-every-old-device', act, 'ok');
  // LD-52, C4-4: asserted before the slot filled, the replacement's anb comes
  // before the slot's completion, so the activation applies first and burns the
  // slot, keeping the bound key: once through an entry issued after both
  // instants, once shown only by the status, whose persisted set keeps the slot
  // (device-head #7: before Normalize's derived activation).
  const bindIssued = s.L.S.nb - s.r.cur.v.delta_ms;
  const first = s.fork();
  await first.op('replace_root', { at: bindIssued + 1_000, asserted: bindIssued - 5_000 });
  const slotAt = first.L.S.nb + first.L.slotG;
  if (first.s.anb >= slotAt) throw new Error('generator bug: the replacement does not activate before the slot completes');
  const shownOnly = first.fork();
  await first.op('rotate_ek', { at: slotAt + 1 });
  await ledgerVec(ctx, 'ledger/activation-before-completion-burns-the-slot', first, 'ok');
  shownOnly.clock = slotAt;
  await ledgerVec(ctx, 'ledger/status-shows-activation-before-completion', shownOnly, 'ok');
  const burn = await enrolled({ realm });
  await burn.bindRecovery({ wait: 'now' });
  await burn.op('recover');
  await burn.op('rotate_ek', { at: burn.s.anb + 1 });
  await ledgerVec(ctx, 'ledger/recover-activation-burns-the-key', burn, 'ok');
  // DR8: a Pe(x) activation is "E, with the root kept" and keeps the set, so the
  // candidate's pending device stays pending in the tombstone and a slot that
  // has not completed stays pending (row 18 is not in DoR:634's burn list).
  const e = await enrolled({ realm });
  await e.bindRecovery({ wait: 'replace' }); // a first bind, pending for RECOVERY_REPLACE_MS
  await e.op('replace_root');
  await e.op('erase'); // row 18 from P(active): Pe(active)
  e.clock = e.s.anb; // the status shows the erase activated
  await ledgerVec(ctx, 'ledger/row-18-erase-keeps-the-pending-device-and-the-slot', e, 'erased', { heads: true });

  // §2.22 #7 (E3-10, V3-06): a root-form cancel's B #7 is its device-intent's
  // #8. Here #8 is one ms later; every signature, digest and bound still holds
  // (#8 stays > asserted), so own devices refuse it on that identity alone
  // (device_rule), as CheckDeviceSubmit refuses it at the authority (malformed).
  const rc = await enrolled({ realm });
  await rc.bindRecovery({ wait: 'delta' }); // a first bind, pending
  await rc.cancelRecoveryBind({ root: true, mutDi: (di) => { di.expires_at_ms += 1; } });
  await ledgerVec(ctx, 'ledger/root-form-cancel-expiry-is-not-device-intent-8', rc, 'device_rule');

  // DR9 (c), RT6-9, G-41, A6: from replacement_nb_ms own devices hold the slot
  // pending and completed, with no upper bound, because the authority may
  // complete it after nb + g (the accumulator's growth, a post-restore floor:
  // L.slotExt); the device-status or the next entry decides between them.
  const nb = s.L.S.nb;
  const later = nb + s.L.slotG + 600_000; // past the unextended completion instant
  const setOf = (a, S = a.L.S) => sha384(encodeS(a.r.id, a.id, S));
  const ext = s.fork();
  ext.L.slotExt = 3_600_000;
  ext.clock = later;
  await ledgerVec(ctx, 'ledger/slot-still-pending-past-nb-plus-g', ext, 'ok');
  const cx = s.fork();
  cx.L.slotExt = 3_600_000;
  await cx.cancelRecoveryBind({ at: later });
  await ledgerVec(ctx, 'ledger/sheet-cancel-past-nb-plus-g-on-an-extended-slot', cx, 'ok');
  // RT6-9 and A11 (DoR §3.1 "Completion under cancel unavailability"): a
  // device-status showing the slot pending at T fixes it pending for every
  // entry issued at or before T, so it refutes an earlier entry that showed the
  // slot completed, and that entry is device_rule. The status shows what the
  // authority would show had it kept the slot pending for that same entry.
  const nd = await newDevice();
  const shownDone = s.fork();
  await shownDone.addDevice({ at: later, newDev: nd });
  const keptPending = s.fork();
  keptPending.L.slotExt = 3_600_000;
  await keptPending.addDevice({ at: later, newDev: nd });
  const pendingSet = await setOf(keptPending);
  await ledgerVec(ctx, 'ledger/status-pending-refutes-an-earlier-entry-showing-completion', shownDone, 'device_rule',
    { ds: { mut: (v) => { v.device_set_digest = pendingSet; } } });
  // The converse: a sheet cancel past nb + g needed the slot pending, and a
  // device-status showing it completed (with no cancel) refutes it.
  const completed = s.fork();
  completed.completeSlot();
  const completedSet = await setOf(completed);
  await ledgerVec(ctx, 'ledger/status-completed-refutes-an-earlier-cancel', cx, 'device_rule',
    { ds: { mut: (v) => { v.device_set_digest = completedSet; } } });
  // A device-status and an entry that match neither hypothesis: the slot
  // emptied with the old key still bound is neither pending nor completed.
  const neither = s.fork();
  neither.clock = later;
  const neitherSet = await setOf(neither, jsNormalize(neither.L.S, { emptySlot: true }));
  await ledgerVec(ctx, 'ledger/slot-status-matching-neither-hypothesis', neither, 'device_status_mismatch',
    { ds: { mut: (v) => { v.device_set_digest = neitherSet; } } });
  const forged = s.fork();
  await forged.addDevice({ at: nb, forceSet: (S) => jsNormalize(S, { emptySlot: true }) });
  await ledgerVec(ctx, 'ledger/slot-entry-matching-neither-hypothesis', forged, 'device_rule');

  // R3, A6: from its anb a raw pending head reads as pending or activated, and
  // the authority may activate it later. A sheet cancel after the anb found the
  // replacement still pending (H_act would have burned the slot, LD-52); a later
  // add_device needs it active, which fixes H_act for it and every later entry
  // (R3), and the next result matched H_act.
  const ha = await enrolled({ realm });
  await ha.bindRecovery({ wait: 'now' });
  await ha.bindRecovery({ wait: 'replace' }); // a refresh pending for RECOVERY_REPLACE_MS
  await ha.op('replace_root');
  const anb = ha.s.anb;
  await ha.cancelRecoveryBind({ at: anb + 10_000, noActivate: true });
  await ha.addDevice({ at: anb + 70_000 });
  await ha.op('rotate_ek');
  await ledgerVec(ctx, 'ledger/activation-extended-past-the-anb-then-fixed-by-an-add-device', ha, 'ok');
  // R4, R5: a status showing the replacement still pending past its anb fixes
  // H_pend for every entry issued at or before its as_of, so an add_device
  // there (H_act only) matches no hypothesis.
  const hp = await enrolled({ realm });
  await hp.op('replace_root');
  const rawHead = hp.fork();
  await hp.addDevice({ at: hp.s.anb + 10_000 });
  const stillPending = await rawHead.status({ asOf: hp.clock + 1_000, visible: hp.clock + 60_000 });
  await ledgerVec(ctx, 'ledger/status-pending-past-the-anb-refutes-an-add-device', hp, 'device_rule',
    { status: stillPending });


  // P1: private ops never move the published status (P1-a, P1-b, P1-f). The
  // statuses are signed afresh, so the comparison is over the message bytes.
  const p = await enrolled({ realm });
  const asOf = p.clock + 600_000;
  const before = await p.status({ asOf });
  const msg = b64e((await decode('signed', before)).msg);
  const p1 = async (name) => pushA2(ctx, { name, type: 'p1', input: { mode: 'status', other_b64: b64e(before) },
    bytes_b64: b64e(await p.status({ asOf })), expect: expectOf('ok', { reencoded_b64: msg }) });
  await p.addDevice({ at: p.clock + 60_000 });
  await p1('p1/a-b/add-device-leaves-the-status-unchanged');
  await p.bindRecovery({ wait: 'now' });
  await p1('p1/f/bind-recovery-leaves-the-status-unchanged');
}

BUILDERS.push(ledgerVectors);

// The ledger across calls. Each vector is a SECOND VerifyDeviceLedger call: its
// prev heads are what a first call persisted (callOne), its bundles only what
// came after, and its expectation what one call from genesis persists at the
// same status, so chaining calls persists exactly what a single call does
// (§2.12, D-269). A new-root activation an entry took is already in the
// persisted set (pending := "", DR5, DR8) and a later call must not apply it
// again; and the device-status co-signed with the from head's retained fix (R4)
// fixed the persisted slot pending at its as_of (RT6-9) or, when it showed an
// activation the persisted set stops before (device-head #7), that activation
// first (C4-4), for every later call too.

// callOne captures the heads a first call at status persists. raw holds the
// verifier's chain-head when a private op took the head's activation.
async function callOne(a, status, raw = a) {
  const chain = raw.expectedChainHead(status);
  return { raw, chain, device: await a.expectedDeviceHead(status, { chain }), bundles: a.bundles.length,
    dbundles: a.L.dbundles.length };
}

async function ledgerVec2(ctx, name, a, c1, reason, o = {}) {
  if (a.bundles.length !== c1.bundles) throw new Error(`generator bug: ${name}: a result after the first call`);
  const status = o.status ?? (await a.status(o.st));
  const ds = o.deviceStatus ?? (await a.deviceStatus(status, o.ds));
  const asOf = (await decode('status', (await decode('signed', status)).msg)).as_of_ms;
  let heads = {};
  if (reason === 'ok') {
    // The second call starts from c1's persisted head, whose fix is the first
    // call's status: R4's retention rule (retainedFix) decides which one stays.
    const prevFix = (await decode('chain-head', c1.chain)).fix;
    const chain = c1.raw.expectedChainHead(status, { prevFix });
    heads = { head_b64: b64e(chain), device_head_b64: b64e(await a.expectedDeviceHead(status, { chain })) };
  }
  await pushA2(ctx, { name, type: 'device-ledger', anchors: b64e(a.r.anchors), trust: a.r.items.map(b64e), hw: HW0,
    prev_head: b64e(c1.chain), prev_device_head: b64e(c1.device), bundles: [],
    device_bundles: a.L.dbundles.slice(c1.dbundles).map(b64e), bytes_b64: b64e(status), device_status: b64e(ds),
    mem: a.mem(), now_ms: asOf + 1_000, use: 'new-secret', expect: expectOf(reason, heads) });
}

async function ledgerAcrossCallsVectors(ctx) {
  const realm = await Realm.create();
  const setOf = (a, S) => sha384(encodeS(a.r.id, a.id, S));
  // A private bind after a recover's anb takes the Pr head's activation (it
  // needs the head active): the old key burned, a first bind. The first call
  // persists the activated set with the raw head, which no result records.
  const pr = await enrolled({ realm });
  await pr.bindRecovery({ wait: 'now' });
  await pr.op('recover');
  const prRaw = pr.fork();
  const at = pr.s.anb + 10_000;
  const prNow = pr.fork();
  await pr.bindRecovery({ wait: 'delta', at });
  const c1 = await callOne(pr, await pr.status(), prRaw);
  // The slot completes after the first call's as_of, which held it pending
  // only up to that instant (RT6-9).
  await ledgerVec2(ctx, 'ledger/across-calls/recover-activated-then-a-delta-bind-completing-later', pr, c1, 'ok',
    { st: { asOf: pr.L.S.nb + pr.L.slotG + 1_000 } });
  await prNow.bindRecovery({ wait: 'now', at });
  const c1n = await callOne(prNow, await prNow.status(), prRaw);
  await ledgerVec2(ctx, 'ledger/across-calls/recover-activated-then-a-now-bind', prNow, c1n, 'ok',
    { st: { asOf: prNow.clock + 120_000 } });
  // An RSK showing the activation applied again: the newly bound key burned,
  // with no entry that unbinds it, a set the authority cannot reach.
  const burned = await setOf(prNow, jsNormalize(prNow.L.S, { activate: true, burn: true }));
  await ledgerVec2(ctx, 'ledger/across-calls/the-activation-shown-again-burning-the-new-key', prNow, c1n,
    'device_status_mismatch', { st: { asOf: prNow.clock + 120_000 }, ds: { mut: (v) => { v.device_set_digest = burned; } } });
  // The same for a replace_root (P): applying its activation again would empty
  // the slot the owner filled after it.
  const pa = await enrolled({ realm });
  await pa.op('replace_root');
  const paRaw = pa.fork();
  await pa.bindRecovery({ wait: 'delta', at: pa.s.anb + 10_000 });
  const c1p = await callOne(pa, await pa.status(), paRaw);
  await ledgerVec2(ctx, 'ledger/across-calls/replace-root-activated-then-a-delta-bind', pa, c1p, 'ok',
    { st: { asOf: pa.clock + 120_000 } });

  // RT6-9 across calls: the first call's device-status showed the slot pending
  // at T1, past nb + g (the authority extended it, L.slotExt). A later entry
  // issued at or before T1 whose set shows the slot completed matches neither
  // hypothesis that status leaves (device_rule).
  const q = await enrolled({ realm });
  await q.op('replace_root');
  const qRaw = q.fork();
  await q.addDevice({ at: q.s.anb + 10_000 }); // H_act, for every later entry (R3)
  await q.bindRecovery({ wait: 'delta' });
  q.L.slotExt = 3_600_000;
  const tc = q.L.S.nb + q.L.slotG;
  const c1q = await callOne(q, await q.status({ asOf: tc + 600_000 }), qRaw);
  const early = q.fork();
  early.L.slotExt = 0;
  await early.addDevice({ at: tc });
  await ledgerVec2(ctx, 'ledger/across-calls/an-entry-before-a-status-showing-the-slot-pending-shows-it-completed', early, c1q,
    'device_rule', { st: { asOf: tc + 660_000 } });

  // C4-4 across calls: the first call's device-status showed the replacement's
  // activation come first and burn the still-pending slot, and the persisted set
  // stops before that activation (device-head #7). A later device-status showing
  // the slot completed first matches no hypothesis that order leaves.
  const s = await enrolled({ realm });
  await s.bindRecovery({ wait: 'now' });
  await s.bindRecovery({ wait: 'delta' });
  const bindIssued = s.L.S.nb - s.r.cur.v.delta_ms;
  await s.op('replace_root', { at: bindIssued + 1_000, asserted: bindIssued - 5_000 });
  const slotAt = s.L.S.nb + s.L.slotG;
  if (s.s.anb >= slotAt) throw new Error('generator bug: the replacement does not activate before the slot completes');
  s.clock = slotAt;
  const c1s = await callOne(s, await s.status());
  const completedFirst = await setOf(s, jsNormalize(jsNormalize(s.L.S, { complete: true }), { activate: true }));
  await ledgerVec2(ctx, 'ledger/across-calls/completion-first-after-a-status-showed-the-activation-first', s, c1s,
    'device_status_mismatch', { st: { asOf: slotAt + 600_000 }, ds: { mut: (v) => { v.device_set_digest = completedFirst; } } });
}

BUILDERS.push(ledgerAcrossCallsVectors);

// ── 9e. Device wraps (DoR §2.5 device-wrap and device-secrets, D-244) ─────
// sealWrap is the generator's own device-wrap, written from §2.5's text: a fresh
// ECDH P-384 epk, Z its 48-byte x-coordinate, K = HKDF-SHA-384(Z, salt
// device_entry_digest, info, 32), AES-256-GCM under a 12-byte nonce with the
// 265-byte AAD. Go's SealDeviceWrap is the second implementation; the vectors
// prove the two agree byte for byte through Go opening what Web Crypto sealed.
const WRAP_INFO = utf8('concord-device-wrap/v1\0');
const ascii = (s) => {
  if (!isUUID(s)) throw new Error(`generator bug: ${s} is not an ascii(36) id`);
  return utf8(s);
};

// wrapInfo is HKDF's info: label ‖ 0x00 ‖ realm(36) ‖ account(36) ‖ recipient(36) ‖ epk(97) ‖ dkx(97).
function wrapInfo(realm, account, recipient, epk, dkx) {
  const info = concat(WRAP_INFO, ascii(realm), ascii(account), ascii(recipient), epk, dkx);
  if (info.length !== 325) throw new Error(`generator bug: wrap info is ${info.length} bytes, not 325`);
  return info;
}

// wrapAAD is realm ‖ account ‖ recipient ‖ device_entry_digest ‖ epk ‖ nonce (265 bytes, §2.5).
function wrapAAD(realm, account, recipient, entry, epk, nonce) {
  const aad = concat(ascii(realm), ascii(account), ascii(recipient), entry, epk, nonce);
  if (aad.length !== 265) throw new Error(`generator bug: wrap AAD is ${aad.length} bytes, not 265`);
  return aad;
}

// sealWrap seals secrets (a device-secrets value) to recipient ({ id, dkx: { pt } })
// under the secrets entry entryDigest. Negative vectors use three hooks, each of
// which keeps the wrap otherwise consistent so that only the named rule refuses it:
// o.recipientId addresses the wrap, its info and its AAD to another device id (the
// key is still self's dkx); o.epk(real) replaces the epk bytes everywhere they are
// bound (info, AAD, the wrap) while Z still comes from the real ephemeral key, and
// is spliced into the encoded text because the codec refuses such a point; o.eph is
// the ephemeral ECDH key pair to use.
export async function sealWrap(realm, account, recipient, entryDigest, secrets, o = {}) {
  const dkx = recipient.dkx.pt;
  if (dkx.length !== 97 || dkx[0] !== 0x04) throw new Error('generator bug: recipient dkx is not an uncompressed point');
  const eph = o.eph ?? (await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-384' }, true, ['deriveBits']));
  const realEpk = new Uint8Array(await subtle.exportKey('raw', eph.publicKey));
  const epk = o.epk ? o.epk(realEpk) : realEpk;
  const rid = o.recipientId ?? recipient.id;
  const peer = await subtle.importKey('raw', dkx, { name: 'ECDH', namedCurve: 'P-384' }, false, []);
  const z = await subtle.deriveBits({ name: 'ECDH', public: peer }, eph.privateKey, 384);
  const hk = await subtle.importKey('raw', z, 'HKDF', false, ['deriveKey']);
  const k = await subtle.deriveKey({ name: 'HKDF', hash: 'SHA-384', salt: entryDigest, info: wrapInfo(realm, account, rid, epk, dkx) },
    hk, { name: 'AES-GCM', length: 256 }, false, ['encrypt']);
  const nonce = rnd(12);
  const ct = new Uint8Array(await subtle.encrypt({ name: 'AES-GCM', iv: nonce, tagLength: 128,
    additionalData: wrapAAD(realm, account, rid, entryDigest, epk, nonce) }, k, encode('device-secrets', secrets)));
  const w = encode('device-wrap', { realm_id: realm, account_id: account, recipient_device_id: rid,
    device_entry_digest: entryDigest, epk: realEpk, nonce, ct });
  if (equalBytes(epk, realEpk)) return w;
  const [from, to] = [`"${b64e(realEpk)}"`, `"${b64e(epk)}"`];
  if (text(w).split(from).length !== 2) throw new Error('generator bug: the epk is not exactly one position of the wrap');
  return raw(text(w).replace(from, to));
}

const pkcs8 = async (key) => new Uint8Array(await subtle.exportKey('pkcs8', key));
// dScalar is an ECDH P-384 private key's 48-byte scalar (recipient_dkx_d, V-09).
const dScalar = async (key) => new Uint8Array(Buffer.from((await subtle.exportKey('jwk', key)).d, 'base64url'));

async function rsaEk() {
  const kp = await subtle.generateKey({ name: 'RSA-OAEP', modulusLength: 4096, publicExponent: new Uint8Array([1, 0, 1]),
    hash: 'SHA-256' }, true, ['encrypt', 'decrypt']);
  return { kp, spki: new Uint8Array(await subtle.exportKey('spki', kp.publicKey)) };
}

// DoR §3.1 "Wraps" (DoR:1306): current; history (an older rotate_ek); the wrong
// recipient; an unknown entry; a substituted EK; root_pkcs8 present on a
// rotate_ek; a PKCS#8 whose optional publicKey field differs from d·G; an
// off-curve epk; a 0x06 hybrid point. Plus the add_device entry that admits self
// (check 1's first kind of secrets entry, opened as history) and the device-ledger
// vector that produces the head every wrap here is opened against, so the secrets
// records are the ones VerifyDeviceLedger writes (device-head #10).
async function wrapVectors(ctx) {
  const a = await Account.create(await Realm.create());
  // The model keeps roots as keys (a.s.root) but EKs only as SPKIs: the wraps
  // need the EK private keys, so every EK this chain binds is a real RSA-OAEP key
  // kept here, by SPKI. The device-head records stay exactly what recordSecrets
  // writes; the sealer reads them and looks the keys up.
  const ekOf = new Map();
  const bindEk = async () => {
    const x = await rsaEk();
    ekOf.set(b64e(x.spki), x.kp.privateKey);
    return x.spki;
  };
  await a.op('enroll_new', { spki: await bindEk() });
  const d0 = a.L.devs.get(a.L.self);
  const rootOf = new Map([[b64e(a.s.root.pt), a.s.root]]);
  // The own device under test is self from before its admission, so its
  // add_device is recorded as one of self's secrets entries (§2.5 check 1: "an
  // add_device admitting self"); d0, the only mature device, acts.
  const nd = await newDevice();
  a.L.self = nd.id;
  await a.addDevice({ newDev: nd, at: a.clock + 60_000 });
  await a.op('rotate_ek', { spki: await bindEk(), actorDev: d0 });
  await a.op('rotate_root', { spki: await bindEk(), actorDev: d0 });
  rootOf.set(b64e(a.s.root.pt), a.s.root);
  const recs = a.L.secrets.map((s) => {
    const [entry, op, root, spki] = s.split('.');
    return { entry: b64d(entry), op, root: root ? rootOf.get(root) : null, ek: ekOf.get(spki) };
  });
  if (recs.map((r) => r.op).join() !== 'add_device,rotate_ek,rotate_root' ||
      recs.some((r) => !r.ek || (r.op === 'rotate_ek') === !!r.root)) {
    throw new Error('generator bug: self\'s secrets entries are not add_device, rotate_ek, rotate_root with their keys');
  }
  const [added, rotEk, latest] = recs;

  const st = await a.status();
  await ledgerVec(ctx, 'wrap/head-from-verified-ledger', a, 'ok', { status: st });
  const dh = await a.expectedDeviceHead(st);
  const ch = a.expectedChainHead(st);
  const dkxD = b64e(await dScalar(nd.dkx.k.privateKey));
  const push = (name, wrap, reason, use) => pushA2(ctx, { name, type: 'device-wrap-open', bytes_b64: b64e(wrap),
    prev_device_head: b64e(dh), prev_head: b64e(ch), recipient_dkx_d: dkxD,
    expect: expectOf(reason, use ? { wrap_use: use } : {}) });
  const seal = (r, secrets, o) => sealWrap(a.r.id, a.id, nd, r.entry, secrets, o);
  const sec = async (r, o = {}) => ({ root_pkcs8: o.root ?? (r.root ? await pkcs8(r.root.k.privateKey) : E0),
    ek_pkcs8: o.ek ?? (await pkcs8(r.ek)) });

  await push('wrap/current', await seal(latest, await sec(latest)), 'ok', 'current');
  await push('wrap/history', await seal(rotEk, await sec(rotEk)), 'ok', 'history');
  await push('wrap/history-add-device', await seal(added, await sec(added)), 'ok', 'history');
  // Sealed to self's dkx, addressed (wrap, info and AAD) to d0: only check 1's recipient refuses.
  await push('wrap/wrong-recipient', await seal(latest, await sec(latest), { recipientId: d0.id }), 'wrap_invalid');
  await push('wrap/unknown-entry', await seal({ ...latest, entry: rnd(48) }, await sec(latest)), 'wrap_invalid');
  // Every private key a wrap here holds is fresh and this account's alone,
  // never one the file publishes elsewhere (A1's cached ek() signs every
  // legacy PoP): recipient_dkx_d opens these ciphertexts, and
  // TestWrapVectorKeysAreSingleUseTestKeys refuses a key any other vector uses.
  await push('wrap/substituted-ek', await seal(latest, await sec(latest, { ek: await pkcs8((await rsaEk()).kp.privateKey) })),
    'wrap_invalid');
  await push('wrap/root-pkcs8-on-rotate-ek', await seal(rotEk, await sec(rotEk, { root: await pkcs8(latest.root.k.privateKey) })),
    'wrap_invalid');
  // Another key's scalar, with the entry's root in PKCS#8's optional publicKey
  // field: Node's P-384 PKCS#8 ends with BIT STRING 00 ‖ the 97-byte point.
  const lying = await pkcs8((await wKey()).k.privateKey);
  if (lying[lying.length - 98] !== 0x00 || lying[lying.length - 97] !== 0x04) {
    throw new Error('generator bug: a P-384 PKCS#8 does not end with its uncompressed public point');
  }
  lying.set(latest.root.pt, lying.length - 97);
  await push('wrap/pkcs8-publickey-field-lies', await seal(latest, await sec(latest, { root: lying })), 'wrap_invalid');
  const offCurve = (p) => {
    const q = p.slice();
    q[96] ^= 1;
    try {
      checkPoint(q);
    } catch {
      return q;
    }
    throw new Error('generator bug: flipping y\'s low bit left the epk on the curve');
  };
  await push('wrap/epk-off-curve', await seal(latest, await sec(latest), { epk: offCurve }), 'wrap_invalid');
  // A valid hybrid encoding: prefix 0x06 needs an even y (0x07 is the odd form).
  let eph;
  do {
    eph = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-384' }, true, ['deriveBits']);
  } while (new Uint8Array(await subtle.exportKey('raw', eph.publicKey))[96] & 1);
  const hybrid = (p) => Uint8Array.of(0x06, ...p.subarray(1));
  await push('wrap/epk-hybrid-prefix', await seal(latest, await sec(latest), { eph, epk: hybrid }), 'wrap_invalid');

  // For the TS port (E-N3, E-N4): each wrap below opens, names a real secrets
  // entry of self, and fails exactly one rule.
  // Check 2: an add_device and a rotate_root entry each require root_pkcs8.
  await push('wrap/add-device-without-root', await seal(added, await sec(added, { root: E0 })), 'wrap_invalid');
  await push('wrap/rotate-root-without-root', await seal(latest, await sec(latest, { root: E0 })), 'wrap_invalid');
  // Check 3: the root must be a P-384 key and the EK an RSA key.
  const p256 = await subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign']);
  await push('wrap/root-pkcs8-p256', await seal(latest, await sec(latest, { root: await pkcs8(p256.privateKey) })), 'wrap_invalid');
  await push('wrap/ek-pkcs8-not-rsa', await seal(latest, await sec(latest, { ek: await pkcs8((await wKey()).k.privateKey) })),
    'wrap_invalid');
  // Check 3: one byte after the root's PKCS#8 element. Go's x509 and Node's
  // Web Crypto both ignore it unless the verifier refuses it explicitly (E-N4).
  const rootDer = await pkcs8(latest.root.k.privateKey);
  await push('wrap/pkcs8-trailing-bytes', await seal(latest, await sec(latest, { root: concat(rootDer, Uint8Array.of(0)) })),
    'wrap_invalid');
  // Check 3, NEW-1: the entry's own scalar in a PKCS#8 that is not its key's
  // canonical encoding. Go's x509 accepts all three, Web Crypto refuses the
  // lying point and the inner curve at import, and a port compares the input
  // with exportKey(importKey(der)). Node's P-384 PKCS#8 is 185 bytes: outer
  // SEQUENCE 30 81 b6, OCTET STRING 04 81 9e at 24, ECPrivateKey 30 81 9b at
  // 27, its publicKey [1] at 83 (no inner parameters).
  const h8 = Buffer.from(rootDer).toString('hex');
  if (rootDer.length !== 185 || !h8.startsWith('3081b6') || h8.slice(48, 60) !== '04819e30819b' || h8.slice(166, 170) !== 'a164') {
    throw new Error('generator bug: a P-384 PKCS#8 is not the 185-byte layout NEW-1 edits');
  }
  const ownLies = rootDer.slice();
  ownLies.set((await wKey()).pt, ownLies.length - 97);
  await push('wrap/pkcs8-own-scalar-publickey-field-lies', await seal(latest, await sec(latest, { root: ownLies })), 'wrap_invalid');
  const innerTail = concat(rootDer, Uint8Array.of(0)); // a byte after the ECPrivateKey, inside the OCTET STRING
  innerTail[2] = 0xb7;
  innerTail[26] = 0x9f;
  await push('wrap/pkcs8-bytes-after-the-ec-private-key', await seal(latest, await sec(latest, { root: innerTail })),
    'wrap_invalid');
  const p256Params = concat(rootDer.subarray(0, 83), Uint8Array.from(Buffer.from('a00a06082a8648ce3d030107', 'hex')),
    rootDer.subarray(83)); // [0] prime256v1 inside a P-384 PrivateKeyInfo
  p256Params[2] = 0xc2;
  p256Params[26] = 0xaa;
  p256Params[29] = 0xa7;
  await push('wrap/pkcs8-inner-curve-p256', await seal(latest, await sec(latest, { root: p256Params })), 'wrap_invalid');
  // The AEAD: the GCM tag's last byte flipped in an otherwise valid wrap.
  const good = await decode('device-wrap', await seal(latest, await sec(latest)));
  const ct = good.ct.slice();
  ct[ct.length - 1] ^= 1;
  await push('wrap/gcm-tag-tampered', encode('device-wrap', { ...good, ct }), 'wrap_invalid');
  // The entry binding (salt and AAD): rotate_ek's secrets sealed under the latest
  // entry's digest, then named rotate_ek's. Check 1 finds that entry and checks
  // 2–3 would accept those secrets for it, so only the binding refuses the wrap.
  const moved = await decode('device-wrap', await seal({ ...rotEk, entry: latest.entry }, await sec(rotEk)));
  await push('wrap/entry-digest-swapped-to-another-own-entry',
    encode('device-wrap', { ...moved, device_entry_digest: rotEk.entry }), 'wrap_invalid');
}

BUILDERS.push(wrapVectors);

// ── 9f. Paused verdicts no other builder reaches (DoR §3.1) ─────────────
// DoR §3.1 (DoR:1291): "every Reason in §2.12 has at least one vector". These
// are the members of §2.12's paused family (DoR:882) that no transition group
// ends on. Each vector is the status alone, verified against the head the
// account already holds (no bundles), so only the §2.13 verdict on the
// status's state decides it. The rest of the family, and the continuity
// `ok`, already have vectors: row/02/pop-pending (continuity ok),
// row/02/pop-disputed, row/03/no-pop-recovery, pin/revoked,
// status/pending-from-active, d136/held-pending-anb-never,
// sets/operator-suspend-unsuspend-from-su and
// ledger/row-18-erase-keeps-the-pending-device-and-the-slot (erased).
async function stateVerdictVectors(ctx) {
  const realm = await Realm.create({ del: { ops: [await wKey()] } });
  const statusOnly = (name, a, reason) => chainVec(ctx, name, a, reason, { from: a.bundles.length });
  // Row 18 from Ac: Pe(active), a pending erase (DoR:602), no pending root.
  let a = await enrolled({ realm });
  await a.op('erase');
  await statusOnly('status/pending-erase', a, 'pending_erase');
  // Row 15 from Ac: Sor, suspended with root_revoked = 1 (DoR:599).
  a = await enrolled({ realm });
  await a.op('revoke_root', { actor: 'operator' });
  await statusOnly('status/root-revoked', a, 'root_revoked');
  // Row 7, then row 11 from Su: P(suspended), a new root pending.
  a = await enrolled({ realm });
  await a.op('suspend');
  await a.op('replace_root');
  await statusOnly('status/pending-from-suspended', a, 'pending_from_suspended');
}

BUILDERS.push(stateVerdictVectors);

// ── 9g. Fix retention, the trust's realm, the first-sight pin (PR-A2) ────
// R4's retention rule (retainedFix; red-team RT-1, RT-1b): each r4/retain
// vector starts from the head an earlier call persisted for a raw pending
// head (fix = the earlier status) and verifies a later status alone, so its
// expect.head_b64 pins which status the head keeps, in Go and Node alike.
// Then review B-SF1 (DoR:715 T3, :925, D-115): a status signed by the trust's
// own RSK but naming another realm, against that realm's head, is
// realm_mismatch. Then red-team RT-2 (D-149, DoR:943): past genesis a head
// pin other than the caller's first-sight pin is pin_changed.
async function fixRetentionVectors(ctx) {
  const realm = await Realm.create();
  const p = await enrolled({ realm });
  await p.op('replace_root');
  const anb = p.s.anb;
  const at = (asOf, o = {}) => p.status({ asOf, ...o });
  const act = await at(anb + 60_000); // shows the head activated
  const pend = await at(anb - 60_000); // shows it pending, not held
  const cases = [
    ['activation-then-held', act, await at(anb + 120_000, { held: 1 }), 'held'],
    ['activation-then-later-activation', act, await at(anb + 120_000), 'ok'],
    ['activation-then-earlier-activation', act, await at(anb + 1_000), 'ok'],
    // A held fix newer than the activation that arrives after it: only the
    // activation-over-pending arm replaces it (by as_of alone it would stay).
    ['held-then-earlier-activation', await at(anb + 120_000, { held: 1 }), await at(anb + 1_000), 'ok'],
    ['pending-then-newer-held', pend, await at(anb + 1_000, { held: 1 }), 'held'],
    ['pending-then-older-pending', pend, await at(anb - 120_000), 'pending_from_active'],
  ];
  for (const [name, old, status, reason] of cases) {
    await chainVec(ctx, `r4/retain/${name}`, p, reason, { from: p.bundles.length, prevHead: p.expectedChainHead(old),
      prevFix: old, status, now: statusR4Of(status).asOf + 1_000, head: true });
  }

  const other = '11111111-1111-4111-8111-ffffffffffff';
  const g = await Account.create(realm, { pin: true });
  await chainVec(ctx, 'realm/status-names-another-realm', g, 'realm_mismatch', {
    prevHead: encode('chain-head', { ...(await decode('chain-head', g.heads[0])), realm_id: other }), bundles: [],
    status: await g.status({ mut: (v) => { v.realm_id = other; } }) });

  const f = await enrolled({ realm }); // row 1: the chain's pin is ""
  await chainVec(ctx, 'pin/first-sight-pin-past-genesis', f, 'pin_changed',
    { from: f.bundles.length, mem: f.mem({ pin: b64e(await sha384(rnd(32))) }) });

  // NEW-2 (DoR:924, :943): a hold does not hide a changed pin; held carries no
  // notice, pin_changed does.
  const heldSt = await at(anb + 1_000, { held: 1 });
  await chainVec(ctx, 'pin/changed-while-held', p, 'pin_changed', { from: p.bundles.length,
    mem: p.mem({ pin: b64e(await sha384(rnd(32))) }), status: heldSt, now: statusR4Of(heldSt).asOf + 1_000 });

  // N-2 (R4 on a status; DoR:738, :2106): once the retained fix shows the raw
  // pending head activated, a status showing it pending again (newer with an
  // extended anb, or older), not held, does not undo it: the fix decides and
  // the head reads as activated. Read as pending, the continuity head wrapped
  // to the legacy key again (Go pins the wrap target). Not refused: DoR step 7
  // lets an honest visible instant move past an activated status (A6).
  const again = async (a, name, act, later) => chainVec(ctx, name, a, 'ok',
    { from: a.bundles.length, prevHead: a.expectedChainHead(act), status: later, now: statusR4Of(later).asOf + 1_000 });
  await again(p, 'r4/status/pending-after-a-shown-activation', act, await at(anb + 120_000, { visible: anb + 600_000 }));
  const c = await Account.create(realm, { pin: true });
  await c.op('enroll_existing', { pop: true }); // continuity pending from absent
  const cAnb = c.s.anb;
  const cAct = await c.status({ asOf: cAnb + 60_000 });
  await again(c, 'r4/status/continuity-pending-after-a-shown-activation', cAct,
    await c.status({ asOf: cAnb + 120_000, visible: cAnb + 600_000 }));
  await again(c, 'r4/status/continuity-older-pending-than-a-shown-activation', cAct, await c.status({ asOf: cAnb - 60_000 }));
  await again(c, 'r4/status/continuity-activated-after-a-shown-activation', cAct, await c.status({ asOf: cAnb + 120_000 }));
}

BUILDERS.push(fixRetentionVectors);

async function main(argv) {
  if (argv.includes('--check')) {
    await checkDrift();
    return;
  }
  const { body, count, draws } = await buildSafeVectorFile();
  const bytes = Buffer.byteLength(body);
  // 8 MiB, raised from the plan's 4 MiB by a recorded decision (PR-A2, 2026-10-09):
  // the DoR's single committed path holds A2's full set. Over it, stop and report.
  if (bytes > 8 * 1024 * 1024) {
    throw new Error(`vectors.json would be ${bytes} bytes, over the 8 MiB budget: stop and report (PR-A2 contract doc, "As built")`);
  }
  writeFileSync(VECTORS_PATH, body);
  console.error(`wrote ${count} vectors in ${draws} draw(s); runtime-only boundaries: ${RUNTIME_ONLY.join(', ') || 'none'}`);
}

// Node real-paths the main module for import.meta.url but not process.argv[1], so
// comparing the two verbatim is false when the invocation path holds a symlinked
// directory (macOS /tmp, a checkout behind a link) and main() would silently never
// run: the pre-commit drift check would exit 0 without checking. Compare real paths.
function isEntryPoint() {
  const entry = process.argv[1];
  if (!entry) return false;
  try {
    return realpathSync(entry) === fileURLToPath(import.meta.url);
  } catch {
    return false; // argv[1] is not a file, so this module was imported, not run
  }
}

if (isEntryPoint()) {
  main(process.argv.slice(2)).catch((e) => {
    console.error(e.message);
    process.exit(1);
  });
}
