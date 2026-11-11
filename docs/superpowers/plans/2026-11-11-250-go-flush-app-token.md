# Go Transitive-Flush App Token Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The weekly Go transitive-flush workflow opens an App-authored pull request on which PR checks run, with no write token live while Go modules are fetched.

**Architecture:** One job in one workflow gains a late App-token mint step and hands that token to `create-pull-request`. `GITHUB_TOKEN` is narrowed to what the contention guard needs. The existing render test gains static assertions that lock the step order.

**Tech Stack:** GitHub Actions YAML, `actions/create-github-app-token`, `peter-evans/create-pull-request`, bash test harness run by pre-commit.

**Spec:** `docs/superpowers/specs/2026-11-11-250-go-flush-app-token-design.md`

## Global Constraints

- Mint step uses `actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510 # v3.3.0`, the same pin as `.github/workflows/refresh-ioc-list.yml`.
- Mint step inputs: `client-id: ${{ vars.PUBLIC_MIRROR_APP_CLIENT_ID }}`, `private-key: ${{ secrets.PUBLIC_MIRROR_APP_PRIVATE_KEY }}`, `permission-contents: write`, `permission-pull-requests: write`.
- Triggers stay `schedule` and `workflow_dispatch`. No `pull_request` trigger is added.
- Job permissions become exactly `contents: read` and `pull-requests: write`.
- This is a public repository. No comment, body text or doc may cite an issue or PR number from another repository.
- Commit subjects use a bare Conventional Commits type with no parenthesized scope.
- Never stage `.claude/state/`.

## Review Focus

- **Mint step moved above the module refresh.** A write token would be live during `go get -u`. Pinned by the order assertion in Task 1.
- **Deferred or no-change run mints a token.** Expected: no token is minted. Pinned by the `if:` count assertion in Task 1.
- **`create-pull-request` falls back to `github.token`.** Expected: the step fails as it does today. Pinned by the token-source assertion in Task 1.
- **Rendered body still tells the reviewer to close and reopen, or links `#1212`.** Expected: neither appears. Pinned by the body assertions in Task 1.
- **App variable or secret missing in this repository.** Expected: the mint step fails and the job goes red, which is visible. No test; this is the action's own behavior, and the end-to-end dispatch in the PR proves the credentials resolve.

---

### Task 1: Late App-token mint in the flush workflow

**Files:**
- Modify: `.github/workflows/go-transitive-flush.yml`
- Test: `scripts/tests/test-go-transitive-flush-render.sh`

**Interfaces:**
- Consumes: nothing.
- Produces: step `id: app-token` named `Mint flush App token`; Task 2's docs describe it by that name.

- [ ] **Step 1: Write the failing assertions**

In `scripts/tests/test-go-transitive-flush-render.sh`, replace the "body must carry the CI re-trigger warning" block with assertions that the rendered body does NOT contain `Close and reopen this PR`, `GITHUB_TOKEN` or `#1212`.

Then add a static block before the summary lines, using the existing `ok` / `bad` helpers and `$WORKFLOW`:

```bash
line_of() { grep -n -- "$1" "$WORKFLOW" | first_line | cut -d: -f1; }
refresh_at=$(line_of '^      - name: Refresh transitive Go module graph')
mint_at=$(line_of '^      - name: Mint flush App token')
open_at=$(line_of '^      - name: Open PR if changed')
GATE="if: steps.contention.outputs.deferred != 'true' && steps.refresh.outputs.has_changes == 'true'"
```

Assertions, one `ok`/`bad` each:

