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
  printf 'POSTGRES_PASSWORD=x\n' >"$ROOT/.env"
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
      esac
      return "${STUB_COMPOSE_RC:-0}"
    }
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
run_fn verb_restore "$archive" --confirm >/dev/null 2>&1; rc=$?
order="$(grep -E '^(compose|docker|main) ' "$WORK/calls" | grep -v pg_isready | sed -E 's/^(compose|docker|main) ([a-z-]+( -[a-zA-Z]+)?( [a-z_-]+)?).*/\1 \2/' | tr '\n' '|')"
if [[ $rc -eq 0 && "$order" == 'compose stop|compose up -d postgres|compose exec -T postgres|docker run --rm|main up|main health|' ]]; then
  pass "restore stops, restores postgres then MinIO, then runs up and health"
else
  fail "restore rc=$rc order=$order"
fi
[[ "$(cat "$WORK/restored.dump")" == PGDUMP-DATA && "$(cat "$WORK/restored.minio")" == MINIO-DATA ]] \
  && pass "restore feeds the archived dump and MinIO data back" || fail "restored data differs"

# ── restore refusals: nothing is stopped ──────────────────────────────
refuse_case() { # LABEL ARCHIVE
  : >"$WORK/calls"
  run_fn verb_restore "$2" --confirm >/dev/null 2>&1; rc=$?
  if [[ $rc -ne 0 ]] && ! grep -q '^compose stop' "$WORK/calls"; then pass "restore refuses $1 before stopping anything"
  else fail "restore $1 rc=$rc calls=$(cat "$WORK/calls")"; fi
}
repack() { # DIR OUT
  tar -C "$1" -czf "$2" .
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
printf 'POSTGRES_PASSWORD=x\n' >"$ROOT/.env"
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

echo
[[ $fails -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "$fails FAILED"; exit 1; }
