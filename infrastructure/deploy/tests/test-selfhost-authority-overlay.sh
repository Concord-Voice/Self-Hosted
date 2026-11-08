#!/usr/bin/env bash
# Tests for docker-compose.authority.yml through test-selfhost-authority-overlay-check.py
# (DoR D-173 contract (a)-(e)). CI-run via pr-ci.yml's
# infrastructure/deploy/tests/test-*.sh loop. The render needs `docker compose`
# but no daemon.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
CHECKER="$HERE/test-selfhost-authority-overlay-check.py"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
PASS=0; FAIL=0
ok()  { echo "PASS: $1"; PASS=$((PASS+1)); }
bad() { echo "FAIL: $1"; FAIL=$((FAIL+1)); }

if ! command -v jq >/dev/null 2>&1; then
  bad "jq is required to build the mutants"
  echo "passed: $PASS  failed: $FAIL"; exit 1
fi
if ! env -i PATH="$PATH" HOME="$HOME" docker compose version >/dev/null 2>&1; then
  if [[ "${CI:-}" == true ]]; then
    bad "docker compose is required in CI to render the overlay"
    echo "passed: $PASS  failed: $FAIL"; exit 1
  fi
  echo "SKIP: docker compose not installed"; exit 0
fi

# Dummy env, keys joined with printf so no KEY=value literal sits in source.
envline() { printf '%s=%s\n' "$1" "$2"; }
{
  envline DOMAIN t.test
  envline PUBLIC_IP 203.0.113.10
  envline FEEDBACK_REPO a/b
  envline SMTP_HOST smtp.t.test
  envline SMTP_FROM fixture@t.test
  envline TRUSTED_PROXY_CIDRS 10.0.0.0/8
  envline WS_EVENT_BUDGET_ENABLED true
  envline WS_EVENT_BUDGET_SCALE_PERCENT 100
  for k in "POSTGRES_PASS""WORD" "REDIS_PASS""WORD" "JWT_SEC""RET" "TURN_SEC""RET" \
           "MINIO_ROOT_USER" "MINIO_ROOT_PASS""WORD" "FEEDBACK_PAT" "MFA_ENCRYPTION_KEY"; do
    envline "$k" ph
  done
} > "$WORK/dummy.env"

# Render a copy of the sources, with every profile the stack uses on, so a
# profile-gated service cannot hide from the rules.
R="$WORK/root"; mkdir -p "$R"
cp "$ROOT"/docker-compose.yml "$ROOT"/docker-compose.production.yml \
  "$ROOT"/docker-compose.selfhost.yml "$ROOT"/docker-compose.authority.yml "$R/"
: >"$R/spa.env"
if (cd "$R" && env -i PATH="$PATH" HOME="$HOME" docker compose --project-directory "$R" \
    --env-file "$WORK/dummy.env" -f docker-compose.yml -f docker-compose.production.yml \
    -f docker-compose.selfhost.yml -f docker-compose.authority.yml \
    --profile services --profile authority-owner config --format json) >"$WORK/real.json" 2>"$WORK/err"; then
  ok "the self-host stack renders with the authority overlay"
else
  bad "the render failed: $(head -c 400 "$WORK/err")"
  echo "passed: $PASS  failed: $FAIL"; exit 1
fi

if out="$(python3 "$CHECKER" <"$WORK/real.json")"; then
  ok "the rendered overlay meets (a)-(e)"
else
  bad "the rendered overlay breaks the contract: $out"
fi

# Each mutant breaks one rule; the checker must refuse it and name the letter.
mutant() { # NAME LETTER JQ
  local out
  jq "$3" "$WORK/real.json" >"$WORK/m.json"
  if out="$(python3 "$CHECKER" <"$WORK/m.json")"; then
    bad "$1: accepted"
  elif grep -qF "($2)" <<<"$out"; then
    ok "$1: refused as ($2)"
  else
    bad "$1: refused without ($2): $out"
  fi
}
mutant "owner URL on authority" a '.services.authority.environment.AUTHORITY_MIGRATE_DATABASE_URL = "x"'
mutant "owner passfile on authority" a '.services.authority.secrets += [{"source":"authority_owner_pgpass"}]'
mutant "owner role in authority URL" a '.services.authority.environment.AUTHORITY_DATABASE_URL |= sub("authority_app"; "authority_owner")'
mutant "owner job without profile" b 'del(.services["authority-owner"].profiles)'
mutant "owner job restarts" b '.services["authority-owner"].restart = "unless-stopped"'
mutant "owner job holds the RSK" c '.services["authority-owner"].secrets += [{"source":"authority_rsk"}]'
mutant "CP holds the TLS key" c '.services["control-plane"].secrets = [{"source":"authority_tls_key"}]'
mutant "CP carries an AUTHORITY_ variable" d '.services["control-plane"].environment.AUTHORITY_TRUST_PATH = "/x"'
mutant "media plane holds an authority secret" d '.services["media-plane"].secrets = [{"source":"authority_app_pgpass"}]'
mutant "CP postgres mounts the authority socket" e '.services.postgres.volumes += [{"type":"volume","source":"authority-pg-socket","target":"/run/x"}]'
mutant "authority postgres on a network" e 'del(.services["authority-postgres"].network_mode)'
mutant "authority postgres listens on TCP" e '.services["authority-postgres"].command |= map(if . == "listen_addresses=" then "listen_addresses=*" else . end)'
mutant "authority postgres runs the CP wrapper" e 'del(.services["authority-postgres"].entrypoint)'

if [[ "$(printf 'not json' | python3 "$CHECKER")" == "FAIL: the rendered model is not JSON" ]]; then
  ok "a non-JSON render is refused"
else
  bad "a non-JSON render was not refused"
fi
f=docker-compose.authority.yml
if grep -qx "$f" "$ROOT/infrastructure/deploy/deploy-manifest.txt"; then
  ok "deploy-manifest.txt lists $f"
else
  bad "deploy-manifest.txt does not list $f"
fi
if grep -qE '^docker-compose\.authority\.yml[[:space:]]+@Concord-Voice/security' "$ROOT/CODEOWNERS"; then
  ok "CODEOWNERS covers $f"
else
  bad "CODEOWNERS has no $f entry owned by @Concord-Voice/security"
fi

echo "passed: $PASS  failed: $FAIL"
[[ "$FAIL" -eq 0 ]]
