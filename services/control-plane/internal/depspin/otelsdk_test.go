// Package depspin_test keeps module requirements that `go mod tidy` would
// otherwise drop. It is test-only, so nothing here is compiled into a binary.
package depspin_test

// ponytail: golang-migrate's go.mod requires go.opentelemetry.io/otel/sdk
// v1.45.0, which CVE-2026-128085 affects (fixed in v1.47.0). SonarCloud reads
// the module graph and flags it, although no otel/sdk package is compiled. A
// `require` at v1.47.0 survives `go mod tidy` only while some package imports
// otel/sdk, so this test file imports it. Remove the file once golang-migrate
// requires v1.47.0 or later.
import _ "go.opentelemetry.io/otel/sdk"
