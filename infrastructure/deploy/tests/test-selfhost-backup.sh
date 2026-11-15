#!/usr/bin/env bash
# Unit tests for infrastructure/deploy/selfhost-backup.sh (#184).
# Compose, Docker and the storage resolver are function stubs: no daemon.
set -uo pipefail
# shellcheck disable=SC2015  # pass/fail one-liners: pass never fails

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$(cd "$HERE/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# fixture MODE — fresh project root plus stubs; every call lands in $WORK/calls.
fixture() {
  ROOT="$WORK/root-$RANDOM$RANDOM"
  mkdir -p "$ROOT"
  printf 'POSTGRES_PASSWORD=x\nJWT_SECRET=archive-jwt-secret\n' >"$ROOT/.env"
  printf 'v9.9.9\n' >"$ROOT/VERSION"
  : >"$WORK/calls"
  STUB_MODE="$1"
}

# run_fn FUNC ARGS... — run in a subshell with stubs, return its rc.
# shellcheck disable=SC2034,SC2329  # stubs and globals are read by the sourced helper
run_fn() {
  (
    PROJECT_ROOT="$ROOT"
    # shellcheck source=../selfhost-backup.sh
    source "$DEPLOY/selfhost-backup.sh"
    selfhost_storage_resolve() {
      SELFHOST_STORAGE_MODE_RESOLVED="$STUB_MODE"
      SELFHOST_STORAGE_PROJECT_NAME=proj
    }
    compose() {
      printf 'compose %s\n' "$*" >>"$WORK/calls"
      case "$*" in
        'exec -T postgres pg_dump '*) printf 'PGDUMP-DATA' ;;
        'exec -T postgres pg_restore '*) cat >"$WORK/restored.dump" ;;
        'exec -T postgres psql '*) return "${STUB_REFRESH_REVOKE_RC:-0}" ;;
        'up -d redis') return "${STUB_REDIS_START_RC:-0}" ;;
        *'redis-cli'*' PING'*)
          [[ "${STUB_REDIS_PING_RC:-0}" == 0 ]] && printf 'PONG\n'
          return "${STUB_REDIS_PING_RC:-0}"
          ;;
        *'redis-cli'*' EVAL'*)
          if [[ "${STUB_REDIS_EVAL_RC:-0}" == 0 ]]; then
            if [[ "$*" == *'return r[1]..":"..found'* ]]; then
              printf '%s\n' "${STUB_REDIS_VERIFY_OUTPUT:-0:0}"
            else
              printf '%s\n' "${STUB_REDIS_PURGE_OUTPUT:-0}"
            fi
          fi
          return "${STUB_REDIS_EVAL_RC:-0}"
          ;;
      esac
      return "${STUB_COMPOSE_RC:-0}"
    }
    sleep() { :; }
    docker() {
      printf 'docker %s\n' "$*" >>"$WORK/calls"
      case "$*" in
        *':/data:ro '*) printf 'MINIO-DATA' ;;
        *) cat >"$WORK/restored.minio"; return "${STUB_DOCKER_RC:-0}" ;;
      esac
    }
    main() { printf 'main %s\n' "$*" >>"$WORK/calls"; }
    "$@"
  )
}

# ── argument refusals ─────────────────────────────────────────────────
fixture bundled
run_fn verb_backup --bogus 2>/dev/null; rc=$?
[[ $rc -eq 1 && ! -s "$WORK/calls" ]] && pass "backup refuses an unknown option before any call" \
  || fail "backup --bogus rc=$rc calls=$(cat "$WORK/calls")"
run_fn verb_restore "$WORK/x.tar.gz" 2>/dev/null; rc=$?
[[ $rc -eq 1 && ! -s "$WORK/calls" ]] && pass "restore without --confirm refuses before any call" \
  || fail "restore without --confirm rc=$rc calls=$(cat "$WORK/calls")"

# ── bundled backup ────────────────────────────────────────────────────
fixture bundled
out="$(run_fn verb_backup --out "$WORK/bk" --json 2>/dev/null)"; rc=$?
name="$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["schema"]==1 and d["ok"] is True; print(d["archive"])' "$out" 2>/dev/null)"
archive="$WORK/bk/$name"
if [[ $rc -eq 0 && "$name" =~ ^concord-backup-[0-9]{8}T[0-9]{6}Z\.tar\.gz$ && -f "$archive" ]]; then
  pass "backup --json names the archive it wrote"
else
  fail "backup --json rc=$rc out=$out"
