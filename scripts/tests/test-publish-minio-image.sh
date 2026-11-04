#!/usr/bin/env bash
# Structural contract tests for the Concord-built MinIO publisher and consumer.
# Structural checks always run; Compose semantics need the Compose CLI and jq, not a daemon or network.
# Regexes intentionally match literal shell variables in the workflow under test.
# shellcheck disable=SC2016
set -uo pipefail

# pre-commit exports its repository's Git context into each hook process. Scrub
# that context before creating the disposable fixture repositories below; otherwise
# their `git -C` commands can accidentally use the parent repository's index.
# shellcheck disable=SC2046
unset $(git rev-parse --local-env-vars)

# first_line: `| head -1` under `set -o pipefail` is a RACE, not a shorthand. head
# exits after the first line and closes the pipe; if the producer has not already
# finished, its next write takes SIGPIPE, the pipeline reports 141, and `set -e`
# kills the script silently with every later statement unreached. Whether it fires
# depends on producer buffering, so it passes on one machine and fails on another.
# first_line DRAINS its input, so there is nothing to race. Prose ABOUT the defect
# deliberately keeps saying `head -1`. Reference: #3105, commit 3f7144952 in
# infrastructure/deploy/tests/test-rtc-port-ingress-parity.sh.
first_line() {
  local l first=
  while IFS= read -r l || [[ -n "$l" ]]; do
    [[ -n "$first" ]] || { first=1; printf '%s\n' "$l"; }
  done
}

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
COMPOSE="$ROOT/docker-compose.yml"
DOCKERFILE="$ROOT/infrastructure/docker/minio/Dockerfile"
WORKFLOW="${MINIO_PUBLISH_WORKFLOW:-$ROOT/.github/workflows/publish-minio-image.yml}"
PR_CI="$ROOT/.github/workflows/pr-ci.yml"
SCAN_RELEVANCE="$ROOT/.github/ci/scan-relevance.yml"
PRECOMMIT="$ROOT/.pre-commit-config.yaml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

readonly SOURCE_COMMIT='9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a' # pragma: allowlist secret -- public upstream Git commit
readonly DERIVATIVE_COMMIT='c7d942fa52807929e119bcb4531e21d099ccd1a8' # pragma: allowlist secret -- public derivative Git commit
readonly DERIVATIVE_TREE='02a0f6c098c96088d2e46d7fb19286d79269d674' # pragma: allowlist secret -- public derivative Git tree
readonly PATCH_SHA256='4b27aac2247f59fbe60ca02ad507cf2775dfa74f0bf78d559afa3024e1137642' # pragma: allowlist secret -- public source checksum
readonly GO_MOD_SHA256='8da7dd5cb011e71491c39350ef0014817acfa6ae983c6d212e5802d5508cae1d' # pragma: allowlist secret -- public source checksum
readonly GO_SUM_SHA256='b6ebc303ee7255db47ed2610b9271876da1146b734ce92ac99ea519a244b9fcc' # pragma: allowlist secret -- public source checksum
readonly SOURCE_SHA256='7097139b7d2db54fda66c1e6fe18dd743a5baac67a0a3900372bdc37f8d17cca' # pragma: allowlist secret -- public source archive checksum
readonly RUNTIME_REF='ghcr.io/concord-voice/minio:RELEASE.2025-10-15T17-29-55Z.CONCORD.823f9aa2bd62@sha256:a1d35733ca68335cc4782e7a81eafa577d841c70ebf9e51fc07f54978fcfc494' # pragma: allowlist secret -- public OCI digest
readonly BUILDER_DIGEST='632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af' # pragma: allowlist secret -- public OCI digest
readonly RUNTIME_BASE_DIGEST='990002083442f6a93cd3249da32ecb7c3f6be778a1bec3a73a9c17fbc40edc15' # pragma: allowlist secret -- public OCI digest
readonly BUILDKIT_DIGEST='99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c' # pragma: allowlist secret -- public OCI digest
readonly BUILDX_VERSION='v0.35.0'
readonly BUILDX_LINUX_AMD64_SHA256='d41ece72044243b4f58b343441ae37446d9c29a7d6b5e11c61847bbcf8f7dfda' # pragma: allowlist secret -- public release-asset digest
readonly CHECKOUT_ACTION='actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1' # pragma: allowlist secret -- public action commit
readonly LOGIN_ACTION='docker/login-action@dbcb813823bdd20940b903addbd779551569679f' # pragma: allowlist secret -- public action commit (v4.6.0)
readonly BUILDX_ACTION='docker/setup-buildx-action@594f3bf4285d9ea8dc53c9a0c9c4092420091003' # pragma: allowlist secret -- public action commit
readonly BUILD_PUSH_ACTION='docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc' # pragma: allowlist secret -- public action commit
readonly ORAS_ACTION='oras-project/setup-oras@005458ad77f1c8facd38a094e4af2e69e5607ff4' # pragma: allowlist secret -- public action commit
readonly PROBE_RUNTIME_REF='example.invalid/minio-contract-probe:invalid'

REQUIRE_COMPOSE=0
if [[ "${1:-}" == "--require-compose" ]]; then
  REQUIRE_COMPOSE=1
  shift
fi
if [[ "$#" -ne 0 ]]; then
  echo "Usage: ${0##*/} [--require-compose]" >&2
  exit 2
fi

fails=0
pass() { echo "PASS: $1"; }
fail() {
  echo "FAIL: $1"
  fails=$((fails + 1))
}

assert_file() {
  local file="$1" description="$2"
  if [[ -f "$file" ]]; then
    pass "$description"
  else
    fail "$description (missing: ${file#"$ROOT/"})"
  fi
}

assert_absent() {
  local file="$1" description="$2"
  if [[ ! -e "$file" ]]; then
    pass "$description"
  else
    fail "$description (unexpected path: ${file#"$ROOT/"})"
  fi
}

assert_contains() {
  local file="$1" pattern="$2" description="$3"
  if grep -Eq -- "$pattern" "$file"; then
    pass "$description"
  else
    fail "$description"
  fi
}

assert_not_contains() {
  local file="$1" pattern="$2" description="$3"
  if grep -Eiq -- "$pattern" "$file"; then
    fail "$description"
  else
    pass "$description"
  fi
}

assert_equals() {
  local actual="$1" expected="$2" description="$3"
  if [[ "$actual" == "$expected" ]]; then
    pass "$description"
  else
    fail "$description (expected '$expected', got '$actual')"
  fi
}

assert_not_equals() {
  local actual="$1" unexpected="$2" description="$3"
  if [[ "$actual" != "$unexpected" ]]; then
    pass "$description"
  else
    fail "$description (unexpected '$actual')"
  fi
}

assert_count() {
  local file="$1" pattern="$2" expected="$3" description="$4" actual
  actual="$(grep -Ec -- "$pattern" "$file" || true)"
  assert_equals "$actual" "$expected" "$description"
}

assert_literal_count() {
  local file="$1" literal="$2" expected="$3" description="$4" actual
  actual="$(grep -Fc -- "$literal" "$file" || true)"
  assert_equals "$actual" "$expected" "$description"
}

assert_before() {
  # Locals renamed off `first_line` when the drain helper took that name: bash keeps
  # function and variable namespaces apart so the old spelling still ran, but
  # `first_line="$(... | first_line | ...)"` is not a line anyone should have to parse.
  local file="$1" first="$2" second="$3" description="$4" first_ln second_ln
  first_ln="$(grep -nFx -- "$first" "$file" | first_line | cut -d: -f1)"
  second_ln="$(grep -nFx -- "$second" "$file" | first_line | cut -d: -f1)"
  if [[ -n "$first_ln" && -n "$second_ln" && "$first_ln" -lt "$second_ln" ]]; then
    pass "$description"
  else
    fail "$description"
  fi
}

assert_sequence() {
  local file="$1" description="$2"
  shift 2
  local -a expected=("$@")
  local index=0 line
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    if [[ "$line" == "${expected[$index]}" ]]; then
      index=$((index + 1))
      if [[ "$index" -eq "${#expected[@]}" ]]; then
        pass "$description"
        return
      fi
    elif [[ "$line" == "${expected[0]}" ]]; then
      index=1
    else
      index=0
    fi
  done <"$file"
  fail "$description"
}

strip_comments() {
  awk '
    function uncomment(line,    i, c, previous, out, single, double, escaped, quote) {
      quote=sprintf("%c", 39)
      for (i=1; i<=length(line); i++) {
        c=substr(line, i, 1)
        previous=(i == 1 ? "" : substr(line, i-1, 1))
        if (escaped) { out=out c; escaped=0; continue }
        if (c == "\\" && !single) { out=out c; escaped=1; continue }
        if (c == quote && !double) { single=!single; out=out c; continue }
        if (c == "\"" && !single) { double=!double; out=out c; continue }
        if (c == "#" && !single && !double && (i == 1 || previous ~ /[[:space:]]/)) break
        out=out c
      }
      sub(/[[:space:]]+$/, "", out)
      return out
    }
    { active=uncomment($0); if (active !~ /^[[:space:]]*$/) print active }
  ' "$1" >"$2"
}

extract_workflow_run_step() {
  local workflow="$1" name="$2" output="$3"
  awk -v step="      - name: $name" '
    $0 == step { found=1; next }
    found && $0 == "        run: |" { body=1; next }
    body && substr($0, 1, 10) == "          " { print substr($0, 11); next }
    body { exit }
  ' "$workflow" >"$output"
}

discover_compose_overlays() {
  local root="$1"
  {
    find "$root" -maxdepth 1 -type f \
      \( -name 'docker-compose*.yml' -o -name 'docker-compose*.yaml' \) \
      ! -name 'docker-compose.yml' -print
    if [[ -d "$root/.zap" ]]; then
      find "$root/.zap" -maxdepth 1 -type f \
        \( -name 'docker-compose*.yml' -o -name 'docker-compose*.yaml' \) -print
    fi
  } | LC_ALL=C sort
}

extract_minio_service() {
  local source="$1" destination="$2"
  awk '
    BEGIN {
      quote=sprintf("%c", 39)
      services_pattern="^[\"" quote "]?services[\"" quote "]?:$"
      minio_pattern="^[[:space:]]+[\"" quote "]?minio[\"" quote "]?[[:space:]]*:"
    }
    $0 ~ services_pattern { services=1; next }
    services && /^[^[:space:]]/ { exit }
    services && !service_indent {
      service_indent=match($0, /[^ ]/) - 1
    }
    services && service_indent {
      indent=match($0, /[^ ]/) - 1
      if (indent == service_indent &&
          $0 ~ minio_pattern) {
        if ($0 != sprintf("%*sminio:", service_indent, "")) exit 1
        inside=1
      } else if (inside && indent == service_indent && $0 ~ /^[[:space:]]+[^:]+:/) {
        exit
      }
      if (inside) print
    }
  ' "$source" >"$destination"
}

compose_minio_contract() {
  local overlay="${1:-}" probe="${2:-}" rendered placeholder="contract-check-not-a-secret"
  local -a compose_files=(-f "$COMPOSE")
  local -a compose_env=(
    "PATH=$PATH"
    "HOME=${HOME:-/tmp}"
    "DOMAIN=contract-check.invalid"
    "FEEDBACK_PAT=$placeholder"
    "FEEDBACK_REPO=example/example"
    "JWT_SECRET=$placeholder"
    "MFA_ENCRYPTION_KEY=$placeholder"
    # Config-only private overlay input; inherited base NUM_WORKERS is four.
    "MEDIASOUP_WEBRTC_SERVER_PORTS=40000-40003"
    "MINIO_ROOT_PASSWORD=$placeholder"
    "MINIO_ROOT_USER=contract-check"
    "POSTGRES_PASSWORD=$placeholder"
    "PUBLIC_IP=127.0.0.1"
    "REDIS_PASSWORD=$placeholder"
    "STORAGE_BACKEND=s3"
    "STORAGE_ENDPOINT=objects.contract-check.invalid:9443"
    "STORAGE_REGION="
    "STORAGE_ACCESS_KEY=contract-check-external-access"
    "STORAGE_SECRET_KEY=$placeholder"
    "STORAGE_USE_SSL=true"
    "STORAGE_BUCKET=$(printf '%s-%s-%s-%s' contract check external media)"
    "STAGE_PRIVATE_ADDRESS=10.246.6.1"
    "SMTP_HOST=smtp.contract-check.invalid"
    "SMTP_PASSWORD=$placeholder"
    "SMTP_FROM=stage@example.invalid"
    "TRUSTED_PROXY_CIDRS=127.0.0.1/32"
    "TURN_SECRET=$placeholder"
    "WS_EVENT_BUDGET_ENABLED=true"
    "WS_EVENT_BUDGET_SCALE_PERCENT=100"
  )
  if [[ -n "$probe" ]]; then
    compose_files+=(-f "$probe")
  fi
  if [[ -n "$overlay" ]]; then
    compose_files+=(-f "$overlay")
  fi
  if ! rendered="$(env -i "${compose_env[@]}" docker compose \
      --env-file /dev/null \
      --project-directory "$ROOT" \
      --profile '*' \
      "${compose_files[@]}" \
      config --format json 2>/dev/null)"; then
    return 1
  fi
  jq -ce '{
    image: .services.minio.image,
    pull_policy: .services.minio.pull_policy,
    build: (.services.minio.build // null)
  }' <<<"$rendered"
}

yaml_has_indirection_token() {
  awk '
    function has_token(line,    i, c, previous, following, single, double, escaped, boundary, quote) {
      quote=sprintf("%c", 39)
      for (i=1; i<=length(line); i++) {
        c=substr(line, i, 1)
        previous=(i == 1 ? "" : substr(line, i-1, 1))
        following=(i == length(line) ? "" : substr(line, i+1, 1))
        if (escaped) { escaped=0; continue }
        if (c == "\\" && double) { escaped=1; continue }
        if (c == quote && !double) { single=!single; continue }
        if (c == "\"" && !single) { double=!double; continue }
        if (single || double) continue
        boundary=(i == 1 || previous ~ /[[:space:]\[\]{},:]/)
        if (boundary && c == "&" && following !~ /^[[:space:]&]$/) return 1
        if (boundary && c == "*" && following !~ /^[[:space:]]$/) return 1
        if (boundary && c == "!" && following !~ /^[[:space:]]$/) return 1
        if (boundary && c == "?" && following ~ /^[[:space:]]$/) return 1
      }
      return 0
    }
    has_token($0) { found=1 }
    END { exit(found ? 0 : 1) }
  ' "$1"
}

yaml_has_empty_forbidden_key() {
  awk '
    function indentation(line) { return match(line, /[^ ]/) - 1 }
    function is_empty_forbidden(line,    text, key, quote, closing) {
      quote=sprintf("%c", 39)
      text=line
      sub(/^[ ]*/, "", text)
      if (substr(text, 1, 1) == "\"" || substr(text, 1, 1) == quote) {
        closing=index(substr(text, 2), substr(text, 1, 1))
        if (!closing) return 0
        key=substr(text, 2, closing-1)
        text=substr(text, closing+2)
      } else {
        if (!match(text, /^[A-Za-z_][A-Za-z0-9_-]*/)) return 0
        key=substr(text, 1, RLENGTH)
        text=substr(text, RLENGTH+1)
      }
      if (text !~ /^[ ]*:/) return 0
      sub(/^[ ]*:[ ]*/, "", text)
      return key ~ /^(image|pull_policy|build)$/ && text == ""
    }
    {
      current=indentation($0)
      if (pending) {
        if (current <= pending_indent) found=1
        pending=0
      }
      if (is_empty_forbidden($0)) {
        pending=1
        pending_indent=current
      }
    }
    END {
      if (pending) found=1
      exit(found ? 0 : 1)
    }
  ' "$1"
}

# Compose's merge tags are allowed ONLY as the whole value of a `ports:` key (#3538:
# docker-compose.selfhost.yml unpublishes the data tier with `ports: !reset []` and
# rebinds the planes with `ports: !override`). Strip exactly that shape before the
# token scan; any other tag, on any other key or with any other value, is still
# indirection. The semantic MinIO contract render below still runs on the overlay.
strip_ports_merge_tags() {  # $1 = file, rewritten in place
  sed -E 's/^([[:space:]]+ports:[[:space:]]*)!(reset|override)([[:space:]]+\[\])?[[:space:]]*$/\1\3/' "$1" >"$1.tags" && mv "$1.tags" "$1"
}

# The managed media colours use these exact profile tags and self-extension.
# Remove only their known shapes from the text guard; both real MinIO renders
# below still run against the unmodified overlay.
strip_media_placement_indirection() {  # $1 = file, rewritten in place
  awk '
    function depth(line, pos) { pos=match(line, /[^ ]/); return pos ? pos-1 : -1 }
    { lines[NR]=$0 }
    END {
      for (i=1; i<=NR; i++) {
        line=lines[i]
        level=depth(line)
        if (level == 0) { services=(line == "services:"); service="" }
        else if (services && level == 2) {
          service=(line == "  control-plane:" || line == "  media-plane:" || line == "  media-plane-blue:" ||
                   line == "  media-plane-green:") ? line : ""
        }
        if (services && level == 4 &&
            ((service == "  control-plane:" &&
              line == "    profiles: !override [services, media-bluegreen]") ||
             (service == "  media-plane:" &&
              (line == "    profiles: !override [media-single]" ||
               line == "    profiles: !override [services]" ||
               line == "    profiles: !override [services, media-single]")) ||
             ((service == "  media-plane-blue:" || service == "  media-plane-green:") &&
              line == "    profiles: !override [media-bluegreen]"))) {
          sub(/!override /, "", line)
        } else if (services && level == 4 &&
                   (service == "  media-plane-blue:" || service == "  media-plane-green:") &&
                   line == "    extends:") {
          extension_end=i+1
          if (lines[extension_end] == "      file: ./docker-compose.yml") extension_end++
          if (lines[extension_end] != "      service: media-plane") { print line; continue }
          next_line=extension_end+1
          while (next_line<=NR && depth(lines[next_line]) == -1) next_line++
          if (next_line>NR || depth(lines[next_line])<=4) { i=extension_end; continue }
        }
        print line
      }
    }
  ' "$1" >"$1.tags" && mv "$1.tags" "$1"
}

# #1899 permits one additional whole-map tag at this exact YAML path and with
# these exact healthy dependencies. Leave any changed block or trailing token
# intact so the ordinary indirection scan refuses it.
# The production overlay shares one reviewed hardening block through a YAML
# anchor, and uses three Compose merge tags on non-MinIO services. Remove only
# the exact anchor declaration, exact service merge lines and those exact tags at
# their admitted service paths; any other anchor, alias, merge or tag stays
# visible and is rejected. The Compose render still evaluates the unmodified
# overlay, so a MinIO image, pull policy or build key added to the anchor body
# changes the effective contract and fails.
strip_production_indirection() {  # $1 = active production overlay, rewritten in place
  awk '
    /^  [^ ]/ { service=$0 }
    /^[^ ]/ { service="" }
    $0 == "x-hardening: &hardening" { print "x-hardening:"; next }
    $0 == "    <<: *hardening" { next }
    service == "  ops-agent:" && $0 == "    profiles: !override [selfhost-managed-ops-inactive]" {
      sub(/!override /, ""); print; next }
    service == "  control-plane:" && $0 == "    env_file: !reset []" { sub(/!reset /, ""); print; next }
    service == "  control-plane:" && $0 == "    volumes: !override" { sub(/ !override/, ""); print; next }
    { print }
  ' "$1" >"$1.tags" && mv "$1.tags" "$1"
}

