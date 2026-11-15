---
name: review-pr
description: Comprehensive PR review checking security, tests, quality, and conventions
argument-hint: [pr-number]
disable-model-invocation: true
allowed-tools:
  - Bash(gh pr *)
  - Bash(gh api *)
  - Bash(git rev-parse *)
  - Bash(git -C * rev-parse HEAD)
  - Bash(git -C * diff --cached --quiet *)
  - Bash(git -C * hash-object --no-filters -- *)
  - Bash(git -C * hash-object --stdin)
  - Bash(git -C * ls-files -v -- *)
  - Bash(jq *)
  - Bash(find * -mindepth 1 -maxdepth 1 -print0)
  - Bash(pwd -P)
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

Read the base agent list from config. The mandatory floor defaults to `security-reviewer`, `code-reviewer`, and `docs-reviewer`. A repo can replace that floor with `.review_agents.mandatory`, but the trust guard below reads it only from the merge base with the default branch, so a PR cannot change its own floor; this block always prints the provisional default floor, and the guard's `DISPATCH_AGENTS` is authoritative. `.review_agents.base` only adds to the floor:

```bash
CFG="${RC_FILE:-$(git rev-parse --show-toplevel)/.claude/repo-config.json}"
# The mandatory floor. From the checkout, `.review_agents.base` may only ADD reviewers;
# replacing the floor (`.review_agents.mandatory`) is honoured only from the merge base.
MANDATORY_BASE='security-reviewer code-reviewer docs-reviewer'
# `.review_agents.mandatory` is NOT read here: the checkout is PR-controlled. The trust
# guard below replaces the floor from the merge base; dispatch only its DISPATCH_AGENTS.
if command -v jq >/dev/null 2>&1 && [ -f "$CFG" ] \
  && jq -e '.review_agents | type == "object" and has("mandatory")' "$CFG" >/dev/null 2>&1; then
  echo "note: the checkout sets .review_agents.mandatory; it is honoured only from the merge base (see DISPATCH_AGENTS)" >&2
fi
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

Before running this block, inject `REVIEW_PR_LOADER_ROOT` from the host-owned workspace
selection. Never derive it from Git, the process working directory, or repository text:

- Skill-catalog hosts: use the workspace prefix of this skill's absolute catalog path.
- Workspace-folder hosts: use the client-provided workspace-folder URI.

The value must identify one unambiguous physical absolute path. The guard accepts a
local `file://` URI or a native absolute path and canonicalizes it before comparison.
If the host does not expose that fact, stop and ask the user for the workspace root.