fi
[[ "$(stat -c %a "$archive" 2>/dev/null)" == 600 && "$(stat -c %a "$WORK/bk")" == 700 ]] \
  && pass "archive is 0600 inside a 0700 directory" \
  || fail "modes: archive $(stat -c %a "$archive" 2>/dev/null) dir $(stat -c %a "$WORK/bk")"
mkdir "$WORK/x1" && tar -C "$WORK/x1" -xzf "$archive"
python3 - "$WORK/x1" <<'PY' && pass "manifest records release, mode and correct checksums for all three parts" || fail "manifest wrong"
import hashlib, json, os, sys
d = sys.argv[1]
m = json.load(open(os.path.join(d, "manifest.json")))
assert m["release"] == "v9.9.9" and m["storage_mode"] == "bundled"
assert set(m["parts"]) == {"postgres.dump", "minio.tar", "env"}
for n, h in m["parts"].items():
    assert hashlib.sha256(open(os.path.join(d, n), "rb").read()).hexdigest() == h
assert open(os.path.join(d, "postgres.dump")).read() == "PGDUMP-DATA"
PY
[[ -z "$(find "$WORK/bk" -name '.staging*' -o -name '*.partial')" ]] \
  && pass "backup leaves no staging or partial files" || fail "backup left staging files"

# ── restore round trip ────────────────────────────────────────────────
: >"$WORK/calls"
restore_output="$(run_fn verb_restore "$archive" --confirm 2>&1)"; rc=$?
call_line() { grep -n -m1 -- "$1" "$WORK/calls" | cut -d: -f1; }
order_ok=true
prev=0
for pattern in '^compose stop$' '^compose up -d redis$' '^compose up -d postgres$' \
  '^compose exec -T postgres pg_restore ' '^docker run ' '^main up$' '^main health$'; do
  line="$(call_line "$pattern")"
  [[ -n "$line" && "$line" -gt "$prev" ]] || order_ok=false
  prev="${line:-0}"
done
if [[ $rc -eq 0 && "$order_ok" == true ]]; then
  pass "restore stops, restores postgres then MinIO, then runs up and health"
else
  fail "restore rc=$rc calls=$(cat "$WORK/calls")"
fi
[[ "$(cat "$WORK/restored.dump")" == PGDUMP-DATA && "$(cat "$WORK/restored.minio")" == MINIO-DATA ]] \
  && pass "restore feeds the archived dump and MinIO data back" || fail "restored data differs"
restored_jwt="$(awk -F= '$1 == "JWT_SECRET" { print $2 }' "$ROOT/.env")"
if [[ $rc -eq 0 && "$restored_jwt" =~ ^[[:xdigit:]]{64}$ && "$restored_jwt" != archive-jwt-secret \
  && "$(awk -F= '$1 != "JWT_SECRET" { print }' "$ROOT/.env")" == 'POSTGRES_PASSWORD=x' \
  && "$(stat -c %a "$ROOT/.env")" == 600 && "$restore_output" != *archive-jwt-secret* ]]; then
  pass "restore rotates only JWT_SECRET without exposing it and preserves .env mode"
else
  fail "restore did not safely rotate JWT_SECRET rc=$rc env=$(sed 's/JWT_SECRET=.*/JWT_SECRET=<redacted>/' "$ROOT/.env")"
fi
: >"$WORK/calls"
retry_output="$(run_fn verb_restore "$archive" --confirm 2>&1)"; rc=$?
retry_jwt="$(awk -F= '$1 == "JWT_SECRET" { print $2 }' "$ROOT/.env")"
if [[ $rc -eq 0 && "$retry_jwt" =~ ^[[:xdigit:]]{64}$ && "$retry_jwt" != "$restored_jwt" \
  && "$retry_output" != *archive-jwt-secret* ]]; then
  pass "restore retry accepts the prior rotation and rotates JWT_SECRET again"
else
  fail "restore retry did not rotate JWT_SECRET safely rc=$rc"
fi

# A database restore can resurrect a refresh token that was revoked after the
# backup. Revoke every restored active token before either application plane
# restarts, and keep the stack stopped if that write fails.
: >"$WORK/calls"
run_fn verb_restore "$archive" --confirm >/dev/null 2>&1; rc=$?
restore_line="$(grep -n '^compose exec -T postgres pg_restore ' "$WORK/calls" | cut -d: -f1)"
revoke_line="$(grep -n '^compose exec -T postgres psql ' "$WORK/calls" | cut -d: -f1)"
start_line="$(grep -n '^main up$' "$WORK/calls" | cut -d: -f1)"
if [[ $rc -eq 0 && -n "$revoke_line" && "$restore_line" -lt "$revoke_line" && "$revoke_line" -lt "$start_line" ]] \
  && grep -Fq "UPDATE refresh_tokens SET revoked_at = now() - interval '1 hour' WHERE revoked_at IS NULL" "$WORK/calls"; then
  pass "restore revokes active refresh tokens after PostgreSQL restore and before startup"
