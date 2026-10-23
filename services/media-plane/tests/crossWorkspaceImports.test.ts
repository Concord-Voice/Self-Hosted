/**
 * CI path-filter coverage for cross-workspace imports.
 *
 * `cameraLayerPolicyParity.test.ts` lives in this workspace but imports the
 * client copy of the camera layer ladder (`packages/protocol`), because pinning the two
 * implementations in agreement is the entire point of that file. CI, however,
 * gates the media-plane test job on a path filter that listed only
 * `services/media-plane/**` — so a PR changing only the desktop policy skipped
 * the one test that guards it, and the guard was structurally blind to half of
 * what it guards.
 *
 * That is not a hypothetical: replacing the desktop copy's `Math.max(w, h)`
 * with a bare width still passes the desktop-side policy suite, whose own
 * fixtures are landscape-only. The parity suite is what catches it, and the
 * parity suite would not have run. Found by Codex on #3277.
 *
 * A comment in two workflow files cannot hold this — the next cross-workspace
 * import would be added without one, and nothing would say so. This test
 * derives the requirement from the imports themselves, so the filter has to
 * follow the code rather than the other way round.
 *
 * This repository's CI runs the media-plane job on every change, with no path
 * gate, so a client-policy change can never skip it. The guard pins that: a
 * job `if:` gate or a workflow path filter brings the skip back, and must then
 * cover every escaping import below.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, resolve, relative, sep } from 'node:path';

const TESTS_DIR = resolve(__dirname);
const MEDIA_PLANE_ROOT = resolve(__dirname, '..');
const REPO_ROOT = resolve(__dirname, '..', '..', '..');

const WORKFLOW = '.github/workflows/ci.yml';

/** Every `.test.ts` under this directory, recursively. */
function testFiles(dir: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      found.push(...testFiles(full));
      continue;
    }
    if (entry.endsWith('.test.ts')) found.push(full);
  }
  return found;
}

/**
 * Relative-specifier imports that resolve OUTSIDE services/media-plane.
 *
 * Deliberately ignores bare specifiers (`vitest`, `node:fs`) and the `@/` alias
 * — the first are packages, the second resolves inside this workspace by
 * config. Only a `../`-escape can reach another workspace's source.
 */
function crossWorkspaceImports(file: string): string[] {
  const source = readFileSync(file, 'utf8');
  // BOTH quote styles. Prettier enforces single quotes here, so a double-quoted
  // import does not survive formatting today — but a guard that depends on a
  // formatting convention has the exact blind spot it exists to close: the
  // import would go undetected and this test would pass while missing it
  // (Gitar, #3277).
  const specifiers = [...source.matchAll(/from\s+['"]([^'"]+)['"]/g)].map((match) => match[1]);
  const escaped: string[] = [];

  for (const specifier of specifiers) {
    if (!specifier.startsWith('.')) continue;
    const resolved = resolve(file, '..', specifier);
    if (resolved.startsWith(MEDIA_PLANE_ROOT + sep)) continue;
    // Normalise to a repo-relative path with forward slashes, and restore the
    // .ts extension the specifier omits.
    const repoRelative = relative(REPO_ROOT, resolved).split(sep).join('/');
    escaped.push(repoRelative.endsWith('.ts') ? repoRelative : `${repoRelative}.ts`);
  }
  return escaped;
}

/**
 * Whether a `paths-filter` glob covers an exact repo-relative file path.
 *
 * Models exactly TWO shapes — an exact path, and a trailing `/**
 * The `if:` block of one job, as raw text.
 *
 * Text rather than a parsed tree because neither `yaml` nor `js-yaml` is a
 * dependency of this workspace, and adding one to assert five string literals
 * is a poor trade. The block is delimited the same way `mediaPlaneTestFilterPaths`
 * delimits the filter: start at the job key, stop at the next key of equal
 * indentation.
 */
function jobIfBlock(workflowSource: string, jobName: string): string {
  const lines = workflowSource.split('\n');
  const start = lines.findIndex((line) => line === `  ${jobName}:`);
  expect(start, `no job named ${jobName}`).toBeGreaterThan(-1);

  const body: string[] = [];
  let inIf = false;
  for (const line of lines.slice(start + 1)) {
    if (/^ {2}\S/.test(line)) break; // next job
    if (/^ {4}if:/.test(line)) {
      inIf = true;
      body.push(line);
      continue;
    }
    if (inIf) {
      // The `if:` value ends at the next key at the same indentation.
      if (/^ {4}\S/.test(line)) break;
      body.push(line);
    }
  }
  return body.join('\n');
}

describe('CI runs this workspace tests for every change to their dependencies', () => {
  it('finds the cross-workspace imports it exists to protect', () => {
    const all = testFiles(TESTS_DIR).flatMap(crossWorkspaceImports);
    // Vacuity floor. With zero escaping imports every assertion below is
    // trivially satisfied, and this guard would pass while guarding nothing —
    // the exact failure mode it was written in response to.
    expect(all.length).toBeGreaterThan(0);
    expect(all).toContain('packages/protocol/src/renderer/services/voice/remoteVideoLayerPolicy.ts');
  });

  it('runs the media-plane job with no path or job gate', () => {
    const ci = readFileSync(join(REPO_ROOT, WORKFLOW), 'utf8');
    // A workflow-level path filter would skip every job for a client-only change.
    expect(ci).not.toMatch(/^\s+paths(-ignore)?:/m);
    expect(jobIfBlock(ci, 'media-plane')).toBe('');
  });
});
