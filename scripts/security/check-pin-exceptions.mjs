#!/usr/bin/env node
//
// pin-exception lint — every deliberately-held dependency version must be
// written down, and every written-down reason must have an expiry.
//
// WHY THIS EXISTS
//
// Three mechanisms in this repo can hold a dependency below what is available:
// an exact version in a manifest, a `ignore:` rule in .github/dependabot.yml,
// and (separately) an `overrides` entry. Each was correct when written. Each
// stops being correct silently, because nothing re-evaluates the reason — the
// tree moves, the advisory gets patched upstream, the incompatibility gets
// fixed, and the pin stays.
//
// That pattern has now cost this repo three times in one week:
//   - a `undici: ^7.29.0` override forced jsdom@30 a major below its own
//     declared range and surfaced as a deadlocked test (PR #2709),
//   - a `tar: ^7.5.16` override sat on 7.5.19, itself VULNERABLE to
//     GHSA-r292-9mhp-454m, while the audit gate graded it moderate and passed,
//   - a `postcss: ^8.5.18` override held vite below the ^8.5.25 it declares.
//
// Sibling controls cover the neighbouring cases: `.audit-allowlist.json` for
// accepted advisories, `.override-range-baseline.json` for override skew. This
// one covers the remaining gap — a version we are choosing not to take.
//
// WHAT IT CHECKS
//   1. every exact npm pin in a covered workspace has a register entry
//   2. every dependabot `ignore:` rule has a register entry
//   3. every entry is well-formed and carries a review_by
//   4. entries past review_by WARN (they never block — a date alone must not
//      red-gate CI, the same posture as the npm-audit gate's expiry scan)
//
// Exit: 0 = pass, 1 = undocumented pin or malformed entry, 2 = usage/parse error.

import { readFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { realpathSync } from 'node:fs';
import { resolve as resolvePath, join } from 'node:path';

const REGISTER = '.github/dependency-pin-exceptions.json';
const DEPENDABOT = '.github/dependabot.yml';
// Every npm directory `.github/dependabot.yml` watches. Keep in lockstep with it:
// a directory Dependabot proposes bumps for but this list omits gets NO pin
// coverage, while the run still prints "every pin and ignore accounted for" — a
// claim about the repository that is true only of the directories listed here.
// That is exactly what happened to the two Cloudflare Workers: both exact-pin
// `wrangler`, neither had a register entry, and the control passed (#3292).
// `assertWorkspacesCoverDependabot` below fails when the two lists drift.
// Exported so the test suite can BUILD its fixtures from this list rather than
// hardcoding a parallel copy. A hardcoded copy is what made the drift assertion
// break all nine ignore-rule cases on first run: minimal fixtures declared one npm
// directory while this list named five, so `stale` fired on every case. Deriving
// means a sixth workspace never breaks the suite — and never silently un-tests it.
export const WORKSPACES = [
  '.github/tools/license-checker',
  // Self-Hosted: the root workspace (packages/*) takes the desktop slot.
  '.',
  'client/admin',
  'services/media-plane',
];
const REQUIRED = ['kind', 'ecosystem', 'workspace', 'package', 'pinned', 'reason', 'review_by'];
const KINDS = new Set(['exact-pin', 'held-back']);

/** An exact npm version: no range operator, no tag, no url. */
export function isExactPin(spec) {
  return typeof spec === 'string' && /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(spec);
}

/** Every exact pin declared by a workspace manifest. */
export function exactPinsIn(pkgJson, workspace) {
  const out = [];
  for (const section of ['dependencies', 'devDependencies', 'optionalDependencies']) {
    for (const [name, spec] of Object.entries(pkgJson?.[section] ?? {})) {
      if (isExactPin(spec)) out.push({ workspace, package: name, pinned: spec, section });
    }
  }
  return out;
}

/**
 * The parsed `dependabot.yml`, via a REAL YAML parser.
 *
 * This replaced ~150 lines of hand-rolled regex scanning, and the reason is the
 * scanning's track record rather than its tidiness. Inside one PR it was patched
 * three times — for mapping-key ORDER, then for FLOW SEQUENCES
 * (`directories: ["/a"]`), then it was found to miss QUOTED KEYS
 * (`'package-ecosystem': 'npm'`) and FLOW MAPPINGS
 * (`- { package-ecosystem: npm }`). Every patch was correct and every one left
 * another valid YAML spelling unread. A regex scanner cannot claim to read YAML;
 * that is a claim whose scope exceeds its mechanism, which is the exact defect
 * this whole PR exists to remove. Each miss returned [] rather than an error, so
 * the lint reported every watched directory as drift and exited 1 — a
 * commit-blocking wrong answer (CodeRabbit, PR #3325).
 *
 * python3 is not a new dependency: pre-commit, which runs this control, IS a
 * python application, and PyYAML is already pinned for CI in
 * `.github/requirements-pyyaml.txt`. An absent interpreter or module THROWS, and
 * both callers map that to exit 2 — "I could not check" — never to 0 or 1.
 * Parsing is also the validation: a document Dependabot cannot load is one this
 * cannot load either, so there is no separate syntax-check step to skip.
 *
 * `execFileSync` with an argument array, never `shell: true`.
 */
const parseCache = new Map();
export function parseDependabot(yamlText) {
  if (parseCache.has(yamlText)) return parseCache.get(yamlText);
  let raw;
  try {
    raw = execFileSync(
      'python3',
      ['-c', 'import sys, json, yaml; json.dump(yaml.safe_load(sys.stdin.read()), sys.stdout)'],
      { input: yamlText, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }
    );
  } catch (err) {
    const detail = String(err.stderr || err.message).trim().split('\n').pop();
    throw new Error(`dependabot.yml could not be parsed (python3 + PyYAML required): ${detail}`);
  }
  const doc = JSON.parse(raw);
  parseCache.set(yamlText, doc);
  return doc;
}

const isMapping = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const isNonEmptyString = (v) => typeof v === 'string' && v.trim() !== '';

/**
 * The `updates:` sequence, with the SHAPE these controls read validated.
 *
 * PyYAML establishes YAML **syntax**, and an earlier comment here claimed that
 * settled whether Dependabot could load the file. It does not, and the gap was a
 * fail-OPEN rather than a cosmetic overclaim: `ignore: not-a-list` is valid YAML,
 * so `Array.isArray(block.ignore)` was false, the ignore rule VANISHED, and the
 * pin lint printed "every pin and ignore accounted for" over an obligation it had
 * silently dropped — the exact shape this control exists to refuse. A non-mapping
 * `updates:` item was dropped the same way, by a `.filter()` that read like
 * defensiveness (CodeRabbit, PR #3325).
 *
 * Scope, stated so the next reader does not inherit the last overclaim: this
 * validates YAML syntax plus the fields these two controls actually read. It is
 * NOT Dependabot's own schema validator — an unknown ecosystem name, a malformed
 * `schedule:`, or any key neither control reads still passes here and may be
 * rejected by Dependabot. "This file parses and the parts we depend on are
 * well-formed" is the whole claim.
 */
function updateBlocks(doc) {
  if (doc == null) throw new Error('dependabot.yml: document is empty');
  if (!isMapping(doc)) throw new Error('dependabot.yml: document is not a mapping');
  const updates = doc.updates;
  if (updates == null) throw new Error('dependabot.yml: no "updates:" sequence');
  if (!Array.isArray(updates)) throw new Error('dependabot.yml: "updates:" is not a sequence');
  updates.forEach((block, i) => {
    const at = `updates[${i}]`;
    if (!isMapping(block)) throw new Error(`dependabot.yml: ${at} is not a mapping`);
    const eco = block['package-ecosystem'];
    if (typeof eco !== 'string' || eco === '') {
      throw new Error(`dependabot.yml: ${at} has no string "package-ecosystem"`);
    }
    // KEY PRESENCE, not value truthiness. `block.directory != null` read
    // `directory: null` as "the key is absent", so a block carrying an explicit
    // null alongside a valid `directories:` passed validation and was silently
    // treated as a directories-only block (CodeRabbit, PR #3325).
    const hasDir = 'directory' in block;
    const hasDirs = 'directories' in block;
    if (!hasDir && !hasDirs) {
      throw new Error(`dependabot.yml: ${at} (${eco}) names neither "directory" nor "directories"`);
    }
    // Dependabot documents these as ALTERNATIVES — `directory` is one location,
    // `directories` is several — so a block carrying both has no single valid
    // declared set. `blockDirectories` returned their UNION, which is a coverage
    // claim nothing supports: `directory: /app` with `directories: [/worker]`
    // reported both as watched and the audit could then print AUDIT CLEAN.
    // Refusing is the only honest answer, because picking one would be guessing
    // which of them Dependabot honours.
    if (hasDir && hasDirs) {
      throw new Error(`dependabot.yml: ${at} (${eco}) declares both "directory" and "directories"; they are alternatives`);
    }
    // LEAF values, not just the container shape. Validating only the container
    // left the same silent-skip one level down: `directories: [null]` became the
    // literal directory "null", `directories: [42]` became "42", and
    // `directory: ""` normalised to the ROOT sentinel — a fabricated claim that
    // the repository root is watched. An empty list read as "watches nothing",
    // which compares as drift over every directory the block was meant to name
    // (CodeRabbit, PR #3325).
    if (hasDir && !isNonEmptyString(block.directory)) {
      throw new Error(`dependabot.yml: ${at} (${eco}) "directory" is not a non-empty string`);
    }
    if (hasDirs) {
      if (!Array.isArray(block.directories) || block.directories.length === 0) {
        throw new Error(`dependabot.yml: ${at} (${eco}) "directories:" is not a non-empty sequence`);
      }
      block.directories.forEach((d, j) => {
        if (!isNonEmptyString(d)) {
          throw new Error(`dependabot.yml: ${at} (${eco}) directories[${j}] is not a non-empty string`);
        }
      });
    }
    if ('ignore' in block) {
      if (!Array.isArray(block.ignore)) {
        throw new Error(`dependabot.yml: ${at} (${eco}) "ignore:" is not a sequence`);
      }
      block.ignore.forEach((rule, j) => {
        if (!isMapping(rule)) {
          throw new Error(`dependabot.yml: ${at}.ignore[${j}] is not a mapping`);
        }
        // `dependency-name` is the field the register is matched on. Without it
        // the rule emitted NO obligation and the lint reported every obligation
        // accounted for — the container-level fail-open, one leaf deeper.
        if (!isNonEmptyString(rule['dependency-name'])) {
          throw new Error(`dependabot.yml: ${at}.ignore[${j}] has no non-empty string "dependency-name"`);
        }
      });
    }
  });
  return updates;
}

// No String() coercion: `updateBlocks` guarantees a non-empty string, and the
// coercion is precisely what turned `directories: [null]` into the directory
// "null". A type guarantee that is enforced upstream should not be re-faked here.
// Dependabot spells the repo root "/", which strips to ""; it is the "." workspace.
const normDir = (d) => d.replace(/^\/+/, '').replace(/\/+$/, '') || '.';

/**
 * Every directory a block names, whichever key spells it.
 *
 * A `directories:` that is not a sequence THROWS rather than being skipped: a
 * skipped key reads downstream as "this block watches nothing", which compares
 * as drift over every directory it actually watches. "Watches nothing" and "I
 * could not read what it watches" are the two claims these scripts exist to keep
 * apart.
 */
function blockDirectories(block) {
  const dirs = [];
  if (block.directory != null) dirs.push(normDir(block.directory));
  if ('directories' in block) {
    if (!Array.isArray(block.directories)) {
      throw new Error(`dependabot.yml: "directories:" is not a sequence (got ${typeof block.directories})`);
    }
    for (const d of block.directories) dirs.push(normDir(d));
  }
  return dirs;
}

/**
 * Every `ignore:` rule, emitted once per directory the block names.
 *
 * A block may declare `directories:` (a LIST) instead of `directory:`, and then
 * one ignore rule applies to EVERY directory in it. An earlier version kept only
 * the list's first entry, so an ignore under the multi-directory `docker` block
 * would have been demanded for `/services/control-plane` alone and gone unchecked
 * in the other three — a fail-OPEN shape (PR #2769 review, CodeRabbit). A block
 * naming no directory still reports once, matched on package alone.
 */
export function dependabotIgnores(yamlText) {
  const out = [];
  for (const block of updateBlocks(parseDependabot(yamlText))) {
    const dirs = blockDirectories(block);
    const ignores = Array.isArray(block.ignore) ? block.ignore : [];
    for (const rule of ignores) {
      // Guaranteed present and non-empty by updateBlocks; no `continue` here,
      // because a silent skip is the defect this control exists to refuse.
      const name = rule['dependency-name'];
      if (dirs.length === 0) out.push({ name, directory: null });
      else for (const dir of dirs) out.push({ name, directory: dir });
    }
  }
  return out;
}

/**
 * Every directory an `npm` update block watches, normalised to the register's
 * leading-slash-free form.
 *
 * Deliberately used to COMPARE against WORKSPACES, never to DERIVE it. Deriving
 * would couple a security control's scope to a file Dependabot also owns: a
 * reordered or malformed dependabot.yml would then silently change what this
 * control checks, and a parser that returned [] would disable the control
 * outright rather than failing it. The hardcoded list stays authoritative; this
 * only ever detects that the two have drifted apart.
 */
export function npmDirectories(yamlText) {
  return ecosystemDirectories(yamlText, 'npm');
}

/**
 * Every directory an update block for `ecosystem` watches, leading-slash-free
 * (the repository root becomes '').
 *
 * `npmDirectories` is the npm-only wrapper this control uses; the generalised
 * form exists so audit-dependabot-coverage.sh can ask the same question of
 * gomod, pip, docker and github-actions WITHOUT a second parser. Two parsers
 * would be two things to keep correct, and this repo has already shipped a fix
 * to one copy of a pair while leaving the other wrong (#3284).
 */
export function ecosystemDirectories(yamlText, ecosystem) {
  const out = new Set();
  for (const block of updateBlocks(parseDependabot(yamlText))) {
    if (block['package-ecosystem'] !== ecosystem) continue;
    for (const dir of blockDirectories(block)) out.add(dir);
  }
  return [...out].sort();
}

/**
 * Scope drift between WORKSPACES and the npm directories dependabot watches.
 *
 * Both directions are failures, for different reasons:
 *   - `uncovered`: dependabot proposes bumps for a directory this control never
 *     reads, so its pins need no justification while the run still reports "every
 *     pin and ignore accounted for". The #3292 defect.
 *   - `stale`: a WORKSPACES entry no block watches — a typo or a deleted
 *     directory. This one is easy to miss because the pin loop skips a
 *     non-existent path with `existsSync`, so a mistyped workspace silently
 *     covers nothing and still passes.
 */
export function workspaceDrift(workspaces, watched) {
  return {
    uncovered: watched.filter((d) => !workspaces.includes(d)),
    stale: workspaces.filter((w) => !watched.includes(w)),
  };
}

/**
 * True for a real calendar date, not merely a YYYY-MM-DD-shaped string.
 * `2026-13-45` matches the shape and is not a date; accepting it would let an
 * entry carry an expiry that can never trigger the WARN (PR #2769 review).
 */
export function isRealDate(s) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return false;
  const [y, m, d] = s.split('-').map(Number);
  const dt = new Date(Date.UTC(y, m - 1, d));
  return dt.getUTCFullYear() === y && dt.getUTCMonth() === m - 1 && dt.getUTCDate() === d;
}