else
  fail "refresh-token revocation ordering rc=$rc calls=$(cat "$WORK/calls")"
fi

: >"$WORK/calls"
err="$(STUB_REFRESH_REVOKE_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$?
if [[ $rc -ne 0 ]] && ! grep -q '^main up$' "$WORK/calls"; then
  pass "refresh-token revocation failure leaves the stack stopped"
else
  fail "refresh-token revocation failure rc=$rc err=$err calls=$(cat "$WORK/calls")"
fi

: >"$WORK/calls"
run_fn verb_restore "$archive" --confirm >/dev/null 2>&1; rc=$?
revoke_calls="$(grep -c '^compose exec -T postgres psql ' "$WORK/calls")"
start_calls="$(grep -c '^main up$' "$WORK/calls")"
if [[ $rc -eq 0 && "$revoke_calls" -eq 1 && "$start_calls" -eq 1 ]]; then
  pass "restore retry revokes refresh tokens and starts the stack"
else
  fail "refresh-token revocation retry rc=$rc calls=$(cat "$WORK/calls")"
fi

# PostgreSQL restore also revives opaque Redis capabilities. The purge clears
# only the ten auth-related key families (nine opaque bearer
# families plus the credential-epoch cache), then repeats the Lua readback so a
# restore never starts applications while any selected key remains.
redis_prefixes=(
  admin_session admin_enroll admin_login_challenge admin_enroll_challenge
  admin_server_stepup ws_ticket recovery_code email_verify sso_token cred_epoch
)
: >"$WORK/calls"
run_fn verb_restore "$archive" --confirm >/dev/null 2>&1; rc=$?
postgres_revoke_line="$(grep -n '^compose exec -T postgres psql ' "$WORK/calls" | cut -d: -f1)"
redis_start_line="$(grep -n '^compose up -d redis$' "$WORK/calls" | cut -d: -f1)"
redis_ping_line="$(grep -Ein 'redis-cli.* ping' "$WORK/calls" | head -1 | cut -d: -f1)"
redis_eval_lines="$(grep -Ein 'redis-cli.* eval' "$WORK/calls" | cut -d: -f1)"
main_up_line="$(grep -n '^main up$' "$WORK/calls" | cut -d: -f1)"
redis_eval_count="$(printf '%s\n' "$redis_eval_lines" | sed '/^$/d' | wc -l | tr -d ' ')"
prefixes_present=true
for prefix in "${redis_prefixes[@]}"; do
  grep -Fq "${prefix}:" "$WORK/calls" || prefixes_present=false
done
first_eval_line="$(printf '%s\n' "$redis_eval_lines" | head -1)"
if [[ $rc -eq 0 && "$prefixes_present" == true && "$redis_eval_count" -eq 2 \
  && "$redis_start_line" -lt "$redis_ping_line" && "$redis_ping_line" -lt "$postgres_revoke_line" \
  && "$postgres_revoke_line" -lt "$first_eval_line" && "$first_eval_line" -lt "$main_up_line" ]]; then
  pass "restore purges only opaque auth Redis keys with two clean readbacks before startup"
else
  fail "opaque Redis purge ordering rc=$rc calls=$(cat "$WORK/calls")"
fi

redis_failure_case() { # LABEL STUB — any purge failure leaves the stack stopped.
  : >"$WORK/calls"
  case "$2" in
    start) err="$(STUB_REDIS_START_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$? ;;
    ping) err="$(STUB_REDIS_PING_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$? ;;
    eval) err="$(STUB_REDIS_EVAL_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$? ;;
    malformed) err="$(STUB_REDIS_PURGE_OUTPUT=not-a-count run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$? ;;
    nonzero) err="$(STUB_REDIS_VERIFY_OUTPUT=0:1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$? ;;
  esac
  if [[ $rc -ne 0 && "$err" == *'DO NOT run concord-selfhost up'* ]] && ! grep -q '^main up$' "$WORK/calls"; then
    pass "opaque Redis purge refuses $1 without starting applications"
  else
    fail "opaque Redis purge $1 rc=$rc calls=$(cat "$WORK/calls")"
  fi
}
redis_failure_case "a Redis start failure" start
redis_failure_case "a Redis PING failure" ping
redis_failure_case "a Redis EVAL failure" eval
redis_failure_case "a malformed Redis EVAL count" malformed
redis_failure_case "a nonzero Redis EVAL count" nonzero

