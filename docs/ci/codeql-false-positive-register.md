# CodeQL False Positive Register

> **Split from the Concord-Voice-Alpha register at the repo split (Alpha @771eb3c).** The Legacy section keeps the 31 entries that concern code now in this repo, as decided in Alpha. New decisions for this repo go under Post-migration.

| Field          | Value      |
|----------------|------------|
| Effective Date | 2026-06-04 |
| Version        | 1.0        |

## Purpose

This file is the canonical record of CodeQL (GitHub code-scanning) false-positive dismissals for Concord Voice. When a CodeQL alert is dismissed as `false positive` via the code-scanning API or UI, a corresponding entry MUST be appended here to preserve the reasoning for future reviewers. Scope note: despite the CodeQL-centric title, this register houses `false positive` dismissals for **every** GitHub Code Scanning tool (govulncheck, Scorecard, ESLint SARIF, …) — one register per surface, not per tool.

GitHub's dismissal `dismissed_comment` is capped at 280 characters and is not discoverable from the codebase. This register holds the full justification and links each dismissal to the PR that made the finding safe.

> **Advanced setup (2026-06-10, #1424):** CodeQL runs in **advanced mode** via the committed
> [`.github/workflows/codeql.yml`](../../.github/workflows/codeql.yml) — matrix
> `go` / `javascript-typescript` / `actions`, `security-extended` queries (parity with the
> retired default setup's `extended` suite). Default setup is disabled.
>
> **How it is invoked (updated 2026-08-28).** `codeql.yml` carries exactly one trigger,
> `workflow_call`. It has no `pull_request`, `push`, or `schedule` of its own. It is
> invoked as a reusable workflow by `pr-ci.yml` for covered source/config changes
> and full-scan PRs, by `nightly-ci.yml` (07:00
> UTC), and by `main-ci.yml` on a `main` push **only when the merged PR carried
> `risk: hotfix`**. The weekly in-file cron was retired on 2026-08-26 in favour of the
> nightly, so there is no separate weekly default-branch baseline scan.
> Dismissals recorded here may need re-applying when an alert re-surfaces as "new" under a
> different analysis category — see Section 1 calibration notes.

CodeQL retains its generated SARIF for an additional read-only, finite JSONL report containing rule IDs, repository-relative primary paths, lines, and optional level/security severity. The report excludes messages, snippets, code flows and host paths; malformed input is refused. Scan rules, upload behavior, severity checks and merge gates remain unchanged. Diagnostic output is not an alert dismissal or evidence that a finding is resolved.

### Dismissal reasons

GitHub code-scanning supports three dismissal reasons:

- **`false positive`** — the alert is wrong about this specific instance (tool error). **This register is only for `false positive` dismissals.**
- **`won't fix`** — the alert is correct but the team accepts the risk (tech debt → tracked via GitHub issues, not here).
- **`used in tests`** — the flagged pattern is test-only (e.g., a fixture). Recorded inline at the dismissal, never as a row here.

> **2026-09-12 scanner sweep — `used in tests` count.** Twelve alerts were dismissed `used in tests` in this sweep and correctly carry no Section 2 row: six JWT test-fixture signing keys, two integration-test fixture cookies, and four workflow-lint YAML fixtures. Recorded here only so the sweep's total (39 `false positive` + 12 `used in tests` = 51) is reconstructable from this file.

### Relationship to the SonarQube FP register

This is the CodeQL analogue of [`sonarqube-false-positive-register.md`](sonarqube-false-positive-register.md). Same discipline: instance-level dismissals with a captured justification, no rule-level suppression. CodeQL inline-suppression is not used. Dismissal goes through the code-scanning API with a reason.

## Legacy (Concord-Voice-Alpha, before the repo split)

### Section 1 — Rule-Calibration Notes

Stances on CodeQL rules whose mismatch with this project's idioms is **structural** — any future occurrence on the same pattern merits the same FP disposition.

#### `go/request-forgery` — runtime egress guard invisible to static taint

| Field | Value |
|---|---|
| First recorded | 2026-06-04 (PR #1362, issue #1361) |
| Rationale | The rule traces `user input → outbound request URL` and cannot model defenses that live at dial time (`net.Dialer.Control`) or redirect time (`http.Client.CheckRedirect`). A proxy whose destination is constrained by a dial-time IP-class denylist (rejecting loopback/private/link-local/metadata on the resolved IP, post-DNS, pre-connect) and a per-hop host-allowlist re-validation is **not** SSRF-vulnerable, but the taint flow `?url= → Do()` is inherent to a proxy and cannot be removed without breaking function. The runtime egress guard is the mitigation; the static alert is a false positive. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `false positive` dismissal for each `go/request-forgery` alert on an outbound-proxy sink protected by the egress-guard pattern documented in [`.claude/rules/backend.md`](../../.claude/rules/backend.md) "Outbound-request SSRF egress guard". |

#### `go/log-injection` — slog structured logging is output-encoded at the handler

| Field | Value |
|---|---|
| First recorded | 2026-07-15 (scanning-alert triage session; batch of 314 alerts) |
| Rationale | The control-plane logs through `pkg/logger` **at the sites this calibration covers — not uniformly** (163 stdlib `log.Printf` call sites exist under `internal/`, concentrated in `internal/websocket`, and they are governed by `sanitizeLogValue` per [`observability.md`](../../.claude/rules/observability.md) §5, not by this note). At a covered site: `*logger.Logger` wrapping `log/slog` — `slog.NewJSONHandler` in production, `slog.NewTextHandler` otherwise). Both handlers **escape control characters in attribute values and the message at render time** (JSON string escaping; Text quoting via `strconv.Quote` for values that need it), so a `\n`/`\r` in a user-derived value cannot line-split the log — CWE-117 log forging is structurally impossible through a slog attr. This is the same *output-encoding guarantee* category that `.claude/rules/observability.md` §5 already carves out for the `%q` verb. CodeQL's taint tracker models the `.Info/.Warn/.Error/.Debug` call as the sink and cannot see the handler-side encoding, so it flags every user-derived value passed as a slog arg. |
| Instance count in Section 2 | 314 (batch entry) |
| Enforced in config (2026-09-24) | The calibration is now encoded instead of dismissed by hand. `.github/codeql/codeql-config.yml` excludes `go/log-injection` and runs `concord/go/log-injection-unstructured` (`.github/codeql/queries/go/LogInjectionUnstructured.ql`): the upstream query's own configuration with one change: a slog attribute **value** that follows a constant key (in `log/slog` calls, `pkg/logger` methods, and `slog.String`/`Any`/… constructors) is removed from the **sinks**. The message, keys, group names and `args...` spreads stay sinks: PR #3440's review showed that slog's default handler writes the message raw and that a tainted key forges a field (red-team proofs, 2026-09-24). **It is a sink exclusion, not a `LogInjection::Sanitizer`, on purpose.** A sanitizer is a barrier, and a barrier also cuts taint for every later read of the same variable: two earlier drafts that used one hid a `log.Printf(v)` that followed a sanitized `h.log.Info("x", "v", v)`. Verified with a CodeQL canary on PR #3440 (commits `db548e377` to `b3e9f4551`): with the exclusion disabled, all nine slog and `log.Printf` lines alerted; with it enabled, exactly the tainted messages, the tainted key and both `log.Printf` lines alerted, and the four constant-keyed values did not. The first canary (`dcd1eaa42`) proved less than it was credited with at the time: its only slog line was a package-level call, and without a same-shape positive twin its silence could not distinguish a working exclusion from a call that was not a sink. Stdlib `log.Printf` and every other unstructured logger remain sinks, so an alert from the scoped query is a **true positive by default**: move the value into a constant-keyed attribute, or route it through `sanitizeLogValue` at a `log.Printf` site. Never dismiss it under this note. A custom `slog.Handler` that writes values raw voids the guarantee; the sanitizer class must then narrow. The per-instance entries below are the historical record of the 434 dismissals that motivated it. |
| Disposition | Per-instance `false positive` dismissal for `go/log-injection` alerts whose sink is a `pkg/logger` slog call — **verify the sink is a slog call before applying this disposition**; the calibration covers `*logger.Logger`/slog structured calls ONLY. Wire-derived strings interpolated into stdlib `log.Printf` (e.g. the websocket package) get no output-encoding guarantee — those remain true positives and must route through `sanitizeLogValue` per `.claude/rules/observability.md` §5 (founding sweep #1365). Do not add per-site `sanitizeLogValue` wrapping to slog args solely to appease the rule — the handler already encodes. |

#### `go/clear-text-logging` — name-heuristic misfire on non-secret values

| Field | Value |
|---|---|
| First recorded | 2026-07-15 (alert #619) |
| Rationale | The rule classifies expressions as "sensitive" partly by identifier name (`password`, `secret`, …). A **boolean flag** whose name contains `password` (e.g. `needsPassword` — "was password step-up required?") carries no credential material; logging it discloses nothing. Verify the flagged expression's type/content before applying: an actual password/token/hash in any form stays a true positive under `.claude/rules/observability.md` §1. |
| Instance count in Section 2 | 1 |
| Disposition | Per-instance `false positive` dismissal when the flagged expression is demonstrably non-secret (boolean/enum flag, resource identifier) and no key material flows to the sink. |

#### `generic.nginx.security.possible-h2c-smuggling` / `generic.nginx.security.request-host-used` — file-scope `map` validation upstream of every flagged proxy block

| Field | Value |
|---|---|
| First recorded | 2026-09-12 (scanner sweep) |
| Rationale | Both rules pattern-match the presence of `$http_upgrade` / `$host` inside a `location`/`proxy_pass` block and cannot see a `map` directive declared elsewhere in the file that already constrains the value before it reaches that block — which is each rule's own recommended remediation, already applied at file scope rather than per-block. **h2c** (`infrastructure/docker/nginx/nginx.conf`, `infrastructure/deploy/nginx/concordvoice.conf`): a top-of-file `map $http_upgrade $validated_upgrade { default ""; ~*^websocket$ websocket; }` means every flagged block forwards `$validated_upgrade`, never `$http_upgrade` — the only value reachable downstream is the literal `websocket`, and h2c maps to the empty string. The Go control-plane also wires no h2c anywhere (`h2c`, `http2.Server`, `golang.org/x/net/http2` do not appear under `services/control-plane`), so there is no upstream to smuggle a request to even if the map were bypassed. **request-host-used**: `$host` is the discriminator key of a Host-allowlist `map`, the rule's own recommended remediation; every downstream consumer uses `$validated_host`, and an unrecognized Host maps to empty and is refused. The map line already carries a `# nosemgrep` annotation explaining this for the sibling Semgrep OSS rule. |
| Correction preserved from the sweep | An initial pass judged the `location /api/` h2c block a TRUE positive on the reasoning that it is not a WebSocket route and so does not need Upgrade forwarding at all. That is a tidiness argument, not a security one: the rule's premise fails there for the identical reason it fails in the other five blocks, and "fix" would have meant deleting the `# nosemgrep` line that keeps that block consistent with its siblings. The right question is never "is this a WebSocket route?" — it is "can `$validated_upgrade` ever hold a value other than `''`/`'websocket'`?", and the map's default arm answers that for all six blocks alike. |
| Drift note | All six h2c alert line numbers had drifted from the analysed commit `8b0db0b2fd727d019cca0ac26997d94752bcee13` (+8 in `nginx.conf`, +14/+16 in `concordvoice.conf`) because of the attachment-upload blocks added by #2881/#3053 — re-locate by block, not by line, before re-verifying. |
| Instance count in Section 2 | 10 — h2c: #1290, #1289, #1287, #1286, #1285, #1284; request-host-used: #1288, #1283, #1282, #1281 |
| Disposition | Per-instance `false positive` for either rule when the flagged directive forwards the file-scope validated variable (`$validated_upgrade` / `$validated_host`) rather than the raw `$http_upgrade` / `$host`, and the corresponding `map`'s default arm resolves to a value the rule's threat model cannot reach. Does not apply to a block that forwards the raw variable directly — that remains a true positive. |

#### `go/weak-sensitive-data-hashing` — a 0/1 flag named `PasswordLogin`, spread across every authority layout by interface dispatch

| Field | Value |
|---|---|
| First recorded | 2026-10-09 (alert #1587); calibrated 2026-10-15 (PR #3854, alerts #1624 / #1625) |
| Rationale | The rule picks its source by identifier name. Its source here is `authoritycontract.CPEvidence.PasswordLogin` (`msg_published.go:144`, field entry `:153`), a `uint64` the codec restricts to 0 or 1 (`kU01`). It records whether the sign-in used a password; it holds no credential. CodeQL then resolves the interface call `m.spec()` in the codec (`codec.go:96`) to every `specer`, so the flag's taint reaches every layout's `Encode()` bytes, layouts with no evidence field included. Any SHA-256 or SHA-384 over those bytes is then reported as password hashing. Those hashes are DoR §2.4 digests and signature pre-hashes (ECDSA P-384 over SHA-384; legacy RSA-PSS over SHA-256), frozen in protocol v1, so a password KDF there would break every verifier. |
| Instance count in Section 2 | 3 |
| Disposition | Per-instance `false positive` when the SARIF code flow's source is `selection of PasswordLogin` (or another 0/1 flag) and the sink hashes `authoritycontract` message bytes for a digest or a signature. Read the code flow's first step before applying this: a flow whose source carries a real password, recovery secret or key material is a true positive and needs Argon2id or a KDF. |


### Section 2 — Instance-Specific Entries


#### Alert #1 — `go/request-forgery` — `services/control-plane/internal/klipy/handlers.go`

| Field | Value |
|---|---|
| Alert | #1 (`go/request-forgery`, "Uncontrolled data used in network request", critical) |
| Location | `services/control-plane/internal/klipy/handlers.go` — the `h.mediaClient.Do(req)` media-proxy sink |
| Dismissed | 2026-06-04, in PR #1362 (Closes #1361) |
| Reason | `false positive` |
| Justification | The KLIPY media proxy's user-supplied `?url=` is constrained by defense-in-depth CodeQL's taint tracker cannot follow: (1) `mediaClient.CheckRedirect` re-applies `scheme==https` + the anchored `allowedMediaHosts` allowlist on every redirect hop; (2) a cloned transport's `net.Dialer.Control` rejects resolved IPs that are loopback / private / ULA / link-local / unspecified / CGNAT / `169.254.169.254` metadata (`Unmap`-normalized), firing post-DNS-resolution and pre-connect on the actual dialed IP — defeating both redirect-SSRF and DNS-rebinding with no TOCTOU window. Verified by `services/control-plane/internal/klipy/egress_test.go`. The alert re-surfaced as "new in PR" only because the fix shifted the sink line into the PR diff. |
| Re-check | CodeQL re-analysis will continue to report this pattern statically; the dismissal stands per the Section 1 calibration note. |

#### Alerts #620–#1000 (batch of 314) — `go/log-injection` — control-plane slog call sites

| Field | Value |
|---|---|
| Alerts | 314 `go/log-injection` alerts (medium), alert numbers ranging #620–#1000 (non-contiguous; every open `go/log-injection` alert as of 2026-07-15) |
| Location | 26 files under `services/control-plane/internal/**` — mfa/handlers.go (55), channels/handlers.go (55), auth/handlers.go (27), media/handlers.go (26), dm/handlers.go (25), voice/handlers.go (22), members/handlers.go (15), friends/handlers.go (15), channels/groups.go (9), servers/handlers.go (7), rbac/handlers.go (7), invites/handlers.go (7), messages/handlers.go (6), klipy/handlers.go (6), voice/temp_grants.go (5), feedback/handlers.go (4), sessions/handlers.go (3), rbac/audit.go (3), ownership/handlers.go (3), media/cleanup.go (3), users/handlers.go (2), privacy/handler.go (2), messages/reactions.go (2), keyrotation/rotator.go (2), email/service.go (2), middleware/logger.go (1) |
| Dismissed | 2026-07-15, batch dismissal via code-scanning API (scanning-alert triage session) |
| Reason | `false positive` |
| Justification | Every flagged sink was mechanically classified (statement-opening scan over all 314 locations) and confirmed to be a `pkg/logger` slog structured call (`.Info/.Warn/.Error/.Debug` with key-value attrs) — zero stdlib `log.Printf` interpolation sites in the batch. slog's JSON/Text handlers escape control characters at render time, so CWE-117 line-splitting is structurally impossible; see the Section 1 `go/log-injection` calibration note. The PII discipline for these logs is governed separately by `.claude/rules/observability.md` (identifiers-only, no payloads) and is unaffected by this dismissal. |
| Re-check | New `go/log-injection` alerts must be re-verified as slog sinks before applying this disposition — a `log.Printf` sink is a true positive requiring `sanitizeLogValue`. |

#### Alert #619 — `go/clear-text-logging` — `services/control-plane/internal/sessions/handlers.go:453`

| Field | Value |
|---|---|
| Alert | #619 (`go/clear-text-logging`, "Clear-text logging of sensitive information", high) |
| Location | `services/control-plane/internal/sessions/handlers.go:453` — `h.log.Info("Session revoked", "user_id", uid, "session_id", sessionID, "password_used", needsPassword)` |
| Dismissed | 2026-07-15 (scanning-alert triage session) |
| Reason | `false positive` |
| Justification | The flagged expression is `needsPassword` — a **boolean** recording whether password step-up authorized the revoke, matched by the rule's name-heuristic on "password". No credential material exists in the statement: `uid` and `sessionID` are resource identifiers (the session ID is the client-supplied URL path parameter of the revoke request, not a bearer credential). See the Section 1 `go/clear-text-logging` calibration note. |
| Re-check | If this log line ever gains an actual secret field, the alert class becomes a true positive under `.claude/rules/observability.md` §1. |

#### Alert #616 — govulncheck `GO-2026-5932` — `go.mod` (golang.org/x/crypto)

| Field | Value |
|---|---|
| Alert | #616 (govulncheck, `GO-2026-5932`, note) |
| Location | `services/control-plane/go.mod` — module dependency `golang.org/x/crypto v0.54.0` |
| Dismissed | 2026-07-15 (scanning-alert triage session) |
| Reason | `false positive` |
| Justification | GO-2026-5932 covers the deprecated `golang.org/x/crypto/openpgp` package tree ("unsafe by design, unmaintained"). The advisory has **no fixed version** (`introduced: 0`, no `fixed` event) — there is no upgrade that clears it. `go mod why golang.org/x/crypto/openpgp` reports "main module does not need package", and govulncheck's own finding text states the code "doesn't appear to call any of the vulnerable symbols" — the match is module-level only. The project's crypto uses other `x/crypto` packages; nothing imports openpgp. Not exploitable, not fixable by bump. |
| Re-check | If a future dependency starts importing `x/crypto/openpgp`, govulncheck will escalate from module-level to symbol-level — that would be a true positive requiring migration to `github.com/ProtonMail/go-crypto/openpgp`. |

#### Alert #344 — Scorecard `VulnerabilitiesID` — repository-level

| Field | Value |
|---|---|
| Alert | #344 (Scorecard `VulnerabilitiesID`, high, "1 existing vulnerabilities detected: GO-2026-5932") |
| Location | Repository-level (no file) |
| Dismissed | 2026-07-15 (scanning-alert triage session) |
| Reason | `false positive` |
| Justification | The check's sole finding is GO-2026-5932 — the same unreachable, unfixable `x/crypto/openpgp` module-level match dismissed as alert #616 above (never imported per `go mod why`; no vulnerable symbols called per govulncheck; no fixed version exists). Distinct from the 2026-06-15 posture where this check surfaced a real media-plane `brace-expansion` DoS and was deliberately left open — that finding was fixed; only the openpgp residue remains. |
| Re-check | Re-assess whenever Scorecard's Vulnerabilities score changes — a new OSV ID in the message text is a fresh finding, not covered by this dismissal. |

#### Alert #1054 — Semgrep OSS `websocket-missing-origin-check` — `internal/websocket/handler.go`

| Field | Value |
|---|---|
| Alert | #1054 (Semgrep OSS, `go.gorilla.security.audit.websocket-missing-origin-check.websocket-missing-origin-check`, warning) |
| Location | `services/control-plane/internal/websocket/handler.go:97` — `h.upgrader.Upgrade(...)` |
| Dismissed | 2026-07-16 (PR #2328) |
| Reason | `false positive` |
| Justification | Re-fingerprint of the long-`nosemgrep`-suppressed FP: the origin check EXISTS — `CheckOrigin` is configured in `NewHandler` and regression-tested in `checkorigin_test.go`. PR #2328 changed the line only to pass the security-header set into `Upgrade`'s responseHeader argument (the 101-handshake header fix); origin enforcement is untouched. "Semgrep OSS" counts any content change on a suppressed line as a new PR-diff alert (see project memory `project_semgrep_oss_refingerprint_fp`). |
| Re-check | If `CheckOrigin` is ever removed from the Upgrader construction in `NewHandler`, this rule becomes a true positive. |

#### Alerts #1103, #1104, #1111 — `go/log-injection` — `services/control-plane/internal/dm/handlers.go`

| Field | Value |
|---|---|
| Alert | #1103 / #1104 / #1111 (`go/log-injection`, "Log entries created from user input", medium) |
| Location | `services/control-plane/internal/dm/handlers.go:1561` (`h.log.Warn("Failed to clear pending key request after distribution", …)`), `:1499` (`h.log.Error("DM key distribution failed", …)`), `:1521` (`h.log.Error("Failed to fetch DM key version", …)`) — all three are the DM key-distribution refactor added by PR #2397 (#2201) |
| Dismissed | 2026-07-22 (PR #2397) |
| Reason | `false positive` |
| Justification | New alert numbers for the SAME slog-output-encoding class already dispositioned in the Section 1 `go/log-injection` calibration note and the #620–#1000 batch (which already lists `dm/handlers.go`). Verified per the batch's Re-check requirement: all three sinks are `pkg/logger` slog structured calls (`.Warn`/`.Error` with key-value attrs `conversation_id`/`user_id`/`error`), NOT stdlib `log.Printf` — slog's JSON/Text handlers escape control characters at render time, so CWE-117 line-splitting is structurally impossible. Per `.claude/rules/observability.md` §5, slog attribute values must NOT be `sanitizeLogValue`-wrapped (the handler already encodes; wrapping is churn for zero security gain). |
| Re-check | A future `log.Printf` sink at these lines would be a true positive requiring `sanitizeLogValue`. |

#### Alerts #1100–#1102 — `go/log-injection` — `services/control-plane/internal/members/handlers.go`

| Field | Value |
|---|---|
| Alert | #1100 / #1101 / #1102 (`go/log-injection`, "Log entries created from user input", medium) |
| Location | `services/control-plane/internal/members/handlers.go:903` — `h.log.Info("Member "+action, "server_id", serverID, "target_user", targetUserID, "by_user", userID)` |
| Dismissed | 2026-07-26 (PR #2483, #2344) |
| Reason | `false positive` |
| Justification | Re-fingerprint of the established structured-slog class after the membership-removal ordering hardening moved the call into this PR's diff. Verified the sink is `pkg/logger`'s `h.log.Info`, not `log.Printf`; slog JSON/Text handlers output-encode the message and every attribute, preventing CWE-117 record splitting. The fields contain identifiers only, never key material or payloads. |
| Re-check | If this call is changed to `log.Printf`, or gains key material or user payloads, it is no longer covered and must be fixed under `.claude/rules/observability.md` §5. |

#### Alerts #1070, #1154–#1157, #1160–#1161, #1185–#1187, #1201–#1204 — `go/log-injection` — control-plane `pkg/logger` sinks

| Field | Value |
|---|---|
| Alerts | 14 `go/log-injection` alerts (medium): #1070, #1154–#1157, #1160–#1161, #1185–#1187, #1201–#1204 |
| Location | `pkg/logger/logger.go:50` (`Logger.With`, removed in [PR #3541](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3541): callers now reach slog's own `With` through the embedded `*slog.Logger`, so the scoped query judges each caller's keys); `internal/auth/handlers.go:1328–1329, 1613, 1638`; `internal/messages/purge.go:105, 224, 313`; `internal/channels/handlers.go:1635` |
| Dismissed | 2026-07-29, batch dismissal via code-scanning API ([PR #2525](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2525), security-alert remediation) |
| Reason | `false positive` |
| Justification | Each sink was confirmed to be a `pkg/logger` structured slog call, not unescaped stdlib `log.Printf`. `Logger.With` only stages attributes; the service's JSON/Text handlers escape control characters at render time, so a user-derived newline cannot create a second log record. `TestHandlersEscapeControlCharsInAttrValues` regression-tests that guarantee. |
| Re-check | Reassess any raw/custom handler or `log.Printf` conversion; those sinks are not covered and must follow `.codex/rules/observability.md` §5. |

#### Alerts #1222, #1223, #1257 — `go/log-injection` — `messages/purge.go` + `privacy/handler.go`

| Field | Value |
|---|---|
| Alerts | 3 `go/log-injection` alerts (medium, "Log entries created from user input"): #1222, #1223, #1257 |
| Location | `internal/messages/purge.go:228` (`server_id`), `internal/messages/purge.go:328` (`channel_id`), `internal/privacy/handler.go:109` (`user_id`) |
| Dismissed | 2026-08-21, direct code-scanning API `PATCH` (SonarCloud/Code-Scanning backlog sweep) |
| Reason | `false positive` |
| Justification | New alert numbers for the structured-slog output-encoding class already dispositioned in the Section 1 `go/log-injection` calibration note and the #620–#1000 batch. The batch's **Re-check requirement was satisfied before dismissal**: each of the three sinks was opened and confirmed to be a `pkg/logger` structured call (`h.log.Error` with key-value attrs), not stdlib `log.Printf`. The handler-side encoding was additionally re-proved by execution rather than asserted from the register — a value carrying `\n`, `\r`, `\t`, `"` and `=` was logged through both `slog.NewJSONHandler` and `slog.NewTextHandler` and rendered on a single line in each (`\n` escaped in JSON; `strconv.Quote`-style quoting in Text), so CWE-117 record-splitting is structurally impossible. **Secondary observation — NOT load-bearing, and it does not cover all three values.** `channelID` and `serverID` are additionally rejected with HTTP 400 by `uuid.Parse` at `purge.go:47` and `purge.go:139`, so those two cannot carry a control character irrespective of the handler. **`userID` had no such shape validation when this was dismissed, and now does — but the dismissal does not rest on it.** As dismissed on 2026-08-21, `middleware.AuthRequired` took the JWT's `user_id` claim as a plain string and checked only that it was non-empty, `privacy/handler.go:66` re-guarded emptiness only, and there was no `uuid.Parse` anywhere in the auth middleware; the value was a UUID only because our own mint path writes one, which is a property of the mint path rather than a validated invariant at this sink. Since #3362 (PR #3363) `AuthRequired` calls `canonicalUserID`, which `uuid.Parse`es the claim and refuses a non-UUID with 401, so `userID` now reaches this sink shape-validated exactly as `channelID` and `serverID` do. **The disposition is unchanged and must stay unchanged: it rests ENTIRELY on the handler-side output encoding proved above, which is sufficient on its own.** Do not re-found this dismissal on the new input validation — #3362 was a fail-closed hardening of a provenance property, not a promise this sink may depend on, and a future change to the auth middleware must not be able to silently invalidate a CodeQL dismissal three packages away. (Original wording corrected 2026-08-21 after CodeRabbit challenged it on PR #2867 — the challenge was correct. Fact refreshed 2026-09-18 by PR #3363, which made the old sentence false while leaving the reasoning intact.) Per `.claude/rules/observability.md` §5 the attrs are NOT `sanitizeLogValue`-wrapped; the handler already encodes and wrapping would be churn for zero security gain. |
| Re-check | Unchanged from the #620–#1000 batch: a new `go/log-injection` alert must be re-verified as a slog sink before this disposition is reused. A `log.Printf` sink is a true positive requiring `sanitizeLogValue`. |

#### Alerts #1417 / #1418 — `go/log-injection` — `messages/purge.go`

| Field | Value |
|---|---|
| Alerts | #1417 and #1418 (`go/log-injection`, medium) |
| Location | `internal/messages/purge.go:229` and `:294` — server-purge error logs |
| Dismissed | 2026-09-24 02:06 UTC (2026-09-23 EDT), individually in PR #3142 |
| Reason | `false positive` |
| Justification | Both calls use fixed message literals with request-derived `server_id` as a structured `h.log.Error` attribute, not interpolation into `log.Printf`. `pkg/logger` uses `slog.JSONHandler` in production and `slog.TextHandler` otherwise; `TestHandlersEscapeControlCharsInAttrValues` proves both encode control characters without splitting a log record. Every production caller of `purgeServerCore` also validates the server ID with `uuid.Parse`, but the handler-side encoding is the load-bearing reason for dismissal. This is the Section 1 structured-slog calibration case. |
| Re-check | Reassess if either sink changes to an unencoded logger or a dynamically assembled message. |

#### Alert #1429 — `go/log-injection` — `services/control-plane/internal/auth/handlers.go`

| Field | Value |
|---|---|
| Alert | #1429 (`go/log-injection`, "Log entries created from user input", medium) |
| Location | `services/control-plane/internal/auth/handlers.go:1699` — `h.log.Warn("Suspicious machine ID mismatch detected", …, "request_machine_id", requestMachineID)` |
| Dismissed | 2026-09-07, in PR #3136 |
| Reason | `false positive` |
| Justification | The sink was verified as a `pkg/logger` structured slog call, not stdlib `log.Printf`. Production JSON and development Text handlers escape control characters at render time, so the request-derived machine identifier cannot forge a second log record. This is the established Section 1 `go/log-injection` calibration case. |
| Re-check | If this call is changed to an unencoded logger or gains key material or user payloads, the dismissal no longer applies. |

#### Alerts #1442, #1441, #1440, #1439, #1438, #1412, #1392, #1264, #1263 — `go/log-injection` — nine control-plane slog sinks

| Field | Value |
|---|---|
| Alerts | 9 `go/log-injection` alerts (medium, "Log entries created from user input"): #1442, #1441, #1440, #1439, #1438, #1412, #1392, #1264, #1263 |
| Location | `sessions/handlers.go:627` and `:484`; `rbac/audit.go:72`; `auth/handlers.go:1693` and `:1340`; `dm/handlers.go:2646`; `media/handlers.go:943`; `rbac/handlers.go:612` (#1264 and #1263 are duplicates at the same location) |
| Dismissed | 2026-09-12, batch dismissal via code-scanning API (scanner sweep) |
| Reason | `false positive` |
| Justification | Extends the Section 1 `go/log-injection` calibration note and the #620–#1000 batch. The batch's re-check requirement was satisfied: each of the nine sinks was opened and confirmed to be a `*logger.Logger` structured slog call whose message argument is a Go string literal and whose tainted value is an attribute — zero stdlib `log.Printf` interpolation sites. The handler-side encoding was re-proved by execution rather than inherited: the repo's own `TestHandlersEscapeControlCharsInAttrValues` (`services/control-plane/pkg/logger/logger_test.go:103`) was run and passes, covering both `NewWithWriter` (TextHandler, mirroring `New("development")`) and a JSONHandler constructed as `New("production")` does; a forged-record payload stays on one physical line under both. An independent probe additionally confirmed slog escapes a tainted value interpolated into the MESSAGE string, not just into an attribute — the encoding guarantee is broader than the attribute path the Section 1 note describes — but this does NOT widen the disposition, which still requires the sink to be a slog call. |
| Re-check | A future `log.Printf` sink at these lines, or a message built by string concatenation before reaching slog, is a true positive requiring `sanitizeLogValue`. |

#### Alert #1437 — `go/clear-text-logging` — `services/control-plane/internal/sessions/handlers.go:484`

| Field | Value |
|---|---|
| Alert | #1437 (`go/clear-text-logging`, "Clear-text logging of sensitive information", high) |
| Location | `services/control-plane/internal/sessions/handlers.go:484` — `h.log.Info("Session revoked", "user_id", uid, "session_id", sessionID, "password_used", needsPassword)` |
| Dismissed | 2026-09-12, in the scanner sweep |
| Reason | `false positive` |
| Justification | Duplicate of the already-dismissed alert #619, re-fingerprinted because the file moved (line 453 → 484); the statement is unchanged. See the Section 1 `go/clear-text-logging` calibration note and the #619 entry above rather than restating it. Load-bearing verification: `needsPassword` is a **bool**, returned by `determineAuthRequired(ctx context.Context, uid string) (needsPassword bool, mode string)` at `handlers.go:390` — a name-heuristic misfire exactly as the Section 1 note predicts. A re-fingerprinted duplicate of a dismissed alert is expected after a file move and needs only re-verification that the statement is unchanged, not re-litigation. |
| Re-check | If this log line ever gains an actual secret field, the alert class becomes a true positive under `.claude/rules/observability.md` §1. |

#### Alerts #1290, #1289, #1287, #1286, #1285, #1284, #1288, #1283, #1282, #1281 — nginx h2c smuggling / request-host-used — nginx configs

| Field | Value |
|---|---|
| Alerts | h2c (`generic.nginx.security.possible-h2c-smuggling`): #1290, #1289, #1287, #1286, #1285, #1284. request-host-used (`generic.nginx.security.request-host-used`): #1288, #1283, #1282, #1281 |
| Location | `infrastructure/docker/nginx/nginx.conf` and `infrastructure/deploy/nginx/concordvoice.conf` |
| Dismissed | 2026-09-12, in the scanner sweep |
| Reason | `false positive` |
| Justification | See the new Section 1 note for both rules. h2c: every flagged block forwards `$validated_upgrade` (produced by a top-of-file `map $http_upgrade $validated_upgrade { default ""; ~*^websocket$ websocket; }`), never raw `$http_upgrade`, so the only value reachable downstream is the literal `websocket`; h2c maps to empty string. The control-plane also wires no h2c anywhere (`h2c` / `http2.Server` / `golang.org/x/net/http2` do not appear under `services/control-plane`). request-host-used: `$host` is the discriminator key of a Host-allowlist `map`, the rule's own recommended remediation; every downstream consumer uses `$validated_host`, and an unrecognized Host maps to empty and is refused. **Correction preserved:** an initial pass judged the `location /api/` h2c block a true positive because it is not a WebSocket route. That is a tidiness argument, not a security one — the rule's premise fails there for the identical reason it fails in the other five blocks, and the "fix" would have deleted the `# nosemgrep` line keeping that block consistent with its siblings; the right question is whether `$validated_upgrade` can ever hold anything but `''`/`'websocket'`, not whether the route is a WebSocket route. |
| Re-check | All six h2c cited lines had drifted (+8 in `nginx.conf`, +14/+16 in `concordvoice.conf`) from the analysed commit `8b0db0b2fd727d019cca0ac26997d94752bcee13` because of the attachment-upload blocks added by #2881/#3053 — relocate by block, not by line, before re-verifying either rule. |

#### Alerts #1292 / #1291 — `dockerfile.security.missing-user.missing-user` / `.missing-user-entrypoint` — `infrastructure/docker/postgres/Dockerfile`

| Field | Value |
|---|---|
| Alerts | #1292 (`dockerfile.security.missing-user.missing-user`) and #1291 (`dockerfile.security.missing-user-entrypoint.missing-user-entrypoint`) |
| Location | `infrastructure/docker/postgres/Dockerfile` — lines 21 and 19 |
| Dismissed | 2026-09-12, in the scanner sweep |
| Reason | `false positive` |
| Justification | The image derives `FROM` the official `postgres` image, whose `docker-entrypoint.sh` runs as root and then `gosu`-steps down to the `postgres` user; adding a `USER` directive would break `initdb` and the entrypoint's `chown`. The rule's premise — that the process runs as root in production — fails. Both alerts are also STALE: `# nosemgrep: dockerfile.security.missing-user-entrypoint.missing-user-entrypoint` sits on line 18 and `# nosemgrep: dockerfile.security.missing-user.missing-user` on line 20, added in commit `50b871f69` (2026-04-08) with rule ids matching the alerts exactly — current-HEAD Semgrep does not re-raise them, so what was needed here was Code Scanning alert-state cleanup, not a code change. |
| Re-check | If the base image is ever swapped for one whose entrypoint does not drop privilege, or a `USER` directive is added upstream of the entrypoint, re-verify before reusing this disposition. |

#### Alerts #1479, #1473, #1411, #1410, #1409, #1407, #1405, #1404 — `go/log-injection` — RBAC structured logging

| Field | Value |
|---|---|
| Alerts | Eight medium `go/log-injection` alerts: #1479, #1473, #1411, #1410, #1409, #1407, #1405, #1404. |
| Location | `services/control-plane/internal/rbac/handlers.go`: #1479 at :305; #1473 at :2237; #1411/#1410 at :3459; #1409 at :2724; #1407 at :2683; #1405/#1404 at :264. The pairs at :3459 and :264 are distinct traces to the same sink. |
| Dismissed | 2026-09-23, in PR #3140, via per-instance code-scanning dismissal. |
| Reason | `false positive` |
| Justification | Every sink was verified at PR head `dde6313b0`: a fixed-literal `h.log.Error` message with variable content only in structured key/value attributes. `Handler.log` is the project's `*logger.Logger`, which embeds `*slog.Logger`; production uses `slog.NewJSONHandler` and development uses `slog.NewTextHandler`. Both output-encode control characters, so an attribute cannot split or forge a log record. `TestHandlersEscapeControlCharsInAttrValues` in `services/control-plane/pkg/logger/logger_test.go` regression-locks both handlers. This is the Section 1 `go/log-injection` calibration case, independently rechecked for each of the six unique sinks; none is stdlib `log.Printf`, a dynamic message, or a raw payload. The alerts were a separate merge blocker despite green CodeQL jobs and were not waived by the PR's code-owner approval exception. |
| Re-check | Reopen the security question if a sink changes to stdlib `log.Printf`, a custom unescaped handler, a dynamic/concatenated message, or starts logging key material or raw payloads. |

#### Alerts #1487, #1488, #1489 — `go/log-injection` — MFA decrypt and step-up logging

| Field | Value |
|---|---|
| Alerts | Three medium `go/log-injection` alerts: #1487, #1488, #1489. |
| Location | `services/control-plane/internal/mfa/handlers.go`: #1487 at :457 (new step-up `VerifyCode` error log); #1488 at :677 and #1489 at :875 (existing `Failed to decrypt TOTP secret` logs that gained a `user_id` attribute, the edited-line shape). |
| Dismissed | 2026-09-24, in PR #3420, via per-instance code-scanning dismissal. |
| Reason | `false positive` |
| Justification | Each sink was verified at PR head `ac204f35e`: a fixed-literal `h.log.Error` message with variable content only in key/value attributes (`user_id`, `sealed_version`, `active_version`, `error`). `Handler.log` is `*logger.Logger`, which wraps `slog.NewJSONHandler` in production and `slog.NewTextHandler` in development; both output-encode control characters, and `TestHandlersEscapeControlCharsInAttrValues` in `services/control-plane/pkg/logger/logger_test.go` locks that. The attributes carry identifiers and error text only, never the TOTP secret, the submitted code, or key material. |
| Re-check | Reopen if a sink changes to stdlib `log.Printf`, a dynamic message, or starts logging the decrypted secret, the submitted code, or key material. |

#### Alerts #1512, #1513, #1514 — `go/log-injection` — DM purge failure logging

| Field | Value |
|---|---|
| Alerts | Three medium `go/log-injection` alerts: #1512, #1513, #1514. |
| Location | `services/control-plane/internal/dm/purge.go`: #1512 at :72 (new `DM purge role lookup failed` log), #1513 at :118 (`DM purge failed`, now also logging `deleted`), #1514 at :126 (`DM purge hide failed`, now also logging `deleted`). |
| Dismissed | 2026-09-24, in PR #3442, via per-instance code-scanning dismissal. |
| Reason | `false positive` |
| Justification | Each sink was verified at PR head `0672d39ed`: a fixed-literal `h.log.Error` message on the dm handler's `*logger.Logger` (`pkg/logger` slog), with variable content only in key/value attributes (`error`, `conversation_id`, `deleted`). The flagged value is the `:id` path parameter, which the handler has already rejected unless `uuid.Parse` accepts it; `deleted` is an integer. The rule-calibration entry above applies: the slog handler output-encodes attribute values. |
| Re-check | Reopen if a sink changes to stdlib `log.Printf`, a dynamic message, or starts logging message content, a password or MFA factor. |

#### Alerts #1401–#1403, #1421–#1423 — `go/log-injection` — `services/control-plane/internal/dm/handlers.go`

| Field | Value |
|---|---|
| Alerts | 6 `go/log-injection` alerts (medium): #1401, #1402, #1403, #1421, #1422, #1423 |
| Location | `services/control-plane/internal/dm/handlers.go:481,2388,3544,3766` — structured `h.log.Info` / `h.log.Error` sinks in open-conversation, credential-epoch guard, message deletion, and DM user-mute error paths |
| Dismissed | 2026-09-05, in PR #3116 (Closes #2907) |
| Reason | `false positive` |
| Justification | All six alerts flag structured `h.log.*` slog calls already present verbatim on the base branch and re-fingerprinted because this PR substantially edits the same file. Both configured handlers escape attacker-controlled attribute values at render time, so control characters cannot split log records. `TestHandlersEscapeControlCharsInAttrValues` proves the JSON and text handler behavior, and `.codex/rules/observability.md` §5 governs the output-encoding guarantee. These are not raw `log.Printf` sinks and must not gain per-site `sanitizeLogValue` wrapping. |
| Re-check | Re-verify that each future alert remains a `pkg/logger` slog sink and that the handler-escaping regression remains intact before reusing this disposition. A raw `log.Printf` sink is a true positive requiring `sanitizeLogValue`. |

#### Alerts #1490 / #1491 — Semgrep OSS SQL string formatting — migration 000159 test

| Field | Value |
|---|---|
| Alerts | #1490 (`CREATE DATABASE`, line 68) and #1491 (`DROP DATABASE`, line 75) |
| Location | `services/control-plane/internal/database/migration_000159_test.go` (original alerts were raised on the pre-rebase `000155` name) |
| Dismissed | 2026-09-23, in PR #3141, via per-instance code-scanning dismissal |
| Reason | `false positive` |
| Justification | The disposable database name is built from a fixed test prefix and `uuid.NewString()` with hyphens removed. It never contains request, user, environment, or database-derived input, and both SQL identifiers pass through `pq.QuoteIdentifier`. The exact cleanup target is the same test-local name. The line-scoped `nosemgrep` directives suppress only the required identifier assembly. This is the same pattern as the migration-expiration test covered by alerts #1433/#1434 and the Section 1 calibration. |
| Re-check | Re-verify the name source and `pq.QuoteIdentifier` call before reusing this disposition; an externally influenced or unquoted identifier is a true positive. |

#### Alert #1430 — Semgrep OSS `string-formatted-query` — `services/control-plane/internal/dm/handlers.go`

| Field | Value |
|---|---|
| Alert | #1430 (Semgrep OSS, `go.lang.security.audit.database.string-formatted-query.string-formatted-query`, warning) |
| Location | `services/control-plane/internal/dm/handlers.go:380–409`, the `queryConversations` statement, on PR #3715 head `8d25ae2d9daf679b564ed991e6d4a186ad3d63d5` |
| Dismissed | 2026-10-14, in PR #3715, by the developer |
| Reason | `false positive` |
| Justification | A re-fingerprint of a finding that `nosemgrep` has long suppressed. This is the same case as alert #1054. The only non-literal operand is `hiddenRangeFilter(1)`, which is `purge.HiddenRangeFilter("m", 1)`: a hard-coded table alias plus the placeholder `$1`. The user id binds through that placeholder. PR #3715 added one selected column (`dc.expiration_window_seconds IS NOT NULL AND dc.expiration_include_pinned`) inside the statement and left the concatenation unchanged, but Semgrep counts any content change in the matched region as a new finding. GitHub anchors the alert to line 380, an unchanged line, so the PR shows no annotation and the `Semgrep OSS` check passes. The protected `verify-pr-scan-results.py` checks the finding's whole region against the added lines, so the alert blocked `Required PR build result` until it was dismissed. |
| Re-check | This becomes a true positive if a value other than a compile-time alias and placeholder position is ever concatenated into this statement. |

#### Alert #1587 — `go/weak-sensitive-data-hashing` — `services/control-plane/internal/authoritycontract/crypto.go:20`

| Field | Value |
|---|---|
| Alert | #1587 (`go/weak-sensitive-data-hashing`, "Use of a broken or weak cryptographic hashing algorithm on sensitive data") |
| Location | `services/control-plane/internal/authoritycontract/crypto.go:20`, `Digest`'s `sha512.Sum384(b)`, on `refs/heads/main` |
| Dismissed | 2026-10-09 by the developer, without a `dismissed_comment`; this row backfilled 2026-10-15 in PR #3854 |
| Reason | `false positive` |
| Justification | Source `selection of PasswordLogin` (`authoritycontract/msg_published.go:153`), the 0/1 flag of the Section 1 note. `Digest` is the DoR §2.4 message digest ("All are SHA-384, 48 bytes") behind the intent, entry and evidence digests. No password reaches it. |
| Re-check | Reopen if a credential is ever passed to `Digest`. |

#### Alert #1624 — `go/weak-sensitive-data-hashing` — `services/control-plane/internal/authority/authoritytest/build.go:216`

| Field | Value |
|---|---|
| Alert | #1624 (`go/weak-sensitive-data-hashing`) |
| Location | `services/control-plane/internal/authority/authoritytest/build.go:216`, `h := sha256.Sum256(msg)` in `legacyPoP`; PR #3854, Go analysis 1957318275 of merge `0bb139412` |
| Dismissed | 2026-10-15, in PR #3854 |
| Reason | `false positive` |
| Justification | The SARIF code flow starts at `selection of PasswordLogin` (`authoritycontract/msg_published.go:153`). It crosses `m.spec()` at `codec.go:96` and leaves through `LegacyEKPoP.Encode()`, whose layout holds only realm_id, account_id, intent_digest and legacy_ek_digest (`msg_published.go:71-77`), so the path cannot carry the flag. The sink is the SHA-256 pre-hash of the fixture's RSA-PSS proof of possession. DoR §2.4 fixes legacy-key signatures as RSA-PSS SHA-256, MGF1-SHA-256, salt 32, and `VerifyLegacyPSS` (`authoritycontract/crypto.go:159-160`) hashes the same way. `authoritytest` is imported only by `_test.go` files. |
| Re-check | Reopen if `legacyPoP` ever hashes credential material, or if `authoritytest` gains a non-test importer. |

#### Alert #1625 — `go/weak-sensitive-data-hashing` — `services/control-plane/internal/authority/signer/signer.go:58`

| Field | Value |
|---|---|
| Alert | #1625 (`go/weak-sensitive-data-hashing`) |
| Location | `services/control-plane/internal/authority/signer/signer.go:58`, `h := sha512.Sum384(msg)` in `(*P384).Sign`; PR #3854, Go analysis 1957318275 of merge `0bb139412` |
| Dismissed | 2026-10-15, in PR #3854 |
| Reason | `false positive` |
| Justification | Same source as #1624. Every reported path crosses `m.spec()` at `codec.go:96` into a signed layout (`Sig`, `CPAuthz`, `Result`, `DeviceResult`). The sink is the RSK's ECDSA digest-before-sign over public protocol bytes. DoR §2.4 requires "ECDSA P-384, SHA-384 over the message bytes" for the RSK, and `VerifyP384` (`authoritycontract/crypto.go:79-84`) verifies against SHA-384, so any other hash makes every RSK signature unverifiable. |
| Re-check | Reopen if `Signer.Sign` is ever given credential material, or if the RSK algorithm moves away from DoR §2.4. |

#### Alert #1656 — `go/weak-sensitive-data-hashing` — `services/control-plane/internal/managedredis/controller.go:984`

| Field | Value |
|---|---|
| Alert | #1656 (`go/weak-sensitive-data-hashing`, "Use of a broken or weak cryptographic hashing algorithm on sensitive data") |
| Location | `services/control-plane/internal/managedredis/controller.go:984`, SHA-256 of the controller-only Redis ACL bootstrap password; PR #3940, Go analysis on `refs/pull/3940/merge` |
| Dismissed | 2026-10-18, in PR #3940 |
| Reason | `false positive` |
| Justification | Redis ACL's `#<hashedpassword>` rule requires exactly the SHA-256 hex digest of the shared secret. Redis itself hashes ACL passwords with SHA-256 and recommends 256-bit generated secrets instead of a slow human-password KDF ([Redis ACL documentation](https://redis.io/docs/latest/operate/oss_and_stack/management/security/acl/)). This code writes only the digest to the root-managed ACL file; a different hash would make Redis `AUTH` fail. The managed Redis runbook now requires independent CSPRNG-generated secrets with at least 256 bits before activation. This dismissal does not attest to any live secret's entropy or authorize production activation. |
| Re-check | Reopen if this digest is used outside Redis ACL, the target Redis ACL format changes, or credential generation is found to violate the runbook requirement. |

## Post-migration (Self-Hosted)

### Rule-Calibration Notes

_None yet._

### Instance-Specific Entries

_None yet._