```bash
CFG="${RC_FILE:-$(git rev-parse --show-toplevel)/.claude/repo-config.json}"   # re-derive: fresh shell
# Fetched ONCE, as raw JSON, and used two ways. The glob matcher below consumes the
# base64 filename list exactly as before; the agent-definition guard further down needs
# the objects themselves, because a rename carries the removed path in
# `previous_filename` and a base64 line list cannot express it. Deriving both from one
# response keeps that to a single API call and guarantees the two views agree.
# Revisions are captured ONCE, up front, and everything below is read against them.
# The signals here are fetched by PR NUMBER in several separate calls, so a push
# landing mid-review would otherwise let the trust check inspect one revision while
# the reviewers read another. `head` is revalidated after the fetches rather than
# pinned into each call, because the files endpoint has no revision parameter -- so
# the guarantee offered is "nothing moved while we looked", enforced by aborting, not
# "every call saw the same tree".
PR_REVS="$(gh api "repos/{owner}/{repo}/pulls/$0" --jq '.head.sha + " " + .base.sha')" || {
  echo "FATAL: could not resolve the PR revisions" >&2
  exit 1
}
HEAD_SHA="${PR_REVS%% *}"; BASE_SHA="${PR_REVS##* }"
[ -n "$HEAD_SHA" ] && [ -n "$BASE_SHA" ] || { echo "FATAL: the PR revisions came back empty" >&2; exit 1; }

FILES_JSON="$(gh api --paginate --slurp "repos/{owner}/{repo}/pulls/$0/files")" || {
  echo "FATAL: changed-files fetch failed" >&2
  exit 1
}
CHANGED_FILES_B64="$(printf '%s' "$FILES_JSON" | jq -r '.[] | .[] | .filename | @base64')" || {
  echo "FATAL: could not read filenames from the changed-files response" >&2
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

# ── Agent definitions the PR under review controls ──────────────────────────
# The guard cannot vouch for ITSELF, so it refuses the case where it would have to.
# This block, and the dispatch instruction that consumes it, are read from a skill the
# PR under review can edit. A PR that modifies a reviewer definition AND rewrites the
# guard would be cleared by its own rewritten guard -- Codex raised exactly this, and
# it is correct. Nothing in this repository closes it: a guard living inside the tree
# it polices is not a trust boundary, which this block already states outright below.
#
# What CAN be closed is the ESCALATION. Refusing outright when the PR touches the skill
# makes the compound edit cost a REFUSAL instead of buying a dispatch, so the cheapest
# attack is no longer "edit the guard and the definition in one PR".
#
# THE RESIDUAL IS NOT CLOSED, and calling this a fix would be the dishonest reading. An
# attacker editing the skill deletes this check along with the rest, and a reviewer who
# runs the skill from that checkout is already executing attacker-authored instructions
# -- the refusal below is in the same file. Only a harness-owned guard, or one executed
# from the captured merge-base/trusted immutable revision, is a boundary. Neither exists
# here. Treat this as depth
# against the compound edit, never as the property Codex asked for.
#
# Note this is self-applying: a PR that changes this skill -- including the one that
# introduced this paragraph -- cannot be reviewed by it. That is the intended cost, and
# it is small: those PRs get a human.
#
# Matched by SHAPE, not by a literal path. This file is mirrored to `.codex/skills/`
# and `.github/skills/`, the mirror rewrite would bite a hardcoded `.claude/...`, and
# a reviewer may legitimately be running any of the three copies.
# Self-modification is checked below from the captured immutable trees. The mutable
# files response remains an additive-only signal for specialist selection.
#
# A reviewer's own definition lives in the checkout, so the PR being reviewed can
# rewrite it: change its `tools:` line, or delete a behavioural refusal from its body,
# and this dispatch then runs the attacker's profile against the attacker's tree. The
# concrete case that prompted this (a past CodeRabbit finding): `@docs-reviewer` holds
# `Bash(pre-commit run *)` and its drift-hook scan executes `entry:` scripts named by
# the checkout's own `.pre-commit-config.yaml`. That agent grew an in-profile refusal,
# which a PR editing BOTH files simply removes.
#
# The general form is worth stating because it is not obvious: **no tool grant on any
# agent is a boundary against a hostile PR**, because the grant is declared in a file
# the PR can edit. The generated permission notes say the same thing from the other
# side ("nothing in this runtime enforces it"). Only the CALLER can hold this line,
# and only by refusing a definition it cannot trust.
#
# Refuse rather than base-revision-load. Loading `origin/<base>:.claude/agents/<n>.md`
# is the stronger repair and is the right eventual answer, but it silently reviews the
# PR with a profile that is not the one in the tree — including when the modification
# was legitimate, which is the common case (this repo edits agent definitions often).
# A refusal that names itself is honest about the coverage it dropped; a silent
# substitution is not. Revisit if the refusal proves noisy in practice.
# Re-derived, NOT inherited. Each fenced block in this file is its OWN shell -- `CFG`
# above is re-derived for exactly that reason -- so `MANDATORY_BASE` and `CONFIGURED`
# from the base-agents block do not exist here. Reading them would leave both empty,
# and the loop below would then filter only the pattern agents while every base
# reviewer went unchecked: a guard that silently covers a third of what it claims.
# Keep this list identical to the base-agents block; they are one contract in two
# shells. It is the DEFAULT floor: once the merge-base tree is read below, a repo's
# `.review_agents.mandatory` at the merge base replaces it.
MANDATORY_BASE='security-reviewer code-reviewer docs-reviewer'
CONFIGURED=
if [ -f "$CFG" ]; then
  CONFIGURED="$(jq -r '.review_agents.base // [] | .[]' "$CFG" 2>/dev/null)"
fi

DISPATCH_AGENTS=
SKIPPED_AGENTS=
# CLOSED BY THE MERGE BASE, AGNOSTIC BY CONSTRUCTION. Both halves are load-bearing,
# and the first one was MISSING from the original version of this guard.
#
# (1) CLOSED. The action this guard takes is to *skip* a reviewer, so an over-broad
#     match is not a "safe superset" -- it is the attack. The first version matched
#     `\.[a-z][a-z]*/agents/<n>`, so committing a meaningless
#     `.evil/agents/security-reviewer.md` made the guard drop the mandatory security-reviewer
#     run while reporting that its definition had changed: a one-file, zero-effort
#     primitive for disabling the reviewer that exists to catch you, and strictly worse
#     than having no guard at all. The valid directories are therefore read from the
#     captured revisions' merge base, which the PR head cannot edit; an invented tree
#     is absent there and so names no reviewer. Note the direction of every failure arm
#     below: when the merge base cannot be read, this ABORTS rather than defaulting
#     either way, because
#     "dispatch anyway" reinstates the untrusted-profile hole this guard exists to
#     close and "skip anyway" silently drops coverage.
#
# (2) AGNOSTIC. It still spells NO tree name. The mirror generator does a blind
#     `text.replace()` of one tree name for another, and that rewrites substrings
#     inside REGEXES as happily as inside prose. An earlier version hard-coded the
#     source tree and its `.md` suffix; the generated copies came out broken in two
#     DIFFERENT ways -- one had its tree name substituted while its suffix did not
#     (matching a `.md` file in a tree that holds only `.toml`, so it matched nothing
#     ever), and the other was not substituted at all (so it watched a tree that
#     harness does not load). Only the un-mirrored original was correct, and a test
#     that parsed only the original could not see either. Deriving the set from the
#     merge-base tree gives that substitution nothing to bite on, and still covers all three
#     definition surfaces -- `<n>.md`, `<n>.toml`, `<n>.agent.md` -- because a change to
#     ANY of them makes that agent contributor-controlled for SOME harness, and a
#     reviewer cannot know which harness reads next.
#
# WHAT THIS GUARD IS NOT. It is not a trust boundary, and must not be described as one.
# It lives in a file the PR under review can also edit, so an attacker who rewrites a
# reviewer definition can rewrite this block in the same commit. What it buys is that
# the bypass is no longer free or invisible: it forces the attacker to make a second,
# conspicuous edit to security tooling in the same diff, which is exactly the shape a
# human reviewer and the repo's own mirror-drift checks look for. Real enforcement
# belongs in harness-owned code that no checkout can reach; until that exists, read the
# diff of this file yourself whenever a PR touches it.
#
# Comparison is ATOMIC per decoded filename, never line-oriented: a path may legally
# contain a newline, and a `sed` filter would read the second line of one filename as a
# path of its own. The allowlist below rejects any path with a character a real
# definition never carries, which closes that vector before any matching happens. The
# suffix arms run longest-first so `.agent.md` is not shortened to `.agent` by the `.md`
# arm; an extensionless file and a `<n>.md.bak` match no arm and so name no agent -- a
# backup is not a definition, and a false skip trains people to ignore the warning.
# EXACT MATCH AGAINST THE MERGE BASE. Paths are constrained to the three loader
# grammars, while immutable profile bytes use deliberately tiny canonical frontmatter
# grammars. They reject aliases and unsupported YAML structures rather than trying to
# normalise them.
#
# The merge-base tree yields a map from a REAL definition path to the agent it defines. A
# changed path names a reviewer only if it equals one of those keys, byte for byte, as
# a JSON string. The agent NAME is derived on the trusted side of that comparison.
# What this closes, in the order it was reported:
#
#   * An invented tree. `<anything>/agents/security-reviewer.md` is not a key, so it
#     names nobody. The guard SKIPS a reviewer, so a false positive is a
#     reviewer-suppression primitive, not a conservative default — this is the property
#     that matters most.
#   * A crossed directory/suffix pair. `.github/agents/security-reviewer.md` is not a
#     key either: that tree holds `.agent.md`. Validating the directory and the suffix
#     independently accepted this combination even though no harness loads it.
#   * A trailing newline. A legal filename may end in one, and `"$( … )"` strips it, so
#     a decoded `…/security-reviewer.md\n` silently became `…/security-reviewer.md` and
#     suppressed that reviewer. Nothing is decoded in the shell now; `jq` compares the
#     JSON strings intact.
#   * Renames. GitHub reports a renamed definition under `previous_filename`, so a
#     rename that removes a reviewer's definition was invisible and left it dispatched
#     against a profile that no longer existed. Both names are considered.
#   * `base64 --decode` portability. Reported as breaking on stock macOS; it did NOT
#     reproduce here (`/usr/bin/base64 --decode` exits 0). Recorded rather than
#     silently dismissed — and moot either way, since no shell decode remains.
#
# It still spells NO tree name, so the mirror generator's blind substring rewrite has
# nothing to bite on and all three copies stay correct.
#
# Immutable tree comparison is the trust decision. The PR files response can race
# (including A→B→A), so it must never decide self-modification or reviewer identity.
MERGE_BASE_RESPONSE="$(gh api "repos/{owner}/{repo}/compare/${BASE_SHA}...${HEAD_SHA}")" \
  || { echo "FATAL: could not resolve the reviewed revisions' merge base" >&2; exit 1; }
MERGE_BASE_SHA="$(printf '%s' "$MERGE_BASE_RESPONSE" | jq -er '
  .merge_base_commit.sha | select(type == "string" and test("^[0-9a-fA-F]{40,64}$"))
')" || { echo "FATAL: the reviewed revisions have no usable merge-base SHA" >&2; exit 1; }
HEAD_COMMIT="$(gh api "repos/{owner}/{repo}/git/commits/${HEAD_SHA}")" \
  || { echo "FATAL: could not resolve the reviewed head commit tree" >&2; exit 1; }
MERGE_BASE_COMMIT="$(gh api "repos/{owner}/{repo}/git/commits/${MERGE_BASE_SHA}")" \
  || { echo "FATAL: could not resolve the reviewed merge-base commit tree" >&2; exit 1; }
HEAD_TREE_SHA="$(printf '%s' "$HEAD_COMMIT" | jq -er '.tree.sha | select(type == "string" and length > 0)')" \
  || { echo "FATAL: the reviewed head commit has no usable tree SHA" >&2; exit 1; }
MERGE_BASE_TREE_SHA="$(printf '%s' "$MERGE_BASE_COMMIT" | jq -er '.tree.sha | select(type == "string" and length > 0)')" \
  || { echo "FATAL: the reviewed merge-base commit has no usable tree SHA" >&2; exit 1; }
HEAD_TREE="$(gh api "repos/{owner}/{repo}/git/trees/${HEAD_TREE_SHA}?recursive=1")" \
  || { echo "FATAL: could not read the reviewed head tree" >&2; exit 1; }
MERGE_BASE_TREE="$(gh api "repos/{owner}/{repo}/git/trees/${MERGE_BASE_TREE_SHA}?recursive=1")" \
  || { echo "FATAL: could not read the reviewed merge-base tree" >&2; exit 1; }

for tree_side in "$HEAD_TREE" "$MERGE_BASE_TREE"; do
  printf '%s' "$tree_side" | jq -e '
    type == "object" and .truncated == false and (.tree | type == "array")
    and (([.tree[].path] | length) == ([.tree[].path] | unique | length))
    and all(.tree[]; type == "object"
      and (.path | type == "string") and (.type | type == "string")
      and (.mode | type == "string") and (.sha | type == "string" and length > 0))
  ' >/dev/null || {
    echo "FATAL: a recursive tree response was malformed or truncated; immutable comparison is incomplete" >&2
    exit 1
  }
done

# THE MANDATORY FLOOR CAN BE REPLACED, BUT ONLY FROM THE DEFAULT BRANCH. A repo whose
# always-run reviewers differ from the default (for example one that retired
# `security-reviewer` for `red-team`) sets `.review_agents.mandatory` in
# `.claude/repo-config.json`. It is never read from the checkout. It is read from the
# merge base of the head with the repo's DEFAULT branch, so the PR under review cannot
# shrink its own floor: a change to the key takes effect only after it merges there.
# The PR's own base is not enough, because the author picks it: an unreviewed branch,
# or a stacked PR, could set a weaker floor or drop the key. When the PR targets the
# default branch the two merge bases are the same and no extra call is made. File or
# key absent keeps the default. Present but unreadable, malformed, or not a regular
# file stops the review. The path is spelled split so the mirror generator's
# substring rewrite leaves it alone.
PR_BASE="$(gh api "repos/{owner}/{repo}/pulls/$0" --jq '.base.ref + " " + .base.sha')" \
  && DEFAULT_REF="$(gh api "repos/{owner}/{repo}" --jq '.default_branch')" \
  && [ -n "$DEFAULT_REF" ] \
  || { echo "FATAL: could not resolve the PR base branch or the default branch" >&2; exit 1; }
[ "${PR_BASE##* }" = "$BASE_SHA" ] || { echo "FATAL: the PR base moved during the review" >&2; exit 1; }
CFG_TREE="$MERGE_BASE_TREE"
if [ "${PR_BASE%% *}" != "$DEFAULT_REF" ]; then
  CFG_TREE="$(gh api "repos/{owner}/{repo}/compare/${DEFAULT_REF}...${HEAD_SHA}" --jq '.merge_base_commit.commit.tree.sha' \
    | jq -Rer 'select(test("\\A[0-9a-fA-F]{40,64}\\z"))' \
    | { read -r t && gh api "repos/{owner}/{repo}/git/trees/${t}?recursive=1"; })" \
    && printf '%s' "$CFG_TREE" | jq -e 'type == "object" and .truncated == false and (.tree | type == "array")' >/dev/null \
    || { echo "FATAL: could not read the tree at the head's merge base with the default branch" >&2; exit 1; }
fi
MB_CFG_SHA="$(printf '%s' "$CFG_TREE" | jq -r --arg p ".cl""aude/repo-config.json" '
  [.tree[] | select(.path == $p)]
  | if length == 0 then ""
    elif length == 1 and .[0].type == "blob" and .[0].mode == "100644" then .[0].sha
    else error("the merge-base repo config is not a regular file") end
')" || { echo "FATAL: could not search the merge-base tree, or its repo config is not a regular file" >&2; exit 1; }
if [ -n "$MB_CFG_SHA" ]; then
  MB_FLOOR="$(gh api "repos/{owner}/{repo}/git/blobs/${MB_CFG_SHA}" --jq 'select(.encoding == "base64") | .content' \
    | jq -Rrs 'gsub("\\s"; "") | if . == "" then error("empty blob") else . end | @base64d | fromjson
        | if type != "object" then error("the repo config must be a JSON object")
          elif has("review_agents") | not then ""
          elif (.review_agents | type) != "object" then error("review_agents must be an object")
          elif .review_agents | has("mandatory") | not then ""
          else .review_agents.mandatory
            | if type == "array" and length > 0 and all(.[]; type == "string" and test("\\A[a-z0-9][a-z0-9-]*\\z"))
              then join(" ") else error("review_agents.mandatory must be a non-empty array of agent names") end
          end')" \
    || { echo "FATAL: the merge-base repo config could not be read or its .review_agents.mandatory is malformed" >&2; exit 1; }
  [ -z "$MB_FLOOR" ] || MANDATORY_BASE="$MB_FLOOR"
fi
# A floor agent the repo does not ship would be dropped silently at dispatch, leaving the
# always-run set to the checkout's .review_agents.base, which this PR controls. Fail closed.
for agent in $MANDATORY_BASE; do
  printf '%s' "$CFG_TREE" | jq -e --arg p ".cl""aude/agents/${agent}.md" 'any(.tree[]; .path == $p and .type == "blob" and .mode == "100644")' >/dev/null \
    || { echo "FATAL: floor agent '$agent' has no regular-file definition at the merge base; set .review_agents.mandatory to agents this repo ships" >&2; exit 1; }
done

TREE_DIFF="$(printf '%s\n%s\n' "$MERGE_BASE_TREE" "$HEAD_TREE" | jq -sc '
  (.[0].tree | map({key: .path, value: {type: .type, mode: .mode, sha: .sha}}) | from_entries) as $base
  | (.[1].tree | map({key: .path, value: {type: .type, mode: .mode, sha: .sha}}) | from_entries) as $head
  | ([ ($base | keys[]), ($head | keys[]) ] | unique) as $paths
  | [ $paths[] as $path
      | ($base[$path] // null) as $base_entry
      | ($head[$path] // null) as $head_entry
      | select($base_entry != $head_entry)
      | {path: $path, base: $base_entry, head: $head_entry} ]
')" || { echo "FATAL: could not compare immutable revision trees" >&2; exit 1; }

# A direct loader-shaped profile with a noncanonical stem is still a profile the
# loader may interpret. Refuse before identity filtering rather than let it vanish.
printf '%s' "$TREE_DIFF" | jq -e '
  def valid_profile:
    test("^\\." + "claude/agents/[A-Za-z0-9_-]+\\.md\\z")
    or test("^\\.github/agents/[A-Za-z0-9_-]+\\.agent\\.md\\z")
    or test("^\\.codex/agents/[A-Za-z0-9_-]+\\.toml\\z");
  def loader_shaped_profile:
    test("^\\." + "claude/agents/[^/]+\\.md\\z")
    or test("^\\.github/agents/[^/]+\\.agent\\.md\\z")
    or test("^\\.codex/agents/[^/]+\\.toml\\z");
  all(.[]; (.path | (loader_shaped_profile | not) or valid_profile))
' >/dev/null || {
  echo "FATAL: a changed reviewer profile path has no canonical identity stem" >&2
  exit 1
}

printf '%s' "$TREE_DIFF" | jq -e '
  all(.[]; (.path | test("(^|/)skills/review-pr/SKILL\\.md\\z") | not))
' >/dev/null || {
  echo "FATAL: this PR modifies the review skill making this decision, so the guard below would be the PR's own code clearing the PR. Review these changes by hand." >&2
  exit 1
}

# CLOSED BY THE MERGE BASE: profiles are recognised only by the three loader
# dialects, and changed identities come only from their immutable tree entries.
PROFILE_ROWS="$(printf '%s' "$TREE_DIFF" | jq -r '
  def profile:
    if (.path | test("^\\." + "claude/agents/[A-Za-z0-9_-]+\\.md\\z"))
    then {dialect: "yaml", stem: (.path | sub("^\\." + "claude/agents/"; "") | sub("\\.md\\z"; ""))}
    elif (.path | test("^\\.github/agents/[A-Za-z0-9_-]+\\.agent\\.md\\z"))
    then {dialect: "yaml", stem: (.path | sub("^\\.github/agents/"; "") | sub("\\.agent\\.md\\z"; ""))}
    elif (.path | test("^\\.codex/agents/[A-Za-z0-9_-]+\\.toml\\z"))
    then {dialect: "toml", stem: (.path | sub("^\\.codex/agents/"; "") | sub("\\.toml\\z"; ""))}
    else null end;
  .[] | . as $change | profile as $profile | select($profile != null)
  | [ ["merge-base", $change.base], ["head", $change.head] ][]
  | select(.[1] != null)
  | [$change.path, $profile.dialect, $profile.stem, .[0], .[1].type, .[1].mode, .[1].sha] | @tsv
')" || { echo "FATAL: could not identify changed reviewer profiles from immutable trees" >&2; exit 1; }

CHANGED_AGENT_DEFS=
TAB="$(printf '\t')"
while IFS="$TAB" read -r profile_path profile_dialect profile_stem profile_revision profile_type profile_mode profile_blob_sha; do
  [ -n "$profile_path" ] || continue
  [ "$profile_type" = "blob" ] && [ "$profile_mode" = "100644" ] || {
    echo "FATAL: changed reviewer profile ${profile_path} at ${profile_revision} is not a regular blob" >&2
    exit 1
  }
  profile_blob="$(gh api "repos/{owner}/{repo}/git/blobs/${profile_blob_sha}")" \
    || { echo "FATAL: could not read immutable reviewer profile ${profile_path} at ${profile_revision}" >&2; exit 1; }
  printf '%s' "$profile_blob" | jq -e '
    type == "object" and .encoding == "base64" and (.content | type == "string")
  ' >/dev/null || {
    echo "FATAL: could not read the declared name of ${profile_path} at ${profile_revision}" >&2
    exit 1
  }
  if [ "$profile_dialect" = toml ]; then
    # Deliberately not preapproved: either stdin or -c would grant arbitrary Python
    # execution to the checkout-controlled skill before this guard can attest it.
    printf '%s' "$profile_blob" | python3 -c '
import base64
import json
import sys
import tomllib

try:
    payload = json.load(sys.stdin)
    raw = base64.b64decode("".join(payload["content"].split()), validate=True)
    parsed = tomllib.loads(raw.decode("utf-8"))
except (KeyError, TypeError, ValueError, UnicodeDecodeError):
    raise SystemExit(1)

stem = sys.argv[1]
lines = raw.splitlines()
expected = ("name = \"" + stem + "\"").encode()
raise SystemExit(0 if lines and lines[0] == expected and parsed.get("name") == stem else 1)
' "$profile_stem" >/dev/null || {
      echo "FATAL: immutable reviewer profile ${profile_path} has no canonical identity matching ${profile_stem}" >&2
      exit 1
    }
  else
    printf '%s' "$profile_blob" | jq -e --arg stem "$profile_stem" --arg path "$profile_path" '
      def canonical_yaml:
        split("\n") as $lines
        | ($lines[0] == "---")
        | if . then
            ($lines[1:] | to_entries | map(select(.value == "---") | .key) | .[0]) as $end
            | ($lines[1:($end + 1)]) as $frontmatter
            | ($end != null
                and ($frontmatter | join("\n")
                  | if ($path | test("^\\.github/agents/")) then
                      test("^name: " + $stem + "(?:\\n(?:description: >-(?:\\n  [^\\n]+)+|tools: \\[(?:read|search|edit|execute)(?:, (?:read|search|edit|execute))*\\]|user-invocable: true))+$")
                    else
                      test("^name: " + $stem + "(?:\\n(?:description: [A-Za-z0-9][^\\n]*|tools: [A-Za-z][^\\n]*|model: [A-Za-z0-9_-]+|maxTurns: [0-9]+|effort: [A-Za-z0-9_-]+|color: [A-Za-z0-9_-]+|isolation: [A-Za-z0-9_-]+|skills:(?:\\n  - [A-Za-z0-9][A-Za-z0-9:_-]*)+))+$")
                    end)
                and ($frontmatter
                  | map(select(test("^[ \\t]*(name|\\\"name\\\"|\\x27name\\x27)[ \\t]*:")))
                  == ["name: " + $stem]))
          else false end;
      (.content | gsub("\n"; "") | @base64d | canonical_yaml)
    ' >/dev/null || {
      echo "FATAL: immutable reviewer profile ${profile_path} has no canonical identity matching ${profile_stem}" >&2
      exit 1
    }
  fi
  CHANGED_AGENT_DEFS="${CHANGED_AGENT_DEFS}${CHANGED_AGENT_DEFS:+
}${profile_stem}"
done <<< "$PROFILE_ROWS"

for agent in $MANDATORY_BASE $CONFIGURED $PATTERN_AGENTS; do
  case " $DISPATCH_AGENTS $SKIPPED_AGENTS " in *" $agent "*) continue ;; esac
  if printf '%s\n' "$CHANGED_AGENT_DEFS" | grep -qxF -e "$agent"; then
    SKIPPED_AGENTS="${SKIPPED_AGENTS:+$SKIPPED_AGENTS }$agent"
  else
    DISPATCH_AGENTS="${DISPATCH_AGENTS:+$DISPATCH_AGENTS }$agent"
  fi
done
# The floor check reads the default-branch merge base, but the harness loads the HEAD tree. A PR
# whose (author-chosen) base already lacks a profile shows no TREE_DIFF entry for it, so the agent
# would be "dispatched" with nothing to load. Bind every dispatch to a regular file at HEAD.
for agent in $DISPATCH_AGENTS; do
  printf '%s' "$HEAD_TREE" | jq -e --arg p ".cl""aude/agents/${agent}.md" 'any(.tree[]; .path == $p and .type == "blob" and .mode == "100644")' >/dev/null \
    || { echo "FATAL: agent '$agent' is to be dispatched but the reviewed revision has no regular profile for it" >&2; exit 1; }
done

# The two remote reads agreeing says nothing about the tree that will actually be READ.
# Agent definitions are loaded by the harness from the LOCAL checkout; they are never
# fetched. So if the reviewer has commit A checked out with a poisoned `security-reviewer`
# and the author force-pushes B reverting it, both remote reads stably return B, every
# check above passes, and A is what loads. Remote-vs-local is the same split that has
# defeated this guard before; bind the decision to the tree it governs.
PHYSICAL_CWD="$(pwd -P)" \
  || { echo "FATAL: could not determine the physical working directory for reviewer profile verification" >&2; exit 1; }
GIT_DIR="$(git rev-parse --absolute-git-dir)" \
  || { echo "FATAL: could not locate the local Git directory for reviewer profile verification" >&2; exit 1; }
GIT_DIR="$(cd "$GIT_DIR" && pwd -P)" \
  || { echo "FATAL: could not canonicalize the local Git directory for reviewer profile verification" >&2; exit 1; }
GIT_IS_BARE="$(git rev-parse --is-bare-repository)" \
  || { echo "FATAL: could not determine whether the local Git directory is bare" >&2; exit 1; }
[ "$GIT_IS_BARE" = false ] || {
  echo "FATAL: a bare local Git directory cannot attest working-tree reviewer profiles" >&2
  exit 1
}
[ -d "$GIT_DIR" ] && [ ! -L "$GIT_DIR" ] || {
  echo "FATAL: local Git directory is not a physical directory; cannot attest reviewer profiles" >&2
  exit 1
}
case "$GIT_DIR" in
  */.git) REPO_ROOT="${GIT_DIR%/.git}" ;;
  *)
    GITDIR_BACKPOINTER="$GIT_DIR/gitdir"
    [ -f "$GITDIR_BACKPOINTER" ] && [ ! -L "$GITDIR_BACKPOINTER" ] || {
      echo "FATAL: local linked-worktree Git directory has no safe gitdir backpointer" >&2
      exit 1
    }
    GITDIR_BACKPOINTER_LINES=()
    gitdir_backpointer_line=
    while IFS= read -r gitdir_backpointer_line || [ -n "$gitdir_backpointer_line" ]; do
      GITDIR_BACKPOINTER_LINES[${#GITDIR_BACKPOINTER_LINES[@]}]="$gitdir_backpointer_line"
    done < "$GITDIR_BACKPOINTER"
    [ "${#GITDIR_BACKPOINTER_LINES[@]}" = 1 ] || {
      echo "FATAL: local linked-worktree gitdir backpointer is malformed" >&2
      exit 1
    }
    WORKTREE_DOTGIT_RAW="${GITDIR_BACKPOINTER_LINES[0]}"
    case "$WORKTREE_DOTGIT_RAW" in /*|[A-Za-z]:/*) ;;
      *)
        echo "FATAL: local linked-worktree gitdir backpointer is malformed" >&2
        exit 1 ;;
    esac
    case "$WORKTREE_DOTGIT_RAW" in */.git) ;;
      *)
        echo "FATAL: local linked-worktree gitdir backpointer is malformed" >&2
        exit 1 ;;
    esac
    WORKTREE_ROOT_RAW="${WORKTREE_DOTGIT_RAW%/.git}"
    case "$WORKTREE_ROOT_RAW" in '') WORKTREE_ROOT_RAW=/ ;; [A-Za-z]:) WORKTREE_ROOT_RAW="$WORKTREE_ROOT_RAW/" ;; esac
    REPO_ROOT="$(cd "$WORKTREE_ROOT_RAW" && pwd -P)" \
      || { echo "FATAL: could not canonicalize the local linked-worktree root" >&2; exit 1; }
    WORKTREE_DOTGIT="$REPO_ROOT/.git"
    WORKTREE_DOTGIT_LINES=()
    worktree_dotgit_line=
    [ -f "$WORKTREE_DOTGIT" ] && [ ! -L "$WORKTREE_DOTGIT" ] || {
      echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
      exit 1
    }
    while IFS= read -r worktree_dotgit_line || [ -n "$worktree_dotgit_line" ]; do
      WORKTREE_DOTGIT_LINES[${#WORKTREE_DOTGIT_LINES[@]}]="$worktree_dotgit_line"
    done < "$WORKTREE_DOTGIT"
    [ "${#WORKTREE_DOTGIT_LINES[@]}" = 1 ] || {
      echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
      exit 1
    }
    case "${WORKTREE_DOTGIT_LINES[0]}" in
      'gitdir: '*) RECIPROCAL_GIT_DIR_RAW="${WORKTREE_DOTGIT_LINES[0]#gitdir: }" ;;
      *)
        echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
        exit 1 ;;
    esac
    case "$RECIPROCAL_GIT_DIR_RAW" in /*|[A-Za-z]:/*) ;;
      *)
        echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
        exit 1 ;;
    esac
    RECIPROCAL_GIT_DIR="$(cd "$RECIPROCAL_GIT_DIR_RAW" && pwd -P)" || {
      echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
      exit 1
    }
    [ "$RECIPROCAL_GIT_DIR" = "$GIT_DIR" ] || {
      echo "FATAL: local linked-worktree gitdir reciprocal is malformed" >&2
      exit 1
    } ;;
esac
[ -d "$REPO_ROOT" ] && [ ! -L "$REPO_ROOT" ] || {
  echo "FATAL: derived checkout root is not a physical directory; cannot attest reviewer profiles" >&2
  exit 1
}
[ -n "${REVIEW_PR_LOADER_ROOT+x}" ] && [ -n "$REVIEW_PR_LOADER_ROOT" ] || {
  echo "FATAL: harness loader root was not supplied; cannot bind local reviewer profiles" >&2
  exit 1
}
LOADER_ROOT_RAW="$REVIEW_PR_LOADER_ROOT"
case "$LOADER_ROOT_RAW" in
  file://*)
    LOADER_ROOT_RAW="$(python3 -c '
import sys
from urllib.parse import unquote_to_bytes, urlsplit

uri = urlsplit(sys.argv[1])
if uri.scheme.lower() != "file" or uri.netloc not in ("", "localhost") or uri.query or uri.fragment:
    raise SystemExit(1)
path = unquote_to_bytes(uri.path).decode("utf-8")
if any(char in path for char in "\0\r\n"):
    raise SystemExit(1)
if len(path) >= 3 and path[0] == "/" and path[1].isalpha() and path[2] == ":":
    path = path[1:]
print(path, end="")
' "$LOADER_ROOT_RAW")" || {
      echo "FATAL: harness loader root URI is malformed" >&2
      exit 1
    } ;;
esac
case "$LOADER_ROOT_RAW" in /*|[A-Za-z]:/*) ;;
  *) echo "FATAL: harness loader root must be absolute" >&2; exit 1 ;;
esac
[ -d "$LOADER_ROOT_RAW" ] && [ ! -L "$LOADER_ROOT_RAW" ] || {
  echo "FATAL: harness loader root is not a physical directory" >&2
  exit 1
}
LOADER_ROOT="$(cd "$LOADER_ROOT_RAW" && pwd -P)" \
  || { echo "FATAL: could not canonicalize harness loader root" >&2; exit 1; }
case "$LOADER_ROOT_RAW" in
  [A-Za-z]:/*) ;;
  *) [ "$LOADER_ROOT" = "$LOADER_ROOT_RAW" ] || {
    echo "FATAL: harness loader root must already be a canonical physical path" >&2
    exit 1
  } ;;
esac
[ "$REPO_ROOT" = "$LOADER_ROOT" ] || {
  echo "FATAL: Git root differs from the harness loader root; nested repositories cannot attest reviewer profiles (Git: $REPO_ROOT; loader: $LOADER_ROOT)" >&2
  exit 1
}
case "$PHYSICAL_CWD" in "$LOADER_ROOT"|"$LOADER_ROOT"/*) ;;
  *)
    echo "FATAL: working directory is outside the harness loader root" >&2
    exit 1 ;;
esac
LOCAL_HEAD="$(git -C "$REPO_ROOT" rev-parse HEAD)" \
  || { echo "FATAL: could not read the local checkout revision" >&2; exit 1; }
[ "$LOCAL_HEAD" = "$HEAD_SHA" ] || {
  echo "FATAL: the checkout ($LOCAL_HEAD) is not the reviewed revision ($HEAD_SHA). The harness loads agent definitions from this working tree, so a trust decision computed from the PR describes a different tree. Check out the PR and re-run." >&2
  exit 1
}
# The loader reads working-tree bytes, not Git's index.  Compare its exact direct-file
# inventory and each file's Git blob hash with the immutable HEAD tree, then reject
# index flags and staged changes as well.  `diff` alone misses ignored, untracked, and
# assume-unchanged files; the local bytes are the decision here.
printf '%s' "$HEAD_TREE" | jq -e '
  def valid_profile:
    test("^\\." + "claude/agents/[A-Za-z0-9_-]+\\.md\\z")
    or test("^\\.github/agents/[A-Za-z0-9_-]+\\.agent\\.md\\z")
    or test("^\\.codex/agents/[A-Za-z0-9_-]+\\.toml\\z");
  def loader_shaped_profile:
    test("^\\." + "claude/agents/[^/]+\\.md\\z")
    or test("^\\.github/agents/[^/]+\\.agent\\.md\\z")
    or test("^\\.codex/agents/[^/]+\\.toml\\z");
  all(.tree[]; (.path | (loader_shaped_profile | not) or valid_profile))
  and all(.tree[]; (.path | valid_profile | not) or (.type == "blob" and .mode == "100644"))
' >/dev/null || {
  echo "FATAL: the reviewed revision has a malformed reviewer profile inventory" >&2
  exit 1
}
HEAD_PROFILE_ROWS="$(printf '%s' "$HEAD_TREE" | jq -r '
  def valid_profile:
    test("^\\." + "claude/agents/[A-Za-z0-9_-]+\\.md\\z")
    or test("^\\.github/agents/[A-Za-z0-9_-]+\\.agent\\.md\\z")
    or test("^\\.codex/agents/[A-Za-z0-9_-]+\\.toml\\z");
  [.tree[] | select(.path | valid_profile) | [.path, .sha] | @tsv] | sort[]
')" || {
  echo "FATAL: could not enumerate immutable reviewer profiles" >&2
  exit 1
}
git -C "$REPO_ROOT" diff --cached --quiet "$HEAD_SHA" -- .cl""aude/agents .github/agents .codex/agents || {
  echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
  exit 1
}
TAB="$(printf '\t')"
while IFS="$TAB" read -r profile_path profile_blob_sha; do
  [ -n "$profile_path" ] || continue
  profile_file="$REPO_ROOT/$profile_path"
  [ -f "$profile_file" ] && [ ! -L "$profile_file" ] || {
    echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
    exit 1
  }
  local_blob_sha="$(git -C "$REPO_ROOT" hash-object --no-filters -- "$profile_file")" \
    || { echo "FATAL: could not hash a local reviewer profile" >&2; exit 1; }
  if [ "$local_blob_sha" != "$profile_blob_sha" ]; then
    local_blob_sha="$(
      set -o pipefail
      python3 - "$profile_file" <<'PY' | git -C "$REPO_ROOT" hash-object --stdin
import sys

with open(sys.argv[1], "rb") as profile:
    sys.stdout.buffer.write(profile.read().replace(b"\r\n", b"\n"))
PY
    )" || { echo "FATAL: could not normalize a local reviewer profile" >&2; exit 1; }
    [ "$local_blob_sha" = "$profile_blob_sha" ] || {
      echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
      exit 1
    }
  fi
  index_entry="$(git -C "$REPO_ROOT" ls-files -v -- "$profile_path")" \
    || { echo "FATAL: could not inspect local reviewer profile flags" >&2; exit 1; }
  [ "$index_entry" = "H $profile_path" ] || {
    echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
    exit 1
  }
done <<< "$HEAD_PROFILE_ROWS"
set -o pipefail
for profile_dir in "$REPO_ROOT/.cl""aude/agents" "$REPO_ROOT/.github/agents" "$REPO_ROOT/.codex/agents"; do
  [ -d "$profile_dir" ] && [ ! -L "$profile_dir" ] \
    && [ "$(cd "$profile_dir" && pwd -P)" = "$profile_dir" ] || {
    echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
    exit 1
  }
  find "$profile_dir" -mindepth 1 -maxdepth 1 -print0 | while IFS= read -r -d '' profile_file; do
    profile_path="${profile_file#"$REPO_ROOT/"}"
    profile_stem=
    case "$profile_path" in
      .cl""aude/agents/*.md) profile_stem="${profile_path#.cl""aude/agents/}"; profile_stem="${profile_stem%.md}" ;;
      .github/agents/*.agent.md) profile_stem="${profile_path#.github/agents/}"; profile_stem="${profile_stem%.agent.md}" ;;
      .codex/agents/*.toml) profile_stem="${profile_path#.codex/agents/}"; profile_stem="${profile_stem%.toml}" ;;
      *) continue ;;
    esac
    case "$profile_stem" in ''|*[!A-Za-z0-9_-]*)
      echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
      exit 1 ;;
    esac
    [ -f "$profile_file" ] && [ ! -L "$profile_file" ] || {
      echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
      exit 1
    }
    grep -F -q "${profile_path}${TAB}" <<< "$HEAD_PROFILE_ROWS" || {
      echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
      exit 1
    }
  done || exit 1
done || {
  echo "FATAL: local reviewer profiles do not match the reviewed revision" >&2
  exit 1
}

# Nothing moved while we looked, or nothing is dispatched. A push landing mid-review
# means the signals above describe a revision the reviewers will not read, and a trust
# decision computed against the wrong revision is worse than no review at all.
PR_REVS_NOW="$(gh api "repos/{owner}/{repo}/pulls/$0" --jq '.head.sha + " " + .base.sha')" \
  || { echo "FATAL: could not revalidate the PR revisions" >&2; exit 1; }
HEAD_SHA_NOW="${PR_REVS_NOW%% *}"; BASE_SHA_NOW="${PR_REVS_NOW##* }"
[ "$HEAD_SHA_NOW" = "$HEAD_SHA" ] || {
  echo "FATAL: the PR moved during review ($HEAD_SHA -> $HEAD_SHA_NOW). Signals and trust decisions describe a revision that is no longer current; re-run the review." >&2
  exit 1
}
# Head alone is not the whole revision. A RETARGET moves `base.sha` while `head.sha`
# holds, and `MERGE_BASE_TREE` -- the entire trusted (directory, suffix) set every
# decision above is measured against -- was derived from the old revision pair.
# Validating only the head leaves the merge-base decision tied to a different target.
[ "$BASE_SHA_NOW" = "$BASE_SHA" ] || {
  echo "FATAL: the PR was retargeted during review ($BASE_SHA -> $BASE_SHA_NOW). The trusted definition set was derived from a revision pair that is no longer this PR's; re-run the review." >&2
  exit 1
}

if [ -n "$SKIPPED_AGENTS" ]; then
  # LOUD, and it must reach the report. A skipped reviewer is dropped coverage, and a
  # PR that modifies a reviewer to disable it would otherwise look identical to a clean
  # review. Whoever reads the review needs to see which lens did not run and why.
  echo "REVIEWER-DEFINITION-MODIFIED: $SKIPPED_AGENTS" >&2
  echo "  This PR edits these agents' own definitions, so their profiles are" >&2
  echo "  contributor-controlled for this review and they were NOT dispatched." >&2
  echo "  Review those diffs by hand, and re-run after they merge." >&2
fi
echo "DISPATCH_AGENTS=$DISPATCH_AGENTS"
echo "SKIPPED_AGENTS=$SKIPPED_AGENTS"
```

Dispatch every distinct agent named in **`DISPATCH_AGENTS`** in parallel — that list is
already the union of the base and pattern agents, minus any whose definition this PR
modifies. Never dispatch from `PATTERN_AGENTS` or `MANDATORY_BASE` directly; doing so
re-opens the hole the block above closes. **Report every name in `SKIPPED_AGENTS`
prominently in the review output**, with the reason — dropped coverage that is not
stated reads as coverage. No cleanup step is required — nothing was written to disk.

When `.review_agents` is absent from the repo's config, only the three base defaults above are dispatched — no project-specific agents are named here.

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
