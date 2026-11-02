#!/bin/bash
# scripts/test-check-review-dispositions.sh
#
# Hermetic fixture tests for scripts/check-review-dispositions.sh. No network:
# every case feeds a CV_REVIEW_FIXTURE JSON blob matching the GraphQL response
# shape, and CV_SKIP_EXISTENCE_CHECKS=1 stubs the `gh issue view` existence probe.
#
# Each case is a FALSIFICATION of the gate: it asserts the gate goes RED for a
# specific hole and GREEN only when that hole is closed. A gate whose tests only
# ever assert the happy path proves nothing about the failure it exists to catch.
#
# Style mirrors scripts/check-env-file.test.sh.
set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/check-review-dispositions.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

# Reviewer display names come from `.bots[]`; the suite supplies its own roster
# via CV_REPO_CONFIG so it never depends on the cwd having a real repo config.
cat > "$TMP/repo-config.json" <<'ROSTER'
{"bots":[
  {"match":"gitar","name":"Gitar","login":"gitar-bot[bot]"},
  {"match":"sonarqubecloud","name":"SonarCloud","login":"sonarqubecloud[bot]"},
  {"match":"coderabbit","name":"CodeRabbit","login":"coderabbitai[bot]"},
  {"name":"Semgrep SAST","login":"github-actions[bot]","marker":"<!-- semgrep-results -->"},
  {"name":"Release Classifier","login":"github-actions[bot]","marker":"<!-- classify-release -->"},
  {"match":"copilot","name":"Copilot Code Review","login":"copilot-pull-request-reviewer[bot]"},
  {"match":"ci-machine","name":"CI Machine","login":"ci-machine"}
]}
ROSTER
export CV_REPO_CONFIG="$TMP/repo-config.json"

# run <name> <expected-exit> <fixture-json>
run() {
    local name="$1" want="$2" json="$3"
    printf '%s' "$json" > "$TMP/f.json"
    local out
    out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1)
    local got=$?
    if [ "$got" -eq "$want" ]; then
        printf '  ok   %-52s (exit %d)\n' "$name" "$got"
        pass=$((pass + 1))
    else
        printf '  FAIL %-52s (want %d, got %d)\n' "$name" "$want" "$got"
        printf '%s\n' "$out" | sed 's/^/         | /'
        fail=$((fail + 1))
    fi
}

# Reusable pieces -------------------------------------------------------------
BOT='{"author":{"__typename":"Bot","login":"coderabbitai"},"body":"nil deref here"}'
BOT_TOP='{"author":{"__typename":"Bot","login":"gitar-bot"},"body":"<kbd>Approved</kbd>"}'
# A reconciliation post naming both bots that appear in these fixtures.
# Written in the DISPLAY NAMES the skill mandates (SKILL.md 3.5.1), not raw
# logins. The original fixture used logins — a shape the real workflow never
# produces — so the suite passed 23/23 over a matcher that false-blocked every
# real reconciliation post. Test the specification's shape, not the code's.
RECON='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| CodeRabbit | Gitar |"}'

human() { printf '{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"%s"}' "$1"; }
botsay() { printf '{"author":{"__typename":"Bot","login":"%s"},"body":"%s"}' "$1" "$2"; }
thread() { # thread <resolved> <resolvedBy> <comments-json>
    printf '{"id":"T","isResolved":%s,"resolvedBy":{"login":"%s"},"path":"a/b.go","line":12,"comments":{"nodes":[%s]}}' "$1" "$2" "$3"
}
doc() { # doc <threads-json> <toplevel-comments-json>
    printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[%s]},"comments":{"nodes":[%s]}}}}}' "$1" "$2"
}

# run_out <name> <expected-substring> <fixture-json>
run_out() {
    local name="$1" want="$2" json="$3"
    printf '%s' "$json" > "$TMP/f.json"
    local out
    out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1)
    if printf '%s' "$out" | grep -qF -- "$want"; then
        printf '  ok   %-52s\n' "$name"
        pass=$((pass + 1))
    else
        printf '  FAIL %-52s (missing: %s)\n' "$name" "$want"
        printf '%s\n' "$out" | sed 's/^/         | /'
        fail=$((fail + 1))
    fi
}

echo "check-review-dispositions.sh — fixture suite"

# --- Assertion 1: thread coverage -------------------------------------------
run "clean: bot-resolved thread WITH human disposition" 0 \
    "$(doc "$(thread true 'coderabbitai[bot]' "$BOT,$(human 'Fixed.\n<!-- cv-disposition: accepted evidence: e5c34d0 -->')")" "$BOT_TOP,$RECON")"

