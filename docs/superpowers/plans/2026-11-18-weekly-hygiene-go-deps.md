# Weekly hygiene: Go dependency risks — Plan

Spec: `docs/superpowers/specs/2026-11-18-weekly-hygiene-go-deps-design.md`

## Tasks

1. **Map each finding to its source.** Run `go mod why -m` and `go mod graph` for each
   flagged module. Expected: every edge comes from `golang-migrate/migrate/v4@v4.20.1`.
2. **Find the first fixed version** for each CVE from OSV / GHSA.
3. **Bump golang-migrate to upstream master** (merged fix PR #1458):
   `go get github.com/golang-migrate/migrate/v4@v4.20.2-0.20261116072128-a746b101346e`.
   Confirm with `gh api repos/golang-migrate/migrate/compare/v4.20.1...a746b101346e` that
   it is master HEAD and is only test-harness and dependency changes. Check the new graph
   modules on OSV.
3b. **Bump `go.opentelemetry.io/otel/trace` to v1.47.0**:
   `go get go.opentelemetry.io/otel/trace@v1.47.0 && go mod tidy`.
   Expected diff: `otel/trace` and `otel/metric` v1.47.0 as `// indirect` in `go.mod`,
   plus the matching `go.sum` lines.
4. **Prove that pins on the other modules do not survive tidy.** `go get` the fixed
   versions, run `go mod tidy`, and confirm the pins are gone. Do not commit this.
5. **Verify:** `go build ./...`, `go vet ./...`, `go test ./...`, `govulncheck ./...`,
   and `go mod tidy` with no diff.
6. **Adversarial check (Workflow):** independent agents re-check the reachability claim,
   the tidy-durability claim, and the fixed-version claims.
7. **PR:** draft PR with a reconcile table (one row per finding). List the findings that
   must be set in the SonarCloud UI, because SCA risks have no MCP status tool.
