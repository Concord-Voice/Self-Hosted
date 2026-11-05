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

# _backup_release — identity a restore must match. A git checkout uses its
# commit, since one VERSION spans many commits and schemas; a packaged install
# without git uses VERSION. Unknown identity refuses restore.
_backup_release() {
  if git -C "$PROJECT_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git -C "$PROJECT_ROOT" rev-parse HEAD 2>/dev/null
  elif [[ -f "$PROJECT_ROOT/VERSION" ]]; then
    tr -d '[:space:]' <"$PROJECT_ROOT/VERSION"
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

# _backup_prepare_dir DIR — create DIR 0700, or accept an existing one only if
# it is ours and closed to group and others. Never chmod a directory we did
# not create: `--out /tmp` must not lock every other user out of /tmp.
_backup_prepare_dir() {
  if [[ ! -e "$1" ]]; then
    (umask 077; mkdir -p -- "$1") || { _backup_fail 'cannot create the backup directory'; return 1; }
    return 0
  fi
  [[ -d "$1" && ! -L "$1" && "$(stat -c '%u %a' -- "$1")" =~ ^$(id -u)\ [0-7]00$ ]] \
    || { _backup_fail 'backup directory must be owned by this user with no group or other access (0700)'; return 1; }
}

_backup_run() {
  local out="$1" stage release
  BACKUP_LAST_NAME=''
  selfhost_storage_resolve "$PROJECT_ROOT" || return 1
  release="$(_backup_release)" && [[ -n "$release" ]] || { _backup_fail 'release identity unavailable'; return 1; }
  [[ -f "$PROJECT_ROOT/.env" ]] || { _backup_fail 'no .env: nothing installed to back up'; return 1; }
  _backup_prepare_dir "$out" || return 1
  stage="$(umask 077; mktemp -d "$out/.staging.XXXXXX")" || { _backup_fail 'cannot create staging'; return 1; }
  local rc=0
  _backup_body "$out" "$stage" "$release" || rc=$?
  rm -rf -- "$stage"
  return "$rc"
}

_backup_body() {
  local out="$1" stage="$2" release="$3" name
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

  python3 - "$stage" "$release" "$SELFHOST_STORAGE_MODE_RESOLVED" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" <<'PY' \
    || { _backup_fail 'cannot write the manifest'; return 1; }
import hashlib, json, os, sys
stage, release, mode, created = sys.argv[1:5]
parts = {}
for name in ("postgres.dump", "minio.tar", "env"):
    path = os.path.join(stage, name)
    if os.path.isfile(path):
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""): h.update(chunk)
        parts[name] = h.hexdigest()
with open(os.path.join(stage, "manifest.json"), "w") as f:
    json.dump({"schema": 1, "release": release, "storage_mode": mode, "created": created, "parts": parts}, f)
PY

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
  rm -rf -- "$stage"
  return "$rc"
}

_restore_body() {
  local archive="$1" stage="$2" release="$3" i
  # One read of the archive: only a top-level directory and the known parts as
  # regular files are accepted, and each is written by name, never by path.
  python3 - "$archive" "$stage" <<'PY' || { _backup_fail 'archive is not a Concord backup'; return 1; }
import os, sys, tarfile
archive, stage = sys.argv[1:3]
allowed = {"manifest.json", "postgres.dump", "env", "minio.tar"}
try:
    with tarfile.open(archive, "r:gz") as tar:
        for m in tar:
            name = m.name[2:] if m.name.startswith("./") else m.name
            if m.isdir() and name in ("", "."):
                continue
            if not m.isreg() or name not in allowed:
                sys.exit(1)
            fd = os.open(os.path.join(stage, name), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, "wb") as out, tar.extractfile(m) as src:
                while chunk := src.read(1 << 20):
                    out.write(chunk)
except (OSError, tarfile.TarError, EOFError):
    sys.exit(1)
PY
  _restore_verify "$stage" "$release" "$SELFHOST_STORAGE_MODE_RESOLVED" || return 1
  # Secrets in .env must match: a restored database under different keys breaks.
  cmp -s -- "$stage/env" "$PROJECT_ROOT/.env" || {
    _backup_fail 'the backup .env differs from this install; put the backup env in place as .env (0600) first'
    return 1
  }

  echo 'Stopping the stack...'
  compose stop || { _backup_fail 'stop failed; nothing was restored'; return 1; }
  echo 'Restoring the database...'
  compose up -d "$BACKUP_PG_SERVICE" || { _restore_stopped 'cannot start postgres; nothing was restored'; return 1; }
  for ((i = 0; ; i++)); do
    compose exec -T "$BACKUP_PG_SERVICE" pg_isready -U concord >/dev/null 2>&1 && break
    (( i < 30 )) || { _restore_stopped 'postgres did not become ready; nothing was restored'; return 1; }
    sleep 2
  done
  # --single-transaction: a failed restore leaves the database as it was.
  compose exec -T "$BACKUP_PG_SERVICE" pg_restore -U concord -d concord --clean --if-exists \
    --no-owner --single-transaction <"$stage/postgres.dump" \
    || { _restore_stopped 'pg_restore failed; the database is unchanged'; return 1; }
  if [[ "$SELFHOST_STORAGE_MODE_RESOLVED" == bundled ]]; then
    echo 'Restoring bundled MinIO data...'
    # Unpack beside the live data and swap only after tar succeeds, so a bad
    # stream leaves the existing objects in place.
    docker run --rm -i --network none -u 0 -v "$(_backup_minio_volume):/data" \
      --entrypoint sh "$BACKUP_HELPER_IMAGE" -c '
        set -e
        new=/data/.concord-restore
        rm -rf "$new"; mkdir "$new"
        tar -C "$new" -xf - || { rm -rf "$new"; exit 1; }
        find /data -mindepth 1 -maxdepth 1 ! -name .concord-restore -exec rm -rf {} +
        find "$new" -mindepth 1 -maxdepth 1 -exec mv {} /data/ \;
        rmdir "$new"' \
      <"$stage/minio.tar" \
      || { _restore_stopped 'MinIO restore failed after the database was restored; re-run restore'; return 1; }
  fi
  echo 'Starting the stack...'
  main up || return 1
  main health
}

# _restore_stopped MSG — a failure after the stack was stopped says so.
_restore_stopped() {
  _backup_fail "$1"
  printf 'selfhost-backup: the stack is stopped; run concord-selfhost up to start it again\n' >&2
}
