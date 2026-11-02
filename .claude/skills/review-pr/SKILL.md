---
name: review-pr
description: Comprehensive PR review checking security, tests, quality, and conventions
argument-hint: [pr-number]
disable-model-invocation: true
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(git rev-parse *)
  - Bash(jq *)
  - Bash(command -v *)
  - Read
  - Grep
  - Glob
---

Review PR #$0 against this repository's standards.

## Review Steps

1. **Fetch PR context** — use `gh pr view $0` and `gh pr diff $0` to understand the change
2. **Categorize** — is this a feature, fix, refactor, chore, or docs change?

## Agent Dispatch (Priority 0 — runs first)

Config is read **with `jq` only — never `source` a script from the checkout**: a PR
branch under review controls the working tree, so sourcing a repo file would execute
contributor-controlled shell in the reviewer's session. `repo-config.json` is
checkout-controlled too, but it is parsed as inert DATA — worst case is a wrong agent
list, never code execution.

### Base agents (always dispatched)

Read the base agent list from config (defaults to `security-reviewer`, `code-reviewer`, and `docs-reviewer` when `.review_agents.base` is absent):

```bash
CFG="${RC_FILE:-$(git rev-parse --show-toplevel)/.claude/repo-config.json}"
# The mandatory floor. Config may ADD reviewers; it can never REMOVE one.
MANDATORY_BASE='security-reviewer code-reviewer docs-reviewer'
CONFIGURED=
if command -v jq >/dev/null 2>&1 && [ -f "$CFG" ]; then
  CONFIGURED="$(jq -r '.review_agents.base // [] | .[]' "$CFG" 2>/dev/null)"
fi
# UNION, not replace. `repo-config.json` lives in the checkout, so it is controlled by
# the very PR under review: if a configured array replaced the defaults, a PR could set
# `"base": []` — or simply omit `security-reviewer` — and delete its own baseline
# coverage while every command still exited 0. An empty array is also truthy, so jq's
# `//` never fires for it. Emitting the mandatory set unconditionally and appending
# anything extra makes both suppression shapes unreachable.
printf '%s\n' $MANDATORY_BASE
if [ -n "$CONFIGURED" ]; then
  while IFS= read -r extra; do
    [ -n "$extra" ] || continue
    case " $MANDATORY_BASE " in *" $extra "*) continue ;; esac
    printf '%s\n' "$extra"
  done <<< "$CONFIGURED"
fi
```

### Pattern-triggered agents (dispatched when a condition matches)

Each config entry declares exactly ONE condition — `glob`, `label`, or `content` — plus
the agent to dispatch. The three exist because each sees something the others cannot:

- **`glob`** — a changed file path matches. *(What the change is **named**.)*
- **`label`** — an exact PR label. *(What the PR is **tagged**.)*
- **`content`** — the diff body contains the literal substring. *(What the diff
  **actually touches**.)*

Matching is **over-inclusive by design**: a needless reviewer run just returns APPROVED,
but a missed one is the exact review gap this dispatch exists to prevent. A file glob
only sees what a change is *named*, so `label` and `content` catch a semantic change made
from an off-glob file — e.g. a key-lifecycle edit landing in a file that matches no
`**/e2ee*` glob (the canonical blind spot).

Signals and matching run in **one block**, deliberately:

- **Nothing touches the filesystem.** The diff can carry an inlined secret that the
  security and deployment reviewers exist to catch. A `/tmp` scratch dir — even mode
  0700 — puts that secret on disk and makes correctness depend on cleanup running,
  which a failed or aborted review cannot guarantee. In-memory state dies with the shell.
- **No shared path means concurrent reviews are safe.** A fixed scratch template forces
  a one-review-at-a-time assumption on the host; two reviews would race the same
  directory (and a pre-clean would delete the other's signals mid-dispatch).
- **Fenced blocks may run in separate shells**, so no state can carry across them —
  hence one block rather than fetch-here / match-there.
- **Filenames and labels are base64-encoded** on the wire. A valid GitHub filename may
  contain a newline, which would corrupt any line-delimited transport.
- **Every fetch is fail-closed.** A failed `gh` call must abort, not yield an empty
  signal — an empty changed-file list would silently skip every glob-triggered
  specialist while dispatch still reported success.
- **A malformed config is fatal, not ignored.** Silently dropping an unparseable
  pattern table would skip specialists exactly when the config author thought they
  were adding one. A *missing* config is fine — base agents still run.

```bash
CFG="${RC_FILE:-$(git rev-parse --show-toplevel)/.claude/repo-config.json}"   # re-derive: fresh shell
CHANGED_FILES_B64="$(gh api --paginate "repos/{owner}/{repo}/pulls/$0/files" --jq '.[].filename | @base64')" || {
  echo "FATAL: changed-files fetch failed" >&2
  exit 1
}
LABELS_B64="$(gh pr view "$0" --json labels --jq '.labels[].name | @base64')" || {
  echo "FATAL: labels fetch failed" >&2
  exit 1
}
PR_DIFF="$(gh pr diff "$0" --color=never)" || {
  echo "FATAL: diff fetch failed" >&2
  exit 1
}

