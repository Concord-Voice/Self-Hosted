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
> override at dispatch. There is no exception: the former auth/crypto/RBAC carve-out
> was retired and replaced by agent selection (see
> [`lifecycle-agent-dispatch.md`](lifecycle-agent-dispatch.md) § Escalation (E1-E4)).
> Cross-vendor profile generation follows
> [`model-capability-tiers.md`](model-capability-tiers.md); on Codex, its role-specific mapping
> assigns `@red-team` to `gpt-6-sol` without changing its source Opus pin.
>
> **This table dispatches reviewers, which are unaffected.** Every agent listed
> below keeps its own pin. Escalation applies to the *fix application* that follows
> a review, not to the review itself — when a finding lands on an E2 surface, spans
> both the frontend and backend (E3), or survives two producer fix attempts (E4),
> the fix is handled per § Escalation rather than by a producer agent. Record it in
> the Phase-8 dispatch line the same way Phase 3 does.

## Agent Dispatch (Priority 0 — runs first)

Fetch three PR signals **once**, up front, then dispatch every entry whose condition matches, in parallel — **but dispatch each AGENT at most once, however many of its rows match.**

**One dispatch per agent, not one per matching row.** The rows are coverage documentation; the
agent is the unit of work. Several rows can name the same reviewer — a change to `**/auth/**`
matches both a file row and the risk-tier row for `@red-team`, and both resolve to the same
agent — and running it twice buys nothing while doubling cost and producing two reports to
reconcile. Collapse the matches, then dispatch, and record in the dispatch line which rows a
reviewer came in on so the coverage claim each row makes stays auditable.

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
| **Risk-tiered** — mandatory at tier T2 or T3, optional at T0 or T1 (see § Codex-conditional reviewers). Mandatory whenever Codex's SECURITY review has not covered the head. The primary risk signals are label `risk: security` / `risk: privacy`, files `**/auth/**`, `**/crypto/**`, `**/permissions/**`, `src/main/ipc*`, `src/preload/*`, and content **[E1]** or **[R1]** | `@red-team` | Priority 1 |
| Files `**/*.go` · content **[S1]** | `@pr-review-toolkit:silent-failure-hunter` (plugin agent) | Priority 2 |
| Files `**/*_test.go`, `**/*.test.ts`, `**/*.test.tsx`, `**/tests/**` | `@pr-review-toolkit:pr-test-analyzer` (plugin agent) | Priority 3 |
| Always — unless the repo's `.review_agents.mandatory`, read from the merge base, names a different always-on security reviewer (see § Repo-defined security floor) | `@security-reviewer` | Priority 1 |
| Label `type: feature` (over-building concentrates in feature work; a review-tooling yield audit measured no unique high-severity findings across a long run of unconditional dispatches) | `@scope-reviewer` | Priority 4 |
| **Codex-conditional** — mandatory unless Codex's CODE review has already covered the current head, optional when it has (see § Codex-conditional reviewers). Covered means a Completed **Code Review** row for the head commit in its summary comment (the SHA there is a 7–40 character prefix), or inline Codex findings on the head. Codex covers generic correctness there. A Completed **Security Review** row does not count, and neither does a 👍 reaction or a usage-limit notice: a past PR had its security review complete and Codex react 👍 while its code review was blocked by the usage limit. Dispatch it as the fallback whenever the Codex code review is blocked, absent, or behind the head — and always when the repo has no Codex reviewer | `@code-reviewer` | Priority 2 |

> **The rows above and the `.review_agents.patterns[]` entries in `.claude/repo-config.json`
> are ONE trigger set in two places** wherever both name the same agent. `/enhanced-pr-review`
> dispatches from this table while `/review-pr` dispatches from the config, so a trigger added to
> only one of them makes a lifecycle review and a direct review disagree about whether the
> specialist ran — which is how a firewall change was once dispatched by one path and skipped by
> the other. Add to both, or to neither.

**Trigger changes after a yield audit.** The `@code-reviewer` and `@scope-reviewer` rows above
changed after a review-tooling audit measured each agent's yield per run. They cover
`/enhanced-pr-review` only. `/review-pr` keeps its own mandatory base
(default `security-reviewer code-reviewer docs-reviewer`, replaced by `.review_agents.mandatory`
from the merge base when set; `.review_agents.base` only adds to it) because a developer
invoking it by hand has asked for that review.

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

