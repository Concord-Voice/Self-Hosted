#!/usr/bin/env bash
#
# resolve-hotfix-label.sh — decide whether THIS push came from a `risk: hotfix` PR.
#
# Extracted from main-ci.yml's inline `Resolve the hotfix label` step so the
# logic is unit-testable without a live runner, following the precedent set by
# classify-release.sh.
#
# WHY THE SHAPE CHANGED. main-ci.yml dispatches the desktop release ONLY on a
# `risk: hotfix` merge; everything else defers to nightly-ci.yml. The old
# implementation answered that with `GET /repos/{owner}/{repo}/commits/{sha}/pulls`
# alone — an EVENTUALLY CONSISTENT endpoint. The commit->PR association is not
# written atomically with the merge, and main-ci starts on the push immediately.
#
# Observed 2026-08-29 on PR #3013: the lookup ran 19 seconds after the
# squash-merge, returned nothing, and the release deferred — to a nightly that
# had already run 28 minutes earlier, costing a full extra day. The label was on
# the PR the entire time; the identical query resolved minutes later. The old
# code logged that as "No risk: hotfix", indistinguishable from the author
# simply not having applied it.
#
# So the association is now the FALLBACK. GitHub writes the PR number into the
# squash commit's subject ("chore: propose desktop release v0.2.42 (#3013)"),
# which arrives in the push payload with no API call and no race.
#
# THE SUBJECT IS NOT TRUSTED ON ITS OWN. A hand-edited squash title can carry a
# `(#N)` that belongs to something else, and reading labels off the wrong number
# could dispatch an UNGATED release — wrong in the dangerous direction. The
# parsed number is therefore verified against `pulls/{n}.merge_commit_sha`
# before its labels are read. That is a direct object read, not an association,
# so it does not reintroduce the race.
#
# FAIL-CLOSED, in three DISTINGUISHABLE states. The old code collapsed "no
# label" and "could not tell" into one output, which is what made the incident
# invisible. `resolution` now separates them, and main-ci logs a warning rather
# than a notice when the answer is unknown.
#
# Inputs (environment variables):
#   SHA                    — the pushed commit SHA (required)
#   GITHUB_REPOSITORY      — owner/repo (required)
#   COMMIT_SUBJECT         — first line of the pushed commit message (optional;
#                            falls back to `git log`, then to the association)
#   HOTFIX_RETRY_ATTEMPTS  — association retries, default 5
#   HOTFIX_RETRY_DELAY     — seconds between retries, default 3
#
# Outputs (written to $GITHUB_OUTPUT if set, else stdout):
#   hotfix      — "true" | "false"
#   resolution  — "subject" | "association" | "no-label" | "undetermined" | "api-error"
#   resolved_via— "subject" | "association" | "none": HOW the PR number was
#                 found, independent of what the answer turned out to be. On a
#                 no-label merge `resolution` cannot say this, and on an
#                 api-error AFTER the PR was located it still reports the path
#                 that located it.
#   pr_number   — the resolved PR number, or "" when none was resolved
#
# Usage:
#   SHA=<sha> GITHUB_REPOSITORY=owner/repo bash .github/scripts/resolve-hotfix-label.sh
#
# Exit codes: 0 always (fail-closed is expressed in the outputs, not the status).

set -euo pipefail

if [[ "${1-}" == "--help" ]]; then
  grep '^#' "$0" | sed 's/^# \?//'
  exit 0
fi

if [[ -z "${SHA:-}" ]]; then
  echo "ERROR: SHA environment variable is required." >&2
  exit 1
fi
if [[ -z "${GITHUB_REPOSITORY:-}" ]]; then
  echo "ERROR: GITHUB_REPOSITORY environment variable is required." >&2
  exit 1
fi

RETRY_ATTEMPTS="${HOTFIX_RETRY_ATTEMPTS:-5}"
RETRY_DELAY="${HOTFIX_RETRY_DELAY:-3}"

emit() {
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "${1}=${2}" >> "$GITHUB_OUTPUT"
  else
    echo "${1}=${2}"
  fi
}

# `resolution` answers WHAT happened; it does NOT answer HOW the PR was found.
# The two collapse on the common outcome: an ordinary merge ends at `no-label`,
# which overwrites `subject`/`association` and leaves an operator unable to tell
# a race-free subject parse from a fallback that had to retry — the very
# distinction this script exists to make. `resolved_via` carries it separately
# so both facts survive. Kept as a SECOND output rather than folded into
# `resolution` (e.g. "no-label:subject") on purpose: main-ci.yml matches
# `resolution` with a literal `case`, so a compound value would fall through to
# the `*)` arm and log "risk: hotfix present" on an ordinary merge — a wrong
# answer is worse than a missing one.
resolved_via="none"

finish() {
  emit "hotfix"       "$1"
  emit "resolution"   "$2"
  emit "resolved_via" "$resolved_via"
  emit "pr_number"    "${3:-}"
  exit 0
}