# The measured hole: a bot closed the thread and nobody decided anything.
run "BLOCK: bot-resolved thread with NO human reply" 1 \
    "$(doc "$(thread true 'coderabbitai[bot]' "$BOT")" "$BOT_TOP,$RECON")"

# The over-strict predecessor rule would have blocked the case above this one;
# this asserts a human reply WITHOUT a marker is still not a decision on record.
run "BLOCK: human reply but no disposition marker" 1 \
    "$(doc "$(thread true 'coderabbitai[bot]' "$BOT,$(human 'yeah good point')")" "$BOT_TOP,$RECON")"

run "BLOCK: bot-authored marker does not count as human" 1 \
    "$(doc "$(thread true 'coderabbitai[bot]' "$BOT,$(botsay coderabbitai '<!-- cv-disposition: accepted evidence: deadbee -->')")" "$BOT_TOP,$RECON")"

run "BLOCK: unknown disposition verb" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: maybe evidence: e5c34d0 -->')")" "$BOT_TOP,$RECON")"

run "BLOCK: accepted without a commit sha" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: accepted evidence: soon -->')")" "$BOT_TOP,$RECON")"

# The fail-open these were missing. A marker present but carrying no `evidence:`
# key is the SECOND empty path for that field; the first sentinel patch covered
# only "no marker at all", so these two exited 0 over an undecided thread.
# Deliberately NOT using `undecided` here: that verb blocks unconditionally, so
# an undecided-shaped case is a wrong-reason green and proves nothing about the
# evidence guard.
run "BLOCK: rejected with no evidence field (fail-open)" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: rejected -->')")" "$BOT_TOP,$RECON")"

run "BLOCK: accepted with no evidence field (fail-open)" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: accepted -->')")" "$BOT_TOP,$RECON")"

run_out "no-evidence violation names the evidence guard, not a shifted column" "has no evidence: field — a/b.go:12" \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: rejected -->')")" "$BOT_TOP,$RECON")"

# Multi-word evidence is the DOCUMENTED shape for rejected ("the row it
# duplicates") and undecided ("what it waits on"). Every fixture here used a
# single token, so a regex that truncated at the first space passed the suite.
run_out "multi-word rejected evidence survives intact" "duplicate of row 3" \
    "$(doc "$(thread false 'NONE' "$BOT,$(human '<!-- cv-disposition: undecided evidence: duplicate of row 3 -->')")" "$BOT_TOP,$RECON")"

run "ok: multi-word rejected evidence is accepted" 0 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: rejected evidence: measured 0.4 ms on 500k rows -->')")" "$BOT_TOP,$RECON")"

run "ok: rejected carries free-text evidence" 0 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human 'FP.\n<!-- cv-disposition: rejected evidence: .claude/rules/backend.md -->')")" "$BOT_TOP,$RECON")"

run "ok: later disposition supersedes an earlier one" 0 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: maybe evidence: x -->'),$(human '<!-- cv-disposition: accepted evidence: e5c34d0 -->')")" "$BOT_TOP,$RECON")"

# Human-authored + human-resolved threads are self-evidently decided.
run "ok: human thread resolved by human needs no marker" 0 \
    "$(doc '{"id":"T","isResolved":true,"resolvedBy":{"login":"dev"},"path":"a/b.go","line":1,"comments":{"nodes":[{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"note to self"}]}}' "$BOT_TOP,$RECON")"

run "BLOCK: unresolved human thread still needs a decision" 1 \
    "$(doc '{"id":"T","isResolved":false,"resolvedBy":null,"path":"a/b.go","line":1,"comments":{"nodes":[{"author":{"__typename":"User","login":"teammate"},"authorAssociation":"MEMBER","body":"is this right?"}]}}' "$BOT_TOP,$RECON")"

# Regression: a MISSING row emits an empty `evidence` field. Tab is IFS
# whitespace, so `read` collapses the resulting `\t\t` into ONE delimiter and
# every later column shifts left — the violation then names ":12" as its path.
# The gate still exits 1, so an exit-code-only test cannot see this. Assert the
# rendered location instead.
run_out "regression: MISSING row reports path, not a shifted column" "a/b.go:12" \
    "$(doc "$(thread true 'coderabbitai[bot]' "$BOT")" "$BOT_TOP,$RECON")"

