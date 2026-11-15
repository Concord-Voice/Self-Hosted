#!/bin/bash
# Helpers for the /pr-creation skill.
# Sourceable from SKILL.md bash blocks and from skills/pr-creation/test-lib.sh.
#
# Conventions:
# - All helpers are pure functions: stdin/stdout only, no side effects.
# - Mutations (gh issue edit, gh pr edit, gh api graphql) live in SKILL.md.
#
# Configuration (org schema) — there are NO hard-coded repo defaults here:
# - Project-board IDs, label namespaces, and the labels source file come from the
#   per-repo OPTIONAL `.claude/repo-config.json`, read via `scripts/repo-config.sh`
#   (`rc_get`/`rc_has`). A repo without a `.project` block has every board op
#   skip/degrade; mutating board helpers fail loudly (`:?`) rather than ever
#   writing to some other repo's board.
# - Owner/repo are derived from `gh` (`gh repo view`) when not exported.
# - Env vars override config: callers may export PROJECT_ID / STATUS_FIELD_ID /
#   PRIORITY_FIELD_ID / OWNER / REPO_NAME / LABELS_FILE / LABEL_NAMESPACES.

# Source the shared repo-config reader (best-effort; absent => helpers degrade).
# Installed, it lives at <repo>/scripts/repo-config.sh; in this tooling repo it is four levels up.
# A caller that already loaded the default branch's helper (see SKILL.md § Execution) keeps it.
if ! declare -F rc_get >/dev/null 2>&1; then
  _rc_helper="$(git rev-parse --show-toplevel 2>/dev/null || pwd)/scripts/repo-config.sh"
  [ -f "$_rc_helper" ] || _rc_helper="$(dirname "${BASH_SOURCE[0]}")/../../../../scripts/repo-config.sh"
  # shellcheck disable=SC1090,SC1091 # path resolved at runtime
  source "$_rc_helper" 2>/dev/null || true
  unset _rc_helper
fi

# Default the board env vars from config when the caller hasn't exported them.
# When config is ALSO absent/empty, rc_get returns "" so these stay empty — and
# the `:?` guards in the mutating helpers below then fire (fail-loud), while the
# read-only helpers early-return. (`:=` only assigns when unset/empty.)
: "${PROJECT_ID:=$(rc_get '.project.id')}"
: "${STATUS_FIELD_ID:=$(rc_get '.project.fields.status.id')}"
: "${PRIORITY_FIELD_ID:=$(rc_get '.project.fields.priority.id')}"

# Default the label namespaces (space-separated) from config; falls back to the
# documented org default set. Used to build the Active-labels allowlist regex.
: "${LABEL_NAMESPACES:=$(rc_get '.labels.namespaces | join(" ")' "type area domain release risk routine workflow-validation")}"

# Script-global memo caches for _resolve_owner / _resolve_repo.
# These are separate from OWNER / REPO_NAME so that the exported env-var
# path (caller sets OWNER/REPO_NAME before sourcing) still works as before.
_RESOLVED_OWNER=""
_RESOLVED_REPO=""

# _resolve_owner / _resolve_repo
# Derive the repo owner / name from `gh` when not exported. Memoized after
# first resolution into script-global _RESOLVED_OWNER / _RESOLVED_REPO so
# subsequent calls (which run in the SAME shell, not a subshell) skip the
# extra `gh repo view` invocation. When the caller exports OWNER/REPO_NAME
# those values are used directly on every call (no gh needed).
# Empty (with a stderr WARN) if gh can't answer — callers decide.
_resolve_owner() {
    if [ -n "${OWNER:-}" ]; then
        printf '%s' "$OWNER"
        return 0
    fi
    if [ -n "$_RESOLVED_OWNER" ]; then
        printf '%s' "$_RESOLVED_OWNER"
        return 0
    fi
    _RESOLVED_OWNER=$(gh repo view --json owner -q .owner.login 2>/dev/null) || _RESOLVED_OWNER=""
    [ -z "$_RESOLVED_OWNER" ] && echo "  WARN: could not derive OWNER from 'gh repo view'; export OWNER explicitly." >&2
    printf '%s' "${_RESOLVED_OWNER:-}"
}
_resolve_repo() {
    if [ -n "${REPO_NAME:-}" ]; then
        printf '%s' "$REPO_NAME"
        return 0
    fi
    if [ -n "$_RESOLVED_REPO" ]; then
        printf '%s' "$_RESOLVED_REPO"
        return 0
    fi
    _RESOLVED_REPO=$(gh repo view --json name -q .name 2>/dev/null) || _RESOLVED_REPO=""
    [ -z "$_RESOLVED_REPO" ] && echo "  WARN: could not derive REPO_NAME from 'gh repo view'; export REPO_NAME explicitly." >&2
    printf '%s' "${_RESOLVED_REPO:-}"
}

