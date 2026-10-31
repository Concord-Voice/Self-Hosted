# PR Review — Specialist-Agent Dispatch Table

> **Single source of truth** for the project-side multi-agent PR review. Consumed by:
> - `/review-pr` (user-facing convenience skill — `disable-model-invocation: true`)
> - `/enhanced-pr-review` Phase 1 Step 5 (dispatches these directly)
> - `/weekly-deps` Tier-C fallback (same direct dispatch)
>
> **How "direct dispatch" works:** the `@`-prefixed entries are subagents — dispatch them
> via the **Agent tool**, which bypasses the `Skill` tool entirely. The one `/`-prefixed
> entry (`/ipc-channel-audit`) is a *model-invocable* skill — dispatch it via the `Skill`
> tool. None of these is a `disable-model-invocation` skill, so the model can dispatch every
> entry from any authorized orchestrator regardless of `review-pr`'s own flag. See
> `.claude/rules/skill-authoring.md` (installed there by `install.sh claude`).
> Its pre-review counterpart is
> [`lifecycle-agent-dispatch.md`](lifecycle-agent-dispatch.md), which covers agent
> dispatch at Phases 1-4. Together they cover every agent-dispatching point in the
> lifecycle. Both pin models in the agent files deliberately - do NOT pass a `model`
> override at dispatch.

## Agent Dispatch (Priority 0 — runs first)

Fetch three PR signals **once**, up front, then dispatch every entry whose condition matches, in parallel:

- **Files** — `gh pr diff <PR> --name-only`
- **Labels** — `gh pr view <PR> --json labels -q '.labels[].name'`
- **Diff body** — `gh pr diff <PR>` (for the content triggers)

Matching is **over-inclusive by design**: a needless reviewer run just returns APPROVED, but a *missed* one is the exact security-review gap this table exists to prevent. A hit on **any** of a row's file / label / content triggers dispatches that reviewer — because a file glob only sees what a change is *named*, not what it *does*. The canonical blind spot: a key-clearing call added to a generically-named reset module, which matches no `**/e2ee*` glob and would otherwise ship unreviewed.

