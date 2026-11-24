#!/usr/bin/env bash
# Root enrollment reset must remove Host Agent trust before changing DB state.
# shellcheck disable=SC2034  # stub inputs are read by the sourced wrapper and fake agent
set -uo pipefail

DEPLOY="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf -- "$WORK"' EXIT
CALLS="$WORK/calls"
ADMIN_ID=3e7138e5-6eb9-4a79-8133-5eab67b7f2b6
fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

cat >"$WORK/agent" <<'AGENT'
#!/usr/bin/env bash
printf 'agent %s\n' "$*" >>"$CALLS"
exit "${STUB_REVOKE_RC:-0}"
AGENT
chmod +x "$WORK/agent"

run_reset() (
  # shellcheck source=../concord-selfhost
  source "$DEPLOY/concord-selfhost"
  require_root() { return "${STUB_ROOT_RC:-0}"; }
  find_agent_binary() {
    [[ "${STUB_AGENT_PRESENT:-1}" == 1 ]] || return 1
    printf '%s\n' "$WORK/agent"
  }
  safe_empty_authorizer_projection() { return "${STUB_EMPTY_TRUST:-1}"; }
  no_host_agent_installed() { return "${STUB_NO_AGENT_HOST:-1}"; }
  compose() {
    printf 'compose %s\n' "$*" >>"$CALLS"
    case "$*" in
      'exec -T control-plane ./main admin lookup-id --username alice')
        [[ "${STUB_LOOKUP_RC:-0}" == 0 ]] || return "$STUB_LOOKUP_RC"
        printf '%s\n' "${STUB_LOOKUP_ID:-$ADMIN_ID}"
        ;;
      'exec -T control-plane ./main admin reset-enrollment --username alice')
        [[ "${STUB_RESET_RC:-0}" == 0 ]] || return "$STUB_RESET_RC"
        printf 'new-enrollment-token\n'
        ;;
      *) return 1 ;;
    esac
  }
  verb_admin "$@"
)

: >"$CALLS"
out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice)"; rc=$?
expected="$(printf 'compose exec -T control-plane ./main admin lookup-id --username alice\nagent authorizer list\nagent authorizer revoke --admin-id %s\ncompose exec -T control-plane ./main admin reset-enrollment --username alice' "$ADMIN_ID")"
[[ "$rc" == 0 && "$out" == 'new-enrollment-token' && "$(cat "$CALLS")" == "$expected" ]] \
  && pass 'lookup, root revoke, then DB reset' || fail "happy path rc=$rc calls=$(cat "$CALLS")"

: >"$CALLS"
out="$(STUB_EMPTY_TRUST=0 CALLS="$CALLS" run_reset reset-enrollment --username alice)"; rc=$?
expected="$(printf 'compose exec -T control-plane ./main admin lookup-id --username alice\ncompose exec -T control-plane ./main admin reset-enrollment --username alice')"
[[ "$rc" == 0 && "$out" == 'new-enrollment-token' && "$(cat "$CALLS")" == "$expected" ]] \
  && pass 'safe never-imported trust skips revoke and permits reset' || fail "fresh host rc=$rc calls=$(cat "$CALLS")"

check_projection() (
  # shellcheck source=../concord-selfhost
  source "$DEPLOY/concord-selfhost"
  stat() { printf '%s\n' "${STUB_STAT:-0 755}"; }
  safe_empty_authorizer_projection "$1"
)
trust_dir="$WORK/concord-voice/agent-authorizers"
mkdir -p "$trust_dir"
trust_file="$trust_dir/authorizers.json"
check_projection "$trust_file" && pass 'root-owned empty trust directory permits fresh reset' \
  || fail 'safe empty trust directory refused'
touch "$trust_file"
check_projection "$trust_file" && fail 'existing trust file treated as absent' \
  || pass 'existing trust file requires CLI revocation'
rm "$trust_file"
ln -s "$WORK/missing" "$trust_file"
check_projection "$trust_file" && fail 'broken trust symlink treated as absent' \
  || pass 'broken trust symlink refuses absence shortcut'
rm "$trust_file"
STUB_STAT='1000 755' check_projection "$trust_file" && fail 'nonroot trust directory accepted' \
  || pass 'nonroot trust directory refuses absence shortcut'
STUB_STAT='0 777' check_projection "$trust_file" && fail 'writable trust directory accepted' \
  || pass 'writable trust directory refuses absence shortcut'

