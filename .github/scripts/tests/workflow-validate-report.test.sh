#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
REPORTER="$REPO_ROOT/.github/scripts/workflow-validate-report.py"
FIXTURE="$(mktemp -d "${TMPDIR:-/tmp}/workflow-validate-test.XXXXXX")"
trap 'rm -rf "$FIXTURE"' EXIT
while IFS= read -r git_var; do unset "$git_var"; done < <(git rev-parse --local-env-vars)

pass=0
fail=0
check() {
  local label="$1"; shift
  if "$@"; then printf '  ✓ %s\n' "$label"; pass=$((pass + 1));
  else printf '  ✗ %s' "$label" >&2; [[ -z "${OUTPUT:-}" ]] || printf ' (%s)' "$OUTPUT" >&2; printf '\n' >&2; fail=$((fail + 1)); fi
}
has() { grep -Fq -- "$2" <<<"$1"; }
escaped_cells() {
  has "$ESCAPED" 'prod\|' && has "$ESCAPED" '&lt;tag&gt;' && has "$ESCAPED" '&#96;' \
    && has "$ESCAPED_SUMMARY" 'prod\|' && has "$ESCAPED_SUMMARY" '&lt;tag&gt;' \
    && ! has "$ESCAPED" '](' && ! has "$ESCAPED" '@fixture' && ! has "$ESCAPED" '*bold*' \
    && ! has "$ESCAPED_SUMMARY" '](' && ! has "$ESCAPED_SUMMARY" '@fixture' \
    && ! has "$ESCAPED_SUMMARY" '*bold*'
}
committed_only() {
  has "$WORKTREE_CHECK_COMMENT" 'production-env' \
    && ! has "$WORKTREE_CHECK_COMMENT" 'WORKTREE\_ONLY\_SECRET'
}
fixed_parse_error() {
  local report_text="$(cat "$COMMENT" "$SUMMARY" 2>/dev/null || true)"
  has "$report_text" 'parse-error' && ! has "$report_text" 'jobs: [' \
    && ! has "$report_text" 'YAML parse error in'
}
no_marker() { [[ ! -e "$REPO/marker-output" ]]; }
no_parser_marker() { [[ ! -e "$MARKER" ]]; }
no_untrusted_json() { ! has "$(cat "$SUMMARY" 2>/dev/null || true)" 'not-json'; }

new_repo() {
  REPO="$FIXTURE/$1"
  mkdir -p "$REPO/.github/workflows"
  git -C "$REPO" init -q -b main
  git -C "$REPO" config user.name Fixture
  git -C "$REPO" config user.email fixture@example.invalid
  git -C "$REPO" config commit.gpgsign false
  cp "$SCRIPT_DIR/fixtures/workflow-validate/staging-only.yml" "$REPO/.github/workflows/base.yml"
  git -C "$REPO" add .
  git -C "$REPO" commit -qm base
  BASE_SHA="$(git -C "$REPO" rev-parse HEAD)"
}
commit_head() {
  git -C "$REPO" add -A
  git -C "$REPO" -c commit.gpgsign=false commit -qm head
  HEAD_SHA="$(git -C "$REPO" rev-parse HEAD)"
}
report() {
  SUMMARY="$REPO/summary.md"; COMMENT="$REPO/comment.md"
  set +e
  (cd "$REPO" && python3 "$REPORTER" "$BASE_SHA" "$HEAD_SHA" "$SUMMARY" "$COMMENT") \
    >"$REPO/out" 2>"$REPO/err"
  RC=$?
  set -e
  OUTPUT="$(cat "$REPO/out" "$REPO/err")"
}
success() { [[ "$RC" == 0 ]]; }
refused() { [[ "$RC" != 0 ]] && has "$(cat "$SUMMARY" 2>/dev/null || true)" "$1" && [[ ! -s "$COMMENT" ]]; }

