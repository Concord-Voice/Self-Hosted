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
  local owner_mode
  owner_mode="$(stat -c '%u %a' -- "$1" 2>/dev/null)" || owner_mode=''
  [[ -d "$1" && ! -L "$1" && "$owner_mode" =~ ^[0-9]+\ [0-7]00$ && "${owner_mode%% *}" == "$(id -u)" ]] \
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

# _restore_stage_jwt_secret ARCHIVED_ENV CURRENT_ENV STAGED_ENV — the archive may predate
# a recovery-token use, so preserving its signing secret would revive that token
# when Redis is empty. Every other .env byte must still match exactly.
_restore_stage_jwt_secret() {
  local archived_env="$1" current_env="$2" staged_env="$3" rc=0
  [[ -f "$archived_env" && -f "$current_env" && ! -L "$current_env" && -f "$staged_env" ]] || {
    _backup_fail 'restore environment refused'
    return 1
  }
  # Exit 2: the files differ outside JWT_SECRET. Exit 3: an I/O error.
  python3 - "$archived_env" "$current_env" "$staged_env" <<'PY' || rc=$?
import os, re, secrets, sys
archived_path, current_path, staged_path = sys.argv[1:]
# Any assignment form Compose could read counts, so `export JWT_SECRET=` or an
# indented copy cannot sit beside the canonical line and keep the old secret.
ANY_JWT = re.compile(rb"^[ \t]*(export[ \t]+)?JWT_SECRET[ \t]*=")
def replace(text, value):
    lines = text.splitlines(keepends=True)
    indexes = [index for index, line in enumerate(lines) if ANY_JWT.match(line)]
    if len(indexes) != 1 or not lines[indexes[0]].startswith(b"JWT_SECRET="):
        raise ValueError()
    index = indexes[0]
    ending = b"\r\n" if lines[index].endswith(b"\r\n") else b"\n" if lines[index].endswith(b"\n") else b""
    lines[index] = b"JWT_SECRET=" + value + ending
    return b"".join(lines)
try:
    archived = open(archived_path, "rb").read()
    current = open(current_path, "rb").read()
except OSError:
    sys.exit(3)
try:
    if replace(archived, b"<JWT_SECRET>") != replace(current, b"<JWT_SECRET>"):
        sys.exit(2)
except ValueError:
    sys.exit(2)
try:
    with open(staged_path, "wb") as output:
        output.write(replace(current, secrets.token_hex(32).encode()))
        output.flush()
        source = os.stat(current_path)
        os.fchown(output.fileno(), source.st_uid, source.st_gid)
        os.fchmod(output.fileno(), 0o600)
        os.fsync(output.fileno())
except OSError:
    sys.exit(3)
PY
  case "$rc" in
    0) return 0 ;;
    2) _backup_fail 'the backup .env differs from this install outside JWT_SECRET' ;;
    *) _backup_fail 'cannot write the restored environment with a new signing secret' ;;
  esac
  return 1
}

# _restore_start_redis — start only the legacy Redis service and prove its
# password-protected command path works before restoring authorization state.
_restore_start_redis() {
  local i reply=''
  echo 'Starting Redis for authorization cleanup...'
  compose up -d redis || { _restore_auth_incomplete 'cannot start Redis for authorization cleanup'; return 1; }
  for ((i = 0; i < 30; i++)); do
    reply="$(compose exec -T redis redis-cli --raw PING 2>/dev/null)" || reply=''
    [[ "$reply" == PONG ]] && return 0
    sleep 1
  done
  _restore_auth_incomplete 'Redis authentication did not become ready'
  return 1
}

# _restore_purge_redis_authority — remove only opaque authorization state that
# can survive a same-host Redis volume across a database restore, plus the
# credential-epoch cache, whose newer values would refuse the restored epochs.
# Each Redis script processes one bounded SCAN page; unrelated keys are never
# deleted.
_restore_purge_redis_authority() {
  local cursor=0 reply scans=0 found=0
  local prefixes=(
    'admin_session:'
    'admin_enroll:'
    'admin_login_challenge:'
    'admin_enroll_challenge:'
    'admin_server_stepup:'
    'ws_ticket:'
    'recovery_code:'
    'email_verify:'
    'sso_token:'
    'cred_epoch:'
  )
  local purge_script='local r=redis.call("SCAN",ARGV[1],"COUNT",10000); local del={}; for _,k in ipairs(r[2]) do for i=2,#ARGV do local p=ARGV[i]; if string.sub(k,1,string.len(p))==p then table.insert(del,k); break end end end; if #del>0 then redis.call("UNLINK",unpack(del)) end; return r[1]'
  local verify_script='local r=redis.call("SCAN",ARGV[1],"COUNT",10000); local found=0; for _,k in ipairs(r[2]) do for i=2,#ARGV do local p=ARGV[i]; if string.sub(k,1,string.len(p))==p then found=found+1; break end end end; return r[1]..":"..found'

  echo 'Purging restored Redis authorization state...'
  while (( scans < 100000 )); do
    reply="$(compose exec -T redis redis-cli --raw EVAL "$purge_script" 0 "$cursor" "${prefixes[@]}" 2>/dev/null)" || {
      _restore_auth_incomplete 'cannot purge Redis authorization state'
      return 1
    }
    [[ "$reply" =~ ^[0-9]+$ ]] || {
      _restore_auth_incomplete 'Redis purge returned an invalid cursor'
      return 1
    }
    cursor="$reply"
    ((scans += 1))
    [[ "$cursor" == 0 ]] && break
  done
  [[ "$cursor" == 0 ]] || {
    _restore_auth_incomplete 'Redis purge did not finish'
    return 1
  }

  cursor=0
  scans=0
  while (( scans < 100000 )); do
    reply="$(compose exec -T redis redis-cli --raw EVAL "$verify_script" 0 "$cursor" "${prefixes[@]}" 2>/dev/null)" || {
      _restore_auth_incomplete 'cannot verify Redis authorization cleanup'
      return 1
    }
    [[ "$reply" =~ ^([0-9]+):([0-9]+)$ ]] || {
      _restore_auth_incomplete 'Redis verification returned an invalid cursor'
      return 1
    }
    cursor="${BASH_REMATCH[1]}"
    found=$((found + BASH_REMATCH[2]))
    ((scans += 1))
    [[ "$cursor" == 0 ]] && break
  done
  [[ "$cursor" == 0 && "$found" == 0 ]] || {
    _restore_auth_incomplete 'Redis authorization state remains after cleanup'
    return 1
  }
}