# `undecided` is a recorded open question: it BLOCKS, but must be distinguishable
# in the output from a thread nobody looked at. Both facts are load-bearing —
# if it stopped blocking it would be an escape hatch; if it read identically to
# MISSING there would be no reason to write it instead of mis-filing as rejected.
run "BLOCK: undecided blocks the gate (not an escape hatch)" 1 \
    "$(doc "$(thread false 'NONE' "$BOT,$(human '<!-- cv-disposition: undecided evidence: @mark-retry-policy -->')")" "$BOT_TOP,$RECON")"

run_out "undecided reports as OPEN, distinguishable from MISSING" "BLOCKING (open): awaiting @mark-retry-policy" \
    "$(doc "$(thread false 'NONE' "$BOT,$(human '<!-- cv-disposition: undecided evidence: @mark-retry-policy -->')")" "$BOT_TOP,$RECON")"

run "BLOCK: undecided still needs an evidence field" 1 \
    "$(doc "$(thread false 'NONE' "$BOT,$(human '<!-- cv-disposition: undecided -->')")" "$BOT_TOP,$RECON")"

# --- Assertion 2: deferral integrity ----------------------------------------
run "BLOCK: deferred with a non-numeric issue ref" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: deferred evidence: later -->')")" "$BOT_TOP,$RECON")"

run "ok: deferred with a well-formed issue ref" 0 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: deferred evidence: #1234 -->')")" "$BOT_TOP,$RECON")"

# --- Assertion 3: reconciliation post ---------------------------------------
# The master-comment coverage clause. The observed shape: zero threads,
# several top-level bot comments. A thread-only gate calls this clean.
run "BLOCK: zero threads, bot comments, NO reconciliation post" 1 \
    "$(doc "" "$BOT_TOP")"

run "ok: zero threads but reconciliation post present" 0 \
    "$(doc "" "$BOT_TOP,$RECON")"

run "BLOCK: reconciliation post omits a bot that spoke" 1 \
    "$(doc "" "$BOT_TOP,$(botsay sonarqubecloud 'QG passed'),$RECON")"

run "BLOCK: two reconciliation posts (must edit in place)" 1 \
    "$(doc "" "$BOT_TOP,$RECON,$RECON")"

# `superseded` was deleted: it validated identically to `rejected`, no fixture
# ever exercised it, and nothing branched on it. It must now read as unknown.
run "BLOCK: superseded is no longer a valid verb" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: superseded evidence: row 1 -->')")" "$BOT_TOP,$RECON")"

# --- Assertion 3 redesign: display names, aliases, author fence, exemption ---
# The regression the 6-char stem caused: prose "SonarCloud" / "Gitar" must PASS.
run "prose display names satisfy coverage (was: false BLOCKING)" 0 \
    "$(doc "" "$(botsay sonarqubecloud 'QG passed'),$(botsay gitar-bot 'verdict'),$(human '<!-- cv-review-reconciliation -->\n| SonarCloud | Gitar |')")"

# The false PASS: a bare github.com URL must NOT satisfy github-actions coverage.
run "github.com URL alone does NOT satisfy Release Classifier (was: false pass)" 1 \
    "$(doc "" "$(botsay github-actions '<!-- classify-release --> bump needed'),$(human '<!-- cv-review-reconciliation -->\n| CodeRabbit | see https://github.com/x/y/pull/1 |')")"

run "ok: 'Release Classifier' prose satisfies github-actions" 0 \
    "$(doc "" "$(botsay github-actions '<!-- classify-release --> bump needed'),$(human '<!-- cv-review-reconciliation -->\n| Release Classifier | ALL CLEAR |')")"

run "BLOCK: bot-authored reconciliation post does not count" 1 \
    "$(doc "" "$(botsay coderabbitai 'summary'),$(botsay coderabbitai '<!-- cv-review-reconciliation -->\n| CodeRabbit |')")"

# Zero reviewers spoke: requiring a post that reconciles nothing is ceremony,
# and Step 9.1 runs this gate on every PR.
run "ok: no reviewer posted at all -> post not required" 0 \
    "$(doc "" "")"

# reviews-surface-only bot (copilot-pull-request-reviewer appears there and
# nowhere else on real PRs) must still require a row.
run "BLOCK: review-summary-only bot needs a reconciliation row" 1 \
    '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]},"comments":{"nodes":[{"author":{"__typename":"User","login":"m"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| CodeRabbit |"}]},"reviews":{"nodes":[{"author":{"__typename":"Bot","login":"copilot-pull-request-reviewer"},"body":"summary"}]}}}}}'