echo 'Reporter behavior:'
new_repo happy
cp "$SCRIPT_DIR/fixtures/workflow-validate/many-secrets.yml" "$REPO/.github/workflows/base.yml"
python3 - "$REPO/.github/workflows/base.yml" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
p.write_text(p.read_text(encoding='utf-8').replace('environment: staging', 'environment: production'), encoding='utf-8')
PY
commit_head
report
check 'changed workflows are read from Git objects' success
check 'V1 production warning and V7 secret names are present' has "$(cat "$COMMENT" 2>/dev/null || true)" 'production-env'
check 'V7 fixture secret is reported as escaped literal text' has "$(cat "$COMMENT" 2>/dev/null || true)" 'JWT\_SECRET'
python3 - "$REPO/.github/workflows/base.yml" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
source = p.read_text(encoding='utf-8')
p.write_text(source.replace('environment: production', 'environment: staging')
                 .replace('secrets.JWT_SECRET', 'secrets.WORKTREE_ONLY_SECRET'), encoding='utf-8')
PY
report
WORKTREE_CHECK_COMMENT="$(cat "$COMMENT" 2>/dev/null || true)"
check 'report uses committed Git object values over changed working-tree YAML' success
check 'committed production environment and secret survive worktree edits' has "$WORKTREE_CHECK_COMMENT" 'JWT\_SECRET'
check 'uncommitted environment and secret values are excluded' committed_only

new_repo added
cp "$SCRIPT_DIR/fixtures/workflow-validate/staging-only.yml" "$REPO/.github/workflows/ci_build.yml"
commit_head; report
check 'added workflow report succeeds' success
added_filename_is_literal() {
  local rendered="$(cat "$COMMENT" 2>/dev/null || true)"
  has "$rendered" '&#46;github/workflows/ci\_build&#46;yml' && ! has "$rendered" '`'
}
check 'added workflow filename is cleaned outside inline code spans' added_filename_is_literal

new_repo autolink
printf '%s\n' 'name: fixture' 'jobs:' '  check:' \
  '    environment: "staging https://example.invalid http://example.invalid www.example.invalid"' \
  > "$REPO/.github/workflows/base.yml"
commit_head; report
urls_are_literal() {
  for rendered in "$(cat "$COMMENT" 2>/dev/null || true)" "$(cat "$SUMMARY" 2>/dev/null || true)"; do
    has "$rendered" 'https&#58;//example&#46;invalid' \
      && has "$rendered" 'http&#58;//example&#46;invalid' \
      && has "$rendered" 'www&#46;example&#46;invalid' \
      && ! has "$rendered" 'https://example.invalid' \
      && ! has "$rendered" 'http://example.invalid' \
      && ! has "$rendered" 'www.example.invalid' || return 1
  done
}
check 'bare HTTP, HTTPS, and www values are not auto-linked in comment or summary' urls_are_literal

new_repo no-op
printf '%s\n' ordinary > "$REPO/readme.txt"; commit_head; report
check 'non-workflow change succeeds as no-op' success
check 'no-op comment is empty' test ! -s "$COMMENT"

new_repo deletion
git -C "$REPO" rm -q .github/workflows/base.yml; commit_head; report
check 'deleted workflows are ignored' success
check 'deletion creates no comment' test ! -s "$COMMENT"

new_repo rename
git -C "$REPO" mv .github/workflows/base.yml .github/workflows/renamed.yml; commit_head; report
check 'rename into workflow directory is treated as addition' success
check 'renamed workflow receives not-dispatchable notice' has "$(cat "$COMMENT" 2>/dev/null || true)" 'renamed&#46;yml'

new_repo malformed
printf '%s\n' 'jobs: [' > "$REPO/.github/workflows/base.yml"; commit_head; report
check 'malformed YAML remains a report row' success
check 'malformed YAML uses fixed text without raw input or parser diagnostics' fixed_parse_error

new_repo escaped
printf '%s\n' 'jobs:' '  check:' '    environment: "prod|` <tag> [link](https://example.invalid) @fixture *bold*"' \
  '    steps:' '      - run: echo "${{ secrets.FIXTURE_TOKEN }}"' > "$REPO/.github/workflows/base.yml"
commit_head; report
ESCAPED="$(cat "$COMMENT" 2>/dev/null || true)"
ESCAPED_SUMMARY="$(cat "$SUMMARY" 2>/dev/null || true)"
check 'special characters remain escaped and literal in report cells' escaped_cells

