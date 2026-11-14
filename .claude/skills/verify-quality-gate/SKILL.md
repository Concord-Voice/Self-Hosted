---
name: verify-quality-gate
description: One-command pre-PR quality gate. Runs the repo's configured lint, type-check, and test commands, optionally reads a code-quality server's gate verdict, then prints a single pass/fail verdict. Use before marking a PR ready for review.
argument-hint: "[--skip-tests] [--fast]"
allowed-tools:
  - Bash(git status *)
  - Bash(git diff *)
  - Bash(git branch *)
  - Read
  - Grep
  - Glob
  - mcp__sonarqube__get_project_quality_gate_status
---

# Verify Quality Gate

Run this repo's pre-PR quality gate and report a single verdict.

Every command this skill runs comes from `.claude/repo-config.json`, not from a
hardcoded list — it has no knowledge of your service directories or toolchain
until it reads them. With no configuration it degrades to a git-state check plus
a clear statement of what it could not verify; it never errors, and it never
guesses at a command.

> **Why the check commands are not in `allowed-tools`.** The list above
> pre-approves only read-only git inspection and the code-quality MCP calls. The
> configured lint/type-check/test commands are deliberately absent: they are
> arbitrary strings from a config file, and blanket-approving arbitrary `Bash`
> is not a trade this skill should make on the operator's behalf. Each configured
> command prompts for approval on first use. That is the intended behavior, not a
> misconfiguration — approve them, or add the specific commands your repo uses to
> your own settings' permission allowlist.

## Model-invocation authorization

This skill IS model-invocable, but the model should invoke it ONLY when (a) a human asked
for it, or (b) it is executing `/dev-lifecycle` Phase 4 (VALIDATE). Do NOT auto-run the
quality gate in unrelated contexts — it is a deliberate, somewhat-expensive pre-PR check,
not a passive read. This is the prose guard for a bash-runner skill (no agent-dispatch
alternative exists); see `.claude/rules/skill-authoring.md` (installed by
`install.sh claude`) for why payload nature determines the guard.

Args: $0 — optional flags
  - `--skip-tests` — skip the test runs (lint + type-check + gate verdict only)
  - `--fast` — prefer each check's `fast_cmd` where one is declared (typically
    dropping race detection and coverage instrumentation)

## Step 0: Resolve the gate configuration

Read every knob once, up front. Absent file or absent keys fall back to the
documented defaults, so this step never errors.

The gate's commands come from config, so read it from the default branch, never from this
checkout: on a PR branch both `scripts/repo-config.sh` and `.claude/repo-config.json` are the
PR's to edit. A gate change takes effect once it merges.

```bash
RC_REF="refs/remotes/origin/$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null)"   # the API is the authority; origin/HEAD goes stale after a rename
[ "$RC_REF" != refs/remotes/origin/ ] || RC_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"
case "$RC_REF" in refs/remotes/origin/HEAD) RC_REF= ;; refs/remotes/origin/?*) ;; *) RC_REF= ;; esac
RC_OID="$([ -n "$RC_REF" ] && git show-ref --verify --hash "$RC_REF" 2>/dev/null)"   # exact refname, no DWIM: a same-named branch or tag cannot stand in
case "$([ -n "$RC_OID" ] && git ls-tree "$RC_OID" -- scripts/repo-config.sh | cut -d' ' -f1)" in
  100644|100755) ;;
  *) echo "FATAL: ${RC_REF:-the default branch} is not fetched or has no regular scripts/repo-config.sh" >&2; return 1 2>/dev/null || exit 1 ;;
esac
RC_SRC="$(git show "$RC_OID:scripts/repo-config.sh")" || { echo "FATAL: could not read the default-branch helper" >&2; return 1 2>/dev/null || exit 1; }
eval "$RC_SRC"   # the commit that was checked; the checkout's copy is never sourced
unset RC_TRUSTED_STATE RC_TRUSTED_CFG   # nothing carries over from an earlier run in this shell
# Presence comes from the tree, not from a read: a blob that is listed but cannot be read is an
# error, never "absent". RC_TRUSTED_STATE: absent = no config on the default branch; present = read.
RC_ENT="$(git ls-tree "$RC_OID" -- .claude/repo-config.json)" || { echo "FATAL: could not list the default-branch tree" >&2; return 1 2>/dev/null || exit 1; }
case "${RC_ENT%% *}" in
  "") RC_TRUSTED_STATE=absent ;;
  100644|100755)
    RC_TRUSTED_CFG="$(git show "$RC_OID:.claude/repo-config.json")" && [ -n "$RC_TRUSTED_CFG" ] \
      || { echo "FATAL: could not read the default-branch .claude/repo-config.json" >&2; return 1 2>/dev/null || exit 1; }
    RC_TRUSTED_STATE=present ;;
  *) echo "FATAL: the default-branch .claude/repo-config.json is not a regular file" >&2; return 1 2>/dev/null || exit 1 ;;
esac
DEFAULT_BRANCH="${RC_REF#refs/remotes/origin/}"
RC_FILE="$(mktemp)"   # rc_get now reads the default branch's config
if [ -n "$RC_TRUSTED_CFG" ]; then printf '%s' "$RC_TRUSTED_CFG" > "$RC_FILE"; else rm -f "$RC_FILE"; fi   # absent config: rc_get's [ -f ] guard returns the defaults
QG_ENABLED="$(rc_get '.quality_gate.enabled' false)"
COVERAGE_MIN="$(rc_get '.quality_gate.coverage_min' 80)"
BLOCKERS_MAX="$(rc_get '.quality_gate.blockers_max' 0)"
HOTSPOTS_MAX="$(rc_get '.quality_gate.hotspots_max' 0)"
QG_DOC="$(rc_get '.quality_gate.doc_path')"        # authoritative gate definition when set
CHECKS="$(rc_get '.quality_gate.checks' '[]')"     # JSON array, run in declared order
QG_SERVER="$(rc_get '.quality_gate.server' 'none')" # 'sonarqube' | 'none'
```

