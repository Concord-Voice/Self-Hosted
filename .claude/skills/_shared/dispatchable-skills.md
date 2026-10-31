# Dispatchable Skills Catalog

This file is the single source of truth for skills that `/issue-creation`, `/pr-creation`, `/dev-lifecycle`, and `/enhanced-pr-review` **MUST consult and dispatch when triggers match** — subject to mode authorization (Component D matrix) and billed-skill preapproval (§ Billed / cloud-cost skills). Consulting the catalog is not optional; a matched candidate that is not dispatched MUST be logged with the reason it was withheld. Each skill is categorized by the `/dev-lifecycle` phase it's a candidate for, with a trigger heuristic and use-case description.

**Dispatch authorization is mode-dependent** (modes are defined by the autonomy-framework design, Component D; that spec lives in the repo that owns autonomy policy and may not be present in every consumer repo):

- **In-the-Loop mode:** dispatches at gated phases (2, 5, 7, 9) require approval; auto-dispatch elsewhere (1, 3, 4, 6, 8). Gates the *shared-state visibility* boundaries.
- **On-the-Loop mode:** dispatches at gated phases (2, 8, 9) require approval; auto-dispatch elsewhere (1, 3, 4, 5, 6, 7). Gates the *judgment* boundaries — the design going in and the review outcome coming out (re-anchored 2026-07-24).
- **Automated End-to-End mode:** full dispatch authority per the mode's gate matrix (only the Phase 9 merge gate remains)

> The five-mode framework (Hand-Held / Careful / Trusted / Weekly Deps / Overnight) was consolidated to these three on 2026-06-20. See [`/dev-lifecycle` SKILL.md](../dev-lifecycle/SKILL.md) Phase 1.5 for the canonical matrix and the deprecated-alias mapping.

> **Content status:** Each entry follows a 4-axis structure: Trigger / Use case / Example / Mode interaction. Always-dispatch entries omit the Trigger axis (they fire unconditionally subject to mode authorization). New entries require Repo Admin approval — see "Adding a new skill to the catalog" at the bottom of this file.

## Dynamic skill scoping (mandatory)

Every ingesting skill (`/issue-creation`, `/pr-creation`, `/dev-lifecycle`, `/enhanced-pr-review`) runs this algorithm at each of its phases. Consulting the catalog is **not optional**, and dispatch of a matched skill is **mandatory** — the selected mode controls *when the gate fires*, not *whether the skill runs*.

1. **Build the context signature** of the ingest:
   - **Labels** — `type:`, `area:`, `domain:`, `release:`, `risk:` on the issue/PR.
   - **File paths** — for a PR: `gh pr diff <N> --name-only`. For an issue (no diff yet): the predicted surfaces from scope analysis.
   - **Keywords** — issue/PR title + body.
   - **Type / surface** — the `type:` label and affected service area(s).
2. **Evaluate triggers.** For each catalog entry whose phase matches the current phase, test its `**Trigger:**` axes (Keywords / Labels / File paths / Other) against the signature, case-insensitively. Always-dispatch entries match unconditionally.
3. **Dispatch every match — mandatory.** A matched skill is *required*. The mode controls only the *timing of the gate*: a gated phase pauses for approval **before** running the matched skill; it does **not** drop the skill from the required set. This supersedes any older "evaluate triggers / skip silently" phrasing — a matched candidate is never silently skipped.
4. **Emit a visibility line** so dispatch is auditable: `Scoped skills (Phase N): [<skill> — matched on <trigger>; …]`.
5. **No silent skip.** If a matched skill is intentionally withheld, log the reason: `withheld: <skill> — <mode-gate pending | billed: awaiting preapproval | awaiting research consent | unknown-skill WARN>`. The unknown-skill case reuses the catalog-dispatch-resilience rule at the bottom of this file. (`awaiting research consent` is the `/deep-research` supervised-mode ASK — distinct from `mode-gate pending`, which is a shared-state visibility gate; deep-research gates the token/context-spend axis instead.)

Billed/cloud-cost skills (§ Billed / cloud-cost skills) are never auto-dispatched — they require explicit user preapproval in every mode. Design-class skills (§ Design-class sequencing) run *before* `/superpowers:brainstorming` within a phase's scoped set.

## Billed / cloud-cost skills (preapproval required)

Some skills incur real cost or run as cloud jobs. They are marked **`billed`** in their catalog entry and are governed by a preapproval gate that overrides normal mode authorization:

- A **`billed`** skill requires **explicit user preapproval before dispatch in every mode — `Automated End-to-End` included.** Preapproval is per-dispatch, not a standing mode grant. No mode auto-runs a billed skill.
- A skill additionally marked **`user-triggered`** cannot be dispatched by Claude at all (session-level skill governance forbids launching it). Claude surfaces the recommendation; the user runs it. (`user-triggered` ⊂ `billed`.)

The canonical member is `/code-review ultra <PR#>` — see its entry under "## Recommend-only / billed" below.

## Design-class sequencing

Catalog entries tagged **`design-class`** build out a design or plan (architecture, system design, frontend design, diagnosis). Within a phase's scoped set they run **before** `/superpowers:brainstorming`, which is reframed as the **synthesis sweep**: it integrates the design-class artifacts into the spec rather than being the sole design step.

`design-class` members (Phase 1–2): `/engineering:architecture`, `/engineering:system-design`, `/frontend-design:frontend-design`, `/product-management:write-spec`, `/engineering:testing-strategy`, `/design:design-handoff`, `/design:ux-copy`, `/engineering:documentation` (docs); (diagnosis, Phase 4/6) `/engineering:debug`, `/superpowers:systematic-debugging`. The tag is a marker — membership may grow as the catalog does.

## Phase 1: UNDERSTAND

### `/issue-brief` (always-dispatch)

**Use case:** Fetch a GitHub issue's title, body, all comments, labels, milestone, linked issues/PRs, and subissues into a structured brief. Eliminates the 5–10-minute manual `gh` archaeology that otherwise opens every issue-driven session.

**Example:** Every `/dev-lifecycle` Phase 1 invocation calls this skill. Concrete: an issue is understood via `/issue-brief <issue#>` at session start.

**Mode interaction:** Always-dispatch at Phase 1 in every mode. Read-only; no gate applies.

### `/engineering:architecture` (candidate)

**Trigger:**
- **Keywords in body/title:** `"ADR"`, `"architecture decision"`, `"design rationale"`, `"choose between"`, regex `/decide\s+(between|on)/i`
- **Labels:** `type: feature` combined with any `area:` label naming a backend service or architecturally significant component
- **Other:** issue body proposes a non-trivial technology choice or service-boundary decision

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Produce a structured ADR (Architecture Decision Record) for a non-trivial technology or design choice, with context, options considered, decision, and consequences. Use when the issue's resolution path requires committing to one of multiple credible technical alternatives.

**Example:** ADR-0002 "Actions-as-SoT deploys" — exactly this skill's shape: context, alternatives considered, decision, and consequences for the infrastructure choice of "GitHub Actions as the source of truth for production deploys." An ADR catalog under `docs/adr/` (in repos that keep one) shows the established ADR pattern.

**Mode interaction:** Auto-dispatch in all modes (Phase 1 is ungated in every mode under the consolidated matrix). Architecture decisions warrant explicit buy-in, but that gate lives at the Phase 2 PLAN boundary (gated in In-the-Loop), not at understand-time; the ADR is surfaced via the Phase 1 visibility line.

### `/operations:vendor-review` (candidate)

**Trigger:**
- **Keywords in body/title:** `"vendor"`, `"third-party SaaS"`, `"procurement"`, `"contract renewal"`, `"TCO"`, `"side-by-side comparison"`
- **Labels:** `area: ops` combined with `risk: security` or `risk: privacy`
- **Other:** issue proposes adopting a new external service, signing a contract, or comparing vendors

**Use case:** Evaluate a vendor proposal with cost analysis, risk assessment (security, privacy, supply chain), and a recommendation. Use when the project must commit to or renew a paid external dependency.