new_repo multiline
printf '%s\n' 'name: fixture' 'jobs:' '  check:' \
  '    environment: "production\n| Forged | row | secrets |"' > "$REPO/.github/workflows/base.yml"
commit_head; report
MULTILINE_COMMENT="$(cat "$COMMENT" 2>/dev/null || true)"
MULTILINE_SUMMARY="$(cat "$SUMMARY" 2>/dev/null || true)"
multiline_row_is_safe() {
  for rendered in "$MULTILINE_COMMENT" "$MULTILINE_SUMMARY"; do
    has "$rendered" 'production \| Forged \| row \| secrets \|' || return 1
    [[ "$(grep -c '^|' <<<"$rendered")" == 3 ]] || return 1
  done
}
check 'quoted newline is flattened into one escaped table row in both outputs' multiline_row_is_safe

new_repo unicode-format
python3 - "$REPO/.github/workflows/base.yml" <<'PY'
import pathlib, sys
pathlib.Path(sys.argv[1]).write_text(
    'name: fixture\njobs:\n  check:\n    environment: "staging\u200bproduction"\n',
    encoding='utf-8',
)
PY
commit_head; report
unicode_format_is_safe() {
  python3 - "$COMMENT" "$SUMMARY" <<'PY'
import pathlib, sys
reports = [pathlib.Path(name).read_text(encoding='utf-8') for name in sys.argv[1:]]
if any('\u200b' in report for report in reports):
    raise SystemExit(1)
if any('staging' not in report or 'production' not in report or 'production-env' not in report
       for report in reports):
    raise SystemExit(1)
PY
}
check 'Unicode Cf controls are removed while visible environment text remains' unicode_format_is_safe

new_repo invalid-sha
BASE_SHA=bad; HEAD_SHA="$(git -C "$REPO" rev-parse HEAD)"; report
check 'invalid SHA refusal is diagnosed and leaves no comment' refused 'invalid base SHA'
BASE_SHA="$(git -C "$REPO" rev-parse HEAD)"; HEAD_SHA=bad; report
check 'invalid head SHA refusal is diagnosed and leaves no comment' refused 'invalid head SHA'

new_repo missing-object
BASE_SHA=0000000000000000000000000000000000000001; HEAD_SHA="$(git -C "$REPO" rev-parse HEAD)"; report
check 'missing Git object refusal is diagnosed and leaves no comment' refused 'invalid commit'

new_repo symlink
printf '%s\n' target > "$REPO/target.yml"; ln -s ../../target.yml "$REPO/.github/workflows/link.yml"
commit_head; report
check 'symlink workflow is refused' refused 'non-regular workflow file'

new_repo mode-change
printf '%s\n' target > "$REPO/target.yml"
rm "$REPO/.github/workflows/base.yml"
ln -s ../../target.yml "$REPO/.github/workflows/base.yml"
commit_head; report
check 'regular workflow changed to a symlink is refused' refused 'non-regular workflow file'

new_repo control-path
bad_name="$(printf 'bad\nname.yml')"
printf '%s\n' harmless > "$REPO/.github/workflows/$bad_name"
commit_head; report
check 'control-character basename is refused' refused 'unsupported workflow path'

new_repo large-blob
dd if=/dev/zero of="$REPO/.github/workflows/large.yml" bs=1048577 count=1 2>/dev/null
commit_head; report
check 'workflow blob above 1 MiB is refused' refused 'workflow input too large'

new_repo total-size
for i in $(seq 1 9); do dd if=/dev/zero of="$REPO/.github/workflows/large-$i.yml" bs=1000000 count=1 2>/dev/null; done
commit_head; report
check 'total workflow input above 8 MiB is refused' refused 'workflow input too large'

new_repo many-files
for i in $(seq -w 1 65); do printf 'name: f%s\n' "$i" > "$REPO/.github/workflows/f$i.yml"; done
commit_head; report
check 'more than 64 changed workflows are refused' refused 'too many changed workflow files'