export function validateEntries(entries) {
  const problems = [];
  entries.forEach((e, i) => {
    for (const f of REQUIRED) {
      if (!e[f] || typeof e[f] !== 'string' || e[f].trim() === '') {
        problems.push(`entry ${i} (${e.package ?? '?'}): missing or empty "${f}"`);
      }
    }
    if (e.kind && !KINDS.has(e.kind)) {
      problems.push(`entry ${i} (${e.package ?? '?'}): kind must be one of ${[...KINDS].join(', ')}`);
    }
    if (e.review_by && !isRealDate(e.review_by)) {
      problems.push(
        `entry ${i} (${e.package ?? '?'}): review_by must be a real YYYY-MM-DD date (got "${e.review_by}")`
      );
    }
    // A held-back entry is a live refusal of an available version, so it must
    // say which one it is refusing. An exact-pin need not.
    if (e.kind === 'held-back' && !e.available) {
      problems.push(`entry ${i} (${e.package ?? '?'}): kind=held-back requires "available" (the version being refused)`);
    }
  });
  return problems;
}

export function findExpired(entries, nowISO) {
  return entries.filter((e) => e.review_by && e.review_by < nowISO.slice(0, 10));
}

const log = (s) => process.stdout.write(`${s}\n`);

function readJson(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (err) {
    process.stderr.write(`check-pin-exceptions: cannot read ${path}: ${err.message}\n`);
    process.exit(2);
  }
}