**Example (hypothetical):** Evaluating a third-party error-tracking SaaS as a Sentry replacement candidate. Would have applied to the Sentry-removal decision had it been entered through `/dev-lifecycle` rather than as a multi-issue ops-mode reversal.

**Mode interaction:** Auto-dispatch in all modes (Phase 1 is ungated). Output is advisory; no contract is signed without explicit user action, and the procurement decision is reviewed at the Phase 2 PLAN gate (gated in In-the-Loop).

### `/deep-research` (candidate)

**Trigger:**
- **Keywords in body/title:** `"research"`, `"compare options"`, `"prior art"`, `"survey"`, `"investigate approaches"`
- **Other:** the issue requires multi-source external research before a design can be committed; OR the user is in a multi-tiered Q&A research dive (layered follow-up questions on an external-fact topic).

**Use case:** Fan-out web research that adversarially verifies claims and synthesizes a cited report. Use at understand-time when the issue's resolution depends on external facts (library comparisons, standards, prior art) rather than codebase knowledge. Dispatched via the **Workflow tool** (`name: deep-research`) — it runs as a background dynamic workflow, not an inline subagent.

**Example (hypothetical):** Choosing between two WebRTC simulcast strategies — `/deep-research` gathers and cross-checks sources before `/engineering:architecture` writes the ADR.

**Mode interaction (mode-conditional — this entry is the EXCEPTION to "dispatch every match"; the supervised-mode ASK overrides phase-ungating, like the `billed` preapproval gate):**
- **Automated End-to-End → REQUIRED, automatic.** Auto-dispatch when triggered. No human is present to answer clarifying questions, so an autonomous deep-research pass substitutes for the Q&A a supervised session would get. Surface the cost estimate, then run.
- **In-the-Loop / On-the-Loop → opportunistic + ASK FIRST.** When the trigger fires — especially a multi-tiered Q&A research dive — OFFER deep-research with a one-line cost/scope estimate and **WAIT for an explicit go-ahead before dispatching.** Offer to narrow scope (which sub-questions to research) given the user is mid-Q&A.
- **The gated axis is token/context spend, NOT shared state.** "Phase 1 is ungated" governs *phase progression*, not authorization to launch a token-heavy fan-out; a read-only research workflow still spends a large budget. "NOT `billed` / runs locally" does **not** remove the consent requirement — the cost being gated is tokens and context, not dollars. A cost caution is necessary but **NOT sufficient** in supervised modes: obtain an explicit yes/no. Obvious research intent (`"compare options"`, an active Q&A) is **not** a substitute for that authorization.
- **Default fail-closed to ASK** when the mode is ambiguous or unset.

## Phase 2: PLAN

### `/superpowers:brainstorming` (always-dispatch)

**Use case:** Refine an idea into a written design spec through structured dialogue: explore project context, ask clarifying questions, propose alternatives, present a design, get user approval, write the spec to `docs/superpowers/specs/`. Mandatory before any creative work. **Runs as the synthesis sweep** — after any matched `design-class` skill (architecture, system design, frontend design, diagnosis) produces its artifact, brainstorming integrates those artifacts into the spec rather than being the sole design step (see § Design-class sequencing).

**Example:** This catalog content's spec (`docs/superpowers/specs/2026-05-22-1139-skill-catalog-content-design.md`, in the repo that owns it) was produced by this skill.

**Mode interaction:** Always-dispatch at Phase 2 when no `*-NNN-*-design.md` spec exists for the issue. Mode controls whether the brainstorming output's gate (HARD-GATE on design approval) waits for explicit user reply or accepts inline approval from prior context. In-the-Loop: waits explicitly. On-the-Loop / Automated End-to-End: accepts pre-approved synthesis if design was just confirmed.

### `/superpowers:writing-plans` (always-dispatch)

**Use case:** Produce an executable task-by-task plan from an approved spec, saved to `docs/superpowers/plans/`. Plan contains actual content (file paths, code snippets, commands) — no placeholders. Required input for `/superpowers:subagent-driven-development` or `/superpowers:executing-plans`.

**Example:** The plan `docs/superpowers/plans/2026-05-22-1139-skill-catalog-content.md` (in the repo that owns it) was produced by this skill.

**Mode interaction:** Always-dispatch at Phase 2 after brainstorming produces a spec (or when the spec already exists). Read-only of the spec; write-only of the plan file.

### `/product-management:write-spec` (candidate)

**Trigger:**
- **Keywords in body/title:** `"PRD"`, `"product spec"`, `"feature spec"`, `"requirements doc"`
- **Labels:** `type: feature` combined with `area: marketing`, `domain: ux`, or absence of code-area labels
- **Other:** issue origin is product/marketing rather than engineering; user is acting in PM capacity

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Produce a PM-style product spec (goal, users, success metrics, scope, non-goals) — an alternative to engineering-focused `/superpowers:brainstorming` when the artifact's audience is product/business rather than engineering.

**Example (hypothetical):** Writing a spec for a new pricing-tier UX flow before any engineering work begins. Would precede `/superpowers:brainstorming` in the engineering phase that follows.

**Mode interaction:** Gated in In-the-Loop. Auto-dispatch in On-the-Loop / Automated End-to-End when triggers fire. Often co-dispatched with `/product-management:product-brainstorming` upstream.

### `/engineering:system-design` (candidate)

**Trigger:**
- **Keywords in body/title:** `"system design"`, `"how should we architect"`, `"service boundaries"`, `"API design"`, `"data model"`
- **Labels:** `type: feature` combined with at least two distinct `area:` labels spanning separate services or surfaces
- **Other:** issue touches ≥2 services or introduces a new persistent data model

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Design multi-service systems and architectures — API contracts, data modeling, service boundaries, failure-mode handling. Use when the work crosses service boundaries or introduces persistence.

**Example:** The autonomy framework spec (`2026-05-21-1109-autonomy-framework-design.md`) is a multi-component system design across hooks, skills, and CLI surfaces — exactly this skill's shape.

**Mode interaction:** Gated in In-the-Loop. Auto-dispatch in On-the-Loop / Automated End-to-End when triggers fire. Often co-dispatched with `/superpowers:brainstorming` (architecture-led brainstorm).

### `/product-management:product-brainstorming` (candidate)

**Trigger:**
- **Keywords in body/title:** `"explore problem"`, `"user need"`, `"market opportunity"`, `"stress-test the idea"`
- **Labels:** `area: marketing` OR `area: ops` combined with `type: feature`
- **Other:** early-stage exploration before commitment to a solution shape

**Use case:** Explore a product idea, problem space, or strategic question with a thinking-partner — an alternative to engineering `/superpowers:brainstorming` when the question is "what should we build" rather than "how should we build it".

**Example (hypothetical):** Brainstorming what monetization tier structure makes sense before any engineering work begins on the licensing-authority service.

**Mode interaction:** Gated in In-the-Loop. Auto in On-the-Loop / Automated End-to-End. Distinct from `/superpowers:brainstorming`, which terminates in an engineering spec; this skill terminates in product direction.

### `/frontend-design:frontend-design` (candidate)

**Trigger:**
- **Keywords in body/title:** `"UI"`, `"new component"`, `"landing page"`, `"visual polish"`
- **Labels:** the repo's frontend `area:` label combined with `domain: ux` or `domain: accessibility`
- **File paths in diff:** <frontend files> for new components, or paths matching the repo's stylesheet directory

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Create distinctive, production-grade frontend interfaces with high design quality — avoids the generic look that AI-authored UIs default to. Use when a new component or page needs visual identity beyond functional correctness.

**Example (hypothetical):** Designing the visual layout of a new "Server Settings" page from scratch, where the catalog's existing patterns don't dictate a specific shape.

**Mode interaction:** Gated at Phase 2 in In-the-Loop. Auto in On-the-Loop / Automated End-to-End. Also surfaces at Phase 3 as a candidate when execution reveals a new component needs design treatment beyond what the plan anticipated (Phase 3 is ungated in all modes).

### `/design:design-handoff` (candidate)