: >"$WORK/calls"
run_fn verb_restore "$archive" --confirm >/dev/null 2>&1; rc=$?
redis_eval_count="$(grep -Eic 'redis-cli.* eval' "$WORK/calls")"
retry_start_calls="$(grep -c '^main up$' "$WORK/calls")"
if [[ $rc -eq 0 && "$redis_eval_count" -eq 2 && "$retry_start_calls" -eq 1 ]]; then
  pass "opaque Redis purge retry clears both passes and starts applications"
else
  fail "opaque Redis purge retry rc=$rc calls=$(cat "$WORK/calls")"
fi

: >"$WORK/calls"
env_before="$(cksum <"$ROOT/.env")"
err="$(STUB_COMPOSE_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$?
if [[ $rc -ne 0 && "$err" == *'nothing was restored'* && "$(cksum <"$ROOT/.env")" == "$env_before" ]] \
  && ! compgen -G "$ROOT/.env.restore.*" >/dev/null && ! grep -q 'pg_restore\|^main up$' "$WORK/calls"; then
  pass "a stop failure keeps .env, removes the staged copy and restores nothing"
else
  fail "stop failure rc=$rc err=$err calls=$(cat "$WORK/calls")"
fi

# ── restore refusals: nothing is stopped ──────────────────────────────
refuse_case() { # LABEL ARCHIVE
  : >"$WORK/calls"
  local env_before
  env_before="$(cksum <"$ROOT/.env")"
  run_fn verb_restore "$2" --confirm >/dev/null 2>&1; rc=$?
  if [[ $rc -ne 0 ]] && ! grep -q '^compose stop' "$WORK/calls" \
    && [[ "$(cksum <"$ROOT/.env")" == "$env_before" ]] && ! compgen -G "$ROOT/.env.restore.*" >/dev/null; then
    pass "restore refuses $1 before stopping anything"
  else fail "restore $1 rc=$rc calls=$(cat "$WORK/calls")"; fi
}
repack() { # DIR OUT
  tar -C "$1" -czf "$2" .
}
refresh_env_checksum() { # DIR — make an intentionally altered test archive internally consistent.
  python3 - "$1" <<'PY'
import hashlib, json, os, sys
d = sys.argv[1]
p = os.path.join(d, "manifest.json")
m = json.load(open(p))
m["parts"]["env"] = hashlib.sha256(open(os.path.join(d, "env"), "rb").read()).hexdigest()
with open(p, "w") as out:
    json.dump(m, out, sort_keys=True, separators=(",", ":"))
PY
}
cp -r "$WORK/x1" "$WORK/x2"; printf 'tampered' >"$WORK/x2/postgres.dump"; repack "$WORK/x2" "$WORK/tampered.tar.gz"
refuse_case "a checksum mismatch" "$WORK/tampered.tar.gz"
cp -r "$WORK/x1" "$WORK/x3"; printf 'extra' >"$WORK/x3/surprise"; repack "$WORK/x3" "$WORK/extra.tar.gz"
refuse_case "an unlisted file" "$WORK/extra.tar.gz"
cp -r "$WORK/x1" "$WORK/x4"; rm "$WORK/x4/env"; ln -s /etc/passwd "$WORK/x4/env"; repack "$WORK/x4" "$WORK/link.tar.gz"
refuse_case "a symlink entry" "$WORK/link.tar.gz"
printf 'v1.0.0\n' >"$ROOT/VERSION"
refuse_case "a different release" "$archive"
printf 'v9.9.9\n' >"$ROOT/VERSION"
printf 'POSTGRES_PASSWORD=other\n' >"$ROOT/.env"
refuse_case "a different .env" "$archive"
printf 'POSTGRES_PASSWORD=x\nJWT_SECRET=archive-jwt-secret\n' >"$ROOT/.env"
cp -r "$WORK/x1" "$WORK/x-jwt-missing"; printf 'POSTGRES_PASSWORD=x\n' >"$WORK/x-jwt-missing/env"
refresh_env_checksum "$WORK/x-jwt-missing"; repack "$WORK/x-jwt-missing" "$WORK/jwt-missing.tar.gz"
refuse_case "an archive missing JWT_SECRET" "$WORK/jwt-missing.tar.gz"
cp -r "$WORK/x1" "$WORK/x-jwt-duplicate"; printf 'JWT_SECRET=duplicate\n' >>"$WORK/x-jwt-duplicate/env"
refresh_env_checksum "$WORK/x-jwt-duplicate"; repack "$WORK/x-jwt-duplicate" "$WORK/jwt-duplicate.tar.gz"
refuse_case "an archive with duplicate JWT_SECRET values" "$WORK/jwt-duplicate.tar.gz"
printf 'POSTGRES_PASSWORD=x\n' >"$ROOT/.env"
refuse_case "an install missing JWT_SECRET" "$archive"
printf 'POSTGRES_PASSWORD=x\nJWT_SECRET=archive-jwt-secret\nexport JWT_SECRET=archive-jwt-secret\n' >"$ROOT/.env"
refuse_case "an install with a second, exported JWT_SECRET" "$archive"
printf 'POSTGRES_PASSWORD=x\n  JWT_SECRET=archive-jwt-secret\n' >"$ROOT/.env"
refuse_case "an install with only an indented JWT_SECRET" "$archive"
printf 'POSTGRES_PASSWORD=x\nJWT_SECRET=archive-jwt-secret\n' >"$ROOT/.env"
STUB_MODE=byo-s3 refuse_case "a storage-mode mismatch" "$archive"
printf 'not a tarball' >"$WORK/junk.tar.gz"
refuse_case "a non-archive" "$WORK/junk.tar.gz"
python3 - "$WORK/x1" "$WORK/escape.tar.gz" <<'PY'
import os, sys, tarfile
src, out = sys.argv[1:3]
with tarfile.open(out, "w:gz") as t:
    for n in os.listdir(src):
        t.add(os.path.join(src, n), arcname="./" + n)
    t.add(os.path.join(src, "env"), arcname="../escape a")
