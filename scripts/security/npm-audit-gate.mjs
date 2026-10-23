// npm-audit gate — pure decision logic + CLI entry.
//
// Why this exists: `npm audit --audit-level=high` as a bare CI gate is
// time-brittle. `npm audit` queries the LIVE advisory database at run time, so
// a newly-published HIGH/CRITICAL advisory red-gates EVERY PR (and main) the
// moment it lands — including advisories for which no fixed version exists yet,
// leaving authors with no way to make CI green. Founding incident 2026-06-15:
// `ws` GHSA-96hv-2xvq-fx4p published at 16:34Z flipped all CI red with zero
// code change (see [internal]quality-gate-definition.md "npm-audit gate").
//
// This gate keeps the teeth (un-accepted HIGH/CRITICAL advisories still fail
// the build, forcing a dependency bump) while adding a tracked exceptions list:
// an operator may ACCEPT a specific advisory (with a reason + review date) to
// unblock when a fix is not yet available, instead of being stuck on upstream.
//
// Hard guarantee: the wall-clock ALONE never blocks CI. The only thing that
// fails the gate is a HIGH/CRITICAL advisory that is NOT in the allowlist. An
// expired `review_by` produces a loud warning, never a block — re-blocking on
// the clock would reintroduce the exact brittleness this gate removes.
//
// An exception is SCOPED, not a standing waiver on an advisory id. Matching on
// the GHSA id alone granted far more than the reviewed `reason` covered: an
// advisory is amended in place upstream (its affected-package list and version
// range both widen), and the same id reappears when a package moves from a
// dev-only corner of the tree onto a runtime path. So `package` is REQUIRED and
// binds the waiver to the package it was written for, and an optional
// `"scope": "dev"` is VERIFIED against a production-only (`npm audit
// --omit=dev`) report rather than taken on trust — a dev-time exception stops
// applying the moment the dependency becomes runtime-reachable. Both checks
// fail closed: an unscoped entry, a package mismatch, or a dev-scoped entry
// with no production report to check against all BLOCK.
//
// No third-party dependency (stdlib `node:` only) — mirrors the no-new-deps
// preference codified for the SSRF guard in [internal]rules/backend.md.

import { readFileSync, realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const GHSA_RE = /GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}/i;

/**
 * Is this a real `npm audit --json` result we can draw conclusions from?
 *
 * The distinction that matters is "no advisories" vs "we do not know". npm
 * writes a valid-JSON `{ "error": ... }` object to stdout on registry / network
 * / auth failure, and a truncated report can lack `vulnerabilities` entirely.
 * Both parse; neither tells you anything about the dependency graph. Reading
 * either as "0 advisories" is how a security gate passes vacuously, so every
 * consumer must route its report through here before trusting an empty result.
 *
 * @param {unknown} a parsed audit JSON
 * @returns {boolean}
 */
export function isUsableAuditReport(a) {
  return !!a && typeof a === 'object' && !a.error && typeof a.vulnerabilities === 'object';
}

/**
 * Extract the set of distinct HIGH/CRITICAL advisories from an
 * `npm audit --json` report. Only object-form `via` entries are real
 * advisories; string-form `via` entries are transitive chain references whose
 * advisory is counted at the dependency that actually carries it.
 *
 * @param {object} audit parsed `npm audit --json` output
 * @returns {Array<{id:string, severity:string, title:string, package:string, url:string}>}
 */
export function extractHighCriticalAdvisories(audit) {
  const found = new Map();
  const vulns = audit?.vulnerabilities ?? {};
  for (const node of Object.values(vulns)) {
    for (const via of node?.via ?? []) {
      if (!via || typeof via !== 'object' || typeof via.url !== 'string') continue;
      const severity = String(via.severity ?? '').toLowerCase();
      if (severity !== 'high' && severity !== 'critical') continue;
      const m = GHSA_RE.exec(via.url);
      // Canonical key is the GHSA id. If a non-GHSA advisory url ever appears,
      // fall back to the full url as the dedupe + allowlist key — an operator
      // accepting such an advisory must list that exact url in the allowlist.
      const id = m ? m[0].toUpperCase() : via.url;
      if (!found.has(id)) {
        found.set(id, {
          id,
          severity,
          title: String(via.title ?? ''),
          package: String(via.name ?? node?.name ?? ''),
          url: via.url,
        });
      }
    }
  }
  return [...found.values()];
}

