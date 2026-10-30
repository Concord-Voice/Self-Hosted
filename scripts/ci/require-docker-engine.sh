#!/usr/bin/env bash
# Read-only publisher admission against the daemon used by subsequent Docker calls.
set -euo pipefail
export LC_ALL=C

readonly minimum='29.8.2'
readonly -a minimum_parts=(29 8 2)

refuse() {
  printf 'error: %s; Docker Engine server >=%s is required\n' "$1" "$minimum" >&2
  exit 1
}

[[ $# -eq 0 ]] || refuse 'Docker Engine guard accepts no arguments'

if ! response_file="$(mktemp 2>/dev/null)"; then
  refuse 'unable to capture Docker Engine server version'
fi
trap 'rm -f -- "$response_file"' EXIT

if ! docker version --format '{{.Server.Version}}' >"$response_file" 2>/dev/null; then
  refuse 'Docker Engine server version query failed'
fi

# Unlike command substitution, read preserves terminal LF bytes. A NUL is an
# invalid delimiter, so refuse it rather than letting Bash silently discard it.
if IFS= read -r -d '' response <"$response_file"; then
  refuse 'invalid Docker Engine server version response'
fi
response="${response%$'\n'}"
if [[ ! "$response" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[A-Za-z0-9.+-]+)?$ ]]; then
  refuse 'invalid Docker Engine server version response'
fi

server_version="${response%%+*}"
IFS='.' read -r -a server_parts <<<"$server_version"
for i in 0 1 2; do
  actual="${server_parts[i]}"
  required="${minimum_parts[i]}"
  # Compare decimal strings without fixed-width arithmetic overflow.
  if [[ ${#actual} -gt ${#required} || (${#actual} -eq ${#required} && "$actual" > "$required") ]]; then
    break
  fi
  if [[ ${#actual} -lt ${#required} || (${#actual} -eq ${#required} && "$actual" < "$required") ]]; then
    refuse 'Docker Engine server version is below the supported floor'
  fi
done

# Canonical decimal fields have no width limit; keep diagnostics bounded even
# when a successful daemon response supplies an unusually large component.
if [[ ${#server_version} -le 64 ]]; then
  printf 'Docker Engine server %s satisfies >=%s\n' "$server_version" "$minimum"
else
  printf 'Docker Engine server satisfies >=%s\n' "$minimum"
fi