# _build_active_label_regex
# Emits a grep -E alternation of the configured label namespaces, e.g.
# `(type|area|domain|...)`, or an empty string when LABEL_NAMESPACES is unset/empty.
# Callers MUST check for an empty return and treat it as "no namespace filter"
# (i.e. degrade to passthrough), not as a literal empty alternation `()`.
_build_active_label_regex() {
    local ns alt=""
    for ns in ${LABEL_NAMESPACES}; do
        if [ -n "$alt" ]; then alt="$alt|$ns"; else alt="$ns"; fi
    done
    # Return empty string when no namespaces configured — callers handle the
    # no-op case rather than constructing a broken regex like `()`.
    printf '%s' "$alt"
}

# _strip_code_blocks BODY
# Internal helper: strips both fenced (``` ... ```) AND inline (`...`) code
# spans from a markdown body so closing-keyword parsers don't false-positive
# on example code in the PR body. (Discovered via dogfood: an example
# `Closes #1 fixes #2` snippet in a Test Plan section was matched as real refs.)
_strip_code_blocks() {
    local body="$1"
    # 1. Strip fenced blocks per GFM: track the opening fence's backtick-run
    #    length and only close on a fence line with AT LEAST that many backticks.
    #    This prevents a 4-backtick outer fence leaking content when the inner
    #    block contains a 3-backtick fence (the inner 3-tick line does NOT close
    #    the outer 4-tick fence). Per CommonMark/GFM, fences may be indented 0-3
    #    spaces — strip leading spaces before counting.
    # 2. Strip inline backticked spans on remaining lines: sed s/`[^`]*`//g
    printf '%s' "$body" \
        | awk '
            BEGIN { in_fence = 0; fence_len = 0 }
            {
                # Strip up to 3 leading spaces for fence detection (GFM rule)
                # (match + substr, not / ? ? ?/: mawk strips only one space with that)
                stripped = $0
                match(stripped, /^ */)
                if (RLENGTH <= 3) stripped = substr(stripped, RLENGTH + 1)
                # Check if this line is a candidate fence opener/closer
                if (match(stripped, /^```+/)) {
                    run = RLENGTH
                    if (!in_fence) {
                        # Opening a new fence
                        in_fence = 1
                        fence_len = run
                        next
                    } else if (run >= fence_len) {
                        # Closing the fence (must have >= opening run length)
                        in_fence = 0
                        fence_len = 0
                        next
                    }
                    # run < fence_len: this is inside the outer fence, treat as content
                }
                if (!in_fence) { print }
            }
        ' \
        | sed 's/`[^`]*`//g'
}

# --- Cloud session routing (Projects v2 proxy bypass) --------------------------------------
# Projects v2 is GraphQL-only, and a cloud session's GitHub proxy serves only a
# pinned set of GraphQL operations — every board call 403s there regardless of
# the token supplied. So in the cloud the board helpers below do not call
# GraphQL at all: the two ID-returning helpers emit a SENTINEL instead of a real
# node ID, and the two mutating helpers recognise that sentinel and dispatch
# the repo's board-sync workflow (via a repo-provided scripts/board-op.sh, REST)
# to do the work on a runner. With no board-op.sh the dispatch fails with an
# ERROR and the SKILL.md call sites WARN and continue.
#
# The sentinel is what keeps every call site in SKILL.md unchanged: a phase still
# "gets an item id" and still "sets a field on it"; only the plumbing under those
# two steps differs by environment.
_board_is_cloud() { [[ "${CLAUDE_CODE_REMOTE:-}" = "true" ]]; }

# BOARD_OP_BIN exists so the routing is testable without a real board or a real
# workflow dispatch — test-lib.sh points it at a stub that
# records its arguments. Unset in normal use.
_board_op() {
    local root bin="${BOARD_OP_BIN:-}"
    if [[ -z "$bin" ]]; then
        root=$(git rev-parse --show-toplevel 2>/dev/null) || return 1
        bin="${root}/scripts/board-op.sh"
    fi
    if [[ ! -x "$bin" ]]; then
        echo "  ERROR: board-op.sh missing or not executable: $bin" >&2
        return 1
    fi
    "$bin" "$@"
}

# Decodes a CLOUD:<n> / CLOUD_NOCREATE:<n> sentinel into the board-op.sh argv it
# stands for, assigning to the caller's board_argv array. Returns non-zero for a
# real node ID, which is how the callers below tell the two apart.
#
# There is deliberately NO name<->option-ID map here. The board-sync workflow takes
# free-form strings and allowlists them, so raw option IDs pass straight through
# to board-op.sh, which owns the single mapping. An earlier
# revision kept a reverse map at this layer because the workflow's inputs were
# `type: choice` and rejected raw IDs; that put the same table in four places
# (here x3 mirrors, plus board-op.sh) with nothing keeping them in sync.
_board_sentinel_argv() {
    case "$1" in
        CLOUD_NOCREATE:*) board_argv=("${1#CLOUD_NOCREATE:}" --no-create) ;;
        CLOUD:*)          board_argv=("${1#CLOUD:}") ;;
        *) return 1 ;;
    esac
}

# parse_pr_closes BODY
# Emits one issue number per line from `Closes #N` / `Fixes #N` / `Resolves #N`
# references in the PR body (case-insensitive). Matches GitHub's exact
# auto-close keyword set: close, closes, closed, fix, fixes, fixed, resolve,
# resolves, resolved. (Note: "closing" is NOT in GitHub's list — the regex
# doesn't match it, intentionally.)
# Code blocks (fenced + inline) are stripped before matching — example code
# in PR bodies shouldn't be treated as real closing references.
# Empty stdout + exit 0 when no matches (no-match is a normal result, not an error).
parse_pr_closes() {
    local body="$1"
    _strip_code_blocks "$body" \
        | grep -oiE '(^|[^[:alnum:]_])(close[sd]?|fix(e[sd])?|resolve[sd]?)[[:space:]]*:?[[:space:]]+#[0-9]+' \
        | grep -oE '#[0-9]+' | tr -d '#' \
        || true
}

# validate_pr_invariants --has-label|--no-label --has-milestone|--no-milestone \
#                       --has-assignee|--no-assignee --has-closes|--no-closes
# Emits one WARN line per missing invariant to stderr. Returns 0 always
# (the caller decides whether missing invariants block merge); this is a
# pre-merge advisory, not a hard gate.
validate_pr_invariants() {
    local has_label=true has_milestone=true has_assignee=true has_closes=true
    while [ $# -gt 0 ]; do
        case "$1" in
            --has-label) has_label=true ;;
            --no-label) has_label=false ;;
            --has-milestone) has_milestone=true ;;
            --no-milestone) has_milestone=false ;;
            --has-assignee) has_assignee=true ;;
            --no-assignee) has_assignee=false ;;
            --has-closes) has_closes=true ;;
            --no-closes) has_closes=false ;;
            *) echo "validate_pr_invariants: unknown flag '$1'" >&2; return 1 ;;
        esac
        shift
    done

    if [ "$has_label" = false ]; then
        echo "WARN: PR has no labels. Apply labels manually via 'gh pr edit \$PR --add-label \"...\"'." >&2
    fi
    if [ "$has_milestone" = false ]; then
        # Do NOT hardcode a title here — milestone titles drift and closed ones are
        # not assignable (see /issue-creation Phase 4 Step 2).
        echo "WARN: PR has no milestone. Set via 'gh pr edit \$PR --milestone \"<title>\"' — list open titles with: gh api --paginate 'repos/{owner}/{repo}/milestones?state=open' --jq '.[].title'" >&2
    fi
    if [ "$has_assignee" = false ]; then
        echo "WARN: PR has no assignee. Assign via 'gh pr edit \$PR --add-assignee @me'." >&2
    fi
    if [ "$has_closes" = false ]; then
        echo "WARN: PR body has no 'Closes #N' reference — issue will not auto-close on merge." >&2
    fi
}

