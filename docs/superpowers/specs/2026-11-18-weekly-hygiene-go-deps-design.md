# Weekly hygiene: Go dependency risks — Design

Issueless ad-hoc run (weekly hygiene sweep, 2026-11-18). Scope: the SonarCloud SCA
dependency risks for Go modules in `services/control-plane` (the only Go module).

## Finding

All 22 Go findings come from one place: the `go.mod` of
`github.com/golang-migrate/migrate/v4` v4.20.1, the latest tagged release. That `go.mod`
requires every database and source driver migrate supports (MongoDB, Cassandra, Spanner,
the S3 source, and more) and its test tools (`dktest`, which pulls `github.com/docker/docker`). SonarCloud
reads the full module graph (`go list -m all`), so it reports these modules.

We import only `migrate/v4`, `database`, `database/postgres`, `source`, `source/file` and
`source/iofs`. `go list -deps -test ./...` lists **none** of the flagged modules, for every
build tag and GOOS this repo uses. No flagged package is compiled into a binary or a test.
`govulncheck ./...` and `govulncheck -mode=binary` report no vulnerabilities.

## Approach

1. **Bump the direct dependency that pulls the modules in.** Upstream migrate master
   (`a746b101346e`, 2026-11-16) is v4.20.1 plus two merged PRs:
   golang-migrate/migrate#1468 replaces `dktest` with an in-house test harness, and
   golang-migrate/migrate#1458 bumps vulnerable dependencies. Outside `go.mod`/`go.sum`,
   these PRs change only test files, the `dktesting` test harness, CI and the README. We
   do not compile `dktesting` or any changed file. The library code we compile does not
   change. We require it as `v4.20.2-0.20261116072128-a746b101346e`. It removes
   `docker/docker`, `dktest`, `grpc-gateway/v2` and `otlptrace` from the graph, among
   others (`Azure/go-ansiterm`, `moby/term`, `morikuni/aec`, `pkg/errors`). It moves grpc
   to v1.83.2 and otel/sdk to v1.45.0. The upstream commit is signed, and its `go.sum`
   hashes match `sum.golang.org`. Move to the tagged v4.20.2 when upstream cuts it. Both
   update paths keep the pseudo-version and never downgrade it: `go get -u` in
   `go-transitive-flush.yml` treats it as newer than v4.20.1, and Dependabot adds the
   current version as a candidate. Two side effects are accepted. Dependabot's 7-day
   cooldown means the weekly flush will likely take the tag first. While we sit on a
   pseudo-version (a semver prerelease), Dependabot also considers prerelease tags for
   migrate, so review any `-beta` it proposes like any other bump. This manual pin also
   skips the 7-day cooldown. No repo rule sets a cooldown for manual bumps, and the
   compiled library code is identical to v4.20.1.
2. **Bump `go.opentelemetry.io/otel/trace` to v1.47.0** (the first fixed version for
   CVE-2026-128085). It is on the package path of `go mod tidy` (test of a dependency:
   `database/postgres.test` → migrate test harness → `moby/moby/client` → `otel/trace`),
   so the bump stays after `go mod tidy`.
3. **Do not pin modules that tidy removes.** A `require` for a module that gives no
   package in `all` is deleted by `go mod tidy`. Tested: pins on grpc-gateway, aws s3,
   eventstream, jose2go, otel/sdk, otlptrace and thrift at their fixed versions are all
   removed by tidy, and the weekly flush would remove them too. A blank-import file to
   keep them would compile vulnerable code into the build, which is worse.
4. **Reject the remaining findings with a technical reason**, recorded per finding in the
   PR body. No `pgx/v5` migration: we do not use pgx (Postgres access is `lib/pq`), so a
   migration changes nothing that runs.

## Acceptance criteria

- `go build ./...` and `go vet ./...` pass, and `go test ./...` passes in CI (with Postgres).
- `govulncheck ./...` reports no vulnerabilities.
- `go mod tidy` leaves `go.mod` and `go.sum` unchanged.
- The one module new to the graph (`go.opentelemetry.io/otel/log` v1.47.0) has no OSV
  advisories. The graph modules that stay after the migrate bump (`moby/moby/api`,
  `moby/moby/client`, `moby/docker-image-spec`, grpc v1.83.2, otel/sdk v1.45.0) have
  none either.
- The PR body has one decision row per finding (package, CVE, action, verification).
- Changed files: `services/control-plane/go.mod`, `go.sum`, these two docs, and the docs
  this bump makes stale: the Go rows of `NOTICE.md`, the golang-migrate version in
  `services/control-plane/migrations/README.md`, and in `.claude/rules/domains/migrations.md`.
