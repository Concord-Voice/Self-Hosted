# .zap/: OWASP ZAP API DAST (#1465)

ZAP Automation Framework plans plus a compose override, for dynamic application
security testing of the control-plane REST API.
[`.github/workflows/zap-api-dast.yml`](../.github/workflows/zap-api-dast.yml)
runs them: nightly via nightly-ci (`plan: full`, advisory) and on operator dispatch.
ZAP was removed from PR CI on 2026-08-26 — a DAST run needs a full compose stack, so
it was the most expensive thing on the PR path, and its verdict moves with the
dependency tree and the ZAP engine image rather than only with the diff. Nightly is
both cheaper per PR and better coverage than the weekly cron it replaced.

| File | Purpose |
|---|---|
| `pr.yaml` | Light plan: OpenAPI import → passive baseline + 5-min light active scan. Named for the PR path it was written for; now reachable only via `workflow_dispatch` with `plan: pr`. Carries the canonical header (never-production rule, REST-only scope honesty, per-exclusion justifications). |
| `full.yaml` | Nightly full plan (renamed from `weekly.yaml` 2026-08-26 — the filename was asserting a cadence it no longer has): same context, 10-min active scan (depth ceiling = the 15-minute `AccessTokenTTL` — see that file's TTL math before raising it). |
| `docker-compose.zap-ci.yml` | CI override layering `CONCORD_ENV=test` + `SMTP_HOST=""` onto the base compose `control-plane` service. |
| `sarif-postprocess.py` | Rewrites ZAP's URL-based finding locations to repo-relative paths so GitHub Code Scanning accepts them (see "SARIF → Code Scanning" below). Locked by `test-sarif-postprocess.sh`. |
| `DAST-FINDINGS.md` | The sentinel file every `zap-api` alert anchors to. The file explains why. |

## Invariants

- **Never production.** The target is the ephemeral in-job compose stack at
  `http://localhost:8080`. Never scan `api.concordvoice.chat`, and never
  scan any deployed host. The Cloudflare proxy at that hostname is
  load-bearing: a scan through it distorts results and trips bot mitigation.
- **REST surface only.** WebSocket hub, media-plane Socket.IO/WebRTC, and
  NATS are out of scope. A green DAST run says nothing about them.
- **Authenticated scanning is load-bearing.** The replacer rule injects a
  seeded test-user JWT. The workflow hard-fails if the control-plane log does
  not show ZAP traffic reaching a JWT-gated route with a 200.
- **The OpenAPI spec is the scan surface**. A drift gate checks `docs/api/openapi.yaml`
  against the live router on every PR (#822's `verify-openapi-coverage`), so
  scan coverage tracks the real API.

## False-positive discipline

Mirrors the SonarQube / CodeQL instance-level FP registers, with two
structural requirements ZAP imposes that the static registers do not:

- **`alertFilter` runs at alert-RAISE time, not retroactively.** The
  `alertFilter` job MUST appear in the plan BEFORE the alert-generating jobs
  (`openapi` → `requestor` → `activeScan` → `passiveScan-wait`). A filter that
  registers *after* the scans is a silent no-op. ZAP reclassifies only alerts it
  raises *after* the filter installs. Founding incident, 2026-06-18: the block
  sat after `passiveScan-wait`, so ~30 advisory alerts reached Code Scanning
  un-suppressed despite "correct" filters. The fix was to reorder the job, plus
  the report-confidences exclusion below.
- **The report job must drop ONLY `falsepositive` confidence**. A reclassify to
  "False Positive" does NOT remove an alert from the SARIF. The report job's
  default `confidences: all` re-includes it. `confidences` is an
  *allowlist* over `{falsepositive, low, medium, high, confirmed}`, so each
  plan's `report` job lists every level EXCEPT `falsepositive`:
  `confidences: [confirmed, high, medium, low]` (sibling of `parameters:`).
  `confirmed` MUST be present. It is ZAP's strongest signal, and omitting it
  silently drops confirmed vulnerabilities. That is the inverse of the intent
  (gitar #1635). Removing the whole key re-leaks the FPs.
- **Scope strength tracks FP nature**. There are two tiers, repo-admin-approved
  2026-06-18 by @markdrogersjr, and finalized after the gitar review of #1635.
  The default is **per-instance** (`ruleId` plus the specific `url`). Five
  classes use the broader **per-rule, API-wide** scope (`ruleId` scoped to
  `^.../api/v1/.*`):
  - `40018` SQL Injection is API-wide for **two** reasons.
    *Structural FP:* every control-plane handler `uuid.Parse`-guards its `{id}`
    or `{user_id}` path param, returning HTTP 400 before any query, and binds
    only `$1`/`$2`. There is no `fmt.Sprintf` and no concatenated SQL. Three
    static layers also gate SQLi: gosec, CodeQL `go/sql-injection`, and Semgrep,
    the last two merge-gated (ADR-0017). DAST 40018 is therefore the weakest,
    redundant SQLi layer.
    *Empirical:* a narrower scope does **not** work. The ZAP `alertFilter`
    url-regex is full-match, and it fails to suppress **deep multi-segment
    path-injection** alerts even when the regex matches in a standard engine.
    #1635 proved this: a family-scoped 40018 left the deep
    `servers/{id}/channel-groups/{group_id}` FP un-suppressed (1 SARIF result),
    while bare `/api/v1/.*` suppressed it (0). The original per-instance
    `servers/[^/]+/channel-groups` likewise never matched that deep URL, which is
    why it stayed a persistent open alert. Bare `/api/v1/.*` is therefore the
    only reliably-matching scope, and the periodic review below is the
    time-boxing.
  - `90022` Application Error Disclosure is the `CONCORD_ENV=test` verbose-error
    artifact. Production returns opaque errors, so every 90022 here is that
    test-mode artifact.
  - `10104` User Agent Fuzzer / `10111` Authentication Request Identified —
    scanner self-observations, never server defects.
  - `10024` Information Disclosure (sensitive info in URL) is the by-design
    recovery-link token in the path. CI uses dummy values.
  - **Per-instance.** `rule 6` (Path Traversal) stays per-instance, because its
    FP-ness is NOT structural. The filter therefore covers only the verified
    `/users/me/preferences` instance, a Postgres-blob read that takes no
    filesystem path from input.
- **Periodic review.** The five API-wide classes are "verified once, trusted
  until re-checked." Re-verify them each release cycle. Re-run the full scan.
  Confirm no *new* attack class hides behind a wildcard. Re-confirm the
  structural argument still holds, so handlers still uuid-guard and
  parameterize. This time-boxing makes the broad scopes a conscious, bounded
  decision rather than a silent-forever wildcard (gitar #1635).
- **A caveat on the `40018` wildcard**. Because the scope is `/api/v1/.*`, a
  *future* endpoint with a genuine SQLi defect would have its DAST 40018 alert
  suppressed. Concord accepts that residual risk. The three
  merge-gated static layers above own SQLi, and DAST is advisory and non-gating.
  ZAP 40018 precision on this uuid-guarded surface is ~0%, because every flagged
  instance to date is an FP. The periodic review is the compensating control.
- **Never delete at rule level** to silence a finding class. That means never
  removing scan rules from the policy, and never using a bare `ruleId` filter
  with no `url` scope. Even the broad forms keep a URL regex that bounds them to
  the scanned surface (API-wide) or to verified families (40018).
- Keep `pr.yaml` and `full.yaml` `alertFilter` + report blocks **mirrored**
  — the only intended difference between the two plans is scan depth.

## SARIF → Code Scanning (the URL-vs-file impedance mismatch)

ZAP is a DAST tool: it reports each finding's location as the **URL it
scanned** (`http://localhost:8080/api/v1/...`). GitHub Code Scanning anchors
every alert to a **file in the repo checkout**. It rejects a SARIF whose
location URIs carry a non-`file` scheme:

> SARIF URI scheme "http" did not match the checkout URI scheme "file"

The `sarif-postprocess.py` script runs between the "Locate SARIF" and "Upload
SARIF" workflow steps. It resolves this by re-anchoring every finding onto
`DAST-FINDINGS.md` (`uriBaseId: %SRCROOT%`). It preserves the real scanned URL
in the alert message (`[http://localhost:8080/...] <original message>`) and in
`properties.originalUri`. It leaves `rule.helpUri` and other non-location
URIs untouched. The script fails loud if any `http(s)` location URI survives the
rewrite. `test-sarif-postprocess.sh` tests it hermetically.

## Running locally

```bash
# Bring up the test stack (from the repo root; needs a stub .env — see the
# "Write CI stub .env" step in zap-api-dast.yml for the required names):
docker compose -f docker-compose.yml -f .zap/docker-compose.zap-ci.yml --profile services up -d --build control-plane

# Seed a user + capture a JWT (see the workflow's seeding step), then:
# (-port 8090: ZAP binds a local proxy even in -cmd mode, 8080 by default —
# the control-plane's port; the collision is fatal yet exits 0.)
docker run --rm --network host -v "$PWD:/zap/wrk" \
  -e ZAP_AUTH_HEADER_VALUE="Bearer <token>" \
  ghcr.io/zaproxy/zaproxy:stable \
  zap.sh -cmd -port 8090 -autorun /zap/wrk/.zap/pr.yaml
```

(On macOS Docker Desktop, swap `--network host` for
`--network container:<control-plane container>` — same effect for this
purpose.)

(Local runs may float on `stable`. CI pins by digest. The upgrade procedure is
in the workflow header.)