# get_authenticated_user
# Emits the gh-authenticated user's login on stdout, or empty + stderr WARN
# if gh isn't authenticated. Never exits non-zero — the caller decides
# how to handle the empty case (typically: skip assignee, log warning).
get_authenticated_user() {
    local login
    if login=$(gh api user --jq .login 2>/dev/null); then
        echo "$login"
    else
        echo "WARN: gh not authenticated; cannot auto-assign PR. Run 'gh auth login' or assign manually." >&2
        return 0
    fi
}

# move_to_status ITEM_ID OPTION_ID
# Updates the project board's Status field for the given item to the given option.
# MUTATING helper: requires PROJECT_ID and STATUS_FIELD_ID (set via repo-config
# `.project.id` / `.project.fields.status.id`, or exported). Fails loudly if unset
# rather than silently writing to the wrong board.
# Uses gh api graphql --input - because the gh CLI cannot pass complex variables
# via -F.
move_to_status() {
    local item_id="$1"
    local option_id="$2"
    local project_id="${PROJECT_ID:?PROJECT_ID required, set via repo-config .project.id}"
    local status_field_id="${STATUS_FIELD_ID:?STATUS_FIELD_ID required, set via repo-config .project.fields.status.id}"

    # Cloud session: $item_id is a CLOUD:/CLOUD_NOCREATE: sentinel rather than a
    # node ID, because Projects v2 is unreachable from here. Dispatch instead of
    # mutating — see "Cloud session routing" above. The raw option ID is passed
    # through; the board-sync workflow allowlists it and board-op.sh owns the mapping.
    local -a board_argv=()
    if _board_sentinel_argv "$item_id"; then
        _board_op "${board_argv[@]}" --status "$option_id"
        return $?
    fi

    local query='mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
        updateProjectV2ItemFieldValue(input: {
            projectId: $projectId,
            itemId: $itemId,
            fieldId: $fieldId,
            value: { singleSelectOptionId: $optionId }
        }) { projectV2Item { id } }
    }'

    local body
    body=$(jq -n \
        --arg query "$query" \
        --arg projectId "$project_id" \
        --arg itemId "$item_id" \
        --arg fieldId "$status_field_id" \
        --arg optionId "$option_id" \
        '{query: $query, variables: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId, optionId: $optionId}}')

    # IMPORTANT: use `printf '%s'` not `echo`. Some shells have xpg_echo enabled,
    # which makes `echo` expand `\n` escape sequences inside the JSON string back
    # into literal newlines — corrupting the JSON body and producing "Problems
    # parsing JSON" from gh. printf '%s' emits bytes verbatim.
    #
    # Capture the response and check for `.errors` — gh exits 0 even when the
    # GraphQL response is an error envelope, which would otherwise be silently
    # treated as success. Mutation only succeeds if data.updateProjectV2ItemFieldValue
    # came back AND no errors array.
    local response
    response=$(printf '%s' "$body" | gh api graphql --input - 2>&1) || {
        echo "  ERROR: move_to_status: gh api failed: $response" >&2
        return 1
    }
    if printf '%s' "$response" | jq -e '.errors' >/dev/null 2>&1; then
        echo "  ERROR: move_to_status: GraphQL errors in response: $(printf '%s' "$response" | jq -c '.errors')" >&2
        return 1
    fi
}

