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
  {"match":"copilot","name":"Copilot Code Review","login":["copilot-pull-request-reviewer[bot]","Copilot"]},
  {"match":"chatgpt-codex-connector","name":"Codex","login":"chatgpt-codex-connector[bot]"},
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
    if printf '%s' "$out" | grep -F -- "$want" >/dev/null; then
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

# Regression: grep is allowed to stop reading after an early fixed-string
# match. The PATH shim makes that behavior deterministic across grep versions;
# it reads only a 4 KiB prefix, then exits, so the gate's long-body printf gets
# SIGPIPE under pipefail. The match is real (Gitar is in the prefix), and the
# existing omitted-reviewer case below remains the negative control.
cat > "$TMP/grep" <<'GREPSHIM'
#!/bin/bash
if [[ "$#" -eq 3 ]] && [[ "$1" = "-qiF" ]] && [[ "$2" = "--" ]] && [[ "$3" = "Gitar" ]]; then
    prefix=$(dd bs=4096 count=1 2>/dev/null)
    /usr/bin/grep -qiF -- Gitar <<<"$prefix"
    exit $?
fi
exec /usr/bin/grep "$@"
GREPSHIM
chmod +x "$TMP/grep"
LONG_RECON_PADDING=$(printf '%*s' 131072 '' | tr ' ' x)
LONG_RECON=$(printf '%s%s%s' \
    '{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\\n| Gitar | ' \
    "$LONG_RECON_PADDING" \
    '"}')
LONG_FIXTURE=$(doc "" "$BOT_TOP,$LONG_RECON")
printf '%s' "$LONG_FIXTURE" > "$TMP/f.json"
LONG_OUT=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_SKIP_EXISTENCE_CHECKS=1 PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1)
LONG_GOT=$?
if [[ "$LONG_GOT" -eq 0 ]]; then
    printf '  ok   %-52s (exit %d)\n' \
        "regression: early grep match does not false-block" "$LONG_GOT"
    pass=$((pass + 1))
else
    printf '  FAIL %-52s (want 0, got %d)\n' \
        "regression: early grep match does not false-block" "$LONG_GOT"
    printf '%s\n' "$LONG_OUT" | sed 's/^/         | /'
    fail=$((fail + 1))
fi

run "BLOCK: reconciliation post omits a bot that spoke" 1 \
    "$(doc "" "$BOT_TOP,$(botsay sonarqubecloud 'QG passed'),$RECON")"

run "BLOCK: two reconciliation posts (must edit in place)" 1 \
    "$(doc "" "$BOT_TOP,$RECON,$RECON")"

# --- Assertion 3b: a reviewer that owns SEVERAL logins -----------------------
# `.bots[].login` is a string OR an array of them. Copilot posts inline comments
# as `Copilot` and review summaries as `copilot-pull-request-reviewer[bot]`, so
# the roster was widened to carry both — and every roster read still fed the
# value straight to `ascii_downcase`, which raises `explode input must be a
# string` on an array and ABORTS THE WHOLE JQ PROGRAM. The script caught that as
# JQERR and exited 2 (UNKNOWN) for every PR in the repo until this was fixed.
#
# It shipped green because THIS SUITE's roster still spelled that entry as a
# string, so the array form the real `.claude/repo-config.json` carries was
# exercised by nothing. The roster above now matches the real one; these two
# cases are what hold it there.
COPILOT_INLINE='{"author":{"__typename":"Bot","login":"Copilot"},"body":"missing null check"}'
RECON_COPILOT='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| Copilot Code Review |"}'

# Resolves to the DISPLAY NAME via the second element of the array. Before the
# fix this exited 2, not 0 — and 2 is UNKNOWN, which is not a refusal.
run "ok: array-login reviewer resolves to its display name" 0 \
    "$(doc "" "$COPILOT_INLINE,$RECON_COPILOT")"

# And it must still BLOCK when the post omits it — proving the case above passes
# because the name was matched, not because the reviewer was skipped entirely.
run "BLOCK: post omits the array-login reviewer that spoke" 1 \
    "$(doc "" "$COPILOT_INLINE,$RECON")"

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