new_repo changed-parser-marker
printf '%s\n' 'name: fixture' 'jobs:' '  check:' '    environment: staging' > "$REPO/.github/workflows/base.yml"
mkdir -p "$REPO/.github/scripts"
printf '%s\n' '# inert changed parser marker' 'touch marker-output' > "$REPO/.github/scripts/parse-dispatched-workflow.sh"
commit_head; report
check 'changed PR parser marker fixture yields a valid static report' success
check 'changed PR parser marker is not executed' no_marker
check 'changed PR parser fixture content is never consumed as executable code' has "$(cat "$COMMENT" 2>/dev/null || true)" 'staging'

new_repo too-large-comment
python3 - "$REPO/.github/workflows/base.yml" <<'PY'
import sys
open(sys.argv[1], 'w', encoding='utf-8').write('jobs:\n  check:\n    environment: "' + ('x' * 33000) + '"\n')
PY
commit_head; report
check 'report above 32 KiB is refused' refused 'comment too large'

exercise_parser_stub() {
  local mode="$1"
  local cloned="$FIXTURE/reporter-$mode"
  mkdir -p "$cloned"
  cp "$REPORTER" "$cloned/workflow-validate-report.py"
  if [[ "$mode" == timeout ]]; then
    python3 - "$cloned/workflow-validate-report.py" <<'PY'
import pathlib, sys
p = pathlib.Path(sys.argv[1])
source = p.read_text(encoding='utf-8')
if 'timeout=15' not in source:
    raise SystemExit('reporter timeout literal was not found')
p.write_text(source.replace('timeout=15', 'timeout=0.2'), encoding='utf-8')
PY
    cat > "$cloned/parse-dispatched-workflow.sh" <<'SH'
#!/usr/bin/env bash
python3 - "$MARKER" <<'PY'
import pathlib, sys, time
time.sleep(0.7)
pathlib.Path(sys.argv[1]).touch()
PY
SH
  else
    printf '%s\n' '#!/usr/bin/env bash' "printf 'not-json\\n'" > "$cloned/parse-dispatched-workflow.sh"
  fi
  chmod +x "$cloned/parse-dispatched-workflow.sh"
}

new_repo invalid-parser-output
printf '%s\n' 'name: fixture' 'jobs:' '  check:' '    environment: staging' > "$REPO/.github/workflows/base.yml"
commit_head
exercise_parser_stub output
SUMMARY="$REPO/summary.md"; COMMENT="$REPO/comment.md"
set +e
(cd "$REPO" && python3 "$FIXTURE/reporter-output/workflow-validate-report.py" \
  "$BASE_SHA" "$HEAD_SHA" "$SUMMARY" "$COMMENT") >"$REPO/out" 2>"$REPO/err"
RC=$?
set -e
OUTPUT="$(cat "$REPO/out" "$REPO/err")"
check 'invalid trusted-parser JSON is refused with a fixed diagnostic' refused 'invalid parser output'
check 'invalid parser output is not copied into the report' no_untrusted_json

new_repo timeout
printf '%s\n' 'name: fixture' 'jobs:' '  check:' '    environment: staging' > "$REPO/.github/workflows/base.yml"
commit_head
exercise_parser_stub timeout
MARKER="$REPO/parser-child-ran"; export MARKER
SUMMARY="$REPO/summary.md"; COMMENT="$REPO/comment.md"
set +e
(cd "$REPO" && python3 "$FIXTURE/reporter-timeout/workflow-validate-report.py" \
  "$BASE_SHA" "$HEAD_SHA" "$SUMMARY" "$COMMENT") >"$REPO/out" 2>"$REPO/err"
RC=$?
set -e
OUTPUT="$(cat "$REPO/out" "$REPO/err")"
sleep 1
check 'parser timeout is refused and leaves no comment' refused 'workflow parser timed out'
check 'parser timeout stops its child process' no_parser_marker
unset MARKER