# find_project_item_id ISSUE_NUMBER
# Emits the project board item ID for the given issue, or empty if the
# issue is not yet on the project board.
# READ-ONLY lookup: when no project board is configured (PROJECT_ID empty),
# returns empty + exit 0 (nothing to look up) rather than failing.
#
# Error contract:
#   - No board configured → empty stdout, exit 0
#   - Issue not on project board → empty stdout, exit 0 (legitimate non-match)
#   - gh/network failure → empty stdout, exit 1 + stderr WARN (distinct signal)
#   - jq parse failure (gh returned non-JSON) → empty stdout, exit 1 + stderr WARN
# The caller in SKILL.md Phase 3 MUST check the exit code to distinguish
# "not on board" from "API failure" — the original triple-mask conflated them.
#
# GraphQL uses parameterized variables (not string interpolation) for
# defense-in-depth — matches the pattern in move_to_status.
find_project_item_id() {
    local issue_number="$1"
    local project_id="${PROJECT_ID:-}"

    # No project board configured → nothing to resolve (skip the gh lookups).
    [ -z "$project_id" ] && return 0

    # Cloud session: this lookup is a Projects v2 query and is blocked here, so
    # emit a sentinel for move_to_status/set_priority to route on.
    #
    # CLOUD_NOCREATE, not CLOUD: this function's local contract is LOOKUP-ONLY —
    # empty means "not on the board" and SKILL.md Phase 3a skips the Status sync
    # on that. A plain CLOUD: sentinel would make the runner's addProjectV2ItemById
    # force-add an issue deliberately kept off the board, so the cloud path would
    # do something the local path refuses. --no-create reproduces the skip.
    # ensure_pr_on_project is the opposite case and does emit CLOUD:.
    #
    # The digits-only guard is repeated because the normal one below is not
    # reached on this path, and the sentinel feeds a workflow dispatch.
    if _board_is_cloud; then
        if ! [[ "$issue_number" =~ ^[0-9]+$ ]]; then
            echo "  ERROR: find_project_item_id: issue_number '$issue_number' is not digits-only" >&2
            return 1
        fi
        printf 'CLOUD_NOCREATE:%s\n' "$issue_number"
        return 0
    fi

    local owner repo
    owner="$(_resolve_owner)"
    repo="$(_resolve_repo)"

    # Defensive input validation: issue_number MUST be digits-only.
    # Callers (parse_pr_closes/parse_pr_refs) already enforce this via grep -oE '[0-9]+',
    # but assert at the helper boundary so future callers can't slip injection.
    if ! [[ "$issue_number" =~ ^[0-9]+$ ]]; then
        echo "  ERROR: find_project_item_id: issue_number '$issue_number' is not digits-only" >&2
        return 1
    fi

    local query='query($owner: String!, $name: String!, $number: Int!) {
      repository(owner: $owner, name: $name) {
        issue(number: $number) {
          projectItems(first: 10) { nodes { id project { id } } }
        }
      }
    }'

    local body
    body=$(jq -n \
        --arg query "$query" \
        --arg owner "$owner" \
        --arg name "$repo" \
        --argjson number "$issue_number" \
        '{query: $query, variables: {owner: $owner, name: $name, number: $number}}')

    local response
    response=$(printf '%s' "$body" | gh api graphql --input - 2>&1) || {
        echo "  WARN: find_project_item_id: gh api failed for #$issue_number: $response" >&2
        return 1
    }

    # Check for GraphQL errors envelope before jq-parsing the data path
    if printf '%s' "$response" | jq -e '.errors' >/dev/null 2>&1; then
        echo "  WARN: find_project_item_id: GraphQL errors for #$issue_number: $(printf '%s' "$response" | jq -c '.errors')" >&2
        return 1
    fi

    # Now safely extract — empty result = legitimately not on board
    printf '%s' "$response" | jq -r --arg pid "$project_id" \
        '.data.repository.issue.projectItems.nodes[] | select(.project.id == $pid) | .id'
}