/**
 * Decide gate outcome.
 *
 * @param {object} audit parsed `npm audit --json` output
 * @param {object} allowlist `{ accepted: [{ id, package, reason, review_by?, scope? }] }`
 * @param {string} [nowISO] ISO date used for expiry comparison (defaults to real now)
 * @param {object} [prodAudit] parsed `npm audit --omit=dev --json` output, used to
 *   VERIFY a `"scope": "dev"` claim. Omitting it makes every dev-scoped entry
 *   block (fail closed) — an unverifiable claim must never pass a security gate.
 * @returns {{advisories:Array, blocked:Array, allowed:Array, expired:Array}}
 */
export function evaluate(audit, allowlist, nowISO, prodAudit) {
  // Fail closed: a report that is not a real `npm audit --json` result — an
  // `{ error: ... }` object emitted on registry/network/auth failure, or a
  // truncated report missing `vulnerabilities` — must NOT be read as
  // "0 advisories => pass". Treat it as a hard error so the caller blocks
  // (exit 2) rather than silently passing a security gate.
  if (!isUsableAuditReport(audit)) {
    throw new Error('audit report missing `vulnerabilities` key (npm audit likely failed) — refusing to pass');
  }
  const advisories = extractHighCriticalAdvisories(audit);

  const accepted = new Map();
  for (const e of allowlist?.accepted ?? []) {
    if (e && typeof e.id === 'string') accepted.set(e.id.toUpperCase(), e);
  }

  // Advisory ids present in the PRODUCTION-only dependency graph. `null` means
  // we have no TRUSTWORTHY production report, which is emphatically not the
  // same as an empty one — see the dev-scope check below, which fails closed on
  // `null` and passes only on a genuine, verified absence.
  //
  // The usability check here is load-bearing, not defensive tidiness. An
  // `{ error: ... }` report (what npm writes to stdout on a registry/network
  // failure) is truthy and parses fine, but has no `vulnerabilities` key — so
  // feeding it to extractHighCriticalAdvisories yields an EMPTY set, and every
  // dev-scoped entry would read as "absent from the production graph" and pass.
  // That is fail-OPEN in the one mechanism whose whole job is to fail closed:
  // a transient npm outage would silently un-gate every dev-scoped exception.
  const prodUsable = prodAudit === undefined || prodAudit === null
    ? 'absent'
    : (isUsableAuditReport(prodAudit) ? 'usable' : 'unusable');
  const prodIds = prodUsable === 'usable'
    ? new Set(extractHighCriticalAdvisories(prodAudit).map((a) => a.id.toUpperCase()))
    : null;

  const now = nowISO ? Date.parse(nowISO) : Date.now();
  const blocked = [];
  const allowed = [];
  const expired = [];

  for (const a of advisories) {
    const entry = accepted.get(a.id.toUpperCase());
    if (!entry) {
      blocked.push(a);
      continue;
    }
    // An entry with no `package` cannot be checked against what npm actually
    // reported, so it does not match anything. Fail closed.
    if (typeof entry.package !== 'string' || entry.package.trim() === '') {
      blocked.push({ ...a, mismatch: `allowlist entry ${entry.id} has no "package" field — add "package": "${a.package}" to scope the exception` });
      continue;
    }
    if (entry.package.trim().toLowerCase() !== String(a.package).trim().toLowerCase()) {
      blocked.push({ ...a, mismatch: `allowlist entry ${entry.id} is scoped to package "${entry.package}" but npm reported it for "${a.package}"` });
      continue;
    }
    // A dev-scoped waiver is void the moment the same advisory appears in the
    // production graph. With no production report we cannot check the claim, so
    // it blocks rather than passing vacuously.
    if (String(entry.scope ?? '').toLowerCase() === 'dev') {
      if (!prodIds) {
        const why = prodUsable === 'unusable'
          ? 'the production-only audit report was unusable (npm likely failed), so the claim could not be verified'
          : 'no production-only audit report was supplied to verify it';
        blocked.push({ ...a, mismatch: `allowlist entry ${entry.id} claims scope "dev" but ${why}` });
        continue;
      }
      if (prodIds.has(a.id.toUpperCase())) {
        blocked.push({ ...a, mismatch: `allowlist entry ${entry.id} claims scope "dev" but ${a.package} is reachable in the PRODUCTION dependency graph` });
        continue;
      }
    }
    const enriched = { ...a, reason: entry.reason ?? '', review_by: entry.review_by ?? '' };
    allowed.push(enriched);
    if (entry.review_by) {
      const rb = Date.parse(entry.review_by);
      // Expiry is WARN-only by design — the clock must never block CI.
      if (!Number.isNaN(rb) && !Number.isNaN(now) && rb < now) expired.push(enriched);
    }
  }

  return { advisories, blocked, allowed, expired };
}