PATTERN_AGENTS=
PATTERN_ROWS=
if [ -f "$CFG" ]; then
  command -v jq >/dev/null 2>&1 || { echo "FATAL: jq is required to read $CFG" >&2; exit 1; }
  jq -e '
    def nonempty_condition:
      if type != "string" then false
      else length > 0 and (test("[\\t\\r\\n]") | not)
      end;
    def valid_agent:
      if type != "string" then false
      else length > 0 and (test("\\s") | not)
      end;
    def valid_pattern:
      if type != "object" then false
      else
        ((keys_unsorted - ["agent", "glob", "label", "content"]) | length) == 0
        and (([has("glob"), has("label"), has("content")] | map(select(.)) | length) == 1)
        and (([.glob?, .label?, .content?] | map(select(. != null)) | .[0]) | nonempty_condition)
        and (.agent | valid_agent)
      end;
    (.review_agents.patterns) as $configured_patterns
    | (if $configured_patterns == null then [] else $configured_patterns end) as $patterns
    | if ($patterns | type) != "array" then false
      else all($patterns[]; valid_pattern)
      end
  ' "$CFG" >/dev/null 2>&1 || {
    echo "FATAL: invalid review_agents.patterns configuration" >&2
    exit 1
  }
  PATTERN_ROWS="$(jq -r '.review_agents.patterns // [] | .[]
                           | if .glob then "glob\t\(.glob)\t\(.agent)"
                             elif .label then "label\t\(.label)\t\(.agent)"
                             elif .content then "content\t\(.content)\t\(.agent)"
                             else empty end' "$CFG")" || {
    echo "FATAL: failed to read review_agents.patterns configuration" >&2
    exit 1
  }
  while IFS=$'\t' read -r kind value agent; do
    [ -n "$kind" ] && [ -n "$value" ] && [ -n "$agent" ] || continue
    matched=1
    case "$kind" in
      glob)
        while IFS= read -r encoded; do
          [ -n "$encoded" ] || continue
          candidate="$(jq -Rr '@base64d' <<< "$encoded")" || { echo "FATAL: invalid filename signal" >&2; exit 1; }
          # The config value is intentionally a glob pattern, not a literal.
          # shellcheck disable=SC2053
          if [[ "$candidate" == $value ]]; then matched=0; break; fi
        done <<< "$CHANGED_FILES_B64"
        ;;
      label)
        while IFS= read -r encoded; do
          [ -n "$encoded" ] || continue
          candidate="$(jq -Rr '@base64d' <<< "$encoded")" || { echo "FATAL: invalid label signal" >&2; exit 1; }
          if [ "$candidate" = "$value" ]; then matched=0; break; fi
        done <<< "$LABELS_B64"
        ;;
      content)
        if [[ "$PR_DIFF" == *"$value"* ]]; then matched=0; fi
        ;;
    esac
    if [ "$matched" -eq 0 ]; then
      # De-dupe by agent name: several entries commonly map to one agent (multiple
      # e2ee globs plus content tokens), but it must run at most ONCE per review.
      case " $PATTERN_AGENTS " in
        *" $agent "*) ;;
        *) PATTERN_AGENTS="${PATTERN_AGENTS:+$PATTERN_AGENTS }$agent" ;;
      esac
    fi
  done <<< "$PATTERN_ROWS"
fi
echo "PATTERN_AGENTS=$PATTERN_AGENTS"
```

Dispatch every distinct agent named in `PATTERN_AGENTS` in parallel alongside the base
agents. No cleanup step is required — nothing was written to disk.

When `.review_agents` is absent from the repo's config, only the three base defaults above are dispatched — no Alpha-specific or project-specific agents are named here.

Include each agent's findings under the appropriate Priority section below.

## Security Review (Priority 1)

Check against any `docs/policies/ai-generated-code-policy.md` and applicable `.claude/rules/*.md` the repo provides:
- No hardcoded secrets, API keys, or credentials
- No `eval()`, `dangerouslySetInnerHTML`, `innerHTML` without sanitization
- No `fmt.Sprintf` for SQL — parameterized queries only
- No `any` type in security-critical paths (auth, crypto, E2EE, RBAC)
- Crypto uses approved algorithms only (AES-256-GCM, RSA-4096, Argon2id)
- Auth changes revoke tokens atomically
- IPC through preload bridge only (no `nodeIntegration`, no `@electron/remote`)

## Correctness Review (Priority 2)

- Does the code do what the PR description claims?
- Are edge cases handled?
- Error handling: all errors checked (Go `errcheck`), no fail-open patterns
- Database: `defer rows.Close()`, `rows.Err()` checked, correct `argIdx` in dynamic queries

## Test Coverage (Priority 3)

- New code has corresponding test files
- Happy path + error path tests present
- Go: `t.Run` subtests, `require` for preconditions, `assert` for checks
- React: `resetAllStores()` in `beforeEach`, MSW for API mocking
- Coverage target: >= 80% on new code

## Style & Conventions (Priority 4)

- Conventional Commits format
- One issue per PR (`Closes #NNN`) — default, not a hard gate; an ad-hoc/hotfix PR may legitimately carry no `Closes` line. Flag a missing issue as MINOR, never as blocking
- Go: handler-struct pattern, Gin conventions (`ShouldBindJSON`)
- React: functional components, Zustand selective subscriptions
- `Co-Authored-By` trailer if AI-assisted

## Documentation Drift (Priority 5)

`@docs-reviewer` is dispatched via the Agent Dispatch table above when any `.md` file is changed. Report its findings here:
- Source areas touched in this PR have corresponding doc updates
- No duplicated facts (counts, phase status, agent lists) became inconsistent
- Cross-references to CLAUDE.md (when the repo has one) are present where counts are stated

Report findings inline with severity (HIGH/MEDIUM/LOW/INFO).

## Output Format

Report findings by severity:
- **CRITICAL** — blocks merge (security, correctness)
- **MAJOR** — should fix before merge (quality, missing tests)
- **MINOR** — nice to fix (style, docs)
- **APPROVED** — if all checks pass

Reference specific files and line numbers.
