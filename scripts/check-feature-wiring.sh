#!/bin/bash
# scripts/check-feature-wiring.sh
#
# Pre-commit + CI guard: every layer of a feature must be REACHED by the layer
# above it.
#
# WHY THIS EXISTS. PR #2157/2 shipped four separate components that were each
# complete, each unit-tested green, and each unreachable:
#
#   1. Session routes registered by a helper that router.go never called.
#   2. A chunked upload client with no caller in the upload path.
#   3. A capability-gated dispatch that gated the limit but not the path.
#   4. A server capability the client had read for weeks and no server ever sent.
#
# Every one passed its own tests. Unit tests prove a component WORKS; nothing in
# a unit test proves anyone CALLS it. The route-shape test was the sharpest
# example -- it built its own gin.New() engine, so it verified the helper against
# a router that existed only inside the test.
#
# Each assertion below is one call-site edge that no test can see, expressed as
# the cheapest thing that can detect its absence: a reference from the caller's
# source. This is a smoke alarm, not a type system -- it proves the name appears,
# not that the call is correct. That is deliberate: the failure mode it guards is
# ABSENCE, and absence is exactly what a grep can see.
#
# Adding an entry is encouraged whenever a seam has no test that spans it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

fail=0
CP=services/control-plane

# caller_file | symbol | what breaks when the edge is missing
# Self-Hosted: the provision-secrets.yml edges live with the managed deploy
# workflow, and the client/desktop edges in the Desktop repository. The public
# docker-compose.production.yml carries no managed billing settings, and the
# managed failover nginx (infrastructure/docker/nginx) does not ship here.
EDGES=(
  "docker-compose.production.yml|CLIENT_MIN_VERSION:|the control-plane never receives the client version floor"
  "$CP/pkg/config/config.go|LoadBillingConfig|billing settings are never loaded, so no boot guard runs and later PRs read an empty config"
  "$CP/cmd/server/main.go|cfg.Billing.LogBoot|the boot log never states the billing mode, key class or founders cutoff"
  "docker-compose.yml|STRIPE_SECRET_KEY:|the development control plane never receives a Stripe key"
  "$CP/internal/api/router.go|RegisterUploadSessionRoutes|the chunked upload session routes answer 404 on every deployment"
  "$CP/internal/api/router.go|SetChunkedAttachmentUpload|every client fails closed to the legacy single-shot upload path"
  "$CP/cmd/server/main.go|StartSessionSweepWorker|abandoned upload sessions are never reclaimed after a Redis loss"
  "$CP/internal/storage/registry.go|AttachmentWriteBackend|the attachment write selector never reaches the boot-time registry"
  "$CP/pkg/config/config.go|ATTACHMENT_WRITE_BACKEND|the attachment write selector is never loaded from configuration"
  "docker-compose.production.yml|ATTACHMENT_WRITE_BACKEND|the production attachment write selector is never passed to the control plane"
)

for edge in "${EDGES[@]}"; do
  IFS='|' read -r file symbol consequence <<< "$edge"
  if [[ ! -f "$file" ]]; then
    echo "check-feature-wiring: caller file is missing: $file" >&2
    fail=1
    continue
  fi
  # Comments mentioning the symbol would satisfy a bare grep, so strip the
  # obvious comment forms first. A determined false positive is still possible;
  # the guard is a floor, not a proof.
  #
  # NOT `grep -q` at the end of this pipeline: -q exits on the first match, which
  # closes the pipe, which SIGPIPEs the upstream grep, which under `pipefail`
  # fails the whole pipeline DESPITE the match succeeding. It is timing-dependent,
  # so it presents as a guard that randomly reports missing wiring -- worse than
  # no guard, because it teaches people to bypass it. Without -q, grep drains its
  # input and no SIGPIPE arises.
  #
  # [[:space:]] not \s: \s is a GNU extension that BSD/macOS grep reads as a
  # literal 's', so the comment strip would silently do nothing on a dev machine
  # while working in CI.
  if ! grep -v -E '^[[:space:]]*(//|\*|#)' "$file" | grep -F "$symbol" >/dev/null; then
    echo "check-feature-wiring: $file does not reference $symbol" >&2
    echo "    consequence: $consequence" >&2
    fail=1
  fi
done

require_in_block() {
  local file="$1" start="$2" end="$3" pattern="$4" consequence="$5" count
  count=$(sed -n "${start},$((end - 1))p" "$file" | grep -v -E '^[[:space:]]*(//|\*|#)' | grep -F -c "$pattern" || true)
  if (( count < 1 )); then
    echo "check-feature-wiring: $file block ${start}-${end} does not reference $pattern" >&2
    echo "    consequence: $consequence" >&2
    fail=1
  fi
}

require_count() {
  local file="$1" pattern="$2" expected="$3" consequence="$4" count
  count=$(grep -v -E '^[[:space:]]*(//|\*|#)' "$file" | grep -F -c "$pattern" || true)
  if (( count != expected )); then
    echo "check-feature-wiring: $file contains $count copies of $pattern (expected $expected)" >&2
    echo "    consequence: $consequence" >&2
    fail=1
  fi
}

