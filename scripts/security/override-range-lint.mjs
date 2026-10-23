#!/usr/bin/env node
//
// override-range lint — flags `overrides` entries that force a consumer onto a
// version it never declared support for.
//
// WHY THIS EXISTS
//
// An `overrides` entry says: "whatever anyone in this tree asks for, give them
// this instead". Silence is not a bug — overriding declared ranges IS the
// feature, so npm emits no warning, `npm ls` prints no `invalid` marker, and
// the resolved tree looks entirely normal. That is correct behaviour for a
// deliberate, current decision.
//
// The failure mode is that the decision expires without being edited. A range
// in `dependencies` is scoped to one package's needs; a global override is an
// unscoped standing claim over EVERY consumer in the tree, including ones that
// do not exist yet. So the entry stays true about the past and quietly becomes
// false about the present, because the tree moved rather than the entry.
//
// Founding incident (PR #2709, 2026-08-14). client/desktop pinned
// `undici: ^7.29.0` to close GHSA-4cwx-7wf7-3272 back when the tree hoisted a
// single copy. By the time jsdom 30 landed:
//
//     @electron/get  declares ^7.24.4  -> forced 7.29.0   satisfies
//     jsdom@30       declares ^8.9.0   -> forced 7.29.0   A MAJOR BELOW
//     node-gyp       declares ^6.25.0  -> forced 7.29.0   A MAJOR ABOVE
//
// Two of three consumers ran a major they never claimed to support, and
// node-gyp had been in that state BEFORE the jsdom bump — nobody noticed,
// because nothing looks. The symptom was not a dependency error: it was
// `import('@playwright/test')` deadlocking under jsdom, surfacing as a 10s
// timeout in a test file that never mentions undici. Neither npm, the
// npm-audit gate (7.29.0 is fully patched — no advisory to report), nor 12,862
// passing tests said anything. A reviewer reading the diff caught it.
//
// WHAT THIS CHECKS
//
// Two decidable properties, from local files only — no network, no registry:
//
//     for every package under `overrides`, for every consumer that declares it,
//     the version that consumer actually resolves to MUST satisfy the range
//     that consumer declared.
//
//     for every caller-supplied `--minimum-version package=version`, every
//     resolved lockfile copy on that major line MUST meet the floor, even after
//     an override is removed.
//
// A violation is a QUESTION, not automatically a bug. Forcing a consumer off
// its declared range is sometimes right. The job here is to make that choice
// visible and re-affirmed rather than silently inherited — which is precisely
// what failed in #2709.
//
// Exit: 0 = pass, 1 = violation present, 2 = usage/parse error (fail closed).

