#!/usr/bin/env bash
# Tests for scripts/check-no-pg-tcp-probes.sh — validates that the hook
# rejects TCP-only probes against PostgreSQL ports (5432, 5433) using
# curl, wget, or `nc -z`, while accepting legitimate patterns like
# `pg_isready`, `psql`, connection-string env values, compose port
# mappings, and curl against non-PG ports on a host named "postgres".
#
# Refs #755 — see [internal]specs/2026-04-27-755-postgres-startup-packet-flood-design.md §4.4.1
#
# Style precedent: scripts/check-env-file.test.sh.
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$SCRIPT_DIR/check-no-pg-tcp-probes.sh"

if [ ! -x "$HOOK" ]; then
  echo "FAIL: $HOOK not found or not executable"
  exit 1
fi

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass=0
fail=0

# run_case <name> <file-extension> <content> <expect_exit>
#   expect_exit=0 — file must pass (no anti-pattern present)
#   expect_exit=1 — file must fail (anti-pattern detected)
run_case() {
  local name="$1"
  local ext="$2"
  local content="$3"
  local expect_exit="$4"

  local f="$TMPDIR/${name}.${ext}"
  printf '%s\n' "$content" > "$f"

  "$HOOK" "$f" >/dev/null 2>&1
  local actual_exit=$?

  if [ "$actual_exit" = "$expect_exit" ]; then
    echo "  ok: $name"
    pass=$((pass + 1))
  else
    echo "  FAIL: $name (expected exit $expect_exit, got $actual_exit)"
    echo "    content: $content"
    fail=$((fail + 1))
  fi
}

echo "== Positive cases (must trigger exit 1) =="
run_case "curl_localhost_5432"      "sh"  'curl -sf localhost:5432'                           1
run_case "curl_http_localhost_5432" "sh"  'curl http://localhost:5432/health'                 1
run_case "wget_localhost_5432"      "sh"  'wget localhost:5432'                               1
run_case "wget_postgres_5433"       "sh"  'wget http://postgres:5433'                         1
run_case "nc_z_localhost_5432"      "sh"  'nc -z localhost 5432'                              1
run_case "nc_zv_postgres_5432"      "sh"  'nc -zv postgres 5432'                              1
run_case "nc_nzv_postgres_5433"     "sh"  'nc -nzv postgres 5433'                             1

echo ""
echo "== Negative cases (must NOT trigger; exit 0) =="
run_case "pg_isready_postgres"      "sh"  'pg_isready -h postgres -p 5432 -U concord'         0
run_case "pg_isready_localhost"     "sh"  'pg_isready -h localhost -U concord -d concord'     0
run_case "psql_admin"               "sh"  'psql -U concord -d concord -Atc "SELECT 1"'        0
run_case "database_url_envvar"      "sh"  'DATABASE_URL=postgres://concord:$PW@postgres:5432/concord'  0
run_case "compose_port_mapping"     "yml" 'ports: ["5432:5432"]'                              0
run_case "curl_postgres_8080"       "sh"  'curl postgres:8080/health'                         0
run_case "curl_control_plane"       "sh"  'curl http://control-plane:8080/health'             0
run_case "comment_documentation"    "sh"  '# anti-pattern documented for posterity: curl localhost:5432' 0

echo ""
echo "== Positive cases — embedded contexts (Family A) =="
run_case "dockerfile_run_curl"      "Dockerfile" 'RUN curl -sf localhost:5432 || exit 1'                  1
run_case "ci_yaml_script_curl"      "yml"        'script: "curl localhost:5432"'                          1
run_case "compose_healthcheck_curl" "yml"        'test: ["CMD-SHELL", "curl -sf http://localhost:5432"]'  1

echo ""
echo "== Positive cases — separated nc flags (Family B) =="
run_case "nc_z_w2_postgres_5432"    "sh"  'nc -z -w2 postgres 5432'                                       1
run_case "nc_w2_z_postgres_5432"    "sh"  'nc -w2 -z postgres 5432'                                       1
run_case "nc_v_z_w2_postgres_5433"  "sh"  'nc -v -z -w2 postgres 5433'                                    1

echo ""
echo "== Allowlist verification =="
# The hook itself contains the patterns in its regex literals. Invoking
# the hook on its own source must NOT trigger (allowlisted).
"$HOOK" "$SCRIPT_DIR/check-no-pg-tcp-probes.sh" >/dev/null 2>&1
self_exit=$?
if [ "$self_exit" = "0" ]; then
  echo "  ok: hook does not self-trigger"
  pass=$((pass + 1))
else
  echo "  FAIL: hook self-triggered (expected exit 0, got $self_exit)"
  fail=$((fail + 1))
fi

# The test script itself contains the patterns in its run_case fixtures.
# Invoking the hook on this test must NOT trigger.
"$HOOK" "$0" >/dev/null 2>&1
test_exit=$?
if [ "$test_exit" = "0" ]; then
  echo "  ok: test does not self-trigger"
  pass=$((pass + 1))
else
  echo "  FAIL: test self-triggered (expected exit 0, got $test_exit)"
  fail=$((fail + 1))
fi

echo ""
echo "Results: $pass passed, $fail failed"

if [ "$fail" -gt 0 ]; then
  exit 1
fi
exit 0