check_binary() (
  # shellcheck source=../concord-selfhost
  source "$DEPLOY/concord-selfhost"
  stat() {
    local target="${*: -1}"
    if [[ "$target" == "${STUB_UNSAFE_PATH:-}" ]]; then
      printf '%s\n' "${STUB_UNSAFE_STAT:-1000 755}"
    else
      printf '0 755\n'
    fi
  }
  find_agent_binary "$1"
)
binary_dir="$WORK/agent-install/libexec"
mkdir -p "$binary_dir"
cp "$WORK/agent" "$binary_dir/concord-agent"
check_binary "$binary_dir/concord-agent" >/dev/null && pass 'trusted installed agent binary accepted' \
  || fail 'trusted agent binary refused'
STUB_UNSAFE_PATH="$binary_dir/concord-agent" check_binary "$binary_dir/concord-agent" >/dev/null 2>&1 \
  && fail 'nonroot installed executable accepted' || pass 'nonroot executable refused'
STUB_UNSAFE_PATH="$binary_dir/concord-agent" STUB_UNSAFE_STAT='0 777' check_binary "$binary_dir/concord-agent" >/dev/null 2>&1 \
  && fail 'writable installed executable accepted' || pass 'writable executable refused'
STUB_UNSAFE_PATH="$binary_dir" STUB_UNSAFE_STAT='0 777' check_binary "$binary_dir/concord-agent" >/dev/null 2>&1 \
  && fail 'writable executable ancestor accepted' || pass 'writable executable ancestor refused'
ln -s "$binary_dir" "$WORK/agent-link"
check_binary "$WORK/agent-link/concord-agent" >/dev/null 2>&1 \
  && fail 'symlinked executable ancestor accepted' || pass 'symlinked executable ancestor refused'

# A host with no Host Agent at all has nothing to revoke; it must still reset.
: >"$CALLS"
out="$(STUB_NO_AGENT_HOST=0 STUB_AGENT_PRESENT=0 CALLS="$CALLS" run_reset reset-enrollment --username alice)"; rc=$?
expected="$(printf 'compose exec -T control-plane ./main admin lookup-id --username alice\ncompose exec -T control-plane ./main admin reset-enrollment --username alice')"
[[ "$rc" == 0 && "$out" == 'new-enrollment-token' && "$(cat "$CALLS")" == "$expected" ]] \
  && pass 'host with no Host Agent installed resets without a revoke' || fail "no-agent host rc=$rc calls=$(cat "$CALLS")"

# The reset failing AFTER the revoke must say trust is already revoked.
: >"$CALLS"
err="$(STUB_RESET_RC=1 CALLS="$CALLS" run_reset reset-enrollment --username alice 2>&1 >/dev/null)"; rc=$?
[[ "$rc" != 0 && "$err" == *'AFTER Host Agent trust was revoked'* ]] \
  && pass 'reset failure after revoke tells the operator trust is already revoked' || fail "post-revoke failure rc=$rc err=$err"

# The operator sees the root store before anything is revoked.
: >"$CALLS"
CALLS="$CALLS" run_reset reset-enrollment --username alice >/dev/null 2>&1
list_line="$(grep -n '^agent authorizer list$' "$CALLS" | cut -d: -f1)"; revoke_line="$(grep -n '^agent authorizer revoke ' "$CALLS" | cut -d: -f1)"
[[ -n "$list_line" && -n "$revoke_line" && "$list_line" -lt "$revoke_line" ]] \
  && pass 'authorizer list is shown before the revoke' || fail "list/revoke order list=$list_line revoke=$revoke_line"

for scenario in root lookup malformed agent revoke; do
  : >"$CALLS"
  case "$scenario" in
    root) STUB_ROOT_RC=1 out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice 2>/dev/null)"; rc=$? ;;
    lookup) STUB_LOOKUP_RC=1 out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice 2>/dev/null)"; rc=$? ;;
    malformed) STUB_LOOKUP_ID='not-a-uuid' out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice 2>/dev/null)"; rc=$? ;;
    agent) STUB_AGENT_PRESENT=0 out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice 2>/dev/null)"; rc=$? ;;
    revoke) STUB_REVOKE_RC=1 out="$(CALLS="$CALLS" run_reset reset-enrollment --username alice 2>/dev/null)"; rc=$? ;;
  esac
  if [[ "$rc" != 0 && "$out" == '' ]] && ! grep -q 'reset-enrollment --username' "$CALLS"; then
    pass "$scenario failure cannot mint a new enrollment token"
  else
    fail "$scenario rc=$rc out=$out calls=$(cat "$CALLS")"
  fi
done

: >"$CALLS"
run_reset reset-enrollment --username alice extra >/dev/null 2>&1; rc=$?
[[ "$rc" != 0 && ! -s "$CALLS" ]] && pass 'unknown trailing argument refuses before lookup' \
  || fail "argument refusal rc=$rc calls=$(cat "$CALLS")"

(( fails == 0 )) && { echo 'ALL PASS'; exit 0; }
echo "$fails FAILED"
exit 1