# A rostered machine account can hold OWNER/MEMBER (an org bot user). Its own
# reconciliation post must not count, or it could sign off on its own review.
machinemember() { printf '{"author":{"__typename":"User","login":"ci-machine"},"authorAssociation":"MEMBER","body":"%s"}' "$1"; }
run "BLOCK: a rostered member account cannot post the reconciliation" 1 \
    "$(doc "" "$(machinemember 'SECURITY: hardcoded key'),$(machinemember '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

run "ok: a human reconciliation still counts beside a machine one" 0 \
    "$(doc "" "$(machinemember '<!-- cv-review-reconciliation -->\n| CI Machine |'),$(human '<!-- cv-review-reconciliation -->\n| CI Machine |')")"

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

badroster "STOP: a 3-char match is too broad for a substring test" 2 \
    '{"bots":[{"match":"bot","name":"Gitar","login":"x"}]}'

ambig_json=$(doc "" "$(botsay gitar-bot 'verdict'),$(botsay security-bot 'SECURITY: hardcoded key'),$(human '<!-- cv-review-reconciliation -->\n| Gitar |')")
printf '%s' '{"bots":[{"match":"-bot","name":"Gitar","login":"x"}]}' > "$TMP/ambig.json"
printf '%s' "$ambig_json" > "$TMP/ambig-f.json"
CV_REVIEW_FIXTURE="$TMP/ambig-f.json" CV_REPO_CONFIG="$TMP/ambig.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 >/dev/null 2>&1; got=$?
if [[ "$got" -eq 2 ]]; then printf '  ok   %-52s (exit %d)\n' "STOP: a match that hits two logins on the PR" "$got"; pass=$((pass+1))
else printf '  FAIL %-52s (want 2, got %d)\n' "STOP: a match that hits two logins on the PR" "$got"; fail=$((fail+1)); fi
# One reviewer posting as two of its own listed logins is not a clash.
own_json=$(doc "" "$(botsay copilot-pull-request-reviewer 'verdict'),$(botsay Copilot 'note'),$(human '<!-- cv-review-reconciliation -->\n| Copilot |')")
printf '%s' '{"bots":[{"match":"copilot","name":"Copilot","login":["copilot-pull-request-reviewer[bot]","Copilot"]}]}' > "$TMP/own.json"
printf '%s' "$own_json" > "$TMP/own-f.json"
err=$(CV_REVIEW_FIXTURE="$TMP/own-f.json" CV_REPO_CONFIG="$TMP/own.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1 >/dev/null); rc=$?
if [[ "$rc" -eq 0 && "$err" != *"more than one account"* ]]; then printf '  ok   %-52s\n' "no STOP: a match hitting only the entry's own logins"; pass=$((pass+1))
else printf '  FAIL %-52s (exit %d)\n' "no STOP: a match hitting only the entry's own logins" "$rc"; printf '%s\n' "$err" | sed 's/^/         | /'; fail=$((fail+1)); fi

badroster "STOP: malformed roster is loud, not silent login-only" 2 \
    '{"bots":[{"match":'

badroster "ok: a well-formed roster is accepted" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":"gitar-bot[bot]"}]}'

# --- Login SHAPE: the same abort one level down (found in review) ----------------
# `[null,"Copilot"]` survives the array normalisation and still reaches
# `ascii_downcase`, aborting the whole jq program. The validator refuses it
# instead. Both outcomes exit 2, so `badroster` is VACUOUS here — it passes with
# or without the validator, which is tests.md § Vacuity's "a diagnosis and a
# crash are both non-zero". Assert the message, and mutation-prove it: reverting
# the validator's login arm turns all three red on the message, never the code.
# The rejected alternative was `map(select(type == "string"))` at the read sites;
# that drops the malformed login, unrosters its reviewer, and passes — a false
# PASS is strictly worse here than a loud UNKNOWN.
badroster_msg() { # badroster_msg <name> <expected-exit> <expected-substring> <roster-json>
    local name="$1" want="$2" msg="$3"
    printf '%s' "$4" > "$TMP/bad.json"
    printf '%s' "$(doc "" "$(botsay gitar-bot 'verdict'),$(human '<!-- cv-review-reconciliation -->\nNothing to see here.')")" > "$TMP/f.json"
    local out; out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_REPO_CONFIG="$TMP/bad.json" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1); local got=$?
    if [[ "$got" -eq "$want" ]] && printf '%s' "$out" | grep -F "$msg" >/dev/null; then
        printf '  ok   %-52s (exit %d)\n' "$name" "$got"; pass=$((pass+1))
    else
        printf '  FAIL %-52s (want %d + msg %s)\n' "$name" "$want" "$msg"; printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1))
    fi
}

badroster_msg "STOP: null inside a login array is named, not aborted" 2 \
    'login element types [null, string]' \
    '{"bots":[{"match":"copilot","name":"Copilot Code Review","login":[null,"Copilot"]}]}'

badroster_msg "STOP: a scalar non-string login is refused, not coerced" 2 \
    'login element types [number]' \
    '{"bots":[{"match":"gitar","name":"Gitar","login":12}]}'

badroster_msg "STOP: a nested login array is refused" 2 \
    'login element types [array]' \
    '{"bots":[{"match":"gitar","name":"Gitar","login":[["gitar-bot[bot]"]]}]}'

