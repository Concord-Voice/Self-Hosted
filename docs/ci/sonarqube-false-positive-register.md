# SonarQube False Positive Register

> **Split from the Concord-Voice-Alpha register at the repo split (Alpha @771eb3c).** The Legacy section keeps the 26 entries that concern code now in this repo, as decided in Alpha. New decisions for this repo go under Post-migration.

| Field          | Value      |
|----------------|------------|
| Effective Date | 2026-04-11 |
| Version        | 1.0        |

## Purpose

This file is the canonical record of SonarQube false-positive decisions for the Concord Voice project. When a SonarQube finding is marked `FALSE_POSITIVE` via the SonarQube API or UI, a corresponding entry MUST be appended here to preserve the reasoning for future reviewers.

The register exists because the `mcp__sonarqube__change_sonar_issue_status` tool used by Claude Code cannot attach justification comments to the marked issue in SonarQube — the tool only flips the status. Without this register, FP decisions would live silently in SonarQube's state with no discoverable reasoning.

> **Security-signal re-homing (2026-06-11, #1426 / ADR-0017):** SAST security
> detection is re-homing from SonarQube's security sensors to per-tool GitHub
> Code Scanning (CodeQL #1424, Semgrep #1426 — categories `semgrep-custom` /
> `semgrep-community`). This is an architecture change recorded per
> ADR-0017 (not published in this repository) §Consequences — NOT a rule
> suppression on AI-authored code. SonarQube retains coverage + new-code
> quality + AI-Code-Assurance (#1430 narrows its scope).
>
> **Enactment (#1430):** the sensor retirement is enacted by deactivating the
> `injection`-tagged VULNERABILITY (taint) rules in project-assigned custom
> quality profiles — the Sonar-staff-documented mechanism for skipping the
> security sensors (no scanner property exists). Run
> `scripts/sonar/narrow-security-profiles.sh` (not published in this repository)
> (dry-run default). This is the recorded ADR-0017 architecture change, NOT
> rule suppression on AI code: hotspot rules, DBD sensors, and all
> maintainability/reliability rules stay active. Quality Gate thresholds are
> byte-identical. CodeQL `security-extended` (#1424) + Semgrep (#1426) own the
> taint classes. The AI-Code-Assurance *security* signal
> originates from GitHub Code Scanning as of #1430.

### Relationship to SonarQube AI-Code Assurance

Concord has **SonarQube AI-Code Assurance enabled**, which prohibits rule-level suppression or tuning of findings on AI-authored code. As a result, **every false positive must be handled at the instance level** via per-issue `FALSE_POSITIVE` markings. Rule-level `sonar-project.properties` edits that disable or tune rules are not allowed. This register is the audit trail that makes the instance-level approach sustainable.

### ACCEPTED vs FALSE_POSITIVE

SonarQube has two distinct "we're not fixing this" states:

- **`ACCEPTED`** — "the rule is correct, but we've decided not to fix it right now" (tech debt).
- **`FALSE_POSITIVE`** — "the rule is wrong about this specific instance" (tool error).

This register is only for `FALSE_POSITIVE` decisions. `ACCEPTED` findings — when SonarQube reports something the team acknowledges as real but chooses not to fix right now — are tracked via ordinary GitHub issues (typically labeled `domain: developer-experience` and prioritized as tech debt). They are not recorded here.

## Known Gap — closed 2026-05-04

This register begins on **2026-04-11** and does not cover the approximately 616
SonarQube false-positive markings made before that date. **No retroactive audit of
them was ever run, and none is planned.**
[#615](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/615), which
tracked that audit, closed **completed** on 2026-05-04; its closing comment records
that the pre-register markings were rectified incrementally through microchanges
across a few hundred PRs rather than audited as a batch. Treat the pre-2026-04-11
markings as resolved by that incremental work — this register simply does not carry
their reasoning.

## Legacy (Concord-Voice-Alpha, before the repo split)

### Section 1 — Rule-Calibration Notes

<!-- audit-exempt: false-positive substring matches ("alphabetical", "alphabetically") -->
High-level stances on SonarQube rules whose mismatch with this project's idiomatic Go code is **structural** (i.e., any future occurrence of the rule on a similar code pattern would merit the same FP disposition). Entries sorted alphabetically by rule key.

#### `css:S9440` — Animation covered by the global reduce-animations override

| Field | Value |
|---|---|
| First recorded | 2026-10-29 (Sonar/code-scanning reconciliation sweep) |
| Rationale | The rule checks each stylesheet alone for a `prefers-reduced-motion` guard. The desktop renderer guards motion once, globally: `[data-reduce-animations='true'] *` in `client/desktop/src/renderer/styles/index.css` zeroes every animation and transition duration, and `settingsStore.applyReduceAnimations` sets that attribute when the user enables Reduce Animations **or** the OS reports `prefers-reduced-motion: reduce` (the OS half added 2026-10-29, with a live `change` listener). A per-file guard would duplicate the global one in every stylesheet. |
| Instance count in Section 2 | 38 (#226–#260, #271–#273) |
| Disposition | Per-instance `FALSE_POSITIVE` for animations in `client/desktop/src/renderer/**`, after confirming the global rule and the OS-preference wiring still exist. Does not apply to `client/admin` (it carries its own `@media` guard) or to motion set from JavaScript, which must check the preference itself. |

#### `docker:S6471` — Root user in the isolated scratch-based Docker metrics agent

| Field | Value |
|---|---|
| First recorded | 2026-07-14 (issue #1690 / PR #2228 review) |
| Rationale | The operations metrics agent is a single static binary in a `scratch` image and must access the host's bind-mounted Docker socket. A fixed non-root UID cannot portably access that socket because its group ID is host-specific; granting a non-root account socket-group access would confer the same Docker API authority and would not reduce that boundary. Compose instead constrains the process with a read-only root filesystem, `no-new-privileges`, and all Linux capabilities dropped. The image contains no shell or package manager, and the agent uses the socket only for the bounded read-only metrics calls enforced in its code and boundary tests. In this exact deployment, changing `USER 0:0` would be cosmetic or would break socket access rather than remove an effective privilege. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` only for the scratch-based operations metrics agent when the compose hardening (`read_only`, `no-new-privileges`, and `cap_drop: ALL`) and read-only Docker API boundary tests remain in place. Re-review if the image gains a shell, writable filesystem, capabilities, or mutating Docker calls. |

#### `docker:S8431` — Base image pinned by BOTH version tag and digest

| Field | Value |
|---|---|
| First recorded | 2026-08-19 (PR #2827, following the PR #2800 review) |
| Rationale | The `<image>:<tag>@sha256:<digest>` form is deliberate supply-chain hardening, not redundancy: the digest supplies immutability, and the tag supplies the *lineage anchor that the update tooling resolves against*. A bare `FROM node@sha256:<digest>` gives Dependabot no variant to anchor on, so it resolves the repository's default tag. PR #2800 demonstrated the consequence on this exact line: `node:latest` is byte-identical to Debian-based `node:26`, so the pin moved from `node:24-alpine` (57.6 MB) to `node:26` (443 MB) with nothing in the diff but a changed hex string — while Dependabot's own PR body described it as "24-alpine to 26-alpine" ten times. S8431's premise, that the tag is redundant because the digest decides, holds for Docker's *resolution* semantics and fails for the *update tooling's*. Dropping the tag loses that update cue; dropping the digest forfeits immutability. Sonar's own [rule clarification](https://community.sonarsource.com/t/docker-s8431-makes-dependabot-upgrades-harder-to-read-and-limits-its-features/183693/6) exempts projects with supported Dependabot/Renovate configuration. |
| Instance count in Section 2 | 13 (#223–#225 are the 2026-10-29 re-keyed `Dockerfile.turn-secret-writer` and `Dockerfile.redis-auth-controller` pins; #126 `Dockerfile:8`; #147–#149 added 2026-08-21 when the remaining three base images were pinned to close Scorecard `PinnedDependenciesID` #1216/#1217/#1227; #196 is the refreshed control-plane pin in PR #3604; #197–#198 are the Go 1.26.8 builder pins in PR #3656; #202–#203 are the Redis controller pins in PR #3940) |
| Disposition | Per-instance `FALSE_POSITIVE` for `FROM <image>:<tag>@sha256:<digest>` pins where the tag names the intended variant line and the digest pins the reviewed index. Re-review if Dependabot gains variant-aware digest resolution (making the tag genuinely redundant), or if a pin is found carrying a tag that disagrees with what its digest resolves to. |

#### `go:S100` — Identifier naming regex excludes underscore in test names

| Field | Value |
|---|---|
| First recorded | 2026-04-11 (issue #431) |
| Rationale | The rule's regex rejects underscores in Go identifier names. This is incompatible with Go's canonical test-naming convention `TestXxx_Yyy`, which uses underscores to separate the test subject from the test variant (`TestConvert_UTF8`, `TestHandler_ServeHTTP`, etc.). The standard library uses this pattern throughout, and `go test` explicitly recognizes it. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` for each occurrence in `*_test.go` files. Rule-level regex tuning is prohibited by project SonarQube AI-Code Assurance policy. |

#### `go:S1192` — Route-path literal duplication required by the OpenAPI coverage scanner

| Field | Value |
|---|---|
| First recorded | 2026-07-15 (PR #2290, #1352) |
| Rationale | The rule asks for a repeated string literal to be extracted to a constant. For **Gin route-path literals in `internal/api/router.go`**, doing so breaks a functional CI gate: [`scripts/api/check-openapi-coverage.sh`](../../scripts/api/check-openapi-coverage.sh) discovers live routes by parsing route registrations **textually** and cannot resolve a path constant — it emits `[WARN] route with unresolved path constant` and then reports every affected route as absent from the router, failing the `static-guards` job. Extracting `"/:id/messages"` (registered under the server, channel, and DM groups) un-registered five routes, including two pre-existing `GET`s unrelated to the change. The repo's own route-coverage guard is a correctness check and outranks a style preference, so the duplication is **required**, not incidental. Empirically verified in PR #2290: the extraction turned a passing `static-guards` red, and restoring the literals turned it green. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` for route-path literals in `internal/api/router.go` **only**, after confirming the literal is consumed by a Gin route registration that `check-openapi-coverage.sh` must parse. S1192 remains valid for ordinary duplicated literals (error strings, column names) — those are fixed by extracting a constant, not marked FP. An explanatory comment at the declaration site records why the literals must not be re-extracted. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `go:S1313` — Hardcoded private-range CIDR in dev-only fallback + public CDN-proxy snapshot

| Field | Value |
|---|---|
| First recorded | 2026-04-18 (issue #666) |
| Rationale | The rule flags hardcoded IP-address literals. In this project the only Go occurrences are RFC1918 private-range CIDR strings (e.g. `172.16.0.0/12`, `10.0.0.0/8`) used as non-production fallbacks for development and CI — never as production server addresses or credentials. Production deploys MUST override these defaults via the `TRUSTED_PROXY_CIDRS` env var, enforced by a startup guard in `config.validate()` and by a required-var check in `docker-compose.production.yml`. The rule's threat model (exfiltrated IPs, production addresses in source) does not apply — these CIDRs are topology constants documented inline. Rewriting them as non-literal expressions (e.g. `net.CIDRMask(12, 32)` + `net.IPv4(172,16,0,0)`) would be strictly less readable and introduce parse-time failure modes. A second documented-constant class also occurs: the public **Cloudflare-proxy CIDR snapshot** (`cloudflareProxyCIDRs` in `config.go` — the ~22 published Cloudflare edge ranges, e.g. `173.245.48.0/20`, `103.21.244.0/22`), a fallback allowlist used when `TRUSTED_PROXY_CIDRS` is unset and refreshed via the invite-landing runbook. These are intentional documented topology constants (the canonical Cloudflare-published ranges), not environment-specific server addresses, so the same override + not-a-credential reasoning and FP disposition apply. |
| Instance count in Section 2 | 3 (instances #12, #13, #14 — `config.go:464` `broadRFC1918CIDRs()` literal list). Pre-register PR #702 instances also exist but were marked FP via SonarCloud UI without a captured key (MCP env-var substitution was broken at the time). Plus the 15 public Cloudflare-proxy CIDR snapshot literals at `config.go:767–781` (#72–#86). |
| Disposition | Per-instance `FALSE_POSITIVE` for each RFC1918 CIDR literal that serves as a documented dev-only fallback with a production override, AND for each public Cloudflare-proxy CIDR literal in the documented `cloudflareProxyCIDRs` snapshot. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `go:S2077` — Safe dynamic SQL construction with constrained values

| Field | Value |
|---|---|
| First recorded | 2026-04-25 (PR #771) |
| Rationale | The rule deliberately performs no taint analysis and therefore flags safe dynamic SQL along with injection risks. Two reviewed sub-patterns exist here. **A. Dynamic column lists and fixed SQL fragments:** helpers such as `buildUpdateClauses` emit only hardcoded column names, while all values use parameterized `$N` placeholders via `args...`; `%s` carries the fixed column-list fragment and `%d` carries only an integer placeholder index. The same pattern covers fragment helpers concatenated into a query, such as `purge.HiddenRangeFilter`/`HiddenRangeFilterForViewerExpr` and the purge engine's allow-listed identifiers: they interpolate only a source-literal table alias, an allow-listed identifier, or an integer placeholder position, never request data. **B. PostgreSQL utility statements:** `ALTER ROLE ... PASSWORD` does not accept bind parameters for either the identifier or password. The operations-metrics reader activation therefore uses a role name derived inside PostgreSQL from `current_database()`, enforces `^concord_ops_metrics_reader_[0-9a-f]{32}$`, and encodes it with `pq.QuoteIdentifier`; the 256-bit `crypto/rand` password is encoded with `pq.QuoteLiteral`. No request/config value reaches the statement, and PostgreSQL statement/parameter logging is disabled in the same transaction before execution. In both patterns the rule's threat model (untrusted input altering SQL structure) does not apply. |
| Instance count in Section 2 | 54 (recounted 2026-10-29: 31 `go:S2077` rows through #191, plus #192–#195; plus #203; plus #204–#206; plus #209; plus #210–#218 on 2026-10-29; plus #274–#278 for #4135) |
| Disposition | Per-instance `FALSE_POSITIVE` after confirming either (A) every dynamic identifier is selected from hardcoded literals and every value remains parameterized, or (B) the statement is a PostgreSQL utility command that cannot bind the dynamic value, every identifier is fixed or internally derived then allowlist-validated and driver-quoted, and the value is generated internally then encoded with the driver's SQL-literal encoder. **Prefer a code fix where one exists:** in PR #2290 the purge reaper's finding was cleared outright by declaring its query a `const` instead of a function returning a literal — the rule fires on the *call*, so making the query visibly static removed the finding with no marking. Only mark FP when the dynamic construction is genuinely unavoidable (a table/column identifier, which SQL cannot parameterize). Inline `// nosemgrep:` remains required when the parallel upstream Semgrep taint rules flag the same reviewed pattern. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `go:S5332` — `http://` prefix used to prime `net/url` parsing, never to communicate

| Field | Value |
|---|---|
| First recorded | 2026-09-12 (scanner sweep) |
| Rationale | The rule flags any `"http://" + ...` literal as a cleartext-protocol use. It cannot distinguish a URL that is actually dialed from one that is parsed and discarded down to its host/port. `storageHostname(endpoint string) (string, bool)` (`services/control-plane/pkg/config/config.go:948`) prepends the literal only so `net/url.Parse` populates `Host` instead of `Path` for a bare `host:port` input — every use of the resulting `u` was traced (`.User`, `.Path`, `.RawQuery`, `.Fragment`, `.Host`, `.Port()`, `.Hostname()`) and none reaches an `http.Client`, `Dial`, or `Do`; the function returns only `(host, true)`. The prefix must match minio-go's own `getEndpointURL`, which the function's documented contract (lines 931-947) exists to mirror exactly — this function was rewritten specifically to close a bypass where a validator parsed the endpoint differently from the dialer (`minio:9000@evil.example.com` userinfo, where `net.SplitHostPort` blessed host="minio" while minio-go dialed `evil.example.com:80` in cleartext carrying the SigV4 credential). Changing the scheme to `https://` would change no connection — the value is never dialed — and would reopen exactly that mismatch. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` only when the parsed URL is traced end-to-end and never reaches a transport (`http.Client`, `Dial`, `Do`, or equivalent), and the literal scheme exists solely to steer `net/url`'s own parse (e.g. to populate `Host` rather than `Path` for a bare `host:port`). Where the fix would be to change the scheme, first confirm the function's purpose is not to mirror a consumer's own parser byte-for-byte — doing so there is a regression, not a fix. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `godre:S8168` — `defer Rollback()` demanded on non-transaction `Begin()` APIs, helper-returns-tx patterns, and deferred rollback *helpers*

| Field | Value |
|---|---|
| First recorded | 2026-07-22 (issue #2201 / PR #2397) |
| Rationale | The rule pattern-matches any `Begin`-named call and demands an immediate `defer <result>.Rollback()`. Four structural mismatches: (1) `credepoch.Fence.Begin()` is NOT a transaction — its op deliberately classifies THREE outcomes (definite commit → `Commit`, definite rollback → `Rollback`, AMBIGUOUS commit → **neither**, so the blocked marker's TTL + DB read-through reconciles; spec §4.4 step 3c). An unconditional deferred Rollback would DELETE the blocked marker after an ambiguous commit, re-opening the stale-token window the fence exists to close. (2) A helper that opens a real transaction and RETURNS it to the caller (`beginRecoveryTx`) cannot defer the rollback inside itself — the tx must outlive the helper; the caller owns the deferred rollback, which the rule cannot see across the function boundary. (3) **Added 2026-08-13 (#2446 / PR #2738):** the rollback IS deferred in the flagged function, but through a *named helper* — `defer presencehook.RollbackUnlessDone(tx, log)` — and the rule only recognizes a literal `defer tx.Rollback()`. Doing what the rule asks would be an active regression here: `presencecapture.GraphPresenceCapture.Complete` owns the COMMIT, so on every **successful** path the transaction is already resolved and `tx.Rollback()` returns `sql.ErrTxDone`. A **bare** `defer tx.Rollback()` would not log that — it discards the return value entirely, which is the problem: it would also discard a REAL rollback failure, the silent-discard pattern `backend.md` forbids. `RollbackUnlessDone` calls `tx.Rollback()` unconditionally and inspects the result, logging genuine failures while tolerating only the `sql.ErrTxDone` sentinel — so the rollback the rule asks for genuinely happens on every failure path, and unlike the literal form the rule wants, its failures are observable. (4) An in-function transaction can close explicitly on every path: DM-derived delivery rolls back each lock/query/commit failure and commits success before unregistering failed clients. A syntax-only requirement for an immediate defer cannot recognize that complete lifecycle; verify every exit rather than inferring a leak from the absence of defer. (Corrected 2026-08-14 after PR #2738 review: an earlier wording claimed the bare form would *log* a spurious error.) |
| Instance count in Section 2 | 31 (#219–#221 and #279–#283 added 2026-10-29) |
| Disposition | Per-instance `FALSE_POSITIVE` when the flagged `Begin` is (a) a `credepoch.Fence.Begin()` (three-outcome op lifecycle, all failure paths call `Rollback` explicitly), (b) a helper that returns the opened tx to a caller that defers the rollback, (c) followed within the same function by a deferred rollback *helper* that calls `tx.Rollback()` and suppresses only `sql.ErrTxDone`, or (d) every post-begin error explicitly invokes a checked rollback helper and the successful path commits before cleanup. Verify (c) at source before marking — confirm the helper actually rolls back rather than merely being named as if it does. For (d), enumerate every exit and verify that any cleanup requiring released locks follows rollback/commit. S8168 remains valid for a genuinely leaked in-function transaction. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `godre:S8188` — Context cancel handed to the caller rather than deferred

| Field | Value |
|---|---|
| First recorded | 2026-10-29 (Sonar/code-scanning reconciliation sweep) |
| Rationale | The rule demands `defer cancel()` right after `context.WithCancel*`. That is wrong when the function's purpose is to return a live context: deferring would cancel it before the caller ever used it. `authority/store.ServeLock.Watch` returns the derived context together with a stop func that calls `cancel(nil)` and waits for the watch goroutine, so the cancel is owned and always reached by the caller's contract. |
| Instance count in Section 2 | 1 (#222) |
| Disposition | Per-instance `FALSE_POSITIVE` when the context (or a closure that cancels it) is returned to the caller and every path that keeps it reaches the cancel. A cancel that is simply dropped stays a real finding. |

#### `godre:S8193` — Unused variable in `if` short-statement

| Field | Value |
|---|---|
| First recorded | 2026-04-11 (issue #431) |
| Rationale | The rule targets predicate functions where a bound value is used only in the condition. Its "compliant" example does not fit Go's canonical error-check idiom `if err := X(); err != nil { ... }`. Rewriting to `if X() != nil { ... }` discards the bound error value, conflicting with `.claude/rules/domains/backend.md` — "Every error MUST be checked. Wrap errors with context." |
| Instance count in Section 2 | 4 |
| Disposition | Per-instance `FALSE_POSITIVE` for each occurrence of the error-check idiom. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `godre:S8196` — Single-method interface that is an opaque domain value, not an agent noun

| Field | Value |
|---|---|
| First recorded | 2026-08-21 (SonarCloud/Code-Scanning backlog sweep) |
| Rationale | The rule applies Effective Go's "one-method interfaces are named by the method name plus an `-er` suffix" convention uniformly to every single-method interface. That convention describes **behaviour adapters** — `Reader`, `Writer`, `Formatter` — where the interface's identity IS the action. It does not describe interfaces that model an opaque **domain value** which happens to expose one predicate. Go's own standard library is the counter-example: `error` has exactly one method (`Error() string`) and is not named `Errorer`. `rbac.PresenceRecheckPlan` (`presence_recheck.go:12`) is the value case — its doc comment states it is "the opaque pre-mutation Rich Presence capture produced inside an authority transaction and consumed after it commits", and that `internal/rbac` never inspects it. Its single method `HasWork() bool` is a predicate ON the captured plan, not the thing the type IS. The mechanical `-er` rename the rule asks for yields `WorkHaver` or `Worker`; the first is not English and the second actively misdescribes the type, implying an executor when the value performs no work at all. Applying the rule here would degrade readability in the exact dimension ("Predictability", "Readability") the rule cites as its justification. **Second reviewed instance (2026-09-12):** `services/control-plane/internal/activepresence/reconciler.go:73`'s `type Gate interface { WithSenders(context.Context, []uuid.UUID, func() error) error }`. `Gate` is likewise an opaque domain value, and it is itself an agent noun — Effective Go's convention permits "or similar agent noun," and net/http.Handler (single-method `ServeHTTP`) is the stdlib precedent for the same shape. The mechanical rename yields "WithSenderser," which is not a name. Corroborating evidence the rule is a bare suffix heuristic rather than a convention check: `GenerationDeleter` at line 65 of the SAME FILE has method `CompareAndDelete` — equally non-derivable from its name — and goes unflagged purely because its own identifier already ends in `-er`. |
| Instance count in Section 2 | 4 (#146 `rbac.PresenceRecheckPlan`; #153 `activepresence.Gate`, added 2026-09-12; #201 `store.ReplicationProbe`, added 2026-10-15; #284 `voiceauthprotocol.ResponsePayload`, added 2026-10-29) |
| Disposition | Per-instance `FALSE_POSITIVE` when the single-method interface models an opaque domain value or capture whose method is a predicate/accessor, AND an `-er` agent-noun rename would misdescribe the type. This carve-out is narrow: a single-method interface that genuinely models a behaviour (something is *done* through it) remains a true positive and must be renamed. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `godre:S8239` — Intentional `context.Background()` for cleanup that must survive request cancellation

| Field | Value |
|---|---|
| First recorded | 2026-06-14 (PR #1516) |
| Rationale | The rule flags `context.Background()` creation where a request-scoped `ctx` is in scope, on the assumption the request context should be propagated. That assumption is correct for in-request work but **wrong for fire-and-forget cleanup that must outlive the request** — specifically post-commit cache invalidation and audit logging that run AFTER the HTTP response transaction has committed. If those used the request `ctx`, a client disconnect (which cancels `ctx`) between commit and cleanup would abort the cache invalidation / audit write, leaving a stale cache entry or a missing audit record — exactly the silent-failure class `.claude/rules/domains/backend.md` guards against. The detached `context.Background()` is the intentional, correct choice here: the cleanup is causally downstream of a committed transaction and its correctness must not depend on the caller still being connected. (Gitar's review on #1516 independently flagged an attempt to "fix" this by switching to the cancellable `ctx`; that change was reverted precisely because it tied durable cleanup to request liveness.) |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` for each `context.Background()` used for post-commit cache/audit cleanup that must survive request cancellation. Confirm by checking the call site runs after the transaction commits and performs durable side effects (cache invalidate, audit log). Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S1082` — Mouse-event-only element flagged for missing keyboard listener (successor to `S6852`)

| Field | Value |
|---|---|
| First recorded | 2026-06-14 (PR #1516) |
| Rationale | `typescript:S1082` ("Visible, non-interactive elements with click handlers must have at least one keyboard listener") is the current rule key SonarCloud emits for the same family previously keyed `typescript:S6852`. It co-fires with `typescript:S6848` (successor to `S6843`) on the `<div onClick={...}>` / `<div onMouseDown={...}>` shape. The disposition is identical to the existing Section 1 entries for `typescript:S6843` / `typescript:S6852`: the a11y rule family is **internally contradictory for non-native elements** — `S6848` demands `role` + keyboard listeners, while its siblings `S6819` ("use a native element, not the ARIA role") and `S6847` ("non-interactive elements should not have listeners") forbid exactly that. Only converting to a native element satisfies all three, which is not viable for overlay backdrops, native-HTML drag-and-drop wrappers, or container tiles that hold block-level children (`<video>`, `<div>`) that `<button>` cannot legally contain. The naive `role="button"` fix was attempted in #1516 and produced 16 NEW `S6819`/`S6847` smells — empirically confirming the contradiction. See the `typescript:S6848` entry below for the full sub-pattern taxonomy. |
| Instance count in Section 2 | 5 |
| Disposition | Per-instance `FALSE_POSITIVE` when the co-located `typescript:S6848` finding on the same line is also marked FP (the two rules are linked — if `S6848` is a real a11y gap, so is `S1082`). Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S1313` — RFC special-purpose address blocks that ARE the SSRF egress denylist

| Field | Value |
|---|---|
| First recorded | 2026-08-21 (SonarCloud/Code-Scanning backlog sweep) |
| Rationale | The TypeScript sibling of the `go:S1313` calibration above, with a stronger form of the same argument. The rule flags hardcoded IP literals on the theory that an address in source is environment-specific configuration that leaks topology or pins a deployment. In `client/desktop/src/main/egressPolicy.ts` the literals are neither: they are the **RFC-defined special-purpose address registries** — RFC 1918 private space, RFC 3927 / RFC 4291 link-local, RFC 6598 CGNAT, RFC 4193 ULA, RFC 5771 multicast, RFC 6052 / RFC 3056 NAT64 and 6to4, RFC 1112 reserved — enumerated to build the `net.BlockList` denylist that `classifyAddress()` uses to refuse SSRF dials to internal networks. The addresses are the security control itself, not a configuration value it consumes. The rule's own remediation (externalise to configuration) is the precise opposite of what this file must do: making the denylist runtime-configurable would create the bypass the module exists to prevent, since anything that can edit that configuration can delete a tier-1 entry and re-open metadata-service and link-local egress. The values are also fixed by IANA registry, not by environment — they are identical in dev, CI and production, so there is nothing for a deployment to override. Note the module is deliberately structured so these constants are unreachable from any caller: `TIER1`/`TIER2` are module-private, and `isDialPermitted` returns `false` for tier 1 with no branch on the approval flag. |
| Instance count in Section 2 | 18 |
| Disposition | Per-instance `FALSE_POSITIVE` for each IP literal inside `egressPolicy.ts`'s `TIER1`/`TIER2` denylist tables, after confirming the literal is a registry-defined special-purpose block and that the surrounding structure is a deny/classify table rather than a dial target or allowlist. An IP literal that is a **destination** (an API host, a STUN/TURN server, a bootstrap peer) is NOT covered by this note and remains a true positive. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S3735` — `void` operator marks a deliberately fire-and-forget promise

| Field | Value |
|---|---|
| First recorded | 2026-07-22 (PR #2374) |
| Rationale | The rule ("Remove this use of the `void` operator") is a general-JS style rule. It is mis-applied to `void <promise>()` used as the **canonical fire-and-forget marker** required by the project's own `@typescript-eslint/no-floating-promises` ESLint rule (`client/desktop/eslint.config.mjs`). Removing the `void` (S3735's remediation) produces a floating promise, which `no-floating-promises` then rejects — the two rules directly conflict. Both flagged sites (`tokenManager` staged-key + legacy-migration adopts) are deliberately unawaited: the owner-scoped writer handles its own failure (warn-and-never-clear, #1288) and must not block the synchronous return of the already-resolved keys. |
| Instance count in Section 2 | 2 (#105, #106) |
| Disposition | Per-instance `FALSE_POSITIVE` for `void <promise>` sites that exist to satisfy `no-floating-promises` on an intentionally-unawaited call. Does NOT apply to a `void` discarding a non-promise expression. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S4084` — Media caption track on WebRTC audio/video elements

| Field | Value |
|---|---|
| First recorded | 2026-04-12 (issue #432) |
| Rationale | The rule requires `<audio>` and `<video>` elements to have a `<track>` child for captions. In Concord Voice, these elements are bound to dynamic WebRTC `MediaStream` objects via `srcObject` — not static media files. WebRTC streams cannot carry `<track>` caption data; captions would need to be delivered via a separate data channel or signaling path, which is outside the scope of the media element itself. The rule's compliance model assumes file-based media, not real-time streams. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` for each occurrence on WebRTC-bound media elements. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S4325` — Type assertions that `tsc` confirms are load-bearing

| Field | Value |
|---|---|
| First recorded | 2026-05-11 (issue #818) |
| Rationale | The rule flags `value as Type` assertions where SonarQube's TypeScript analyzer believes the receiver accepts the original-typed value. In practice, SonarQube's analyzer does not fully replicate TypeScript's compiler flow analysis, and over-flags several idiomatic narrowing patterns where running `tsc --noEmit` actively errors when the cast is removed. Sub-patterns confirmed FP in this codebase (each verified by removing the cast and observing the specific TS error): **A.** Forced cast `X as unknown as Record<string, unknown>` for a Zustand store's `getState()` flowing into a `callSetter`/`pushDraftsToStore` helper — TS2352, because the store's discriminated state type lacks an index signature and isn't structurally assignable to `Record<string, unknown>` without going through `unknown`. The `as unknown` step is load-bearing. **B.** `string as LiteralUnion` after an `Array.includes(value)` check — `.includes()` returns `boolean` but does not narrow the value's type to the array's literal-union; TS2345 on the downstream call without the cast. **C.** `errback(err as Error)` inside a catch block — TS5+ types catch variables as `unknown` by default; downstream consumers (e.g., mediasoup's `errback`) require `Error`. TS2345 without the cast. **D.** `IIFE-result as 'literal' \| 'literal2'` — TypeScript widens the inferred IIFE return type to `string` when the body has multiple narrowed returns plus a non-literal fallback; TS2322 without the cast. **E.** `IPC-bridge-string as TypedUnion` — Electron IPC bridges incoming `data.type` / `data.status` payloads as the wire type (`string`); internal stores require the narrower union. TS2345 without the cast. **F.** `Uint8Array.buffer as ArrayBuffer` — `.buffer` is typed `ArrayBufferLike` (union of `ArrayBuffer \| SharedArrayBuffer`); React setState expecting `ArrayBuffer` rejects the union. TS2345 without the cast. **G.** `({}) as TypedShape` for a default-empty fallback — `{}` is not structurally assignable to types whose downstream consumers access required-shaped properties. TS2339 on the property access without the cast. |
| Instance count in Section 2 | 11 (+2 — rows #155–#156 added 2026-09-12, re-fingerprinted after `voiceService.ts` moved to `services/voice/`) |
| Disposition | Per-instance `FALSE_POSITIVE` when `tsc --noEmit` confirms removal of the cast produces a TS2322 / TS2345 / TS2339 / TS2352 error. The TS compiler's diagnostic is the authoritative arbiter — it is more accurate than SonarQube's reduced TS analyzer on these flow-narrowing patterns. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S6582` — optional chain suggested on an explicit `=== null ||` / `!x ||` membership guard

| Field | Value |
|---|---|
| First recorded | 2026-07-22 (PR #2374) |
| Rationale | The rule ("Prefer using an optional chain expression") is a general conciseness rule. It is mis-applied to explicit `receipt === null || … receipt.field …` / `!completion || completion.field …` membership guards where the leading null-check is **load-bearing for correctness**, not redundant. Applying the remediation changes semantics: in `e2eeService.clearKeysIfInitializationCurrent`, rewriting `receipt === null || this.sessionKeys !== receipt.sessionKeys` to `this.sessionKeys !== receipt?.sessionKeys` evaluates to `false` when `receipt` is `null` AND the singleton fields are `undefined`, letting the guard fall through so `clearKeys()` runs on a null receipt — a CWE-212 key-custody regression. In the `ipc/sso.ts` completion guards, the explicit `!completion ||` prefix is reorder-safe (later `completion.field` accesses depend on it), whereas the optional-chain rewrite's safety hinges on evaluation order and breaks silently if conditions are reordered. |
| Instance count in Section 2 | 3 (#107, #108, #109) |
| Disposition | Per-instance `FALSE_POSITIVE` for `S6582` flagged on an explicit `=== null ||` / `!x ||` membership guard where the optional-chain rewrite changes short-circuit semantics or correctness. Does NOT apply to a simple `a && a.b` genuinely equivalent to `a?.b`. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S6819` — Custom ARIA combobox and adjustable-separator patterns

| Field | Value |
|---|---|
| First recorded | 2026-04-20 (issue #651) |
| Rationale | Two reviewed patterns require an explicit ARIA role because no native element provides the same semantics and behavior.<br><br>**A — Rich combobox:** The rule suggests replacing custom `role="listbox"` markup with semantic HTML (`<select size=...>`, `<select multiple=...>`, `<datalist>`). None of those alternatives can carry avatar thumbnails, role-color badges, custom icons, or integrate with a controlled textarea's cursor for `@`-trigger filtering. `MentionAutocomplete` and `EmojiAutocomplete` implement the custom ARIA combobox contract with `role="listbox"`, `aria-activedescendant`, per-option `role="option"`/`aria-selected`, and complete keyboard navigation.<br><br>**B — Adjustable separator:** `DockShell` uses the WAI-ARIA adjustable-separator pattern for a sidebar splitter: explicit `role="separator"`, `tabIndex={0}`, accessible name/orientation/current/minimum/maximum values, pointer resizing, and ArrowLeft/ArrowRight keyboard handlers with Shift acceleration. A native `<hr>` is a static horizontal rule and cannot provide the focusable, keyboard-adjustable splitter contract. Removing the explicit role would erase the widget semantics while leaving an interactive generic element. |
| Instance count in Section 2 | 3 (row #67 `MentionAutocomplete`; row #91 `EmojiAutocomplete`; row #116 `DockShell` adjustable separator) |
| Disposition | Per-instance `FALSE_POSITIVE` for either (A) a custom ARIA-compliant combobox that requires rich per-option content or (B) a WAI-ARIA adjustable separator that exposes the explicit role, focusability, value properties, and keyboard resize handlers. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S6843` — Non-native interactive element with a compensating control

| Field | Value |
|---|---|
| First recorded | 2026-05-27 (PR #747, #115) |
| Rationale | The rule flags `<div>` (or other non-native) elements that carry interactive event handlers (`onClick`, `onMouseDown`, `onContextMenu`) without declaring an interactive role + tabIndex + keyboard listener. The naive "fix" (adding `role="button"` + `tabIndex` + an `onKeyDown` handler) triggers SIBLING rules in the same family: `typescript:S6817` ("use `<button>` instead of `role='button'`") and `typescript:S6822` ("`tabIndex` should only be declared on interactive elements"). Net effect of the naive fix is MORE findings, not fewer. This entry covers two sub-patterns where the rule is over-firing in Concord Voice:<br><br>**Sub-pattern A — Propagation-blocking wrapper inside a popover/menu.** The handlers exist ONLY to call `e.stopPropagation()`. The element is NOT user-interactive; it's a layout wrapper that prevents an outer popover's outside-click handler from firing when the user interacts with descendants (typically dragging a slider whose pointer may exit the descendant's bounding box mid-drag). Adding role/tabIndex/keyboard handlers would falsely advertise the wrapper to assistive tech. All real interaction happens on focusable child elements (slider, button) that already carry their own keyboard support.<br><br>**Sub-pattern B — Non-native element with a native sibling that provides the keyboard equivalent.** The non-native element (e.g., a participant tile `<div>`) carries a mouse-convenience handler (typically `onContextMenu`) that opens a menu at the cursor. A native `<button>` sibling (e.g., a kebab/three-dots trigger rendered at the tile's corner) provides the keyboard equivalent: the button is focusable by default, gets Enter/Space for free, has an accessible name via `aria-label`, and opens the same menu. The keyboard-access concern that motivates the rule is fully satisfied by the sibling button — the rule's threat model (no keyboard path to the function) does not apply. Wrapping the outer element in `<button>` is not viable because the tile contains block-level children (`<video>`, `<div>`) which `<button>` cannot semantically contain. |
| Instance count in Section 2 | 3 |
| Disposition | Per-instance `FALSE_POSITIVE` when the underlying pattern matches sub-pattern A or B above. Confirm by reading the handler body and the surrounding JSX:<br>• **Sub-pattern A:** the handler is demonstrably pure-`stopPropagation`/`preventDefault` (no other side effects), AND the wrapper sits inside an outer popover/menu that owns the real user interaction.<br>• **Sub-pattern B:** there is a native interactive element (`<button>`, `<a href>`, native input) rendered alongside the non-native element that opens the same menu / triggers the same action, providing the keyboard-access path the rule was designed to ensure.<br><br>If neither sub-pattern applies, this disposition does NOT apply and the finding is a real a11y gap. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S6845` — `tabIndex` on a WAI-ARIA adjustable separator

| Field | Value |
|---|---|
| First recorded | 2026-07-29 (issue #1750 / PR #2510) |
| Rationale | The rule advises placing `tabIndex` only on native interactive elements, but an adjustable separator is an explicitly focusable ARIA widget. `DockShell` implements that contract with `role="separator"`, `tabIndex={0}`, `aria-label`, `aria-orientation="vertical"`, `aria-valuemin`, `aria-valuemax`, `aria-valuenow`, pointer resizing, and ArrowLeft/ArrowRight keyboard handlers with Shift acceleration. Removing `tabIndex` would make the splitter unreachable by keyboard. Replacing it with `<hr>` would supply only static horizontal-rule semantics; an `<hr>` cannot implement a focusable, keyboard-adjustable splitter. |
| Instance count in Section 2 | 1 (row #118) |
| Disposition | Per-instance `FALSE_POSITIVE` only when `tabIndex` makes an explicit `role="separator"` adjustable, the element exposes its orientation and current/minimum/maximum values, and keyboard handlers perform the same bounded resize operation as pointer input. |

#### `typescript:S6848` — Non-native interactive element (successor to `S6843`), a mutually contradictory rule family

| Field | Value |
|---|---|
| First recorded | 2026-06-14 (PR #1516) |
| Rationale | `typescript:S6848` ("Avoid non-native interactive elements...") is the current rule key SonarCloud emits for the family previously keyed `typescript:S6843`. The disposition and rationale are inherited from the `typescript:S6843` entry above. The defining property: **the a11y rule family cannot be simultaneously satisfied on a non-native element.** `S6848` says "add `role` + `tabIndex` + keyboard listeners"; `S6819` says "don't use the ARIA `role`, use the native element"; `S6847` says "non-interactive elements should not carry mouse/keyboard listeners." The only mutual solution is converting the element to native HTML — impossible for the cases below. PR #1516 empirically demonstrated this: applying the `role="button"` + `onKeyDown` fix to these elements produced **16 new `S6819`/`S6847` smells** (net-negative), so the fix was reverted and the findings marked FP per this entry. Sub-patterns observed in Concord Voice (each verified by reading the JSX):<br><br>**A — Propagation-blocking wrapper** (see `S6843` sub-pattern A): handlers are pure `stopPropagation`/`preventDefault`; real interaction lives on focusable children.<br>**B — Native-sibling keyboard equivalent** (see `S6843` sub-pattern B): a sibling `<button>` provides the keyboard path (e.g. participant-tile kebab trigger).<br>**C — Overlay/modal backdrop dismiss**: a full-screen backdrop `<div onClick={onClose}>` whose only action is click-outside-to-dismiss. The keyboard path already exists — a global `Escape` handler (`Modal` document keydown, `ScreenSharePicker` Escape) plus a visible, focusable close `<button>`. Making the backdrop itself focusable/role=button would falsely advertise the dismiss-surface to assistive tech and duplicate the close button.<br>**D — Hover-reveal informational element**: `onMouseEnter`/`onMouseLeave` that reveal a tooltip or slide-in panel (e.g. `MainView` E2EE indicator, `ChannelPanel` peek). There is no click action to make keyboard-operable; the core state is already conveyed by an always-visible icon (Lock/LockOpen) and the hover content is progressive enhancement. `S6848`/`S6847` firing on a non-interactive hover affordance is over-firing.<br>**E — Native HTML5 drag-and-drop wrapper**: `onDragStart`/`onDragOver`/`onDrop` on `ServerBar` reorder wrappers. Keyboard-accessible drag-and-drop is a separately-scoped design-system concern, not satisfiable by `role`+`onKeyDown` on the drag wrapper; the rule's role/tabIndex remedy does not provide a keyboard reorder path.<br>**F — WAI-ARIA adjustable separator:** `DockShell`'s resize handle is intentionally a non-native element with `role="separator"`, `tabIndex={0}`, accessible orientation/current/minimum/maximum values, pointer resizing, and ArrowLeft/ArrowRight keyboard handlers with Shift acceleration. A native `<hr>` has static horizontal-rule semantics and cannot implement the keyboard-adjustable splitter contract. |
| Instance count in Section 2 | 13 |
| Disposition | Per-instance `FALSE_POSITIVE` when the element matches sub-pattern A–F above and cannot be converted to a native interactive element without tripping `S6819`/`S6847` or losing required ARIA widget semantics. The keyboard-access concern the rule was designed to enforce is satisfied by a documented alternative or by the explicit adjustable-separator contract, OR is inapplicable (no action / native-DnD). If none applies, this disposition does NOT apply and the finding is a real a11y gap. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S6852` — Co-fires with S6843 on the propagation-blocking wrapper pattern

| Field | Value |
|---|---|
| First recorded | 2026-05-27 (PR #747, #115) |
| Rationale | Same pattern as the Section 1 entry for `typescript:S6843`. `S6843` ("non-native interactive elements need role + tabIndex") and `S6852` ("non-interactive elements with click handlers need keyboard listeners") frequently co-fire on the same line — both target the `<div onClick={...}>` shape. When the click handler is a pure `e.stopPropagation()`, `S6852`'s "click handlers need keyboard listeners" is mis-calibrated: there's no user action to support with a keyboard equivalent because the click handler does nothing user-visible. |
| Instance count in Section 2 | 2 |
| Disposition | Per-instance `FALSE_POSITIVE` when (a) the underlying element matches the propagation-blocking wrapper described in the `typescript:S6843` Section 1 entry, AND (b) `S6843` is also marked FP on the same line via this register. The two rules are linked — if `S6843` is real (the element IS interactive), then `S6852` is also real. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `typescript:S7764` — `self` is the canonical WorkerGlobalScope idiom, and `globalThis` re-flags a CodeQL FP

| Field | Value |
|---|---|
| First recorded | 2026-06-14 (PR #1516) |
| Rationale | The rule ("Prefer `globalThis` over `self`") is a general-JS modernization. It is mis-applied inside a **dedicated Web Worker** (`client/desktop/src/renderer/workers/e2eeWorker.ts`), where `self` is the spec-canonical reference to the `WorkerGlobalScope` and is the established idiom. Critically, applying the rule's remediation has a concrete harm: changing `self.onmessage = ...` to `globalThis.onmessage = ...` **edits the worker's message-handler line**, which causes CodeQL to re-attribute the pre-existing `js/missing-origin-check` alert (alert #405) to the PR diff and **block the Alpha Code Scanning merge gate**. That CodeQL alert is itself a false positive — a dedicated worker receives messages only from the parent context that spawned it (same-origin by spec; `MessageEvent.origin` is the empty string for parent↔worker messages), so there is no cross-origin `postMessage` sender to validate. The `js/missing-origin-check` rule targets `window.addEventListener('message', ...)` cross-window scenarios, not dedicated-worker `onmessage`. So `globalThis` yields no benefit on a worker's global while triggering a spurious security-gate block; `self` is retained. **Second sub-case — renderer `window.setTimeout`/`setInterval` whose return is stored in a `number`-typed ref.** In the Electron renderer, `@types/node` shares the global scope with `lib.dom`, so `globalThis.setTimeout(...)` resolves to the **Node** overload returning `NodeJS.Timeout`, which is not assignable to a `number`-typed timer ref (`useRef<number \| null>`); `window.setTimeout` is DOM-typed (returns `number`). Applying the `globalThis` remediation breaks `tsc --noEmit` (TS2322 'Timeout' is not assignable to 'number'), so `window` is the correct idiom for browser timer IDs and the rule is a FP for these call sites. **Third sub-case — ACCEPTED, not FALSE_POSITIVE, recorded here because the distinction is exactly what this register exists to preserve.** `client/desktop/src/renderer/services/voice/screenAudioBridge.ts:342` compares `event.source` against bare `globalThis`; the rule is right that `globalThis` and `window` are the same object at runtime, but the spelling is unavailable because the compiler rejects it: `event.source` is typed `MessageEventSource \| null` (`Window \| MessagePort \| ServiceWorker`), and TypeScript never merges that union with `typeof globalThis` — only the `window` declaration in lib.dom carries `Window & typeof globalThis`. Verified: the change fails typecheck with `screenAudioBridge.ts(342,9): error TS2367: This comparison appears to be unintentional because the types 'MessageEventSource \| null' and 'typeof globalThis' have no overlap.` The only route to compliance is a double cast, and this is the check that refuses a same-document attacker forging a message event, so blinding the compiler there is the wrong trade. Per this register's own ACCEPTED-vs-FALSE_POSITIVE distinction, a compiler limitation is not the rule being wrong about the instance — so `AaCUeaEHto_c3DKA1XL5` was set `ACCEPTED` (2026-09-12), not `FALSE_POSITIVE`, and carries no Section 2 row. The sibling `S7764` on the same file's line 341 WAS fixed (`globalThis.location.origin`). |
| Instance count in Section 2 | 5 (worker `self`: #71, #87, #88; renderer `window.setTimeout` return-type: #89, #90) |
| Disposition | Per-instance `FALSE_POSITIVE` for (a) `self` references in dedicated-Web-Worker scope (`src/renderer/workers/**`), where `self` is the canonical idiom and the `globalThis` remediation would touch a message-handler line and re-flag the CodeQL `js/missing-origin-check` FP; and (b) `window.setTimeout`/`setInterval` whose return is stored in a `number`-typed ref, where `globalThis` pulls in the Node `Timeout` overload and breaks `tsc`. Does NOT apply to non-worker modules with no timer-return-typing conflict, where `globalThis` is preferred. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |

#### `tssecurity:S5147` — URL path concatenation in internal API-helper module

| Field | Value |
|---|---|
| First recorded | 2026-06-01 (PR #1264) |
| Rationale | The rule (and its companion `securityhotspot:S5852` "Ensure that tainted data is validated before being used to construct a client-side request URL") fires whenever a function parameter flows into a `fetch()` URL via template concatenation. In `client/desktop/src/renderer/services/apiClient.ts`, the `apiFetch(path, init)` public surface accepts a relative API route string and concatenates it with the build-time-constant `API_BASE` (read from `src/renderer/config.ts`). Sonar's taint analyzer flags `path` as "user-controllable" because it is an external function parameter, but in this codebase: (a) `apiFetch` is an internal renderer-only helper — no path argument ever crosses a UI input boundary unsanitized, (b) every call site uses a hardcoded route template (`/api/v1/...`) with optional embedded entity IDs from store state (UUIDs, snowflake IDs sourced from the same trusted backend), (c) `API_BASE` is a build-time constant pinning the destination to the official `concordvoice.chat` API host — even an attacker-influenced `path` cannot escape the origin via this code path because the URL grammar parses `${API_BASE}${path}` left-to-right with no scheme/host substitution available downstream of the constant prefix. The rule's threat model (SSRF to attacker-controlled origin or arbitrary URL fetch) cannot apply when the URL prefix is a build-time constant. Note: the exact Sonar rule key is the editor's best inference from the rule descriptors quoted in PR #1264; will be reconciled with the issue key when the SonarQube MCP env-var substitution is fixed and the FP markings are re-recorded. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `FALSE_POSITIVE` for each `fetch(\`${API_BASE}${path}\`, ...)` invocation in `apiClient.ts` where `path` is the public parameter of `apiFetch` (or a callee that received it unmodified). After PR #1264, the URL construction is centralized in the single `apiFetchRaw()` helper, so this disposition applies to one site rather than three. Rule-level disable is prohibited by project SonarQube AI-Code Assurance policy. |


## Post-migration (Self-Hosted)

### Rule-Calibration Notes

_None yet._

### Instance-Specific Entries

_None yet._

## Recreation-recovery / drift-guard (ADR-0017 narrowed profile)

The **2026-06-14 SonarCloud project recreation** (see the Section 2 banner) did
more than invalidate issue keys: a freshly-recreated project reverts to the
stock **`Sonar way`** quality profiles, silently undoing the ADR-0017 narrowing
(the deactivation of the `injection`-tagged VULNERABILITY taint rules in the
custom `Concord way (<lang>) — ADR-0017 narrowed` profiles). With the narrowing
reverted, SonarCloud's JS/TS/Go security sensors run again — re-introducing both
the #1381 timeout and a duplicate SAST signal that ADR-0017 deliberately re-homed
to CodeQL + Semgrep.

A CI **drift-guard** now detects that regression:
[`scripts/sonar/check-narrowed-profile.sh`](../../scripts/sonar/check-narrowed-profile.sh)
(read-only) asserts, per language (js/ts/go), that (1) the profile *assigned to
the project* is the narrowed one, and (2) it has **zero active**
injection-tagged VULNERABILITY rules — querying `rules/search` scoped with
`qprofile=<key>&activation=true` (an org-wide query would always find injection
rules in the catalog and false-positive forever). In Concord-Voice-Alpha it
runs as an advisory step of the `sonar-profile-drift` job; this repo's CI does
not run it yet (Sonar CI wiring is #36). The restore op
(`scripts/sonar/narrow-security-profiles.sh --apply`) stays in Alpha, which
manages the shared org profiles. The hermetic self-test
(`scripts/sonar/test-check-narrowed-profile.sh`) is in this repo.

## Schema Rules

1. **Section 1 entries** use the 4-field table format and are sorted alphabetically by rule key. Add a new Section 1 entry when a rule's mismatch is structural, regardless of current instance count.
2. **Section 2 entries** are append-only. Rows are numbered sequentially and never renumbered.
3. **Never delete a Section 2 row.** If an FP is later reopened, apply GitHub-flavored markdown strikethrough (`~~text~~`) to every non-pipe cell in the row and append a `**Reopened YYYY-MM-DD (reason)**` annotation to the Justification column *outside* the strikethrough so the reason remains readable. Example: `| ~~5~~ | ~~2026-04-11~~ | ~~AZ0bzFOrh5l158zVMp5B~~ | ~~go:S6418~~ | ~~...~~ | ~~Claude (#431)~~ | ~~original justification~~ **Reopened 2026-05-10 (security-reviewer flagged as real finding)** |`. This preserves the original decision as crossed-out history while making the reopen reason scannable.
4. **Marker column format:** `<identity> (#<issue-or-pr-number>)`. For Claude Code sessions, use `Claude (#NNN)`. For pre-existing FPs discovered during pre-flight verification, use `pre-existing FP (YYYY-MM-DD)`.
5. **Date column:** ISO 8601 `YYYY-MM-DD` of the day the FP was marked.
6. **Cross-reference Section 1 from Section 2** when the justification is rule-level.

## Related Issues

- [#431](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/431) — Register genesis, initial 7 entries
- [#432](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/432) — Final ACCEPTED-bucket cleanup, 1 new entry (S4084 WebRTC audio)
- [#615](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/615) — Retroactive audit of ~616 pre-register FPs; closed **completed** 2026-05-04 without running the audit (resolved incrementally instead)

## Maintenance

- **On new FP marking:** Append a row to Section 2. If the rule has no Section 1 entry and the justification is rule-level, add a Section 1 entry first (alphabetical insert), then append to Section 2.
- **Obtaining the Issue Key:** Run `mcp__sonarqube__search_sonar_issues_in_projects` (or the equivalent SonarCloud UI page) filtered by project, file, and rule to locate the specific finding. The returned `key` field (format `AZxxxxxxxxxxxxxxxxxx`) is the value to paste into the Issue Key column. The `mcp__sonarqube__change_sonar_issue_status` call that flips the finding to `FALSE_POSITIVE` takes the same key as its `key` parameter.
- **On rule reopening:** Never delete. Strikethrough the row (see Schema Rule 3 for syntax) and add a `**Reopened YYYY-MM-DD (reason)**` annotation. If the reopened finding's rule was the *only* instance keeping a Section 1 entry alive, update that entry's `Instance count in Section 2` field but do not remove the Section 1 entry — the rule-level reasoning remains valid for future instances.
- **Flipping an FP via the SonarQube MCP requires two calls, not one.** SonarCloud's state machine does not allow direct transitions between terminal states, so an issue currently in `ACCEPTED` must first be reopened and then re-resolved as false-positive. The pattern is: (1) `mcp__sonarqube__change_sonar_issue_status({ key, status: ["reopen"] })`, immediately followed by (2) `mcp__sonarqube__change_sonar_issue_status({ key, status: ["falsepositive"] })`. Never abort between these two calls for the same issue — a successful reopen without a follow-up falsepositive leaves the finding in an `OPEN` state that can fail the Quality Gate. See `docs/policies/mcp-server-policy.md` §8 for the full gotcha writeup.
- **When Section 2 grows past ~50 rows:** Consider splitting Section 2 into sub-sections grouped by rule key (`#### godre:S8193`, `#### go:S6418`, etc.), preserving the global sequential numbering. This is a forward-compatibility hint, not a hard rule — smaller registers don't need grouping. The split should happen as its own PR, not mixed in with a routine FP marking.
- **Agent discovery:** `.claude/agents/docs-reviewer.md` watches this file. `.claude/agents/docs-writer.md` has a Mode 1 special-case for appending entries.