run "ok: 'Copilot' prose satisfies the reviews-surface bot" 0 \
    '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]},"comments":{"nodes":[{"author":{"__typename":"User","login":"m"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| Copilot Code Review | no findings |"}]},"reviews":{"nodes":[{"author":{"__typename":"Bot","login":"copilot-pull-request-reviewer"},"body":"summary"}]}}}}}'

# --- Pagination: truncation is UNKNOWN, never clean --------------------------
run "FAIL-CLOSED: truncated reviewThreads is exit 2" 2 \
    '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true},"nodes":[]},"comments":{"nodes":[]},"reviews":{"nodes":[]}}}}}'

run "FAIL-CLOSED: truncated top-level comments is exit 2" 2 \
    '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]},"comments":{"pageInfo":{"hasNextPage":true},"nodes":[]},"reviews":{"nodes":[]}}}}}'

# --- Shared-login personas: the false PASS finding 3 named ------------------
# A CI login carries several personas disambiguated by marker. Keying coverage
# on the login collapsed them, so naming one satisfied the other — the same
# false PASS on the most-missed reviewer that the 6-char-stem bug produced,
# reached by a different route. Reproduced with the SHIPPED roster, no attacker.
CLASSIFY='<!-- classify-release --> bump required'
SEMGREP='<!-- semgrep-results --> 0 findings'

run "BLOCK: naming only Semgrep leaves Release Classifier unaccounted" 1 \
    "$(doc "" "$(botsay github-actions "$SEMGREP"),$(botsay github-actions "$CLASSIFY"),$(human '<!-- cv-review-reconciliation -->\n| Semgrep SAST | no findings |')")"

run "ok: naming BOTH personas of the shared login passes" 0 \
    "$(doc "" "$(botsay github-actions "$SEMGREP"),$(botsay github-actions "$CLASSIFY"),$(human '<!-- cv-review-reconciliation -->\n| Semgrep SAST | Release Classifier |')")"

run "ok: only one persona posted -> only that one is required" 0 \
    "$(doc "" "$(botsay github-actions "$SEMGREP"),$(human '<!-- cv-review-reconciliation -->\n| Semgrep SAST |')")"

# A machine USER account registered in the roster is a reviewer. `__typename`
# separates Apps from Users, not machines from humans — and the helper that
# closes this shipped DEAD once already because nothing called it and no
# fixture would have noticed.
machineuser() { printf '{"author":{"__typename":"User","login":"ci-machine"},"authorAssociation":"NONE","body":"%s"}' "$1"; }

run "BLOCK: a rostered machine USER account still owes a coverage row" 1 \
    "$(doc "" "$(machineuser 'SECURITY: hardcoded key'),$(human '<!-- cv-review-reconciliation -->\n| CodeRabbit |')")"