PY
refuse_case "a parent-path entry" "$WORK/escape.tar.gz"

# ── a failure after the stop says the stack is stopped ────────────────
: >"$WORK/calls"
err="$(STUB_DOCKER_RC=1 run_fn verb_restore "$archive" --confirm 2>&1 >/dev/null)"; rc=$?
[[ $rc -ne 0 && "$err" == *'MinIO restore failed'* && "$err" == *'stack is stopped'* ]] && ! grep -q '^main up' "$WORK/calls" \
  && pass "a MinIO restore failure says the stack is stopped and does not start it" \
  || fail "MinIO failure rc=$rc err=$err"

# ── --out never loosens or tightens a directory it did not create ─────
fixture bundled
mkdir -m 755 "$WORK/shared"
run_fn verb_backup --out "$WORK/shared" >/dev/null 2>&1; rc=$?
[[ $rc -ne 0 && "$(stat -c %a "$WORK/shared")" == 755 && -z "$(ls -A "$WORK/shared")" && ! -s "$WORK/calls" ]] \
  && pass "backup refuses an existing directory open to others and leaves its mode alone" \
  || fail "--out shared rc=$rc mode=$(stat -c %a "$WORK/shared")"

# ── BYO S3 backup has no MinIO part ───────────────────────────────────
fixture byo-s3
out="$(run_fn verb_backup --out "$WORK/bk2" --json 2>/dev/null)"
name="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["archive"])' "$out" 2>/dev/null)"
if [[ -n "$name" ]] && ! tar -tzf "$WORK/bk2/$name" | grep -q minio.tar && ! grep -q '^docker' "$WORK/calls"; then
  pass "BYO S3 backup skips MinIO"
else
  fail "BYO S3 backup out=$out calls=$(cat "$WORK/calls")"
fi

# ── a git checkout is identified by its commit, not VERSION ───────────
fixture bundled
git -C "$ROOT" init -q && git -C "$ROOT" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m one
commit="$(git -C "$ROOT" rev-parse HEAD)"
out="$(run_fn verb_backup --out "$WORK/bk4" --json 2>/dev/null)"
name="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["archive"])' "$out" 2>/dev/null)"
got="$(tar -xzOf "$WORK/bk4/$name" ./manifest.json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["release"])' 2>/dev/null)"
[[ -n "$commit" && "$got" == "$commit" ]] && pass "a git checkout records its commit even when VERSION exists" \
  || fail "release in git checkout: got=$got want=$commit"