echo; echo 'Trusted workflow policy:'
python3 - "$REPO_ROOT" "$FIXTURE" <<'PY'
import os, pathlib, subprocess, sys, tempfile, textwrap
import yaml
root = pathlib.Path(sys.argv[1]); temp = pathlib.Path(sys.argv[2])
doc = yaml.load((root / '.github/workflows/workflow-validate.yml').read_text(), Loader=yaml.BaseLoader)
errors = []
def need(ok, message):
    if not ok: errors.append(message)
events = doc.get('on', {}); event = events.get('pull_request_target', {})
need(bool(event), 'requires pull_request_target')
need(event.get('types') == ['labeled', 'synchronize'], 'preserves labeled/synchronize events')
need(event.get('branches') == ['main'], 'restricts event to main base')
jobs = doc.get('jobs', {}); analysis = jobs.get('analyze-changed-workflows', {}); fork = jobs.get('notify-fork-pr', {})
need("github.event.pull_request.base.ref == 'main'" in analysis.get('if', ''), 'analysis explicitly gates base ref')
need("github.event.pull_request.head.repo.full_name == github.repository" in analysis.get('if', ''), 'analysis is same-repository only')
need("contains(github.event.pull_request.labels.*.name, 'workflow-validation: dispatch')" in analysis.get('if', ''),
     'analysis retains the workflow-validation label gate')
need("github.event.pull_request.base.ref == 'main'" in fork.get('if', ''), 'fork notice explicitly gates base ref')
need("contains(github.event.pull_request.labels.*.name, 'workflow-validation: dispatch')" in fork.get('if', ''),
     'fork notice retains the workflow-validation label gate')
steps = analysis.get('steps', [])
checkout = next((s for s in steps if str(s.get('uses', '')).startswith('actions/checkout@')), {})
need(checkout.get('with', {}).get('ref') == '${{ github.event.pull_request.base.sha }}', 'checkout uses base SHA')
need(checkout.get('with', {}).get('persist-credentials') == 'false', 'checkout disables persisted credentials')
need(checkout.get('with', {}).get('fetch-depth') == '0', 'checkout fetches complete history')
need(bool(__import__('re').fullmatch(r'actions/checkout@[0-9a-f]{40}(?:\s+#.*)?', str(checkout.get('uses', '')))),
     'checkout action is pinned to a full commit SHA')
need(all('head.sha' not in str(s.get('with', {}).get('ref', '')) for s in steps), 'no PR-head checkout')
need(any('--require-hashes' in str(s.get('run', '')) and '.github/requirements-pyyaml.txt' in str(s.get('run', '')) for s in steps),
     'PyYAML install uses the checked-in hash-pinned requirements')
reporter = next((s for s in steps if 'workflow-validate-report.py' in str(s.get('run', ''))), {})
need(bool(reporter), 'trusted reporter is invoked')
need('GH_TOKEN' not in reporter.get('env', {}), 'reporter receives no GitHub token')
need(reporter.get('env', {}).get('COMMENT_PATH') == '${{ runner.temp }}/workflow-validate-comment.md',
     'comment output is routed to the runner temporary directory')
publish = next((s for s in steps if s.get('id') == 'publish-report'), {})
need(all('GH_TOKEN' not in s.get('env', {}) for s in steps if s is not publish),
     'all analysis steps except publisher receive no GitHub token')
need(all('head.repo.full_name' not in str(s.get('run', '')) and 'head.ref' not in str(s.get('run', '')) for s in steps),
     'PR-supplied identity and ref values are not interpolated into shell code')
need(bool(publish), 'publisher has publish-report id')
need('GH_TOKEN' in publish.get('env', {}), 'only publisher receives GitHub token')
run = publish.get('run', '')
need('gh api' in run and 'head.sha' in run and 'base.ref' in run, 'publisher rechecks current SHA and base')
need('gh pr comment' in run and '--body-file' in run, 'publisher uses comment body file')
need(run.find('gh api') < run.find('gh pr comment'), 'fresh-head read precedes comment')
need(analysis.get('permissions', {}).get('contents') == 'read', 'analysis contents permission is read-only')
need(analysis.get('permissions', {}).get('pull-requests') == 'write', 'analysis retains comment permission')
need(fork.get('permissions', {}).get('pull-requests') == 'write', 'fork job can post rejection notice')
fork_steps = fork.get('steps', [])
need(not any(str(s.get('uses', '')).startswith('actions/checkout@') for s in fork_steps),
     'fork notice does not check out PR content')