# ── API helper: an error is not an empty answer ──────────────────────────────
# The original inline lookup wrote `2>/dev/null || echo ""`, which turned an
# authorization failure into the same empty string as "no PR found yet". That is
# why the 2026-08-29 miss could not be diagnosed from the log — a 403 and a
# not-yet-written association were byte-identical. An early draft of THIS script
# inherited the same pattern; it is spelled out here so it does not creep back.
# The error is recorded in a FILE, not a variable. api() is called from inside
# command substitution ($(...)), which is a SUBSHELL — a variable assigned there
# never reaches the caller, so an early draft recorded every failure into a
# value that was discarded microseconds later. That would have reproduced the
# exact defect this script exists to fix: an API failure silently indistinguish-
# able from an empty answer. Caught by the api-error case failing.
API_ERR_FILE=$(mktemp)
API_ERR_TMP=$(mktemp)
trap 'rm -f "$API_ERR_FILE" "$API_ERR_TMP"' EXIT

api() { # api <path> <jq-filter>
  local out rc
  out=$(gh api "$1" --jq "$2" 2>"$API_ERR_TMP") && rc=0 || rc=$?
  if (( rc == 0 )); then
    printf '%s' "$out"
    return 0
  fi
  cat "$API_ERR_TMP" >> "$API_ERR_FILE"
  return 1
}

api_errored() { [[ -s "$API_ERR_FILE" ]]; }

# ── Verification, used by BOTH resolution paths ──────────────────────────────
# The association returns PRs that CONTAIN the commit, which is not the same as
# the PR that PRODUCED it — a still-open PR whose branch carries the commit can
# appear here. Verifying merge_commit_sha is what makes either path safe to read
# labels from; without it a wrong label could dispatch an ungated release.
pr_produced_this_commit() {
  local merge_sha
  merge_sha=$(api "repos/${GITHUB_REPOSITORY}/pulls/${1}" '.merge_commit_sha // ""') || return 1
  [[ -n "$merge_sha" && "$merge_sha" == "$SHA" ]]
}

# ── 1. The subject line, which is race-free ──────────────────────────────────
# FIRST LINE ONLY, and the LAST `(#N)` on it. GitHub appends the reference at
# the end of the squash subject; a title that also cites an issue mid-sentence
# would otherwise win on a greedy match. The body is excluded entirely because
# `Closes #123` trailers live there and refer to issues, not to this PR.
subject="${COMMIT_SUBJECT:-}"
if [[ -z "$subject" ]]; then
  subject=$(git log -1 --format=%s "$SHA" 2>/dev/null || echo "")
fi
subject="${subject%%$'\n'*}"

pr_from_subject=$(printf '%s' "$subject" \
  | grep -oE '\(#[0-9]+\)' | tail -1 | grep -oE '[0-9]+' || true)

# ── 2. Verify the subject's number really names THIS commit ─────────────────
verified_pr=""
if [[ -n "$pr_from_subject" ]] && pr_produced_this_commit "$pr_from_subject"; then
  verified_pr="$pr_from_subject"
  resolved_via="subject"
fi

resolution="subject"

# ── 3. Fallback: the association, retried AND verified ───────────────────────
# Retried because it may not be written yet; verified because "contains" is not
# "produced". A single unverified attempt is the shape that failed.
if [[ -z "$verified_pr" ]]; then
  resolution="association"
  attempt=1
  while (( attempt <= RETRY_ATTEMPTS )); do
    candidate=$(api "repos/${GITHUB_REPOSITORY}/commits/${SHA}/pulls" '.[0].number // ""') || candidate=""
    if [[ -n "$candidate" ]] && pr_produced_this_commit "$candidate"; then
      verified_pr="$candidate"
      resolved_via="association"
      break
    fi
    (( attempt < RETRY_ATTEMPTS )) && sleep "$RETRY_DELAY"
    attempt=$(( attempt + 1 ))
  done
fi

# ── 4. Unknown is its own answer, and an API error is its own KIND of unknown ─
# Fail-closed either way, but the log has to say which. Collapsing these is what
# hid the #3013 miss and what made its cause unprovable afterwards.
if [[ -z "$verified_pr" ]]; then
  if api_errored; then
    echo "::error::Hotfix resolution could not reach the GitHub API: $(tr '\n' ' ' < "$API_ERR_FILE")" >&2
    finish "false" "api-error" ""
  fi
  finish "false" "undetermined" ""
fi

# ── 5. Read the labels off the verified PR ───────────────────────────────────
if ! labels=$(api "repos/${GITHUB_REPOSITORY}/issues/${verified_pr}/labels" '[.[].name] | join(",")'); then
  echo "::error::Hotfix resolution could not read labels for #${verified_pr}: $(tr '\n' ' ' < "$API_ERR_FILE")" >&2
  finish "false" "api-error" "$verified_pr"
fi

# Anchored between delimiters so a future `risk: hotfix-candidate` cannot match.
if printf ',%s,' "$labels" | grep -q ',risk: hotfix,'; then
  finish "true" "$resolution" "$verified_pr"
fi

finish "false" "no-label" "$verified_pr"
