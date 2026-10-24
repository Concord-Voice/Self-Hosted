#!/usr/bin/env bash
# scripts/check-compose-manifest-coverage.sh
#
# Tier A — production-compose manifest-coverage lint
# Asserts every local build.context and build.additional_contexts value in
# docker-compose.{yml,production.yml} is present (as exact path OR via parent)
# in deploy-manifest.txt.
#
# Matching semantics: exact-or-parent. A child path does NOT satisfy a
# parent context (e.g., manifest entry 'services/control-plane/migrations'
# does NOT satisfy build context 'services/control-plane' — pre-#878 bug).
#
# Usage:
#   ./scripts/check-compose-manifest-coverage.sh                          # default mode (defensive)
#   ./scripts/check-compose-manifest-coverage.sh /path/to/manifest        # override manifest path
#   ./scripts/check-compose-manifest-coverage.sh --scope=production-active # strict mode
#
# Exit codes:
#   0 — all production-active contexts found (WARNs allowed)
#   1 — at least one production-active context missing
#
# See docs/superpowers/specs/2026-05-29-1246-compose-manifest-coverage-lint-design.md

set -euo pipefail

# Resolve REPO_ROOT — works whether invoked from anywhere
REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || pwd)

# Default manifest path
MANIFEST="${REPO_ROOT}/infrastructure/deploy/deploy-manifest.txt"

# Default scope: 'all' (defensive — profile-gated services produce WARN)
SCOPE="all"

# Positional + flag parsing
for arg in "$@"; do
    case "$arg" in
        --scope=production-active) SCOPE="production-active" ;;
        --scope=all) SCOPE="all" ;;
        --scope=*)
            echo "::error::Unknown --scope value: '${arg#--scope=}' (expected 'all' or 'production-active')" >&2
            exit 2
            ;;
        -*)
            echo "::error::Unknown flag: $arg" >&2
            exit 2
            ;;
        *)
            MANIFEST="$arg"
            ;;
    esac
done

# Profile-gated build contexts: these are declared in docker-compose.yml under
# a non-default profile (e.g., 'licensing') and are NOT active in the
# production 'services' profile. Default mode produces ::warning:: for missing
# manifest entries here; --scope=production-active silent-skips them entirely.
#
# Maintenance: when adding a new profile-gated build context, append the
# normalized path here. If this list grows past ~5 entries, revisit and
# consider proper YAML-based profile detection (spec §3.4 deferred YAGNI).
#
# Currently EMPTY. The sole member was services/licensing-authority, removed
# in #1785 along with its stub service. The mechanism is deliberately kept:
# it is the extension point for the next profile-gated context (self-hosted
# licensing was cancelled in #210; no successor is planned), and its behavior stays
# covered by scripts/test-check-compose-manifest-coverage.sh against a
# synthetic fixture context — though note that coverage is currently vacuous
# and the mechanism itself is unreachable, tracked in:
#   https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2885
PROFILE_GATED_CONTEXTS=()

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
    echo "::error::docker compose is required to inspect build contexts" >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "::error::jq is required to inspect rendered Compose JSON" >&2
    exit 1
fi

# is_profile_gated <context>: returns 0 if the context is in PROFILE_GATED_CONTEXTS, 1 otherwise.
is_profile_gated() {
    local ctx="$1"
    local g
    for g in "${PROFILE_GATED_CONTEXTS[@]}"; do
        if [ "$ctx" = "$g" ]; then
            return 0
        fi
    done
    return 1
}