# Found in review: each of these unrostered a reviewer and exited 0.
badroster_msg "STOP: a non-array .bots is refused, not read as empty" 2 \
    'not an array' '{"bots":{"match":"gitar","name":"Gitar","login":"gitar-bot[bot]"}}'
badroster_msg "STOP: a match with a space can never match a login" 2 \
    'can never match' '{"bots":[{"match":"CI Reviewer","name":"CI Reviewer"}]}'
badroster_msg "STOP: a match with an underscore can never match a login" 2 \
    'can never match' '{"bots":[{"match":"ci_reviewer","name":"CI Reviewer"}]}'
badroster_msg "STOP: a non-string name is refused" 2 \
    'must be a string' '{"bots":[{"match":"gitar","name":7,"login":"gitar-bot[bot]"}]}'

# The other direction: absent and explicit-null logins are LEGAL (an entry may
# match on `.match` alone), so the new arm must not widen into them.
badroster "ok: an absent login is not a malformed one" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar"}]}'

badroster "ok: an explicit null login is not a malformed one" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":null}]}'

# --- Login USABILITY: the same false PASS with no type error (found in review) ---
# `[]`, `[""]`, `["  "]` and the scalar `""` all normalise cleanly and match
# nothing, leaving the entry unrostered. A BOT-typed reviewer survives that (the
# gate falls back to the bare login) but a machine reviewer GraphQL types as
# `User` is in scope ONLY because it is rostered — so it silently stops owing a
# coverage row and the gate reports CLEAN. Measured at that placement before
# fixing: exit 0 on `[]` / `[""]` / `["  "]` / `""`, exit 1 on `["Copilot"]`.
# A bot-typed probe refutes nothing here, which is why the first probe missed it.
badroster_msg "STOP: an empty login array is refused, not silently unrostered" 2 \
    'declares no login alias at all' \
    '{"bots":[{"name":"Copilot Code Review","login":[]}]}'

badroster_msg "STOP: a blank-string login array is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":[""]}]}'

badroster_msg "STOP: a whitespace-only login is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["  "]}]}'

# The scalar form was ALREADY a false PASS on main — a pre-existing hole this arm
# closes, not a regression introduced by the array normalisation.
badroster_msg "STOP: a scalar empty-string login is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":""}]}'

# REVERSED IN ROUND 6, and left here as the reversal rather than deleted. This
# case used to assert the opposite -- "one usable login is enough, blank siblings
# are inert" -- and that was a recorded decision, not an oversight. It was wrong.
# A multi-login entry exists BECAUSE one reviewer posts under several logins,
# so a malformed member silently drops the surface it was added to
# cover, and the entry keeps passing on the member that happens to be spelled
# right. Every declared alias must be usable.
badroster_msg "STOP: a blank sibling DOES spoil the entry (reversed in round 6)" 2 \
    'can never match' \
    '{"bots":[{"match":"gitar","name":"Gitar","login":["","gitar-bot[bot]"]}]}'

# The finding's own fixture: valid review-surface login, malformed inline one.
# Measured before the fix: exit 0, with a User-typed `Copilot` comment matching
# neither stored value and leaving scope entirely.
badroster_msg "STOP: a mixed array drops the surface its sibling covers" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["copilot-pull-request-reviewer[bot]","Copilot "]}]}'

# THE FLOOR for the reversal: a correctly-spelled multi-login entry -- the real
# multi-login shape -- must still be accepted, or the stricter rule refuses the very
# roster it was written to protect.
badroster "ok: a correctly spelled multi-login entry is still accepted" 1 \
    '{"bots":[{"match":"copilot","name":"Copilot Code Review","login":["copilot-pull-request-reviewer[bot]","Copilot"]}]}'

# The usability check runs AFTER the consumers' own normalisation, because
# checking the raw string is the same bug one step earlier: `loginset` applies
# `ascii_downcase | sub("\\[bot\\]$";"")`, so `["[bot]"]` is non-blank at
# the validator and EMPTY at the read path (found in review — within one
# commit of the raw check landing). Measured before fixing: exit 0 on all three
# shapes below with a User-typed machine reviewer.
badroster_msg "STOP: a bare bot suffix normalises to nothing and is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["[bot]"]}]}'

badroster_msg "STOP: a scalar bare bot suffix is refused too" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":"[bot]"}]}'

badroster_msg "STOP: whitespace plus a bot suffix still normalises to nothing" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["  [bot]"]}]}'

# THE FLOOR for that arm. Every real roster login ends in `[bot]`, so an arm that
# rejected the suffix itself rather than a login that is ONLY the suffix would
# refuse the entire shipped roster. This case fails first if that happens.
badroster "ok: a real bot login survives the suffix normalisation" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":["gitar-bot[bot]","sonarqubecloud[bot]"]}]}'