# validate_against_active_labels < LABEL_PER_LINE
# Reads candidate labels from stdin, emits only those present in the Active
# section of the labels source file. The source file is resolved from (in order):
#   $LABELS_FILE  →  repo-config `.labels.source` under the repo root  →
#   <repo-root>/.claude/rules/labels.md.
# Deprecated labels are dropped with a stderr WARN.
#
# Graceful degradation: when NO labels source was named (no $LABELS_FILE, no
# repo-config `.labels.source`) and the default .claude/rules/labels.md is ABSENT,
# this is a NO-OP passthrough that returns success. (A repo may legitimately not
# maintain a curated labels.md.) When a source WAS named and is missing, it fails
# closed (exit 1, no output) so the taxonomy check is not silently skipped.
#
# When the source file EXISTS but its Active section is empty/malformed, that's a
# real misconfiguration: return exit 1 rather than silently passing through
# deprecated labels and defeating the labels.md drift discipline.
#
# The namespace allowlist regex is built from LABEL_NAMESPACES (default from
# repo-config `.labels.namespaces`).
validate_against_active_labels() {
    local labels_file="${LABELS_FILE:-}" explicit=1
    if [ -z "$labels_file" ]; then
        local root configured
        root="${REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}"
        # Without the helper (a --user or plugin-only install) read the key directly,
        # so a named source still fails closed instead of degrading to passthrough.
        if declare -F rc_get >/dev/null; then
            configured="$(rc_get '.labels.source' '')"
        else
            configured="$(jq -r '.labels.source // empty' "${root%/}/.claude/repo-config.json" 2>/dev/null || true)"
        fi
        [[ -n "$configured" ]] || { explicit=0; configured=.claude/rules/labels.md; }
        labels_file="${root%/}/${configured}"
    fi

    # Source file absent: a repo that never named one degrades to a passthrough.
    # A source that was named (LABELS_FILE or .labels.source) and is missing fails
    # closed, because passing labels through would skip the taxonomy check.
    if [ ! -f "$labels_file" ]; then
        if [[ "$explicit" == 1 ]]; then
            echo "ERROR: labels source '${labels_file}' not found; refusing to pass labels through unchecked." >&2
            return 1
        fi
        echo "INFO: labels source '${labels_file}' not found; skipping Active-labels validation (passthrough)." >&2
        cat
        return 0
    fi

    # Build the Active-labels allowlist, scoped to the configured namespaces.
    # When LABEL_NAMESPACES is empty, _build_active_label_regex returns "" —
    # degrade to a no-op passthrough (same graceful path as absent labels file).
    local ns_regex
    ns_regex="$(_build_active_label_regex)"
    if [ -z "$ns_regex" ]; then
        echo "INFO: LABEL_NAMESPACES is empty; skipping Active-labels validation (passthrough)." >&2
        cat
        return 0
    fi

    local allowlist
    # The Active section when the taxonomy has one; otherwise the whole file up to any
    # Deprecated section (the bundled labels.md lists labels under namespace headings).
    local active_range='/^## Active labels/,/^## Deprecated labels/p'
    grep -q '^## Active labels' "$labels_file" || active_range='1,/^## Deprecated labels/p'
    allowlist=$(sed -n "$active_range" "$labels_file" \
        | grep -oE "\`(${ns_regex}): [a-z0-9.-]+\`" \
        | sed 's/^`//; s/`$//' \
        | sort -u) || true

    if [ -z "$allowlist" ]; then
        echo "ERROR: no active labels parsed from ${labels_file} (malformed Active section?). /pr-creation cannot validate without the allowlist; refusing to passthrough deprecated labels." >&2
        cat >/dev/null
        return 1
    fi

    while IFS= read -r label; do
        [ -z "$label" ] && continue
        if printf '%s\n' "$allowlist" | grep -qxF "$label"; then
            printf '%s\n' "$label"
        else
            echo "WARN: skipping deprecated label '$label' (not in the labels source Active section)" >&2
        fi
    done
}