strip_byo_dependency_merge_tag() {  # $1 = active canonical file, rewritten in place
  awk '
    BEGIN {
      expected[1]="    depends_on: !override"
      expected[2]="      postgres:"
      expected[3]="        condition: service_healthy"
      expected[4]="      redis:"
      expected[5]="        condition: service_healthy"
      expected[6]="      nats:"
      expected[7]="        condition: service_healthy"
    }
    {
      lines[NR]=$0
      indent=match($0, /[^ ]/) - 1
      if (indent == 0) {
        services=($0 == "services:")
        if (services) services_count++
        control=0
      }
      if (services && indent == 2) {
        control=($0 == "  control-plane:")
        if (control) control_count++
      }
      if (services && control && indent == 4 && /^    depends_on:/) {
        dependency_count++
        if ($0 == expected[1]) start=NR
      }
    }
    END {
      accepted=(services_count == 1 && control_count == 1 &&
                dependency_count == 1 && start && start+6 <= NR)
      for (i=1; i<=7; i++) {
        if (lines[start+i-1] != expected[i]) accepted=0
      }
      if (start+7 <= NR && match(lines[start+7], /[^ ]/) - 1 > 4) accepted=0
      for (i=1; i<=NR; i++) {
        if (accepted && i == start) print "    depends_on:"
        else print lines[i]
      }
    }
  ' "$1" >"$1.tags" && mv "$1.tags" "$1"
}

overlay_uses_forbidden_indirection() {
  local active="$TMP/overlay.active"
  local key_pattern="(^|[[:space:]{,])[\"']?(extends|<<)[\"']?[[:space:]]*:"
  local null_key_pattern="(^|[[:space:]{,])[\"']?(image|pull_policy|build)[\"']?[[:space:]]*:[[:space:]]*((~|null)([[:space:]]*(,|}|$))|!!null)"
  strip_comments "$1" "$active"
  strip_ports_merge_tags "$active"
  strip_media_placement_indirection "$active"
  # Only the canonical root overlay may use the dependency tag exception.
  if [[ "$1" == "$ROOT/docker-compose.byo-s3.yml" ]]; then
    strip_byo_dependency_merge_tag "$active"
  fi
  # Basename match lets the mutants below exercise the same exception.
  if [[ "${1##*/}" == "docker-compose.production.yml" ]]; then
    strip_production_indirection "$active"
  fi
  grep -Eq '\\|^[[:space:]]*\?' "$active" ||
    grep -Eiq -- "$key_pattern" "$active" ||
    grep -Eiq -- "$null_key_pattern" "$active" ||
    yaml_has_indirection_token "$active" ||
    yaml_has_empty_forbidden_key "$active"
}

minio_overlay_is_safe() {
  local contract probe_contract
  overlay_uses_forbidden_indirection "$1" && return 1
  contract="$(compose_minio_contract "$1")" || return 1
  probe_contract="$(compose_minio_contract "$1" "$MINIO_CONTRACT_PROBE")" || return 1
  [[ "$contract" == "$CANONICAL_MINIO_CONTRACT" &&
    "$probe_contract" == "$PROBED_MINIO_CONTRACT" ]]
}

assert_overlay_guard_accepts() {
  local overlay="$1" description="$2"
  [[ "$COMPOSE_SEMANTICS_AVAILABLE" -eq 1 ]] || return 0
  if minio_overlay_is_safe "$overlay"; then
    pass "$description"
  else
    fail "$description"
  fi
}

assert_overlay_guard_rejects() {
  local overlay="$1" description="$2"
  [[ "$COMPOSE_SEMANTICS_AVAILABLE" -eq 1 ]] || return 0
  if minio_overlay_is_safe "$overlay"; then
    fail "$description"
  else
    pass "$description"
  fi
}

assert_compose_accepts() {
  local compose_file="$1" description="$2"
  [[ "$COMPOSE_SEMANTICS_AVAILABLE" -eq 1 ]] || return 0
  if compose_minio_contract "$compose_file" >/dev/null; then
    pass "$description"
  else
    fail "$description"
  fi
}

COMPOSE_SEMANTICS_AVAILABLE=0
CANONICAL_MINIO_CONTRACT=""
MINIO_CONTRACT_PROBE="$TMP/minio-contract-probe.yml"
PROBED_MINIO_CONTRACT=""
printf '%s\n' 'services:' '  minio:' "    image: $PROBE_RUNTIME_REF" \
  '    pull_policy: always' >"$MINIO_CONTRACT_PROBE"
if command -v docker >/dev/null 2>&1 &&
    docker compose version >/dev/null 2>&1 &&
    command -v jq >/dev/null 2>&1; then
  if CANONICAL_MINIO_CONTRACT="$(compose_minio_contract)" &&
      jq -e --arg image "$RUNTIME_REF" \
        '.image == $image and .pull_policy == "missing" and .build == null' \
        <<<"$CANONICAL_MINIO_CONTRACT" >/dev/null &&
      PROBED_MINIO_CONTRACT="$(compose_minio_contract "" "$MINIO_CONTRACT_PROBE")" &&
      jq -e --arg image "$PROBE_RUNTIME_REF" \
        '.image == $image and .pull_policy == "always" and .build == null' \
        <<<"$PROBED_MINIO_CONTRACT" >/dev/null; then
    COMPOSE_SEMANTICS_AVAILABLE=1
  else
    fail "Docker Compose cannot render the canonical MinIO contract"
  fi
elif [[ "$REQUIRE_COMPOSE" -eq 1 ]]; then
  fail "--require-compose needs Docker Compose and jq"
else
  echo "SKIP: Compose-semantic MinIO checks (Docker Compose and jq unavailable)"
fi

normalize_lines() {
  awk '{$1=$1; print}' "$1" >"$2"
}

join_continuations() {
  awk '{ sub(/[[:space:]]+$/, ""); if (sub(/\\$/, "")) { printf "%s ", $0; next } print }' \
    "$1" >"$2"
}

FIXTURE_ROOT="$TMP/consumer-fixtures"
mkdir -p "$FIXTURE_ROOT/.zap"
printf '%s\n' 'services:' '  minio:' '    environment:' '      MINIO_BROWSER: off' \
  >"$FIXTURE_ROOT/docker-compose.valid.yml"
printf '%s\n' 'services:' '  worker:' '    image: example.invalid/worker' \
  '    depends_on:' '      minio:' \
  '        condition: service_started' \
  >"$FIXTURE_ROOT/docker-compose.depends-on.yml"
printf '%s\n' 'services:' '  minio:' '    environment:' '      MINIO_BROWSER: off' \
  >"$FIXTURE_ROOT/docker-compose.canary.yaml"
printf '%s\n' 'services:' '  worker:' '    image: example.invalid/worker' \
  >"$FIXTURE_ROOT/.zap/docker-compose.scan.yaml"
printf '%s\n' 'services:' '  minio:' '    build: ./infrastructure/docker/minio' \
  >"$FIXTURE_ROOT/docker-compose.build.yml"
printf '%s\n' 'services:' '  minio:' '      "build": ./infrastructure/docker/minio' \
  >"$FIXTURE_ROOT/docker-compose.quoted-build.yml"
printf '%s\n' 'x-minio: &minio-contract' '  image: minio/minio:latest' \
  'services:' '  minio:' '    <<: *minio-contract' \
  >"$FIXTURE_ROOT/docker-compose.merge.yml"
printf '%s\n' 'services:' '  canonical-minio:' "    image: $RUNTIME_REF" \
  '    pull_policy: missing' \
  >"$FIXTURE_ROOT/minio-extends-service.yml"
printf '%s\n' 'services:' '  environment-only:' '    environment:' \
  '      MINIO_BROWSER: off' \
  >"$FIXTURE_ROOT/minio-extends-environment.yml"
printf '%s\n' 'services:' '  minio:' '    extends:' \
  "      file: $FIXTURE_ROOT/minio-extends-service.yml" \
  '      service: canonical-minio' \
  >"$FIXTURE_ROOT/docker-compose.extends.yml"
printf '%s\n' 'x-extends-key: &extends-key extends' 'services:' '  minio:' \
  '    *extends-key:' \
  "      file: $FIXTURE_ROOT/minio-extends-environment.yml" \
  '      service: environment-only' \
  >"$FIXTURE_ROOT/docker-compose.alias-extends.yml"
printf '%s\n' 'x-extends-key: &/e extends' 'services:' '  minio:' \
  '    */e:' \
  "      file: $FIXTURE_ROOT/minio-extends-environment.yml" \
  '      service: environment-only' \
  >"$FIXTURE_ROOT/docker-compose.slash-alias-extends.yml"
printf '%s\n' 'services: { minio: { image: minio/minio:latest } }' \
  >"$FIXTURE_ROOT/.zap/docker-compose.flow.yml"
# Regression for #2252: valid Compose spellings must not bypass the runtime guard.
printf '%s\n' 'services:' '    minio:' '      image: minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.four-space.yml"
printf '%s\n' 'services: {' '  minio: {' '    image: minio/minio:latest' '  }' '}' \
  >"$FIXTURE_ROOT/docker-compose.multiline-flow.yml"
printf '%s\n' '"services":' '  minio:' '    image: minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.quoted-services.yml"
printf '%s\n' 'services: &svc' '  minio:' '    image: minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.services-anchor.yml"
printf '%s\n' 'services :' '  minio:' '    image: minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.services-space.yml"
printf '%s\n' 'x-services: &unsafe-services' '  minio:' \
  '    image: minio/minio:latest' 'services:' '  <<: *unsafe-services' \
  >"$FIXTURE_ROOT/docker-compose.services-merge.yml"
printf '%s\n' 'services:' '  minio:' '    "ima\u0067e": minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.escaped-image.yml"
printf '%s\n' 'services:' '  minio:' '    { image: minio/minio:latest }' \
  >"$FIXTURE_ROOT/docker-compose.inline-image.yml"
printf '%s\n' 'services:' '  ? minio' '  :' '    image: minio/minio:latest' \
  >"$FIXTURE_ROOT/docker-compose.explicit-minio.yml"
printf '%s\n' 'services:' '  minio:' "    image: $RUNTIME_REF" \
  >"$FIXTURE_ROOT/docker-compose.same-image.yml"
printf '%s\n' 'services:' '  minio:' '    pull_policy: missing' \
  >"$FIXTURE_ROOT/docker-compose.same-pull-policy.yml"
printf '%s\n' 'services:' '  minio:' '    image:' \
  >"$FIXTURE_ROOT/docker-compose.null-image.yml"
printf '%s\n' 'services:' '  minio:' '    pull_policy: null' \
  >"$FIXTURE_ROOT/docker-compose.null-pull-policy.yml"
printf '%s\n' 'services:' '  minio:' '    build: ~' \
  >"$FIXTURE_ROOT/docker-compose.null-build.yml"
# #3538: merge tags are tolerated only as the whole value of a `ports:` key.
printf '%s\n' 'services:' '  postgres:' '    ports: !reset []' \
  '  control-plane:' '    ports: !override' '      - "127.0.0.1:8080:8080"' \
  >"$FIXTURE_ROOT/docker-compose.ports-tags.yml"
printf '%s\n' 'services:' '  minio:' '    image: !reset null' \
  >"$FIXTURE_ROOT/docker-compose.tag-minio-image.yml"
printf '%s\n' 'services:' '  minio:' '    environment: !override' '      MINIO_ROOT_USER: x' \
  >"$FIXTURE_ROOT/docker-compose.tag-environment.yml"
printf '%s\n' 'services:' '  control-plane:' '    ports: !override {}' \
  >"$FIXTURE_ROOT/docker-compose.tag-ports-mapping.yml"
printf '%s\n' 'services:' '  control-plane:' '    ports: !reset [] !reset' \
  >"$FIXTURE_ROOT/docker-compose.tag-ports-trailing.yml"
printf '%s\n' 'services:' '  minio:' '    pull_policy: !reset' \
  >"$FIXTURE_ROOT/docker-compose.tag-pull-policy.yml"

# #1899: the only additional tag exception is this literal dependency block in
# the canonical fourth overlay. Snapshot data stays outside overlay discovery;
# every mutation is replayed at the same filename, isolating shape from naming.
BYO_GUARD_CANONICAL="$FIXTURE_ROOT/docker-compose.byo-s3.yml"
BYO_GUARD_APPROVED="$TMP/byo-s3.approved-input"
cat >"$BYO_GUARD_CANONICAL" <<'BYOYAML'
services:
  minio:
    profiles:
      - selfhost-bundled-minio-inactive
  control-plane:
    depends_on: !override
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
      nats:
        condition: service_healthy
    environment:
      STORAGE_BACKEND: "${STORAGE_BACKEND:?STORAGE_BACKEND must be set for BYO storage}"
      STORAGE_ENDPOINT: "${STORAGE_ENDPOINT:?STORAGE_ENDPOINT must be set for BYO storage}"
      STORAGE_REGION: "${STORAGE_REGION:-}"
      STORAGE_ACCESS_KEY: "${STORAGE_ACCESS_KEY:?STORAGE_ACCESS_KEY must be set for BYO storage}"
      STORAGE_SECRET_KEY: "${STORAGE_SECRET_KEY:?STORAGE_SECRET_KEY must be set for BYO storage}"
      STORAGE_USE_SSL: "${STORAGE_USE_SSL:?STORAGE_USE_SSL must be set for BYO storage}"
      STORAGE_BUCKET: "${STORAGE_BUCKET:?STORAGE_BUCKET must be set for BYO storage}"
BYOYAML
cp -- "$BYO_GUARD_CANONICAL" "$BYO_GUARD_APPROVED"
# Identical content at non-root paths must not acquire the root-only exception.
BYO_GUARD_ZAP="$FIXTURE_ROOT/.zap/docker-compose.byo-s3.yml"
BYO_GUARD_NESTED="$FIXTURE_ROOT/nested/docker-compose.byo-s3.yml"
mkdir -p "$FIXTURE_ROOT/nested"
cp -- "$BYO_GUARD_APPROVED" "$BYO_GUARD_ZAP"
cp -- "$BYO_GUARD_APPROVED" "$BYO_GUARD_NESTED"
# compose_minio_contract keeps the real base source but resolves env_file paths
# against its project directory. This private empty fixture replaces spa.env.
: >"$FIXTURE_ROOT/spa.env"
BYO_GUARD_CASES="$TMP/byo-guard-cases"
mkdir -p "$BYO_GUARD_CASES"
# Each fixture changes one dimension of the approved dependency exception, or
# adds one independent indirection beside it. All later guard calls use the real
# functions above; no stripper, parser or contract function is overridden.
for byo_case in off-service off-attribute wrong-tag inline-map inline-list inline-scalar \
  flow-map sequence missing-dependency extra-dependency unhealthy-dependency \
  extra-dependency-attribute short-dependency nested-tag trailing-anchor trailing-tag \
  sibling-anchor sibling-alias merge-key extends anchored-services explicit-key \
  tag-image tag-pull-policy tag-build tag-environment \
  empty-image empty-pull-policy empty-build null-image null-pull-policy null-build; do
  byo_input="$BYO_GUARD_CASES/$byo_case.input"
  case "$byo_case" in
    off-service) sed 's/^  control-plane:/  worker:/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    off-attribute) sed 's/^    depends_on:/    labels:/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    wrong-tag) sed 's/depends_on: !override/depends_on: !reset/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    inline-map|inline-list|inline-scalar|flow-map|sequence)
      case "$byo_case" in
        inline-map) byo_header='    depends_on: !override {}' ;;
        inline-list) byo_header='    depends_on: !override []' ;;
        inline-scalar) byo_header='    depends_on: !override postgres' ;;
        flow-map) byo_header='    depends_on: !override {postgres: {condition: service_healthy}, redis: {condition: service_healthy}, nats: {condition: service_healthy}}' ;;
        sequence) byo_header=$'    depends_on: !override\n      - postgres\n      - redis\n      - nats' ;;
      esac
      printf '%s\n' "$byo_header" >"$TMP/byo-header"
      awk -v header_file="$TMP/byo-header" '
        /^    depends_on:/ {
          while ((getline line < header_file) > 0) print line
          close(header_file)
          inside=1
          next
        }
        inside && /^    environment:/ { inside=0 }
        !inside { print }
      ' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    missing-dependency) sed '/^      nats:/{N;d;}' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    extra-dependency) awk '/^    environment:/ { print "      minio:"; print "        condition: service_healthy" } { print }' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    unhealthy-dependency) awk '!done && /condition: service_healthy/ { sub(/condition: service_healthy/, "condition: service_started"); done=1 } { print }' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    extra-dependency-attribute) awk '{ print; if ($0 == "      postgres:") print "        required: false" }' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    short-dependency) sed '/^      postgres:/{N;s/.*/      postgres: service_healthy/;}' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    nested-tag) awk '!done && /condition: service_healthy/ { sub(/condition: service_healthy/, "condition: !override service_healthy"); done=1 } { print }' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    trailing-anchor) sed 's/depends_on: !override/depends_on: !override \&dependencies/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    trailing-tag) sed 's/depends_on: !override/depends_on: !override !reset/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    sibling-anchor) sed 's/^    environment:/    environment: \&storage/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    sibling-alias)
      printf '%s\n' 'x-storage: &storage' '  STORAGE_REGION: synthetic-region' >"$byo_input"
      awk '/^    environment:/ {print "    environment: *storage"; exit} {print}' "$BYO_GUARD_APPROVED" >>"$byo_input" ;;
    merge-key)
      printf '%s\n' 'x-storage: &storage' '  SYNTHETIC_EXTRA: value' >"$byo_input"
      awk '{ print; if ($0 ~ /^    environment:/) print "      <<: *storage" }' "$BYO_GUARD_APPROVED" >>"$byo_input" ;;
    extends) awk '/^    profiles:/ { print "    extends:"; print "      service: unrelated-minio" } { print }' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    anchored-services) sed 's/^services:/services: \&services/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    explicit-key) sed 's/^    depends_on: !override/    ? depends_on\n    : !override/' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
    tag-image|tag-pull-policy|tag-build|tag-environment|empty-image|empty-pull-policy|empty-build|null-image|null-pull-policy|null-build)
      case "$byo_case" in
        tag-image) byo_recipe="    image: !override $RUNTIME_REF" ;;
        tag-pull-policy) byo_recipe='    pull_policy: !reset missing' ;;
        tag-build) byo_recipe='    build: !reset {}' ;;
        tag-environment) byo_recipe=$'    environment: !override\n      MINIO_BROWSER: off' ;;
        empty-*) byo_recipe_key="${byo_case#empty-}"; byo_recipe="    ${byo_recipe_key//-/_}:" ;;
        null-*) byo_recipe_key="${byo_case#null-}"; byo_recipe="    ${byo_recipe_key//-/_}: null" ;;
      esac
      printf '%s\n' "$byo_recipe" >"$TMP/byo-recipe"
      awk -v recipe_file="$TMP/byo-recipe" '
        { print }
        $0 == "  minio:" {
          while ((getline line < recipe_file) > 0) print line
          close(recipe_file)
        }
      ' "$BYO_GUARD_APPROVED" >"$byo_input" ;;
  esac
  # A generation failure or unchanged fixture cannot pass a negative assertion.
  if [[ ! -s "$byo_input" ]] || cmp -s "$byo_input" "$BYO_GUARD_APPROVED"; then
    fail "BYO guard mutation fixture must differ from accepted input: $byo_case"
  fi
done
# No tagged dependencies: this valid YAML alone must also pass the text guard,
# providing a reachable control independent of the future carve-out.
sed 's/depends_on: !override/depends_on:/' "$BYO_GUARD_APPROVED" \
  >"$BYO_GUARD_CASES/untagged-control.input"
# Retain direct tagged-build denial alongside the existing image/pull/env cases.
printf '%s\n' 'services:' '  minio:' '    build: !reset {}' \
  >"$FIXTURE_ROOT/docker-compose.tag-build.yml"

DISCOVERED_OVERLAYS="$TMP/discovered-overlays"
discover_compose_overlays "$FIXTURE_ROOT" >"$DISCOVERED_OVERLAYS"
assert_literal_count "$DISCOVERED_OVERLAYS" \
  "$FIXTURE_ROOT/docker-compose.valid.yml" 1 \
  "consumer guard discovers a newly added root Compose overlay"
assert_literal_count "$DISCOVERED_OVERLAYS" \
  "$FIXTURE_ROOT/.zap/docker-compose.flow.yml" 1 \
  "consumer guard discovers a newly added ZAP Compose overlay"
assert_literal_count "$DISCOVERED_OVERLAYS" \
  "$BYO_GUARD_ZAP" 1 \
  "consumer guard discovers a ZAP overlay with the canonical BYO basename"
assert_literal_count "$DISCOVERED_OVERLAYS" \
  "$FIXTURE_ROOT/docker-compose.canary.yaml" 1 \
  "consumer guard discovers a newly added root Compose YAML overlay"