# ROUND 4, and the reason the arm above is now DERIVED rather than patched.
# `["Copilot "]` was blank-STRIPPED by the old validator (non-empty, accepted)
# and whitespace-PRESERVING in `loginset`, so the `== "copilot"` missed it
# and the reviewer went unrostered. That is the same validator/consumer
# divergence as the `[bot]` round, in the opposite direction -- which is what
# retired approximation in favour of the matching rule itself: after
# normalisation an alias must still be a possible GitHub login.
badroster_msg "STOP: a trailing-space alias can never match, and is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["Copilot "]}]}'

badroster_msg "STOP: a leading-space alias is refused" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":[" Copilot"]}]}'

badroster_msg "STOP: a scalar padded alias is refused too" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":"Copilot "}]}'

badroster_msg "STOP: an interior space can never match either" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["Co pilot"]}]}'

# FLOOR for the derived rule: it must still admit every shape the SHIPPED roster
# uses -- hyphens, digits, and a plain lowercase alias. An over-strict regex here
# refuses the whole roster and takes every PR in the repo to exit 2, which is the
# failure this PR exists to fix, re-entered through the fix itself.
badroster "ok: hyphens and digits in a real login are admitted" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":["chatgpt-codex-connector[bot]","github-actions[bot]","coderabbitai[bot]","gitar-bot[bot]","Copilot","s0nar2"]}]}'

# ROUND 5, and the character class was never the hole -- the ANCHOR was. jq's
# Oniguruma `$` also matches BEFORE a trailing newline, so `^[a-z0-9][a-z0-9-]*$`
# accepted `"copilot\\n"` while `\\A...\\z` refuses it. Measured directly:
#   ["copilot\\n"] | test("^[a-z0-9][a-z0-9-]*$") -> true
#   ["copilot\\n"] | test("\\A[a-z0-9][a-z0-9-]*\\z") -> false
# and the fixture reached exit 0 before the anchor change. Same class as the
# whitespace round: a character the reads PRESERVE and no login contains.
badroster_msg "STOP: a trailing newline alias can never match" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["Copilot\n"]}]}'

# NOT a discriminating case for the anchor, and labelled so nobody reads it as
# one: `$` can only match before a TRAILING newline, so an embedded one is
# refused under both anchors and this case stays GREEN under the mutant. It is
# kept because refusing it is real behaviour worth pinning -- the case above is
# the one that goes red. Measured: mutant kills 1 of these 2.
badroster_msg "STOP: an embedded newline alias is refused (both anchors)" 2 \
    'can never match' \
    '{"bots":[{"name":"Copilot Code Review","login":["Copilot\nevil"]}]}'

# THE DECLINED BOUNDARY, pinned on purpose. Round 5 also asked for GitHub's
# signup grammar -- no terminal or doubled hyphen. Declined: `["Copilot-"]` is a
# wrong NAME, identical in kind and consequence to `["Copilto"]`, which no
# grammar catches. A newline is an invisible accident this gate can refuse; a
# misspelling is not, and only the roster's author can see it. Encoding the
# grammar would shrink an unclosable set while binding the gate to an external
# policy, in the direction that refuses a real roster.
# If you deliberately tighten this later, THIS case goes red first -- read the
# rationale in check-review-dispositions.sh before changing the expectation.
badroster "ok: an ungrammatical login is ACCEPTED (typo class, not catchable)" 1 \
    '{"bots":[{"match":"gitar","name":"Gitar","login":["Copilot-","co--pilot"]}]}'

# ROUND 7 (Gitar): the VALIDATOR ITSELF must fail closed. Its jq used
# `2>/dev/null` and never read `$?`, so a roster whose `.bots` member is not an
# object made the validator ERROR, produce no findings, and -- under
# `set -euo pipefail` -- kill the script at a bare `exit 5` with ZERO output.
# Not exit 2, so nothing downstream reads it as UNKNOWN, and the operator sees
# nothing. A validator that cannot run must never read as a clean roster.
badroster_msg "STOP: a validator jq that ERRORS is exit 2, not a silent crash" 2 \
    'could not validate the reviewer roster' \
    '{"bots":[42]}'

badroster_msg "STOP: a string roster member also fails the validator closed" 2 \
    'could not validate the reviewer roster' \
    '{"bots":["not-an-object"]}'

# ROUND 8 (CodeRabbit): the same false PASS through the OTHER selector. An entry
# with no `.login` is legal -- match-only is a real shape -- but the match arm
# stringifies with `tostring`, so `match: false` became the 5-character "false"
# and passed, while `matchstr` in the read path takes STRINGS ONLY and returns
# "". The entry selected nothing and a User-typed reviewer relying on it left
# scope. Measured before the fix: all three reported CLEAN.
badroster_msg "STOP: no login and a boolean match selects nothing" 2 \
    'has no usable selector' \
    '{"bots":[{"name":"Copilot Code Review","match":false}]}'