# filter_inherited_labels JSON_LABEL_ARRAY
# Emits one label per line, filtered for inheritance into a PR.
# Input is a JSON array of strings (e.g., from `gh issue view --json labels --jq '[.labels[].name]'`).
# Excludes: `type: epic` (PRs are not epics), any `routine: *` (PRs are not
# auto-generated), and `risk: hotfix`.
#
# `risk: hotfix` is excluded for a different reason than the other two, and the
# distinction matters. Those are semantically wrong on a PR. This one is a
# RELEASE CONTROL in repos that define it: on the PR it means "this merge cuts a
# release immediately, bypassing the scheduled blocking-red gate", so a hotfix
# can ship on a red suite. On the ISSUE it meant something
# weaker and older — "this problem is urgent" — decided during triage, possibly
# weeks earlier and by someone else.
#
# Inheriting it would silently convert a triage opinion into an ungated release
# nobody consciously chose. The label is not lost: callers warn and print the
# exact command to apply it, so the decision is made at PR time by a person.
filter_inherited_labels() {
    local labels_json="$1"
    printf '%s' "$labels_json" \
        | jq -r '.[] | select(. != "type: epic") | select(. != "risk: hotfix") | select(startswith("routine: ") | not)'
}

# dropped_release_controls JSON_LABEL_ARRAY
# Emits the release-control labels filter_inherited_labels deliberately dropped,
# one per line, so the caller can surface them instead of silently discarding.
dropped_release_controls() {
    local labels_json="$1"
    printf '%s' "$labels_json" | jq -r '.[] | select(. == "risk: hotfix")'
}

# warn_dropped_release_controls JSON_LABEL_ARRAY ISSUE_NUMBER PR_NUMBER
# Writes the operator-facing warning for each withheld release control to stderr,
# including a copy-pasteable command to apply it deliberately. Silent when none.
#
# This lives here rather than inline in SKILL.md because the inline version had a
# bug no test could reach: `for d in $(dropped_release_controls ...)` word-splits
# on the space inside `risk: hotfix`, iterating over `risk:` and `hotfix` and
# emitting two --add-label commands for labels that do not exist — breaking the
# one thing the warning exists to provide. Logic that can be wrong belongs where
# the suite can execute it.
warn_dropped_release_controls() {
    local labels_json="$1" issue_n="$2" pr_number="$3" dropped
    # `while IFS= read -r`, never `for ... in $(...)` — label names contain spaces.
    dropped_release_controls "$labels_json" | while IFS= read -r dropped; do
        [[ -n "$dropped" ]] || continue
        echo "WARN: #${issue_n} carries '${dropped}' — NOT inherited." >&2
        echo "      In repos that use it as a release control, on this PR it" >&2
        echo "      would cut a release on merge and bypass the scheduled gate." >&2
        echo "      Apply it only if that is the intent:" >&2
        echo "      gh pr edit ${pr_number} --add-label '${dropped}'" >&2
    done
}

# parse_pr_refs BODY
# Emits one issue number per line from `Refs #N` / `Ref #N` references
# (non-closing — these are related but not auto-closed on merge).
# Code blocks stripped before matching (same rationale as parse_pr_closes).
# Empty stdout + exit 0 when no matches.
parse_pr_refs() {
    local body="$1"
    _strip_code_blocks "$body" \
        | grep -oiE '(^|[^[:alnum:]_])refs?[[:space:]]*:?[[:space:]]+#[0-9]+' \
        | grep -oE '#[0-9]+' | tr -d '#' \
        || true
}