assert_literal_count "$DISCOVERED_OVERLAYS" \
  "$FIXTURE_ROOT/.zap/docker-compose.scan.yaml" 1 \
  "consumer guard discovers a newly added ZAP Compose YAML overlay"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.four-space.yml" \
  "Docker Compose accepts a four-space-indented MinIO override"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.multiline-flow.yml" \
  "Docker Compose accepts a multiline flow-style MinIO override"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.quoted-services.yml" \
  "Docker Compose accepts a quoted block-style services key"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.services-anchor.yml" \
  "Docker Compose accepts an anchored services mapping"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.services-space.yml" \
  "Docker Compose accepts whitespace before the services colon"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.services-merge.yml" \
  "Docker Compose accepts a merged services mapping"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.escaped-image.yml" \
  "Docker Compose accepts an escaped MinIO image key"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.inline-image.yml" \
  "Docker Compose accepts an inline MinIO service mapping"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.explicit-minio.yml" \
  "Docker Compose accepts an explicit MinIO mapping key"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.extends.yml" \
  "Docker Compose accepts a canonical no-op MinIO extends"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.same-image.yml" \
  "Docker Compose accepts a repeated canonical MinIO image"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.same-pull-policy.yml" \
  "Docker Compose accepts a repeated canonical MinIO pull policy"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.alias-extends.yml" \
  "Docker Compose accepts an alias-valued no-op MinIO extends"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.null-image.yml" \
  "Docker Compose accepts a null MinIO image override"
assert_compose_accepts "$FIXTURE_ROOT/docker-compose.null-pull-policy.yml" \
  "Docker Compose accepts a null MinIO pull policy override"
assert_overlay_guard_accepts "$FIXTURE_ROOT/docker-compose.valid.yml" \
  "consumer guard accepts an overlay that leaves the MinIO runtime contract unchanged"
assert_overlay_guard_accepts "$FIXTURE_ROOT/docker-compose.depends-on.yml" \
  "consumer guard accepts nested MinIO dependency references"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.build.yml" \
  "consumer guard rejects a MinIO build override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.quoted-build.yml" \
  "consumer guard fails closed on a quoted or unusually indented MinIO build override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.merge.yml" \
  "consumer guard rejects a MinIO YAML merge"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.extends.yml" \
  "consumer guard rejects MinIO extends"
assert_overlay_guard_rejects "$FIXTURE_ROOT/.zap/docker-compose.flow.yml" \
  "consumer guard fails closed on an unsupported MinIO service declaration"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.four-space.yml" \
  "consumer guard rejects a four-space-indented MinIO image override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.multiline-flow.yml" \
  "consumer guard fails closed on a multiline flow-style MinIO override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.quoted-services.yml" \
  "consumer guard rejects an override beneath a quoted services key"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.services-anchor.yml" \
  "consumer guard rejects an override beneath an anchored services mapping"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.services-space.yml" \
  "consumer guard rejects an override beneath a spaced services key"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.services-merge.yml" \
  "consumer guard rejects a merged MinIO service override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.escaped-image.yml" \
  "consumer guard rejects an escaped MinIO image override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.inline-image.yml" \
  "consumer guard rejects an inline MinIO image override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.explicit-minio.yml" \
  "consumer guard rejects an explicit MinIO service key"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.same-image.yml" \
  "consumer guard rejects a repeated canonical MinIO image"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.same-pull-policy.yml" \
  "consumer guard rejects a repeated canonical MinIO pull policy"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.alias-extends.yml" \
  "consumer guard rejects an alias-valued MinIO extends"
if overlay_uses_forbidden_indirection \
    "$FIXTURE_ROOT/docker-compose.slash-alias-extends.yml"; then
  pass "consumer guard detects a slash-prefixed YAML anchor or alias"
else
  fail "consumer guard detects a slash-prefixed YAML anchor or alias"
fi
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.slash-alias-extends.yml" \
  "consumer guard rejects a slash-prefixed alias-valued MinIO extends"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.null-image.yml" \
  "consumer guard rejects a null MinIO image override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.null-pull-policy.yml" \
  "consumer guard rejects a null MinIO pull policy override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.null-build.yml" \
  "consumer guard rejects a null MinIO build override"
assert_overlay_guard_accepts "$FIXTURE_ROOT/docker-compose.ports-tags.yml" \
  "consumer guard accepts ports: !reset [] and ports: !override on non-MinIO services"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.tag-minio-image.yml" \
  "consumer guard rejects image: !reset null on MinIO"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.tag-environment.yml" \
  "consumer guard rejects environment: !override"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.tag-ports-mapping.yml" \
  "consumer guard rejects ports: !override with an inline mapping"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.tag-ports-trailing.yml" \
  "consumer guard rejects ports: !reset [] followed by a second tag"
assert_overlay_guard_rejects "$FIXTURE_ROOT/docker-compose.tag-pull-policy.yml" \
  "consumer guard rejects a tag on a non-ports key under MinIO"

# Permit only the reviewed media profile/extension shapes. A tag on MinIO or
# an extra extension attribute must still fail before semantic comparison.
MEDIA_GUARD_ALLOWED="$TMP/media-guard-allowed.yml"
printf '%s\n' 'services:' '  control-plane:' \
  '    profiles: !override [services, media-bluegreen]' '  media-plane:' \
  '    profiles: !override [services, media-single]' '  media-plane-blue:' \
  '    extends:' '      service: media-plane' \
  '    profiles: !override [media-bluegreen]' \
  >"$MEDIA_GUARD_ALLOWED"
if overlay_uses_forbidden_indirection "$MEDIA_GUARD_ALLOWED"; then
  fail "text guard accepts only the reviewed media profile and extension shapes"
else
  pass "text guard accepts only the reviewed media profile and extension shapes"
fi
awk '{ print; if ($0 == "    extends:") print "      file: ./docker-compose.yml" }' \
  "$MEDIA_GUARD_ALLOWED" \
  >"$TMP/media-guard-staging.yml"
if overlay_uses_forbidden_indirection "$TMP/media-guard-staging.yml"; then
  fail "text guard accepts only the exact staging media base extension"
else
  pass "text guard accepts only the exact staging media base extension"
fi
sed 's@file: ./docker-compose.yml@file: unexpected.yml@' \
  "$TMP/media-guard-staging.yml" >"$TMP/media-guard-wrong-base.yml"
if overlay_uses_forbidden_indirection "$TMP/media-guard-wrong-base.yml"; then
  pass "text guard rejects an unexpected staging media base extension"
else
  fail "text guard rejects an unexpected staging media base extension"
fi
awk '{ print; if ($0 == "      service: media-plane") print "      file: unexpected.yml" }' \
  "$MEDIA_GUARD_ALLOWED" >"$TMP/media-guard-extra.yml"
if overlay_uses_forbidden_indirection "$TMP/media-guard-extra.yml"; then
  pass "text guard rejects an extra media extension attribute"
else
  fail "text guard rejects an extra media extension attribute"
fi
printf '%s\n' 'services:' '  control-plane:' \
  '    profiles: !override [services, media-bluegreen, rogue]' \
  >"$TMP/media-guard-extra-profile.yml"
if overlay_uses_forbidden_indirection "$TMP/media-guard-extra-profile.yml"; then
  pass "text guard rejects an extra managed profile"
else
  fail "text guard rejects an extra managed profile"
fi
printf '%s\n' 'services:' '  minio:' '    profiles: !override [services]' \
  >"$TMP/minio-profile-tag.yml"
if overlay_uses_forbidden_indirection "$TMP/minio-profile-tag.yml"; then
  pass "text guard rejects a MinIO profile tag"
else
  fail "text guard rejects a MinIO profile tag"
fi

# Pure-text checks: no Compose needed, so they run on every machine.
if overlay_uses_forbidden_indirection "$FIXTURE_ROOT/docker-compose.ports-tags.yml"; then
  fail "text guard tolerates ports-only merge tags"
else
  pass "text guard tolerates ports-only merge tags"
fi
for rejected in tag-minio-image tag-environment tag-ports-mapping tag-ports-trailing tag-pull-policy tag-build; do
  if overlay_uses_forbidden_indirection "$FIXTURE_ROOT/docker-compose.$rejected.yml"; then
    pass "text guard still flags $rejected as indirection"
  else
    fail "text guard still flags $rejected as indirection"
  fi
done
# Bind the guard's real ROOT interface privately for fixture calls. COMPOSE still
# points to the original shared source; later production discovery sees real ROOT.
assert_byo_guard_fixtures() {
  local ROOT="$FIXTURE_ROOT"
  # #1899 dependency-tag scope is checked at the text boundary even without
  # Compose. Render failure from absent STORAGE_* cannot satisfy these negatives.
  if overlay_uses_forbidden_indirection "$BYO_GUARD_CASES/untagged-control.input"; then
    fail "BYO guard accepts an untagged exact dependency-map control"
  else
    pass "BYO guard accepts an untagged exact dependency-map control"
  fi
  if overlay_uses_forbidden_indirection "$BYO_GUARD_CANONICAL"; then
    fail "BYO guard accepts the canonical fourth-overlay exact dependency override"
  else
    pass "BYO guard accepts the canonical fourth-overlay exact dependency override"
  fi
  # Path is varied separately from every structural mutation below. Both
  # same-basename negatives must retain the exact accepted content.
  if cmp -s "$BYO_GUARD_ZAP" "$BYO_GUARD_APPROVED" &&
      cmp -s "$BYO_GUARD_NESTED" "$BYO_GUARD_APPROVED"; then
    pass "non-root BYO same-basename fixtures equal the canonical approved input"
  else
    fail "non-root BYO same-basename fixtures equal the canonical approved input"
  fi
  if overlay_uses_forbidden_indirection "$BYO_GUARD_ZAP"; then
    pass "BYO dependency exception rejects a .zap same-basename exact block"
  else
    fail "BYO dependency exception rejects a .zap same-basename exact block"
  fi
  if overlay_uses_forbidden_indirection "$BYO_GUARD_NESTED"; then
    pass "BYO dependency exception rejects an arbitrary nested same-basename exact block"
  else
    fail "BYO dependency exception rejects an arbitrary nested same-basename exact block"
  fi
  cp -- "$BYO_GUARD_APPROVED" "$FIXTURE_ROOT/docker-compose.byo-other.yml"
  if overlay_uses_forbidden_indirection "$FIXTURE_ROOT/docker-compose.byo-other.yml"; then
    pass "BYO dependency exception rejects an off-filename exact block"
  else
    fail "BYO dependency exception rejects an off-filename exact block"
  fi
  for byo_input in "$BYO_GUARD_CASES"/*.input; do
    byo_case="${byo_input##*/}"; byo_case="${byo_case%.input}"
    [[ "$byo_case" != untagged-control ]] || continue
    [[ -s "$byo_input" ]] && ! cmp -s "$byo_input" "$BYO_GUARD_APPROVED" || continue
    cp -- "$byo_input" "$BYO_GUARD_CANONICAL"
    if overlay_uses_forbidden_indirection "$BYO_GUARD_CANONICAL"; then
      pass "BYO dependency exception rejects $byo_case at the canonical filename"
    else
      fail "BYO dependency exception rejects $byo_case at the canonical filename"
    fi
  done
  cp -- "$BYO_GUARD_APPROVED" "$BYO_GUARD_CANONICAL"
  assert_overlay_guard_accepts "$BYO_GUARD_CANONICAL" \
    "consumer guard accepts canonical BYO while retaining dormant MinIO's normal and probe contract"
  # Keep these separate from the lexical carve-out: skipping an inactive MinIO,
  # dropping the all-profile render or deleting either semantic comparison must fail.
  if [[ "$COMPOSE_SEMANTICS_AVAILABLE" -eq 1 ]]; then
    if byo_normal_contract="$(compose_minio_contract "$BYO_GUARD_CANONICAL")" &&
        [[ "$byo_normal_contract" == "$CANONICAL_MINIO_CONTRACT" ]]; then
      pass "canonical BYO all-profile render preserves MinIO image, pull policy and build"
    else
      fail "canonical BYO all-profile render preserves MinIO image, pull policy and build"
    fi
    if byo_probe_contract="$(compose_minio_contract "$BYO_GUARD_CANONICAL" "$MINIO_CONTRACT_PROBE")" &&
        [[ "$byo_probe_contract" == "$PROBED_MINIO_CONTRACT" ]]; then
      pass "canonical BYO all-profile probe preserves MinIO image, pull policy and build"
    else
      fail "canonical BYO all-profile probe preserves MinIO image, pull policy and build"
    fi
  fi
}
assert_byo_guard_fixtures

TAGS_IN="$TMP/strip-tags.in"
TAGS_WANT="$TMP/strip-tags.want"
printf '%s\n' '    ports: !reset []' '    ports: !override' '    ports:   !override   ' \
  '    ports: !override {}' '    ports: !reset [] !reset' '    pull_policy: !reset' \
  '    ports: ["80:80"]' '      - "127.0.0.1:8080:8080"' >"$TAGS_IN"
printf '%s\n' '    ports:  []' '    ports: ' '    ports:   ' \
  '    ports: !override {}' '    ports: !reset [] !reset' '    pull_policy: !reset' \
  '    ports: ["80:80"]' '      - "127.0.0.1:8080:8080"' >"$TAGS_WANT"
strip_ports_merge_tags "$TAGS_IN"
if cmp -s "$TAGS_IN" "$TAGS_WANT"; then
  pass "strip_ports_merge_tags strips only a whole-value ports tag"
else
  fail "strip_ports_merge_tags strips only a whole-value ports tag"
fi

assert_file "$COMPOSE" "shared MinIO consumer Compose file exists"
assert_file "$DOCKERFILE" "MinIO source-build Dockerfile exists"
assert_file "$WORKFLOW" "MinIO publisher workflow exists"
assert_file "$PR_CI" "PR CI workflow exists"
assert_file "$SCAN_RELEVANCE" "shared PR relevance policy exists"
assert_file "$PRECOMMIT" "pre-commit configuration exists"

if [[ -f "$PR_CI" && -f "$SCAN_RELEVANCE" ]]; then
  WORKFLOW_FILTER="$TMP/scan-relevance.workflow-filter"
  awk '
    $0 == "workflow_lint:" { inside=1; found=1; next }
    inside && $0 ~ /^[a-z_][a-z0-9_]*:/ { exit }
    inside { print }
    END { if (!found) exit 2 }
  ' "$SCAN_RELEVANCE" >"$WORKFLOW_FILTER"
  # Scoped to the extracted filter, like its siblings: a whole-file count broke
  # when another filter legitimately listed the same path; it must stay under
  # workflow_lint in the shared map.
  assert_literal_count "$WORKFLOW_FILTER" "  - '.pre-commit-config.yaml'" 1 \
    "PR workflow-lint filter includes pre-commit configuration changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - 'docker-compose*.yml'" 1 \
    "PR workflow-lint runs the MinIO contract on shared consumer and root overlay changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - '.zap/docker-compose*.yml'" 1 \
    "PR workflow-lint runs the MinIO contract on ZAP overlay changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - 'docker-compose*.yaml'" 1 \
    "PR workflow-lint runs the MinIO contract on root Compose YAML changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - '.zap/docker-compose*.yaml'" 1 \
    "PR workflow-lint runs the MinIO contract on ZAP Compose YAML changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - 'infrastructure/docker/minio/dependencies.patch'" 1 \
    "PR workflow-lint runs the MinIO contract when the dependency patch changes"
  assert_literal_count "$WORKFLOW_FILTER" "  - 'infrastructure/docker/minio/minio.openvex.json'" 1 \
    "PR workflow-lint runs the MinIO contract when the OpenVEX document changes"
  assert_literal_count "$PR_CI" \
    'bash scripts/tests/test-publish-minio-image.sh --require-compose' 1 \
    "PR CI requires Compose-semantic MinIO validation"
fi

if [[ -f "$PRECOMMIT" ]]; then
  PUBLISH_HOOK="$TMP/precommit.publish-minio"
  TAG_GUARD_HOOK="$TMP/precommit.check-oci-tag"
  awk '
    $0 ~ /^[[:space:]]*- id:[[:space:]]+test-publish-minio-image[[:space:]]*$/ { inside=1 }
    inside && seen && $0 ~ /^[[:space:]]*- id:/ { exit }
    inside { print; seen=1 }
  ' "$PRECOMMIT" >"$PUBLISH_HOOK"
  awk '
    $0 ~ /^[[:space:]]*- id:[[:space:]]+test-check-oci-tag[[:space:]]*$/ { inside=1 }
    inside && seen && $0 ~ /^[[:space:]]*- id:/ { exit }
    inside { print; seen=1 }
  ' "$PRECOMMIT" >"$TAG_GUARD_HOOK"
  assert_literal_count "$PUBLISH_HOOK" '\.pre-commit-config\.yaml' 1 \
    "publisher pre-commit hook runs when its configuration changes"
  assert_literal_count "$PUBLISH_HOOK" 'dependencies\.patch' 1 \
    "publisher pre-commit hook runs when the dependency patch changes"
  assert_literal_count "$PUBLISH_HOOK" 'minio\.openvex\.json' 1 \
    "publisher pre-commit hook runs when the OpenVEX document changes"
  assert_literal_count "$PUBLISH_HOOK" '(?:\.zap/)?docker-compose(?:\.[A-Za-z0-9_-]+)*\.ya?ml' 1 \
    "publisher pre-commit hook runs on shared MinIO consumer and overlay changes"
  assert_literal_count "$TAG_GUARD_HOOK" '\.pre-commit-config\.yaml' 1 \
    "OCI tag-guard pre-commit hook runs when its configuration changes"
  # Substring presence does not prove the regex matches the file: match it for real.
  # The pattern uses (?:...), so PCRE through perl (macOS grep has no -P).
  PUBLISH_HOOK_FILES="$(sed -n -E "s/^[[:space:]]*files:[[:space:]]*'(.*)'[[:space:]]*$/\1/p" "$PUBLISH_HOOK" | first_line)"
  hook_matches() { perl -e 'exit(($ARGV[1] =~ /$ARGV[0]/) ? 0 : 1)' "$1" "$2"; }
  assert_not_equals "$PUBLISH_HOOK_FILES" "" "publisher pre-commit hook declares a files: filter"
  hook_matches "$PUBLISH_HOOK_FILES" 'infrastructure/docker/minio/minio.openvex.json'
  assert_equals "$?" 0 "publisher pre-commit files: filter matches the OpenVEX document"
  hook_matches "$PUBLISH_HOOK_FILES" 'infrastructure/docker/minio/minio.openvex.json.bak'
  assert_not_equals "$?" 0 "publisher pre-commit files: filter does not match a .bak copy of the OpenVEX document"
fi

if [[ -f "$COMPOSE" ]]; then
  ACTIVE_COMPOSE="$TMP/compose.active"
  MINIO_SERVICE="$TMP/compose.minio"
  strip_comments "$COMPOSE" "$ACTIVE_COMPOSE"
  extract_minio_service "$ACTIVE_COMPOSE" "$MINIO_SERVICE"

  assert_count "$MINIO_SERVICE" '^    image:' 1 \
    "shared MinIO service has exactly one runtime image"
  minio_image="$(awk '$1 == "image:" { print $2 }' "$MINIO_SERVICE")"
  assert_equals "$minio_image" "$RUNTIME_REF" \
    "shared MinIO service consumes the exact Concord runtime digest"
  assert_literal_count "$MINIO_SERVICE" '    pull_policy: missing' 1 \
    "shared MinIO service pulls the reviewed digest only when absent"
  assert_not_contains "$MINIO_SERVICE" 'minio/minio' \
    "shared MinIO service has no Docker Hub fallback"
  assert_not_contains "$MINIO_SERVICE" \
    "^[[:space:]]+[\"']?(build|extends|<<)[\"']?[[:space:]]*:" \
    "shared MinIO service cannot build or inherit a runtime override"
fi