badroster_msg "STOP: no login and NO match selects nothing" 2 \
    'has no usable selector' \
    '{"bots":[{"name":"Copilot Code Review"}]}'

badroster_msg "STOP: no login and a null match selects nothing" 2 \
    'has no usable selector' \
    '{"bots":[{"name":"Copilot Code Review","match":null}]}'

# FLOORS for round 8 -- both legal shapes must survive, or the arm refuses real
# rosters. A match-only entry IS a supported shape, and every shipped entry is
# login-only; refusing either would take every PR in the repo to exit 2.
badroster "ok: a match-only entry with a usable match is accepted" 1 \
    '{"bots":[{"name":"Gitar Reviewer","match":"gitar"}]}'

badroster "ok: a login-only entry needs no match at all" 1 \
    '{"bots":[{"name":"Gitar Reviewer","login":"gitar-bot[bot]"}]}'

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
#!/bin/bash
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
#!/bin/bash
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

# --- Live-path pagination ----------------------------------------------------
# The live path fetches each connection with `gh api graphql --paginate --slurp`,
# which hands back an ARRAY of pages. These cases shim that array directly: gh's
# own cursor-following is gh's problem, but whether this script MERGES every page
# is ours, and a merge that silently kept only the first page would still exit 0
# on a clean PR. Every case here puts the deciding evidence on page TWO.

# thread2 <id> <resolved> <resolvedBy> <comments-json>  — thread() hardcodes id "T"
thread2() {
    printf '{"id":"%s","isResolved":%s,"resolvedBy":{"login":"%s"},"path":"a/b.go","line":12,"comments":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}' "$1" "$2" "$3" "$4"
}
tpage() { printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":%s},"nodes":[%s]}}}}}' "$1" "$2"; }
cpage() { printf '{"data":{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasNextPage":%s},"nodes":[%s]}}}}}' "$1" "$2"; }
rpage() { printf '{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":%s},"nodes":[%s]}}}}}' "$1" "$2"; }

# Discriminates on the query text, in the same order the script's own queries are
# distinguishable: the nested one first (it alone says `node(id:`), then threads,
# then reviews, and conversation comments last as the fallback.
paginate_shim() {
    cat > "$TMP/gh" <<'SHIM'
#!/bin/bash
case "$1 $2" in
  "repo view") echo "example-org/example-repo"; exit 0 ;;
esac
q=""
for a in "$@"; do case "$a" in query=*) q="$a" ;; esac; done
case "$q" in
  *"node(id:"*)        f=threadcomments ;;
  *reviewThreads*)     f=threads ;;
  *"reviews(first"*)   f=reviews ;;
  *)                   f=comments ;;
esac
if [[ -f "$CV_TEST_PAGES/$f.json" ]] && [[ "$(cat "$CV_TEST_PAGES/$f.json")" != "MISSING-FIXTURE-SENTINEL" ]]; then
  cat "$CV_TEST_PAGES/$f.json"; exit 0
fi
echo "no fixture for $f" >&2; exit 1
SHIM
    chmod +x "$TMP/gh"
}

run_pages() { # run_pages <name> <expected-exit> <threads-array> <comments-array> <reviews-array> [threadcomments-array]
    local name="$1" want="$2"
    mkdir -p "$TMP/pages"
    printf '%s' "$3" > "$TMP/pages/threads.json"
    printf '%s' "$4" > "$TMP/pages/comments.json"
    printf '%s' "$5" > "$TMP/pages/reviews.json"
    if [[ -n "${6:-}" ]]; then printf '%s' "$6" > "$TMP/pages/threadcomments.json"
    else rm -f "$TMP/pages/threadcomments.json"; fi
    paginate_shim
    local out; out=$(CV_TEST_PAGES="$TMP/pages" PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1); local got=$?
    if [[ "$got" -eq "$want" ]]; then printf '  ok   %-52s (exit %d)\n' "$name" "$got"; pass=$((pass+1))
    else printf '  FAIL %-52s (want %d, got %d)\n' "$name" "$want" "$got"; printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1)); fi
}

CLEAN_T="$(thread2 T1 true 'coderabbitai[bot]' "$BOT,$(human '<!-- cv-disposition: accepted evidence: e5c34d0 -->')")"
NAKED_T="$(thread2 T2 true 'coderabbitai[bot]' "$BOT")"