# compose_contexts <file>: render the Compose stack ending in <file> in an
# isolated environment, then emit every primary/named context. Production is
# an override, so it is rendered with the base file. Fixed placeholders satisfy
# required interpolation without reading deployment secrets or the project .env.
# Avoid newer no-resolution flags because CI may ship an older Compose CLI;
# absolute local paths from its default rendering are normalized below.
compose_contexts() {
    local compose_file="$1"
    local rendered
    local placeholder="manifest-check-not-a-secret"
    local -a compose_files=(-f "$compose_file")
    if [ "$compose_file" = "${REPO_ROOT}/docker-compose.production.yml" ]; then
        compose_files=(
            -f "${REPO_ROOT}/docker-compose.yml"
            -f "$compose_file"
        )
    fi
    local -a compose_env=(
        "PATH=$PATH"
        "HOME=${HOME:-/tmp}"
        "DOMAIN=manifest-check.invalid"
        "FEEDBACK_PAT=$placeholder"
        "FEEDBACK_REPO=example/example"
        "JWT_SECRET=$placeholder"
        "MFA_ENCRYPTION_KEY=$placeholder"
        "MINIO_ROOT_PASSWORD=$placeholder"
        "MINIO_ROOT_USER=manifest-check"
        "POSTGRES_PASSWORD=$placeholder"
        "PUBLIC_IP=127.0.0.1"
        "REDIS_PASSWORD=$placeholder"
        "SMTP_HOST=smtp.manifest-check.invalid"
        "SMTP_FROM=manifest-check@invalid.example"
        "TRUSTED_PROXY_CIDRS=127.0.0.1/32"
        "TURN_SECRET=$placeholder"
    )
    if ! rendered=$(env -i "${compose_env[@]}" docker compose \
        --env-file /dev/null \
        --project-directory "$REPO_ROOT" \
        "${compose_files[@]}" \
        config \
        --format json); then
        echo "::error::Unable to parse $(basename "$compose_file") with docker compose config" >&2
        return 1
    fi

    jq -r '
        .services[]?.build? |
        if type == "string" then .
        elif type == "object" then
            (.context // empty),
            ((.additional_contexts // {}) |
                if type == "object" then .[]
                elif type == "array" then .[] | sub("^[^=]+="; "")
                else empty
                end)
        else empty
        end
    ' <<< "$rendered"
}

# Buildx accepts non-filesystem named contexts (images, Git/HTTP URLs, and
# sibling services). They are resolved remotely and are not deploy-manifest
# inputs, so only plain filesystem values enter the coverage matcher.
is_local_context() {
    local ctx="$1"
    case "$ctx" in
        ""|*://*|service:*|target:*|docker-image:*|git:*|ssh:*) return 1 ;;
        *) return 0 ;;
    esac
}

# Compose files to scan
COMPOSE_FILES=(
    "${REPO_ROOT}/docker-compose.yml"
    "${REPO_ROOT}/docker-compose.production.yml"
)

# Graceful degradation: missing manifest file
if [ ! -f "$MANIFEST" ]; then
    echo "WARN: manifest file $MANIFEST not found; skipping check" >&2
    exit 0
fi

# Graceful degradation: manifest exists but is not readable (e.g., chmod 000).
# Without this check, awk's "can't open file" error would propagate as exit 2
# via set -e — opaque to a CI consumer that can't distinguish this from a
# flag-parsing rejection. Surface as a script-controlled WARN + exit 0 instead.
if [ ! -r "$MANIFEST" ]; then
    echo "WARN: manifest file $MANIFEST exists but is not readable; skipping check" >&2
    exit 0
fi

# Graceful degradation: manifest with no parseable entries
# (e.g., empty file, comments-only). The case-statement match has
# nothing to compare against — every context would be reported as
# missing, which is misleading. Bail with WARN instead.
#
# awk-based count handles empty input cleanly. Using grep -v under
# `set -o pipefail` is foot-gun-prone: grep returns 1 when nothing
# matches, killing the pipeline before wc runs.
PARSEABLE_LINES=$(awk '!/^[[:space:]]*(#|$)/{n++} END{print n+0}' "$MANIFEST")
if [ "$PARSEABLE_LINES" = "0" ]; then
    echo "WARN: manifest $MANIFEST has no parseable entries (empty or comments-only); skipping check" >&2
    exit 0
fi

MISSING=0
WARNED=0
SEEN_CONTEXTS=()
for f in "${COMPOSE_FILES[@]}"; do
    if [ ! -f "$f" ]; then
        echo "WARN: compose file $f not found; skipping" >&2
        continue
    fi
    if ! contexts=$(compose_contexts "$f"); then
        MISSING=$((MISSING + 1))
        continue
    fi
    while IFS= read -r ctx; do
        [ -z "$ctx" ] && continue
        is_local_context "$ctx" || continue
        ctx_normalized="${ctx#"$REPO_ROOT"/}"
        ctx_normalized="${ctx_normalized#./}"
        ctx_normalized="${ctx_normalized%/}"
        already_seen="no"
        for seen_context in "${SEEN_CONTEXTS[@]+"${SEEN_CONTEXTS[@]}"}"; do
            if [ "$seen_context" = "$ctx_normalized" ]; then
                already_seen="yes"
                break
            fi
        done
        [ "$already_seen" = "yes" ] && continue
        SEEN_CONTEXTS+=("$ctx_normalized")
        matched="no"
        while IFS= read -r manifest_entry; do
            manifest_entry="${manifest_entry%%#*}"
            # Pure-bash strip (eliminates per-line subshell — Gitar perf finding).
            manifest_entry="${manifest_entry// /}"
            # Strip CR (defense-in-depth for Windows-edited manifests with CRLF).
            manifest_entry="${manifest_entry//$'\r'/}"
            [ -z "$manifest_entry" ] && continue
            case "$ctx_normalized" in
                "$manifest_entry"|"$manifest_entry"/*) matched="yes"; break ;;
            esac
        done < "$MANIFEST"
        if [ "$matched" = "no" ]; then
            if is_profile_gated "$ctx_normalized"; then
                if [ "$SCOPE" = "production-active" ]; then
                    # Silent skip: --scope=production-active narrows the surface
                    # to production-active services only. Profile-gated services
                    # aren't checked at all in this mode.
                    :
                else
                    echo "::warning::Profile-gated context '$ctx_normalized' (from $(basename "$f")) is not in $(basename "$MANIFEST"). Defensive coverage gap; would block deploy if licensing profile activates."
                    WARNED=$((WARNED + 1))
                fi
            else
                echo "::error::Context path '$ctx_normalized' (from $(basename "$f")) is not in $(basename "$MANIFEST")"
                MISSING=$((MISSING + 1))
            fi
        fi
    done <<< "$contexts"
done

if [ "$MISSING" -gt 0 ]; then
    echo "::error::$MISSING build-context path(s) missing from $(basename "$MANIFEST")"
    exit 1
fi
if [ "$WARNED" -gt 0 ]; then
    # Distinguish from the all-clear case so a developer skimming stdout
    # doesn't read "all present" while a WARN sits unread on stderr.
    echo "Tier A lint: all production-active contexts present in $(basename "$MANIFEST") (${WARNED} profile-gated WARN(s) emitted — see stderr)"
else
    echo "Tier A lint: all production-compose build contexts present in $(basename "$MANIFEST")"
fi