# The production overlay exception admits only its exact reviewed anchor and
# tags. Mutate the real overlay to prove each widening is still refused.
PROD_OVERLAY="$ROOT/docker-compose.production.yml"
for mutation in anchor-image minio-tag other-alias ops-agent-profile; do
  mutant_dir="$TMP/production-$mutation"
  mkdir -p "$mutant_dir"
  case "$mutation" in
    anchor-image)
      sed 's|^x-hardening: &hardening$|&\n  image: example.invalid/minio:rogue|' "$PROD_OVERLAY" ;;
    minio-tag)
      sed '/^  minio:$/a\    image: !reset null' "$PROD_OVERLAY" ;;
    other-alias)
      sed '0,/^    <<: \*hardening$/s//    <<: *rogue/' "$PROD_OVERLAY" ;;
    ops-agent-profile)
      sed 's/\[selfhost-managed-ops-inactive\]/[selfhost-managed-ops-inactive, rogue]/' "$PROD_OVERLAY" ;;
  esac >"$mutant_dir/docker-compose.production.yml"
  assert_overlay_guard_rejects "$mutant_dir/docker-compose.production.yml" \
    "production overlay guard rejects $mutation"
done

while IFS= read -r overlay; do
  overlay_name="${overlay#"$ROOT/"}"
  assert_file "$overlay" "$overlay_name exists"
  if [[ ! -f "$overlay" ]]; then
    continue
  fi

  if [[ "$COMPOSE_SEMANTICS_AVAILABLE" -ne 1 ]]; then
    continue
  fi
  if minio_overlay_is_safe "$overlay"; then
    pass "$overlay_name cannot override the canonical MinIO runtime contract"
  else
    fail "$overlay_name cannot override the canonical MinIO runtime contract"
  fi
done < <(discover_compose_overlays "$ROOT")

if [[ -f "$DOCKERFILE" ]]; then
  ACTIVE_DOCKERFILE="$TMP/Dockerfile.active"
  LOGICAL_DOCKERFILE="$TMP/Dockerfile.logical"
  BUILDER_STAGE="$TMP/Dockerfile.builder"
  RUNTIME_STAGE="$TMP/Dockerfile.runtime"
  strip_comments "$DOCKERFILE" "$ACTIVE_DOCKERFILE"
  join_continuations "$ACTIVE_DOCKERFILE" "$LOGICAL_DOCKERFILE"
  stage_count="$(awk 'toupper($0) ~ /^[[:space:]]*FROM[[:space:]]/ { count++ } END { print count+0 }' "$LOGICAL_DOCKERFILE")"
  assert_equals "$stage_count" 2 "Dockerfile has exactly builder and runtime stages"
  awk '
    toupper($0) ~ /^[[:space:]]*FROM[[:space:]]/ { stage++ }
    stage == 1 { print }
  ' "$LOGICAL_DOCKERFILE" >"$BUILDER_STAGE"
  awk '
    toupper($0) ~ /^[[:space:]]*FROM[[:space:]]/ { stage++ }
    stage == 2 { print }
  ' "$LOGICAL_DOCKERFILE" >"$RUNTIME_STAGE"

  assert_contains "$BUILDER_STAGE" "^FROM[[:space:]]+--platform=\\\$BUILDPLATFORM[[:space:]]+golang:1\\.26\\.10-bookworm@sha256:${BUILDER_DIGEST}[[:space:]]+AS[[:space:]]+builder$" \
    "builder stage runs natively and is named and digest-pinned"
  assert_contains "$RUNTIME_STAGE" "^FROM[[:space:]]+registry\\.access\\.redhat\\.com/ubi9/ubi-micro:9\\.6@sha256:${RUNTIME_BASE_DIGEST}[[:space:]]+AS[[:space:]]+runtime$" \
    "effective runtime stage is named and digest-pinned"
  builder_instructions="$(awk '/^[A-Za-z]+([[:space:]]|$)/ { print toupper($1) }' "$BUILDER_STAGE")"
  assert_equals "$builder_instructions" $'FROM\nARG\nARG\nARG\nARG\nARG\nWORKDIR\nCOPY\nENV\nRUN' \
    "builder instruction set is closed and ordered"
  runtime_instructions="$(awk '/^[A-Za-z]+([[:space:]]|$)/ { print toupper($1) }' "$RUNTIME_STAGE")"
  assert_equals "$runtime_instructions" $'FROM\nARG\nARG\nARG\nCOPY\nCOPY\nCOPY\nCOPY\nCOPY\nLABEL\nLABEL\nLABEL\nLABEL\nLABEL\nLABEL\nLABEL\nLABEL\nLABEL\nUSER\nEXPOSE\nVOLUME\nSTOPSIGNAL\nENTRYPOINT\nCMD' \
    "runtime instruction set is closed and ordered"
  assert_contains "$BUILDER_STAGE" '^ARG[[:space:]]+TARGETOS=linux$' "builder fixes default target OS"
  assert_contains "$BUILDER_STAGE" '^ARG[[:space:]]+TARGETARCH$' "builder consumes Buildx target architecture"
  assert_contains "$BUILDER_STAGE" '^ARG[[:space:]]+DERIVATIVE_COMMIT$' "builder receives the reviewed derivative commit"
  assert_contains "$BUILDER_STAGE" '^ARG[[:space:]]+UPSTREAM_COMMIT$' "builder receives the explicit upstream origin commit"
  assert_contains "$BUILDER_STAGE" '^WORKDIR[[:space:]]+/src$' "builder works only in copied source tree"
  assert_contains "$BUILDER_STAGE" '^COPY[[:space:]]+\.[[:space:]]+\.$' "builder copies the verified context once"
  assert_contains "$BUILDER_STAGE" '^ENV[[:space:]]+SOURCE_DATE_EPOCH=1760549395$' \
    "build fixes SOURCE_DATE_EPOCH"
  assert_count "$BUILDER_STAGE" '^RUN[[:space:]]' 1 "builder has exactly one RUN instruction"
  build_run="$(grep -E '^RUN[[:space:]]' "$BUILDER_STAGE")"
  build_segments="$(awk -F ' && ' '{ print NF }' <<<"$build_run")"
  assert_equals "$build_segments" 5 "builder RUN contains required args, toolchain check, output mkdir, linker assignment, and build"
  assert_contains "$BUILDER_STAGE" '^RUN : "\$\{DERIVATIVE_COMMIT:\?\}" "\$\{DERIVATIVE_TAG:\?\}" "\$\{UPSTREAM_COMMIT:\?\}" &&[[:space:]]+test "\$\(go env GOVERSION\)" = "go1\.26\.10" &&[[:space:]]+mkdir -p /out &&[[:space:]]+LDFLAGS="-s -w .+" &&[[:space:]]+GOTOOLCHAIN=local CGO_ENABLED=0 GOOS="\$TARGETOS" GOARCH="\$TARGETARCH" go build -mod=readonly -buildvcs=false -tags=kqueue -trimpath -ldflags "\$LDFLAGS" -o /out/minio \.$' \
    "builder RUN is the closed deterministic compile pipeline"
  assert_contains "$BUILDER_STAGE" 'ReleaseTag=\$DERIVATIVE_TAG' \
    "binary identity uses the derivative release tag"
  assert_contains "$BUILDER_STAGE" 'CommitID=\$DERIVATIVE_COMMIT' \
    "binary identity uses the derivative commit"
  assert_not_contains "$BUILDER_STAGE" "CommitID=$SOURCE_COMMIT" \
    "binary identity does not misrepresent derivative source as pristine upstream"
  assert_contains "$WORKFLOW" 'DERIVATIVE_COMMIT' \
    "publisher supplies the reviewed derivative commit"
  assert_contains "$WORKFLOW" 'UPSTREAM_COMMIT' \
    "publisher supplies upstream origin separately"

  for contract in \
    'GOTOOLCHAIN=local' \
    'CGO_ENABLED=0' \
    'GOOS=.*TARGETOS' \
    'GOARCH=.*TARGETARCH' \
    '(^|[[:space:]])-mod=readonly([[:space:]]|$)' \
    '(^|[[:space:]])-buildvcs=false([[:space:]]|$)' \
    '(^|[[:space:]])-tags(=|[[:space:]])kqueue([[:space:]]|$)' \
    '(^|[[:space:]])-trimpath([[:space:]]|$)'; do
    assert_contains "$BUILDER_STAGE" "$contract" "build contract is active: $contract"
  done

  for symbol in \
    'Version=2025-10-15T17:29:55Z' \
    'CopyrightYear=2025' \
    'GOPATH=/go' \
    'GOROOT=/usr/local/go'; do
    assert_contains "$BUILDER_STAGE" "github\\.com/minio/minio/cmd\\.$symbol" \
      "linker fixes $symbol"
  done
  assert_contains "$BUILDER_STAGE" 'ReleaseTag=\$DERIVATIVE_TAG' "linker fixes the derivative release tag"

  assert_contains "$RUNTIME_STAGE" '^COPY[[:space:]]+--from=builder[[:space:]]+/out/minio[[:space:]]+/usr/bin/minio$' \
    "runtime copies only the source-built server binary"
  assert_contains "$RUNTIME_STAGE" '^COPY[[:space:]]+--from=builder[[:space:]]+/etc/ssl/certs/ca-certificates\.crt[[:space:]]+/etc/ssl/certs/ca-certificates\.crt$' \
    "runtime copies the builder CA trust bundle directly"
  assert_contains "$RUNTIME_STAGE" '^COPY[[:space:]]+--from=builder[[:space:]]+/src/LICENSE[[:space:]]+/licenses/LICENSE$' \
    "runtime copies upstream LICENSE directly"
  assert_contains "$RUNTIME_STAGE" '^COPY[[:space:]]+--from=builder[[:space:]]+/src/NOTICE[[:space:]]+/licenses/NOTICE$' \
    "runtime copies upstream NOTICE directly"
  assert_contains "$RUNTIME_STAGE" '^COPY[[:space:]]+--from=builder[[:space:]]+/src/CREDITS[[:space:]]+/licenses/CREDITS$' \
    "runtime copies upstream CREDITS directly"
  assert_not_contains "$RUNTIME_STAGE" '^RUN[[:space:]]' \
    "final runtime stage cannot modify copied artifacts"

  for label in \
    'org.opencontainers.image.title="MinIO"' \
    'org.opencontainers.image.source="https://github.com/minio/minio"' \
    'org.opencontainers.image.version="\$DERIVATIVE_TAG"' \
    'org.opencontainers.image.revision="\$DERIVATIVE_COMMIT"' \
    'org.opencontainers.image.licenses="AGPL-3.0-or-later"' \
    'org.opencontainers.image.created="2025-10-15T17:29:55Z"' \
    'org.opencontainers.image.documentation="https://github.com/Concord-Voice/Concord-Voice/blob/main/infrastructure/docker/minio/SOURCE-BUILD.md"' \
    'io.concordvoice.minio.upstream-tag="RELEASE.2025-10-15T17-29-55Z"'; do
    assert_contains "$RUNTIME_STAGE" "^LABEL[[:space:]]+$label$" \
      "runtime label is fixed: $label"
  done
  assert_contains "$RUNTIME_STAGE" '^LABEL[[:space:]]+io\.concordvoice\.minio\.upstream-commit="\$UPSTREAM_COMMIT"$' \
    "runtime labels the upstream origin commit separately"

  assert_equals "$(awk 'toupper($1)=="USER" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'USER 0' "effective runtime user is root"
  assert_equals "$(awk 'toupper($1)=="ENTRYPOINT" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'ENTRYPOINT ["/usr/bin/minio"]' "effective entrypoint directly execs MinIO"
  assert_equals "$(awk 'toupper($1)=="CMD" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'CMD ["server", "/data", "--console-address", ":9001"]' "effective default command starts the server"
  assert_equals "$(awk 'toupper($1)=="EXPOSE" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'EXPOSE 9000 9001' "effective exposed ports are exact"
  assert_equals "$(awk 'toupper($1)=="VOLUME" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'VOLUME ["/data"]' "effective persistent volume is /data"
  assert_equals "$(awk 'toupper($1)=="STOPSIGNAL" { line=$0 } END { print line }' "$RUNTIME_STAGE")" \
    'STOPSIGNAL SIGTERM' "effective stop signal is SIGTERM"

  assert_not_contains "$RUNTIME_STAGE" '(^|[[:space:]/"])(mc|curl)([[:space:]/"]|$)' \
    "runtime does not bundle mc or curl"
  assert_not_contains "$ACTIVE_DOCKERFILE" '(^|[^[:alnum:]_.-])latest([^[:alnum:]_.-]|$)' \
    "Dockerfile has no floating latest reference"
  assert_not_contains "$ACTIVE_DOCKERFILE" '(https?://(dl\.)?min\.io|wget[[:space:]]|curl[[:space:]])' \
    "Dockerfile does not download an upstream binary"
  assert_not_contains "$ACTIVE_DOCKERFILE" 'chmod[[:space:]]+(-R[[:space:]]+)?0?777' \
    "Dockerfile does not create world-writable paths"
  assert_not_contains "$ACTIVE_DOCKERFILE" '(docker-entrypoint|chroot)' "Dockerfile has no wrapper or chroot path"
fi

if [[ -f "$WORKFLOW" ]]; then
  ACTIVE_WORKFLOW="$TMP/workflow.active"
  TRIM_WORKFLOW="$TMP/workflow.trim"
  ON_BLOCK="$TMP/workflow.on"
  CONCURRENCY_BLOCK="$TMP/workflow.concurrency"
  TOP_PERMISSIONS="$TMP/workflow.permissions"
  TOP_ENV="$TMP/workflow.env"
  JOBS_BLOCK="$TMP/workflow.jobs"
  PUBLISH_JOB="$TMP/workflow.publish"
  PUBLISH_LINES="$TMP/workflow.publish.trim"
  PUBLISH_LOGICAL="$TMP/workflow.publish.logical"
  PUBLISH_PERMISSIONS="$TMP/workflow.publish.permissions"
  SOURCE_VERIFY_BLOCK="$TMP/workflow.source-verify.sh"
  SOURCE_PREPARE_BLOCK="$TMP/workflow.source-prepare.sh"
  AMD_METADATA_BLOCK="$TMP/workflow.amd-metadata.sh"
  ARM_METADATA_BLOCK="$TMP/workflow.arm-metadata.sh"
  ARM_JOB="$TMP/workflow.arm"
  ARM_LINES="$TMP/workflow.arm.trim"
  ARM_LOGICAL="$TMP/workflow.arm.logical"
  ARM_PERMISSIONS="$TMP/workflow.arm.permissions"
  FINALIZE_JOB="$TMP/workflow.finalize"
  FINALIZE_LINES="$TMP/workflow.finalize.trim"
  FINALIZE_LOGICAL="$TMP/workflow.finalize.logical"
  FINALIZE_PERMISSIONS="$TMP/workflow.finalize.permissions"
  strip_comments "$WORKFLOW" "$ACTIVE_WORKFLOW"
  normalize_lines "$ACTIVE_WORKFLOW" "$TRIM_WORKFLOW"
  tr '\n' ' ' <"$ACTIVE_WORKFLOW" >"$TMP/workflow.flat"

  awk '$0=="on:" { inside=1; next } inside && /^[^[:space:]]/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$ON_BLOCK"
  on_events="$(awk '/^  [A-Za-z0-9_-]+:/ { key=$1; sub(/:$/, "", key); print key }' "$ON_BLOCK")"
  on_inputs="$(awk '/^      [A-Za-z0-9_-]+:/ { key=$1; sub(/:$/, "", key); print key }' "$ON_BLOCK")"
  assert_equals "$on_events" 'workflow_dispatch' "workflow_dispatch is the only trigger"
  assert_equals "$on_inputs" 'reason' "reason is the only dispatcher input"
  assert_contains "$ON_BLOCK" '^        description:[[:space:]]+.*already-public package.*pre-dispatch legal approval' \
    "refresh reason requires the legal-approval record for public packages"

  awk '$0=="concurrency:" { inside=1; next } inside && /^[^[:space:]]/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$CONCURRENCY_BLOCK"
  concurrency_contract="$(awk '/^  [A-Za-z0-9_-]+:/ { $1=$1; print }' "$CONCURRENCY_BLOCK")"
  assert_equals "$concurrency_contract" $'group: publish-minio-image\ncancel-in-progress: false' \
    "publisher concurrency is exact and non-cancelling"

  awk '$0=="permissions:" { inside=1; next } inside && /^[^[:space:]]/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$TOP_PERMISSIONS"
  top_permissions="$(awk '/^  [A-Za-z0-9_-]+:/ { $1=$1; print }' "$TOP_PERMISSIONS")"
  assert_equals "$top_permissions" 'contents: read' "workflow permissions are exactly contents read"
  assert_count "$ACTIVE_WORKFLOW" '^[[:space:]]*permissions:[[:space:]]*$' 4 \
    "workflow has exactly top-level and three job permission blocks"

  awk '$0=="env:" { inside=1; next } inside && /^[^[:space:]]/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$TOP_ENV"
  env_contract="$(awk '/^  [A-Za-z0-9_-]+:/ { $1=$1; print }' "$TOP_ENV" | sort)"
  minio_derivative_commit="$(awk '$1=="MINIO_DERIVATIVE_COMMIT:" { print $2 }' "$TOP_ENV")"
  minio_derivative_tree="$(awk '$1=="MINIO_DERIVATIVE_TREE:" { print $2 }' "$TOP_ENV")"
  minio_derivative_tag="$(awk '$1=="MINIO_DERIVATIVE_TAG:" { print $2 }' "$TOP_ENV")"
  minio_patch_sha="$(awk '$1=="MINIO_PATCH_SHA256:" { print $2 }' "$TOP_ENV")"
  minio_go_mod_sha="$(awk '$1=="MINIO_GO_MOD_SHA256:" { print $2 }' "$TOP_ENV")"
  minio_go_sum_sha="$(awk '$1=="MINIO_GO_SUM_SHA256:" { print $2 }' "$TOP_ENV")"
  minio_source_sha="$(awk '$1=="MINIO_SOURCE_SHA256:" { print $2 }' "$TOP_ENV")"
  minio_runtime_image="$(awk '$1=="RUNTIME_IMAGE:" { print $2 }' "$TOP_ENV")"
  minio_source_image="$(awk '$1=="SOURCE_IMAGE:" { print $2 }' "$TOP_ENV")"
  expected_env="$(printf '%s\n' \
    "BUILDX_LINUX_AMD64_SHA256: $BUILDX_LINUX_AMD64_SHA256" \
    "BUILDX_VERSION: $BUILDX_VERSION" \
    "MINIO_DERIVATIVE_COMMIT: $minio_derivative_commit" \
    "MINIO_DERIVATIVE_TAG: $minio_derivative_tag" \
    "MINIO_DERIVATIVE_TREE: $minio_derivative_tree" \
    "MINIO_GO_MOD_SHA256: $minio_go_mod_sha" \
    "MINIO_GO_SUM_SHA256: $minio_go_sum_sha" \
    "MINIO_PATCH_SHA256: $minio_patch_sha" \
    "MINIO_SOURCE_SHA256: $minio_source_sha" \
    "MINIO_COMMIT: $SOURCE_COMMIT" \
    'MINIO_TAG: RELEASE.2025-10-15T17-29-55Z' \
    "RUNTIME_IMAGE: $minio_runtime_image" \
    "SOURCE_IMAGE: $minio_source_image" \
    "STAGING_RUNTIME_IMAGE: ghcr.io/concord-voice/minio-staging:$minio_derivative_tag-\${{ github.run_id }}" \
    "STAGING_SOURCE_IMAGE: ghcr.io/concord-voice/minio-source-staging:$minio_derivative_tag-\${{ github.run_id }}" | sort)"
  assert_equals "$env_contract" "$expected_env" "workflow has only fixed publication constants"
  assert_equals "$minio_derivative_commit" "$DERIVATIVE_COMMIT" "workflow pins the intended dependency derivative commit"
  assert_equals "$minio_derivative_tree" "$DERIVATIVE_TREE" "workflow pins the intended dependency derivative tree"
  assert_equals "$minio_patch_sha" "$PATCH_SHA256" "workflow pins the intended dependency patch checksum"
  assert_equals "$minio_go_mod_sha" "$GO_MOD_SHA256" "workflow pins the intended go.mod checksum"
  assert_equals "$minio_go_sum_sha" "$GO_SUM_SHA256" "workflow pins the intended go.sum checksum"
  assert_equals "$minio_source_sha" "$SOURCE_SHA256" "workflow pins the intended source archive checksum"
  assert_equals "$(sha256sum "$ROOT/infrastructure/docker/minio/dependencies.patch" | cut -d ' ' -f1)" \
    "$PATCH_SHA256" "checked-in dependency patch matches the intended immutable checksum"
  assert_contains <(printf '%s\n' "$minio_derivative_commit") '^[0-9a-f]{40}$' "derivative identity is a full Git commit"
  assert_contains <(printf '%s\n' "$minio_derivative_tree") '^[0-9a-f]{40}$' "derivative tree identity is a full Git tree"
  assert_equals "${minio_derivative_tag##*.}" "${minio_derivative_commit:0:12}" "derivative tag names its commit"
  for checksum in "$minio_patch_sha" "$minio_go_mod_sha" "$minio_go_sum_sha" "$minio_source_sha"; do
    assert_contains <(printf '%s\n' "$checksum") '^[0-9a-f]{64}$' "source provenance checksum is SHA-256"
  done
  assert_contains <(printf '%s\n' "$minio_runtime_image") "^ghcr\\.io/concord-voice/minio:${minio_derivative_tag}$" "runtime staging and stable tags use the derivative identity"
  assert_contains <(printf '%s\n' "$minio_source_image") "^ghcr\\.io/concord-voice/minio-source:${minio_derivative_tag}$" "source staging and stable tags use the derivative identity"

  awk '$0=="  publish:" { inside=1; next } inside && /^  [A-Za-z0-9_-]+:/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$PUBLISH_JOB"
  normalize_lines "$PUBLISH_JOB" "$PUBLISH_LINES"
  join_continuations "$PUBLISH_LINES" "$PUBLISH_LOGICAL"
  extract_workflow_run_step "$ACTIVE_WORKFLOW" \
    'Verify exact unchanged upstream source and apply reviewed dependency patch' "$SOURCE_VERIFY_BLOCK"
  extract_workflow_run_step "$ACTIVE_WORKFLOW" \
    'Prepare deterministic corresponding source' "$SOURCE_PREPARE_BLOCK"
  extract_workflow_run_step "$ACTIVE_WORKFLOW" \
    'Verify embedded Go module metadata on native amd64' "$AMD_METADATA_BLOCK"
  extract_workflow_run_step "$ACTIVE_WORKFLOW" \
    'Verify embedded Go module metadata on native arm64' "$ARM_METADATA_BLOCK"
  assert_contains "$AMD_METADATA_BLOCK" 'docker run .*--platform linux/amd64.*golang:1\.26\.10-bookworm@sha256:' \
    "amd64 module inspector is pinned and explicitly selects linux/amd64"
  assert_contains "$ARM_METADATA_BLOCK" 'docker run .*golang:1\.26\.10-bookworm@sha256:' \
    "arm64 module inspector uses the native pinned Go image"
  assert_not_contains "$ARM_METADATA_BLOCK" '--platform' \
    "arm64 module inspector does not request emulation"
  assert_contains "$AMD_METADATA_BLOCK" 'grep -Eq .*go1\\.26\\.10.*GO_BUILD_INFO' \
    "amd64 metadata gate requires the embedded Go compiler version"
  assert_contains "$ARM_METADATA_BLOCK" 'grep -Eq .*go1\\.26\\.10.*GO_BUILD_INFO' \
    "arm64 metadata gate requires the embedded Go compiler version"
  assert_contains "$ARM_METADATA_BLOCK" 'github\\.com/rabbitmq/amqp091-go.*v1\\.14\\.0' \
    "arm64 metadata gate requires the repaired AMQP module"
  assert_contains "$AMD_METADATA_BLOCK" 'google\\.golang\\.org/grpc.*v1\\.83\\.2' \
    "amd64 metadata gate requires the repaired gRPC module"
  while IFS=' ' read -r gate_module gate_version; do
    gate_pattern="${gate_module//./\\\\.}.*${gate_version//./\\\\.}"
    assert_contains "$AMD_METADATA_BLOCK" "$gate_pattern" \
      "amd64 metadata gate requires $gate_module $gate_version"
    assert_contains "$ARM_METADATA_BLOCK" "$gate_pattern" \
      "arm64 metadata gate requires $gate_module $gate_version"
  done <<'GATES'