/**
 * Scan an allowlist for entries whose `review_by` date has passed — regardless
 * of whether the advisory is still reported by `npm audit`. Used by the weekly
 * audit-allowlist-expiry workflow to surface stale exceptions that should be
 * removed or refreshed. This is distinct from `evaluate`'s `expired` (which
 * only flags currently-reported advisories).
 *
 * @param {object} allowlist `{ accepted: [{ id, reason, review_by? }] }`
 * @param {string} [nowISO] ISO date (defaults to real now)
 * @returns {Array<{id:string, reason:string, review_by:string}>}
 */
export function findExpiredExceptions(allowlist, nowISO) {
  const now = nowISO ? Date.parse(nowISO) : Date.now();
  const expired = [];
  for (const e of allowlist?.accepted ?? []) {
    if (!e || typeof e.id !== 'string' || !e.review_by) continue;
    const rb = Date.parse(e.review_by);
    if (!Number.isNaN(rb) && !Number.isNaN(now) && rb < now) {
      expired.push({ id: e.id, reason: e.reason ?? '', review_by: e.review_by });
    }
  }
  return expired;
}

// ── CLI entry ──────────────────────────────────────────────────────────────
// Gate mode:    node npm-audit-gate.mjs --audit <audit.json> --allowlist <a.json> [--audit-prod <prod.json>] [--now <ISO>]
//   --audit-prod is an `npm audit --omit=dev --json` report. It is what makes a
//   `"scope": "dev"` allowlist entry verifiable; without it those entries block.
//   Exit 0 = pass, 1 = blocked advisory present, 2 = usage / parse / unusable-report error.
// Expiry scan:  node npm-audit-gate.mjs --expiry-scan --allowlist <a.json> [--allowlist <b.json> ...] [--now <ISO>]
//   Exit 0 = none expired, 1 = at least one expired. Used by the WEEKLY
//   audit-allowlist-expiry workflow, NOT by PR CI — the wall-clock never blocks PR gates.

function parseArgs(argv) {
  const out = { allowlists: [] };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--audit') out.audit = argv[++i];
    else if (a === '--audit-prod') out.auditProd = argv[++i];
    else if (a === '--allowlist') out.allowlists.push(argv[++i]);
    else if (a === '--now') out.now = argv[++i];
    else if (a === '--expiry-scan') out.expiryScan = true;
  }
  return out;
}

function readJson(path, label) {
  let raw;
  try {
    raw = readFileSync(path, 'utf8');
  } catch (err) {
    process.stderr.write(`npm-audit-gate: cannot read ${label} at ${path}: ${(err instanceof Error ? err.message : String(err))}\n`);
    process.exit(2);
  }
  try {
    return JSON.parse(raw);
  } catch (err) {
    process.stderr.write(`npm-audit-gate: ${label} at ${path} is not valid JSON: ${(err instanceof Error ? err.message : String(err))}\n`);
    process.exit(2);
  }
}

