---
name: reproduce-then-fix
description: Test-first bug fixing — write a failing reproduction test before touching any production code, prove the test is not vacuous, then iterate hypothesis → fix → green inside a bounded retry budget. Runs standalone on a bug issue, or as a per-task sub-skill of /dev-lifecycle Phase 3. Ends at a green fix commit and hands to /dev-lifecycle Phase 4 — it never opens a PR.
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
  - Bash(git checkout *)
  - Bash(git branch *)
  - Bash(gh issue *)
  - Bash(gh pr *)
  - Read
  - Grep
  - Glob
  - Edit
  - Write
---

<DISCIPLINE-GATE>
This skill encodes a non-negotiable ordering: **a failing reproduction test exists, is proven non-vacuous, and is committed BEFORE any production-code edit is proposed**. The ordering is what makes the bug fix verifiable. If you skip Phase 1 because "the bug is obvious," the resulting change has no regression test and the bug can reappear silently. Do not skip phases or merge them; each gate is load-bearing.

The one gate that is NOT negotiable under any autonomy mode is **Phase 1.4** — a repro test that has never been proven capable of failing for the right reason is worse than no test, because it converts an unverified fix into a green one.
</DISCIPLINE-GATE>

# Reproduce-Then-Fix

A bounded, test-first debugging workflow for issue `$0` — a numeric GitHub issue number in standalone mode (validated at the start of Step 1.0). It ends at a **green fix on a branch**, not at a PR.

---

## Entry modes

This skill runs two ways. Establish which one you are in before anything else — it decides what you must gather and where you hand back.

| | **Standalone** | **Sub-skill of `/dev-lifecycle` Phase 3** |
|---|---|---|
| Invoked by | A developer typing `/reproduce-then-fix <N>` | The `/dev-lifecycle` orchestrator, per defect-repair task |
| `$0` | Required, numeric issue number | May be absent — the task context is in the invocation |
| Branch | Select or create `feature/<N>-<topic>` (Step 1.0); never commit onto the default branch | Already exists. Never switch, never create |
| Issue context | Fetch it yourself (Step 1.1) | Inherited — the spec and plan are already in the caller's context. Do NOT re-fetch |
| Autonomy mode | Ask, or default to gating Phase 1.4 | Inherited from the caller's Phase 1.5 contract |
| Scope | The whole bug | **The one task you were given.** Other tasks in the plan are not yours |
| Hands back to | `/dev-lifecycle <N>`, which detects Phase 4 | Returns control to the caller's Phase 3 loop |