github.com/go-jose/go-jose/v4 v4.1.6
golang.org/x/net v0.62.0
github.com/apache/thrift v0.25.0
github.com/buger/jsonparser v1.1.2
github.com/eclipse/paho.mqtt.golang v1.5.1
github.com/Azure/go-ntlmssp v0.1.1
go.mongodb.org/mongo-driver v1.17.7
golang.org/x/crypto v0.58.0
filippo.io/edwards25519 v1.1.1
github.com/klauspost/compress v1.18.7
go.etcd.io/etcd/client/pkg/v3 v3.5.33
github.com/prometheus/prometheus v0.303.0
go.opentelemetry.io/otel/sdk v1.44.0
GATES
  assert_contains "$AMD_METADATA_BLOCK" '! grep -Eq .*GO_BUILD_INFO' \
    "module metadata gate rejects replacements"

  # These blocks are taken from the real native metadata steps. The Docker stub
  # supplies benign go version -m output while preserving the production checks.
  METADATA_FIXTURE_DIR="$TMP/module-metadata-fixtures"
  METADATA_BIN="$TMP/module-metadata-bin"
  mkdir -p "$METADATA_FIXTURE_DIR" "$METADATA_BIN" "$TMP/runner-temp"
  # One list drives the valid fixture and a wrong-/missing- fixture per module, so
  # a gate that is weakened (for example `|| true`) cannot hide behind a module
  # that has no failing fixture. The slug is the last path element.
  METADATA_GATE_MODULES='github.com/go-jose/go-jose/v4 v4.1.6
golang.org/x/net v0.62.0
github.com/apache/thrift v0.25.0
github.com/buger/jsonparser v1.1.2
github.com/eclipse/paho.mqtt.golang v1.5.1
github.com/Azure/go-ntlmssp v0.1.1
go.mongodb.org/mongo-driver v1.17.7
golang.org/x/crypto v0.58.0
filippo.io/edwards25519 v1.1.1
github.com/klauspost/compress v1.18.7
go.etcd.io/etcd/client/pkg/v3 v3.5.33
github.com/prometheus/prometheus v0.303.0
go.opentelemetry.io/otel/sdk v1.44.0'
  printf '/inspect/minio: go1.26.10\n path\tfixture.invalid/minio\n dep\tgithub.com/rabbitmq/amqp091-go\tv1.14.0\th1:fixture\n dep\tgoogle.golang.org/grpc\tv1.83.2\th1:fixture\n' \
    >"$METADATA_FIXTURE_DIR/valid"
  while IFS=' ' read -r gate_module gate_version; do
    printf ' dep\t%s\t%s\th1:fixture\n' "$gate_module" "$gate_version" >>"$METADATA_FIXTURE_DIR/valid"
  done <<<"$METADATA_GATE_MODULES"
  sed 's/go1\.26\.10/go1.26.8/' "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/wrong-go"
  sed 's/go1\.26\.10/go1.26.9/' "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/vulnerable-go-1269"
  sed 's/v1\.14\.0/v1.13.0/' "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/wrong-amqp"
  sed '/google.golang.org\/grpc/d' "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/missing-grpc"
  METADATA_MODULE_SLUGS=""
  while IFS=' ' read -r gate_module gate_version; do
    gate_slug="${gate_module##*/}"
    METADATA_MODULE_SLUGS+="wrong-$gate_slug missing-$gate_slug suffix-$gate_slug "
    awk -F'\t' -v OFS='\t' -v m="$gate_module" '$2 == m { $3 = "v0.0.1" } { print }' \
      "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/wrong-$gate_slug"
    # The real version with a digit appended (v1.18.7 -> v1.18.71): only a gate that
    # keeps its ([[:space:]]|$) anchor rejects it, so an unanchored prefix match fails.
    awk -F'\t' -v OFS='\t' -v m="$gate_module" '$2 == m { $3 = $3 "1" } { print }' \
      "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/suffix-$gate_slug"
    assert_not_equals "$(cmp -s "$METADATA_FIXTURE_DIR/valid" "$METADATA_FIXTURE_DIR/suffix-$gate_slug"; echo $?)" 0 \
      "suffix-$gate_slug fixture differs from the valid fixture"
    awk -F'\t' -v m="$gate_module" '$2 != m' \
      "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/missing-$gate_slug"
    # Each generated fixture must differ from valid by exactly its one module line.
    assert_not_equals "$(cmp -s "$METADATA_FIXTURE_DIR/valid" "$METADATA_FIXTURE_DIR/wrong-$gate_slug"; echo $?)" 0 \
      "wrong-$gate_slug fixture differs from the valid fixture"
    assert_equals "$(grep -c "$gate_module" "$METADATA_FIXTURE_DIR/missing-$gate_slug" || true)" 0 \
      "missing-$gate_slug fixture has no $gate_module dependency line"
  done <<<"$METADATA_GATE_MODULES"
  cat "$METADATA_FIXTURE_DIR/valid" >"$METADATA_FIXTURE_DIR/replaced"
  printf ' => example.invalid/replacement v9.9.9\n' >>"$METADATA_FIXTURE_DIR/replaced"
  cat >"$METADATA_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DOCKER_LOG"
case "$1" in
  pull|rm) exit 0 ;;
  create) exit 0 ;;
  cp)
    [[ "${METADATA_FAIL:-}" != cp ]]
    target="${@: -1}"
    mkdir -p "$(dirname "$target")"
    : >"$target"
    ;;
  run)
    [[ "${METADATA_FAIL:-}" != run ]]
    cat "$METADATA_FIXTURE"
    ;;
esac
EOF
  cat >"$METADATA_BIN/sudo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == --preserve-env=* ]]; then shift; fi