**Trigger:**
- **Keywords in body/title:** `"design handoff"`, `"engineering spec"`, `"Figma to code"`, `"spec sheet"`
- **Labels:** the repo's frontend `area:` label combined with `domain: ux`
- **Other:** a finished design exists (Figma, image, prototype) and needs translation to code-level specs

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Generate developer handoff specs from a design — layout, design tokens, component props, interaction states, responsive breakpoints, edge cases. Bridge between visual design and implementation.

**Example (hypothetical):** A Figma mockup for the new account-deletion confirmation flow being converted into a developer ticket with measurements, token references, and state transitions.

**Mode interaction:** Gated in In-the-Loop (handoff format is stakeholder-facing). Auto in On-the-Loop / Automated End-to-End.

### `/design:ux-copy` (candidate)

**Trigger:**
- **Keywords in body/title:** `"copy"`, `"microcopy"`, `"button text"`, `"error message"`, `"CTA"`, `"empty state"`
- **Labels:** the repo's frontend `area:` label combined with `domain: ux`
- **File paths in diff:** <frontend files> containing user-facing strings

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Write or review UX copy — microcopy, error messages, empty states, CTAs, onboarding text. Use when the wording of user-facing strings is non-trivial or when localizing tone.

**Example (hypothetical):** Writing the wording for the account-erasure confirmation dialog, where "Delete Account" vs "Erase Account" vs "Permanently remove your account" each carry different implications.

**Mode interaction:** Gated in In-the-Loop. Auto in On-the-Loop / Automated End-to-End. Often co-dispatched with `/design:accessibility-review` (screen-reader copy).

### `/design:accessibility-review` (candidate)

**Trigger:**
- **Keywords in body/title:** `"WCAG"`, `"a11y"`, `"keyboard navigation"`, `"screen reader"`, `"contrast"`, `"ARIA"`
- **Labels:** `domain: accessibility`
- **File paths in diff:** <frontend files> for new interactive elements

**Use case:** Run a WCAG 2.1 AA audit on a design or page — color contrast, keyboard navigation, touch-target size, screen-reader behavior. Use before user-facing release of new UI surfaces.

**Example (hypothetical):** An a11y audit on the new emoji-picker component before merging — ensuring keyboard arrow navigation, ARIA labels, and contrast on hovered states.

**Mode interaction:** Gated at Phase 2 in In-the-Loop. Auto at Phase 8 in all modes when label `domain: accessibility` is on the PR OR when new <frontend files> with interactive elements are in the diff (treated as a release-block check; Phase 8 is ungated under the consolidated matrix).

### `/engineering:testing-strategy` (candidate)

**Trigger:**
- **Keywords in body/title:** `"test strategy"`, `"test plan"`, `"what to test"`, `"coverage gaps"`
- **Labels:** `domain: testing-qa`
- **File paths in diff:** new source files without companion test files (e.g., new `*.ts`/`*.go` without `*.test.ts`/`*_test.go`)

**Tags:** `design-class` (runs before `/superpowers:brainstorming` in the scoped set — see § Design-class sequencing).

**Use case:** Plan tests for a new feature with the right balance of unit/integration/E2E. Use when introducing a substantial module or when validation reveals coverage gaps that the existing `test-writer` agent can't reason through alone.

