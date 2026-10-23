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
EDGES=(
  ".github/workflows/provision-secrets.yml|vars.CLIENT_MIN_VERSION|the dormant client version floor is not sourced from GitHub public configuration"
  ".github/workflows/provision-secrets.yml|compose_dotenv_quote \"\${CLIENT_MIN_VERSION:-}\"|the client version floor can be re-interpolated by Compose dotenv parsing"
  ".github/workflows/provision-secrets.yml|emit_public_config CLIENT_MIN_VERSION|the client version floor is not emitted into the public deployment configuration"
  ".github/workflows/provision-secrets.yml|printf 'CLIENT_MIN_VERSION=%s\\n'|managed host writers omit the client version floor from deploy.env.local"
  ".github/workflows/provision-secrets.yml|,CLIENT_MIN_VERSION,|the SSH writer does not receive the client version floor"
  "docker-compose.production.yml|CLIENT_MIN_VERSION:|the control-plane never receives the client version floor"
  "$CP/pkg/config/config.go|LoadBillingConfig|billing settings are never loaded, so no boot guard runs and later PRs read an empty config"
  "$CP/cmd/server/main.go|cfg.Billing.LogBoot|the boot log never states the billing mode, key class or founders cutoff"
  ".github/workflows/provision-secrets.yml|bash [internal]check-billing-env.sh|a bad billing value reaches the host and crash-loops the control plane at its next restart"
  ".github/workflows/provision-secrets.yml|vars.BILLING_PURCHASE_MODE|the purchase-mode kill switch is not sourced from GitHub public configuration"
  "docker-compose.yml|STRIPE_SECRET_KEY:|the development control plane never receives a Stripe key"
  "docker-compose.production.yml|STRIPE_SECRET_KEY:|the production control plane never receives a Stripe key"
  "docker-compose.production.yml|BILLING_PURCHASE_MODE:|the production purchase-mode switch never reaches the control plane"
  "$CP/internal/api/router.go|RegisterUploadSessionRoutes|the chunked upload session routes answer 404 on every deployment"
  "$CP/internal/api/router.go|SetChunkedAttachmentUpload|every client fails closed to the legacy single-shot upload path"
  "$CP/cmd/server/main.go|StartSessionSweepWorker|abandoned upload sessions are never reclaimed after a Redis loss"
  "$CP/internal/storage/registry.go|AttachmentWriteBackend|the attachment write selector never reaches the boot-time registry"
  "$CP/pkg/config/config.go|ATTACHMENT_WRITE_BACKEND|the attachment write selector is never loaded from configuration"
  "docker-compose.production.yml|ATTACHMENT_WRITE_BACKEND|the production attachment write selector is never passed to the control plane"
  "client/desktop/src/renderer/hooks/messaging/useFileUpload.ts|uploadAttachmentChunked|the client never takes the chunked path, whatever the server advertises"
  "client/desktop/src/renderer/components/Chat/MessageInput.tsx|onCancel|an in-flight upload cannot be cancelled"
  "client/desktop/src/renderer/components/Purge/PurgeMessagesModal.tsx|StepUpCredentials|the DM purge step-up falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/PurgeFenceStepUpDialog.tsx|StepUpCredentials|turning purge protection off falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/DirectMessages/DMThreadRemovalDialog.tsx|StepUpCredentials|DM Clear falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/MFATierSelector.tsx|StepUpCredentials|the MFA method actions fall back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/MFASetup.tsx|StepUpCredentials|TOTP setup, key registration and recovery-key replace fall back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/EmailSmsSetup.tsx|StepUpCredentials|email setup falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/RecoveryCircle.tsx|StepUpCredentials|saving a Recovery Circle falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Chat/DeleteRefusalModal.tsx|StepUpCredentials|the delete soft-lock falls back to a code box the account may not be able to fill"
  "client/desktop/src/renderer/components/Settings/SettingsPage.tsx|SecuritySection|account protection has no reachable Settings pane"
  "client/desktop/src/renderer/components/Settings/SettingsPage.tsx|PrivacySection|privacy controls have no reachable Settings pane"
  "client/desktop/src/renderer/components/Settings/SecuritySection.tsx|SessionStepUpDialog|session revocation never reaches its credential dialog"
  "client/desktop/src/renderer/components/Settings/SecuritySection.tsx|BackupCodeRegenerateStage|backup-code regeneration never reaches its credential stage"
  "client/desktop/src/renderer/components/Settings/SessionStepUpDialog.tsx|stepUpRouteAdapters|the sessions routes' refusals are read by substring or not at all, and a near-miss string becomes a factor error"
  "client/desktop/src/renderer/components/Settings/BackupCodeRegenerateStage.tsx|stepUpRouteAdapters|the backup-code route's refusals are read by substring or not at all, and a near-miss string becomes a factor error"
  "client/desktop/src/renderer/hooks/auth/useStepUpFactor.ts|fetchStepUpRequirements|the step-up picker never learns which factors the account can use"
  "client/desktop/src/renderer/services/system/resetService.ts|useTotpAcceptedStore|the recently-used-code hint survives logout and an account switch"
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

WF=.github/workflows/provision-secrets.yml
require_count "$WF" 'VAR_ROOM_PLACEMENT_ENABLED: ${{ vars.ROOM_PLACEMENT_ENABLED }}' 1 \
  "room placement must be sourced from the GitHub environment variable"
require_count "$WF" 'case "$VAR_ROOM_PLACEMENT_ENABLED" in' 1 \
  "room placement must reject values other than empty, true or false"
require_count "$WF" 'emit_public_config ROOM_PLACEMENT_ENABLED "${VAR_ROOM_PLACEMENT_ENABLED:-false}"' 1 \
  "room placement must default to disabled in the canonical writer"
require_count "$WF" ',ROOM_PLACEMENT_ENABLED,' 1 \
  "the SSH writer must forward room placement to the host"
require_count "$WF" "printf 'ROOM_PLACEMENT_ENABLED=%s\\n'" 7 \
  "both runner and SSH writers must include room placement in their env artifacts"
require_count "$WF" 'VAR_MEDIA_DEPLOY_PROFILE: ${{ vars.MEDIA_DEPLOY_PROFILE }}' 1 \
  "the media profile must be sourced from the GitHub environment variable"
require_count "$WF" 'emit_public_config MEDIA_DEPLOY_PROFILE "${VAR_MEDIA_DEPLOY_PROFILE:-media-single}"' 1 \
  "the media profile must default to media-single in the canonical writer"
require_count "$WF" ',ROOM_PLACEMENT_ENABLED,MEDIA_DEPLOY_PROFILE,' 1 \
  "the SSH writer must forward the media profile to the host"
require_count "$WF" "printf 'MEDIA_DEPLOY_PROFILE=%s\\n'" 2 \
  "both production deploy.env.local writers must include the media profile"
runner_secrets_start=$(grep -n '^      - name: Build deploy\.env\.local$' "$WF" | cut -d: -f1)
runner_dotenv_start=$(grep -n '^      - name: Build \.env (docker-compose variables)$' "$WF" | cut -d: -f1)
runner_dotenv_end=$(grep -n '^      - name: Validate generated env file (runner-side, fail fast)$' "$WF" | cut -d: -f1)
ssh_writer_start=$(grep -n '^      - name: Upload to server via SSH$' "$WF" | cut -d: -f1)
ssh_writer_end=$(grep -n '^      - name: Summary$' "$WF" | cut -d: -f1)

for anchor in runner_secrets_start runner_dotenv_start runner_dotenv_end ssh_writer_start ssh_writer_end; do
  if [[ -z "${!anchor}" ]]; then
    echo "check-feature-wiring: missing workflow writer anchor: $anchor" >&2
    fail=1
  fi
done

if [[ -n "$runner_secrets_start" && -n "$runner_dotenv_start" ]]; then
  require_in_block "$WF" "$runner_secrets_start" "$runner_dotenv_start" \
    "printf 'CLIENT_MIN_VERSION=%s\\n'" \
    "the runner-side deploy.env.local writer omits the client version floor"
fi
if [[ -n "$runner_dotenv_start" && -n "$runner_dotenv_end" ]]; then
  require_in_block "$WF" "$runner_dotenv_start" "$runner_dotenv_end" \
    '_q=$(compose_dotenv_quote "${CLIENT_MIN_VERSION:-}")' \
    "the runner-side Compose dotenv writer can re-interpolate the client version floor"
fi
if [[ -n "$ssh_writer_start" && -n "$ssh_writer_end" ]]; then
  require_in_block "$WF" "$ssh_writer_start" "$ssh_writer_end" \
    "printf 'CLIENT_MIN_VERSION=%s\\n'" \
    "the SSH-side deploy.env.local writer omits the client version floor"
  require_in_block "$WF" "$ssh_writer_start" "$ssh_writer_end" \
    '_q=$(compose_dotenv_quote "${CLIENT_MIN_VERSION:-}")' \
    "the SSH-side Compose dotenv writer can re-interpolate the client version floor"
fi

require_count "$WF" '_q=$(compose_dotenv_quote "${CLIENT_MIN_VERSION:-}")' 2 \
  "both managed Compose dotenv writers must quote the client version floor"
require_count "$WF" 'CLIENT_MIN_VERSION=${CLIENT_MIN_VERSION:-}' 0 \
  "the client version floor must not be emitted raw into a Compose dotenv writer"

# Stripe billing (#2033): each of the six names is written by both
# deploy.env.local writers, quoted by both Compose dotenv writers, and passed
# to the SSH step, whose set -u aborts on a name missing from envs:.
billing_deploy_template="printf 'NAME=%s\\n' \"\${NAME@Q}\""
billing_dotenv_template='_q=$(compose_dotenv_quote "${NAME:-}")'
for billing_var in STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET BILLING_PURCHASE_MODE \
  BILLING_PURCHASE_ALLOWLIST FOUNDERS_PRICING_ENDS_AT STRIPE_AUTOMATIC_TAX; do
  require_count "$WF" "${billing_deploy_template//NAME/$billing_var}" 2 \
    "a deploy.env.local writer omits ${billing_var}"
  require_count "$WF" "${billing_dotenv_template//NAME/$billing_var}" 2 \
    "a Compose dotenv writer omits ${billing_var} or lets Compose re-interpolate it"
  require_count "$WF" ",${billing_var}," 1 \
    "the SSH step's envs: list omits ${billing_var}, so set -u aborts the server-side write"
done

NGINX=[internal]nginx/concordvoice.conf
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

FAILOVER_NGINX=infrastructure/docker/nginx/nginx.conf
failover_ws_start=$(grep -n '^    location /api/v1/ws {' "$FAILOVER_NGINX" | cut -d: -f1)
failover_ws_end=$(grep -n '^    # Attachment uploads' "$FAILOVER_NGINX" | cut -d: -f1)
if [[ -n "$failover_ws_start" && -n "$failover_ws_end" ]]; then
  require_in_block "$FAILOVER_NGINX" "$failover_ws_start" "$failover_ws_end" \
    'access_log /var/log/nginx/access.log timed_ws;' \
    "failover WebSocket logs can retain client_version query text"
else
  echo "check-feature-wiring: could not locate the failover WebSocket nginx block" >&2
  fail=1
fi
require_count "$FAILOVER_NGINX" 'log_format timed_ws' 1 \
  "failover WebSocket requests must use a dedicated query-free log format"
require_count "$FAILOVER_NGINX" '"$request_method $log_uri $server_protocol"' 1 \
  "the failover log format must log the redacted, query-free URI"
require_count "$FAILOVER_NGINX" '"$request"' 0 \
  "no failover log format may log the raw request line, which carries bearer codes and query text"
require_server_access_logs "$FAILOVER_NGINX"

# The admin-console vhost is rendered into conf.d, which loads before
# sites-enabled, so it cannot use `timed` from concordvoice.conf: it defines
# admin_timed on the same $log_uri map variable.
ADMIN_TMPL=[internal]nginx/admin-console.conf.template
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
