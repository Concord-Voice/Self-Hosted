---
name: reproduce-then-fix
description: Test-first bug fixing — write a failing reproduction test before touching any production code, then iterate hypothesis → fix → green inside a bounded retry budget. Use for bug reports tracked in GitHub issues. Requires `$0` to be a numeric issue number; if no issue exists yet, file one via `/issue-creation` first.
argument-hint: [issue-number]
allowed-tools:
  - Bash(go test *)
  - Bash(go vet *)
  - Bash(golangci-lint *)
  - Bash(npm test *)
  - Bash(npm run *)
  - Bash(git diff *)
  - Bash(git log *)
  - Bash(git add *)
  - Bash(git commit *)
  - Bash(git status *)
  - Bash(git branch *)
  - Bash(git checkout *)
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Read
  - Grep
  - Glob
  - Edit
  - Write
---

<DISCIPLINE-GATE>
This skill encodes a non-negotiable ordering: **failing test exists and is committed BEFORE any production-code edit is proposed**. The ordering is what makes the bug fix verifiable. If you skip Phase 1 because "the bug is obvious," the resulting PR has no regression test and the bug can reappear silently. Do not skip phases or merge them; each gate is load-bearing.
</DISCIPLINE-GATE>

# Reproduce-Then-Fix

A bounded, test-first debugging workflow for issue `$0` — a numeric GitHub issue number (validated in Step 1.1).

---

## Phase 1: REPRODUCE — write the failing test FIRST

### Step 1.0 — Get onto the issue branch (before any edit or commit)

`/dev-lifecycle` routes bugs here before its own branch step, so this skill selects the branch itself. Never commit onto the default branch.

```bash
BRANCH=$(git branch --list "feature/$0-*" --format='%(refname:short)' | head -1)
[ -z "$BRANCH" ] && BRANCH=$(git branch -r --list "origin/feature/$0-*" --format='%(refname:short)' | head -1)
if [ -n "$BRANCH" ]; then
  git checkout "${BRANCH#origin/}" 2>/dev/null || git checkout --track "$BRANCH"
else
  # Topic is 2-4 significant words from the issue title, lowercased, hyphenated.
  git checkout -b feature/$0-<topic>
fi
```

### Step 1.1 — Gather the symptom

Validate that `$0` is a numeric issue number first. If `$0` is not numeric (e.g., free-text description), refuse to proceed and instruct the developer to file an issue via `/issue-creation`, then re-invoke `/reproduce-then-fix <new-issue-number>`. The downstream commit messages and PR body templates reference `#$0` and `Closes #$0`, which require a numeric value.

Once validated, pull the issue brief:

```bash
gh issue view $0 --json title,body,labels,comments --jq '{title: .title, body: .body, labels: [.labels[].name], comments: [.comments[] | {author: .author.login, body: .body}]}'
```

If the issue body is sparse on reproduction detail, ask the developer to paste:
- The user-facing symptom (what they did, what they expected, what happened)
- Any logs, stack traces, or screenshots
- The narrowest reproducible recipe they know

### Step 1.2 — Locate the relevant test module

Use Glob/Grep to find the existing test file that should cover the misbehaving code:

- Go: `<pkg>/internal/.../*_test.go` or sibling `*_test.go` files in the affected package
- TypeScript unit: `<pkg>/tests/unit/**/*.test.ts(x)` or `src/**/*.test.ts(x)`
- TypeScript E2E: `<pkg>/tests/e2e/**/*.spec.ts` or `tests/e2e/**/*.spec.ts`

If no test file exists for the affected code path, create one — but **name it after the bug being reproduced**, not the file being tested (e.g., `dm_pin_revoked_key_test.go` rather than `dm_handler_test.go`). This makes the regression test discoverable later.

### Step 1.3 — Write the MINIMAL failing test

Constraints on the repro test:
- **Smallest unit that captures the bug.** If the symptom is a 500 from `POST /api/v1/dm/pin`, a unit test on the handler beats an e2e test through the SPA — same signal, 100× faster.
- **Asserts on observable behavior**, not implementation internals. The test must still fail after a "clean fix" — it's not validating the buggy code path, it's validating the contract the user expected.
- **Deterministic.** No `time.Sleep`, no random fixtures, no real network. Follow any `.claude/rules/*.md` test conventions the repo provides.
- **Names the issue/PR** in the test name or a single-line comment: `TestDMPin_KeyRevoked_Returns410` plus `// regression for #NNN` is the canonical form.

### Step 1.4 — Confirm "fails for the right reason"

Run the test:

```bash
# Go
go test -race -run '<test name>' ./<package>/...

# TypeScript (Vitest or Jest)
npm test -- --run <test path>
```

The output must show:
1. The test **fails**, not errors. An error during setup (missing fixture, undefined function) does NOT count as reproducing the bug.
2. The failure assertion matches the symptom. If the bug is "returns 500 instead of 410" and the test fails because "expected 410, got 200", that is the **wrong** failure — the bug is not actually being exercised.

Paste the failure output in chat and ask the developer:

