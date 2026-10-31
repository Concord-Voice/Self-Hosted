import { randomUUID } from 'node:crypto';
import { appendFile, readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

export const ANALYSIS_KEYS = [
  'codeql_actions', 'codeql_go', 'codeql_js', 'eslint', 'govulncheck', 'osv',
  'semgrep_custom', 'semgrep_community', 'semgrep_canary', 'sonar',
  'sonar_force_all', 'sonar_desktop', 'sonar_control_plane',
  'sonar_media_plane', 'sonar_admin',
];
export const COMPONENT_KEYS = [
  'services', 'spa', 'selfhost', 'desktop', 'control_plane', 'media_plane',
  'media_plane_tests', 'admin', 'production_docker', 'dev_stack', 'native', 'go_lint',
];
export const ADVISORY_KEYS = [
  'go_toolchain', 'openapi', 'spa_env', 'deployment', 'selfhost', 'feature_wiring',
  'adr_numbers', 'secrets_contract', 'dispatch_trust', 'go_test_contracts',
  'review_contracts', 'tooling_mirrors', 'graphify_contracts', 'native_ioc',
  'sonar_profile', 'changelog', 'dev_checks', 'workflow_lint',
];
const ADVISORY_FILTER_KEYS = ADVISORY_KEYS.map(key => key === 'selfhost' ? 'selfhost_guard' : key);
const STATIC_GUARD_KEYS = ADVISORY_KEYS.filter(key => key !== 'workflow_lint');
export const WORKER_KEYS = ['worker_apple', 'worker_invite'];
export const NPM_LICENSE_KEYS = ['license_desktop', 'license_media', 'license_admin'];
const FILTER_KEYS = [...ANALYSIS_KEYS, ...COMPONENT_KEYS, ...ADVISORY_FILTER_KEYS,
  ...WORKER_KEYS, ...NPM_LICENSE_KEYS, 'license_go', 'all_changed'];

const CODEQL = {
  codeql_actions: { language: 'actions', runner: 'ubuntu-24.04', 'build-mode': 'none' },
  codeql_go: { language: 'go', runner: 'ubuntu-24.04', 'build-mode': 'manual' },
  codeql_js: { language: 'javascript-typescript', runner: 'ubuntu-24.04', 'build-mode': 'none' },
};

const sha = value => typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);

export function resolveScanMode(value, runId, runAttempt, now = Date.now()) {
  if (typeof value !== 'string' || !value.startsWith('required-relevant')) return value;
  const match = /^required-relevant:([1-9]\d*):([1-9]\d*):([1-9]\d*)$/.exec(value);
  if (!match) return 'required-full';
  const expiry = Number(match[3]);
  const seconds = Math.floor(now / 1000);
  return match[1] === runId && match[2] === runAttempt &&
    Number.isSafeInteger(expiry) && expiry > seconds && expiry <= seconds + 3600
    ? 'required-relevant' : 'required-full';
}

