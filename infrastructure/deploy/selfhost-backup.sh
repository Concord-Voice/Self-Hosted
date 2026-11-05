#!/usr/bin/env bash
# Self-host backup and restore (#184). Sourced by concord-selfhost after
# selfhost-storage.sh; relies on its `compose` function and resolved model.
#
# Archive: concord-backup-<UTC>.tar.gz (0600) holding
#   manifest.json   schema, release, storage mode, UTC time, sha256 per part
#   postgres.dump   pg_dump --format=custom of the concord database
#   minio.tar       bundled MinIO volume (bundled mode only; BYO S3 is the operator's)
#   env             the root .env (secrets: the archive is 0600 in a 0700 dir)
# ponytail: MinIO is copied live (crash-consistent, not quiesced); stop the
# stack first if a frozen object store matters more than uptime.

readonly BACKUP_PG_SERVICE=postgres
readonly BACKUP_HELPER_IMAGE=concordvoice-postgres:16-alpine

_backup_fail() { printf 'selfhost-backup: %s\n' "$1" >&2; return 1; }

# _backup_release — identity a restore must match. A packaged install ships
# VERSION; a checkout uses its commit. Unknown identity refuses restore.
_backup_release() {
  if [[ -f "$PROJECT_ROOT/VERSION" ]]; then
    tr -d '[:space:]' <"$PROJECT_ROOT/VERSION"
  else
    git -C "$PROJECT_ROOT" rev-parse HEAD 2>/dev/null
  fi
}

_backup_minio_volume() { printf '%s_minio-data' "$SELFHOST_STORAGE_PROJECT_NAME"; }

# _backup_json OK [NAME] — fixed keys; NAME is our own generated file name.
_backup_json() {
  if [[ -n "${2:-}" ]]; then
    printf '{"schema":1,"ok":%s,"archive":"%s"}\n' "$1" "$2"
  else
    printf '{"schema":1,"ok":%s}\n' "$1"
  fi
}

verb_backup() {
  local out="$PROJECT_ROOT/backups" json=0 stage name release rc=0
  while (( $# )); do
    case "$1" in
      --out) [[ -n "${2:-}" ]] || { _backup_fail '--out needs a directory'; return 1; }; out="$2"; shift 2 ;;
      --json) json=1; shift ;;
      *) _backup_fail 'backup accepts only --out DIR and --json'; return 1 ;;
    esac
  done
  if (( json )); then
    _backup_run "$out" >&2 || rc=$?
    if (( rc == 0 )); then _backup_json true "$BACKUP_LAST_NAME"; else _backup_json false; fi
    return "$rc"
  fi
  _backup_run "$out"
}

_backup_run() {
  local out="$1" stage release
  BACKUP_LAST_NAME=''
  selfhost_storage_resolve "$PROJECT_ROOT" || return 1
  release="$(_backup_release)" && [[ -n "$release" ]] || { _backup_fail 'release identity unavailable'; return 1; }
  [[ -f "$PROJECT_ROOT/.env" ]] || { _backup_fail 'no .env: nothing installed to back up'; return 1; }
  (
    umask 077
    mkdir -p -- "$out" && chmod 700 -- "$out"
  ) || { _backup_fail 'cannot prepare the backup directory'; return 1; }
  stage="$(umask 077; mktemp -d "$out/.staging.XXXXXX")" || { _backup_fail 'cannot create staging'; return 1; }
  local rc=0
  _backup_body "$out" "$stage" "$release" || rc=$?
  rm -rf -- "$stage"
  return "$rc"
}

_backup_body() {
  local out="$1" stage="$2" release="$3" name sum part sep
  name="concord-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"

  echo 'Dumping the database...'
  compose exec -T "$BACKUP_PG_SERVICE" pg_dump -U concord -d concord --format=custom \
    >"$stage/postgres.dump" || { _backup_fail 'pg_dump failed'; return 1; }
  [[ -s "$stage/postgres.dump" ]] || { _backup_fail 'pg_dump produced no data'; return 1; }

  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == bundled ]]; then
    echo 'Copying bundled MinIO data...'
    docker run --rm --network none -u 0 -v "$(_backup_minio_volume):/data:ro" \
      --entrypoint tar "$BACKUP_HELPER_IMAGE" -C /data -cf - . >"$stage/minio.tar" \
      || { _backup_fail 'MinIO copy failed'; return 1; }
  fi
  cp -- "$PROJECT_ROOT/.env" "$stage/env" || { _backup_fail 'cannot copy .env'; return 1; }

  {
    printf '{"schema":1,"release":"%s","storage_mode":"%s","created":"%s","parts":{' \
      "$release" "$SELFHOST_STORAGE_MODE_RESOLVED" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    sep=''
    for part in postgres.dump minio.tar env; do
      [[ -f "$stage/$part" ]] || continue
      sum="$(sha256sum -- "$stage/$part" | cut -d' ' -f1)"
      printf '%s"%s":"%s"' "$sep" "$part" "$sum"
      sep=','
    done
    printf '}}\n'
  } >"$stage/manifest.json"

  if ! (umask 077; tar -C "$stage" -czf "$out/.$name.partial" .) \
      || ! mv -- "$out/.$name.partial" "$out/$name"; then
    rm -f -- "$out/.$name.partial"
    _backup_fail 'cannot write the archive'
    return 1
  fi
  BACKUP_LAST_NAME="$name"
  echo "Backup written: $out/$name"
}