run "ok: naming the rostered machine account satisfies coverage" 0 \
    "$(doc "" "$(machineuser 'SECURITY: hardcoded key'),$(human '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

# A resolved thread opened by the rostered machine account stays in scope.
run "BLOCK: resolved machine-account thread with no disposition" 1 \
    "$(doc "$(thread true dev "$(machineuser 'nil deref')")" "$(human '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

run "ok: resolved machine-account thread with a disposition" 0 \
    "$(doc "$(thread true dev "$(machineuser 'nil deref'),$(human '<!-- cv-disposition: accepted evidence: e5c34d0 -->')")" "$(human '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

run "BLOCK: the machine account cannot record its own disposition" 1 \
    "$(doc "$(thread true dev "$(machineuser 'nil deref'),$(machineuser '<!-- cv-disposition: accepted evidence: e5c34d0 -->')")" "$(human '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

# Two markers in ONE body: `first` used to drop the second persona silently.
run "BLOCK: two markers in one comment require BOTH personas" 1 \
    "$(doc "" "$(botsay github-actions '<!-- semgrep-results --> and <!-- classify-release --> in one body'),$(human '<!-- cv-review-reconciliation -->\n| Semgrep SAST |')")"

run "ok: naming both personas of a two-marker comment passes" 0 \
    "$(doc "" "$(botsay github-actions '<!-- semgrep-results --> and <!-- classify-release --> in one body'),$(human '<!-- cv-review-reconciliation -->\n| Semgrep SAST | Release Classifier |')")"

# --- Roster validation: an alias is a key, so it is validated before trust ---
badroster() { # badroster <name> <expected-exit> <roster-json>
    printf '%s' "$3" > "$TMP/bad.json"
    printf '%s' "$(doc "" "$(botsay gitar-bot 'verdict'),$(human '<!-- cv-review-reconciliation -->\nNothing to see here.')")" > "$TMP/f.json"
    local out; out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_REPO_CONFIG="$TMP/bad.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1); local got=$?
    if [ "$got" -eq "$2" ]; then printf '  ok   %-52s (exit %d)\n' "$1" "$got"; pass=$((pass+1))
    else printf '  FAIL %-52s (want %d, got %d)\n' "$1" "$2" "$got"; printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1)); fi
}

badroster "STOP: single-space name is a universal alias" 2 \
    '{"bots":[{"match":"-","name":" ","login":"x"}]}'

badroster "STOP: newline in name splits into a 1-char alias" 2 \
    '{"bots":[{"match":"gitar","name":"Gitar Reviewer\ne","login":"gitar-bot[bot]"}]}'

badroster "STOP: single-char match donates its alias to the fleet" 2 \
    '{"bots":[{"match":"o","name":"Gitar","login":"x"}]}'

badroster "STOP: malformed roster is loud, not silent login-only" 2 \
    '{"bots":[{"match":'

badroster "ok: a well-formed roster is accepted" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":"gitar-bot[bot]"}]}'

# --- Roster degradation: no config means login-only matching ----------------
# Not merely "safe" — correct. An unregistered reviewer has no display name to
# accept, so requiring its literal login is the honest bar. This must not
# silently pass on a display name the roster never declared.
noroster() { # noroster <name> <expected-exit> <fixture>
    printf '%s' "$3" > "$TMP/f.json"
    local out; out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_SKIP_EXISTENCE_CHECKS=1 CV_REPO_CONFIG="$TMP/absent.json" "$SCRIPT" 999 2>&1); local got=$?
    if [ "$got" -eq "$2" ]; then printf '  ok   %-52s (exit %d)\n' "$1" "$got"; pass=$((pass+1))
    else printf '  FAIL %-52s (want %d, got %d)\n' "$1" "$2" "$got"; printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1)); fi
}

noroster "no roster: display name alone does NOT satisfy coverage" 1 \
    "$(doc "" "$(botsay sonarqubecloud 'QG passed'),$(human '<!-- cv-review-reconciliation -->\n| SonarCloud |')")"

noroster "no roster: the literal login still satisfies coverage" 0 \
    "$(doc "" "$(botsay sonarqubecloud 'QG passed'),$(human '<!-- cv-review-reconciliation -->\n| sonarqubecloud |')")"

# --- Fail-closed behavior ----------------------------------------------------
run "FAIL-CLOSED: API error body is exit 2, not a pass" 2 \
    '{"errors":[{"message":"Could not resolve to a Repository"}]}'

run "FAIL-CLOSED: null pullRequest is exit 2, not a pass" 2 \
    '{"data":{"repository":{"pullRequest":null}}}'

# The live path (no fixture) must exit 2 when `gh` itself fails — a network
# blip must never be reported as "blocking violations". Exercised with a shim.
printf '#!/bin/sh\necho "error connecting to api.github.com" >&2\nexit 1\n' > "$TMP/gh"
chmod +x "$TMP/gh"
out=$(PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1); got=$?
if [ "$got" -eq 2 ]; then
    printf '  ok   %-52s (exit 2)\n' "FAIL-CLOSED: gh failure on live path is exit 2"
    pass=$((pass + 1))
else
    printf '  FAIL %-52s (want 2, got %d)\n' "FAIL-CLOSED: gh failure on live path is exit 2" "$got"
    printf '%s\n' "$out" | sed 's/^/         | /'
    fail=$((fail + 1))
fi

# --- Authorization: identity class is not authorization ----------------------
outsider() { printf '{"author":{"__typename":"User","login":"drive-by"},"authorAssociation":"NONE","body":"%s"}' "$1"; }
member()   { printf '{"author":{"__typename":"User","login":"teammate"},"authorAssociation":"MEMBER","body":"%s"}' "$1"; }

run "BLOCK: a NONE-association commenter cannot clear a finding" 1 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(outsider '<!-- cv-disposition: rejected evidence: .claude/rules/backend.md -->')")" "$BOT_TOP,$RECON")"

