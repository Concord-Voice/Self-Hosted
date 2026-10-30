# scripts/tests/ — Test layout convention

Shell tests for `scripts/` and `infrastructure/deploy/` entry points. They follow the
roll-your-own bash assertion style established by `infrastructure/deploy/tests/`. No frameworks
(no bats, no shellspec).

## Naming

`test-<subject>.sh`, one file per subject. `test-concord-dev-<area>.sh` is one family among
several — it splits `scripts/concord-dev.sh` by area:

- `test-concord-dev-args.sh` — argument parsing + verb dispatch
- `test-concord-dev-console.sh` — console verb
- `test-concord-dev-dev-code.sh` — dev-code verb
- `test-concord-dev-billing-listen.sh` — billing-listen verb (docker stubbed)
- `test-concord-dev-down.sh` — teardown failures, env fallback, retained Docker
  ports, and safe same-repository listener recovery
- `test-concord-dev-env-drift.sh` — env-file drift detection
- `test-concord-dev-helpers.sh` — helper functions via source
- `test-concord-dev-logs.sh` — logs verb
- `test-concord-dev-pid.sh` — PID lifecycle (real fork/kill)
- `test-concord-dev-preflight.sh` — pre-flight error paths

Other subjects in this directory:

- `test-concord-selfhost.sh` — the self-hosted stack CLI
- `test-install-selfhost.sh` — the self-hosted installer
- `test-provision-cert.sh` — certificate provisioning
- `test-publish-minio-image.sh` — MinIO image publishing
- `test-check-oci-tag.sh` — OCI tag validation
- `test-go-transitive-flush-render.sh` — Go transitive-flush issue rendering

The `selfhost-deploy-tests` commit hook runs all twelve self-host suites directly on Linux.
On macOS it uses `run-selfhost-deploy-tests.sh` and a cached Ubuntu image; Docker Desktop
must be running because the certificate and media suites exercise real test containers.
The hook containers have access to the local Docker daemon, so run them only on a trusted
worktree. On macOS, only the two TLS observer suites use bridge networking to reach
Docker Desktop's loopback-published test ports; the other ten use no network. The
installer and security-event suites use container-local temporary files because
Docker Desktop's bind mount does not preserve the mode bits those fixtures test.
The Main CD fixture hook also uses this pinned Ubuntu image on macOS because staging
requires Linux `renameat2`; its source and Git metadata mounts are read-only, and it
has no Docker socket or network. Linux runs the suite directly.

Run `ls scripts/tests/` for the current set.

## Mocking pattern

Two strategies, chosen per test file:

### PATH-override stubs (mirrors infrastructure/deploy/tests/)

```bash
STUB_DIR=$(mktemp -d)
trap 'rm -rf "$STUB_DIR"' EXIT
cat > "$STUB_DIR/docker" <<'STUB'
#!/usr/bin/env bash
echo "stub-docker $*" >> "$DOCKER_STUB_LOG"
exit 0
STUB
chmod +x "$STUB_DIR/docker"

PATH="$STUB_DIR:$PATH" ./scripts/concord-dev.sh up
```

Use when testing full-verb behavior (e.g., `up` succeeds when docker stub returns 0).

### Source the script

```bash
source "$PROJECT_ROOT/scripts/concord-dev.sh"
# Now helpers like wait_for_service, stop_process are callable directly.
wait_for_service "http://localhost:9999/never" "Test" 1
```

Use when testing individual helpers. Works because the script's tail
guards `main "$@"` behind `[[ "${BASH_SOURCE[0]}" == "${0}" ]]`.

## Assertions

Print `PASS: <description>` on success, `FAIL: <description>` and
`exit 1` on failure. No frameworks. Example:

```bash
if wait_for_service "http://localhost:9999/never" "Test" 1 2>/dev/null; then
  echo "FAIL: wait_for_service should time out on unreachable URL"
  exit 1
else
  echo "PASS: wait_for_service times out on unreachable URL"
fi
```

## Running locally

```bash
for t in scripts/tests/test-*.sh; do
  echo "=== $t ==="
  env PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin /bin/bash "$t" || exit 1
done
```

The explicit system Bash and `PATH` also apply Bash 3.2 compatibility to child
scripts launched through `/usr/bin/env bash`.