1. `refresh_at`, `mint_at` and `open_at` are all non-empty and `refresh_at < mint_at < open_at`.
2. `grep -cF -- "$GATE" "$WORKFLOW"` equals `3` (render, mint, open).
3. `grep -qF 'token: ${{ steps.app-token.outputs.token }}' "$WORKFLOW"` succeeds and `grep -qF 'token: ${{ github.token }}' "$WORKFLOW"` fails.
4. `grep -qE '^      contents: write' "$WORKFLOW"` fails and `grep -qE '^      contents: read' "$WORKFLOW"` succeeds.

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash scripts/tests/test-go-transitive-flush-render.sh`
Expected: non-zero exit, with `FAIL:` lines for the body assertions and all four static assertions.

- [ ] **Step 3: Change the workflow**

In `.github/workflows/go-transitive-flush.yml`:

- Job `flush` permissions: `contents: read`, `pull-requests: write`.
- Insert the `Mint flush App token` step (`id: app-token`) between "Render PR body" and "Open PR if changed", gated by the same `if:` line, with the pin and inputs from Global Constraints.
- "Open PR if changed": `token: ${{ steps.app-token.outputs.token }}`.
- Rendered body: delete the whole `## ⚠ CI does not run on this PR until you re-trigger it` section and the `- Founding issue: #1212` line.
- Header comment: replace the "CI re-trigger note" and the paragraph after it with a note that the PR is App-authored so PR checks start on their own, and that the token is minted only after the module refresh. Remove `#1228`. In the "Open PR if changed" comment, remove `(#847)`. Update the top-level permissions comment so it no longer says `create-pull-request` uses the job token.

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash scripts/tests/test-go-transitive-flush-render.sh`
Expected: exit 0, `0 failed`.

- [ ] **Step 5: Prove the order assertion is not vacuous**

Temporarily move the mint step above "Refresh transitive Go module graph", run the test, confirm assertion 1 fails, then restore the file (`git checkout -- .github/workflows/go-transitive-flush.yml` is NOT the restore, since the change is uncommitted; undo the move by hand) and re-run to green.

- [ ] **Step 6: Lint and commit**

Run: `pre-commit run --files .github/workflows/go-transitive-flush.yml scripts/tests/test-go-transitive-flush-render.sh`
Expected: all hooks pass.

```bash
git add .github/workflows/go-transitive-flush.yml scripts/tests/test-go-transitive-flush-render.sh
git commit -m "ci: open go-transitive-flush PRs with an App installation token"
```

### Task 2: Docs that describe the old behavior

**Files:**
- Modify: `.claude/rules/domains/backend.md` (§ Dependency Hygiene)
- Modify: `.claude/skills/weekly-deps/SKILL.md` (bot-PR discovery queries)
- Modify: `.pre-commit-config.yaml` (hook display name only)

**Interfaces:**
- Consumes: the step name `Mint flush App token` from Task 1.
- Produces: nothing.

- [ ] **Step 1: `backend.md`**

Replace the "CI re-trigger required" bullet with a "PR authorship" bullet: the PR is opened with a short-lived App installation token minted after the module refresh, so `pull_request` workflows run on it with no manual step. In the "Bot-PR CI re-trigger pattern" paragraph, state that `go-transitive-flush.yml` no longer belongs to the `GITHUB_TOKEN` category. Remove issue numbers that belong to another repository from the lines you touch.

- [ ] **Step 2: `weekly-deps/SKILL.md`**

After the `author:app/github-actions` discovery query, add a third query with the same `--json` field list: `--search 'label:"routine: go-transitive-hygiene" is:open'`, with a one-line comment that App-authored flush PRs do not match the author query.

- [ ] **Step 3: `.pre-commit-config.yaml`**

The hook `test-go-transitive-flush-render` is named `'Test go-transitive-flush PR-body render (#1212)'`. Rename it to `'Test go-transitive-flush PR-body render and token order'`.

- [ ] **Step 4: Verify and commit**

Run: `pre-commit run --files .claude/rules/domains/backend.md .claude/skills/weekly-deps/SKILL.md .pre-commit-config.yaml`
Expected: all hooks pass. If a mirror-sync hook reports a stale copy of either doc, regenerate the mirror with the command the hook prints and stage it.

```bash
git add .claude/rules/domains/backend.md .claude/skills/weekly-deps/SKILL.md .pre-commit-config.yaml
git commit -m "docs: describe App-authored go-transitive-flush PRs"
```

## After the tasks

- Create the repository label: `gh label create "routine: go-transitive-hygiene" --description "Weekly Go transitive module refresh" --color BFD4F2`.
- End-to-end dispatches (issue acceptance criteria) run from `main` after merge, because `workflow_dispatch` on a feature branch would open a flush PR built from that branch. Record both results on issue #250.