verb_restore() {
  local archive='' confirm=0 json=0 rc=0
  while (( $# )); do
    case "$1" in
      --confirm) confirm=1; shift ;;
      --json) json=1; shift ;;
      -*) _backup_fail 'restore accepts ARCHIVE --confirm [--json]'; return 1 ;;
      *) [[ -z "$archive" ]] || { _backup_fail 'one archive only'; return 1; }; archive="$1"; shift ;;
    esac
  done
  [[ -n "$archive" ]] || { _backup_fail 'restore needs an archive path'; return 1; }
  (( confirm )) || { _backup_fail 'restore replaces the database and stored files; re-run with --confirm'; return 1; }
  if (( json )); then
    _restore_run "$archive" >&2 || rc=$?
    if (( rc == 0 )); then _backup_json true; else _backup_json false; fi
    return "$rc"
  fi
  _restore_run "$archive"
}

# _restore_verify DIR — every listed part present and matching; nothing unlisted.
_restore_verify() {
  python3 - "$1" "$2" "$3" <<'PY'
import hashlib, json, os, re, sys
stage, release, mode = sys.argv[1:4]
def fail(msg):
    print("selfhost-backup: " + msg, file=sys.stderr); sys.exit(1)
try:
    with open(os.path.join(stage, "manifest.json")) as f:
        m = json.load(f)
except (OSError, ValueError):
    fail("manifest missing or unreadable")
if m.get("schema") != 1: fail("unknown backup schema")
if m.get("release") != release: fail("backup release does not match this install")
if m.get("storage_mode") != mode: fail("backup storage mode does not match this install")
parts = m.get("parts")
allowed = {"postgres.dump", "env", "minio.tar"}
if not isinstance(parts, dict) or not {"postgres.dump", "env"} <= set(parts) or not set(parts) <= allowed:
    fail("manifest parts invalid")
if (mode == "bundled") != ("minio.tar" in parts): fail("manifest parts do not fit the storage mode")
present = set(os.listdir(stage)) - {"manifest.json"}
if present != set(parts): fail("archive contents do not match the manifest")
for name, want in parts.items():
    if not isinstance(want, str) or not re.fullmatch(r"[0-9a-f]{64}", want): fail("manifest checksum invalid")
    path = os.path.join(stage, name)
    if os.path.islink(path) or not os.path.isfile(path): fail("archive part is not a regular file")
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""): h.update(chunk)
    if h.hexdigest() != want: fail("checksum mismatch: " + name)
PY
}

_restore_run() {
  local archive="$1" stage release
  selfhost_storage_resolve "$PROJECT_ROOT" || return 1
  [[ -f "$archive" ]] || { _backup_fail 'archive not found'; return 1; }
  release="$(_backup_release)" && [[ -n "$release" ]] || { _backup_fail 'release identity unavailable'; return 1; }
  stage="$(umask 077; mktemp -d)" || { _backup_fail 'cannot create staging'; return 1; }
  local rc=0
  _restore_body "$archive" "$stage" "$release" || rc=$?
  rm -rf -- "$stage" "$stage.list"
  return "$rc"
}

_restore_body() {
  local archive="$1" stage="$2" release="$3" i
  # Refuse links, devices and absolute or parent paths before extracting.
  if ! tar -tvzf "$archive" >"$stage.list" 2>/dev/null || ! python3 -c '
import sys
for line in open(sys.argv[1]):
    kind, name = line[0], line.split()[-1]
    if kind not in "-d" or name.startswith("/") or ".." in name.split("/"):
        sys.exit(1)
' "$stage.list"; then
    _backup_fail 'archive is not a Concord backup'
    return 1
  fi
  tar -C "$stage" --no-same-owner -xzf "$archive" || { _backup_fail 'cannot extract the archive'; return 1; }
  _restore_verify "$stage" "$release" "$SELFHOST_STORAGE_MODE_RESOLVED" || return 1
  # Secrets in .env must match: a restored database under different keys breaks.
  cmp -s -- "$stage/env" "$PROJECT_ROOT/.env" || {
    _backup_fail 'the backup .env differs from this install; put the backup env in place as .env (0600) first'
    return 1
  }

  echo 'Stopping the stack...'
  compose stop || { _backup_fail 'stop failed'; return 1; }
  echo 'Restoring the database...'
  compose up -d "$BACKUP_PG_SERVICE" || { _backup_fail 'cannot start postgres'; return 1; }
  for ((i = 0; i < 30; i++)); do
    compose exec -T "$BACKUP_PG_SERVICE" pg_isready -U concord >/dev/null 2>&1 && break
    sleep 2
  done
  compose exec -T "$BACKUP_PG_SERVICE" pg_restore -U concord -d concord --clean --if-exists \
    --no-owner --single-transaction <"$stage/postgres.dump" || { _backup_fail 'pg_restore failed'; return 1; }
  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == bundled ]]; then
    echo 'Restoring bundled MinIO data...'
    docker run --rm -i --network none -u 0 -v "$(_backup_minio_volume):/data" \
      --entrypoint sh "$BACKUP_HELPER_IMAGE" -c 'find /data -mindepth 1 -delete && tar -C /data -xf -' \
      <"$stage/minio.tar" || { _backup_fail 'MinIO restore failed'; return 1; }
  fi
  echo 'Starting the stack...'
  main up || return 1
  main health
}