import { readFileSync, writeFileSync, realpathSync, existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { resolve as resolvePath, join, sep } from 'node:path';

const BASELINE_FILE = '.override-range-baseline.json';

/**
 * Load `semver` from the package under test rather than from the repo root.
 *
 * There is no root package.json, so a repo-root script has no node_modules of
 * its own. Every workspace carries semver in its committed lockfile, so this
 * resolves against the directory being linted.
 *
 * Fails closed on purpose: a lint that cannot evaluate ranges must not report
 * "no violations". That would be a vacuous pass of exactly the kind this repo
 * has been bitten by before.
 */
export function loadSemver(pkgDir) {
  const require = createRequire(join(resolvePath(pkgDir), 'noop.js'));
  return require('semver');
}

/**
 * Resolve which physical copy of `dep` a consumer at lockfile path `fromPath`
 * actually gets, following Node's nearest-ancestor node_modules rule.
 *
 * Lockfile v3 keys look like:
 *   ""                                        (the root package)
 *   "node_modules/a"
 *   "node_modules/a/node_modules/b"
 *
 * A consumer at "node_modules/a" asking for "b" checks
 * "node_modules/a/node_modules/b" first, then falls back to "node_modules/b".
 * Returns the matching lockfile entry with its path, or null when the dep is
 * absent from the tree entirely (an optional dep that was never installed).
 */
export function resolveFrom(packages, fromPath, dep) {
  // Ancestor scopes, nearest first. "node_modules/a/node_modules/b" yields
  // itself, then "node_modules/a", then "" (root).
  const scopes = [];
  let cur = fromPath;
  for (;;) {
    scopes.push(cur);
    const idx = cur.lastIndexOf('/node_modules/');
    if (idx === -1) break;
    cur = cur.slice(0, idx);
  }
  if (scopes[scopes.length - 1] !== '') scopes.push('');

  for (const scope of scopes) {
    const candidate = scope === '' ? `node_modules/${dep}` : `${scope}/node_modules/${dep}`;
    if (Object.hasOwn(packages, candidate)) {
      return { path: candidate, entry: packages[candidate] };
    }
  }
  return null;
}

/**
 * True when a resolved entry is not a plain registry version that semver can
 * reason about — a `file:` stub, a workspace link, a git or tarball URL.
 *
 * These are categorically different from a version skew: they are deliberate
 * substitutions, and no semver range can express them. services/media-plane's
 * `node-domexception: file:./stubs/node-domexception` is the live example.
 * Skipping them needs no configuration and cannot mask a major mismatch,
 * because there is no comparable version to mismatch.
 */
export function isNonSemverResolution(entry, semver) {
  if (!entry) return true;
  if (entry.link === true) return true;
  if (typeof entry.version !== 'string') return true;
  if (/^(file|link|git|github|https?):/i.test(entry.version)) return true;
  return semver.valid(entry.version) === null;
}

/**
 * Collect every consumer of every overridden package whose resolved version
 * does not satisfy that consumer's own declared range.
 *
 * `overrides` may be nested (npm allows `{ "pkg": { "dep": "range" } }`), so
 * the overridden NAMES are collected from the top level plus any nested keys —
 * the lint cares about which packages are being forced, not what they are
 * forced to.
 */
export function findViolations(lock, overrides, semver) {
  const packages = packagesFromLock(lock);
  // Array.isArray is load-bearing, not defensive noise: `typeof [] === 'object'`,
  // so a lockfile with `"packages": []` sailed through this guard, then
  // Object.entries found no consumers and the run reported CLEAN — a vacuous
  // pass on a malformed lockfile, in the one function whose whole posture is
  // fail-closed. Caught in review on PR #2768; regression-tested.
  const overridden = collectOverriddenNames(overrides);
  const violations = [];
  const skipped = [];

  for (const [consumerPath, consumer] of Object.entries(packages)) {
    if (!consumer || typeof consumer !== 'object') continue;

    // Only real dependency edges. peerDependencies are excluded deliberately:
    // npm reports unmet peers itself (ERESOLVE), so they are already visible,
    // and an override is not what satisfies them.
    const edges = {
      ...(consumer.dependencies ?? {}),
      ...(consumer.devDependencies ?? {}),
      ...(consumer.optionalDependencies ?? {}),
    };

    for (const [dep, range] of Object.entries(edges)) {
      if (!overridden.has(dep)) continue;
      if (typeof range !== 'string') continue;

      const resolved = resolveFrom(packages, consumerPath, dep);
      if (!resolved) continue; // never installed (optional dep) — nothing forced

      if (isNonSemverResolution(resolved.entry, semver)) {
        skipped.push({
          consumer: consumerPath === '' ? '(root)' : consumerPath,
          dep,
          range,
          resolved: resolved.entry?.version ?? '(no version)',
          reason: 'non-semver resolution (file:/link:/git: or workspace link)',
        });
        continue;
      }

      // A range npm accepts but semver cannot validate (`latest`, `workspace:*`,
      // `github:org/repo`, a tarball URL) is NOT skipped. It is reported, under
      // its own heading, and it blocks.
      //
      // This looks asymmetric against the file:/link: skip above, and the
      // asymmetry is deliberate (raised in review on PR #2768). The two cases
      // are opposites:
      //
      //   non-semver RESOLUTION — the override replaced the package with a stub.
      //     There is no version to compare against anything. Skipping is the
      //     only available action and cannot hide a skew, because no skew is
      //     expressible.
      //
      //   non-semver DECLARED RANGE — the consumer deliberately asked for
      //     something off-registry, and the override handed it a registry
      //     version instead. That is a STRONGER form of the harm this lint
      //     exists to catch, not an exemption from it. Skipping would build a
      //     blind spot precisely where the override is most surprising.
      //
      // What review was right about is the wording: these are not measured
      // skews, so they must not print under the "forced outside its declared
      // range" banner. They get their own section that says what they are —
      // unevaluatable, needing human judgement — and remain baselineable like
      // any other row.
      if (semver.validRange(range) === null) {
        violations.push({
          consumer: consumerPath === '' ? '(root)' : consumerPath,
          dep,
          range,
          resolved: resolved.entry.version,
          kind: 'unparseable-range',
        });
        continue;
      }

      if (!semver.satisfies(resolved.entry.version, range, { includePrerelease: true })) {
        violations.push({
          consumer: consumerPath === '' ? '(root)' : consumerPath,
          dep,
          range,
          resolved: resolved.entry.version,
          kind:
            semver.major(resolved.entry.version) !== majorOf(range, semver)
              ? 'major-mismatch'
              : 'range-mismatch',
        });
      }
    }
  }

  return { violations, skipped };
}

function packagesFromLock(lock) {
  const packages = lock?.packages;
  if (!packages || typeof packages !== 'object' || Array.isArray(packages)) {
    throw new Error('lockfile has no `packages` map (need lockfileVersion 2 or 3)');
  }
  return packages;
}

/**
 * Find resolved copies on the floor's major line that are below that floor.
 * This reads the lockfile directly so a floor remains enforced after an
 * override has been removed.
 */
export function findMinimumVersionViolations(
  lock,
  floors,
  semver,
  dir,
  pkg,
  verifyInstalledVersions = false
) {
  if (!pkg || typeof pkg !== 'object' || Array.isArray(pkg)) {
    throw new Error('package manifest must be an object');
  }
  const packages = packagesFromLock(lock);
  const violations = [];
  const workspace = resolvePath(dir);

  for (const path of Object.keys(packages)) {
    if (
      path !== '' &&
      path.split('/').some((segment) =>
        segment === '' || segment === '.' || segment === '..' || segment.includes('\\')
      )
    ) {
      throw new Error(`invalid lockfile package path escapes workspace: ${path}`);
    }
  }

  for (const floor of floors) {
    const aliasPaths = new Set();
    const reverseAliasPaths = new Set();
    const rootEdges = new Map();
    const rootIdentityMismatches = new Set();
    const targetsFloorAlias = (value) =>
      value === `npm:${floor.dep}` || value.startsWith(`npm:${floor.dep}@`);
    for (const [consumerPath, lockConsumer] of Object.entries(packages)) {
      const consumers = consumerPath === '' ? [pkg, lockConsumer] : [lockConsumer];
      for (const consumer of consumers) {
        if (!consumer || typeof consumer !== 'object' || Array.isArray(consumer)) continue;
        const edges = new Map();
        const edgeSections = new Map();
        for (const section of ['dependencies', 'devDependencies', 'optionalDependencies', 'peerDependencies']) {
          const declared = consumer[section];
          if (declared === undefined) continue;
          if (!declared || typeof declared !== 'object' || Array.isArray(declared)) {
            throw new Error(`malformed ${section} declaration at ${consumerPath || '(root)'}`);
          }
          for (const [alias, target] of Object.entries(declared)) {
            if (typeof target !== 'string') continue;
            const priorTarget = edges.get(alias);
            const aliasesAwayFromFloor = (value) =>
              value.startsWith('npm:') && !targetsFloorAlias(value);
            const optionalOverridesDependency =
              section === 'optionalDependencies' &&
              edgeSections.get(alias) === 'dependencies' &&
              !(
                alias === floor.dep &&
                (aliasesAwayFromFloor(priorTarget) || aliasesAwayFromFloor(target))
              );
            if (
              edges.has(alias) &&
              priorTarget !== target &&
              !optionalOverridesDependency &&
              (alias === floor.dep || targetsFloorAlias(priorTarget) || targetsFloorAlias(target))
            ) {
              throw new Error(`conflicting dependency declaration for ${alias} at ${consumerPath || '(root)'}`);
            }
            edges.set(alias, target);
            edgeSections.set(alias, section);
          }
        }
        for (const [alias, target] of edges) {
          const rootTarget = rootEdges.get(alias);
          if (consumerPath === '' && rootEdges.has(alias) && rootTarget !== target) {
            if (
              alias === floor.dep ||
              (rootTarget.startsWith('npm:') &&
                target.startsWith('npm:') &&
                (targetsFloorAlias(rootTarget) || targetsFloorAlias(target)))
            ) {
              throw new Error(`conflicting dependency declaration for ${alias} at (root)`);
            }
            if (targetsFloorAlias(rootTarget) || targetsFloorAlias(target)) {
              rootIdentityMismatches.add(alias);
            }
          }
          if (consumerPath === '') rootEdges.set(alias, target);
          let targetPackage;
          let targetSpec;
          if (target.startsWith('npm:')) {
            const match = /^npm:(@[^@/\s]+\/[^@/\s]+|[^@/\s]+)(?:@(.+))?$/.exec(target);
            if (!match || (match[2] !== undefined && match[2].trim() === '')) {
              if (alias === floor.dep) {
                throw new Error(`malformed npm alias declaration for ${alias} at ${consumerPath || '(root)'}`);
              }
              continue;
            }
            targetPackage = match[1];
            targetSpec = match[2];
          }
          const targetsFloor = targetPackage === floor.dep;
          const targetsDifferentPackage = alias === floor.dep && targetPackage && !targetsFloor;
          if (!targetsFloor && !targetsDifferentPackage) continue;
          if (target.startsWith('npm:')) {
            const packageSegments = targetPackage.startsWith('@') ? targetPackage.slice(1).split('/') : [targetPackage];
            if (
              targetPackage.length > 214 ||
              packageSegments.some((segment) => !/^[a-z0-9][a-z0-9._~-]*$/.test(segment))
            ) {
              throw new Error(`malformed npm alias declaration for ${alias} at ${consumerPath || '(root)'}`);
            }
            const spec = targetSpec;
            if (
              spec !== undefined &&
              (/^(?:file|link|git(?:\+[^:]*)?|github|https?):/i.test(spec) ||
                spec.includes('/') ||
                semver.validRange(spec) === null && !/^[A-Za-z][A-Za-z0-9._-]*$/.test(spec))
            ) {
              throw new Error(`malformed npm alias declaration for ${alias} at ${consumerPath || '(root)'}`);
            }
          }
          const resolved = resolveFrom(packages, consumerPath, alias);
          if (resolved) {
            if (targetsFloor) aliasPaths.add(resolved.path);
            if (targetsDifferentPackage) reverseAliasPaths.add(resolved.path);
          }
        }
      }
    }

    const rootIdentityMismatchPaths = new Map();
    for (const alias of rootIdentityMismatches) {
      const resolved = resolveFrom(packages, '', alias);
      if (!resolved) throw new Error(`conflicting dependency declaration for ${alias} at (root)`);
      rootIdentityMismatchPaths.set(resolved.path, alias);
    }

    for (const [path, entry] of Object.entries(packages)) {
      const manifestPath = path === '' ? null : resolvePath(workspace, path, 'package.json');
      if (manifestPath && !manifestPath.startsWith(`${workspace}${sep}`)) {
        throw new Error(`lockfile path escapes workspace: ${path}`);
      }
      const isInstalledPath = path.split('/').includes('node_modules');
      const hasLockIdentity =
        isInstalledPath && entry && typeof entry === 'object' && !Array.isArray(entry) && Object.hasOwn(entry, 'name');
      if (hasLockIdentity && (typeof entry.name !== 'string' || entry.name.trim() === '')) {
        throw new Error(`malformed lock identity at ${path}`);
      }
      const matchesIdentity = hasLockIdentity && entry.name === floor.dep;
      const matchesInstallPath =
        !reverseAliasPaths.has(path) &&
        (path === `node_modules/${floor.dep}` || path.endsWith(`/node_modules/${floor.dep}`));
      const matchesAlias = aliasPaths.has(path);
      const isDeclaredCandidate = matchesIdentity || matchesInstallPath || matchesAlias || reverseAliasPaths.has(path);
      let installed;
      let manifestError;
      let manifestPresent = false;
      if (isInstalledPath && manifestPath && existsSync(manifestPath)) {
        manifestPresent = true;
        try {
          installed = JSON.parse(readFileSync(manifestPath, 'utf8'));
        } catch {
          manifestError = true;
        }
      }
      const hasInstalledManifestObject =
        installed !== null && typeof installed === 'object' && !Array.isArray(installed);
      const matchesInstalledIdentity =
        path !== '' &&
        !hasLockIdentity &&
        hasInstalledManifestObject &&
        installed.name === floor.dep;
      if (!isDeclaredCandidate && !matchesInstalledIdentity) continue;
      if (manifestError || (manifestPresent && !hasInstalledManifestObject)) {
        throw new Error(`malformed installed package manifest for ${floor.dep} at ${path}`);
      }
      if (
        manifestPresent &&
        (typeof installed.name !== 'string' ||
          typeof installed.version !== 'string' ||
          !semver.valid(installed.version))
      ) {
        throw new Error(`malformed installed package manifest for ${floor.dep} at ${path}`);
      }
      if (
        rootIdentityMismatchPaths.has(path) &&
        (!manifestPresent || installed.name !== floor.dep)
      ) {
        throw new Error(
          `conflicting dependency declaration for ${rootIdentityMismatchPaths.get(path)} at (root)`
        );
      }
      if (reverseAliasPaths.has(path) && (matchesIdentity || matchesInstalledIdentity)) {
        throw new Error(`contradictory reverse alias identity for ${floor.dep} at ${path}`);
      }
      if (!matchesInstallPath && !matchesIdentity && !matchesAlias && !matchesInstalledIdentity) {
        continue;
      }
      if (!entry || typeof entry !== 'object' || Array.isArray(entry)) {
        throw new Error(`malformed matching resolution for ${floor.dep} at ${path}`);
      }
      if (typeof entry.version !== 'string' || semver.valid(entry.version) === null) {
        throw new Error(`malformed matching resolution for ${floor.dep} at ${path}`);
      }
      if (verifyInstalledVersions && manifestPresent) {
        if (installed.name !== floor.dep) {
          throw new Error(
            `installed package identity ${installed.name} does not match ${floor.dep} at ${path}`
          );
        }
        if (installed.version !== entry.version) {
          throw new Error(
            `installed ${floor.dep} version ${installed.version} does not match lockfile ${entry.version} at ${path}`
          );
        }
      }
      if (semver.major(entry.version) !== semver.major(floor.version)) continue;
      if (semver.lt(entry.version, floor.version)) {
        violations.push({ dep: floor.dep, minimum: floor.version, resolved: entry.version, path });
      }
    }
  }

  return violations;
}

/**
 * Find exact nested overrides whose installed parent no longer needs the
 * forced version. In strict mode that makes a temporary containment override
 * expired even if its paired baseline was deleted first.
 */
export function findExpiredNestedOverrides(lock, overrides, semver) {
  const packages = lock?.packages;
  if (!packages || typeof packages !== 'object' || Array.isArray(packages)) return [];

  const expired = [];
  for (const [parent, children] of Object.entries(overrides)) {
    if (!children || typeof children !== 'object' || Array.isArray(children)) continue;
    const parentEntries = Object.entries(packages).filter(
      ([path, entry]) =>
        (path === `node_modules/${parent}` || path.endsWith(`/node_modules/${parent}`)) &&
        entry &&
        typeof entry === 'object'
    );

    for (const [dep, forced] of Object.entries(children)) {
      if (typeof forced !== 'string' || semver.valid(forced) === null) continue;
      const ranges = parentEntries
        .map(
          ([, entry]) =>
            ({ ...entry.dependencies, ...entry.devDependencies, ...entry.optionalDependencies })[dep]
        )
        .filter((range) => typeof range === 'string');
      if (
        ranges.length === 0 ||
        ranges.every(
          (range) => semver.validRange(range) !== null && semver.satisfies(forced, range)
        )
      ) {
        expired.push({ parent, dep, forced, reason: ranges.length === 0 ? 'missing-edge' : 'compatible-ranges' });
      }
    }
  }
  return expired;
}

/** Best-effort major for a range, used only to label a violation. */
function majorOf(range, semver) {
  const min = semver.minVersion(range);
  return min ? semver.major(min) : null;
}

/**
 * Identity of a violation for baseline matching.
 *
 * DELIBERATELY the full tuple — consumer, package, declared range, resolved
 * version — and NOT just the package name. This is the property that makes the
 * baseline safe rather than a blanket mute.
 *
 * #2709 is the proof. `undici` was ALREADY violating via node-gyp (declares
 * ^6.25.0, forced to 7.29.0) long before jsdom 30 arrived. A baseline keyed on
 * the package name would have listed "undici" as accepted and then silently
 * absorbed jsdom's brand-new ^8.9.0 violation — the exact bug this lint exists
 * to catch would have been muted by its own baseline on day one.
 *
 * Keying on the tuple means a new consumer, a changed declared range, or a
 * different resolved version all fail to match and are reported.
 */
export function violationKey(v) {
  return JSON.stringify([v.consumer, v.dep, v.range, v.resolved]);
}

/**
 * Split violations into ones already accepted in the baseline and ones that are
 * new, plus baseline entries that no longer occur (cruft worth pruning).
 */
export function applyBaseline(violations, baseline) {
  const accepted = Array.isArray(baseline?.accepted) ? baseline.accepted : [];
  const acceptedKeys = new Set(accepted.map(violationKey));
  const seen = new Set(violations.map(violationKey));

  return {
    fresh: violations.filter((v) => !acceptedKeys.has(violationKey(v))),
    known: violations.filter((v) => acceptedKeys.has(violationKey(v))),
    stale: accepted.filter((a) => !seen.has(violationKey(a))),
  };
}

/**
 * Overridden package NAMES, from top-level keys plus any nested-object keys.
 *
 * npm's nested form `{ "foo": { "undici": "^7" } }` is PARENT-SCOPED — it means
 * "when undici is a dependency of foo", not a global undici override. This
 * function deliberately flattens that scoping and collects the names only.
 *
 * Raised in review on PR #2768 as a false-positive risk. It is not one, and the
 * reason is worth stating because it is not obvious: this set only decides which
 * package names to EXAMINE. Every verdict comes from the lockfile — a consumer's
 * resolved version against that consumer's own declared range. Under a
 * parent-scoped override, unrelated consumers resolve inside their own ranges
 * and pass trivially, so widening the name set cannot manufacture a finding.
 *
 * Verified against real npm rather than argued: a tree with
 * `overrides: { "some-unrelated-parent": { "undici": "^7.29.0" } }` and jsdom@30
 * resolves jsdom's undici to 8.10.0, and this lint reports clean.
 *
 * A finding can therefore only appear where a consumer genuinely resolves
 * outside what it declared — which is worth surfacing whichever override caused
 * it. Full parent-selector matching would be materially more code for no
 * behavioural gain; the Worker workspaces now use the nested form for a
 * temporary Miniflare containment, so retaining the conservative superset
 * keeps the lint generic while that scoped override is in place.
 */
export function collectOverriddenNames(overrides) {
  const names = new Set();
  const walk = (obj) => {
    if (!obj || typeof obj !== 'object') return;
    for (const [k, v] of Object.entries(obj)) {
      // A nested override block is keyed by the PARENT package name; its own
      // keys are the packages actually being forced. Record both — a top-level
      // key is always a forced package, and a nested key is too.
      if (!k.startsWith('.')) names.add(k);
      if (v && typeof v === 'object') walk(v);
    }
  };
  walk(overrides);
  return names;
}

function readJson(path, label) {
  let raw;
  try {
    raw = readFileSync(path, 'utf8');
  } catch (err) {
    process.stderr.write(`override-range-lint: cannot read ${label} at ${path}: ${err.message}\n`);
    process.exit(2);
  }
  try {
    return JSON.parse(raw);
  } catch (err) {
    process.stderr.write(`override-range-lint: ${label} at ${path} is not valid JSON: ${err.message}\n`);
    process.exit(2);
  }
}

/**
 * Write the baseline, sorted so regeneration produces a stable diff rather than
 * lockfile-iteration-order churn that reviewers cannot read.
 */
function writeBaseline(path, violations) {
  const body = {
    '//': [
      'Accepted override-range skews — see scripts/security/override-range-lint.mjs.',
      'Each row is a consumer that resolves OUTSIDE the range it declares, because an',
      '`overrides` entry forces it there. Rows are matched on the full tuple, so a new',
      'consumer or a changed range/resolution is reported even for a package already',
      'listed here. Prune a row when the lint reports it as no longer occurring.',
      'Regenerate: node scripts/security/override-range-lint.mjs --dir <pkg> --update-baseline',
    ].join(' '),
    accepted: [...violations]
      .map((v) => ({ consumer: v.consumer, dep: v.dep, range: v.range, resolved: v.resolved }))
      .sort((a, b) => violationKey(a).localeCompare(violationKey(b))),
  };
  writeFileSync(path, `${JSON.stringify(body, null, 2)}\n`, 'utf8');
}

function parseArgs(argv) {
  const out = {
    dir: '.',
    failOnStale: false,
    json: false,
    baseline: null,
    updateBaseline: false,
    verifyInstalledVersions: false,
    minimumVersionSpecs: [],
  };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--dir') out.dir = argv[++i];
    else if (a === '--baseline') out.baseline = argv[++i];
    else if (a === '--minimum-version') out.minimumVersionSpecs.push(argv[++i]);
    else if (a === '--fail-on-stale') out.failOnStale = true;
    else if (a === '--verify-installed-versions') out.verifyInstalledVersions = true;
    else if (a === '--update-baseline') out.updateBaseline = true;
    else if (a === '--json') out.json = true;
    else {
      process.stderr.write(`override-range-lint: unknown argument: ${a}\n`);
      process.exit(2);
    }
  }
  if (typeof out.dir !== 'string' || out.dir === '') {
    process.stderr.write('override-range-lint: --dir requires a path\n');
    process.exit(2);
  }
  return out;
}