_restore_body() {
  local archive="$1" stage="$2" release="$3" i staged_env=''
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
  staged_env="$(umask 077; mktemp "$PROJECT_ROOT/.env.restore.XXXXXX")" || {
    _backup_fail 'cannot stage the restored environment'
    return 1
  }
  _restore_stage_jwt_secret "$stage/env" "$PROJECT_ROOT/.env" "$staged_env" || {
    rm -f -- "$staged_env"
    return 1
  }

  echo 'Stopping the stack...'
  compose stop || { rm -f -- "$staged_env"; _backup_fail 'stop failed; nothing was restored'; return 1; }
  echo 'Rotating the signing secret...'
  mv -f -- "$staged_env" "$PROJECT_ROOT/.env" \
    || { rm -f -- "$staged_env"; _restore_stopped 'cannot install the new signing secret'; return 1; }
  staged_env=''
  _restore_start_redis || return 1
  echo 'Restoring the database...'
  compose up -d "$BACKUP_PG_SERVICE" || { _restore_auth_incomplete 'cannot start postgres before authorization cleanup'; return 1; }
  for ((i = 0; ; i++)); do
    compose exec -T "$BACKUP_PG_SERVICE" pg_isready -U concord >/dev/null 2>&1 && break
    (( i < 30 )) || { _restore_auth_incomplete 'postgres did not become ready before authorization cleanup'; return 1; }
    sleep 2
  done
  # --single-transaction: a failed restore leaves the database as it was.
  compose exec -T "$BACKUP_PG_SERVICE" pg_restore -U concord -d concord --clean --if-exists \
    --no-owner --single-transaction <"$stage/postgres.dump" \
    || { _restore_auth_incomplete 'pg_restore failed; the database is unchanged but authorization cleanup did not run'; return 1; }
  # An older backup can revive a refresh token revoked after it was taken.
  # Use a past timestamp so the 30-second grace-refresh path cannot recover it.
  compose exec -T "$BACKUP_PG_SERVICE" psql -U concord -d concord -v ON_ERROR_STOP=1 \
    -c "UPDATE refresh_tokens SET revoked_at = now() - interval '1 hour' WHERE revoked_at IS NULL" >/dev/null \
    || { _restore_auth_incomplete 'cannot revoke restored sessions'; return 1; }
  _restore_purge_redis_authority || return 1
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
      || { _restore_incomplete 'MinIO restore failed after the database was restored'; return 1; }
  fi
  echo 'Starting the stack...'
  main up || return 1
  main health || return 1
  echo 'Restore complete. Every user must sign in again. Password, MFA, key and invite changes made after this backup were rolled back; re-apply them (see the restore runbook).'
}

# _restore_stopped MSG — a pre-restore failure left the prior authorization
# state intact, so the operator can resume it.
_restore_stopped() {
  _backup_fail "$1"
  printf 'selfhost-backup: the stack is stopped; run concord-selfhost up to resume the prior state\n' >&2
}

# _restore_auth_incomplete MSG — JWT rotation or database restore has started,
# but its matching opaque authorization cleanup did not complete.
_restore_auth_incomplete() {
  _backup_fail "$1"
  printf 'selfhost-backup: authorization invalidation is incomplete; DO NOT run concord-selfhost up; fix the failure and re-run restore\n' >&2
}

# _restore_incomplete MSG — persistent state has changed but the restore did
# not reach its completed state.
_restore_incomplete() {
  _backup_fail "$1"
  printf 'selfhost-backup: the stack is stopped; the restore is incomplete; DO NOT run concord-selfhost up; fix the failure and re-run restore\n' >&2
}