# Two things, both about the log format that reaches a request. (1) Every
# server block sets its own access_log: without one, the base config's default
# format logs the raw request line, and that format is defined outside these
# files, so the "$request" guard above cannot see it. (2) Every access_log
# directive is either `off` or selects timed, timed_ws or admin_timed, the only
# formats defined with $log_uri; a server-level logger on any other format
# (combined, main) passes check (1) and writes bearer codes in clear.
require_server_access_logs() {
  local file="$1" missing bad
  missing=$(awk '/^server [{]/{s=1; h=0; next} s && /^    access_log /{h=1} s && /^[}]/{if(!h) n++; s=0} END{print n+0}' "$file")
  if (( missing != 0 )); then
    echo "check-feature-wiring: $file has $missing server block(s) with no server-level access_log" >&2
    echo "    consequence: those requests use the base config's default format, which logs bearer codes" >&2
    fail=1
  fi
  bad=$(grep -nE '^[[:space:]]*access_log[[:space:]]' "$file" | grep -vE 'access_log[[:space:]]+(off;|[^[:space:]]+[[:space:]]+(admin_)?timed(_ws)?([[:space:]]|;))' || true)
  if [[ -n "$bad" ]]; then
    echo "check-feature-wiring: $file has an access_log that does not use a redacted format (timed, timed_ws or admin_timed):" >&2
    printf '    %s\n' "$bad" >&2
    echo "    consequence: that log writes bearer codes in clear" >&2
    fail=1
  fi
}

require_count docs/api/openapi.yaml \
  "pattern: '^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$'" 2 \
  "the REST and WebSocket client-version declarations must share the exact parser contract"

ws_start=$(grep -n '^  /ws:$' docs/api/openapi.yaml | cut -d: -f1)
ws_end=$(grep -n '^components:$' docs/api/openapi.yaml | cut -d: -f1)
if [[ -n "$ws_start" && -n "$ws_end" ]]; then
  require_in_block docs/api/openapi.yaml "$ws_start" "$ws_end" \
    "'403':" "the WebSocket compatibility floor is undocumented"
  require_in_block docs/api/openapi.yaml "$ws_start" "$ws_end" \
    "\$ref: '#/components/responses/ClientVersionTooOld'" \
    "the WebSocket compatibility denial is not bound to its canonical response"
else
  echo "check-feature-wiring: could not locate the /ws OpenAPI block" >&2
  fail=1
fi

NGINX=infrastructure/deploy/nginx/concordvoice.conf
http_ws_nginx_start=$(grep -nF '    location ~ ^/api/v1/ws/?$ {' "$NGINX" | cut -d: -f1)
http_ws_nginx_end=$(awk '/^    location \/ \{/{ print NR; exit }' "$NGINX")
if [[ -n "$http_ws_nginx_start" && -n "$http_ws_nginx_end" ]]; then
  require_in_block "$NGINX" "$http_ws_nginx_start" "$http_ws_nginx_end" \
    'access_log /var/log/nginx/scanner.log timed_ws if=$is_scanner;' \
    "scanner HTTP-to-HTTPS WebSocket redirects can retain client_version query text"
  require_in_block "$NGINX" "$http_ws_nginx_start" "$http_ws_nginx_end" \
    'access_log /var/log/nginx/access.log timed_ws;' \
    "normal HTTP-to-HTTPS WebSocket redirects can retain client_version query text"
else
  echo "check-feature-wiring: could not locate the HTTP WebSocket nginx block" >&2
  fail=1
fi

ws_nginx_start=$(grep -n '^    location /api/v1/ws {' "$NGINX" | cut -d: -f1)
ws_nginx_end=$(grep -n '^    # --- Attachment uploads' "$NGINX" | cut -d: -f1)
if [[ -n "$ws_nginx_start" && -n "$ws_nginx_end" ]]; then
  require_in_block "$NGINX" "$ws_nginx_start" "$ws_nginx_end" \
    'access_log /var/log/nginx/scanner.log timed_ws if=$is_scanner;' \
    "scanner WebSocket logs can retain client_version query text"
  require_in_block "$NGINX" "$ws_nginx_start" "$ws_nginx_end" \
    'access_log /var/log/nginx/access.log timed_ws;' \
    "normal WebSocket logs can retain client_version query text"
else
  echo "check-feature-wiring: could not locate the WebSocket nginx block" >&2
  fail=1
fi
require_count "$NGINX" 'log_format timed_ws' 1 \
  "WebSocket requests must use a dedicated query-free log format"
require_count "$NGINX" '"$request_method $log_uri $server_protocol"' 2 \
  "both log formats must log the redacted, query-free URI (timed and timed_ws)"
require_count "$NGINX" '"$request"' 0 \
  "no log format may log the raw request line, which carries bearer codes and query text"
require_server_access_logs "$NGINX"

# The admin-console vhost is rendered into conf.d, which loads before
# sites-enabled, so it cannot use `timed` from concordvoice.conf: it defines
# admin_timed on the same $log_uri map variable.
ADMIN_TMPL=infrastructure/deploy/nginx/admin-console.conf.template
require_count "$ADMIN_TMPL" '"$request_method $log_uri $server_protocol"' 1 \
  "the admin vhost log format must log the redacted, query-free URI"
require_count "$ADMIN_TMPL" '"$request"' 0 \
  "no admin log format may log the raw request line, which carries bearer codes and query text"
require_server_access_logs "$ADMIN_TMPL"

if [[ $fail -ne 0 ]]; then
  echo "" >&2
  echo "A component with no caller is not a shipped feature. If an edge here is" >&2
  echo "genuinely obsolete, delete BOTH the edge and the code it was reaching." >&2
  exit 1
fi

echo "check-feature-wiring: ${#EDGES[@]} call-site edges present"