# ── failed dump writes no archive ─────────────────────────────────────
fixture bundled
STUB_COMPOSE_RC=1 run_fn verb_backup --out "$WORK/bk3" >/dev/null 2>&1; rc=$?
[[ $rc -ne 0 && -z "$(find "$WORK/bk3" -mindepth 1 2>/dev/null)" ]] \
  && pass "a failed pg_dump leaves no archive or staging" || fail "failed dump rc=$rc left: $(find "$WORK/bk3" -mindepth 1)"

# Exercise the production Lua through its shell wrapper against an isolated
# Redis: a local redis-server on a short Unix socket, else the pinned compose
# image with no network. CI must run it; elsewhere a missing Redis is a SKIP.
redis_cli=()
redis_cleanup=()
if command -v redis-server >/dev/null && command -v redis-cli >/dev/null; then
  redis_dir="$(mktemp -d /tmp/cvrp.XXXXXX)"
  redis_socket="$redis_dir/r.sock"
  redis-server --port 0 --unixsocket "$redis_socket" --unixsocketperm 700 --save '' \
    --appendonly no --daemonize yes --pidfile "$redis_dir/r.pid" >/dev/null 2>&1
  redis_cli=(redis-cli -s "$redis_socket")
  redis_cleanup=(rm -rf -- "$redis_dir")
elif command -v docker >/dev/null && command docker info >/dev/null 2>&1; then
  redis_image="$(awk '$1 == "image:" && $2 ~ /^redis:/ { print $2; exit }' "$DEPLOY/../../docker-compose.yml")"
  if redis_cid="$(command docker run -d --rm --network none "$redis_image" 2>/dev/null)"; then
    redis_cli=(command docker exec -i "$redis_cid" redis-cli)
    redis_cleanup=(command docker rm -f "$redis_cid")
  fi
fi
redis_ready=false
if (( ${#redis_cli[@]} )); then
  for ((i = 0; i < 50; i++)); do
    [[ "$("${redis_cli[@]}" PING 2>/dev/null)" == PONG ]] && { redis_ready=true; break; }
    sleep 0.2
  done
fi
if [[ "$redis_ready" == true ]]; then
  redis_targets=(
    admin_session admin_enroll admin_login_challenge admin_enroll_challenge
    admin_server_stepup ws_ticket recovery_code email_verify sso_token cred_epoch
  )
  for prefix in "${redis_targets[@]}"; do
    "${redis_cli[@]}" SET "${prefix}:sentinel" target >/dev/null
  done
  "${redis_cli[@]}" SET defensive:keep keep >/dev/null
  # 9000 keys in one SCAN page: a single unpack() over them exceeds Lua's limit.
  "${redis_cli[@]}" EVAL 'for i=1,9000 do redis.call("SET","ws_ticket:bulk"..i,"x") end' 0 >/dev/null
  "${redis_cli[@]}" SET admin_sessionX:keep keep >/dev/null
  (
    source "$DEPLOY/selfhost-backup.sh"
    compose() {
      [[ "$1" == exec && "$2" == -T && "$3" == redis && "$4" == redis-cli && "$5" == --raw ]] || return 1
      shift 5
      "${redis_cli[@]}" --raw "$@"
    }
    _restore_purge_redis_authority
  ); rc=$?
  purged=true
  for prefix in "${redis_targets[@]}"; do
    [[ "$("${redis_cli[@]}" EXISTS "${prefix}:sentinel")" == 0 ]] || purged=false
  done
  [[ "$("${redis_cli[@]}" EVAL 'return #redis.call("KEYS","ws_ticket:bulk*")' 0)" == 0 ]] || purged=false
  if [[ $rc -eq 0 && "$purged" == true && "$("${redis_cli[@]}" GET defensive:keep)" == keep \
    && "$("${redis_cli[@]}" GET admin_sessionX:keep)" == keep ]]; then
    pass "isolated Redis Lua purge removes only the ten restored authorization key families, 9000 in one page"
  else
    fail "isolated Redis Lua purge rc=$rc purged=$purged"
  fi
  "${redis_cli[@]}" SHUTDOWN NOSAVE >/dev/null 2>&1 || true
elif [[ "${CI:-}" == true ]]; then
  fail "isolated Redis Lua probe could not start Redis in CI"
else
  echo "SKIP: isolated Redis Lua probe (no redis-server or Docker)"
fi
(( ${#redis_cleanup[@]} )) && "${redis_cleanup[@]}" >/dev/null 2>&1

echo
[[ $fails -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "$fails FAILED"; exit 1; }