const log = (s) => process.stdout.write(`${s}\n`);

function runGate(args) {
  if (!args.audit || args.allowlists.length === 0) {
    process.stderr.write('npm-audit-gate: --audit <file> and --allowlist <file> are required\n');
    process.exit(2);
  }
  const audit = readJson(args.audit, 'audit report');
  const allowlist = readJson(args.allowlists[0], 'allowlist');
  const auditProd = args.auditProd ? readJson(args.auditProd, 'production-only audit report') : undefined;

  let result;
  try {
    result = evaluate(audit, allowlist, args.now, auditProd);
  } catch (err) {
    // Fail closed: an unusable audit report blocks the build (exit 2) rather
    // than passing a security gate on a malformed/error report.
    log(`::error::npm-audit-gate: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(2);
  }
  const { advisories, blocked, allowed, expired } = result;

  log(`npm-audit gate: ${advisories.length} HIGH/CRITICAL advisory(ies); ${allowed.length} accepted, ${blocked.length} blocking.`);
  for (const a of allowed) {
    log(`  accepted  ${a.severity.toUpperCase().padEnd(8)} ${a.id} (${a.package}) — ${a.reason || 'no reason recorded'}`);
  }
  for (const a of expired) {
    // GitHub Actions annotation — surfaced but non-blocking.
    log(`::warning::npm-audit exception ${a.id} (${a.package}) passed its review_by ${a.review_by}; re-evaluate and bump or refresh the exception.`);
  }
  for (const a of blocked) {
    // When an exception existed but did not apply, say WHY — otherwise the
    // operator sees a blocked advisory they believe is already allowlisted.
    const why = a.mismatch ? ` [exception NOT applied: ${a.mismatch}]` : '';
    log(`::error::npm-audit HIGH/CRITICAL advisory ${a.id} (${a.package}) — ${a.title}.${why} ${a.url}`);
  }

  if (blocked.length > 0) {
    log('');
    log('Gate FAILED. To resolve, either:');
    log('  1. Bump the affected dependency (prefer an `overrides` entry in package.json), or');
    log('  2. If no fix is available yet, add the advisory id to the package\'s .audit-allowlist.json');
    log('     `accepted` array with a `reason` and a `review_by` date.');
    process.exit(1);
  }
  log('Gate passed.');
  process.exit(0);
}

function runExpiryScan(args) {
  if (args.allowlists.length === 0) {
    process.stderr.write('npm-audit-gate: --expiry-scan requires at least one --allowlist <file>\n');
    process.exit(2);
  }
  let expiredTotal = 0;
  for (const path of args.allowlists) {
    const allowlist = readJson(path, 'allowlist');
    for (const e of findExpiredExceptions(allowlist, args.now)) {
      expiredTotal += 1;
      log(`::warning::${path}: exception ${e.id} passed its review_by ${e.review_by} — re-evaluate, bump the dep, or refresh the date. (${e.reason || 'no reason recorded'})`);
    }
  }
  if (expiredTotal > 0) {
    log(`audit-allowlist expiry scan: ${expiredTotal} expired exception(s) need review.`);
    process.exit(1);
  }
  log('audit-allowlist expiry scan: no expired exceptions.');
  process.exit(0);
}

function main() {
  const args = parseArgs(process.argv.slice(2));
  if (args.expiryScan) runExpiryScan(args);
  else runGate(args);
}

// Robust direct-invocation guard. The string idiom
// `import.meta.url === \`file://${process.argv[1]}\`` silently mis-compares for
// symlinked / spaced / non-ASCII paths — a security gate that no-ops on a
// symlinked runner would pass vacuously — so canonicalize both sides.
function invokedDirectly() {
  if (!process.argv[1]) return false;
  try {
    return realpathSync(process.argv[1]) === fileURLToPath(import.meta.url);
  } catch {
    return false;
  }
}

if (invokedDirectly()) {
  main();
}