> **[E1] and [R1] are placeholders you must fill in.** Unlike [S1], [N1] and [D1] — whose
> tokens are language and infrastructure idioms that hold in most Go/TypeScript repos — a
> crypto or authorization trigger only works when it names the symbols *your* codebase uses.
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
- **[N1] Packet path** — `network_mode`, `DOCKER-USER`, `iptables`, `ufw `, `userland-proxy`, `ports:`, `.listen(` + your own transport's bind/announce config keys (a new listener or a changed bind is the chain/interface/protocol/mode question a service-boundary review does not ask)
- **[D1] Deployment-config** — `os.Getenv`, `getEnv(`, `getEnvAlias(`, `secrets.`, `vars.` (the `${{ secrets.X }}` / `${{ vars.X }}` workflow refs are the reviewer's Class-2 secret-scope-drift charter — e.g. a signing secret moved in a build workflow, which matches no glob above)

**Three of these are plugin or advisory agents, and their standing differs.** `@pr-review-toolkit:silent-failure-hunter` is the enforcement for the error-discard rule your backend rules file should carry (`errcheck` honors explicit `_` blank-discards, so the linter cannot catch it) — its findings are **blocking**, exactly like a first-party reviewer's — a standing that two production incidents from swallowed errors earned it. `@pr-review-toolkit:pr-test-analyzer` reviews test *quality* (do the tests actually exercise the change?), a gap between `@test-writer` (authors them) and `@code-reviewer` (generic); its findings are normal-severity. It declares `model: inherit`, so it runs on the orchestrator's model and can be expensive per run; a repo-owned test-quality reviewer with a lower pin that reads only the diff and its covering tests is a reasonable replacement. `@scope-reviewer` is **advisory only** — a reasoned refusal is a valid resolution and is recorded, not re-argued; it must never block a merge.

**`@red-team` is adversarial, not additive coverage.** It constructs executable proof-of-concept exploits rather than checking a list, and is dispatched with `isolation: worktree`. **That isolation redirects its working directory; it does not sandbox it** — the agent's own definition (`red-team.md`) says so: an absolute path in a `Write` or a `Bash` command still reaches the rest of the filesystem. Treat a PoC artifact appearing in the shared tree as expected-but-wrong, and check for one before you commit: a past red-team PoC was swept into a commit and had to be removed. Its artifacts are **evidence**, never merged without human review. It NEVER replaces `@security-reviewer`, whose value is consistent baseline coverage. A `@red-team` finding without a passing PoC is a hypothesis and must be reported as one. It also runs pre-PR at Phase 4 per [`lifecycle-agent-dispatch.md`](lifecycle-agent-dispatch.md).

## Repo-defined security floor

A repo that replaced `@security-reviewer` with another always-on reviewer (for example one
that retired the checklist reviewer in favour of `@red-team`) lists its floor in
`.review_agents.mandatory`. It is never read from the checkout. It is read from the merge base
of the head with the repo's **default branch**, which review-pr's trust guard resolves. When the
PR targets the default branch, that is the PR's own merge base. When it targets another branch, it
is not, because the author picks the base and an unreviewed branch or a stacked PR could carry a
weaker floor. When the key is set, its agents replace the default floor
(`security-reviewer code-reviewer docs-reviewer`), and the `@security-reviewer` references in the
rest of this table mean that floor. A PR that changes the key is judged by the old value. When
the merge base has no config file, or the file has no such key, use the default floor. Stop and
report it, rather than falling back silently, when any of these hold:

- the file is not a regular file;
- the file cannot be read or is not a JSON object;
- `review_agents` is not an object;
- the key is not a non-empty array of names matching `^[a-z0-9][a-z0-9-]*$`.

## Codex-conditional reviewers

`@security-reviewer` stays always-on as the checklist baseline. Two other first-party reviewers
depend on what Codex covered on the **current head**. When the repo has no Codex reviewer
configured, nothing is ever covered, so both are mandatory under the rules below.

**Covered** means a Completed row for that review type in Codex's summary comment
(`<!-- codex-pull-request-review-summary -->`) whose commit is a 7–40 character prefix of the
head SHA, or inline Codex findings of that type on the head. **Not covered** is everything else:
a Failed row, a usage-limit or allowance notice, a 👍 reaction on its own, a row for an older
commit, or no Codex comment at all. The two review types are independent, so a Completed
Security Review never counts as Code Review coverage, and the reverse.

**Mandatory** means the reviewer is dispatched and Phase 8 does not pass without its report.
**Optional** means the orchestrator decides; a dispatch states its reason in one line and a skip
needs none. Either way, record the decision in the dispatch line, for example
`code-reviewer: optional, skipped (Codex Code Review completed on a1b2c3d)` or
`red-team: T2, mandatory (Codex Security Review not covered; RS2 src/auth/session.go)`.

### `@code-reviewer`

| Codex Code Review on the head | `@code-reviewer` |
|---|---|
| Not covered | Mandatory |
| Covered | Optional — dispatch when the diff carries logic Codex's summary did not discuss |

### `@red-team`: risk tiers

Compute three inputs on the head (four when `.red_team.critical_agent` is set: add the changed files against `.red_team.critical_paths[]`), then take the **highest** tier whose condition holds.

1. **Codex security coverage:** covered or not covered, as defined above.
2. **Codex severity:** the highest Codex Security Review finding still unresolved on the head.
   P0 is critical, P1 high, P2 medium and P3 low.
3. **Risk signals**, read from files, labels and diff body:
   - **Primary** (any one is enough):
     **RS1** label `risk: security` or `risk: privacy`;
     **RS2** a changed file under `**/auth/**`, `**/crypto/**`, `**/permissions/**`,
     `src/main/ipc*` or `src/preload/*` (plus any repo-specific sensitive surface, such as a
     native addon, that your `.review_agents.patterns[]` routes to `red-team`);
     **RS3** content **[E1]** or **[R1]**.
   - **Secondary** (two or more are needed):
     **RS4** a workflow trust-boundary edit, meaning a line under `.github/workflows/**` that adds
     or changes `id-token:`, `environment:`, `pull_request_target`, a `permissions:` write scope
     or a `secrets.` reference;
     **RS5** a new runtime dependency or a changed lockfile in a shipped package;
     **RS6** a new network listener or externally reachable route (content **[N1]**, or a new
     route registration in the backend).

| Tier | Condition | `@red-team` |
|---|---|---|
| **T3 Critical** (only when `.red_team.critical_agent` is set) | Codex Security has an unresolved P0 or P1 on the head; OR a changed file matches `.red_team.critical_paths[]`; OR `@red-team` returns a CRITICAL or HIGH finding with a passing PoC | Mandatory, and the critical agent is mandatory too |
| **T2 Elevated** | Codex Security did not cover the head; OR it left an unresolved P0, P1 or P2; OR any primary signal; OR two or more secondary signals | Mandatory |
| **T1 Guarded** | Codex Security covered the head with nothing above P3, no primary signal, exactly one secondary signal | Optional (recommended) |
| **T0 Clear** | Codex Security covered the head with nothing above P3, and no risk signal | Optional |

The primary signals reproduce `@red-team`'s unconditional trigger, so Codex coverage can only make
it optional on a PR that touches none of those surfaces.

**T3 and the critical agent.** A repo may name a second, deeper red-team agent for its most
sensitive code in `.claude/repo-config.json`: `.red_team.critical_agent` (a bare agent name, no
`@`, resolving to `.claude/agents/<name>.md`) and `.red_team.critical_paths[]` (globs, matched
exactly as review-pr matches `.review_agents.patterns[].glob`: a bash pattern match against each
changed path from the repo root, where `*` also matches `/`). Read both keys from the **merge
base**, never the head: a PR must not be able to lower its own tier by editing them. Use the merge base of the head with the repo's **default branch**, not with the
PR's own base: the author picks the base, so an unreviewed branch or a stacked PR could carry a
weaker value. Resolve it with `git merge-base "refs/remotes/origin/$DEFAULT_REF" "$HEAD_SHA"` (the full refname, so a branch or tag named `origin/<default>` cannot stand in; review-pr's
floor guard does the same through the API), then read
`git show "$MERGE_BASE_SHA:.claude/repo-config.json" | jq '.red_team'`. If the merge base or that
file cannot be read, treat the PR as T3 (fail closed). A PR that changes either key, or the critical
agent's own file, is T3. When the PR modifies `.claude/agents/<critical_agent>.md`, never dispatch
the edited definition; that would let the PR weaken its own critical reviewer. Report the PR as
blocked on a run of the merge-base definition or on a human security review, the same refusal
review-pr applies to any reviewer whose definition the PR modifies. With `critical_agent` unset there is no T3 tier, and an unresolved Codex P0 or P1 stays T2. A T3 reached
through a `@red-team` finding is evaluated **after** `@red-team` reports, so it adds the critical
agent to the same review run rather than starting a new one. The critical agent never replaces
`@red-team` or a domain reviewer; they still run on their own rows. If the critical agent cannot
run on the current harness, or its agent file does not exist, report the PR as blocked on a run of
that agent (fail closed). Record it in the dispatch line, for example
`red-team: T3, mandatory; <critical-agent>: mandatory (critical_paths match)`. Do not substitute
another agent or model.

## Agent names in GitHub posts

Write agent names **bare** in anything posted to GitHub (PR and issue comments, review
replies, review bodies, PR descriptions, issue bodies): `red-team`, `code-reviewer`,
`security-reviewer`. Never `@red-team`. GitHub resolves an `@`-prefixed word as a user or team mention, so a
prefixed agent name notifies, or links, whoever owns that login. The `@` prefix in this file and
in the other skill and agent docs is local dispatch notation, and stays there.

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
| Files matching your deployment-config surface (env consumers, the whole `.github/workflows/**` tree, deploy scripts, `docker-compose*.yml`) · content **[D1]** | `@deployment-config-reviewer` | Priority 1 |
| Files matching your packet-path surface (firewall/provisioning scripts, reverse-proxy config, `docker-compose*.yml`, and the config files where bound ports are decided) · content **[N1]** | `@network-engineer` | Priority 2 |

`[E1]`, `[R1]`, `[N1]`, and `[D1]` above reuse the content-trigger tokens defined
above — replace the example token lists with the symbol names your own
codebase actually uses.

Two lessons for the deployment-config and packet-path rows. **Glob the whole workflow tree,
not an enumerated list of workflows** — an enumerated list silently skips the ones nobody
remembered (e.g. the scanning workflows), and a trust-boundary weakening such as an added
`environment:` or `id-token: write` carries no [D1] token. **List the files where a bound
port is actually decided**, not just the obvious deploy files: content tokens match the diff
body, not the file, so a PR that edits only a port line produces a diff the bind tokens may
sit far away from — and the failure then is the wrong specialist, not no specialist.

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