export function composeSelection(filters, metadata, options) {
  const mode = options && typeof options === 'object' && !Array.isArray(options)
    ? options.mode : undefined;
  const policySha = options && typeof options === 'object' && !Array.isArray(options)
    ? options.policySha : undefined;
  const validMetadata = metadata !== null && typeof metadata === 'object' && !Array.isArray(metadata);
  const validFilters = filters !== null && typeof filters === 'object' && !Array.isArray(filters);
  const validPR = validMetadata && metadata.event_name === 'pull_request'
    && ['head_sha', 'base_sha', 'merge_sha'].every(key => sha(metadata[key]))
    && Number.isSafeInteger(metadata.changed_files)
    && metadata.changed_files >= 0 && metadata.changed_files <= 3000;
  const validPush = validMetadata && metadata.event_name === 'push'
    && sha(metadata.before) && sha(metadata.after)
    && metadata.before !== '0'.repeat(40) && metadata.after !== '0'.repeat(40)
    && metadata.history_available === true;
  const validSchedule = validMetadata && metadata.event_name === 'schedule' && sha(metadata.head_sha);
  const validMode = ['legacy', 'required-full', 'required-relevant'].includes(mode);
  const validFilterSet = validFilters
    && Object.keys(filters).length === FILTER_KEYS.length
    && FILTER_KEYS.every(key => filters[key] === 'true' || filters[key] === 'false')
    && Object.keys(filters).every(key => FILTER_KEYS.includes(key));
  const known = validMode && sha(policySha)
    && (validPR || validPush || validSchedule)
    && validMetadata && typeof metadata.hotfix === 'boolean'
    && validFilterSet;

  if (!known) return { version: 1, known: false, mode, policy_sha: policySha };

  const selected = Object.fromEntries(ANALYSIS_KEYS.map(key => [key, filters[key] === 'true']));
  const full = mode !== 'required-relevant' || metadata.hotfix || validSchedule;
  const forceCoverage = selected.sonar_force_all || (mode !== 'legacy' && full);
  const coverage_required = Object.fromEntries(
    ['desktop', 'control_plane', 'media_plane', 'admin'].map(key => [key,
      forceCoverage || selected[`sonar_${key}`] || filters[key] === 'true']),
  );
  selected.sonar ||= Object.values(coverage_required).some(Boolean);

  if (full) {
    for (const key of Object.keys(CODEQL).concat([
      'eslint', 'govulncheck', 'osv', 'semgrep_custom', 'semgrep_community', 'semgrep_canary', 'sonar',
    ])) selected[key] = true;
  }

  if (validPR) {
    for (const key of Object.keys(CODEQL)) selected[key] = false;
  }

  const result = {
    version: 1,
    known: true,
    mode,
    ...(validPR ? {
      head_sha: metadata.head_sha,
      base_sha: metadata.base_sha,
      merge_sha: metadata.merge_sha,
      changed_files: metadata.changed_files,
    } : validPush ? { before: metadata.before, after: metadata.after } : { head_sha: metadata.head_sha }),
    policy_sha: policySha,
    hotfix: metadata.hotfix,
    selected,
    advisory: Object.fromEntries(ADVISORY_KEYS.map((key, index) =>
      [key, filters[ADVISORY_FILTER_KEYS[index]] === 'true' ||
        (key === 'selfhost' && filters.selfhost === 'true')])),
    workers: WORKER_KEYS.filter(key => filters[key] === 'true')
      .map(key => key === 'worker_apple' ? 'apple-sso-bridge' : 'invite-landing'),
    npm_workspaces: NPM_LICENSE_KEYS.filter(key => filters[key] === 'true')
      .map(key => ({ license_desktop: '.', license_media: 'services/media-plane',
        license_admin: 'client/admin' })[key]),
    license_go: filters.license_go === 'true',
    coverage_required,
    codeql_matrix: JSON.stringify({ include: Object.keys(CODEQL)
      .filter(key => selected[key]).map(key => CODEQL[key]) }),
  };
  result.advisory.static_guards = STATIC_GUARD_KEYS.some(key => result.advisory[key]);
  return result;
}

async function writeOutput(path, name, value) {
  const delimiter = `CI_SCAN_${randomUUID().replaceAll('-', '')}`;
  await appendFile(path, `${name}<<${delimiter}\n${value}\n${delimiter}\n`, 'utf8');
}

async function main() {
  const inputPath = process.argv[2];
  if (!inputPath || !process.env.GITHUB_OUTPUT) {
    throw new Error('usage: ci-scan-relevance.mjs <input.json> with GITHUB_OUTPUT set');
  }
  let input;
  try {
    input = JSON.parse(await readFile(inputPath, 'utf8'));
  } catch {
    input = null;
  }
  const validInput = input !== null && typeof input === 'object' && !Array.isArray(input);
  const rawMode = validInput && (input.mode === undefined || input.mode === '') ? 'legacy'
    : validInput ? input.mode : undefined;
  const mode = resolveScanMode(rawMode, process.env.GITHUB_RUN_ID, process.env.GITHUB_RUN_ATTEMPT);
  const selection = composeSelection(validInput ? input.filters : undefined,
    validInput ? input.metadata : undefined, {
      mode,
      policySha: validInput ? input.policy_sha : undefined,
    });
  await writeOutput(process.env.GITHUB_OUTPUT, 'known', String(selection.known));
  await writeOutput(process.env.GITHUB_OUTPUT, 'selection', JSON.stringify(selection));
  await writeOutput(process.env.GITHUB_OUTPUT, 'codeql_matrix', selection.codeql_matrix ?? '{"include":[]}');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => {
    process.stderr.write(`ci-scan-relevance: ${error.message}\n`);
    process.exitCode = 1;
  });
}
