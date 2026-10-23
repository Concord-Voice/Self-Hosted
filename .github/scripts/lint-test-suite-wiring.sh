#!/usr/bin/env bash
#
# lint-test-suite-wiring.sh — assert every `.github/scripts/tests/*.test.sh` is
# INVOKED by something, REACHABLE by everything gating that invocation, and
# ENFORCING, meaning its failure can actually fail the build. Any one of the
# three missing leaves a suite that looks covered and reports nothing.
#
# The third was added late and is the one most easily overlooked: a suite run
# under `continue-on-error: true`, or ending in `|| true`, executes and has its
# red discarded — exactly as silent as one that never ran. "Wired and reachable"
# was the original contract and it was not sufficient.
#
# Four shell shapes discard a status, and each was found separately: a `||`
# handler that exits 0 (`|| echo ignored`, `|| exit 0`), a suite used as an
# `if`/`while` CONDITION (bash suppresses errexit there, and an `if` with no
# matching branch exits 0), and a BACKGROUNDED invocation (`&`), whose async
# status is 0 whatever the suite eventually does. All three join the `||`
# handler in the fail-closed default: unrecognised means swallowing.
#
# Reachability is checked at four levels, because any of them can do the
# skipping: the workflow's own `on:` trigger paths, the job's `if:`, the step's
# `if:`, and the dorny path filter a gate resolves to. A pre-commit hook is
# gated by `files:`/`exclude:` at both the config and hook level.
#
# WHY THIS EXISTS. The split occurred three times in `pr-ci.yml` during #3139
# alone, and the third instance was introduced by the commit fixing the second:
#   1. promote-unreleased-changelog.test.sh was in neither list.
#   2. check-pattern-register.test.sh and check-required-statuses.test.sh had no
#      runner at all — not a workflow, not pre-commit — so they had never
#      executed anywhere despite passing.
#   3. Those two were then added to the runner list without their filter paths,
#      so a PR touching only them still fast-exited before reaching the step.
# Every instance was caught by a human or a bot reviewer; none by a check. Two
# occurrences is a missing rule, and this is the rule.
#
# THE FAILURE IS SILENT IN BOTH DIRECTIONS, which is what makes it worth a lint.
# An unrun suite reports nothing — it does not fail, it is simply absent from
# the log — and a suite behind a filter that excludes it is absent on exactly
# the PRs that change it. Neither shows up as a red check.
#
# A test's IMPLEMENTATION script is checked too. `X.test.sh` exercising `X.sh`
# is useless if a change to `X.sh` alone does not trigger the job. When `X.sh`
# keeps its logic in a snake_case sibling (`x_y.py` beside `x-y.sh`), that file
# is checked the same way.
#
# WHAT THIS DOES NOT MODEL — and why the list is closed.
#
# The three obligations above are the contract and they are settled. What is
# frozen is the MODELLING: this reads shell text and workflow YAML with regexes,
# not with a parser, and a shell is larger than any set of regexes. Review of
# #3139 found roughly a dozen constructs where an unmodelled shape was read as a
# valid invocation — each is now handled, each has a test, and each was a place
# this failed OPEN rather than closed.
#
# The stopping line is deliberate: none of those constructs exists anywhere in
# this repository, and adding modelling for shapes nobody writes costs more than
# it protects. So the rule from here is EVIDENCE FIRST — extend this only for a
# construct that actually appears in the tree, or that a reviewer reproduces as
# a certified-but-unwired suite. Not for one that merely could exist.
#
# The gaps below are split by DIRECTION, because that is what decides what each
# one costs. A fail-CLOSED gap reports a suite as unwired or unenforcing when it
# may be fine: noise, and an author who reads the diagnostic can act on it. A
# fail-OPEN gap certifies a suite that is not actually wired or enforcing: a
# silent pass, which is the thing this lint exists to prevent.
#
# An earlier revision of this note claimed every gap was fail-closed. That was
# not true, and is corrected here — an inaccurate limits section is worse than
# none, because it invites exactly the trust the limits exist to withhold.
#
# Fail-CLOSED gaps (noise):
#
#   * A backgrounded invocation is never enforcing. A status-propagating
#     `wait "$!" || exit 1` could in principle recover it; none exists here, so
#     the recogniser is not written and cannot be tested.
#   * Only the default shell and `shell: sh` are modelled for errexit and
#     pipefail. Any other `shell:` value -- a custom template like
#     `shell: bash {0}`, or `pwsh`, or `python` -- is treated as carrying
#     NEITHER unless the body says `set -e` / `set -o pipefail` explicitly. That
#     over-reports a `shell: python` step, which is not a shell script at all;
#     the alternative certifies a suite whose red the shell was never going to
#     act on, so the noise is the side to err on.
#   * `set -e` / `set +e` tracking is textual across the whole run block. It does
#     not scope to functions or subshells, so a `set +e` inside one is read as
#     applying below it.
#   * `pull_request_target` never admits, because it runs at the BASE commit and
#     a suite riding it tests the old copy of itself. A workflow that re-checks
#     out the PR head would be fine, but establishing that is more modelling
#     than this wants -- so it fails closed and says so.
#   * A runner reached through a variable (`R=bash; $R <suite>`) is not
#     recognised as a runner.
#   * Only `run:` bodies are read. A suite invoked from a composite action or a
#     reusable workflow (`uses:`) is invisible here.
#
# Fail-OPEN gaps — REPRODUCED, NOT FIXED. Each certifies a suite that does not
# actually run, or does not actually enforce. They were found by review after
# the modelling was frozen, and are recorded rather than chased because closing
# them kept introducing new ones: five of the six findings in the preceding
# review round were defects in the two commits before it. Buying out of that
# cycle is what this freeze is for. Each is written so it can be reproduced
# without rediscovering it:
#
#   * An `if:` naming `pull_request_target` reads as pull-request-reachable,
#     although only the BASE revision runs there. The trigger check rejects that
#     event; the CONDITION check still uses the broader PR_EVENTS set.
#   * `needs:` is not followed. A runner job whose prerequisite is `if: false`
#     is skipped by GitHub, and only the runner's own conditions are inspected.
#   * errexit state is not seeded from the selected shell, and the unknown-shell
#     rejection scans the whole block — so under `shell: bash {0}` a LATER
#     `set -e` satisfies that rejection retroactively.
#   * `find_suite_token()` returns the first path MENTION on a line, not the
#     invocation. With `echo <suite>; set +e; bash <suite>` the option state is
#     read at the echo's offset and the real invocation is never examined.
#   * A statically dead shell branch still counts: `if false; then bash <suite>;
#     fi` is segmented into what looks like a reachable command.
#   * An `if:` conjunct this does not DECIDE is dropped rather than distrusted,
#     so a step gated to a branch or an actor reads as pull-request-reachable:
#     `if: github.ref == 'refs/heads/main'` certifies a suite that never runs on
#     a PR. `github.event_name` against a literal is the only conjunct evaluated.
#     Distrusting every unrecognised conjunct would red correct steps, which is
#     why it is written this way — but the DIRECTION is open, not closed, and an
#     earlier revision of this note filed it under fail-closed on the strength of
#     that rationale. The cost of the alternative is not the direction of the gap.
#   * Pre-commit `types:` / `types_or:` / `exclude_types:` are not read (only
#     `files:` / `exclude:`), and a hook's `args:` are not read (only `entry`).
#   * A pre-commit hook's `stages:` is not read either, so a `stages: [manual]`
#     hook -- which an ordinary commit never runs -- stands in for an
#     automatic runner. Same family as the `types:` gap above and the
#     schedule-only workflow below: every one of them is the model answering
#     'does this runner exclude the suite' when the question is 'can an
#     ordinary commit or pull request reach this runner at all'.
#   * `rest_runs_after()` looks only past the END of the line, so
#     `bash <suite> && echo passed; echo ignored` — all on one line — reads as
#     an AND-list with nothing after it.
#   * A job-output projection loses POLARITY. `tests: ${{ steps.filter.outputs.x
#     == 'false' }}` consumed as `needs.changes.outputs.tests == 'true'` is
#     inverted, and only the nested filter reference is extracted.
#   * `branches:` / `branches-ignore:` on a trigger are not read, so a runner
#     under `pull_request: {branches: [release]}` is certified for a PR that
#     targets `main`.
#   * A `needs.<job>.outputs.<name>` gate is resolved against the workflow-global
#     job table without checking that the consumer job actually declares
#     `needs: <job>`. Without it the output is unavailable and the condition is
#     permanently false, but the gate resolves.
#   * One explicit exception is resolved from the shared scan-relevance map:
#     workflow-lint's `advisory.workflow_lint` projection, only when the same
#     condition carries both detector-failure and unknown-output fallbacks.
#     Other JSON projections are not interpreted; they fail closed as unknown
#     sources rather than being treated as ordinary path-filter outputs.
#   * An unconditional `exit` is recognised only at the START of a line, so
#     `echo before; exit 0; bash <suite>` on one physical line leaves the dead
#     invocation counted as a runner. Segmenting on `;` is the fix; the
#     recogniser's other job -- not eating a helper function's `return` -- is
#     what makes that more than a one-line change.
#   * A workflow carrying no AUTOMATIC event at all -- `schedule:` or
#     `workflow_dispatch:` only -- admits, on the stated grounds that its trigger
#     does not exclude the suite. That answers the wrong question: nothing
#     downstream asks whether a pull request can reach the workflow in the first
#     place, so a schedule-only runner is certified as wired AND reachable.
#
# Usage:
#   lint-test-suite-wiring.sh [repo-root]
#   lint-test-suite-wiring.sh --help
#
# Exits:
#   0 — every suite has a runner that can be reached and whose failure counts,
#       and every filter-gated invocation lists both the suite and the script
#       it exercises
#   1 — at least one suite is unwired, unreachable, or unenforced
#   2 — script error (bad args, missing tree, unparseable workflow)

set -euo pipefail

usage() {
  awk 'NR==1{next} /^[^#]/{exit} {sub(/^# ?/,""); print}' "$0"
}

ROOT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    -*) echo "ERROR: unknown flag: $1" >&2; exit 2 ;;
    *)
      if [[ -n "$ROOT" ]]; then echo "ERROR: too many arguments" >&2; exit 2; fi
      ROOT="$1"; shift ;;
  esac
done

if [[ -z "$ROOT" ]]; then
  ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
fi
if [[ ! -d "$ROOT/.github/scripts/tests" ]]; then
  echo "ERROR: no .github/scripts/tests under: $ROOT" >&2
  exit 2
fi

# The Python lives in a sibling FILE for the reason promote-unreleased-changelog.sh
# gives above its own python3 call: a heredoc on stdin can hang on macOS bash
# before 5.3 patch 16. Do not inline it back.
python3 "$(dirname "${BASH_SOURCE[0]}")/lint_test_suite_wiring.py" "$ROOT"
