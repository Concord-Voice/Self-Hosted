#!/usr/bin/env bash
# Self-host update to a verified release tag (#185). Sourced by concord-selfhost
# together with selfhost-backup.sh; relies on `main`, `compose` and verb_backup.
#
# Trust root: infrastructure/deploy/release-signers, an SSH allowed-signers
# file (git's gpg.ssh.allowedSignersFile format) naming the release keys.
# Until the release-signing work signs tags, that file holds no key and every
# tag is refused: the verb fails closed rather than trusting the remote.

readonly UPDATE_SIGNERS_FILE="$SCRIPT_DIR/release-signers"
# Fetched tags land here, never under refs/tags, so a remote cannot replace a local tag.
readonly UPDATE_CANDIDATE_REF=refs/concord-update/candidate
readonly UPDATE_TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'

_update_fail() { printf 'selfhost-update: %s\n' "$1" >&2; return 1; }

verb_update() {
  local tag='' confirm=0 json=0 rc=0
  while (( $# )); do
    case "$1" in
      --to) [[ -n "${2:-}" ]] || { _update_fail '--to needs a tag'; return 1; }; tag="$2"; shift 2 ;;
      --confirm) confirm=1; shift ;;
      --json) json=1; shift ;;
      *) _update_fail 'update accepts --to TAG --confirm [--json]'; return 1 ;;
    esac
  done
  [[ "$tag" =~ $UPDATE_TAG_RE ]] || { _update_fail 'tag must look like vX.Y.Z'; return 1; }
  (( confirm )) || { _update_fail 'update takes a backup, switches release and restarts the stack; re-run with --confirm'; return 1; }
  if (( json )); then
    _update_run "$tag" >&2 || rc=$?
    if (( rc == 0 )); then printf '{"schema":1,"ok":true}\n'; else printf '{"schema":1,"ok":false}\n'; fi
    return "$rc"
  fi
  _update_run "$tag"
}

# _update_signed_keys — at least one non-comment line in the signers file.
_update_signed_keys() {
  [[ -f "$UPDATE_SIGNERS_FILE" ]] && grep -qvE '^[[:space:]]*(#|$)' "$UPDATE_SIGNERS_FILE"
}

# _update_tag_header REF — "<type> <tag>" from the signed tag object's header.
_update_tag_header() {
  git -C "$PROJECT_ROOT" cat-file tag "$1" 2>/dev/null \
    | awk '/^$/ { exit } $1 == "type" { t = $2 } $1 == "tag" { n = $2 } END { print t " " n }'
}

_update_run() {
  local tag="$1" previous
  git -C "$PROJECT_ROOT" rev-parse --git-dir >/dev/null 2>&1 \
    || { _update_fail 'update needs a git checkout of Self-Hosted'; return 1; }
  _update_signed_keys || { _update_fail 'no release signing keys are configured; refusing every tag'; return 1; }
  [[ -z "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=no)" ]] \
    || { _update_fail 'tracked files are modified; commit or discard local changes first'; return 1; }
  previous="$(git -C "$PROJECT_ROOT" rev-parse HEAD)" || return 1

  echo "Fetching $tag..."
  git -C "$PROJECT_ROOT" fetch --no-tags origin "+refs/tags/$tag:$UPDATE_CANDIDATE_REF" \
    || { _update_fail 'cannot fetch the tag'; return 1; }
  git -C "$PROJECT_ROOT" -c gpg.format=ssh -c "gpg.ssh.allowedSignersFile=$UPDATE_SIGNERS_FILE" \
    verify-tag "$UPDATE_CANDIDATE_REF" >/dev/null 2>&1 \
    || { _update_fail 'tag signature is missing or not from a release key'; return 1; }
  # verify-tag checks the signature only. The signed header must name this tag
  # and a commit, or an old signed release could be served under a newer name.
  [[ "$(_update_tag_header "$UPDATE_CANDIDATE_REF")" == "commit $tag" ]] \
    || { _update_fail 'the signed tag is for a different release than requested'; return 1; }

  echo 'Taking a backup first...'
  verb_backup || { _update_fail 'backup failed; nothing was changed'; return 1; }

  git -C "$PROJECT_ROOT" checkout -q --detach "$UPDATE_CANDIDATE_REF^{commit}" \
    || { _update_fail 'cannot switch to the tag; nothing else was changed'; return 1; }
  echo "Switched to $tag. Rebuilding..."
  # Run the tag's own wrapper: this shell still holds the previous version.
  local cli="$SCRIPT_DIR/concord-selfhost"
  if "$cli" up && "$cli" health; then
    echo "Updated to $tag."
    return 0
  fi
  _update_fail "the stack did not come back healthy on $tag"
  local backup_name="${BACKUP_LAST_NAME:-LATEST_BACKUP}"
  printf 'To roll back:\n  git -C %q checkout --detach %s\n  ./infrastructure/deploy/concord-selfhost restore backups/%s --confirm\n' \
    "$PROJECT_ROOT" "$previous" "$backup_name" >&2
  return 1
}