`.quality_gate.checks[]` entries carry:

| Field | Meaning |
|---|---|
| `name` | Label shown in the results table |
| `dir` | Directory to run in, relative to the repo root (default `.`) |
| `cmd` | The command to run |
| `fast_cmd` | Optional alternative used under `--fast` |
| `group` | `lint` \| `typecheck` \| `test` — `--skip-tests` skips the `test` group |

**If `CHECKS` is empty**, do not invent commands and do not scan for a build
system. Report `no checks configured` for the command section, run Steps 1 and 5
only, and say plainly in the verdict that local verification was not performed.
A gate that silently guesses is worse than one that admits it has nothing to run.

**If `QG_ENABLED` is false**, this repo has opted out: run Step 1, print a
one-line note, and stop. `/dev-lifecycle` Phase 4 already treats the gate as
skipped in that case.

## Execution Order

Run checks **sequentially in declared order**, reporting each result as it
completes. Do NOT parallelize. When an early check fails, still run the
remaining ones so the user sees the whole picture in one pass — but the verdict
names the *earliest* blocker, because that is where they should start.

### Step 1: Git state check
- `git status` — is the working tree clean, or does the user know what's dirty?
- `git branch --show-current` — confirm this is a feature branch, not the default branch.

### Step 2: Lint and static analysis
Run every `checks[]` entry with `group == "lint"`, each in its declared `dir`.

### Step 3: Type-check
Run every `checks[]` entry with `group == "typecheck"`.

A note worth carrying into your own config: **per-process or per-target
type-check configs are usually NOT redundant with a single root check.** When a
sub-config restricts its library set (e.g. excluding DOM globals from a
Node-side build), names that resolve fine under the root config fail the real
build. If your CI builds several targets from one source tree, declare a
type-check entry per target rather than one root entry — and be especially
careful with any surface whose build step strips types without checking them,
since nothing else in CI type-checks it.

### Step 4: Tests (unless `--skip-tests`)
Run every `checks[]` entry with `group == "test"`, using `fast_cmd` when
`--fast` was passed and the entry declares one.

### Step 5: Code-quality server gate

Only when `QG_SERVER` names a server AND the matching MCP tools are available in
this session. If either is missing, print `not configured` for this row and do
not treat it as a failure.

For `sonarqube`:
1. Call `mcp__sonarqube__get_project_quality_gate_status`, passing `branch`
   explicitly (the server's branch-vs-pullRequest contract: use `branch` for a
   long- or short-lived branch, `pullRequest` for a PR analysis key, never both).
2. If the gate is `ERROR`, enumerate the failing conditions with metric key,
   threshold, and actual value.
3. If the gate is `WARN` or `OK`, report the conditions closest to their
   thresholds so the user can see where drift is accumulating.

Compare against `COVERAGE_MIN` / `BLOCKERS_MAX` / `HOTSPOTS_MAX`, and defer to
`QG_DOC` when it is set — that document, not this skill, is the authority on
what the gate means.

## Output Format

Print a compact results table, then a single-line verdict:

```
Quality Gate Report — <branch>
──────────────────────────────────────────────────
  ✓ backend lint          (0 issues)
  ✓ backend vet           (clean)
  ✗ frontend lint         (3 errors, 1 warning)
  ✓ type-check            (clean)
  ✓ backend tests         (152 passed)
  ✓ frontend tests        (272 passed)
  ✗ quality gate          (coverage_new_code: 72% < 80%)
──────────────────────────────────────────────────
VERDICT: FAIL — fix the frontend lint errors and raise new-code coverage to ≥80%

First blocker: frontend lint at <path>:<line>
```

Use ✓ and ✗ (nothing else); color is fine if your terminal supports it. Rows are
whatever `checks[]` declared — never a fixed set.

## Verdict Rules

- **PASS**: every configured check green AND the server gate is `OK` (or not configured)
- **WARN**: all checks green but the server gate is `WARN` (print what is close to threshold)
- **FAIL**: any check red — name the first blocker prominently
- **UNVERIFIED**: no checks configured — say so explicitly rather than reporting PASS

## What This Skill Does NOT Do

- It does not open or update a PR — commit and push after this passes
- It does not run whatever your pre-commit framework already runs on commit
- It does not run end-to-end/browser suites — those belong in CI
- It does not fix anything it finds — it reports and stops
- It does not infer commands you have not configured

## Why This Skill Exists

Without it, verifying a PR means running several separate commands across
several directories and reading as many output formats — friction that turns
into "I'll check it in CI", and CI-only discovery burns runner budget and
wall-clock. This collapses the bundle into one invocation with one verdict.