run "BLOCK: a NONE-association reconciliation post does not count" 1 \
    "$(doc "" "$(botsay coderabbitai 'summary'),$(outsider '<!-- cv-review-reconciliation -->\n| CodeRabbit |')")"

run "ok: MEMBER association is accepted" 0 \
    "$(doc "$(thread true 'x[bot]' "$BOT,$(member '<!-- cv-disposition: rejected evidence: .claude/rules/backend.md -->')")" "$BOT_TOP,$RECON")"

# --- Assertion 2 gets its first real coverage --------------------------------
# Every other fixture sets CV_SKIP_EXISTENCE_CHECKS=1, so the `gh issue view`
# probe — the thing advertised as "deferral integrity, fail-closed" — had ZERO
# coverage. These three shim `gh` to exercise it for real.
gh_shim() { # gh_shim <issue-view-behavior>
    cat > "$TMP/gh" <<SHIM
#!/bin/sh
case "\$1 \$2" in
  "repo view")  echo "example-org/example-repo"; exit 0 ;;
  "issue view") $1 ;;
  *) echo "unexpected: \$*" >&2; exit 1 ;;
esac
SHIM
    chmod +x "$TMP/gh"
}
run_shim() { # run_shim <name> <expected-exit> <fixture> <issue-view-behavior>
    local name="$1" want="$2" json="$3"
    gh_shim "$4"
    printf '%s' "$json" > "$TMP/f.json"
    local out; out=$(CV_REVIEW_FIXTURE="$TMP/f.json" PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1); local got=$?
    if [ "$got" -eq "$want" ]; then printf '  ok   %-52s (exit %d)\n' "$name" "$got"; pass=$((pass+1))
    else printf '  FAIL %-52s (want %d, got %d)\n' "$name" "$want" "$got"; printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1)); fi
}

DEFER_DOC="$(doc "$(thread true 'x[bot]' "$BOT,$(human '<!-- cv-disposition: deferred evidence: #1234 -->')")" "$BOT_TOP,$RECON")"

run_shim "Assertion 2: deferred issue EXISTS -> pass" 0 "$DEFER_DOC" \
    'echo "https://github.com/example-org/example-repo/issues/1234"; exit 0'

run_shim "Assertion 2: deferred issue MISSING -> block" 1 "$DEFER_DOC" \
    'echo "GraphQL: Could not resolve to an Issue" >&2; exit 1'

# A transport/auth failure is NOT a missing issue; conflating them asserts a
# fact the probe never established.
run_shim "Assertion 2: transport failure is exit 2, not a false BLOCK" 2 "$DEFER_DOC" \
    'echo "error connecting to api.github.com" >&2; exit 1'

# `gh issue view` succeeds on a PR number, so a deferral could name a PR.
run_shim "Assertion 2: deferred ref is a PR, not an issue -> block" 1 "$DEFER_DOC" \
    'echo "https://github.com/example-org/example-repo/pull/1234"; exit 0'

# --- Live-path fail-closed, second stage -------------------------------------
# The earlier shim fails on the FIRST gh call, so only the repo-resolution guard
# was exercised; the graphql guard was never reached.
cat > "$TMP/gh" <<'SHIM'
#!/bin/sh
case "$1 $2" in
  "repo view") echo "example-org/example-repo"; exit 0 ;;
  *) echo "error connecting to api.github.com" >&2; exit 1 ;;
esac
SHIM
chmod +x "$TMP/gh"
out=$(PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1); got=$?
if [ "$got" -eq 2 ]; then printf '  ok   %-52s (exit 2)\n' "FAIL-CLOSED: graphql failure is exit 2"; pass=$((pass+1))
else printf '  FAIL %-52s (want 2, got %d)\n' "FAIL-CLOSED: graphql failure is exit 2" "$got"; fail=$((fail+1)); fi

# A missing fixture file exited 1 ("blocking violations") instead of 2.
out=$(CV_REVIEW_FIXTURE="$TMP/does-not-exist.json" "$SCRIPT" 999 2>&1); got=$?
if [ "$got" -eq 2 ]; then printf '  ok   %-52s (exit 2)\n' "FAIL-CLOSED: unreadable fixture is exit 2"; pass=$((pass+1))
else printf '  FAIL %-52s (want 2, got %d)\n' "FAIL-CLOSED: unreadable fixture is exit 2" "$got"; fail=$((fail+1)); fi

echo ""
echo "passed: $pass   failed: $fail"
[ "$fail" -eq 0 ] || exit 1