# pick_inheritance_source CLOSES REFS
# Emits the issue number this PR should inherit metadata from: the first
# `Closes` issue if there is one, else the first `Refs` issue.
# Empty stdout + exit 0 when neither is present (no-source is a normal result,
# not an error — the `set -e` call sites depend on this).
#
# Used by BOTH Phase 3c (Project Priority) and Phase 4 (milestone). Both write
# the PR's OWN board item / milestone, so both take the same source. (Phase 3a
# is different and correctly stays `Closes`-only: it mutates the *issue's*
# board Status, and a `Refs` issue is not being closed by this PR.)
#
# The `Refs` fallback is not a courtesy — staged multi-PR work references its
# issue with `Refs #N` ON PURPOSE so stage 1 does not auto-close the issue and
# orphan stages 2..n. Those PRs belong to the issue's milestone exactly as much
# as a `Closes` PR does, so inheriting only from `Closes` silently drops them
# out of the release-tracking view (observed on a staged four-PR change).
# `Closes` still wins when both are present.
pick_inheritance_source() {
    local closes="$1" refs="$2"
    printf '%s\n%s\n' "$closes" "$refs" | grep -m1 -E '^[0-9]+$' || true
}

# ensure_pr_on_project PR_NUMBER
# Adds PR to the project board (idempotent) and emits its project item ID.
# By design: ALL PRs are technically in review status,
# even in draft — so we always ensure the PR is on the board with Status: In Review.
#
# addProjectV2ItemById is idempotent — if the PR is already on the board,
# it returns the existing item ID rather than creating a duplicate.
#
# Error contract:
#   - Success → emit item ID, exit 0
#   - PR not found / gh failure → empty stdout, exit 1 + stderr WARN
ensure_pr_on_project() {
    local pr_number="$1"
    local project_id="${PROJECT_ID:?PROJECT_ID required, set via repo-config .project.id}"

    # Cloud session: adding to the board is a Projects v2 mutation and is
    # blocked here. Emit the sentinel — the runner's board-op.sh does the add
    # via addProjectV2ItemById, which is idempotent, so the caller's subsequent
    # move_to_status dispatch adds-and-sets in one go.
    if _board_is_cloud; then
        if ! [[ "$pr_number" =~ ^[0-9]+$ ]]; then
            echo "  ERROR: ensure_pr_on_project: pr_number '$pr_number' is not digits-only" >&2
            return 1
        fi
        printf 'CLOUD:%s\n' "$pr_number"
        return 0
    fi

    local owner repo
    owner="$(_resolve_owner)"
    repo="$(_resolve_repo)"

    if ! [[ "$pr_number" =~ ^[0-9]+$ ]]; then
        echo "  ERROR: ensure_pr_on_project: pr_number '$pr_number' is not digits-only" >&2
        return 1
    fi

    # Step 1: get the PR's GraphQL node ID (the "content ID" used by addProjectV2ItemById).
    local content_id
    content_id=$(gh api "repos/${owner}/${repo}/pulls/${pr_number}" --jq '.node_id' 2>&1) || {
        echo "  WARN: ensure_pr_on_project: failed to fetch PR #${pr_number} node ID: ${content_id}" >&2
        return 1
    }

    # Step 2: idempotent add — returns existing item ID if already on the board.
    local query='mutation($projectId: ID!, $contentId: ID!) {
        addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
            item { id }
        }
    }'

    local body
    body=$(jq -n \
        --arg query "$query" \
        --arg projectId "$project_id" \
        --arg contentId "$content_id" \
        '{query: $query, variables: {projectId: $projectId, contentId: $contentId}}')

    local response
    response=$(printf '%s' "$body" | gh api graphql --input - 2>&1) || {
        echo "  WARN: ensure_pr_on_project: gh api failed for PR #${pr_number}: ${response}" >&2
        return 1
    }

    if printf '%s' "$response" | jq -e '.errors' >/dev/null 2>&1; then
        echo "  WARN: ensure_pr_on_project: GraphQL errors for PR #${pr_number}: $(printf '%s' "$response" | jq -c '.errors')" >&2
        return 1
    fi

    printf '%s' "$response" | jq -r '.data.addProjectV2ItemById.item.id'
}