exec "$@"
EOF
  chmod +x "$METADATA_BIN/docker" "$METADATA_BIN/sudo"
  run_metadata_case() {
    local arch="$1" fixture="$2" failure="${3:-}" digest="${4:-sha256:$(printf '%064d' 1)}" block
    if [[ "$arch" == amd64 ]]; then block="$AMD_METADATA_BLOCK"; else block="$ARM_METADATA_BLOCK"; fi
    METADATA_FIXTURE="$METADATA_FIXTURE_DIR/$fixture" METADATA_FAIL="$failure" \
      DOCKER_LOG="$TMP/docker-$arch-$fixture-$failure.log" \
      TMPDIR="$TMP" STAGING_RUNTIME_IMAGE=example.invalid/minio-staging:fixture \
      RUNTIME_DIGEST="$digest" GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=1 \
      PATH="$METADATA_BIN:$PATH" \
      bash -euo pipefail "$block" >"$TMP/metadata.out" 2>&1
  }
  for arch in amd64 arm64; do
    run_metadata_case "$arch" valid '' sha256:1
    assert_not_equals "$?" 0 "actual $arch metadata step rejects a malformed runtime digest"
    assert_absent "$TMP/docker-$arch-valid-.log" "$arch digest format rejection happens before Docker is invoked"
    run_metadata_case "$arch" valid
    assert_equals "$?" 0 "actual $arch metadata gate accepts the valid compiler and dependency fixture"
    metadata_log="$TMP/docker-$arch-valid-.log"
    if [[ "$arch" == amd64 ]]; then
      assert_contains "$metadata_log" '--platform linux/amd64' "amd64 metadata inspector routes to linux/amd64"
    else
      assert_not_contains "$metadata_log" '--platform' "arm64 metadata inspector stays native"
    fi
    for invalid in wrong-go vulnerable-go-1269 wrong-amqp missing-grpc $METADATA_MODULE_SLUGS replaced; do
      run_metadata_case "$arch" "$invalid"
      assert_not_equals "$?" 0 "actual $arch metadata gate rejects $invalid module metadata"
    done
    run_metadata_case "$arch" valid cp
    assert_not_equals "$?" 0 "actual $arch metadata gate fails closed when binary extraction fails"
    run_metadata_case "$arch" valid run
    assert_not_equals "$?" 0 "actual $arch metadata gate fails closed when Go inspection fails"
  done
  assert_contains "$PUBLISH_JOB" '^    if:[[:space:]]+github\.ref == '\''refs/heads/main'\''$' \
    "publisher refuses non-main workflow dispatches"
  awk '$0=="    permissions:" { inside=1; next } inside && /^    [^[:space:]]/ { exit } inside { print }' \
    "$PUBLISH_JOB" >"$PUBLISH_PERMISSIONS"
  publish_permissions="$(awk '/^      [A-Za-z0-9_-]+:/ { $1=$1; print }' "$PUBLISH_PERMISSIONS" | sort)"
  assert_equals "$publish_permissions" $'contents: read\npackages: write' \
    "publish job permissions are exactly contents read and packages write"
  write_grants="$(grep -Eo '[A-Za-z0-9_-]+:[[:space:]]+write([[:space:]]|$)' "$ACTIVE_WORKFLOW" | sed -E 's/[[:space:]]+$//' | sort -u || true)"
  assert_equals "$write_grants" 'packages: write' "packages write is the only write grant"
  assert_not_contains "$ACTIVE_WORKFLOW" 'write-all' "workflow never grants write-all"
  assert_not_contains "$ACTIVE_WORKFLOW" 'permissions:[[:space:]]*\{' "workflow forbids flow-style permission bypasses"

  awk '$0=="  smoke-arm64:" { inside=1; next } inside && /^  [A-Za-z0-9_-]+:/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$ARM_JOB"
  normalize_lines "$ARM_JOB" "$ARM_LINES"
  join_continuations "$ARM_LINES" "$ARM_LOGICAL"
  awk '$0=="    permissions:" { inside=1; next } inside && /^    [^[:space:]]/ { exit } inside { print }' \
    "$ARM_JOB" >"$ARM_PERMISSIONS"
  arm_permissions="$(awk '/^      [A-Za-z0-9_-]+:/ { $1=$1; print }' "$ARM_PERMISSIONS" | sort)"
  assert_equals "$arm_permissions" $'contents: read\npackages: read' \
    "arm64 job permissions are exactly contents and packages read"
  awk '$0=="  finalize:" { inside=1; next } inside && /^  [A-Za-z0-9_-]+:/ { exit } inside { print }' \
    "$ACTIVE_WORKFLOW" >"$FINALIZE_JOB"
  normalize_lines "$FINALIZE_JOB" "$FINALIZE_LINES"
  join_continuations "$FINALIZE_LINES" "$FINALIZE_LOGICAL"
  awk '$0=="    permissions:" { inside=1; next } inside && /^    [^[:space:]]/ { exit } inside { print }' \
    "$FINALIZE_JOB" >"$FINALIZE_PERMISSIONS"
  finalize_permissions="$(awk '/^      [A-Za-z0-9_-]+:/ { $1=$1; print }' "$FINALIZE_PERMISSIONS" | sort)"
  assert_equals "$finalize_permissions" $'contents: read\npackages: write' \
    "finalizer permissions are exactly contents read and packages write"
  awk '$0=="jobs:" { inside=1; next } inside { print }' "$ACTIVE_WORKFLOW" >"$JOBS_BLOCK"
  job_set="$(awk '/^  [A-Za-z0-9_-]+:/ { key=$1; sub(/:$/, "", key); print key }' "$JOBS_BLOCK")"
  assert_equals "$job_set" $'publish\nsmoke-arm64\nfinalize' \
    "publisher has build, native arm64, and finalization jobs"
  assert_contains "$ARM_JOB" '^    needs:[[:space:]]+publish$' "native arm64 smoke depends on publish"
  assert_contains "$WORKFLOW" '^    runs-on:[[:space:]]+ubuntu-24\.04-arm[[:space:]]+# trust-boundary \(ADR-0016\); native-arm64 correctness \(ADR-0013 section 3\)$' \
    "native smoke uses the governed arm64 runner"
  assert_count "$ACTIVE_WORKFLOW" '^    runs-on:[[:space:]]+ubuntu-24\.04([[:space:]]|$)' 2 \
    "publisher and finalizer use GitHub-hosted amd64 runners"
  assert_literal_count "$WORKFLOW" '# trust-boundary (ADR-0016)' 3 \
    "every credential-path job documents the runner trust boundary"
  assert_contains "$FINALIZE_JOB" '^    needs:[[:space:]]+\[publish, smoke-arm64\]$' \
    "final tags depend on both native verification jobs"

  allowed_uses="$CHECKOUT_ACTION"$'\n'"$BUILD_PUSH_ACTION"$'\n'"$BUILDX_ACTION"$'\n'"$LOGIN_ACTION"$'\n'"$ORAS_ACTION"
  actual_uses="$(sed -nE 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]+([^[:space:]#]+).*/\2/p' "$ACTIVE_WORKFLOW" | sort -u)"
  assert_equals "$actual_uses" "$(printf '%s\n' "$allowed_uses" | sort)" "third-party action set is exact"
  assert_literal_count "$ACTIVE_WORKFLOW" "uses: $CHECKOUT_ACTION" 4 \
    "checkout is SHA-pinned for Concord, MinIO, arm64 guard, and finalization"
  assert_literal_count "$ACTIVE_WORKFLOW" "uses: $LOGIN_ACTION" 2 "$LOGIN_ACTION is used exactly twice"
  assert_literal_count "$ACTIVE_WORKFLOW" "uses: $ORAS_ACTION" 2 "$ORAS_ACTION is used exactly twice"
  assert_before "$PUBLISH_LINES" "uses: $ORAS_ACTION" "uses: $LOGIN_ACTION" \
    "publisher installs ORAS before registry credentials are available"
  assert_before "$FINALIZE_LINES" "uses: $ORAS_ACTION" "uses: $LOGIN_ACTION" \
    "finalizer installs ORAS before registry credentials are available"
  for action in "$BUILDX_ACTION" "$BUILD_PUSH_ACTION"; do
    assert_literal_count "$ACTIVE_WORKFLOW" "uses: $action" 1 "$action is used exactly once"
  done
  assert_not_contains "$ACTIVE_WORKFLOW" 'setup-qemu|tonistiigi/binfmt' \
    "publisher has no privileged QEMU/binfmt dependency"
  assert_literal_count "$ACTIVE_WORKFLOW" 'persist-credentials: false' 4 \
    "all checkouts discard Git credentials"
  assert_contains "$ACTIVE_WORKFLOW" "^  BUILDX_VERSION:[[:space:]]+${BUILDX_VERSION}$" \
    "Buildx version is fixed"
  assert_contains "$ACTIVE_WORKFLOW" "^  BUILDX_LINUX_AMD64_SHA256:[[:space:]]+${BUILDX_LINUX_AMD64_SHA256}$" \
    "Buildx Linux amd64 release asset has a reviewed SHA-256"
  assert_contains "$TRIM_WORKFLOW" '^BUILDX_PATH="\$HOME/\.docker/cli-plugins/docker-buildx"$' \
    "Buildx is installed as a Docker CLI plugin"
  assert_contains "$TRIM_WORKFLOW" '^curl .*--proto '\''=https'\'' .*--tlsv1\.2 .*--output "\$BUILDX_PATH" "https://github\.com/docker/buildx/releases/download/\$\{BUILDX_VERSION\}/buildx-\$\{BUILDX_VERSION\}\.linux-amd64"$' \
    "Buildx comes from the fixed official release asset over HTTPS"
  assert_literal_count "$ACTIVE_WORKFLOW" '          printf '\''%s  %s\n'\'' "$BUILDX_LINUX_AMD64_SHA256" "$BUILDX_PATH" | sha256sum -c -' 1 \
    "Buildx is SHA-256 verified before use"
  assert_before "$TRIM_WORKFLOW" \
    'printf '\''%s %s\n'\'' "$BUILDX_LINUX_AMD64_SHA256" "$BUILDX_PATH" | sha256sum -c -' \
    'chmod 0755 "$BUILDX_PATH"' \
    "Buildx cannot execute before its checksum passes"
  assert_contains "$ACTIVE_WORKFLOW" '^          cache-binary:[[:space:]]+false$' \
    "setup-buildx cannot replace the verified binary from its cache"
  assert_not_contains "$ACTIVE_WORKFLOW" '^          version:[[:space:]]+v0\.35\.0$' \
    "setup-buildx reuses the verified preinstalled binary"
  assert_before "$TRIM_WORKFLOW" 'chmod 0755 "$BUILDX_PATH"' "uses: $BUILDX_ACTION" \
    "verified Buildx is installed before builder setup"
  assert_before "$TRIM_WORKFLOW" "uses: $BUILDX_ACTION" "uses: $LOGIN_ACTION" \
    "Buildx setup finishes before registry credentials are available"
  assert_contains "$ACTIVE_WORKFLOW" "^          driver-opts:[[:space:]]+image=moby/buildkit:v0\\.34\\.1@sha256:${BUILDKIT_DIGEST}$" \
    "BuildKit driver image is digest-pinned"
  assert_contains "$ACTIVE_WORKFLOW" '^          buildkitd-flags:[[:space:]]+--debug$' \
    "custom BuildKit flags reset the action defaults"
  assert_not_contains "$ACTIVE_WORKFLOW" 'allow-insecure-entitlement' \
    "publisher does not enable insecure BuildKit entitlements"
  assert_contains "$ACTIVE_WORKFLOW" 'version:[[:space:]]*["'\'']?1\.3\.3["'\'']?' "ORAS version is fixed at 1.3.3"

  assert_contains "$ACTIVE_WORKFLOW" '^          repository:[[:space:]]+minio/minio$' "workflow checks out upstream MinIO"
  assert_contains "$ACTIVE_WORKFLOW" '^          ref:[[:space:]]+\$\{\{ env\.MINIO_TAG \}\}$' "MinIO checkout uses the fixed tag"
  assert_contains "$ACTIVE_WORKFLOW" '^          path:[[:space:]]+minio-source$' "MinIO checkout path is fixed"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^ACTUAL_COMMIT="\$\(git -C minio-source rev-parse HEAD\)"$' "source block reads checked-out MinIO commit"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^test "\$ACTUAL_COMMIT" = "\$MINIO_COMMIT"$' "source block requires the fixed upstream commit"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^SOURCE_STATUS="\$\(git -C minio-source status --porcelain --untracked-files=all --ignored\)"$' \
    "source block captures tracked, untracked, and ignored upstream changes"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^test -z "\$SOURCE_STATUS"$' "source block requires pristine upstream input"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^printf .*\$MINIO_PATCH_SHA256.*sha256sum -c -$' "source block verifies the exact dependency patch"
  assert_contains "$SOURCE_VERIFY_BLOCK" 'PATCHED_STATUS.*M go.mod\\n M go.sum' \
    "source block permits exactly the two manifest edits"
  assert_contains "$SOURCE_VERIFY_BLOCK" 'diff --name-only --no-renames.*go.mod\\ngo.sum' \
    "source block rejects every other tracked source path"
  assert_contains "$SOURCE_VERIFY_BLOCK" 'commit-tree "\$DERIVATIVE_TREE" -p "\$MINIO_COMMIT" -m' \
    "derivative commit is parented to the verified upstream commit"
  assert_contains "$SOURCE_VERIFY_BLOCK" "GIT_AUTHOR_DATE='2026-11-02T00:00:00Z'.*GIT_COMMITTER_DATE='2026-11-02T00:00:00Z'" \
    "derivative commit identity and timestamp are deterministic"
  assert_before "$SOURCE_VERIFY_BLOCK" 'test "$ACTUAL_COMMIT" = "$MINIO_COMMIT"' \
    'SOURCE_STATUS="$(git -C minio-source status --porcelain --untracked-files=all --ignored)"' \
    "upstream commit is checked before source cleanliness"
  assert_before "$SOURCE_VERIFY_BLOCK" 'test -z "$SOURCE_STATUS"' \
    'git -C minio-source apply --check --whitespace=error "$GITHUB_WORKSPACE/infrastructure/docker/minio/dependencies.patch"' \
    "patch is applied only after the upstream checkout is proven clean"
  assert_before "$SOURCE_VERIFY_BLOCK" \
    'git -C minio-source apply --check --whitespace=error "$GITHUB_WORKSPACE/infrastructure/docker/minio/dependencies.patch"' \
    'git -C minio-source apply --whitespace=error "$GITHUB_WORKSPACE/infrastructure/docker/minio/dependencies.patch"' \
    "dependency patch is checked before application"
  assert_before "$SOURCE_VERIFY_BLOCK" \
    'git -C minio-source add -- go.mod go.sum' \
    'DERIVATIVE_TREE="$(git -C minio-source write-tree)"' \
    "only the allowlisted manifests enter the derivative tree"
  assert_before "$SOURCE_VERIFY_BLOCK" 'DERIVATIVE_TREE="$(git -C minio-source write-tree)"' \
    'test "$DERIVATIVE_TREE" = "$MINIO_DERIVATIVE_TREE"' "derivative tree is measured before comparison"
  assert_before "$SOURCE_VERIFY_BLOCK" 'test "$DERIVATIVE_COMMIT" = "$MINIO_DERIVATIVE_COMMIT"' \
    'test "$(git -C minio-source show -s --format=%P "$DERIVATIVE_COMMIT")" = "$MINIO_COMMIT"' \
    "derivative commit is checked against its upstream parent"
  assert_contains "$SOURCE_VERIFY_BLOCK" '^printf .*\$MINIO_GO_MOD_SHA256.*minio-source/go.mod.*\$MINIO_GO_SUM_SHA256.*minio-source/go.sum.*sha256sum -c -$' \
    "source block checks separate go.mod and go.sum SHA-256 pins"
  assert_contains "$ACTIVE_WORKFLOW" '^          context:[[:space:]]+minio-build-context$' "build consumes the extracted verified derivative"
  assert_contains "$ACTIVE_WORKFLOW" '^          file:[[:space:]]+infrastructure/docker/minio/Dockerfile$' "build uses Concord Dockerfile"
  assert_contains "$SOURCE_PREPARE_BLOCK" '^printf .*\$MINIO_SOURCE_SHA256.*sha256sum -c -$' \
    "source archive is checked against its independent SHA-256 pin"
  assert_contains "$SOURCE_PREPARE_BLOCK" '^printf .*\$MINIO_GO_MOD_SHA256.*minio-build-context/go.mod.*> go.mod.sha256$' \
    "source artifact records the go.mod checksum separately"
  assert_contains "$SOURCE_PREPARE_BLOCK" '^printf .*\$MINIO_GO_SUM_SHA256.*minio-build-context/go.sum.*> go.sum.sha256$' \
    "source artifact records the go.sum checksum separately"
  assert_before "$SOURCE_PREPARE_BLOCK" \
    'git -C minio-source archive --format=tar "$MINIO_DERIVATIVE_COMMIT" | gzip -n > minio-source.tar.gz' \
    'printf '\''%s  %s\n'\'' "$MINIO_SOURCE_SHA256" minio-source.tar.gz | sha256sum -c -' \
    "archive uses the derivative commit and verifies its pinned checksum"
  assert_before "$SOURCE_PREPARE_BLOCK" \
    'printf '\''%s  %s\n'\'' "$MINIO_SOURCE_SHA256" minio-source.tar.gz | sha256sum -c -' \
    'mkdir minio-build-context' "build context is created only after archive verification"
  assert_before "$SOURCE_PREPARE_BLOCK" 'tar -xzf minio-source.tar.gz -C minio-build-context' \
    'cp infrastructure/docker/minio/dependencies.patch dependencies.patch' \
    "build context is extracted from the verified source archive"

  # Execute the workflow's real preparation blocks against a tiny local Git repo.
  # No Go toolchain or network is involved; all pins are derived from this fixture.
  SOURCE_FIXTURE="$TMP/source-fixture"
  SOURCE_TEMPLATE="$SOURCE_FIXTURE/template"
  SOURCE_WORKSPACE="$SOURCE_FIXTURE/workspace"
  SOURCE_PATCH="$SOURCE_WORKSPACE/infrastructure/docker/minio/dependencies.patch"
  mkdir -p "$SOURCE_TEMPLATE" "$SOURCE_WORKSPACE/infrastructure/docker/minio"
  git -C "$SOURCE_TEMPLATE" init -q
  git -C "$SOURCE_TEMPLATE" config user.name 'Fixture Builder'
  git -C "$SOURCE_TEMPLATE" config user.email 'fixture@example.invalid'
  cat >"$SOURCE_TEMPLATE/go.mod" <<'EOF'
module fixture.invalid/minio

go 1.25.0

require example.invalid/dependency v1.0.0
EOF
  printf 'example.invalid/dependency v1.0.0 h1:fixturebase\n' >"$SOURCE_TEMPLATE/go.sum"
  printf 'benign fixture source\n' >"$SOURCE_TEMPLATE/README.md"
  printf 'ignored-fixture\n' >"$SOURCE_TEMPLATE/.gitignore"
  git -C "$SOURCE_TEMPLATE" add -- go.mod go.sum README.md .gitignore
  git -C "$SOURCE_TEMPLATE" commit -qm 'fixture upstream base'
  FIXTURE_UPSTREAM_COMMIT="$(git -C "$SOURCE_TEMPLATE" rev-parse HEAD)"
  FIXTURE_UPSTREAM_TREE="$(git -C "$SOURCE_TEMPLATE" rev-parse 'HEAD^{tree}')"
  FIXTURE_BASE_MOD_SHA="$(sha256sum "$SOURCE_TEMPLATE/go.mod" | cut -d ' ' -f1)"
  sed -i.bak 's/example.invalid\/dependency v1.0.0/example.invalid\/dependency v1.1.0/' "$SOURCE_TEMPLATE/go.mod"
  rm -f "$SOURCE_TEMPLATE/go.mod.bak"
  printf 'example.invalid/dependency v1.1.0 h1:fixtureupdated\n' >"$SOURCE_TEMPLATE/go.sum"
  git -C "$SOURCE_TEMPLATE" diff -- go.mod go.sum >"$SOURCE_PATCH"
  FIXTURE_PATCH_SHA="$(sha256sum "$SOURCE_PATCH" | cut -d ' ' -f1)"
  FIXTURE_MOD_SHA="$(sha256sum "$SOURCE_TEMPLATE/go.mod" | cut -d ' ' -f1)"
  FIXTURE_SUM_SHA="$(sha256sum "$SOURCE_TEMPLATE/go.sum" | cut -d ' ' -f1)"
  git -C "$SOURCE_TEMPLATE" add -- go.mod go.sum
  FIXTURE_DERIVATIVE_TREE="$(git -C "$SOURCE_TEMPLATE" write-tree)"
  FIXTURE_DERIVATIVE_COMMIT="$(GIT_AUTHOR_NAME='Concord Voice' GIT_AUTHOR_EMAIL='build@concordvoice.com' GIT_AUTHOR_DATE='2026-11-02T00:00:00Z' GIT_COMMITTER_NAME='Concord Voice' GIT_COMMITTER_EMAIL='build@concordvoice.com' GIT_COMMITTER_DATE='2026-11-02T00:00:00Z' git -C "$SOURCE_TEMPLATE" -c commit.gpgsign=false commit-tree "$FIXTURE_DERIVATIVE_TREE" -p "$FIXTURE_UPSTREAM_COMMIT" -m 'Concord fresh dependency remediation for #3441 #3469 #3527 #3528 #3530-#3534 #3716')"
  git -C "$SOURCE_TEMPLATE" reset -q --hard "$FIXTURE_UPSTREAM_COMMIT"
  git -C "$SOURCE_TEMPLATE" fetch -q . "$FIXTURE_DERIVATIVE_COMMIT"
  FIXTURE_ARCHIVE_SHA="$(git -C "$SOURCE_TEMPLATE" archive --format=tar "$FIXTURE_DERIVATIVE_COMMIT" | gzip -n | sha256sum | cut -d ' ' -f1)"
  cp "$ROOT/infrastructure/docker/minio/Dockerfile" "$SOURCE_WORKSPACE/infrastructure/docker/minio/Dockerfile"
  cp "$ROOT/infrastructure/docker/minio/SOURCE-BUILD.md" "$SOURCE_WORKSPACE/infrastructure/docker/minio/SOURCE-BUILD.md"

  source_fixture_case() {
    local name="$1"
    mkdir -p "$SOURCE_FIXTURE/cases/$name"
    cp -R "$SOURCE_TEMPLATE" "$SOURCE_FIXTURE/cases/$name/minio-source"
    cp -R "$SOURCE_WORKSPACE/infrastructure" "$SOURCE_FIXTURE/cases/$name/"
  }
  run_source_verify() {
    local dir="$1" patch_sha="$2" mod_sha="$3" sum_sha="$4" upstream="$5" tree="$6" commit="$7"
    (cd "$dir" && env GITHUB_WORKSPACE="$dir" MINIO_COMMIT="$upstream" \
      MINIO_PATCH_SHA256="$patch_sha" MINIO_GO_MOD_SHA256="$mod_sha" MINIO_GO_SUM_SHA256="$sum_sha" \
      MINIO_DERIVATIVE_TREE="$tree" MINIO_DERIVATIVE_COMMIT="$commit" \
      MINIO_SOURCE_SHA256="$FIXTURE_ARCHIVE_SHA" MINIO_TAG=fixture-upstream \
      MINIO_DERIVATIVE_TAG=fixture-derivative bash "$SOURCE_VERIFY_BLOCK" >verify.out 2>&1)
    SOURCE_VERIFY_RC=$?
  }
  source_fixture_case positive-one
  source_fixture_case positive-two
  for source_case in positive-one positive-two; do
    case_dir="$SOURCE_FIXTURE/cases/$source_case"
    run_source_verify "$case_dir" "$FIXTURE_PATCH_SHA" "$FIXTURE_MOD_SHA" "$FIXTURE_SUM_SHA" \
      "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
    assert_equals "$SOURCE_VERIFY_RC" 0 "actual source verifier accepts the pinned benign fixture ($source_case)"
    (cd "$case_dir" && env GITHUB_WORKSPACE="$case_dir" MINIO_COMMIT="$FIXTURE_UPSTREAM_COMMIT" \
      MINIO_PATCH_SHA256="$FIXTURE_PATCH_SHA" MINIO_GO_MOD_SHA256="$FIXTURE_MOD_SHA" MINIO_GO_SUM_SHA256="$FIXTURE_SUM_SHA" \
      MINIO_DERIVATIVE_TREE="$FIXTURE_DERIVATIVE_TREE" MINIO_DERIVATIVE_COMMIT="$FIXTURE_DERIVATIVE_COMMIT" \
      MINIO_SOURCE_SHA256="$FIXTURE_ARCHIVE_SHA" MINIO_TAG=fixture-upstream MINIO_DERIVATIVE_TAG=fixture-derivative \
      bash "$SOURCE_PREPARE_BLOCK" >prepare.out 2>&1)
    assert_equals "$?" 0 "actual source preparer accepts the verified fixture ($source_case)"
  done
  assert_equals "$(cmp -s "$SOURCE_FIXTURE/cases/positive-one/minio-source.tar.gz" "$SOURCE_FIXTURE/cases/positive-two/minio-source.tar.gz"; echo $?)" 0 \
    "repeated source preparation produces byte-identical archives"
  assert_equals "$(diff -qr "$SOURCE_FIXTURE/cases/positive-one/minio-build-context" "$SOURCE_FIXTURE/cases/positive-two/minio-build-context" >/dev/null; echo $?)" 0 \
    "repeated source preparation produces byte-identical build contexts"
  mkdir -p "$SOURCE_FIXTURE/reference-context"
  git -C "$SOURCE_FIXTURE/cases/positive-one/minio-source" archive --format=tar "$FIXTURE_DERIVATIVE_COMMIT" | tar -xf - -C "$SOURCE_FIXTURE/reference-context"
  assert_equals "$(diff -qr "$SOURCE_FIXTURE/cases/positive-one/minio-build-context" "$SOURCE_FIXTURE/reference-context" >/dev/null; echo $?)" 0 \
    "extracted build context matches the verified derivative tree"

  source_fixture_case wrong-base
  WRONG_BASE="$SOURCE_FIXTURE/cases/wrong-base"
  printf 'changed base\n' >>"$WRONG_BASE/minio-source/README.md"
  git -C "$WRONG_BASE/minio-source" add README.md
  git -C "$WRONG_BASE/minio-source" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm 'wrong upstream base'
  run_source_verify "$WRONG_BASE" "$FIXTURE_PATCH_SHA" "$FIXTURE_MOD_SHA" "$FIXTURE_SUM_SHA" "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
  assert_not_equals "$SOURCE_VERIFY_RC" 0 "actual verifier rejects the wrong upstream base"
  assert_equals "$(sha256sum "$WRONG_BASE/minio-source/go.mod" | cut -d ' ' -f1)" \
    "$FIXTURE_BASE_MOD_SHA" \
    "wrong-base failure occurs before patch application"
  assert_absent "$WRONG_BASE/minio-source.tar.gz" "wrong-base rejection prevents archive creation"
  assert_absent "$WRONG_BASE/minio-build-context" "wrong-base rejection prevents context creation"

  source_fixture_case altered-patch
  ALTERED_PATCH="$SOURCE_FIXTURE/cases/altered-patch"
  printf '\n# changed bytes\n' >>"$ALTERED_PATCH/infrastructure/docker/minio/dependencies.patch"
  run_source_verify "$ALTERED_PATCH" "$FIXTURE_PATCH_SHA" "$FIXTURE_MOD_SHA" "$FIXTURE_SUM_SHA" "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
  assert_not_equals "$SOURCE_VERIFY_RC" 0 "actual verifier rejects changed patch bytes"
  assert_equals "$(git -C "$ALTERED_PATCH/minio-source" status --porcelain | wc -l | tr -d ' ')" 0 \
    "changed-patch failure occurs before patch application"
  assert_absent "$ALTERED_PATCH/minio-source.tar.gz" "changed-patch rejection prevents archive creation"
  assert_absent "$ALTERED_PATCH/minio-build-context" "changed-patch rejection prevents context creation"

  source_fixture_case bad-manifest-hash
  BAD_HASH="$SOURCE_FIXTURE/cases/bad-manifest-hash"
  run_source_verify "$BAD_HASH" "$FIXTURE_PATCH_SHA" "$(printf '%064d' 0)" "$FIXTURE_SUM_SHA" "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
  assert_not_equals "$SOURCE_VERIFY_RC" 0 "actual verifier rejects a mismatched manifest hash"
  assert_equals "$(sha256sum "$BAD_HASH/minio-source/go.mod" | cut -d ' ' -f1)" "$FIXTURE_MOD_SHA" \
    "manifest-hash failure follows successful patch application"
  assert_equals "$(git -C "$BAD_HASH/minio-source" write-tree)" "$FIXTURE_UPSTREAM_TREE" \
    "manifest-hash failure leaves the upstream index untouched"
  assert_absent "$BAD_HASH/minio-source.tar.gz" "manifest-hash rejection prevents archive creation"
  assert_absent "$BAD_HASH/minio-build-context" "manifest-hash rejection prevents context creation"

  source_fixture_case extra-harmless-file
  EXTRA_FILE="$SOURCE_FIXTURE/cases/extra-harmless-file"
  cp -R "$SOURCE_TEMPLATE" "$SOURCE_FIXTURE/extra-source"
  git -C "$SOURCE_FIXTURE/extra-source" reset -q --hard "$FIXTURE_UPSTREAM_COMMIT"
  git -C "$SOURCE_FIXTURE/extra-source" apply "$SOURCE_PATCH"
  printf 'harmless extra source\n' >"$SOURCE_FIXTURE/extra-source/benign-extra.txt"
  git -C "$SOURCE_FIXTURE/extra-source" add -N -- benign-extra.txt
  git -C "$SOURCE_FIXTURE/extra-source" diff >"$EXTRA_FILE/infrastructure/docker/minio/dependencies.patch"
  EXTRA_PATCH_SHA="$(sha256sum "$EXTRA_FILE/infrastructure/docker/minio/dependencies.patch" | cut -d ' ' -f1)"
  run_source_verify "$EXTRA_FILE" "$EXTRA_PATCH_SHA" "$FIXTURE_MOD_SHA" "$FIXTURE_SUM_SHA" "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
  assert_not_equals "$SOURCE_VERIFY_RC" 0 "actual verifier rejects an additional harmless source path"
  assert_equals "$(git -C "$EXTRA_FILE/minio-source" status --porcelain --untracked-files=all --ignored | wc -l | tr -d ' ')" 3 \
    "extra-path failure occurs after applying the patch and before derivative creation"
  assert_absent "$EXTRA_FILE/minio-source.tar.gz" "extra-path rejection prevents archive creation"
  assert_absent "$EXTRA_FILE/minio-build-context" "extra-path rejection prevents context creation"

  source_fixture_case ignored-file
  IGNORED_FILE="$SOURCE_FIXTURE/cases/ignored-file"
  printf 'fixture-only\n' >"$IGNORED_FILE/minio-source/ignored-fixture"
  run_source_verify "$IGNORED_FILE" "$FIXTURE_PATCH_SHA" "$FIXTURE_MOD_SHA" "$FIXTURE_SUM_SHA" "$FIXTURE_UPSTREAM_COMMIT" "$FIXTURE_DERIVATIVE_TREE" "$FIXTURE_DERIVATIVE_COMMIT"
  assert_not_equals "$SOURCE_VERIFY_RC" 0 "actual verifier rejects an ignored untracked file"
  assert_equals "$(sha256sum "$IGNORED_FILE/minio-source/go.mod" | cut -d ' ' -f1)" "$FIXTURE_BASE_MOD_SHA" \
    "ignored-file rejection occurs before patch application"
  assert_equals "$(git -C "$IGNORED_FILE/minio-source" write-tree)" "$FIXTURE_UPSTREAM_TREE" \
    "ignored-file rejection leaves the upstream index untouched"
  assert_absent "$IGNORED_FILE/minio-source.tar.gz" "ignored-file rejection prevents archive creation"
  assert_absent "$IGNORED_FILE/minio-build-context" "ignored-file rejection prevents context creation"

  assert_count "$ACTIVE_WORKFLOW" '^          outputs:' 1 "workflow has exactly one runtime exporter field"
  assert_contains "$ACTIVE_WORKFLOW" '^          outputs:[[:space:]]+type=image,name=\$\{\{ env\.STAGING_RUNTIME_IMAGE \}\},push=true,rewrite-timestamp=true$' \
    "runtime exporter publishes only the reproducible run-scoped staging image"
  assert_not_contains "$ACTIVE_WORKFLOW" '^[[:space:]]+(tags|push):' \
    "runtime publication cannot add a second implicit exporter"
  assert_count "$ACTIVE_WORKFLOW" '^[[:space:]]+platforms:' 1 "workflow has exactly one build platform field"
  assert_contains "$ACTIVE_WORKFLOW" '^          platforms:[[:space:]]+linux/amd64,linux/arm64$' \
    "runtime build platform set is exact"
  assert_literal_count "$ACTIVE_WORKFLOW" 'push=true,rewrite-timestamp=true' 1 \
    "runtime exporter is the only push path and rewrites layer timestamps"
  assert_count "$ACTIVE_WORKFLOW" '^          provenance:[[:space:]]+false$' 1 \
    "runtime build uses the repository's fixed provenance policy"
  assert_not_contains "$ACTIVE_WORKFLOW" 'ghcr\.io/concord-voice/minio[^:]*:latest' "publisher never writes latest"
  literal_targets="$(grep -Eo 'ghcr\.io/concord-voice/minio(-source)?:[^[:space:]]+' "$ACTIVE_WORKFLOW" | sort -u || true)"
  expected_targets="$(printf '%s\n' "$minio_source_image" "$minio_runtime_image" | sort)"
  assert_equals "$literal_targets" "$expected_targets" "literal stable publication targets are exactly the two fixed tags"
  assert_not_contains "$ACTIVE_WORKFLOW" '(docker[[:space:]]+push|docker[[:space:]]+buildx[[:space:]]+build[^#]*--push)' \
    "workflow cannot add scripted Docker publication targets"
  assert_literal_count "$ACTIVE_WORKFLOW" 'oras push' 1 "workflow has exactly one source publication command"
  assert_contains "$TRIM_WORKFLOW" '^ORAS_OUTPUT="\$\(oras push --format json --annotation org\.opencontainers\.image\.created=2025-10-15T17:29:55Z "\$STAGING_SOURCE_IMAGE" ' \
    "ORAS publication targets only the run-scoped source staging tag"
  assert_contains "$PUBLISH_LINES" '^RUNTIME_STATE="\$\(scripts/ci/check-oci-tag\.sh "\$RUNTIME_IMAGE" "\$RUNTIME_DIGEST"\)"$' \
    "runtime stable-tag check is digest-aware and fail closed"
  assert_contains "$PUBLISH_LINES" '^SOURCE_STATE="\$\(scripts/ci/check-oci-tag\.sh "\$SOURCE_IMAGE" "\$SOURCE_DIGEST"\)"$' \
    "source stable-tag check is digest-aware and fail closed"
  assert_sequence "$PUBLISH_LINES" "runtime-only stable publication fails closed before smoke testing" \
    'if [[ "$RUNTIME_STATE" == "matching" && "$SOURCE_STATE" == "absent" ]]; then' \
    'echo "::error::Runtime stable tag exists without corresponding source; stop and escalate"' \
    'exit 1' \
    'fi'
  assert_before "$TRIM_WORKFLOW" "uses: $ORAS_ACTION" \
    'ORAS_OUTPUT="$(oras push --format json --annotation org.opencontainers.image.created=2025-10-15T17:29:55Z "$STAGING_SOURCE_IMAGE" minio-source.tar.gz:application/vnd.concord.minio.source.v1.tar+gzip minio-source.tar.gz.sha256:text/plain dependencies.patch:application/vnd.concord.minio.dependencies.patch.v1+diff dependencies.patch.sha256:text/plain go.mod.sha256:text/plain go.sum.sha256:text/plain source-provenance.txt:text/plain runtime-manifest-digest.txt:text/plain Dockerfile:text/plain SOURCE-BUILD.md:text/markdown)"' \
    "ORAS is initialized before source staging publication"
  assert_before "$TRIM_WORKFLOW" "uses: $BUILD_PUSH_ACTION" \
    'RUNTIME_STATE="$(scripts/ci/check-oci-tag.sh "$RUNTIME_IMAGE" "$RUNTIME_DIGEST")"' \
    "runtime staging publication precedes conflict check"
  assert_before "$TRIM_WORKFLOW" \
    'ORAS_OUTPUT="$(oras push --format json --annotation org.opencontainers.image.created=2025-10-15T17:29:55Z "$STAGING_SOURCE_IMAGE" minio-source.tar.gz:application/vnd.concord.minio.source.v1.tar+gzip minio-source.tar.gz.sha256:text/plain dependencies.patch:application/vnd.concord.minio.dependencies.patch.v1+diff dependencies.patch.sha256:text/plain go.mod.sha256:text/plain go.sum.sha256:text/plain source-provenance.txt:text/plain runtime-manifest-digest.txt:text/plain Dockerfile:text/plain SOURCE-BUILD.md:text/markdown)"' \
    'SOURCE_STATE="$(scripts/ci/check-oci-tag.sh "$SOURCE_IMAGE" "$SOURCE_DIGEST")"' \
    "source staging publication precedes conflict check"

  assert_contains "$PUBLISH_JOB" '^    outputs:$' "publish job exposes immutable outputs"
  assert_contains "$PUBLISH_JOB" 'runtime_digest:[[:space:]]+\$\{\{ steps\.push\.outputs\.digest \}\}' \
    "runtime job output comes from build-push digest"
  assert_contains "$PUBLISH_JOB" 'source_digest:[[:space:]]+\$\{\{ steps\.source\.outputs\.digest \}\}' \
    "source job output comes from ORAS digest step"
  assert_contains "$PUBLISH_LINES" '^ORAS_OUTPUT="\$\(oras push .*minio-source\.tar\.gz:application/vnd\.concord\.minio\.source\.v1\.tar\+gzip minio-source\.tar\.gz\.sha256:text/plain dependencies\.patch:application/vnd\.concord\.minio\.dependencies\.patch\.v1\+diff dependencies\.patch\.sha256:text/plain go\.mod\.sha256:text/plain go\.sum\.sha256:text/plain source-provenance\.txt:text/plain runtime-manifest-digest\.txt:text/plain Dockerfile:text/plain SOURCE-BUILD\.md:text/markdown\)"$' \
    "source push captures ORAS JSON and binds it to the staged runtime digest"
  assert_contains "$PUBLISH_LINES" '^SOURCE_DIGEST="\$\(jq -r '\''\.digest'\'' <<<"\$ORAS_OUTPUT"\)"$' \
    "source digest is parsed directly from ORAS output"
  assert_contains "$PUBLISH_LINES" '^echo "digest=\$SOURCE_DIGEST" >> "\$GITHUB_OUTPUT"$' \
    "source digest becomes a step output"

  assert_literal_count "$PUBLISH_LINES" 'RUNTIME_REF="${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}"' 2 \
    "amd64 smoke and module inspection use the pushed staging digest"
  assert_literal_count "$ARM_LINES" 'RUNTIME_REF="${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}"' 2 \
    "arm64 smoke and module inspection use the pushed staging digest"
  assert_contains "$PUBLISH_JOB" 'RUNTIME_DIGEST:[[:space:]]+\$\{\{ steps\.push\.outputs\.digest \}\}' \
    "amd64 smoke receives pushed runtime digest"
  expected_platform_line='ACTUAL_PLATFORMS="$(docker buildx imagetools inspect "$RUNTIME_REF" --raw | jq -r '\''.manifests[].platform | "\(.os)/\(.architecture)"'\'' | sort -u | paste -sd, -)"'
  expected_label_line='jq -e --arg commit "$MINIO_DERIVATIVE_COMMIT" --arg tag "$MINIO_DERIVATIVE_TAG" --arg upstream "$MINIO_COMMIT" --arg upstream_tag "$MINIO_TAG" '\''.["org.opencontainers.image.revision"] == $commit and .["org.opencontainers.image.version"] == $tag and .["io.concordvoice.minio.upstream-tag"] == $upstream_tag and .["io.concordvoice.minio.upstream-commit"] == $upstream and .["org.opencontainers.image.licenses"] == "AGPL-3.0-or-later" and .["org.opencontainers.image.source"] == "https://github.com/minio/minio" and .["org.opencontainers.image.created"] == "2025-10-15T17:29:55Z"'\'' <<<"$LABELS" >/dev/null'
  assert_sequence "$PUBLISH_LOGICAL" "amd64 version and platform checks consume pushed digest" \
    'RUNTIME_REF="${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}"' \
    'docker pull "$RUNTIME_REF"' \
    'test "$(docker image inspect "$RUNTIME_REF" --format '\''{{.Config.User}}'\'')" = "0"' \
    'LABELS="$(docker image inspect "$RUNTIME_REF" --format '\''{{json .Config.Labels}}'\'')"' \
    "$expected_label_line" \
    'VERSION_OUTPUT="$(docker run --rm --platform linux/amd64 "$RUNTIME_REF" --version)"' \
    'VERSION_LINE="$(printf '\''%s\n'\'' "$VERSION_OUTPUT" | sed -n '\''1p'\'')"' \
    'test "$VERSION_LINE" = "minio version $MINIO_DERIVATIVE_TAG (commit-id=$MINIO_DERIVATIVE_COMMIT)"' \
    "$expected_platform_line" \
    'test "$ACTUAL_PLATFORMS" = "linux/amd64,linux/arm64"'

  server_line='docker run -d --name "$SERVER_CONTAINER" --network "$NETWORK" --network-alias minio -p 127.0.0.1:19000:9000 -e MINIO_ROOT_USER -e MINIO_ROOT_PASSWORD --security-opt no-new-privileges:true --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID --mount "type=volume,source=$DATA_VOLUME,target=/data" "$RUNTIME_REF" server /data --console-address ":9001"'
  assert_literal_count "$PUBLISH_LOGICAL" "$server_line" 1 \
    "amd64 server starts pushed digest exactly once with production arguments"
  assert_literal_count "$PUBLISH_LOGICAL" 'curl --fail --silent --show-error http://127.0.0.1:19000/minio/health/live' 1 \
    "amd64 smoke checks liveness on digest-started server"
  assert_literal_count "$PUBLISH_LOGICAL" 'curl --fail --silent --show-error http://127.0.0.1:19000/minio/health/ready' 1 \
    "amd64 smoke checks readiness on digest-started server"
  assert_before "$PUBLISH_LOGICAL" "$server_line" \
    'curl --fail --silent --show-error http://127.0.0.1:19000/minio/health/live' \
    "server start precedes liveness"
  assert_before "$PUBLISH_LOGICAL" "$server_line" \
    'curl --fail --silent --show-error http://127.0.0.1:19000/minio/health/ready' \
    "server start precedes readiness"

  S3_BLOCK="$TMP/workflow.s3-smoke.sh"
  S3_FIRST='CURL_HELP="$(curl --help all)"'
  S3_LAST='test "$(sha256sum "$SMOKE_DIR/payload" | cut -d '\'' '\'' -f1)" = "$(sha256sum "$SMOKE_DIR/download" | cut -d '\'' '\'' -f1)"'
  assert_literal_count "$PUBLISH_LINES" "$S3_FIRST" 1 "S3 smoke feature-gates curl SigV4"
  assert_literal_count "$PUBLISH_LINES" "$S3_LAST" 1 "S3 smoke compares uploaded and downloaded hashes"
  awk -v first="$S3_FIRST" -v last="$S3_LAST" '$0 == first { active=1 } active { print } active && $0 == last { exit }' \
    "$PUBLISH_LINES" >"$TMP/workflow.s3-body"
  printf 'set -euo pipefail\n' >"$S3_BLOCK"
  cat "$TMP/workflow.s3-body" >>"$S3_BLOCK"
  assert_contains "$S3_BLOCK" '^CURL_HELP="\$\(curl --help all\)"$' "S3 smoke checks curl features before sending requests"
  assert_contains "$S3_BLOCK" '^grep -Fq -- '\''--aws-sigv4'\'' <<<"\$CURL_HELP"$' "S3 smoke refuses curl without SigV4 support"
  assert_contains "$S3_BLOCK" '^\(umask 077; printf '\''user = "%s:%s"\\n'\'' "\$MINIO_ROOT_USER" "\$MINIO_ROOT_PASSWORD" > "\$S3_CURL_CONFIG"\)$' \
    "S3 credentials are written under a restrictive umask"
  assert_contains "$S3_BLOCK" '^chmod 0600 "\$S3_CURL_CONFIG"$' "S3 credential config is owner-only"
  assert_contains "$S3_BLOCK" '^actual_status="\$\(curl --disable --fail --silent --show-error --aws-sigv4 '\''aws:amz:us-east-1:s3'\'' --config "\$S3_CURL_CONFIG" --output "\$output_path" --write-out '\''%\{http_code\}'\'' "\$@"\)"$' \
    "S3 requests use SigV4 and the protected config without credential arguments"
  assert_contains "$S3_BLOCK" '^test "\$actual_status" = "\$expected_status"$' "S3 smoke fails on an unexpected HTTP status"
  assert_not_contains "$S3_BLOCK" 'curl[^\n]*(MINIO_ROOT_USER|MINIO_ROOT_PASSWORD|--user|--password)' \
    "S3 credentials never appear in curl arguments"
  assert_sequence "$S3_BLOCK" "S3 lifecycle checks exact success statuses and payload parity" \
    's3_request 200 /dev/null --request PUT "$S3_ENDPOINT/$S3_BUCKET"' \
    's3_request 200 /dev/null --request PUT --data-binary "@$SMOKE_DIR/payload" "$S3_ENDPOINT/$S3_BUCKET/$S3_OBJECT"' \
    's3_request 200 /dev/null --head "$S3_ENDPOINT/$S3_BUCKET/$S3_OBJECT"' \
    's3_request 200 "$SMOKE_DIR/download" --request GET "$S3_ENDPOINT/$S3_BUCKET/$S3_OBJECT"' \
    's3_request 204 /dev/null --request DELETE "$S3_ENDPOINT/$S3_BUCKET/$S3_OBJECT"' \
    's3_request 204 /dev/null --request DELETE "$S3_ENDPOINT/$S3_BUCKET"' \
    "$S3_LAST"
  FAKE_CURL="$TMP/fake-curl"
  S3_RUN="$TMP/s3-smoke-fixture"
  FAKE_CURL_LOG="$TMP/s3-curl-requests.log"
  FAKE_CURL_ARGV_LOG="$TMP/s3-curl-argv.log"
  mkdir -p "$FAKE_CURL" "$S3_RUN"
  cat >"$FAKE_CURL/curl" <<'FAKECURL'
