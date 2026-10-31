# Semgrep False Positive Register

> **Split from the Concord-Voice-Alpha register at the repo split (Alpha @771eb3c).** The Legacy section keeps the 10 entries that concern code now in this repo, as decided in Alpha. New decisions for this repo go under Post-migration.

| Field          | Value      |
|----------------|------------|
| Effective Date | 2026-05-11 |
| Version        | 1.0        |

## Purpose

This file is the canonical record of Semgrep false-positive decisions for the Concord Voice project. When you suppress a Semgrep finding inline with a `# nosemgrep:` annotation, append a matching entry here. That preserves the reasoning for future reviewers.

The register exists because inline `nosemgrep:` annotations carry only a short rationale comment near the code. Cross-cutting context — "why this rule is mismatched against the project's idiom" — does not fit alongside the suppressed line. Without a central record, every reviewer re-discovers it. The Semgrep MCP plugin also runs Pro rules locally. Those may differ from the rule set Concord's CI Semgrep job runs (`semgrep.yml`, a standalone PR-only workflow since #1426, uses `.semgrep/` custom rules + `p/security-audit` + `p/owasp-top-ten`). Pro rules sometimes false-positive on patterns CI accepts.

### Relationship to the SonarQube FP Register

`docs/ci/sonarqube-false-positive-register.md` is the canonical record for **SonarQube** false-positives, governed by Concord's SonarQube AI-Code Assurance policy. This register is the **Semgrep** equivalent. The two are separate because:

- SonarQube's AI-Code Assurance enforces a strict no-rule-tuning posture. This register has no equivalent assurance gate.
- Semgrep findings can be suppressed in code (`nosemgrep:`) without any platform API call. SonarQube findings need an API call.
- Rule IDs and severities are not comparable across the two tools.

### Suppression mechanics

Inline suppressions in code MUST cite the specific rule ID (per project memory `project_semgrep_mcp_hooks_config`):

```yaml
# nosemgrep: <full.rule.id>
problematic-line-here
```

Bare `# nosemgrep` (no rule ID) is too broad — it suppresses ALL rules at that line and silently hides future findings as the rule set grows. Always list the specific rule ID.

The annotation must appear on the line **immediately above** the finding, or as an inline suffix on the same line. Intervening comment lines break the proximity heuristic.

## Legacy (Concord-Voice-Alpha, before the repo split)

### Section 1 — Rule-Calibration Notes

High-level stances on Semgrep rules whose mismatch with this project's idiomatic code is **structural**. Any future occurrence of the rule on a similar code pattern merits the same FP disposition. Entries are sorted alphabetically by rule ID.

#### `go.lang.security.audit.database.string-formatted-query.string-formatted-query`

Semgrep flags SQL statements assembled with identifier fragments even when every
fragment is either selected from a closed set of test-local literals or is a
migration-owned catalog value passed through `pq.QuoteIdentifier`. PostgreSQL
does not permit identifiers to be supplied as query parameters. Treat an
instance as a false positive only when every interpolated fragment is either a
closed-set test literal with no external data flow or an identifier protected by
`pq.QuoteIdentifier`, and every interpolated non-identifier value remains
parameterized.
Unquoted identifiers outside a closed test-only set, interpolated values, and
identifiers derived from request input are not covered by this calibration note.


### Section 2 — Instance-Specific Entries

Per-finding justifications for specific suppressions. Each entry names the file, line, rule, and reasoning.

#### Closed media upstream map on public Socket.IO routes (#3876)

| Field | Value |
|---|---|
| File | `infrastructure/deploy/nginx/concordvoice.conf`, lines 293 and 469 |
| First recorded | 2026-10-15, PR #3876 |
| Rules | `generic.nginx.security.dynamic-proxy-host.dynamic-proxy-host` and `generic.nginx.security.missing-internal.missing-internal` (two findings each) |
| Suppressed by | Both rule IDs in the `# nosemgrep:` line immediately above each `proxy_pass` |
| Rationale | `$media_plane_upstream` comes only from the closed `$arg_colour` map: `blue` selects the local blue upstream, `green` selects the local green upstream, and every other value selects blue. The request cannot supply an upstream host. Both `/socket.io/` locations must accept client signalling handshakes, so `internal` would break the route rather than prevent SSRF. |
| Verification | The local community rulesets reported these four findings before suppression and zero findings on the nginx file after it; the map and both public locations were inspected. |
| Disposition | Rule-specific inline suppressions for these two locations only; revisit if the map or route exposure changes. |

