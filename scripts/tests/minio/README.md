# MinIO security verification harnesses

This directory contains verification harnesses for the Concord dependency remediation of Apache Thrift (`github.com/apache/thrift v0.25.0`) in MinIO (`internal/s3select/parquet`).

## Background & Scope

Issue #3527 updates Apache Thrift from `v0.24.0` to `v0.25.0` to remediate residual vulnerabilities (CVE-2026-63772, CVE-2026-66055, CVE-2026-82458, CVE-2026-83663, CVE-2026-94637).

MinIO uses `github.com/fraugster/parquet-go` for S3 Select Parquet support (`internal/s3select/parquet`), which depends on `github.com/apache/thrift`. Parquet support remains default-off in production (`MINIO_API_SELECT_PARQUET=off`).

## Harness: `verify-parquet.go`

`verify-parquet.go` performs two distinct checks:

1. **Part 1: Real `parquet-go` Reader Compatibility Checks**
   - **Valid dataset**: Generates an in-memory Parquet file using `parquetschema` and `parquetgo.NewFileWriter`, reads it back with `parquetgo.NewFileReader`, and verifies row counts, the column count, and the integer `id` values. Also tests existing testdata files (such as `internal/s3select/testdata/testdata.parquet`) when present.
   - **Malformed datasets**: Checks rejection of general non-Parquet input (`NOT_A_PARQUET_FILE`, without isolating header validation), truncated files (< 12 bytes), corrupted footer metadata length, and malformed Thrift footer metadata bytes.

2. **Part 2: Separately Labeled Generated `parquet.FileMetaData.Read` + `TMemoryBuffer` Discriminator**
   - Uses real generated Thrift types (`parquet.NewFileMetaData()`) with `thrift.NewTMemoryBuffer()`.
   - Supplies a declared schema count of 33 with insufficient payload (0 remaining bytes and 7 remaining bytes).
   - In Thrift `v0.25.0`, this triggers `thrift.TProtocolException` with `TypeId == thrift.SIZE_LIMIT` (`container of 33 elements needs at least 33 bytes...`).
   - The supplied Thrift `v0.24.0` diagnostic control reports `EOF` for both payloads. See the evidence record.

### Important Boundary Distinction

> [!IMPORTANT]
> The production `StreamTransport` path in `parquet-go` with empty `TConfiguration` wraps an `io.LimitReader` and does **NOT** expose known remaining bytes to Thrift. Therefore, the known-size container allocation bound is demonstrated specifically on `TMemoryBuffer` and does **NOT** establish or imply a newly enforced small container/allocation cap on MinIO's production `StreamTransport` path.

## How to Run

Run the harness under the patched MinIO derivative's module graph with `-mod=readonly`:

```bash
cd /path/to/minio-source
go run -mod=readonly /path/to/concord/scripts/tests/minio/verify-parquet.go
```

After the Docker Engine admission check in the [source recipe](../../../infrastructure/docker/minio/SOURCE-BUILD.md) passes, run inside the pinned builder container (`golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af`):

```bash
docker run --rm \
  -v "$PWD/infrastructure/docker/minio/dependencies.patch:/tmp/dependencies.patch:ro" \
  -v "$PWD/scripts/tests/minio/verify-parquet.go:/tmp/verify-parquet.go:ro" \
  golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af \
  bash -c '
    set -euo pipefail
    export GOTOOLCHAIN=local
    cd /tmp && git clone --quiet --depth 1 --branch RELEASE.2025-10-15T17-29-55Z https://github.com/minio/minio.git r 2>/dev/null
    cd /tmp/r
    test "$(git rev-parse HEAD)" = 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a # pragma: allowlist secret -- public upstream commit
    git apply /tmp/dependencies.patch
    go run -mod=readonly /tmp/verify-parquet.go
  '
```

## Range-header regression (`range-regression_test.go`, #3716)

The stdlib-only test exercises `http.FileServer` with a fixed 1,024-byte asset.
A single range and 200 one-byte ranges retain HTTP 206 behavior; 201 ranges
must be ignored and return the complete asset with HTTP 200. Invalid and
nonoverlapping ranges return 416. The total requested bytes remain smaller
than the asset, so the older aggregate-size fallback cannot produce a false
pass. No exhaustion workload or timing threshold is used.

Run the same file under the old and new compilers, from the patched MinIO
source directory. `CONCORD_ROOT` names this repository and `MINIO_TEST_GO`
names the absolute path to the selected compiler:

```bash
# Working directory: the patched MinIO source, whose go.mod says go 1.26.0.
GODEBUG= GOTOOLCHAIN=local "$MINIO_TEST_GO" test -mod=readonly -count=1 -v \
  "$CONCORD_ROOT/scripts/tests/minio/range-regression_test.go"
```

Use a writable `GOCACHE` when the normal cache is unavailable. Do not run with
`GO111MODULE=off`: the newer compiler then selects a legacy compatibility
range limit of one, so that test would not represent this derivative. Do not
set `httpservecontentmaxranges=0`; it disables the corrected bound. The Go
1.26.8 run must fail the 201-range case (206 instead of 200); Go 1.26.10 must
pass all six cases. The publisher contract suite separately exercises both
actual native metadata steps, accepting 1.26.10 and rejecting 1.26.8.

This harness is an explicit validation command, not a newly added PR-CI job.
It proves standard-library behavior, not the exact MinIO console route. That
needs a separate bounded check against a freshly built binary, disposable
storage and a loopback-only console: retrieve a real asset larger than 201
bytes with `Accept-Encoding: identity`, verify an ordinary range, then compare
the 201-range response with the complete original asset. Record the binary
hash and metadata with the result. A cross-built arm64 binary does not prove
native arm64 runtime behavior.
