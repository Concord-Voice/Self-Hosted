#!/bin/bash
# test-detect-secrets.sh — regression tests for hooks/detect-secrets.sh
# No framework — check() harness. Sensitive-looking inputs are assembled from
# fragments at runtime so this test file contains no literal PEM header or
# credential (which would trip other scanners on this repo).
#
# Founding regression (2026-07-15): the private-key pattern begins with '-----',
# so `grep -qE "$pattern"` parsed it as options ("grep: unrecognized option").
# Fixed with `grep -qE -e "$pattern"` (+ printf instead of echo). Lock it here.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
HOOK="$HERE/detect-secrets.sh"
fail=0; pass=0
check() { if eval "$2"; then pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

# run <json-prompt> -> captures combined output in $OUT, exit code in $RC
# shellcheck disable=SC2034 # OUT/RC are read by the eval'd check expressions
run() { OUT="$(printf '%s' "$1" | bash "$HOOK" 2>&1)"; RC=$?; }

# 1. Benign prose mentioning db engines → PASS (exit 0)
run '{"prompt":"please fix the postgres database connection timeout in prod"}'
check "benign prose passes (exit 0)"                '[ "$RC" -eq 0 ]'

# 2. Private-key header → BLOCK (exit 2) AND no grep option-parse error.
B=BEGIN; K='PRIVATE KEY'; HDR="-----${B} RSA ${K}-----"
run "$(printf '{"prompt":"%s"}' "$HDR")"
check "private-key header blocks (exit 2)"          '[ "$RC" -eq 2 ]'
check "no grep 'unrecognized option' error"         '! printf "%s" "$OUT" | grep -qi "unrecognized option"'

# 3. Connection string with embedded creds → BLOCK (exit 2)
U=user; P=pass; URL="postgres://${U}:${P}@host:5432/db"
run "$(printf '{"prompt":"connect via %s"}' "$URL")"
check "credentialed connection string blocks"       '[ "$RC" -eq 2 ]'

# 4. Bare engine words (no ://user:pass@) → PASS (exit 0) — no false positive
run '{"prompt":"matched pattern (postgres|mysql|redis|mongodb) unrecognized option"}'
check "bare pattern words pass (no false positive)" '[ "$RC" -eq 0 ]'

# 5. Empty prompt → PASS (exit 0)
run '{"prompt":""}'
check "empty prompt passes (exit 0)"                '[ "$RC" -eq 0 ]'

# ── DB-URI userinfo anchoring (2026-07-31) ───────────────────────────────────
# Founding regression: the userinfo groups were '[^:]+' and '[^@]+', which
# exclude only ':' and '@'. grep is line-oriented, so those groups could span
# spaces and slashes — any bare "scheme://host:port" plus ANY later '@' on the
# same line matched. An @mention or an email address next to a connection URL
# blocked ordinary prompts. Cases 6 and 7 lock the false positives out; case 8
# locks a true positive the old '+' quantifier missed entirely.

# 6. Bare host:port + an unrelated @mention → PASS (exit 0)
run '{"prompt":"redis://localhost:6379 is healthy, ask @markd to confirm"}'
check "host:port + @mention passes"                 '[ "$RC" -eq 0 ]'

# 7. Bare host:port with a path + an email later in the line → PASS (exit 0)
run '{"prompt":"postgres://localhost:5432/example-org is up — ping ops@example.com"}'
check "host:port + email passes"                    '[ "$RC" -eq 0 ]'

# 8. Empty-userinfo credential URI (redis://:secret@host) → BLOCK (exit 2).
# The pre-fix '[^:]+' required >=1 char before the colon, so this real
# credential shape sailed through. Assembled from fragments, per the file header.
P8=s3cret; URL8="redis://:${P8}@localhost:6379"
run "$(printf '{"prompt":"set REDIS_URL to %s"}' "$URL8")"
check "empty-userinfo credential URI blocks"        '[ "$RC" -eq 2 ]'

# 9. Credential URI mid-sentence → BLOCK (exit 2) — tightening must not
# require the URI to start the line or be surrounded by whitespace.
U9='admin'; P9=pw123; URL9="mysql://${U9}:${P9}@10.0.0.5/main"
run "$(printf '{"prompt":"please set it to %s before deploying"}' "$URL9")"
check "mid-sentence credential URI blocks"          '[ "$RC" -eq 2 ]'

echo "── ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