# THE case this whole change exists for: page 1 is clean, the violation is on
# page 2. A merge that drops page 2 reports the PR clean and exits 0.
run_pages "pagination: a violation on thread page 2 still blocks" 1 \
    "[$(tpage true "$CLEAN_T"),$(tpage false "$NAKED_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON")]" \
    "[$(rpage false "")]"

# Same shape on the two other connections. Assertion 3 needs every bot that spoke
# anywhere to be named, so a dropped page-2 bot makes the gate report clean over
# a reviewer nobody reconciled.
run_pages "pagination: a bot on comment page 2 must be reconciled" 1 \
    "[$(tpage false "$CLEAN_T")]" \
    "[$(cpage true "$RECON"),$(cpage false "$(botsay 'copilot-pull-request-reviewer' 'looks fine')")]" \
    "[$(rpage false "")]"

run_pages "pagination: a bot on review page 2 must be reconciled" 1 \
    "[$(tpage false "$CLEAN_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON")]" \
    "[$(rpage true ""),$(rpage false "$(botsay 'copilot-pull-request-reviewer' 'looks fine')")]"

# ...and the same surfaces, fully reconciled, are green — otherwise the three
# cases above would pass on a gate that simply blocks everything.
RECON3='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| CodeRabbit | Gitar | Copilot Code Review |"}'
run_pages "pagination: page 2 reconciled on every surface is green" 0 \
    "[$(tpage true "$CLEAN_T"),$(tpage false "")]" \
    "[$(cpage true "$BOT_TOP,$RECON3"),$(cpage false "$(botsay 'copilot-pull-request-reviewer' 'looks fine')")]" \
    "[$(rpage false "")]"

# NESTED connection: --paginate follows only the outermost cursor, so a thread
# with >100 comments needs its own pass. The disposition lives on comment page 2;
# without the nested fetch the thread reads as undispositioned.
NESTED_T="$(printf '{"id":"T9","isResolved":true,"resolvedBy":{"login":"coderabbitai[bot]"},"path":"a/b.go","line":12,"comments":{"pageInfo":{"hasNextPage":true},"nodes":[%s]}}' "$BOT")"
run_pages "pagination: a disposition on thread-comment page 2 is found" 0 \
    "[$(tpage false "$NESTED_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON")]" \
    "[$(rpage false "")]" \
    "[{\"data\":{\"node\":{\"comments\":{\"pageInfo\":{\"hasNextPage\":true},\"nodes\":[$BOT]}}}},{\"data\":{\"node\":{\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[$(human '<!-- cv-disposition: accepted evidence: e5c34d0 -->')]}}}}]"

# Page size: Linux caps one argv string at 128 KiB, so pages handed to jq as
# --argjson made a large PR exit 2 (UNKNOWN) instead of being read. One ~200 KB
# review page and one ~200 KB nested thread-comment page must still go green.
BIG_BODY="$(head -c 200000 /dev/zero | tr '\0' 'x')"
run_pages "pagination: pages over 128 KiB are still read" 0 \
    "[$(tpage false "$NESTED_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON3")]" \
    "[$(rpage false "$(botsay 'copilot-pull-request-reviewer' "$BIG_BODY")")]" \
    "[{\"data\":{\"node\":{\"comments\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[$(botsay 'coderabbitai[bot]' "$BIG_BODY"),$(human '<!-- cv-disposition: accepted evidence: e5c34d0 -->')]}}}}]"

# Found in review: each of these exited 0 over a surface that was never read.
run_pages "FAIL-CLOSED: an empty nested comment fetch is a STOP" 2 \
    "[$(tpage false "$NESTED_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON")]" \
    "[$(rpage false "")]" \
    "[]"

# Fail-closed still holds per connection. An error envelope on a LATER page is
# the dangerous one: page 1 parses and only page 2 carries the null.
#
# Asserted on the MESSAGE, not just the exit code, and that is the point. jq's
# indexing strictness makes the assembly step throw on a null page all by
# itself, so an exit-2-only assertion passes with the per-page probe deleted —
# it would be pinning a side effect of `null | .reviewThreads` rather than the
# guard. One `// empty` or `?` added to that jq for robustness turns the side
# effect into a fail-OPEN, and the exit-code test would still be green. Naming
# the guard in the assertion is what keeps the two distinguishable.
run_pages_out() { # run_pages_out <name> <expected-substring> <threads> <comments> <reviews>
    local name="$1" want="$2"
    mkdir -p "$TMP/pages"
    printf '%s' "$3" > "$TMP/pages/threads.json"
    printf '%s' "$4" > "$TMP/pages/comments.json"
    printf '%s' "$5" > "$TMP/pages/reviews.json"
    rm -f "$TMP/pages/threadcomments.json"
    paginate_shim
    local out; out=$(CV_TEST_PAGES="$TMP/pages" PATH="$TMP:$PATH" "$SCRIPT" 999 2>&1)
    if printf '%s' "$out" | grep -F -- "$want" >/dev/null; then
        printf '  ok   %-52s\n' "$name"; pass=$((pass+1))
    else
        printf '  FAIL %-52s (missing: %s)\n' "$name" "$want"
        printf '%s\n' "$out" | sed 's/^/         | /'; fail=$((fail+1))
    fi
}
ERR_PAGES="[$(tpage true "$CLEAN_T"),{\"data\":{\"repository\":{\"pullRequest\":null}},\"errors\":[{\"message\":\"rate limited\"}]}]"
run_pages "FAIL-CLOSED: an error envelope on page 2 is exit 2" 2 \
    "$ERR_PAGES" "[$(cpage false "$BOT_TOP,$RECON")]" "[$(rpage false "")]"