| Condition (file glob · label · content — any one matches) | Agent / Skill | Reports to |
|---|---|---|
| Files matching `` the directory named by `rc_get '.migrations.dir'` (default `migrations/`) ``/*.sql | `@migration-reviewer` | Priority 2 |
| Files `src/frontend/**` (adjust to your layout — matches `frontend-reviewer.md`'s own scope) | `@frontend-reviewer` | Priority 2 |
| Files `src/preload/*`, `src/main/ipc*` | `/ipc-channel-audit --only-new` (model-invocable skill — dispatch via the Skill tool) | Priority 1 |
| Any `.md` file changed | `@docs-reviewer` | Priority 5 |
| Label `risk: security` · files `**/auth/**`, `**/crypto/**`, `**/permissions/**`, `src/main/ipc*`, `src/preload/*` · content **[E1]** or **[R1]** | `@red-team` | Priority 1 |
| Files `**/*.go` · content **[S1]** | `@pr-review-toolkit:silent-failure-hunter` (plugin agent) | Priority 2 |
| Files `**/*_test.go`, `**/*.test.ts`, `**/*.test.tsx`, `**/tests/**` | `@pr-review-toolkit:pr-test-analyzer` (plugin agent) | Priority 3 |
| Always | `@security-reviewer` | Priority 1 |
| Always | `@scope-reviewer` | Priority 4 |
| Always | `@code-reviewer` | Priority 2 |

> The Electron globs above (`src/preload/*`, `src/main/ipc*`) assume the Electron
> app lives at the repo root (`.`) — the directory named by
> `rc_get '.paths.electron_dir'` (default `.`). If this repo nests it elsewhere
> (e.g. `apps/desktop/`), prefix both globs with that directory.

**Project-specific reviewers.** This table covers reviewers generic enough to
ship with the template. A repo with its own sensitive subsystems (crypto, an
authorization model, a realtime transport, deployment config) should add
matching domain reviewers via `.claude/repo-config.json`'s `.review_agents.patterns[]`
array — same shape as a row above (file globs / label / content trigger →
agent name + priority). See "Example: add your own domain reviewers" below
for a worked template.

**Content triggers** — dispatch when the diff body contains any listed token (case-sensitive; matches added, removed, or context lines — over-inclusive is the safe direction). These are heuristics with a known ceiling; tune a list if a real change slips through or it over-fires. <!-- ponytail: heuristic token lists, not AST analysis — the point is to catch key-lifecycle / RBAC-semantics / env-consumer edits made from off-glob files, not to be exhaustive. Upgrade path if it proves too noisy or too leaky: narrow/widen the alternation, don't reach for a parser. -->

> **[E1] and [R1] are placeholders you must fill in.** Unlike [S1] and [D1] — whose
> tokens are language idioms that hold in any Go/TypeScript repo — a crypto or
> authorization trigger only works when it names the symbols *your* codebase uses.
> The lists below are shaped as descriptions plus a generic starting set; replace
> them with your real identifiers via `.claude/repo-config.json`'s
> `.review_agents.patterns[]` `content` conditions. Left as-is they under-fire.

- **[E1] Crypto / key lifecycle** — the module that holds key material, its store or
  hook, and the key-lifecycle verbs (clear / wrap / rotate / invalidate / revoke /
  fence). Generic starting set: `clearKeys`, `wrappingKey`, `sessionKeys`,
  `rotateKey`, `revokeKey`, `invalidate` + your own key-holding service's name.
- **[R1] Authorization / RBAC** — the effective-permission resolver, any permission
  cache or enforcer, and the join tables that grant roles to principals. Generic
  starting set: `role_permissions`, `member_roles`, `ResolvePermissions`,
  `PermissionCache`, `rbac.` + your own permission middleware's name.
- **[S1] Silent failure** — `_ = `, `_, _ = `, `catch {}`, `.catch(() => {})`, `recover()`, `if err != nil { return nil }` (the blank-discard and swallow shapes `errcheck` accepts but a backend rules file should forbid)
- **[D1] Deployment-config** — `os.Getenv`, `getEnv(`, `getEnvAlias(`, `secrets.`, `vars.` (the `${{ secrets.X }}` / `${{ vars.X }}` workflow refs are the reviewer's Class-2 secret-scope-drift charter — e.g. a signing secret moved in `build-desktop.yml`, which matches no glob above)

**Three of these are plugin or advisory agents, and their standing differs.** `@pr-review-toolkit:silent-failure-hunter` is the enforcement for the error-discard rule your backend rules file should carry (`errcheck` honors explicit `_` blank-discards, so the linter cannot catch it) — its findings are **blocking**, exactly like a first-party reviewer's — a standing that two production incidents from swallowed errors earned it. `@pr-review-toolkit:pr-test-analyzer` reviews test *quality* (do the tests actually exercise the change?), a gap between `@test-writer` (authors them) and `@code-reviewer` (generic); its findings are normal-severity. `@scope-reviewer` is **advisory only** — a reasoned refusal is a valid resolution and is recorded, not re-argued; it must never block a merge.

**`@red-team` is adversarial, not additive coverage.** It constructs executable proof-of-concept exploits rather than checking a list, and runs in an isolated git worktree (`isolation: worktree`) so its PoC tests never touch the shared tree. It NEVER replaces `@security-reviewer`, whose value is consistent baseline coverage. A `@red-team` finding without a passing PoC is a hypothesis and must be reported as one. It also runs pre-PR at Phase 4 per [`lifecycle-agent-dispatch.md`](lifecycle-agent-dispatch.md).

Collect each entry's findings under its "Reports to" priority section below.

## Example: Add Your Own Domain Reviewers

A repo with dense, incident-linked invariants around a specific domain —
encryption, an authorization model, a realtime transport, deployment config —
benefits from a dedicated read-only reviewer the way `migration-reviewer`
covers migrations. Copy `migration-reviewer` as your starting template — it is
`plugins/reviewer-packs/agents/migration-reviewer.md` in the agentic-dev-tooling
source repo, and `.claude/agents/migration-reviewer.md` once installed (both
paths are given because this file is read from either place; neither is a link,
because no single relative path resolves in both). Keep its read-only tool
shape: `Read, Grep, Glob, Bash(git diff *), Bash(git log *), Bash(gh pr view *),
Bash(gh pr diff *)`. Drop your new agent beside it in `.claude/agents/`, then
add a matching entry to this table via `.claude/repo-config.json`'s
`.review_agents.patterns[]`. Illustrative rows for the domains above:

| Condition (file glob · label · content — any one matches) | Agent | Reports to |
|---|---|---|
| Files `**/e2ee*`, `**/crypto*` · label `domain: e2ee` · content **[E1]** | `@e2ee-reviewer` | Priority 1 |
| Files `**/rbac/*`, `**/permissions/*`, `**/member_roles*`, `**/role_permissions*` · label `domain: authorization` · content **[R1]** | `@rbac-reviewer` | Priority 2 |
| Files `src/media/**` (adjust to your realtime-transport layout) | `@socket-io-reviewer` | Priority 2 |
| Files matching your deployment-config surface (env consumers, provisioning workflows, deploy scripts, `docker-compose*.yml`) · content **[D1]** | `@deployment-config-reviewer` | Priority 1 |

`[E1]`, `[R1]`, and `[D1]` above reuse the content-trigger tokens defined
above — replace the example token lists with the symbol names your own
codebase actually uses.

## Priority 1 — Security

Check against your repo's own AI-code and crypto policy docs, where it has them:
no hardcoded secrets; no `eval()` / `dangerouslySetInnerHTML` / `innerHTML` without
sanitization; no `fmt.Sprintf` for SQL (parameterized only); no `any` in security-critical
paths (auth, crypto, E2EE, RBAC); approved crypto only (AES-256-GCM, RSA-4096, Argon2id);
auth changes revoke tokens atomically; IPC through the preload bridge only (no
`nodeIntegration`, no `@electron/remote`).

## Priority 2 — Correctness

Does the code do what the PR claims? Are edge cases handled? All errors checked (Go
`errcheck`), no fail-open patterns. Database: `defer rows.Close()`, `rows.Err()` checked
after iteration, correct `argIdx` in dynamic UPDATE builders.

## Priority 3 — Test Coverage

New code has corresponding test files; happy-path + error-path present. Go: `t.Run`
subtests, `require` for preconditions, `assert` for checks. React: `resetAllStores()` in
`beforeEach`, MSW for API mocking. Coverage target: ≥ 80% on new code.

## Priority 4 — Style & Conventions

Conventional Commits format; one issue per PR (`Closes #NNN`) — default, not a hard gate, so a missing `Closes` on an ad-hoc/hotfix PR is MINOR at most; Go handler-struct pattern +
Gin `ShouldBindJSON`; React functional components + Zustand selective subscriptions;
`Co-Authored-By` trailer if AI-assisted.

## Priority 5 — Documentation Drift

`@docs-reviewer` (dispatched above when any `.md` changed): touched source areas have
corresponding doc updates; no duplicated facts (counts, phase status, agent lists) became
inconsistent; cross-references to `CLAUDE.md` present where counts are stated.

## Output Format

Report findings by severity:
- **CRITICAL** — blocks merge (security, correctness)
- **MAJOR** — should fix before merge (quality, missing tests)
- **MINOR** — nice to fix (style, docs)
- **APPROVED** — if all checks pass

Reference specific files and line numbers.