function parseMinimumVersionSpecs(specs, semver) {
  const floors = [];
  const seen = new Set();
  for (const spec of specs) {
    if (typeof spec !== 'string') throw new Error('--minimum-version requires package=version');
    const equals = spec.indexOf('=');
    const dep = spec.slice(0, equals);
    const version = spec.slice(equals + 1);
    if (equals <= 0 || spec.indexOf('=', equals + 1) !== -1 || /\s/.test(dep) || !semver.valid(version)) {
      throw new Error(`invalid --minimum-version ${String(spec)} (expected package=version)`);
    }
    const normalized = semver.valid(version);
    const key = `${dep}@${semver.major(normalized)}`;
    if (seen.has(key)) throw new Error(`duplicate --minimum-version for ${key}`);
    seen.add(key);
    floors.push({ dep, version: normalized });
  }
  return floors;
}

const log = (s) => process.stdout.write(`${s}\n`);

function main() {
  const args = parseArgs(process.argv.slice(2));
  const dir = resolvePath(args.dir);
  const pkgPath = join(dir, 'package.json');
  const lockPath = join(dir, 'package-lock.json');

  for (const [p, label] of [
    [pkgPath, 'package.json'],
    [lockPath, 'package-lock.json'],
  ]) {
    if (!existsSync(p)) {
      process.stderr.write(`override-range-lint: ${label} not found at ${p}\n`);
      process.exit(2);
    }
  }

  const pkg = readJson(pkgPath, 'package.json');
  const lock = readJson(lockPath, 'package-lock.json');

  const overrides = pkg.overrides;
  const baselinePath = args.baseline ?? join(dir, BASELINE_FILE);
  const hasOverrides = overrides && Object.keys(overrides).length > 0;
  if (!hasOverrides && args.minimumVersionSpecs.length === 0) {
    const baseline =
      args.failOnStale && existsSync(baselinePath) ? readJson(baselinePath, 'baseline') : null;
    const { stale } = args.failOnStale ? applyBaseline([], baseline) : { stale: [] };
    if (stale.length > 0) {
      if (args.json) {
        log(
          JSON.stringify(
            { dir: args.dir, fresh: [], known: [], stale, skipped: [], minimumVersionViolations: [] },
            null,
            2
          )
        );
      } else {
        for (const s of stale) {
          log(
            `  WARN  baseline row no longer occurs, prune it: ${s.dep} via ${s.consumer} (declared ${s.range}, was resolving ${s.resolved})`
          );
        }
        log(
          `override-range lint: ${stale.length} stale baseline row(s) in ${args.dir}; remove the obsolete override and baseline.`
        );
      }
      process.exit(1);
    }
    log(`override-range lint: no overrides declared in ${args.dir} — nothing to check.`);
    process.exit(0);
  }

  // Fail closed rather than vacuously pass when ranges cannot be evaluated.
  let semver;
  try {
    semver = loadSemver(dir);
  } catch (err) {
    process.stderr.write(
      `override-range-lint: cannot load 'semver' from ${dir} (is the workspace installed?): ${err.message}\n`
    );
    process.exit(2);
  }

  let floors;
  try {
    floors = parseMinimumVersionSpecs(args.minimumVersionSpecs, semver);
  } catch (err) {
    process.stderr.write(`override-range-lint: ${err.message}\n`);
    process.exit(2);
  }

  let result;
  try {
    result = findViolations(lock, overrides ?? {}, semver);
  } catch (err) {
    process.stderr.write(`override-range-lint: ${err.message}\n`);
    process.exit(2);
  }

  let minimumVersionViolations;
  try {
    minimumVersionViolations = findMinimumVersionViolations(
      lock,
      floors,
      semver,
      dir,
      pkg,
      args.verifyInstalledVersions
    );
  } catch (err) {
    process.stderr.write(`override-range-lint: ${err.message}\n`);
    process.exit(2);
  }

  const { violations, skipped } = result;

  const baseline = existsSync(baselinePath) ? readJson(baselinePath, 'baseline') : null;

  if (args.updateBaseline && minimumVersionViolations.length === 0) {
    writeBaseline(baselinePath, violations);
    log(`override-range lint: wrote ${violations.length} accepted violation(s) to ${baselinePath}`);
    process.exit(0);
  }

  const { fresh, known, stale } = applyBaseline(violations, baseline);
  const expiredNestedOverrides = args.failOnStale
    ? findExpiredNestedOverrides(lock, overrides ?? {}, semver)
    : [];

  if (args.json) {
    log(
      JSON.stringify(
        { dir: args.dir, fresh, known, stale, expiredNestedOverrides, skipped, minimumVersionViolations },
        null,
        2
      )
    );
    process.exit(
      fresh.length > 0 ||
        minimumVersionViolations.length > 0 ||
        (args.failOnStale && (stale.length > 0 || expiredNestedOverrides.length > 0))
        ? 1
        : 0
    );
  }

  for (const s of skipped) {
    log(`  skipped   ${s.dep} via ${s.consumer} — ${s.reason}`);
  }

  // Stale baseline rows are cruft, not risk: warn so the file self-cleans
  // rather than accumulating permanent exemptions nobody re-reads.
  for (const s of stale) {
    log(
      `  WARN  baseline row no longer occurs, prune it: ${s.dep} via ${s.consumer} (declared ${s.range}, was resolving ${s.resolved})`
    );
  }

  for (const expired of expiredNestedOverrides) {
    const detail =
      expired.reason === 'missing-edge'
        ? `no installed ${expired.parent} declares ${expired.dep}`
        : `every installed ${expired.parent} range already accepts ${expired.forced}`;
    log(`  EXPIRED nested override ${expired.parent} -> ${expired.dep}@${expired.forced}: ${detail}`);
  }

  for (const v of minimumVersionViolations) {
    log(`  BELOW MINIMUM  ${v.dep}@${v.resolved} at ${v.path} is below minimum ${v.minimum}`);
  }

  if (fresh.length === 0 && minimumVersionViolations.length === 0) {
    if (args.failOnStale && (stale.length > 0 || expiredNestedOverrides.length > 0)) {
      const expiryProblems = [
        stale.length > 0 && `${stale.length} stale baseline row(s)`,
        expiredNestedOverrides.length > 0 &&
          `${expiredNestedOverrides.length} expired nested containment override(s)`,
      ]
        .filter(Boolean)
        .join('; ');
      log(
        `override-range lint: ${expiryProblems} in ${args.dir}; remove the obsolete override and baseline.`
      );
      process.exit(1);
    }
    const n = Object.keys(overrides ?? {}).length;
    const suffix = known.length > 0 ? ` (${known.length} baselined)` : '';
    log(
      `override-range lint: ${n} override(s) in ${args.dir}; no new consumer forced outside its declared range${suffix}.`
    );
    process.exit(0);
  }

  // Two kinds with genuinely different meanings, so they are reported
  // separately. Lumping them under one banner made an unevaluatable
  // declaration read as a measured version skew (review, PR #2768).
  const skews = fresh.filter((v) => v.kind !== 'unparseable-range');
  const unevaluatable = fresh.filter((v) => v.kind === 'unparseable-range');

  log(`override-range lint: ${fresh.length + minimumVersionViolations.length} NEW finding(s) in ${args.dir}:`);

  if (minimumVersionViolations.length > 0) {
    log('');
    log(`  ── ${minimumVersionViolations.length} resolved package(s) below a required minimum`);
  }

  if (skews.length > 0) {
    log('');
    log(`  ── ${skews.length} consumer(s) forced outside the range they declare`);
    for (const v of skews) {
      log(`  ${v.kind}`);
      log(`    package   ${v.dep}`);
      log(`    consumer  ${v.consumer}`);
      log(`    declares  ${v.range}`);
      log(`    resolves  ${v.resolved}`);
    }
    log('');
    log('  An override is forcing a consumer onto a version it never declared support');
    log('  for. This is a question, not automatically a bug — forcing can be deliberate.');
    log('  Decide, do not inherit: re-check the override against `npm ls <pkg> --all` for');
    log('  the real consumer set, and against the per-line first_patched of EVERY advisory');
    log('  if the override exists for security reasons. If obsolete, remove it (PR #2709).');
  }

  if (unevaluatable.length > 0) {
    log('');
    log(`  ── ${unevaluatable.length} declaration(s) this lint cannot evaluate`);
    for (const v of unevaluatable) {
      log(`  ${v.kind}`);
      log(`    package   ${v.dep}`);
      log(`    consumer  ${v.consumer}`);
      log(`    declares  ${v.range}   <- npm accepts this; semver cannot parse it`);
      log(`    resolves  ${v.resolved}`);
    }
    log('');
    log('  These are NOT measured skews — no comparison was possible. They block anyway,');
    log('  because the consumer asked for something off-registry (a dist-tag, a git ref, a');
    log('  workspace protocol) and the override handed it a registry version instead. That');
    log('  needs a human to look, not a silent skip. Deliberately not treated like the');
    log('  file:/link: RESOLUTION skip: there, nothing is comparable; here, the override is');
    log('  at its most surprising.');
  }

  log('');
  log(`  Intended? Record it: node scripts/security/override-range-lint.mjs --dir ${args.dir} --update-baseline`);
  process.exit(1);
}

// Robust direct-invocation guard, mirroring npm-audit-gate.mjs: the string
// idiom `import.meta.url === \`file://${process.argv[1]}\`` silently
// mis-compares for symlinked / spaced / non-ASCII paths, and a gate that
// no-ops on a symlinked runner passes vacuously.
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