run_pages_out "FAIL-CLOSED: ...and the per-page probe is what caught it" \
    "returned no pullRequest" \
    "$ERR_PAGES" "[$(cpage false "$BOT_TOP,$RECON")]" "[$(rpage false "")]"

# A connection whose fetch fails outright, mid-way through the three. Pinned by
# MESSAGE for the same reason as the probe above: the per-page probe backstops a
# swallowed fetch failure (empty output is not a JSON array either), so an
# exit-code-only assertion stays green with `|| true` appended to the fetch. Two
# independent guards on the most dangerous failure is deliberate; a test that
# cannot tell which one fired is not.
run_pages_out "FAIL-CLOSED: a failed reviews fetch names that fetch" \
    "could not read reviews" \
    "[$(tpage false "$CLEAN_T")]" "[$(cpage false "$BOT_TOP,$RECON")]" "MISSING-FIXTURE-SENTINEL"

# Found in review: both exited 0 over a surface that was never read.
run_pages_out "FAIL-CLOSED: every connection returning [] is a STOP" \
    "returned no pullRequest" "[]" "[]" "[]"
# gh stops quietly when the last page says hasNextPage:true but has no cursor.
run_pages_out "FAIL-CLOSED: a last page still reporting more is truncated" \
    "review surface is truncated" \
    "[$(tpage true "$CLEAN_T")]" \
    "[$(cpage false "$BOT_TOP,$RECON")]" \
    "[$(rpage false "")]"

# ---------------------------------------------------------------------------
# REGRESSION: a roster entry may carry SEVERAL logins.
#
# Copilot posts inline comments as `Copilot` and review summaries as
# `copilot-pull-request-reviewer[bot]`, so the roster made `.login` an array. Nothing in
# the enumeration was taught that shape, and `["a","b"] | ascii_downcase` is a HARD
# jq abort -- "explode input must be a string" -- which killed the whole program.
# The gate then STOPped (exit 2) on EVERY pr in the repository, not merely on a PR
# that happened to involve Copilot, because the roster is walked for every author.
#
# It failed CLOSED, which is the right direction, but a merge gate that can never
# pass is still a broken merge gate. These cases pin both shapes.
# ---------------------------------------------------------------------------
cat > "$TMP/roster-multi.json" <<'ROSTER'
{"bots":[
  {"match": "gitar", "name": "Gitar", "login": "gitar-bot[bot]"},
  {"match": "copilot", "name": "Copilot Code Review",
   "login": ["copilot-pull-request-reviewer[bot]", "Copilot"]}
]}
ROSTER

# A roster field that is neither string nor array must not abort it either: the
# entry is skipped, the program survives, and the other reviewers still resolve.
# This fixture was SPLIT in two because its halves have opposite correct
# answers and one fixture cannot assert both.
#
# This half: a non-string `.marker` or `.match` must not abort the program. The
# roster validator checks neither field's type, so this case still expects exit 0.
cat > "$TMP/roster-weird.json" <<'ROSTER'
{"bots":[
  {"match": "gitar", "name": "Gitar", "login": "gitar-bot[bot]"},
  {"name": "Broken Entry", "login": "broken-bot[bot]", "marker": 7, "match": false}
]}
ROSTER

# The other half -- a non-string `.login` -- is a DELIBERATE reversal of an
# earlier behaviour, and the reasoning is on the record. Skipping the member keeps
# the rest of the roster working (availability); the validator instead refuses
# the roster (integrity), because skipping leaves that entry's
# reviewer unrostered: for a BOT-typed reviewer the bare-login fallback saves it,
# but a reviewer GitHub types as `User` is in scope ONLY because it is rostered,
# so it silently stops owing a coverage row. Measured against the skipping version:
# `[null]`, `[12]` and a bare `null` each report CLEAN with such a reviewer.
#
# The earlier objection -- "a merge gate that can never pass is still a broken
# merge gate" -- was about aborting on EVERY roster including valid ones. This
# refuses only a roster that is actually malformed, and the shipped roster passes,
# so that objection does not reach it. For a gate whose whole thesis is that it
# never reports clean over an undecided thread, a loud UNKNOWN naming the entry
# beats a quiet degraded run.
cat > "$TMP/roster-badlogin.json" <<'ROSTER'
{"bots":[
  {"match": "gitar", "name": "Gitar", "login": "gitar-bot[bot]"},
  {"name": "Broken Entry", "login": 42}
]}
ROSTER