need(not any('gh workflow run' in str(s.get('run', '')) for s in fork_steps),
     'fork path only posts its explanatory notice')
if errors:
    for e in errors: print('  ✗ ' + e, file=sys.stderr)
    raise SystemExit(1)
for label in ('protected event and branch gates', 'base-SHA checkout and no PR execution', 'token-scoped publisher'):
    print('  ✓ ' + label)

# Execute the trusted publisher block with a local gh stub for current/stale heads.
with tempfile.TemporaryDirectory(prefix='publish-test-') as d:
    d = pathlib.Path(d); log = d / 'calls'; stub = d / 'gh'
    stub.write_text('''#!/usr/bin/env bash\nset -euo pipefail\nif [[ "$1" == api ]]; then if [[ "${GH_API_MODE:-}" == fail ]]; then exit 42; elif [[ "${GH_API_MODE:-}" == invalid ]]; then printf 'not-json\\n'; else printf "{\\"head\\":{\\"sha\\":\\"%s\\"},\\"base\\":{\\"ref\\":\\"%s\\"}}\\n" "$LIVE_SHA" "$LIVE_BASE"; fi; elif [[ "$1 $2" == "pr comment" ]]; then echo called >> "$CALL_LOG"; else exit 91; fi\n''')
    stub.chmod(0o755); (d / 'body.md').write_text('safe fixture body\n')
    script = d / 'publisher.sh'; script.write_text(run)
    event_sha = 'a' * 40
    env = dict(os.environ, PATH=f'{d}:{os.environ.get("PATH", "")}', LIVE_SHA=event_sha,
               LIVE_BASE='main', EVENT_HEAD_SHA=event_sha, HEAD_SHA=event_sha, BASE_SHA='b' * 40,
               PR_BASE_REF='main', PR_NUMBER='2', RUNNER_TEMP=str(d),
               REPO='owner/repo', COMMENT_PATH=str(d / 'body.md'), CALL_LOG=str(log),
               GITHUB_STEP_SUMMARY=str(d / 'summary'))
    current = subprocess.run(['bash', str(script)], env=env, capture_output=True, text=True)
    if current.returncode or not log.exists():
        print('  ✗ current SHA is published: ' + current.stderr, file=sys.stderr); raise SystemExit(1)
    log.unlink(); env['LIVE_SHA'] = 'c' * 40
    stale = subprocess.run(['bash', str(script)], env=env, capture_output=True, text=True)
    if stale.returncode or log.exists():
        print('  ✗ stale SHA suppresses comment: ' + stale.stderr, file=sys.stderr); raise SystemExit(1)
    env['LIVE_SHA'] = event_sha; env['LIVE_BASE'] = 'release'
    changed_base = subprocess.run(['bash', str(script)], env=env, capture_output=True, text=True)
    if changed_base.returncode or log.exists():
        print('  ✗ changed base suppresses comment: ' + changed_base.stderr, file=sys.stderr); raise SystemExit(1)
    env['LIVE_BASE'] = 'main'; env['GH_API_MODE'] = 'fail'
    api_failure = subprocess.run(['bash', str(script)], env=env, capture_output=True, text=True)
    if api_failure.returncode == 0 or log.exists():
        print('  ✗ GitHub API failure stops publication: ' + api_failure.stderr, file=sys.stderr); raise SystemExit(1)
    env['GH_API_MODE'] = 'invalid'
    invalid_api = subprocess.run(['bash', str(script)], env=env, capture_output=True, text=True)
    if invalid_api.returncode == 0 or log.exists():
        print('  ✗ invalid API JSON stops publication: ' + invalid_api.stderr, file=sys.stderr); raise SystemExit(1)
    print('  ✓ current SHA publishes; stale/base-changed/API-failed/invalid-JSON paths do not publish')
PY

printf '\nPASS: %s  FAIL: %s\n' "$pass" "$fail"
[[ "$fail" == 0 ]]