```
Repro test fails as follows:
<paste output>

Does this match the bug you're seeing? [yes / no — wrong failure mode]
```

**Do NOT proceed to Phase 2 until the developer confirms.** A wrong-failure-mode test is worse than no test — it gives false confidence.

### Step 1.5 — Commit the repro

Once confirmed:

```bash
git add <test file>
git commit -m "test: reproduce #$0 — <one-line bug description>"
```

This commit is the regression test that future contributors will rely on. Keep it free of fix code.

---

## Phase 2: FIX LOOP — bounded iteration

Maximum **5 hypothesis-test iterations** before escalating. Track iterations explicitly.

### Iteration template

For iteration N (1 ≤ N ≤ 5):

**Hypothesis** — one sentence stating what you believe is wrong and why. Cite the file:line you suspect.

**Smallest change** — make the narrowest edit that would falsify or confirm the hypothesis. No refactors, no adjacent cleanup, no "while I'm here." If the hypothesis is wrong, you want to revert one diff, not unpick a sprawl.

**Run the repro test** — same command as Phase 1.4.

**If red** — record the new failure mode in a hypothesis table, revert the change (`git checkout -- <file>`), and form a new hypothesis. Go to iteration N+1.

**If green** — run the full affected test module:

```bash
# Go
go test -race ./<package>/...

# TypeScript
npm test -- --run <module>
```

If the module is green, run the **entire test suite** to check for regressions:

```bash
# Go
go test -race ./...

# TypeScript
npm test
```

If the full suite is green → proceed to Phase 3.
If the full suite breaks something else → record in hypothesis table, revert, iterate.

### Iteration budget exhausted (N = 5)

If 5 iterations have not produced a green-on-green state, **stop** and produce a written hypothesis-evidence table:

```
Reproduce-then-fix budget exhausted for #$0.

| # | Hypothesis | Change attempted | Test result | Why ruled out |
|---|-----------|-----------------|-------------|---------------|
| 1 | ... | ... | red | ... |
| 2 | ... | ... | red on different test | ... |
| ... | | | | |

Recommendation: <pair-with-developer | broaden test scope | check upstream dependency>
```

Hand back to the developer. Do NOT spend a 6th iteration without explicit re-approval — at iteration 5+ the marginal information per attempt drops sharply and the right move is usually a fresh pair of eyes.

---

## Phase 3: SHIP — fix commit + PR

### Step 3.1 — Stage and commit the fix

```bash
git add <production files>
git commit -m "fix: <one-line description> (#$0)"
```

The fix commit must NOT contain test file edits — the repro test is already committed in Phase 1.5. If the fix forced a test-file change (e.g., a brittle assertion that should have been more flexible), commit that separately as `test: <description>` after the fix.

### Step 3.2 — Open the PR

```bash
gh pr create --draft --title "fix: <description> (#$0)" --body-file <path>
```

The PR body MUST link the repro test commit:

```markdown
## Repro
- Failing test added in <test-commit-sha>: `<test name>`
- Verified failure mode matches issue symptom (see commit message)

## Fix
- <one-paragraph explanation of what was wrong and what changed>
- Hypothesis iterations: <N>/5

Closes #$0
```

The link from fix → repro commit is what makes this PR auditable. Don't squash-merge until the linkage is preserved (or re-stated in the squash commit message).

### Step 3.3 — Hand back to `/dev-lifecycle`

Reproduce-then-fix ends at the draft PR. Subsequent phases (CI monitor, ready-for-review, enhanced-pr-review, merge) are owned by `/dev-lifecycle`. Invoke:

```
/dev-lifecycle $0
```

It will detect Phase 6 (CI MONITOR) from the existing branch + draft PR.

---

## What This Skill Does NOT Do

- Does NOT edit production code before a failing test exists and is committed
- Does NOT proceed past Phase 1.4 without developer confirmation of the failure mode
- Does NOT continue past iteration 5 without escalation
- Does NOT merge the repro-test commit and fix commit into one — they are separately auditable
- Does NOT handle features, refactors, or "while I'm here" cleanup — scope is strictly the bug under `$0`
- Does NOT skip the full-suite regression check after the repro test turns green

---

## Why This Skill Exists

Bug fixes without a failing-test-first discipline have two failure modes:

1. **Fixed the wrong thing** — the developer's mental model of the symptom was incomplete; the "fix" addresses an adjacent issue and the original bug recurs.
2. **No regression guard** — without a committed reproduction test, the bug can be re-introduced silently by an unrelated refactor weeks later.

Both modes have happened in this codebase (the recent CORS preflight and certbot renewal sessions referenced in the design rationale would have been caught earlier by a test-first loop). The bounded 5-iteration budget exists because debugging cycles past iteration 5 usually indicate the hypothesis space is too large for solo work — that's the signal to bring in another pair of eyes, not to grind.

The skill's text-level discipline is the enforcement mechanism. There is intentionally no PreToolUse hook gating production edits on test additions — such a hook would also block feature work, refactors, and migration edits. Discipline lives in the skill, not in the harness.