function main() {
  const root = process.argv[2] ? resolvePath(process.argv[2]) : process.cwd();
  const nowISO = process.env.PIN_LINT_NOW ?? new Date().toISOString();

  const registerPath = join(root, REGISTER);
  if (!existsSync(registerPath)) {
    process.stderr.write(`check-pin-exceptions: register not found at ${registerPath}\n`);
    process.exit(2);
  }
  const register = readJson(registerPath);
  const entries = Array.isArray(register.exceptions) ? register.exceptions : null;
  if (!entries) {
    process.stderr.write('check-pin-exceptions: register has no "exceptions" array\n');
    process.exit(2);
  }

  const malformed = validateEntries(entries);
  const documented = new Set(entries.map((e) => `${e.workspace}|${e.package}`));
  const byName = new Set(entries.map((e) => e.package));

  // An ignore: rule is a LIVE REFUSAL of an available version — which is exactly
  // what kind=held-back means — so it must be matched by a held-back entry, not
  // by just any entry. Matching on workspace+package alone let the pre-existing
  // exact-pin for @electron/fuses silently discharge a new ignore's obligation:
  // the register went on asserting "being at latest is normal" while 2.1.3 had
  // been refused since 2026-06-29 (PR #3199 review, security-reviewer, MEDIUM).
  // An exact-pin cannot express a hold — it carries no `available` and its
  // review_by is pin-shaped, so nothing decays and nothing ever warns.
  const heldBack = entries.filter((e) => e.kind === 'held-back');
  const heldByWorkspace = new Set(heldBack.map((e) => `${e.workspace}|${e.package}`));
  const heldByName = new Set(heldBack.map((e) => e.package));

  const undocumented = [];
  for (const ws of WORKSPACES) {
    const p = join(root, ws, 'package.json');
    if (!existsSync(p)) continue;
    for (const pin of exactPinsIn(readJson(p), ws)) {
      if (!documented.has(`${pin.workspace}|${pin.package}`)) undocumented.push(pin);
    }
  }

  // Ignores match on workspace+package when the block names a directory, and
  // fall back to package-only when it does not. Matching on package alone let a
  // register entry for one workspace satisfy an ignore in another (PR #2769
  // review, CodeRabbit, Major). `directory: "/client/desktop"` is normalised to
  // the register's `workspace: "client/desktop"` form.
  const normWs = (d) => (d ?? '').replace(/^\/+/, '').replace(/\/+$/, '');
  const undocumentedIgnores = [];
  const dbPath = join(root, DEPENDABOT);
  const drift = { uncovered: [], stale: [] };
  // Fail closed when dependabot.yml is absent, symmetric with the missing-register
  // check above. Without this the file's absence is INDISTINGUISHABLE FROM A PASS:
  // `drift` keeps its empty default so the scope comparison never runs, every
  // `ignore:` obligation silently evaporates, and the run still prints "every pin
  // and ignore accounted for". A control that reports success when the thing it
  // audits is missing is the exact defect this file exists to catch, one level up
  // (Gitar, PR #3325 — it read the guard rather than the intent).
  if (!existsSync(dbPath)) {
    process.stderr.write(`check-pin-exceptions: dependabot config not found at ${dbPath}\n`);
    process.exit(2);
  }
  // A parser that cannot read the config must not fall through to exit 1, which
  // this lint uses for "findings" — an unreadable dependabot.yml would then be
  // reported as a pin problem, and an uncaught throw would say so with a stack
  // trace. Exit 2 means "could not check", which is a different claim.
  try {
    Object.assign(drift, workspaceDrift(WORKSPACES, npmDirectories(readFileSync(dbPath, 'utf8'))));
    for (const { name, directory } of dependabotIgnores(readFileSync(dbPath, 'utf8'))) {
      const ws = normWs(directory);
      const label = ws ? `${name} (in ${ws})` : name;
      const heldMatch = ws ? heldByWorkspace.has(`${ws}|${name}`) : heldByName.has(name);
      if (heldMatch) continue;
      // Distinguish "no entry at all" from "an entry exists under the wrong kind".
      // Without this the second case reports as undocumented, and the reader goes
      // looking for an entry that is right there.
      const anyMatch = ws ? documented.has(`${ws}|${name}`) : byName.has(name);
      undocumentedIgnores.push(
        anyMatch ? `${label} — entry exists but is not kind="held-back"` : label
      );
    }
  } catch (err) {
    process.stderr.write(`check-pin-exceptions: cannot read ${dbPath}: ${err.message}\n`);
    process.exit(2);
  }

  const expired = findExpired(entries, nowISO);
  for (const e of expired) {
    log(`  WARN  review_by ${e.review_by} has passed: ${e.package} (${e.workspace}) — re-validate or extend`);
  }

  const drifted = drift.uncovered.length + drift.stale.length;
  if (malformed.length === 0 && undocumented.length === 0 && undocumentedIgnores.length === 0 && drifted === 0) {
    const suffix = expired.length ? `; ${expired.length} past review_by (warning only)` : '';
    log(`pin-exception lint: ${entries.length} documented exception(s); every pin and ignore accounted for${suffix}.`);
    process.exit(0);
  }

  if (malformed.length) {
    log('pin-exception lint: malformed register entries:');
    for (const p of malformed) log(`    ${p}`);
  }
  if (undocumented.length) {
    log(`pin-exception lint: ${undocumented.length} exact pin(s) with no register entry:`);
    for (const u of undocumented) log(`    ${u.workspace}  ${u.package}@${u.pinned}  (${u.section})`);
  }
  if (undocumentedIgnores.length) {
    log(`pin-exception lint: ${undocumentedIgnores.length} dependabot ignore rule(s) with no register entry:`);
    for (const n of undocumentedIgnores) log(`    ${n}`);
  }
  if (drift.uncovered.length) {
    log(`pin-exception lint: ${drift.uncovered.length} npm director(ies) dependabot watches but this control does not read:`);
    for (const d of drift.uncovered) log(`    ${d}`);
    log('    Add them to WORKSPACES. Until then their exact pins need no justification');
    log('    and this lint still reports "every pin and ignore accounted for".');
  }
  if (drift.stale.length) {
    log(`pin-exception lint: ${drift.stale.length} WORKSPACES entr(ies) no npm update block watches:`);
    for (const d of drift.stale) log(`    ${d}`);
    log('    Either a typo or a removed directory. A mistyped path is skipped silently');
    log('    by the existsSync guard in the pin loop, so it covers nothing and passes.');
  }
  log('');
  log('A held version needs a written reason and an expiry. An exact pin makes every bump');
  log('arrive as a reviewable PR; an ignore: rule actively refuses an available version. Both');
  log(`are decisions, and decisions rot — record them in ${REGISTER}.`);
  log('');
  log('An ignore: rule needs kind="held-back" specifically, naming the version it refuses');
  log('in "available". An exact-pin entry does not satisfy one: it means "being at latest is');
  log('normal", which is the opposite of what an ignore asserts.');
  process.exit(1);
}

function invokedDirectly() {
  if (!process.argv[1]) return false;
  try {
    return realpathSync(process.argv[1]) === fileURLToPath(import.meta.url);
  } catch {
    return false;
  }
}

if (invokedDirectly()) main();