**Example (hypothetical):** Planning tests for a new state store (the repo's state-management library) with optimistic-update logic — what to unit-test vs what to cover via component/integration tests, and what belongs in an E2E.

**Mode interaction:** Gated at Phase 2 in In-the-Loop. Auto at Phase 4 in all modes when the repo's code-quality gate (if it ships one — see § Repo-local skills) reports a new-code-coverage threshold miss (Phase 4 is ungated under the consolidated matrix).

### `/engineering:tech-debt` (candidate)

**Trigger:**
- **Keywords in body/title:** `"tech debt"`, `"cleanup"`, `"refactor for clarity"`, `"this got messy"`
- **Labels:** `type: chore` combined with `domain: developer-experience`
- **Other:** explicit refactor PR adjacent to feature work

**Use case:** Surface and prioritize technical-debt items affecting the current work — distinct from out-of-scope cleanup. Identifies debt that's blocking or amplifying the current change.

**Example (hypothetical):** Recognizing during a `/dev-lifecycle` Phase 2 plan that the file being modified has grown to 1200 lines and that a focused split is in-scope for the current change (per the writing-plans skill's guidance).

**Mode interaction:** Gated in In-the-Loop. Auto in On-the-Loop / Automated End-to-End. Output is advisory; no refactor happens without explicit acknowledgment in the plan.

### `/engineering:documentation` (candidate)

**Trigger:**
- **Keywords in body/title:** `"document"`, `"docs"`, `"runbook"`, `"ADR"`, `"write up"`
- **Labels:** `area: docs`
- **File paths in diff:** `docs/**`, `*.md`, `.claude/rules/**`

**Tags:** `design-class` (docs).

**Use case:** Author or substantially revise documentation as a first-class deliverable. Directly serves the docs-in-PR invariant — when a change needs a doc written or updated, this skill writes it *in the PR* rather than deferring it. Pairs with `/anthropic-skills:doc-coauthoring` for structured long-form docs.

**Example (hypothetical):** A PR that adds a new env var also needs an entry in the declared-schema env example file + a runbook note; this skill produces them in-PR.

**Mode interaction:** Gated in In-the-Loop. Auto in On-the-Loop / Automated End-to-End when triggers fire.

### `/anthropic-skills:doc-coauthoring` (candidate)

**Trigger:**
- **Keywords in body/title:** `"proposal"`, `"spec"`, `"design doc"`, `"documentation"`, `"decision doc"`
- **Labels:** `area: docs`

**Use case:** Structured co-authoring of long-form documentation (proposals, technical specs, decision docs) through guided context transfer and iteration. Complements `/engineering:documentation` when the doc is substantial enough to warrant a co-authoring workflow — and, like it, lands the doc in the PR rather than deferring it.

**Example (hypothetical):** Drafting a multi-section migration-strategy doc that ships alongside the migration PR.

**Mode interaction:** Gated in In-the-Loop. Auto in On-the-Loop / Automated End-to-End when triggers fire.

### `/cloudflare:wrangler` (candidate)

**Trigger:**
- **File paths in diff:** `src/frontend/functions/**` (adjust to your layout), `**/wrangler.toml`, `infrastructure/deploy/deploy-spa.sh`, any Cloudflare Worker source
- **Keywords:** `cloudflare`, `wrangler`, `pages`, `worker`, `kv namespace`, `deploy-spa`

**Use case:** Current Wrangler CLI/config contract for Pages and Workers work — your deploy CLI reference whenever the repo's static/edge deploy relies on Cloudflare Pages or Workers. The `--cwd` discovery foot-gun is a known source of silent Function drops.

**Example:** A change to the `/assets/*` caching Function — fetch the current Pages Functions + wrangler contract before editing.

**Mode interaction:** Auto-dispatch when triggers fire (subject to the phase gate).

### `/cloudflare:workers-best-practices` (candidate)

**Trigger:**
- **File paths in diff:** Cloudflare Worker source (`**/worker*.{js,ts}`, `src/frontend/functions/**` — adjust to your layout)
- **Keywords:** `worker`, `durable object`, `edge`, `cf pages function`

**Use case:** Edge-runtime idioms and limits for Worker code — request lifecycle, caching semantics, what does not exist at the edge.

**Example:** Adding a route to the invite-landing Worker.

**Mode interaction:** Auto-dispatch when triggers fire.

### `/resend:resend` (candidate)

**Trigger:**
- **File paths in diff:** `**/email/**`, `**/mail*.go`, SMTP/`RESEND_*` config surfaces
- **Keywords:** `email`, `smtp`, `resend`, `verification email`, `password reset email`

**Use case:** Current Resend API contract — your email provider reference whenever the repo's transactional mail runs through Resend (e.g. an `RESEND_API_KEY` provisioned via a secrets manager); its API is past most training cutoffs.

**Example:** Changing the account-verification mail template or send path.

**Mode interaction:** Auto-dispatch when triggers fire.

### `/resend:email-best-practices` (candidate)

**Trigger:**
- **Keywords:** `deliverability`, `spam`, `dmarc`, `spf`, `dkim`, `email template`, `bounce`
- **Other:** the change alters what an outbound email contains or when it is sent

**Use case:** Deliverability and template discipline — the failure mode is silent (mail lands in spam), so it is worth front-loading rather than discovering from user reports.

**Example:** Adding a new notification email class.

**Mode interaction:** Auto-dispatch when triggers fire.

### `/design:design-system` (candidate · design-class)

**Trigger:**
- **File paths in diff:** `src/frontend/styles/**` (adjust to your layout), any `*.css` introducing a new custom property
- **Keywords:** `design token`, `theme`, `color scheme`, `typography scale`, `design system`

**Use case:** Token/system-level design work — a repo's design tokens. When the repo maintains many theme blocks (multiple schemes x dark/light), a new token must resolve in ALL of them, which is a systems problem, not a component problem.

**Example:** Introducing a semantic color token for a new surface class.

**Mode interaction:** Auto-dispatch when triggers fire. Runs BEFORE `/superpowers:brainstorming` per design-class sequencing.

## Phase 3: EXECUTE

### `/superpowers:subagent-driven-development` OR `/superpowers:executing-plans` (always-dispatch, per the Implementation Approach selected at Phase 1.5)

**Use case:** Execute an approved plan task-by-task. Under `Inline`, `executing-plans` runs the whole plan in-session with batch checkpoints (lower cost, less isolation). Under `Subagent Driven`, `subagent-driven-development` dispatches a fresh subagent per task with two-stage review (high isolation, higher token cost). Under `Dynamic`, `executing-plans` is the session spine and the orchestrator decides inline-vs-subagent **per task** against the D1–D5 criteria in `/dev-lifecycle` Phase 3, sized by the tier profile's batch axis (`_shared/model-capability-tiers.md`); subagent-decided tasks follow the `subagent-driven-development` per-task contract, and every decision emits an auditable visibility line. On the `generic` tier, Dynamic degrades to a plan-time static split.

**Example:** A docs-only issue's execution went inline via `/superpowers:executing-plans` because Phase 1.5 selected `Inline` for that lift.

**Mode interaction:** Always-dispatch at Phase 3 once a plan exists. All modes auto-progress through plan completion — Phase 3 is ungated in every mode under the consolidated matrix (the former `Hand-Held` per-task gate was retired on 2026-06-20). The Phase 1.5 Implementation Approach (with tier-aware defaults per `_shared/model-capability-tiers.md`) determines which skill(s) fire and at what granularity. Tier never alters the gate matrix (see the tier catalog's "Invariants tiers may not alter").

### `/superpowers:test-driven-development` (candidate)

**Trigger:**
- **Keywords in body/title:** `"new function"`, `"new endpoint"`, `"new component"`, `"add capability"`
- **Labels:** `type: feature` combined with any code `area:` label (a backend service, frontend surface, or similar)
- **Other:** plan introduces new behavior (excludes pure refactors, doc edits, config bumps)

**Use case:** Write a failing test first, then write minimal implementation to make it pass, then refactor. Use for new behavior — function, endpoint, component, store action. Doesn't apply to docs or config-only changes.

**Example (hypothetical):** A new <backend service> endpoint requiring happy-path + error-path tests. Both tests are written and fail before the handler implementation lands; the handler then brings them to green.

**Mode interaction:** Auto-dispatch in all modes (Phase 3 is ungated). The TDD discipline itself is rigid; the skill doesn't soften when modes loosen.

### `/simplify` (candidate, maps to `pr-review-toolkit:code-simplifier`)

**Trigger:**
- **Keywords in body/title:** `"simplify"`, `"clean up"`, `"reduce complexity"`, regex `/cyclomatic/i`
- **Labels:** `domain: developer-experience`
- **Other:** a code-reviewer agent flagged a function as overly complex (CC > 10) in a prior review pass

**Use case:** Simplify recently-modified code for clarity, consistency, and maintainability while preserving all functionality. The short `/simplify` alias maps to `pr-review-toolkit:code-simplifier`. Use after writing a logical chunk of code or after a fix that added several conditional branches.

**Example (hypothetical):** After a bug-fix adds three null-checks to the same reducer, `/simplify` collapses them into a single early-return or extracts a guard helper.

**Mode interaction:** Auto-dispatch in all modes (Phase 3 is ungated). Co-firing with `/superpowers:test-driven-development` is expected — TDD writes the test, simplifier may suggest refactor of the implementation. Output is a suggested refactor — landing still requires explicit acknowledgment.

### `/frontend-design:frontend-design` (candidate, also in Phase 2)

**Use case:** Same as the Phase 2 entry — execution-time fallback when a new component emerges that the plan didn't anticipate at Phase 2. See the Phase 2 entry for full triggers and examples.

**Mode interaction:** Auto-dispatch in all modes (Phase 3 is ungated); a mid-execution design dispatch is surfaced via the Phase 3 visibility line so the developer can confirm the change is in scope.

### `/superpowers:dispatching-parallel-agents` (candidate)

**Trigger:**
- **Other:** the plan (or one task) is a wide independent sweep — the D6 breadth rung — or two or more plan tasks are dispatched in parallel

**Use case:** Discipline for fanning work out across agents: prompt shape, isolation, and how to review returns. Directly governs the named-agent fan-out in `lifecycle-agent-dispatch.md` Phase 3.

**Example:** A migration touching 30 call sites, split across parallel `@backend-developer` dispatches.

**Mode interaction:** Auto-dispatch when D1/D6 selects a parallel or Workflow path.

### `/operations:runbook` (candidate)

**Trigger:**
- **File paths in diff:** `infrastructure/**`, `.github/workflows/**`, `docker-compose*.yml`, `services/*/Dockerfile`
- **Labels:** `area: ops`, `area: ci-cd`
- **Other:** the change introduces an operator-facing procedure (activation, rollback, refresh, incident response)

**Use case:** Authoring an operator runbook. Ship runbooks WITH infra changes (e.g. under `docs/runbooks/`); this skill supplies the structure so a procedure written under deadline is still followable at 3am.

**Example:** A new guarded activation sequence needs its rollback procedure documented.

**Mode interaction:** Auto-dispatch when triggers fire.

## Phase 4: VALIDATE

### `/superpowers:verification-before-completion` (always-dispatch)

**Use case:** Run concrete verification commands (tests, lint, build, manual checks) and confirm their output BEFORE claiming work complete. Enforces "evidence before assertions" — no "should work" claims without proof.

**Example:** Every Phase 4 invocation. Before this catalog content lands, this skill verifies the file parses as markdown, has no broken internal links, and matches the structure documented in the parent spec.

**Mode interaction:** Always-dispatch at Phase 4 in all modes. The "no fix is complete without verification" rule is mode-independent.

> If the repo ships a code-quality-gate skill (e.g. a SonarCloud/SonarQube gate predictor — see § Repo-local skills), it is also an always-dispatch Phase 4 check. It is listed in the repo-local section because not every repo has such a gate.

### `/engineering:debug` (candidate)

**Trigger:**
- **Keywords in body/title:** `"error"`, `"stack trace"`, `"works in staging but not prod"`, `"unexpected behavior"`
- **Labels:** `type: bug`
- **Other:** test failure surfaced during Phase 4 validation, or first appearance of an unexplained error

**Tags:** `design-class` (diagnosis; produces a root-cause artifact the fix consumes — see § Design-class sequencing).

**Use case:** Structured debugging — reproduce locally, isolate the failing path, diagnose root cause, propose fix. Use when behavior diverges from expected and the cause is not immediately obvious.

**Example:** A relative-URL fix in a custom protocol handler — a `net::ERR_UNEXPECTED` error needed root-cause analysis traversing the custom URL scheme + WHATWG URL behavior before the fix path was clear.

**Mode interaction:** Auto-dispatch in all modes (Phase 4 is ungated) when a single test failure is the trigger; debugging is exploratory, so the dispatch is surfaced via the visibility line.

### `/superpowers:systematic-debugging` (candidate)

**Trigger:**
- **Keywords in body/title:** `"intermittent"`, `"can't reproduce"`, `"multiple debug attempts failed"`
- **Other:** `/engineering:debug` already ran in this session without resolving the issue; OR the bug spec flags "complex" / "multi-cycle root cause"

**Tags:** `design-class` (diagnosis; produces a root-cause artifact the fix consumes — see § Design-class sequencing).

**Use case:** Process-driven debugging — hypothesis generation, reproduction harness, bisection, narrowing the search space. Use when single-pass debugging hasn't yielded a root cause.

**Example:** A stranded-version-tag bug (versions bumped on the main branch but never tagged) required multi-cycle root-cause analysis traversing GitHub Actions transitive-skip semantics; exactly this skill's shape.

**Mode interaction:** Auto-dispatch in all modes once `/engineering:debug` has run unsuccessfully in the same session; otherwise dispatched per the standard Phase 4 / Phase 6 candidate rule (both phases are ungated under the consolidated matrix).

### `/engineering:testing-strategy` (candidate)

**Use case:** Same as the Phase 2 entry — Phase 4 cross-dispatch when the repo's coverage check (e.g. a code-quality-gate skill, if the repo ships one — see § Repo-local skills) reports a coverage threshold miss. See the Phase 2 entry for full triggers.

**Mode interaction:** Auto-dispatch in all modes at Phase 4 when coverage is below the 80% threshold for new code (Phase 4 is ungated). Output is a remediation plan; user approves before any test files land.

### `/verify` (candidate)

**Trigger:**
- **Labels:** `type: feature` or `type: bug` with a runnable user-facing surface
- **Other:** the change's correctness is best confirmed by running the app and observing behavior, not just unit tests

**Use case:** Run the app and observe real behavior to confirm a change does what it should. Complements `/superpowers:verification-before-completion` (which runs commands) with actual app-driven confirmation. Also a Phase 8 candidate when a reviewer wants runtime evidence.

**Example (hypothetical):** A new message-pinning UI flow — `/verify` drives the app to confirm the pin renders and persists.

**Mode interaction:** Auto-dispatch in all modes when triggers fire (Phase 4 is ungated under the consolidated matrix).

### `/run` (candidate)

**Trigger:**
- **Other:** a phase needs the app launched (to screenshot, to confirm a change works in the real app, or as a precondition for `/verify`)

**Use case:** Launch and drive this project's app to see a change working. Prefers a project-specific launch skill if one exists; otherwise falls back to built-in Electron / server patterns. Also a Phase 8 candidate. Distinct from `/verify`: `/run` launches; `/verify` launches *and judges* behavior against intent.

**Example (hypothetical):** Bringing up the desktop client to screenshot a new settings page for the PR description.

**Mode interaction:** Auto-dispatch in all modes when triggers fire (Phase 4 is ungated under the consolidated matrix).

### `/sonarqube:sonar-quality-gate` (candidate)

**Trigger:**
- **Other:** always, once a PR exists; before a PR, whenever the branch is ready for validation

**Use case:** Reads the SonarCloud Quality Gate directly via MCP. The QG is a MANDATORY merge gate; `/verify-quality-gate` runs the local check set, this reads the authoritative server verdict.

**Example:** Confirming the >=80% new-code condition before flipping ready.

**Mode interaction:** Auto-dispatch (Phase 4 is ungated).

### `/sonarqube:sonar-coverage` (candidate)

**Trigger:**
- **Other:** new-code coverage is below the 80% gate, or the diff adds a source file with no matching test file

**Use case:** Identifies the exact uncovered lines rather than guessing. The >=80% new-code condition is the most common Phase-4 failure; pair with `@test-writer` to close the gap.

**Example:** A new Go handler lands at 62% new-code coverage.

**Mode interaction:** Auto-dispatch when the gate is red.

## Phase 5: COMMIT+DRAFT PR

### `/commit-commands:commit-push-pr` (always-dispatch when mode authorizes)

**Use case:** Orchestrated stage + commit + push + `gh pr create --draft` in one flow. Confirms changes, writes a conventional-commit message, pushes the branch, and opens a draft PR with body templated from the spec/plan.

**Example:** Every Phase 5 success path. The commit-push-pr boundary is the first visibility moment for the change — once pushed, the diff exists on GitHub and bot reviewers see it (on draft, only some run).

**Mode interaction:** Gated in In-the-Loop and On-the-Loop (PR push is a shared-state visibility boundary). Auto in Automated End-to-End. The mode matrix lives in [`/dev-lifecycle` SKILL.md](../dev-lifecycle/SKILL.md) Phase 1.5.

### `/pr-creation` (always-dispatch after `gh pr create` succeeds)

**Use case:** Sync PR metadata with associated issue(s): inherit labels via the `type:`/`area:`/`domain:`/`release:`/`risk:` namespaces (filtering out `routine:` and `type: epic`), move the project board's Status to "In review" for `Closes #N` references, auto-assign the gh-authenticated user. Lightweight mirror of `/issue-creation` for PRs.

**Example:** Every Phase 5 invocation after `gh pr create` returns a PR number. See [`pr-creation` SKILL.md](../pr-creation/SKILL.md) for the full lifecycle.

**Mode interaction:** Always-dispatch in all modes — metadata-only operation, preapproved (no separate gate even in In-the-Loop). Phase 5 of `/pr-creation` is advisory and WARN-level only; it does not block.

## Phase 6: CI MONITOR

### `/ci-status` (always-dispatch)

**Use case:** Poll the CI check status for the open PR, summarize pass/fail/pending, suggest remediation when something fails, and offer to mark Ready when green. Optionally loop via `/loop 2m /ci-status <PR>` for hands-off polling.

**Example:** Every Phase 6 invocation. For a pure-docs PR the relevant checks are pre-commit hooks (already passed locally), Semgrep SAST (no-op for `.md`), and the static-analysis gate (filters non-source files).

**Mode interaction:** Always-dispatch at Phase 6. All modes auto-progress once CI is green — Phase 6 is ungated under the consolidated matrix; `In-the-Loop` next pauses at the Phase 7 READY checkpoint, `On-the-Loop` / `Automated End-to-End` continue.

### `/engineering:debug` (candidate, CI test failure)

**Use case:** Same as the Phase 4 entry — Phase 6 cross-dispatch when CI reports a test failure that didn't surface locally. See the Phase 4 entry for full triggers.

**Mode interaction:** Auto-dispatch in all modes when a CI test failure occurs (Phase 6 is ungated under the consolidated matrix).

### `/superpowers:systematic-debugging` (candidate, CI test failure)

**Use case:** Same as the Phase 4 entry — Phase 6 cross-dispatch when CI failure is reproducible only on CI infra (cache-cold builds, ephemeral container environments). See the Phase 4 entry for full triggers.

**Mode interaction:** Auto-dispatch in all modes when `/engineering:debug` has run unsuccessfully against the same CI failure (Phase 6 is ungated under the consolidated matrix).

### `/reproduce-then-fix` (candidate, CI test failure with reproducible bug)

**Trigger:**
- **Keywords in body/title:** none — fires on CI signal, not text
- **Other:** CI test failure that maps to a reproducible bug (failing test names a specific code path; not an infrastructure flake)

**Use case:** Test-first bug fixing — write a failing reproduction test first, then iterate hypothesis → fix → green inside a bounded retry budget. Replaces ad-hoc Phase 6 debug-and-patch when CI surfaces a real bug introduced by the PR.

**Example (hypothetical):** CI reports `TestUserCreate_DuplicateEmail` failing on the PR. `/reproduce-then-fix` runs the failing test locally to confirm reproduction, then iterates fix candidates until the test passes. The reproduction test ensures the fix actually closes the bug class, not just a surface symptom.

**Mode interaction:** Gated for entry into the reproduce-then-fix flow (it's a different lifecycle shape than `/dev-lifecycle`). Auto-progression once the reproduction test exists.

## Phase 7: READY

No skills dispatched — `gh pr ready` is a single command.

## Phase 8: FINAL REVIEW

### `/enhanced-pr-review` (always-dispatch)

**Use case:** Reconcile findings from whichever AI reviewers the repo has configured (e.g. Copilot, a static-analysis gate, Semgrep SAST, a release classifier) with project-specific subagent reviews (security-reviewer, code-reviewer, docs-reviewer, file-pattern specialists) into a unified triage table. Surfaces version-bump recommendations from a release classifier when present (high miss rate otherwise). Internally dispatches `/review-pr` for the subagent suite.

**Example:** Every Phase 8 invocation. See [`enhanced-pr-review` SKILL.md](../enhanced-pr-review/SKILL.md) for the full reconciliation logic.

**Non-deferrable findings (per enhanced-pr-review §2.5.0):** documentation findings the PR makes stale, and any finding in a file the PR modifies (the no-rot rule), are categorically non-deferrable — Fix-in-PR. The only escape for a disproportionately large preexisting fix is a lockstep companion PR with explicit developer approval (§2.5.6), never a follow-up issue.

**Mode interaction:** Always-dispatch at Phase 8. The triage table presentation is read-only; the "apply fixes" sub-checkpoint auto-applies non-conflict findings in all modes (Phase 8 is ungated under the consolidated matrix). Conflicts (findings needing a human judgment call) always surface for an explicit decision regardless of mode, and the Phase 9 merge gate is the universal floor.

### `/superpowers:receiving-code-review` (candidate)

**Trigger:**
- **Other:** incoming review feedback that is unclear, technically questionable, or pushes back on a deliberate decision documented in the spec/plan
- **Labels:** none — fires on review-content shape

**Use case:** Rigorously evaluate review feedback before implementing — verification not performative agreement. Use when a Copilot or human reviewer flags something that conflicts with an explicit design decision, or when the feedback's premise looks wrong.

**Example (hypothetical):** Copilot suggests "extract this into a helper" on a function that the spec deliberately keeps inline for locality; this skill defends the deliberate decision and replies with the rationale instead of agreeing reflexively.

**Mode interaction:** Auto-dispatch in all modes (Phase 8 is ungated under the consolidated matrix). The "no blind agreement" discipline is a behavioral property of the skill, not a phase gate — genuine human-judgment conflicts surface for an explicit decision, the same way `/enhanced-pr-review` handles them.

### `/design:design-critique` (candidate)

**Trigger:**
- **Labels:** `domain: ux`
- **File paths in diff:** <frontend files> for new components, or substantial changes (>50 LOC) to existing component files

**Use case:** Structured design feedback on usability, hierarchy, and consistency at PR review time. Use when a UI change is non-trivial and benefits from a focused design pass beyond what `/enhanced-pr-review` surfaces.

**Example (hypothetical):** A new modal layout for the server-settings flow being reviewed for visual hierarchy and primary-action prominence before merging.

**Mode interaction:** Auto-dispatch in all modes when triggers fire (Phase 8 is ungated under the consolidated matrix).

### `/design:accessibility-review` (candidate)

**Use case:** Same as the Phase 2 entry — Phase 8 dispatch when the PR includes new interactive UI elements regardless of whether Phase 2 audited them. See the Phase 2 entry for triggers and examples.

**Mode interaction:** Auto-dispatch in all modes at Phase 8 when label `domain: accessibility` is on the PR OR when new <frontend files> with interactive elements are in the diff (Phase 8 is ungated under the consolidated matrix).

### `/simplify` (candidate)

**Use case:** Same as the Phase 3 entry — Phase 8 dispatch when a reviewer (subagent or bot) flags a function as overly complex. See the Phase 3 entry for triggers.

**Mode interaction:** Auto-dispatch in all modes (Phase 8 is ungated under the consolidated matrix). Output is a suggested refactor — landing requires explicit user buy-in even when auto-dispatched.

### `/code-review ultra` (billed + user-triggered — see "Recommend-only / billed" section below)

### `/security-review` (candidate)

**Trigger:**
- **Labels:** `risk: security`, or any `domain:` label naming a security-sensitive surface (e.g. encryption, authorization)
- **File paths in diff:** the repo's auth/authz code, IPC/trust-boundary surfaces, origin/CORS gating, anything under `**/crypto/**` or matching encryption-related paths
- **Other:** PR body names an auth/crypto/RBAC surface

**Use case:** Run a security review of the pending branch changes against OWASP Top 10 / CWE Top 25 / the repo's security policy. Complements `/review-pr`'s `security-reviewer` subagent with a focused, branch-level pass when the diff is security-dense.

**Example (hypothetical):** A PR touching a token-manager + the refresh-token rotation path — `/security-review` runs a dedicated pass before merge.

**Mode interaction:** Auto-dispatch in all modes when triggers fire (Phase 8 is ungated under the consolidated matrix; the dispatch is read-only). Surfaces findings into the Phase 8 reconciliation table.

### `/engineering:code-review` (candidate)

**Trigger:**
- **Keywords in body/title:** `"review"`, `"is this safe"`, `"check before merge"`
- **Other:** PR diff present; a focused general-engineering review lens is wanted beyond the subagent suite

**Use case:** Review code changes for security, performance, and correctness (N+1 queries, injection, missing edge cases, error handling). Complements `/review-pr`'s project-side subagents with a general engineering-review lens.

**Example (hypothetical):** A <backend service> PR with new query-building logic — `/engineering:code-review` checks for parameterization and N+1 patterns.

**Mode interaction:** Auto-dispatch in all modes when triggers fire (Phase 8 is ungated under the consolidated matrix).

### `/sonarqube:sonar-fix-issue` (candidate)

**Trigger:**
- **Other:** SonarCloud reported new issues on this PR, or a finding needs a FALSE_POSITIVE transition

**Use case:** Applies a Sonar finding's fix, and — critically — encodes the two-step `reopen` -> `falsepositive` transition SonarCloud's state machine requires for an ACCEPTED/RESOLVED issue. Aborting between those two calls leaves the finding open and regresses the Quality Gate. Every FP decision still requires an entry in `docs/ci/sonarqube-false-positive-register.md`.

**Example:** A Go idiom flagged by a mis-calibrated rule needs an instance-level FP mark.

**Mode interaction:** Auto-dispatch when Sonar findings are present. NEVER use it to suppress or tune a rule — AI-Code Assurance forbids rule-level suppression; instance-level FP only.

### `/ponytail:ponytail-review` (candidate)

**Trigger:**
- **Other:** always, as the over-engineering lens on the diff
- **Keywords in the diff:** a new abstraction with one implementation, a new config value that never varies, a new dependency, scaffolding for a future requirement

**Use case:** Reviews the diff for over-engineering: code that did not need to exist, an abstraction with one caller, a dependency added for what a few lines would do. This is a DIFFERENT lens from `/simplify`, which refines code that should exist — ponytail asks whether it should exist at all. Complements `@scope-reviewer`, which applies the same lens against the issue's stated scope.

**Example:** A PR adds a factory, an interface, and a config flag to support one call site.

**Mode interaction:** Auto-dispatch at Phase 8. Advisory: its findings are recommendations, not blocking — a deliberate simplification refusal is a valid answer and should be recorded as such.

## Phase 9: MERGE-READY

### `gh pr merge --squash` (the merge command itself)

**Use case:** Squash-merge the PR with a conventional-commit subject. Not a skill, but the terminal action of `/dev-lifecycle`. Includes the pre-merge thread-resolution gate (programmatic check of unresolved bot review threads via GraphQL `reviewThreads`).

**Example:** Every `/dev-lifecycle` Phase 9 invocation, after the user explicitly approves.

**Mode interaction:** ALWAYS gated regardless of mode. The merge gate is the universal floor — explicit user approval is required even in `Automated End-to-End`. No mode can auto-merge.

### `/release-notes` (candidate, when version tag will be cut)

**Trigger:**
- **Labels:** `risk: release-gate` on the PR
- **Other:** the PR includes a version-bump commit (`chore: bump version to vX.Y.Z`); OR the merge will cut a milestone-completing tag

**Use case:** Draft GitHub release notes for a version tag by walking git log since the previous tag, grouping commits by Conventional Commit type, and cross-referencing closed issues. Includes a "Docs that may need updating" section.

**Example (hypothetical):** Cutting a new milestone-completing version tag. Every published tag would go through this skill.

**Mode interaction:** Gated in all modes. Release notes are an author-facing artifact — user reviews the draft before the tag is pushed.

### `/engineering:deploy-checklist` (candidate, when version tag will be cut)

**Trigger:**
- **Labels:** `risk: release-gate` combined with `area: ci-cd` or `area: ops`
- **File paths in diff:** changes under the repo's deploy/infra directory, its CD workflow(s) in `.github/workflows/`, or its production container/compose files

**Use case:** Pre-deploy checklist — verify env vars set, rollback plan documented, comms drafted, post-deploy verification steps known. Use when the PR's merge will trigger a production deploy.

**Example (hypothetical):** A <backend service> PR that adds a new env var and consuming logic; the checklist verifies the secret-provisioning workflow updates have landed before merge.

**Mode interaction:** Gated in all modes (production-impact). The checklist is the developer's preflight; auto-progression after checklist completion only in On-the-Loop / Automated End-to-End.

## Cross-cutting (not phase-aligned)

### `/reproduce-then-fix`

Replaces `/dev-lifecycle` for bug-fix entry. Test-first ordering: failing reproduction test before any production code change.

**Use case:** Bounded retry budget for the hypothesis → fix → green cycle. Produces its own draft PR and hands back to `/dev-lifecycle` from Phase 6 onward, so the bug-fix lifecycle still benefits from CI monitoring + bot review.

**Example:** Any issue with label `type: bug` and a clear reproduction path. Started via `/reproduce-then-fix <issue#>` instead of `/dev-lifecycle`.

**Mode interaction:** Independent lifecycle, not gated by `/dev-lifecycle` modes. The reproduction-test discipline is structural — modes don't soften the "failing test first" rule.

### `/weekly-deps`

Replaces `/dev-lifecycle` for batch dependabot PR handling.

**Use case:** Categorizes PRs by risk tier (HIGH / MEDIUM / LOW / UNKNOWN), requests Copilot review on LOW/UNKNOWN (since bot-authored PRs miss the auto-review pass), reconciles AI reviewer findings via `/enhanced-pr-review` for Tier C, processes safe bumps first to avoid lockfile conflicts, escalates framework/core-dependency changes for careful review.

**Example:** Tuesday's batch of dependabot PRs, including periodic security-data refresh PRs from GitHub Actions bots.

**Mode interaction:** Maps to `Automated End-to-End` mode (the former `Weekly Deps` mode folded into it on 2026-06-20). Auto end-to-end except for content flags (major-version bumps, framework changes, or unexpected surface-area expansion), which surface for developer review through `/weekly-deps`'s own risk-tier triage rather than a mode-matrix special case.

### `/issue-creation`

Mid-lifecycle child-spawn when scope analysis at Phase 2 shows ≥3 independent sub-issues should be filed.

**Trigger:**
- **Other:** Phase 2 scope analysis identifies ≥3 independent sub-issues to file as children of the current parent
- **User intent:** explicit `/issue-creation` invocation by the user

**Use case:** Enforces an 8-phase workflow — adjacency scan, scope assessment, pre-spec generation with research, label/milestone alignment, relationship linking, preview checkpoint, creation, post-creation follow-through. Creates well-specified GitHub issues that are immediate `/dev-lifecycle` intakes.

**Example:** Filing the children of a parent epic — the children are spawned as immediate `/dev-lifecycle` intakes with consistent labels and proper sub-issue attachment.

**Mode interaction:** Gated in all modes (filing work creates obligations). The preview checkpoint is mode-independent.

### `/product-management:sprint-planning`

Orthogonal — PM workflow, not implementation.

**Trigger:**
- **Keywords in body/title:** `"sprint planning"`, `"iteration scope"`, `"backlog prioritization"`
- **Labels:** `area: ops` combined with `type: chore`
- **Other:** explicit PM-mode invocation; weekly/iteration cadence

**Use case:** Plans iteration scope, prioritizes backlog, sets capacity expectations. Output is a sprint plan, not a code change.

**Example (hypothetical):** Weekly sprint planning for a milestone's completion — deciding which open issues are in vs out of the cut.

**Mode interaction:** Not dispatched within `/dev-lifecycle`. Listed here for cross-skill discoverability only.

## Recommend-only / billed (Claude cannot invoke; user-triggered)

### `/code-review ultra` (billed + user-triggered)

**Recommendation trigger:** Phase 8 surfaces 5+ subagent findings that warrant deep cross-check.

**Recommendation text:** "Consider running `/code-review ultra <PR#>` for a comprehensive cloud-based multi-agent review."

**Use case:** Deep multi-agent PR review that runs in the cloud — `/code-review ultra` for the current branch, `/code-review ultra <PR#>` for a GitHub PR. Surfaces cross-checks that local subagent reviews don't reach (longer reasoning budget, broader codebase context). Tagged **`billed`** + **`user-triggered`**: Claude cannot invoke it — only recommend it. *(Deprecated alias: `/ultrareview` — same command.)*

**Mode interaction:** Recommendation always surfaces in chat at Phase 8 when the trigger fires. Never auto-dispatched in any mode — it is user-triggered and billed (§ Billed / cloud-cost skills); Claude cannot launch it via Bash or any other mechanism. This constraint is enforced by Claude Code's session-level skill governance, not by repo configuration.

---

## Repo-local skills (present only if the repo ships them)

The entries above are the generic baseline — they apply to any repo consuming this catalog. The entries **below** describe skills that exist only in some repos (stack-specific scaffolders, schema migrators, trust-boundary auditors, the code-quality gate, the version-tag release checklist). Their triggers and use cases are deliberately stack-specific because they only make sense in a repo that ships that stack.

**The dispatcher consults a repo-local entry only if the named skill exists in the current plugin/skill inventory.** If it is absent, the orchestrator does **not** fail — it applies the **Catalog-dispatch resilience** rule at the very bottom of this file (a one-line WARN, then continue). That resilience rule is exactly what makes referencing these optional entries safe: a repo without the skill simply skips it. Phase placement (when present) is noted per entry.

The triggers below use the same file-path / label / keyword axes as the main catalog. Where a path is shown as a placeholder (e.g. `<frontend files>`, `<backend service>`), substitute the path convention of the repo that actually ships the skill.

### `/encryption-flow` (candidate — understand-time, Phase 1)

*(A repo that ships end-to-end encryption may expose this under its own slug; match on whatever name that repo registers.)*

**Trigger:**
- **Labels:** a `domain:` label denoting end-to-end-encryption work
- **File paths in diff:** paths matching the repo's encryption/crypto modules (e.g. `**/crypto/**`) or key-storage migrations
- **Other:** the work touches key generation, wrapping, epoch rotation, or the encryption service

**Use case:** Walk your product's core domain flows end to end — whichever paths the repo treats as canonical for its encryption model — and enumerate every file each one touches, from key generation to persistence. Use at understand-time to confirm the mental model before editing crypto paths.

**Example (hypothetical):** Before a change to a key-epoch handling path, this skill confirms which files the epoch ledger touches.

**Mode interaction:** Auto-dispatch in all modes (Phase 1 is ungated); read-only, which already justifies no gate.

### `/scaffold-component` (candidate — execute-time, Phase 3; user-invoked only)

**Trigger:**
- **File paths in diff:** new component files under <frontend files>
- **Labels:** the repo's frontend `area:` label combined with `type: feature`

**Use case:** Scaffold a new frontend component with co-located styles, tests, and the repo's conventions (typed props, the repo's state-store subscription idiom). Repo-local skill. Use when a plan introduces a brand-new component rather than modifying an existing one.

**Example (hypothetical):** A new settings-panel component — `/scaffold-component` lays down the component + styles + test skeleton to convention.

**Mode interaction:** user-invoked only — surfaced as a developer suggestion when triggers fire; the model never invokes it in any mode (`disable-model-invocation: true`).

### `/scaffold-endpoint` (candidate — execute-time, Phase 3; user-invoked only)

**Trigger:**
- **File paths in diff:** a new handler/route under the repo's backend service tree
- **Labels:** the repo's backend `area:` label combined with `type: feature`

**Use case:** Scaffold a new REST endpoint with handler (the repo's handler + dependency-injection pattern), tests (happy + error path), and router registration. Repo-local skill. Use when a plan adds a new backend route.

**Example (hypothetical):** A new `GET /api/v1/servers/:id/audit-log` route — `/scaffold-endpoint` lays down the handler + tests + registration.

**Mode interaction:** user-invoked only — surfaced as a developer suggestion when triggers fire; the model never invokes it in any mode (`disable-model-invocation: true`).

### `/create-migration` (candidate — execute-time, Phase 3; user-invoked only)

**Trigger:**
- **Keywords in body/title:** `"migration"`, `"schema"`, `"new table"`, `"new column"`, `"alter table"`
- **File paths in diff:** the repo's migrations directory
- **Other:** the work changes the database schema (consult the repo's migrations rule)

**Use case:** Create a numbered up/down migration pair following the repo's conventions (zero-padded sequence, timestamped columns, concurrent indexes, up/down symmetry). Repo-local skill. Use whenever a plan changes the database schema.

**Example (hypothetical):** Adding a `user_preferences` table — `/create-migration` produces the numbered `.up.sql` + `.down.sql` pair to convention.

**Mode interaction:** user-invoked only — surfaced as a developer suggestion when triggers fire (schema changes are often access-control / encryption-sensitive); the model never invokes it in any mode (`disable-model-invocation: true`).

### `/verify-quality-gate` (always-dispatch when present — validate-time, Phase 4)

**Use case:** Check the repo's code-quality gate predictions locally before push — new-code-coverage threshold, blocker/critical issue count, security hotspots. Catches gate failures pre-CI rather than waiting for the post-push scan. Ships in `core-lifecycle`, and runs the lint/type-check/test commands the repo declares in `.quality_gate.checks[]` — with none declared it reports UNVERIFIED rather than inferring a toolchain. It is an **always-dispatch** Phase 4 check when `.quality_gate.enabled` is true; with the gate disabled, or the skill absent from a partial install, the orchestrator skips it per the resilience rule. The Phase 2/4 cross-dispatch of `/engineering:testing-strategy` keys off this skill's coverage-miss signal when it is present.

**Example (hypothetical):** For a pure-docs PR the gate filters non-source files and reduces to a passive check.

**Mode interaction:** Always-dispatch at Phase 4 in all modes when present. Read-only.

### `/ipc-channel-audit` (candidate — final-review, Phase 8; also Phase 4)

**Trigger:**
- **File paths in diff:** the repo's inter-process / trust-boundary surfaces (e.g. main-process IPC and preload directories)
- **Other:** the PR adds or modifies an inter-process channel surface

**Use case:** Enumerate every inter-process channel in the repo, cross-reference caller sites, and flag any handler missing sender-frame (or equivalent trust-boundary) validation. Repo-local skill. Use before a release or when the inter-process surface changes. Also a Phase 4 candidate.

**Example (hypothetical):** A PR adding a new `window:*` handler — `/ipc-channel-audit` confirms it carries the documented sender-frame validation (or the accepted-exception pattern).

**Mode interaction:** Auto-dispatch at Phase 8 in all modes when the relevant trust-boundary paths are in the diff (release-block check; Phase 8 is ungated under the consolidated matrix).

### `/release-checklist` (candidate — merge-ready, Phase 9, when a version tag will be cut; user-invoked only)

**Trigger:**
- **Keywords in body/title:** `"release"`, `"version bump"`, `"cut tag"`, `"ship"`
- **Labels:** `risk: release-gate`
- **Other:** PR adds a version bump in the repo's version manifest; OR an explicit pre-release verification request

**Use case:** Run pre-release verification checks for a new version — version consistency (the version manifest matches expected), CI status on the main branch, test suite, security checks (dependency alerts, secret scanning, pre-commit). Repo-local skill. Distinct from `/engineering:deploy-checklist` (general pre-deploy ops) by being version-tag-specific.

**Example (hypothetical):** Before cutting a new tag, run `/release-checklist <version>` to verify CI green on the main branch, no open P1/P2 issues for the milestone, and no critical dependency alerts.

**Mode interaction:** user-invoked only — surfaced as a developer suggestion when triggers fire; the model never invokes it in any mode (`disable-model-invocation: true`).

---

## Adding a new skill to the catalog

Adding a new dispatchable skill = adding an entry to this file. The catalog is the single source of truth consulted at runtime by `/dev-lifecycle` and `/enhanced-pr-review`.

### Eligibility

- Must be a **model-invocable target**: either a registered skill invocable via the `Skill` tool, OR a bundled/saved **dynamic workflow** invocable via the Workflow tool by name (e.g. `/deep-research`, dispatched as `name: deep-research`). NOT a one-off ad-hoc agent or an inline single-use workflow script. A workflow-dispatched entry MUST state its dispatch path in its Use case (see `/deep-research` for the precedent).
- Must be present in the plugin inventory of the project — if the skill is only available locally, document that limitation in the entry's Use case section.
- Must have a clear `/dev-lifecycle` phase placement (one of Phase 1–9 or Cross-cutting).

### Required fields per entry

For **always-dispatch** entries:

```markdown
### `/skill-name` (always-dispatch)

**Use case:** [~2 sentences describing what the skill does and when it adds value.]

**Example:** [Real PR/issue reference, or a "Hypothetical:" scenario marked explicitly.]

**Mode interaction:** [How dispatch behaves per mode — auto in On-the-Loop / Automated End-to-End, gated in In-the-Loop at its gated phases (2, 5, 7, 9), special cases noted.]
```

For **candidate** entries, add a `**Trigger:**` block before `**Use case:**`:

```markdown
**Trigger:**
- **Keywords in body/title:** `"keyword1"`, `"keyword2"`, regex `/pattern/i`
- **Labels:** `area: X`, `domain: Y`
- **File paths in diff:** `path/glob/**`
- **Other:** (any non-keyword/label/path condition)
```

Omit individual trigger axes that don't apply. A candidate entry MUST have at least one axis populated.

For **recommend-only / billed** entries, use `**Recommendation trigger:**` and `**Recommendation text:**` in place of the standard sections (see `/code-review ultra` for the canonical example).

### Approval

- New entries require **Repo Admin approval** (from the repo's CODEOWNERS / admins). Consistent with the labels.md governance for new labels.
- File the request as an issue with `type: chore` + `area: tooling` titled `Catalog proposal: <skill-name>`. Issue body explains the use case, proposed phase placement, and trigger axes.
- On approval, the catalog entry is added in the same PR as the skill itself (if new) or as a standalone docs PR (if the skill already exists in the plugin inventory).

### Validation

- PR adding a new entry MUST be reviewed by the `docs-reviewer` agent at Phase 8 of `/dev-lifecycle`.
- The reviewer confirms:
  - No orphan reference (the skill exists in the plugin inventory, or is explicitly marked aspirational)
  - No broad-overlap collision with an existing entry (if overlap is intentional, the use-case text documents the expected co-firing)
  - All required fields populated per the dispatch class

### Catalog-dispatch resilience

If a catalog entry references a skill that doesn't exist in the current plugin inventory, the orchestrator (`/dev-lifecycle` or `/enhanced-pr-review`) logs a one-line WARN (`WARN: catalog references unknown skill /<name>; skipping`) and continues. This is intentional — the catalog is a single source of truth across plugin loads, and entries may reference skills that are present in some loads but not others (e.g., user-only-triggered skills, plugin-bundle inconsistencies).