#!/usr/bin/env bash
if [[ "${1:-}" == --help && "${2:-}" == all ]]; then
  [[ "${FAKE_CURL_SIGV4:-1}" == 1 ]] && printf '%s\n' '--aws-sigv4'
  exit 0
fi
printf '%s\n' "$*" >>"$FAKE_CURL_ARGV_LOG"
[[ "$*" != *"$MINIO_ROOT_USER"* && "$*" != *"$MINIO_ROOT_PASSWORD"* ]] || exit 94
method=GET output= url= config=
while (($#)); do
  case "$1" in
    --request) method="$2"; shift 2 ;;
    --head) method=HEAD; shift ;;
    --output) output="$2"; shift 2 ;;
    --config) config="$2"; shift 2 ;;
    --data-binary|--aws-sigv4|--write-out) shift 2 ;;
    http://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[[ -n "$config" && -f "$config" ]] || exit 91
grep -Fq "user = \"$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD\"" "$config" || exit 92
printf '%s %s\n' "$method" "$url" >>"$FAKE_CURL_LOG"
if [[ "$method" == "${FAKE_CURL_FAIL_METHOD:-}" ]]; then exit 22; fi
case "$method $url" in
  'PUT http://127.0.0.1:19000/concord-smoke') status=200 ;;
  'PUT http://127.0.0.1:19000/concord-smoke/payload') status=200 ;;
  'HEAD http://127.0.0.1:19000/concord-smoke/payload') status=200 ;;
  'GET http://127.0.0.1:19000/concord-smoke/payload')
    status=200
    if [[ "${FAKE_CURL_HASH_MISMATCH:-0}" == 1 ]]; then printf 'different payload\n' >"$output"; else printf 'S3 fixture payload\n' >"$output"; fi
    ;;
  'DELETE http://127.0.0.1:19000/concord-smoke/payload') status=204 ;;
  'DELETE http://127.0.0.1:19000/concord-smoke') status=204 ;;
  *) exit 93 ;;