In **standalone** mode, run Step 1.0 first: it validates that `$0` is numeric and puts you on the issue branch. (Ad-hoc issueless work is legitimate; it is just not this skill's entry shape. Use `/dev-lifecycle` for that, which does not require an issue.)

### Step 1.0 — Get onto the issue branch (standalone only, before any edit or commit)

`/dev-lifecycle` routes bugs here before its own branch step, so this skill selects the branch itself. Never commit onto the default branch.

Validate that `$0` is a numeric issue number before touching any branch. If `$0` is not numeric (e.g., free-text description), refuse to proceed and instruct the developer to file an issue via `/issue-creation`, then re-invoke `/reproduce-then-fix <new-issue-number>`. The branch name, commit messages and PR body templates reference `#$0` and `Closes #$0`, which require a numeric value. Also stop if `git status --porcelain` shows uncommitted changes, so a checkout cannot carry them onto the wrong branch.

```bash
case "$0" in
  ''|*[!0-9]*) echo "Issue number must be numeric: $0" >&2; exit 1 ;;
esac
[ -z "$(git status --porcelain)" ] || { echo "Uncommitted changes present; commit or stash them first" >&2; exit 1; }
# Same names /dev-lifecycle recognises: the bare feature/<N> and feature/<N>-<topic>.
BRANCH=$(git branch --list "feature/$0" "feature/$0-*" --format='%(refname:short)' | head -1)
[ -z "$BRANCH" ] && BRANCH=$(git branch -r --list "origin/feature/$0" "origin/feature/$0-*" --format='%(refname:short)' | head -1)
if [ -n "$BRANCH" ]; then
  git checkout "${BRANCH#origin/}" 2>/dev/null || git checkout --track "$BRANCH"
else
  # Topic is 2-4 significant words from the issue title, lowercased, hyphenated.
  git checkout -b "feature/$0-<topic>"
fi
```

In **sub-skill** mode you are one task inside someone else's lifecycle. Do not create branches, do not commit anything outside your task's files, and do not open a PR under any circumstances.

### Degrading gracefully

This skill is shared across repos with different agent inventories. **A missing agent and a missing sibling skill degrade differently, and conflating them is how gates get invented or skipped.**

**A missing AGENT → do the work inline.** The dispatch buys isolation, not capability; the discipline is in this file either way.

```
WARN: @test-writer not present in this repo; authoring the repro test inline
```

**A missing SIBLING SKILL → stop and hand back. Never improvise it.** Phase 3 hands to `/dev-lifecycle` Phase 4 precisely *because* this skill does not implement the quality gate, the coverage gate, the docs probe, or the adversarial security pass. In a repo with no `/dev-lifecycle`, those gates have no owner — so report the green fix and the gap, and let the developer decide:

```
WARN: /dev-lifecycle not present in this repo — Phase 4 gates have no owner here.
      Repro committed <sha>, fix committed <sha>, full suite green.
      NOT run: quality gate, coverage gate, docs/count drift, adversarial security pass.
      Run this repo's equivalents before opening a PR.
```

Do **not** synthesize substitute gates, and do **not** treat their absence as permission to skip to a PR. Reporting the gap is the correct outcome; inventing a gate is not.

The discipline below is what matters. The dispatch is how it is best executed where the fleet exists.

---

## Phase 1: REPRODUCE — a failing test FIRST

### Step 1.1 — Gather the symptom

**Standalone only** (sub-skill mode inherits this — skip it):

```bash
gh issue view $0 --json title,body,labels,comments --jq '{title: .title, body: .body, labels: [.labels[].name], comments: [.comments[] | {author: .author.login, body: .body}]}'
```

If the issue body is sparse on reproduction detail, ask the developer for:

- The user-facing symptom — what they did, what they expected, what happened
- Any logs, stack traces, or screenshots
- The narrowest reproducible recipe they know

**Write down the oracle before you write the test.** One sentence: *the observable that distinguishes broken from fixed.* "Returns 410 rather than 500." "The second call reuses the cached key rather than re-deriving it." "`residentLen()` stays at or below `2*capacity` across a promotion cycle." If you cannot state the oracle in one sentence, you do not yet understand the bug well enough to test it, and Step 1.3 will produce something that agrees with your fix instead of checking it.

### Step 1.2 — Locate the relevant test module

Use Glob/Grep to find the existing test file that should cover the misbehaving code:

- Go: sibling `*_test.go` files in the affected package
- TypeScript unit: `**/tests/unit/**/*.test.ts(x)` or `src/**/*.test.ts(x)`
- TypeScript E2E: `**/tests/e2e/**/*.spec.ts`

If no test file covers the affected path, create one — but **name it after the bug being reproduced**, not the file being tested (`dm_pin_revoked_key_test.go`, not `dm_handler_test.go`). That is what makes the regression test discoverable when someone breaks it again in two years.

### Step 1.3 — Author the MINIMAL failing test

**Dispatch `@test-writer`** for this step (isolation-critical per the `/dev-lifecycle` D3 criterion — test authoring is separated from fix authoring so that a fix agent can never quietly relax the test that judges it).

The dispatch prompt MUST carry these four constraints, because `@test-writer`'s ordinary contract is "write tests for code that works" and this one inverts it:

1. **This test must FAIL against the current tree.** That is the deliverable. A passing test is not a success here — it is a finding, and you report it rather than adjusting the test until it fails.
2. **Touch test files only.** No production file, not even to add an export or loosen a visibility modifier. If the code is untestable as written, say so and stop.
3. **The oracle** (from Step 1.1), verbatim.
4. **The failure message must name the property** — not merely differ from expected.

Constraints on the test itself:

- **Smallest unit that captures the bug.** If the symptom is a 500 from `POST /api/v1/dm/pin`, a unit test on the handler beats an E2E test through the SPA — same signal, 100× faster.
- **Asserts on observable behavior**, not implementation internals. It must still fail after a *clean* fix — it is validating the contract the user expected, not the shape of the buggy code path.
- **Deterministic.** No `time.Sleep`, no random fixtures, no real network. Follow any `.claude/rules/*.md` test conventions the repo provides.
- **Names the issue** in the test name or a single-line comment: `TestDMPin_KeyRevoked_Returns410` plus `// regression for #NNN`.

### Step 1.4 — Prove the test is not vacuous ⚠️ GATE

A test that has never been *seen* to fail for the right reason proves nothing. Run it:

```bash
go test -race -run '<test name>' ./<package>/...   # Go
npm test -- --run <test path>                      # Vitest / Jest
```

**(a) It must FAIL, not error.** A setup error — missing fixture, undefined symbol, compile failure — does not reproduce anything. **Read why it went red.** A build error reads exactly like a successful falsification and is not one: a named assertion failing is evidence; a compiler message is not.

**(b) The failure must match the symptom.** If the bug is "returns 500 instead of 410" and the test fails with "expected 410, got 200", that is the *wrong* failure — the bug is not being exercised.

**(c) Walk the vacuity checklist.** A green suite hides at least seven distinct ways a test can assert nothing. These are recorded failures, not hypotheticals — check each:

| # | Mode | The question that catches it |
|---|---|---|
| 1 | **Invalid fixture** — the input is rejected before reaching the code under test | Does my fixture satisfy every validator on the path? Assert that in the test |
| 2 | **Missing dimension** — the assertion ignores the observable the fix changes | List everything the fix alters; confirm the assertion observes each one |
| 3 | **Violating branch never executed** — right property, wrong path | Which paths can violate this invariant, and does the test drive each? |
| 4 | **One of N call sites** — the invariant is guarded in several places, tested in one | Break it in the *other* branch. Does the test still go red? |
| 5 | **One-sided bound satisfied by nothing happening** — `LessOrEqual(n, 2)` where `n` is always 0 | What does this assert when the count is zero, and is zero reachable because the arm never ran? Time-, size- and interval-gated arms are the usual carriers |
| 6 | **Look-alike fixture** — a hand-built error or constant that reads correct but has a different identity than the real sentinel | Am I comparing against the actual exported value, or a string that resembles it? |
| 7 | **Falsified on the wrong axis** — the test is genuinely sensitive to the mutation you imagined, and structurally blind to the defect that exists | Does my mutation exercise the same axis as the real bug, or do the test and the defect share a blind spot? |

**(d) Run a positive control.** Red-green proves the test responds to the one mutation you thought of. A positive control proves the harness can reach the path at all: **make the observable appear on purpose**, and confirm the test sees it. This is the only check that catches modes 3, 5 and 7, and the only one that catches a harness that was never wired up — a mock missing a field, a component that renders disabled, a listener never registered.

**(e) If you mutate to falsify, verify the mutation applied.** An anchor-based patch (`sed`, `str.replace(old, new, 1)`) is a silent no-op when the anchor does not match, and a surviving test then reads as vacuous when it is fine. `git diff` the mutated tree before believing any verdict. Prefer changing a **value** over deleting a statement, and anchor on something a formatter cannot reflow.

**The gate.** Report the evidence:

```
Repro test: <name>
Fails with: <paste the assertion failure>
Oracle: <the one-sentence observable>
Vacuity checklist: <modes considered, and why each is excluded>
Positive control: <what was forced, and that the test observed it>
```

- **`In-the-Loop`, or standalone with no mode established** → stop and ask: *"Does this match the bug you're seeing? [yes / no — wrong failure mode]"*. Do NOT proceed without confirmation.
- **`On-the-Loop` / `Automated End-to-End`** → proceed on the evidence above, which must be complete. The gate is not skipped, it is discharged by evidence rather than by a human. An incomplete checklist is a stop in every mode.

### Step 1.5 — Commit the repro

```bash
git add <test file>
git commit -m "test: reproduce #$0 — <one-line bug description>"
```

**When there is no issue number** — a sub-skill run whose `$0` is absent — use the inherited task identifier instead, and if there is none, drop the marker rather than emitting a dangling one:

```bash
git commit -m "test: reproduce <task-id> — <one-line bug description>"   # inherited id
git commit -m "test: reproduce <one-line bug description>"               # no identifier
```

Never emit `#` or `(#)` with nothing after it — a dangling marker reads as a broken link forever.

Keep this commit free of fix code. It is the regression guard future contributors inherit, and its separateness is what makes the fix auditable.

---

## Phase 2: FIX LOOP — bounded iteration

Maximum **5 hypothesis-test iterations**. Track the count explicitly.

> This budget is *hypothesis iterations* and is distinct from the orchestrator's per-failure retry budget (tier axis 4, which governs pre-commit local remediation attempts). They are different scopes; neither caps the other.

### Who applies the fix

Route by the surface the hypothesis implicates, per the repo's Phase-3 dispatch table — `@backend-developer` or `@frontend-developer` — then check the **E1-E4 escalation criteria** before dispatching. In a debugging loop two of them fire often:

- **E2** — the bug lives on an auth / crypto / key-handling / RBAC / IPC-sender surface, or on any extra surface the repo lists in `.agents.escalation_surfaces` (for example a concurrency fence). Escalate on the first iteration, not the third.
- **E4** — **iteration 3 with no green.** Two failed hypotheses on the same task is a producer bounce; hand the accumulated hypothesis table to the repo's escalation agent (`.agents.escalation`, see `_shared/lifecycle-agent-dispatch.md` § Escalation; or to the developer, when none is defined) rather than spending iterations 4 and 5 the same way.

For a one-line fix in a file already open, execute inline — the delegation-overhead gate (D0) applies here exactly as it does in Phase 3.

### Iteration template

For iteration N (1 ≤ N ≤ 5):

**Hypothesis** — one sentence on what is wrong and why, citing the `file:line` you suspect.

**Smallest change** — the narrowest edit that would confirm or falsify it. No refactors, no adjacent cleanup, no "while I'm here." A wrong hypothesis should cost one revert, not an unpicking.

**Run the repro test** — same command as Step 1.4.

**If red** — record the new failure mode in the hypothesis table, revert the hypothesis, and form a *different* hypothesis. Not a variation of the same one. Go to N+1.

> **Revert the HYPOTHESIS, not the file.** `git checkout -- <file>` restores the file to HEAD, which silently destroys any unrelated uncommitted edit in it — and if the hypothesis touched several files, reverting one leaves the rest half-applied. Before iteration 1, confirm a clean baseline (`git status --porcelain` empty apart from the committed repro test). Then reverse the exact patch across **every** file the hypothesis touched:
>
> ```bash
> git diff > /tmp/hypothesis-N.patch    # capture before reverting — it is your audit trail
> git apply -R /tmp/hypothesis-N.patch  # reverses precisely what you applied
> git status --porcelain                # must be empty; if not, stop and report
> ```
>
> If the baseline is not clean and the developer wants those edits kept, say so and stop rather than reverting over them. Losing uncommitted work to a debugging loop is not an acceptable cost of a hypothesis test.

**If green** — widen in two steps, because a fix that greens the repro by breaking something else is not a fix:

```bash
go test -race ./<package>/...    &&  go test -race ./...      # Go: module, then all
npm test -- --run <module>       &&  npm test                 # TS: module, then all
```

Then **re-verify the repro test still falsifies.** A fix that changed the code's shape can silently move the test off the path it was written to exercise — re-apply the Step 1.4(e) mutation and confirm it still goes red. A proven test can stop testing its subject; this is where that is cheapest to catch.

**Revert that mutation immediately, then re-run before going anywhere.** The mutation is deliberately broken production code sitting in your working tree.

> **Do NOT reuse the hypothesis-revert recipe here — the tree is different.** In the fix loop the baseline is clean, so `git diff` is the hypothesis alone. At this point your **fix is still uncommitted**, so `git diff` is *fix + mutation* and `git apply -R` of it would delete the fix along with the mutation. Separate them explicitly:
>
> ```bash
> git diff > /tmp/fix-only.patch           # BEFORE mutating — the fix, alone
> #   ...apply the mutation, run the repro test, confirm it goes red...
> git diff > /tmp/fix-plus-mutation.patch
> git apply -R /tmp/fix-plus-mutation.patch   # back to the committed baseline
> git apply    /tmp/fix-only.patch            # fix restored, mutation gone
> git diff | diff -q - /tmp/fix-only.patch    # MUST match, or stop and report
> ```
>
> That last line is the check, not a formality: it is what proves the mutation left and the fix stayed.

Then re-run the repro test **and** the full suite on the restored tree, and confirm `git status --porcelain` shows only the intended fix. Do not enter Phase 3 or write the fix commit on an un-restored tree — shipping the mutation would be the worst possible outcome of a step whose entire purpose is verification.

Full suite green on the restored tree, and the test still falsifiable → Phase 3.

### Budget exhausted (N = 5)

Stop. Produce the hypothesis-evidence table:

`<identifier>` below is `#$0` when there is an issue number, or the inherited task id in sub-skill mode. When **neither** exists, drop the trailing ` for <identifier>` entirely — never emit a bare `#` or `(#)`.

```
Reproduce-then-fix budget exhausted for <identifier>.

| # | Hypothesis | Change attempted | Test result | Why ruled out |
|---|-----------|-----------------|-------------|---------------|
| 1 | ... | ... | red | ... |

Repro test is committed at <sha> and remains valid.
Recommendation: <pair-with-developer | broaden test scope | check upstream dependency>
```

The repro commit **stays** — it is the durable product of this run even when the fix is not. Hand back to the developer (standalone) or return the table to the caller (sub-skill). Do NOT spend a 6th iteration without explicit re-approval: past iteration 5 the marginal information per attempt drops sharply, and the right move is a fresh pair of eyes.

---

## Phase 3: HAND BACK — green fix, no PR

Commit the fix, then stop.

```bash
git add <production files>
git commit -m "fix: <one-line description> (#$0)"
# no issue number: "fix: <one-line description> (<task-id>)" — or omit the suffix entirely
```

The fix commit must NOT contain test-file edits — the repro test is already committed at Step 1.5. If the fix legitimately forced a test change (a brittle assertion that should have been more flexible), commit that separately as `test: <description>` *after* the fix, and say why in the message.

**Sub-skill mode:** return control to the caller's Phase 3 loop. Report the repro commit SHA, the fix commit SHA, the iteration count, and the oracle. Nothing else.

**Standalone mode:** hand to the lifecycle.

```
/dev-lifecycle $0
```

Detection resolves `branch + implementation commits + no PR` to **Phase 4: VALIDATE** with no argument needed.

### Why this skill stops here

Phase 4 is not optional bookkeeping — it is four gates this skill does not implement and must not bypass:

1. `verification-before-completion` and the repo's quality gate
2. The repo's new-code coverage gate (`.quality_gate.coverage_min`, when `.quality_gate.enabled`) — a repro test plus a fix is rarely enough on its own
3. The docs / count-drift probe, which is non-deferrable and lands in the same PR
4. The pre-PR adversarial security pass on security-sensitive surfaces

An earlier revision of this skill opened its own draft PR and handed back at Phase 6, which skipped all four for every bug fix that used it. Stopping one phase earlier costs nothing and closes them. **Do not re-add PR creation here.**

---

## What this skill does NOT do

- Does NOT edit production code before a failing test exists, is proven non-vacuous, and is committed
- Does NOT accept a red test without establishing *why* it is red
- Does NOT proceed past Step 1.4 on an incomplete vacuity checklist, in any autonomy mode
- Does NOT continue past iteration 5 without escalation
- Does NOT merge the repro-test commit and the fix commit into one — they are separately auditable
- Does NOT open PRs or push, in either entry mode, and does NOT create branches in sub-skill mode (standalone Step 1.0 only selects or creates the issue branch)
- Does NOT handle features, refactors, or "while I'm here" cleanup — the scope is the one defect
- Does NOT skip the full-suite regression check, nor the post-fix re-falsification of the repro test

---

## Why this skill exists

Bug fixes without a failing-test-first discipline have two failure modes:

1. **Fixed the wrong thing** — the mental model of the symptom was incomplete, the "fix" addresses an adjacent issue, and the original bug recurs.
2. **No regression guard** — without a committed reproduction test, the bug is silently re-introduced by an unrelated refactor weeks later.

Both have happened in this codebase. The bounded 5-iteration budget exists because debugging cycles past iteration 5 usually mean the hypothesis space is too large for solo work — that is the signal to escalate, not to grind.

The vacuity checklist in Step 1.4 exists because a **third** failure mode turned out to be the most common of all: the test is written, it goes red, it goes green, and it never tested the claim. Every row in that table was paid for once already.

The skill's text is the enforcement mechanism. There is deliberately no PreToolUse hook gating production edits on test additions — such a hook would also block feature work, refactors, and migration edits. Discipline lives in the skill, not in the harness.