run_roster() { # run_roster <name> <roster-file> <expected-exit> <fixture-json>
    local name="$1" roster="$2" want="$3" json="$4"
    printf '%s' "$json" > "$TMP/f.json"
    local out
    out=$(CV_REVIEW_FIXTURE="$TMP/f.json" CV_REPO_CONFIG="$roster" CV_SKIP_EXISTENCE_CHECKS=1 "$SCRIPT" 999 2>&1)
    local got=$?
    # An abort is the specific failure being pinned, so name it explicitly rather
    # than letting it hide inside a generic exit-code mismatch.
    if printf '%s' "$out" | grep "could not enumerate reviewers" >/dev/null; then
        printf '  FAIL %-52s (reviewer enumeration ABORTED)\n' "$name"
        printf '%s\n' "$out" | sed 's/^/         | /'
        fail=$((fail + 1))
        return
    fi
    if [[ "$got" -eq "$want" ]]; then
        printf '  ok   %-52s (exit %d)\n' "$name" "$got"
        pass=$((pass + 1))
    else
        printf '  FAIL %-52s (want %d, got %d)\n' "$name" "$want" "$got"
        printf '%s\n' "$out" | sed 's/^/         | /'
        fail=$((fail + 1))
    fi
}

COPILOT_SUMMARY='{"author":{"__typename":"Bot","login":"Copilot"},"body":"looks fine"}'
RECON_COPILOT='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| Copilot Code Review |"}'
NO_THREADS='{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]},"comments":{"pageInfo":{"hasNextPage":false},"nodes":['"$COPILOT_SUMMARY"','"$RECON_COPILOT"']},"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}'

run_roster "ARRAY login resolves its persona instead of aborting" \
    "$TMP/roster-multi.json" 0 "$NO_THREADS"

# The SECOND login in the array must resolve too -- matching only the first would
# pass the test above while still losing the persona this change exists to capture.
RECON_WRONG='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| Gitar |"}'
NO_THREADS_WRONG='{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]},"comments":{"pageInfo":{"hasNextPage":false},"nodes":['"$COPILOT_SUMMARY"','"$RECON_WRONG"']},"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}'
run_roster "CONTROL: an unnamed array-login persona still BLOCKS" \
    "$TMP/roster-multi.json" 1 "$NO_THREADS_WRONG"

GITAR_TOP='{"author":{"__typename":"Bot","login":"gitar-bot"},"body":"hi"}'
RECON_GITAR='{"author":{"__typename":"User","login":"dev"},"authorAssociation":"OWNER","body":"<!-- cv-review-reconciliation -->\n| Gitar |"}'
WEIRD='{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]},"comments":{"pageInfo":{"hasNextPage":false},"nodes":['"$GITAR_TOP"','"$RECON_GITAR"']},"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}'
run_roster "a non-string marker/match does not abort the program (type check)" \
    "$TMP/roster-weird.json" 0 "$WEIRD"

run_roster "a non-string LOGIN is refused, not skipped (validator wins)" \
    "$TMP/roster-badlogin.json" 2 "$WEIRD"

# Regression: coverage matching is fed through a pipe under `set -o pipefail`.
# grep exits as soon as it finds the reviewer name, so a sufficiently large
# reconciliation body can make the producer receive SIGPIPE and turn a real
# match into a false BLOCK. Keep the name near the beginning and make the tail
# exceed the pipe buffer deterministically; the body is otherwise an ordinary
# one-reviewer reconciliation fixture.
LONG_FIXTURE=$(jq -cn --argjson reviewer "$BOT_TOP" '
    {data:{repository:{pullRequest:{reviewThreads:{nodes:[]},comments:{nodes:[
        $reviewer,
        {author:{__typename:"User",login:"dev"},authorAssociation:"OWNER",
         body:("<!-- cv-review-reconciliation -->\n| Gitar |\n" + ("padding evidence\n" * 8192))}
    ]},reviews:{nodes:[]}}}}}')
run "large reconciliation body keeps an early reviewer match" 0 "$LONG_FIXTURE"

echo ""
echo "passed: $pass   failed: $fail"
[ "$fail" -eq 0 ] || exit 1