#### `go.gin.path-traversal.gin-path-traversal-taint.gin-path-traversal-taint`

| Field | Value |
|---|---|
| File | `services/control-plane/internal/spa/handler.go` |
| Approximate line | 141 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-05-28 (issue [#977](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/977)) |
| Rule | `go.gin.path-traversal.gin-path-traversal-taint.gin-path-traversal-taint` (Pro ruleset, `p/gosec`) |
| Suppressed by | `// nosemgrep:` on the line immediately above the `fs.Open(path.Join(...))` call |
| Rationale | The rule traces the gin-derived `parts[]` slice (from `c.Param("filepath")` → `strings.SplitN`) into the `fs.Open` sink as untrusted user input. Two independent mechanisms defend the code structurally. First, `http.Dir(h.spaDir)` on the line above: Go's built-in `http.FileSystem` sandbox prevents path-traversal by design, and the in-tree comment immediately above the `fs := http.Dir(...)` line notes this. Second, `resolveSafePath(h.spaDir, parts[0], parts[1])` called earlier in `ServeAsset` (defined in the same file): it validates the version segment via `isValidVersion`, calls `filepath.Clean`, rejects `..` segments, and asserts the resolved path stays under `absSpaDir + "/"` via `strings.HasPrefix`. The Semgrep taint rule pattern-matches the sink without seeing these structural guards. |
| Verification | `semgrep --config p/gosec services/control-plane/internal/spa/handler.go --json \| jq '.results \| length'` returns `0` after the directive is applied. Existing tests `cd services/control-plane && go test ./internal/spa/...` still pass — no functional change. |
| Disposition | Inline suppression. The defense is structural to this handler's `ServeAsset` design; the same pattern would NOT appear elsewhere in the codebase (this is the only SPA-static-asset handler). No Section 1 rule-calibration note needed. |

#### `go.lang.security.audit.database.string-formatted-query.string-formatted-query`

| Field | Value |
|---|---|
| File | `services/control-plane/internal/database/opsmetrics_reader_test.go` |
| Approximate lines | 209-579 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-07-14 (PR [#2228](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2228), GitHub code-scanning alerts [#978](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/978)-[#990](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/990)) |
| Rule | `go.lang.security.audit.database.string-formatted-query.string-formatted-query` (`p/security-audit` / `p/owasp-top-ten` community scan) |
| Suppressed by | `// nosemgrep:` on the line immediately above each of the 13 `ExecContext` calls; each directive names only this rule and states whether the identifier is fixed or migration-owned. The corresponding GitHub code-scanning alerts are dismissed individually as `false positive`, not accepted. |
| Rationale | These integration tests must create, grant, alter, revoke, and remove PostgreSQL roles and databases. PostgreSQL query parameters cannot represent identifiers. Every dynamic identifier is either a test-local constant or the role name read from migration-owned PostgreSQL catalog state, and every one is passed through `pq.QuoteIdentifier` before it enters SQL text. No request, user, environment, or other externally supplied value is interpolated. The rule recognizes string assembly but does not model `pq.QuoteIdentifier` as the required identifier sanitizer. |
| Verification | Pinned Semgrep 1.121.0 with the CI community configs (`p/security-audit` and `p/owasp-top-ten`) reports zero findings after the directives. The isolated PostgreSQL integration slice `go test ./internal/database -run 'Test(Migration000088\|OpsMetricsReader)' -count=1` passes against PostgreSQL 16, including role-membership, privilege-drift, HBA, and lifecycle checks. |
| Disposition | False positive. Retain the line-scoped directives and GitHub instance-level false-positive dismissals. Re-evaluate if any identifier source or quoting path changes. |

| Field | Value |
|---|---|
| File | `services/control-plane/internal/dm/handlers.go` |
| Approximate lines | 318–339 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-09-07 (alert [#1430](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/1430)) |
| Rule | `go.lang.security.audit.database.string-formatted-query.string-formatted-query` (`p/security-audit` / `p/owasp-top-ten`) |
| Suppressed by | Existing rule-specific `// nosemgrep:` immediately above the `queryConversations` statement at line 317. |
| Rationale | The query concatenates only the fixed SQL fragment produced by `hiddenRangeFilter` (`internal/dm/hidden_ranges.go:107-108`), which fixes the table alias to `m`; the authenticated `userID` remains the `$1` bound parameter. `purge/hiddenfilter.go:20-25` generates only the fixed alias and integer placeholders. No request-shaped identifier or value enters the SQL text. |
| Verification | Static security review traced the fragment through both helper paths and confirmed the suppression is limited to the identifier/placeholder assembly. No remote GitHub dismissal was performed for alert #1430. |
| Disposition | False positive under the Section 1 calibration. Retain the existing rule-specific suppression; re-evaluate if the filter fragment or identifier source changes. |

| Field | Value |
|---|---|
| File | `services/control-plane/internal/expiration/migration_integration_test.go` |
| Approximate lines | 85, 94 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-09-07 (PR #3144) |
| Rule | `go.lang.security.audit.database.string-formatted-query.string-formatted-query` (`p/security-audit` / `p/owasp-top-ten`) |
| Suppressed by | `// nosemgrep:` immediately above the `CREATE DATABASE` and cleanup `DROP DATABASE` statements. The GitHub code-scanning alerts [#1433](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/1433) (line 85) and [#1434](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/1434) (line 94) were dismissed individually as `false positive` at `fc359db6a`; no broad rule tuning was applied. |
| Rationale | The disposable database name is generated locally from a UUID and is never request, user, or environment input. PostgreSQL identifiers cannot be query parameters; both statements use `pq.QuoteIdentifier` before assembling SQL text. The rule flags the required identifier assembly without modeling the sanitizer. |
| Verification | Focused migration round-trip test passes with race detection against PostgreSQL 16 (2.730s). Pinned Semgrep 1.121.0 with `p/security-audit` and `p/owasp-top-ten` returns zero unsuppressed findings in the JSON result; its SARIF output retains the two source-suppressed results and recognizes both suppressions. No externally supplied value reaches either statement. |
| Disposition | False positive under the Section 1 calibration. Retain the rule-specific inline directives and the individual GitHub dismissals; do not broaden the rule suppression. Re-evaluate if the identifier source or quoting path changes. |

#### `go.lang.security.audit.database.string-formatted-query.string-formatted-query` — security-event member audit fixture

| Field | Value |
|---|---|
| File | `services/control-plane/internal/members/security_event_test.go` |
| Approximate lines | 171, 175, and 178 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-09-05 (issue [#2273](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2273)) |
| Rule | `go.lang.security.audit.database.string-formatted-query.string-formatted-query` (`p/security-audit` / `p/owasp-top-ten` community scan) |
| Suppressed by | Rule-specific `// nosemgrep:` comments immediately above the test-only `CREATE TRIGGER`, `DROP TRIGGER`, and `DROP FUNCTION` statements. |
| Rationale | The test must vary PostgreSQL event and table identifiers, which cannot be query parameters. `mutationEvent` and `mutationTable` come only from the three literal test cases (`UPDATE`/`DELETE` and `server_members`/`server_bans`); the remaining identifiers are local constants. No request, fixture, environment, database result, or other external value reaches the SQL text. |
| Verification | The CI community configs (`p/security-audit` and `p/owasp-top-ten`) report zero findings on the affected files after the directives, and `go test -c ./internal/members` compiles the integration-test package. |
| Disposition | False positive, test file only. Re-evaluate if an identifier source becomes external or the closed literal set is removed. |

#### `go.lang.security.audit.sqli.gosql-sqli.gosql-sqli`

| Field | Value |
|---|---|
| File | `services/control-plane/internal/media/handlers_test.go` |
| Approximate line | 113 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-05-19 (PR for issue #1026) |
| Rule | `go.lang.security.audit.sqli.gosql-sqli.gosql-sqli` (Pro ruleset, `auto` config) |
| Suppressed by | `# nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,go.lang.security.audit.sqli.gosql-sqli.gosql-sqli` on the line immediately above the `db.Exec(fmt.Sprintf(...))` call |
| Rationale | The flagged code is a test-cleanup helper that issues `DELETE FROM <table>` for a hardcoded list of table names stored in a `[]string` slice defined two lines above. The `table` variable is entirely derived from a compile-time constant slice — there is no user input, no external data, and no runtime-variable table names. The Pro taint rule (`gosql-sqli`) traces `fmt.Sprintf` → `db.Exec` as a taint sink without distinguishing that the format argument originates from a hardcoded slice. The existing OSS suppression (`string-formatted-query`) was already present in the original codebase before this PR; the Pro rule fires in addition and requires the extended rule ID list. |
| Verification | The `tables` slice is defined on the immediately preceding line with string literals only. `grep -n 'tables :=' handlers_test.go` confirms no external input flows into the slice. |
| Disposition | Inline suppression, both OSS and Pro rule IDs listed. The `//nolint:gosec` directive is also present for the `golangci-lint` `G201` check. This pattern is structural to test cleanup in this codebase and would merit a Section 1 rule-calibration note if it appears in other test files. |

#### `go.jwt-go.security.jwt.hardcoded-jwt-key`

| Field | Value |
|---|---|
| File | `services/control-plane/internal/middleware/auth_test.go`, `services/control-plane/internal/middleware/auth_tokenclass_test.go`, `services/control-plane/internal/websocket/handler_test.go`, `services/control-plane/internal/mfa/security_event_test.go` |
| Approximate line | `auth_test.go` `makeToken`; `auth_tokenclass_test.go` in `TestBothReadersRejectAnyPurposeClaim`; `handler_test.go` 378, 399, 421, 594; `mfa/security_event_test.go` 251 and 296 (line numbers shift with PRs) |
| First recorded | 2026-08-22 (PR [#2900](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2900)) — the four `handler_test.go` suppressions predate this entry and are **backfilled** here; the MFA fixture was added 2026-09-05 for issue [#2273](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2273) |
| Rule | `go.jwt-go.security.jwt.hardcoded-jwt-key` (`p/security-audit`) |
| Suppressed by | `// nosemgrep:` on the line immediately above the `SignedString([]byte(secret))` call |
| Rationale | The rule flags a literal passed as a JWT signing key. Every flagged site is a **test fixture minting a token for the validator under test**. A validator test cannot exercise signature verification without holding the key both sides use, and the alternatives are worse: reading a real secret into the test suite or making deterministic expiry/classification tests depend on external state. The keys are test-local constants or the literal `"test"`; they appear in no production path and authenticate nothing. |
| Verification | `git grep -n 'testJWTSecret\|tokenClassSecret' -- ':!*_test.go'` returns nothing. The CI community configs report zero findings on the affected files after the directives, and the two targeted MFA classification tests pass. |
| Disposition | Inline suppression, test files only. A production hit for this rule is NOT a false positive and must not be suppressed by citing this entry. |

#### `semgrep.concord-go-sql-sprintf`

| Field | Value |
|---|---|
| File | `services/control-plane/cmd/migrate-media/main.go` |
| Approximate lines | 381, 436, 578 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-08-30 (PR [#3043](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/3043)) |
| Rule | `semgrep.concord-go-sql-sprintf` (in-repo `.semgrep/concord-go-security.yml`) |
| Suppressed by | Rule-specific `// nosemgrep:concord-go-sql-sprintf` suffixes on the opening line of each of the three multiline `fmt.Sprintf` calls. |
| Rationale | The formatted fragments are column identifiers selected only from the two hard-coded profile migration options (`avatar_url` and `header_image_url`) or from the strict manifest parser that maps exact `avatars/<canonical-uuid>` and `banners/<canonical-uuid>` entries to those same identifiers. User IDs, original URLs, and replacement URLs remain `$N` query parameters; no external value enters SQL text. |
| Verification | Pinned Semgrep 1.121.0 reports zero findings when the CI custom config is emitted in one output format. The combined JSON + SARIF invocation preserves the ignored matches in SARIF with `suppressions[].state = accepted`; manual review confirms the identifier sources and parameter boundary above. The targeted migrate-media package tests pass. |
| Disposition | False positive. Retain the rule-specific suppressions and dismiss GitHub code-scanning alerts [#1366](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/1366)-[#1371](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/code-scanning/1371) individually as `false positive`; re-evaluate if either identifier source changes. |

#### Platform-triaged AI findings — MFA settings step-up and SuppressEmbeds authorization review

| Field | Value |
|---|---|
| File | Ten findings across `services/control-plane/internal/api/router.go`, `services/control-plane/internal/auth/handlers.go`, `services/control-plane/internal/mfa/handlers.go`, and `services/control-plane/internal/rbac/handlers.go` — see the per-finding table below |
| Approximate line | Per finding, below (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-09-23, branch `fix/mfa-step-up-and-suppress-embeds`, per the [MFA settings step-up and SuppressEmbeds design](../superpowers/specs/2026-09-23-mfa-step-up-and-suppress-embeds-design.md) §1 and §8 |
| Rule | N/A — these are AI-detected findings surfaced by the Semgrep platform's AppSec review, not a `.semgrep/` custom rule or a Pro/OSS ruleset match. Platform AI findings of this kind carry no rule ID, so the usual `<full.rule.id>` field does not apply here. |
| Suppressed by | N/A — no inline `# nosemgrep:` annotation exists or was added for any of the ten. With no rule ID there is nothing to cite in a code annotation, so the disposition belongs at the finding level in the Semgrep platform UI, not in source. As of 2026-09-23 that platform-side dismissal has NOT been performed — it is a developer action. |
| Rationale | See the per-finding table below — one line each, grouped by finding class. |
| Verification | The six pending-registration findings were adjudicated SAFE by the [2026-07-17 vulnhunt security audit](../reference/security-audits/2026-07-17-vulnhunt/report.md), partition SG-A. The three MFA AUTHZ findings and the one rbac SQLi finding were verified by direct source read (constant-time comparison and JWT-derived user ID; both flagged code versions' SQL construction, respectively) during the design's source review. |
| Disposition | False positive, all ten (triage verdict; platform-side dismissal pending). These are 10 of the 14 findings named in the design's §1 Problem section; the remaining 4 were real defects — see the fixed-findings table immediately below, which is NOT a false-positive record but is kept here for a complete account of all 14. |

| # | File | Approximate line | Finding class | Rationale |
|---|---|---|---|---|
| 1 | `services/control-plane/internal/api/router.go` | 833 | Pending-registration IDOR | `pending_id` is `uuid.New()` — 122 random bits — returned only in the registrant's own `201` response. |
| 2 | `services/control-plane/internal/api/router.go` | 903 | Pending-registration IDOR | Same as above. |
| 3 | `services/control-plane/internal/api/router.go` | 951 | Pending-registration IDOR | Same as above. |
| 4 | `services/control-plane/internal/auth/handlers.go` | 825 | Pending-registration IDOR | Same as above. |
| 5 | `services/control-plane/internal/auth/handlers.go` | 895 | Pending-registration IDOR | Same as above. |
| 6 | `services/control-plane/internal/auth/handlers.go` | 943 | Pending-registration IDOR | Same as above. |
| 7 | `services/control-plane/internal/mfa/handlers.go` | 1962 | MFA AUTHZ | The user ID comes only from the JWT (never a path or body parameter); setup requires password + a current MFA code; every submitted code is compared in constant time against the user's own Redis-held key. |
| 8 | `services/control-plane/internal/mfa/handlers.go` | 2003 | MFA AUTHZ | Same as above. |
| 9 | `services/control-plane/internal/mfa/handlers.go` | 2086 | MFA AUTHZ | Same as above. |
| 10 | `services/control-plane/internal/rbac/handlers.go` | 691 | SQL injection | Both code versions at that line build the SQL statement from constant column names and generated `$N` placeholders only — no interpolated value or identifier is attacker-controlled. |

**Real findings fixed by this PR (not false positives — recorded for a complete account of the 14 named in the design).** Branch `fix/mfa-step-up-and-suppress-embeds`:

| # | File | Approximate line | Finding | Status |
|---|---|---|---|---|
| 1 | `services/control-plane/internal/messages/handlers.go` | `SuppressEmbeds` (:1050) | Authorization checked after reading suppression state | Fixed, commit `e0974a62a` |
| 2 | `services/control-plane/internal/mfa/handlers.go` | `EmailSmsDisable` (:2551) | No step-up before disabling a security control | Fixed, commit `1dcbdf347` |
| 3 | `services/control-plane/internal/mfa/handlers.go` | `SetBackupEmail` (:2683) | No step-up before changing the recovery channel | Fixed, commit `1dcbdf347` |
| 4 | `services/control-plane/internal/mfa/handlers.go` | `StoreRecoveryKey` (:2770) | No step-up before overwriting the recovery key | Fixed, commit `1dcbdf347` |

Corrected 2026-09-24: the four rows above originally cited `services/control-plane/internal/api/router.go` at lines 1049/1962/2041/2138, which do not correspond to this code — `router.go` only registers routes, it does not implement the handlers the findings describe. The file:line pairs above point at the actual handler functions.

#### `go.lang.security.audit.database.string-formatted-query.string-formatted-query` — migration 000159 test database

| Field | Value |
|---|---|
| File | `services/control-plane/internal/database/migration_000159_test.go` (original alerts were raised on the pre-rebase `000155` name) |
| Approximate lines | 68, 75 (line numbers shift with PRs; see latest commit) |
| First recorded | 2026-09-23 (PR #3141; GitHub code-scanning alerts #1490/#1491) |
| Rule | `go.lang.security.audit.database.string-formatted-query.string-formatted-query` (`p/security-audit` / `p/owasp-top-ten`) |
| Suppressed by | `// nosemgrep:` immediately above the `CREATE DATABASE` and cleanup `DROP DATABASE` statements; each directive names this rule and cites `pq.QuoteIdentifier`. |
| Rationale | The disposable database name uses a fixed `concord_migration_000159_` prefix plus `uuid.NewString()` with hyphens removed. It has no request, user, environment, or database-derived input, and both statements pass the identifier through `pq.QuoteIdentifier`; cleanup targets that exact test-local name. The rule flags the required PostgreSQL identifier assembly without modeling the sanitizer. |
| Verification | Source review confirms the name is locally generated and both SQL identifiers are quoted. Code Scanning alerts #1490/#1491 were dismissed individually as false positives in PR #3141. |
| Disposition | False positive. Retain the rule-specific directives and per-instance dismissals; re-evaluate if the identifier source or quoting path changes. |


## Post-migration (Self-Hosted)

### Rule-Calibration Notes

_None yet._

### Instance-Specific Entries

_None yet._

## Adding a new entry

When suppressing a new Semgrep finding:

1. Verify the finding is genuinely false-positive by inspecting the flagged code and running the Pro rule directly: `semgrep --config p/secrets <file>` or equivalent.
2. Identify the full rule ID: `semgrep --config p/secrets --json <file> | jq '.results[].check_id'`.
3. Add a `# nosemgrep: <rule-id>` annotation on the line immediately above the finding, with a one-sentence rationale comment.
4. Add an entry to **Section 2** above with: file, line, first recorded date + PR, rule ID, suppression mechanism, rationale, verification steps, disposition.
5. If the rule is structurally mis-calibrated (would fire on many similar patterns in this codebase), also add a **Section 1** rule-calibration note.
6. Commit both the code change and the register update in the same PR.

## Known Gap

The last full inventory below is a historical 2026-08-28 snapshot: the register
then recorded 7 decisions while the tracked tree carried 59 suppression lines.
As of 2026-09-06, the tree carries 67 suppression lines across 24 files and
Section 2 has 9 instance-specific decision entries, several of which group
multiple lines. The register remains incomplete, and the gap is not explained
by its 2026-05-11 start date.

Re-measure with a repo-wide `git grep`, never a directory list — a directory list is
what produced the earlier wrong claim that no pre-register `nosemgrep:` annotations
existed:

```bash
git grep -c "nosemgrep" -- services client infrastructure
git grep -ln "nosemgrep" -- services client infrastructure
```

Historical inventory as of 2026-08-28:

| Area | Files | Suppressions | Recorded in Section 2 |
|---|---:|---:|---:|
| `services/control-plane` — Go source and tests | 19 | 49 | 7 |
| `infrastructure/deploy/nginx/concordvoice.conf` | 1 | 5 | 0 |
| `infrastructure/docker/nginx/nginx.conf` | 1 | 3 | 0 |
| `infrastructure/docker/postgres/Dockerfile` | 1 | 2 | 0 |
| `client/` | 0 | 0 | — |
| **Total** | **22** | **59** | **7** |

Two things the earlier wording got wrong:

- **The gap is not a pre-register gap.** At least 30 of the 49 in-scope Go
  suppressions were first introduced after 2026-05-11, so the start date does not
  account for them. The largest single holder is
  `services/control-plane/internal/database/opsmetrics_reader_test.go` with 13, added
  2026-07-14.
- **The previous table was not the whole config-file set.**
  `infrastructure/deploy/nginx/concordvoice.conf` carries 5 suppressions and appeared
  in neither Section 2 nor that table.

**Backfill Section 2, or record a deliberate and explicit exclusion** — for config
files, for test files, or for both. Until one of those happens, do not read this
register as a complete account of what is suppressed, and do not read "covers Go and
TypeScript source" as meaning that scope is fully covered. It is not.