esac
if [[ "${FAKE_CURL_WRONG_DELETE_STATUS:-0}" == 1 && "$method" == DELETE ]]; then status=200; fi
printf '%s' "$status"
FAKECURL
  chmod +x "$FAKE_CURL/curl"
  printf 'S3 fixture payload\n' >"$S3_RUN/payload"
  run_s3_case() {
    local sigv4="$1" fail_method="$2" wrong_delete="$3" mismatch="$4" rc=0
    : >"$FAKE_CURL_LOG"
    : >"$FAKE_CURL_ARGV_LOG"
    if PATH="$FAKE_CURL:$PATH" FAKE_CURL_LOG="$FAKE_CURL_LOG" FAKE_CURL_ARGV_LOG="$FAKE_CURL_ARGV_LOG" FAKE_CURL_SIGV4="$sigv4" \
      FAKE_CURL_FAIL_METHOD="$fail_method" FAKE_CURL_WRONG_DELETE_STATUS="$wrong_delete" \
      FAKE_CURL_HASH_MISMATCH="$mismatch" SMOKE_DIR="$S3_RUN" \
      MINIO_ROOT_USER=fixture-user MINIO_ROOT_PASSWORD=fixture-password \
      bash "$S3_BLOCK" >"$TMP/s3-smoke.out" 2>&1; then :; else rc=$?; fi
    S3_RUN_RC="$rc"
  }
  run_s3_case 1 '' 0 0
  assert_equals "$S3_RUN_RC" 0 "extracted SigV4 S3 lifecycle succeeds with benign curl"
  assert_sequence "$FAKE_CURL_LOG" "extracted S3 lifecycle performs bucket and object operations in order" \
    'PUT http://127.0.0.1:19000/concord-smoke' \
    'PUT http://127.0.0.1:19000/concord-smoke/payload' \
    'HEAD http://127.0.0.1:19000/concord-smoke/payload' \
    'GET http://127.0.0.1:19000/concord-smoke/payload' \
    'DELETE http://127.0.0.1:19000/concord-smoke/payload' \
    'DELETE http://127.0.0.1:19000/concord-smoke'
  config_mode="$(stat -c '%a' "$S3_RUN/s3-curl.conf" 2>/dev/null || stat -f '%Lp' "$S3_RUN/s3-curl.conf" 2>/dev/null)"
  assert_equals "$config_mode" 600 "extracted S3 flow creates owner-only credentials"
  assert_contains "$FAKE_CURL_ARGV_LOG" -- '--disable --fail --silent --show-error --aws-sigv4 aws:amz:us-east-1:s3 --config .*s3-curl\.conf' \
    "extracted S3 requests pass SigV4 and protected config through curl argv"
  assert_not_contains "$FAKE_CURL_ARGV_LOG" 'fixture-password|fixture-user' "extracted S3 flow does not put credentials on curl argv"
  run_s3_case 0 '' 0 0
  assert_not_equals "$S3_RUN_RC" 0 "extracted S3 flow stops when curl lacks SigV4"
  assert_equals "$(wc -l <"$FAKE_CURL_LOG" | tr -d ' ')" 0 "unsupported curl sends no S3 requests"
  run_s3_case 1 GET 0 0
  assert_not_equals "$S3_RUN_RC" 0 "extracted S3 flow propagates a curl transport failure"
  assert_equals "$(wc -l <"$FAKE_CURL_LOG" | tr -d ' ')" 4 "transport failure stops before delete operations"
  run_s3_case 1 '' 1 0
  assert_not_equals "$S3_RUN_RC" 0 "extracted S3 flow rejects an unexpected HTTP status"
  assert_equals "$(wc -l <"$FAKE_CURL_LOG" | tr -d ' ')" 5 "unexpected delete status stops before bucket deletion"
  run_s3_case 1 '' 0 1
  assert_not_equals "$S3_RUN_RC" 0 "extracted S3 flow rejects downloaded payload hash mismatch"
  assert_equals "$(wc -l <"$FAKE_CURL_LOG" | tr -d ' ')" 6 "hash mismatch is checked after the complete transfer lifecycle"
  assert_sequence "$PUBLISH_LINES" "amd64 smoke verifies graceful stop" \
    'docker stop --time 30 "$SERVER_CONTAINER"' \
    'test "$(docker inspect --format '\''{{.State.ExitCode}}'\'' "$SERVER_CONTAINER")" = "0"'

  assert_contains "$ARM_JOB" 'RUNTIME_DIGEST:[[:space:]]+\$\{\{ needs\.publish\.outputs\.runtime_digest \}\}' \
    "native arm64 smoke receives staged digest"
  assert_sequence "$ARM_LOGICAL" "native arm64 version check consumes staged digest" \
    'RUNTIME_REF="${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}"' \
    'sudo docker pull "$RUNTIME_REF"' \
    'VERSION_OUTPUT="$(sudo docker run --rm "$RUNTIME_REF" --version)"' \
    'VERSION_LINE="$(printf '\''%s\n'\'' "$VERSION_OUTPUT" | sed -n '\''1p'\'')"' \
    'test "$VERSION_LINE" = "minio version $MINIO_DERIVATIVE_TAG (commit-id=$MINIO_DERIVATIVE_COMMIT)"'
  arm_server_line='sudo --preserve-env=MINIO_ROOT_USER,MINIO_ROOT_PASSWORD docker run -d --name minio-arm64-smoke -p 127.0.0.1:19001:9000 -e MINIO_ROOT_USER -e MINIO_ROOT_PASSWORD --security-opt no-new-privileges:true --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID "$RUNTIME_REF" server /data --console-address ":9001"'
  assert_literal_count "$ARM_LOGICAL" "$arm_server_line" 1 \
    "native arm64 starts staged digest with production security options"
  assert_literal_count "$ARM_LOGICAL" 'curl --fail --silent --show-error http://127.0.0.1:19001/minio/health/ready' 1 \
    "native arm64 checks readiness"
  assert_before "$ARM_LOGICAL" "$arm_server_line" \
    'curl --fail --silent --show-error http://127.0.0.1:19001/minio/health/ready' \
    "native arm64 start precedes readiness"

  assert_contains "$FINALIZE_JOB" 'RUNTIME_DIGEST:[[:space:]]+\$\{\{ needs\.publish\.outputs\.runtime_digest \}\}' \
    "finalizer receives the verified runtime digest"
  assert_contains "$FINALIZE_JOB" 'SOURCE_DIGEST:[[:space:]]+\$\{\{ needs\.publish\.outputs\.source_digest \}\}' \
    "finalizer receives the corresponding-source digest"
  assert_contains "$FINALIZE_LINES" '^oras pull --output "\$SOURCE_CHECK_DIR" "\$\{STAGING_SOURCE_IMAGE\}@\$\{SOURCE_DIGEST\}"$' \
    "finalizer fetches the source artifact by staged digest"
  assert_contains "$FINALIZE_LINES" '^test "\$\(cat "\$SOURCE_CHECK_DIR/runtime-manifest-digest\.txt"\)" = "\$RUNTIME_DIGEST"$' \
    "finalizer verifies the source artifact is bound to the runtime digest"
  assert_literal_count "$FINALIZE_LINES" 'oras cp "${STAGING_SOURCE_IMAGE}@${SOURCE_DIGEST}" "$SOURCE_IMAGE"' 1 \
    "finalizer promotes the matching source artifact only after native smokes"
  assert_literal_count "$FINALIZE_LINES" 'oras cp "${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}" "$RUNTIME_IMAGE"' 1 \
    "finalizer promotes the full runtime index only after native smokes"
  assert_literal_count "$FINALIZE_LINES" 'test "$(scripts/ci/check-oci-tag.sh "$SOURCE_IMAGE" "$SOURCE_DIGEST")" = "matching"' 1 \
    "finalizer proves the source stable tag matches the verified digest"
  assert_literal_count "$FINALIZE_LINES" 'test "$(scripts/ci/check-oci-tag.sh "$RUNTIME_IMAGE" "$RUNTIME_DIGEST")" = "matching"' 1 \
    "finalizer proves the runtime stable tag matches the verified digest"
  assert_sequence "$FINALIZE_LINES" "finalizer refuses a runtime-only stable publication" \
    'if [[ "$RUNTIME_STATE" == "matching" && "$SOURCE_STATE" == "absent" ]]; then' \
    'echo "::error::Runtime stable tag exists without corresponding source; stop and escalate"' \
    'exit 1' \
    'fi'
  assert_sequence "$FINALIZE_LINES" "source is promoted and verified before runtime publication" \
    'if [[ "$SOURCE_STATE" == "absent" ]]; then' \
    'oras cp "${STAGING_SOURCE_IMAGE}@${SOURCE_DIGEST}" "$SOURCE_IMAGE"' \
    'fi' \
    'test "$(scripts/ci/check-oci-tag.sh "$SOURCE_IMAGE" "$SOURCE_DIGEST")" = "matching"' \
    'if [[ "$RUNTIME_STATE" == "absent" ]]; then' \
    'oras cp "${STAGING_RUNTIME_IMAGE}@${RUNTIME_DIGEST}" "$RUNTIME_IMAGE"' \
    'fi' \
    'test "$(scripts/ci/check-oci-tag.sh "$RUNTIME_IMAGE" "$RUNTIME_DIGEST")" = "matching"'

  assert_contains "$FINALIZE_JOB" 'GITHUB_STEP_SUMMARY' "finalizer writes a workflow summary"
  assert_literal_count "$FINALIZE_LINES" 'printf '\''**Runtime digest:** `%s`\n'\'' "$RUNTIME_DIGEST" >> "$GITHUB_STEP_SUMMARY"' 1 \
    "summary writes runtime digest without shell evaluation"
  assert_literal_count "$FINALIZE_LINES" 'printf '\''**Source digest:** `%s`\n'\'' "$SOURCE_DIGEST" >> "$GITHUB_STEP_SUMMARY"' 1 \
    "summary writes source digest without shell evaluation"
  assert_contains "$FINALIZE_LINES" '^SAFE_REASON="\$\(printf.*cut -c1-500.*s/&/.*s/</.*s/>/' \
    "summary bounds and HTML-escapes the dispatcher reason"
  assert_contains "$FINALIZE_JOB" 'Initial package creation.*make the source package public' \
    "summary requires source-first public visibility"
  assert_contains "$FINALIZE_JOB" 'anonymous source pull by digest' \
    "summary requires anonymous source digest proof before runtime visibility"
  assert_contains "$FINALIZE_JOB" 'make the runtime package public' \
    "summary orders runtime visibility after source proof"
  assert_contains "$FINALIZE_JOB" 'anonymous runtime pull by digest' \
    "summary requires anonymous runtime digest proof"
  assert_contains "$FINALIZE_JOB" 'already-public packages.*legal approval must precede dispatch' \
    "summary gives the subsequent public-package legal gate"
fi

# OpenVEX contract. The jq program is exercised against a fixture built here and
# against single-fault mutants of it, so a pass is not an artefact of the real file.
MINIO_VEX="$ROOT/infrastructure/docker/minio/minio.openvex.json"
# The document is DIGEST-BOUND: a repository-wide product (no @sha256) made Trivy's
# suppression independent of the image, which was proven against the registry. Each
# statement names exactly the two measured published platform images, and nothing else.
MINIO_VEX_AMD64='pkg:oci/minio@sha256:3c38547c532e8436526725b8696a33efcdf61a79f88bcb2b5979050e41b5c44e?repository_url=ghcr.io/concord-voice/minio' # pragma: allowlist secret -- public OCI digest
MINIO_VEX_ARM64='pkg:oci/minio@sha256:c9aa69ce251b22a4e3a4c4cabd9f9e36f7019ef2778c140803ecc267846da25b?repository_url=ghcr.io/concord-voice/minio' # pragma: allowlist secret -- public OCI digest
MINIO_VEX_PRODUCTS="$MINIO_VEX_AMD64
$MINIO_VEX_ARM64"
MINIO_VEX_PAIRS='CVE-2026-40179|pkg:golang/github.com/prometheus/prometheus@v0.303.0
CVE-2026-42151|pkg:golang/github.com/prometheus/prometheus@v0.303.0
CVE-2026-42154|pkg:golang/github.com/prometheus/prometheus@v0.303.0
CVE-2026-44903|pkg:golang/github.com/prometheus/prometheus@v0.303.0
CVE-2026-81870|pkg:golang/go.opentelemetry.io/otel/sdk@v1.44.0
GO-2026-5932|pkg:golang/golang.org/x/crypto@v0.55.0'

# The contract is an allowlist: every field Trivy's go-vex matcher counts is either
# pinned or refused, because an unlisted field can widen suppression silently.
check_minio_vex() {
  jq -e --arg products "$MINIO_VEX_PRODUCTS" --arg pairs "$MINIO_VEX_PAIRS" '
    .["@context"] == "https://openvex.dev/ns/v0.2.0"
    and (.["@id"] | type == "string" and length > 0)
    and (.author | type == "string" and length > 0)
    and (.timestamp | type == "string" and length > 0)
    and (.statements | length) == 6
    and all(.statements[];
      (keys - ["vulnerability", "products", "status", "justification", "impact_statement"] | length) == 0
      and (.vulnerability | type == "object" and (keys == ["name"]))
      and .status == "not_affected"
      and ((.justification // "") as $j | [
        "vulnerable_code_not_present",
        "vulnerable_code_not_in_execute_path",
        "vulnerable_code_cannot_be_controlled_by_adversary",
        "inline_mitigations_already_exist",
        "component_not_present"] | index($j) != null)
      and ((.impact_statement // "") | type == "string" and length > 0
           and test("[0-9]{4}-[0-9]{2}-[0-9]{2}"))
      and (.products | type == "array" and length > 0)
      and all(.products[];
        type == "object"
        and (keys - ["@id", "subcomponents"] | length) == 0
        and (.["@id"] | type == "string"
             and test("^pkg:oci/minio@sha256:[0-9a-f]{64}\\?repository_url=ghcr\\.io/concord-voice/minio$"))
        and (.subcomponents | type == "array" and length > 0)
        and all(.subcomponents[];
          type == "object"
          and keys == ["@id"]
          and (.["@id"] | type == "string" and test("^pkg:golang/[^@?#]+@v[0-9][^@?#]*$"))))
      and ([.products[]["@id"]] | sort) == ($products | split("\n") | sort))
    and ([.statements[] | .vulnerability.name as $v
          | (.products // [])[] | (.subcomponents // [])[]
          | "\($v)|\(.["@id"])"] | unique) == ($pairs | split("\n") | sort)
  ' "$1" >/dev/null 2>&1
}

if ! command -v jq >/dev/null 2>&1; then
  fail "jq is required for the MinIO OpenVEX contract"
else
  assert_file "$MINIO_VEX" "MinIO OpenVEX document exists"
  if [[ -f "$MINIO_VEX" ]]; then
    check_minio_vex "$MINIO_VEX"
    assert_equals "$?" 0 "shipped MinIO OpenVEX document satisfies the VEX contract"
  fi

  VEX_FIXTURE_DIR="$TMP/vex-fixtures"
  mkdir -p "$VEX_FIXTURE_DIR"
  jq -n --arg products "$MINIO_VEX_PRODUCTS" '
    def stmt($cve; $subs): {
      vulnerability: {name: $cve},
      products: ($products | split("\n") | map({"@id": ., subcomponents: ($subs | map({"@id": .}))})),
      status: "not_affected",
      justification: "vulnerable_code_not_present",
      impact_statement: "The affected package is not linked into /usr/bin/minio in either listed platform image (symbol check 2026-10-01)."
    };
    {
      "@context": "https://openvex.dev/ns/v0.2.0",
      "@id": "https://example.invalid/vex/fixture",
      author: "fixture",
      timestamp: "2026-10-01T00:00:00Z",
      version: 1,
      statements: [
        stmt("CVE-2026-40179"; ["pkg:golang/github.com/prometheus/prometheus@v0.303.0"]),
        stmt("CVE-2026-42151"; ["pkg:golang/github.com/prometheus/prometheus@v0.303.0"]),
        stmt("CVE-2026-42154"; ["pkg:golang/github.com/prometheus/prometheus@v0.303.0"]),
        stmt("CVE-2026-44903"; ["pkg:golang/github.com/prometheus/prometheus@v0.303.0"]),
        stmt("CVE-2026-81870"; ["pkg:golang/go.opentelemetry.io/otel/sdk@v1.44.0"]),
        stmt("GO-2026-5932"; ["pkg:golang/golang.org/x/crypto@v0.55.0"])
      ]
    }' >"$VEX_FIXTURE_DIR/valid.json"
  check_minio_vex "$VEX_FIXTURE_DIR/valid.json"
  assert_equals "$?" 0 "VEX contract accepts a constructed valid document"

  # vex_mutant NAME JQ_PROGRAM: build NAME.json from valid.json with one edit.
  VEX_MUTANTS=""
  vex_mutant() {
    local name="$1"; shift
    jq "$@" "$VEX_FIXTURE_DIR/valid.json" >"$VEX_FIXTURE_DIR/$name.json"
    VEX_MUTANTS+="$name "
  }
  # Product identity: digest-bound, exactly the two measured platform images.
  vex_mutant versionless '.statements[0].products[0]["@id"] = "pkg:oci/minio?repository_url=ghcr.io/concord-voice/minio"'
  vex_mutant wrong-product '.statements[0].products[0]["@id"] = "pkg:oci/minio@sha256:3c38547c532e8436526725b8696a33efcdf61a79f88bcb2b5979050e41b5c44e?repository_url=ghcr.io/other/minio"'
  vex_mutant wrong-digest '.statements[0].products[0]["@id"] |= sub("3c38547c"; "4c38547c")'
  vex_mutant one-arch-only '.statements[0].products |= .[:1]'
  vex_mutant product-qualifier '.statements[0].products[0]["@id"] += "&x=y"'
  vex_mutant third-product '.statements[0].products += [(.statements[0].products[0] | .["@id"] = "pkg:oci/minio@sha256:\("a" * 64)?repository_url=ghcr.io/concord-voice/minio")]'
  vex_mutant duplicate-product '.statements[0].products += [.statements[0].products[0]]'
  # Document and statement shape.
  vex_mutant context-v010 '.["@context"] = "https://openvex.dev/ns/v0.1.0"'
  vex_mutant no-doc-id 'del(.["@id"])'
  vex_mutant no-author 'del(.author)'
  vex_mutant no-timestamp 'del(.timestamp)'
  vex_mutant affected '.statements[0].status = "affected"'
  vex_mutant bogus-justification '.statements[0].justification = "bogus"'
  vex_mutant no-justification 'del(.statements[0].justification)'
  vex_mutant action-statement '.statements[0].action_statement = "none"'
  vex_mutant vuln-string '.statements[0].vulnerability = "CVE-2026-42151"'
  vex_mutant empty-impact '.statements[0].impact_statement = ""'
  vex_mutant impact-number '.statements[0].impact_statement = 5'
  vex_mutant seventh-statement '.statements += [.statements[0]]'
  # Subcomponent shape and identity.
  vex_mutant versionless-sub '.statements[0].products[0].subcomponents[0]["@id"] = "pkg:golang/github.com/prometheus/prometheus"'
  vex_mutant non-golang-sub '.statements[0].products[0].subcomponents[0]["@id"] = "pkg:npm/lodash@4.17.21"'
  vex_mutant qualified-sub '.statements[0].products[0].subcomponents[0]["@id"] += "?arch=amd64"'
  # Length stays 6 and every shape rule still holds: ONLY the pairs rule can reject these.
  vex_mutant swapped-cve '.statements[5].vulnerability.name = "CVE-2099-0001"'
  vex_mutant extra-subcomponent '.statements[5].products |= map(.subcomponents += [{"@id": "pkg:golang/golang.org/x/crypto@v0.56.0"}])'
  # Mutants that widen what Trivy's go-vex matcher suppresses. The matcher counts
  # vulnerability @id/name/aliases, product and subcomponent identifiers/hashes,
  # and treats a product with no (or empty) subcomponents as matching on the
  # product alone, so each of these is a single-field edit the contract must refuse.
  # Edited in place, so the product set is intact and only the subcomponent rule fires.
  vex_mutant nosub-product 'del(.statements[0].products[0].subcomponents)'
  vex_mutant empty-subcomponents '.statements[0].products[0].subcomponents = []'
  vex_mutant vuln-aliases '.statements[0].vulnerability.aliases = ["CVE-2026-56855"]'
  vex_mutant vuln-atid '.statements[0].vulnerability["@id"] = "CVE-2026-56855"'
  vex_mutant sub-identifiers '.statements[0].products[0].subcomponents[0].identifiers = {purl: "pkg:golang/golang.org/x/crypto"}'
  vex_mutant sub-hashes '.statements[0].products[0].subcomponents[0].hashes = {"sha-256": "pkg:golang/golang.org/x/crypto@v0.57.0"}'
  vex_mutant product-identifiers '.statements[0].products[0].identifiers = {purl: "pkg:oci/minio"}'
  vex_mutant no-date '.statements[0].impact_statement = "The affected package is not linked into /usr/bin/minio."'
  for vex_mutant in $VEX_MUTANTS; do
    # A failed jq edit must not "reject" vacuously: the mutant has to be valid JSON
    # and has to differ from the document the contract accepts.
    if ! jq -e . "$VEX_FIXTURE_DIR/$vex_mutant.json" >/dev/null 2>&1 ||
        cmp -s "$VEX_FIXTURE_DIR/valid.json" "$VEX_FIXTURE_DIR/$vex_mutant.json"; then
      fail "VEX $vex_mutant mutant is vacuous (not valid JSON, or identical to valid.json)"
      continue
    fi
    check_minio_vex "$VEX_FIXTURE_DIR/$vex_mutant.json"
    assert_not_equals "$?" 0 "VEX contract rejects the $vex_mutant mutant"
  done
fi

if [[ $fails -eq 0 ]]; then
  echo "ALL PASS"
  exit 0
fi

echo "$fails FAILED"
exit 1
