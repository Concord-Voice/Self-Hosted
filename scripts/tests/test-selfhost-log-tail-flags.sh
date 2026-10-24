#!/usr/bin/env bash
# Regression for clustered Docker Compose log tail options.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TARGET="$ROOT/infrastructure/deploy/concord-selfhost"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
MODEL='{"services":{"control-plane":{},"media-plane":{}}}'
QUERY_LOG="$TMP/queries"
LOG_CALL="$TMP/log-call"
# shellcheck source=/dev/null # The sourced target is resolved from the active checkout.
source "$TARGET"

# Mock only Compose. Persist calls because verb_logs captures config output.
# shellcheck disable=SC2329 # Invoked indirectly by verb_logs.
compose() {
  if [[ "${3:-}" == config ]]; then
    printf 'query\n' >>"$QUERY_LOG"
    printf '%s\n' "$MODEL"
  elif [[ "${3:-}" == logs ]]; then
    printf '%s\n' "$@" >"$LOG_CALL"
  else
    printf 'unexpected compose call: %s\n' "$*" >&2
    return 99
  fi
}

fails=0
check_case() {
  local name="$1" want_rc="$2" want_error="$3" want_query="$4"
  local want_log="$5" out rc status have_query=0
  shift 5
  local -a expected=() arguments=()
  while (($#)) && [[ "$1" != :: ]]; do expected+=("$1"); shift; done
  [[ "${1:-}" == :: ]] && shift
  arguments=("$@")
  : >"$QUERY_LOG"; : >"$LOG_CALL"
  if out="$(verb_logs "${arguments[@]}" 2>&1)"; then rc=0; else rc=$?; fi
  status=PASS
  if [[ "$rc" != "$want_rc" ]] || [[ "$out" != *"$want_error"* ]]; then status=FAIL; fi
  [[ -s "$QUERY_LOG" ]] && have_query=1
  if [[ "$have_query" != "$want_query" ]]; then status=FAIL; fi
  if [[ "$want_log" == 1 ]]; then
    if [[ ! -f "$LOG_CALL" ]] || ! diff -u <(printf '%s\n' "${expected[@]}") "$LOG_CALL" >/dev/null; then status=FAIL; fi
  elif [[ -s "$LOG_CALL" ]]; then status=FAIL
  fi
  if [[ "$status" == PASS ]]; then
    printf 'PASS: %s\n' "$name"
  else
    printf 'FAIL: %s (expected rc=%s error=%q query=%s logs=%s; got rc=%s message=%q logs=%s)\n' \
      "$name" "$want_rc" "$want_error" "$want_query" "$want_log" "$rc" "$out" \
      "$(if [[ -f "$LOG_CALL" ]]; then tr '\n' ' ' <"$LOG_CALL"; else printf '<none>'; fi)"
    fails=$((fails + 1))
  fi
}

check_case 'separated -tn consumes tail and dispatches service' 0 '' 1 1 --profile '*' logs -f -tn 20 control-plane :: -tn 20 control-plane
check_case 'separated -fn consumes tail and dispatches service' 0 '' 1 1 --profile '*' logs -f -fn 20 control-plane :: -fn 20 control-plane
check_case 'separated -tfn consumes tail and dispatches service' 0 '' 1 1 --profile '*' logs -f -tfn 20 control-plane :: -tfn 20 control-plane
check_case 'completed -tn omits follow and dispatches service' 0 '' 1 1 --profile '*' logs -tn 20 control-plane :: --no-follow -tn 20 control-plane
check_case 'attached -tn20 preserves service dispatch' 0 '' 1 1 --profile '*' logs -f -tn20 control-plane :: -tn20 control-plane
check_case 'attached -fn20 preserves service dispatch' 0 '' 1 1 --profile '*' logs -f -fn20 control-plane :: -fn20 control-plane
check_case 'separate -t -n remains valid' 0 '' 1 1 --profile '*' logs -f -t -n 20 control-plane :: -t -n 20 control-plane
check_case 'plain -n remains valid' 0 '' 1 1 --profile '*' logs -f -n 20 control-plane :: -n 20 control-plane
check_case 'flag-like --since value is not follow' 0 '' 1 1 --profile '*' logs --since -f control-plane :: --no-follow --since -f control-plane
check_case 'unknown service remains refused' 1 'unknown log service' 1 0 :: absent-service
check_case 'no-follow clustered follow remains refused before query' 1 'conflicting log follow options' 0 0 :: --no-follow -fn 20 control-plane

exit "$((fails > 0))"