# get_issue_priority_option_id ISSUE_NUMBER
# Emits the option ID of the issue's current Priority field value, or empty
# if the issue has no Priority set (or isn't on the project board).
# READ-ONLY lookup: when no project board is configured (PROJECT_ID empty),
# returns empty + exit 0.
#
# Error contract:
#   - No board configured → empty stdout, exit 0
#   - Issue has Priority set → emit option ID, exit 0
#   - Issue not on board or Priority unset → empty stdout, exit 0
#   - gh/network failure → empty stdout, exit 1 + stderr WARN
get_issue_priority_option_id() {
    local issue_number="$1"
    local project_id="${PROJECT_ID:-}"

    # No project board configured → nothing to look up (skip the gh lookups).
    [ -z "$project_id" ] && return 0

    # Cloud session: reading the source issue's Priority is a Projects v2 query,
    # blocked here. Defer the whole inheritance to the runner by emitting
    # CLOUD_INHERIT:<issue>, which set_priority turns into
    # --inherit-priority-from. Callers test this value for emptiness to mean
    # "nothing to inherit", so the sentinel must be non-empty.
    if _board_is_cloud; then
        if ! [[ "$issue_number" =~ ^[0-9]+$ ]]; then
            echo "  ERROR: get_issue_priority_option_id: issue_number '$issue_number' is not digits-only" >&2
            return 1
        fi
        printf 'CLOUD_INHERIT:%s\n' "$issue_number"
        return 0
    fi

    local owner repo
    owner="$(_resolve_owner)"
    repo="$(_resolve_repo)"

    if ! [[ "$issue_number" =~ ^[0-9]+$ ]]; then
        echo "  ERROR: get_issue_priority_option_id: issue_number '$issue_number' is not digits-only" >&2
        return 1
    fi

    local query='query($owner: String!, $name: String!, $number: Int!) {
      repository(owner: $owner, name: $name) {
        issue(number: $number) {
          projectItems(first: 10) {
            nodes {
              project { id }
              fieldValueByName(name: "Priority") {
                ... on ProjectV2ItemFieldSingleSelectValue { optionId }
              }
            }
          }
        }
      }
    }'

    local body
    body=$(jq -n \
        --arg query "$query" \
        --arg owner "$owner" \
        --arg name "$repo" \
        --argjson number "$issue_number" \
        '{query: $query, variables: {owner: $owner, name: $name, number: $number}}')

    local response
    response=$(printf '%s' "$body" | gh api graphql --input - 2>&1) || {
        echo "  WARN: get_issue_priority_option_id: gh api failed for #${issue_number}: ${response}" >&2
        return 1
    }

    if printf '%s' "$response" | jq -e '.errors' >/dev/null 2>&1; then
        echo "  WARN: get_issue_priority_option_id: GraphQL errors for #${issue_number}: $(printf '%s' "$response" | jq -c '.errors')" >&2
        return 1
    fi

    # Extract Priority optionId for the matching project. Empty when issue
    # has no Priority set or isn't on this project board.
    printf '%s' "$response" | jq -r --arg pid "$project_id" \
        '[.data.repository.issue.projectItems.nodes[] | select(.project.id == $pid) | .fieldValueByName.optionId // empty] | .[0] // empty'
}

# set_priority ITEM_ID OPTION_ID
# Sets the Priority single-select field on a project item.
# Mirrors move_to_status but uses the Priority field ID.
# MUTATING helper: requires PROJECT_ID and PRIORITY_FIELD_ID (set via repo-config
# `.project.id` / `.project.fields.priority.id`, or exported). Fails loudly if unset.
set_priority() {
    local item_id="$1"
    local option_id="$2"
    local project_id="${PROJECT_ID:?PROJECT_ID required, set via repo-config .project.id}"
    local priority_field_id="${PRIORITY_FIELD_ID:?PRIORITY_FIELD_ID required, set via repo-config .project.fields.priority.id}"

    # Cloud session — see move_to_status. Two sentinels can arrive here: the item
    # is a CLOUD:/CLOUD_NOCREATE: handle, and the option may be
    # CLOUD_INHERIT:<issue> when get_issue_priority_option_id could not read the
    # source issue's Priority (that read is a Projects v2 query, blocked too).
    # In that case the runner resolves the inheritance itself.
    local -a board_argv=()
    if _board_sentinel_argv "$item_id"; then
        if [[ "${option_id#CLOUD_INHERIT:}" != "$option_id" ]]; then
            _board_op "${board_argv[@]}" --inherit-priority-from "${option_id#CLOUD_INHERIT:}"
            return $?
        fi
        _board_op "${board_argv[@]}" --priority "$option_id"
        return $?
    fi

    local query='mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
        updateProjectV2ItemFieldValue(input: {
            projectId: $projectId,
            itemId: $itemId,
            fieldId: $fieldId,
            value: { singleSelectOptionId: $optionId }
        }) { projectV2Item { id } }
    }'

    local body
    body=$(jq -n \
        --arg query "$query" \
        --arg projectId "$project_id" \
        --arg itemId "$item_id" \
        --arg fieldId "$priority_field_id" \
        --arg optionId "$option_id" \
        '{query: $query, variables: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId, optionId: $optionId}}')

    local response
    response=$(printf '%s' "$body" | gh api graphql --input - 2>&1) || {
        echo "  ERROR: set_priority: gh api failed: $response" >&2
        return 1
    }
    if printf '%s' "$response" | jq -e '.errors' >/dev/null 2>&1; then
        echo "  ERROR: set_priority: GraphQL errors in response: $(printf '%s' "$response" | jq -c '.errors')" >&2
        return 1
    fi
}
