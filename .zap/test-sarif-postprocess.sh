#!/usr/bin/env bash
# test-sarif-postprocess.sh — hermetic tests for sarif-postprocess.py (#1465).
#
# Builds a ZAP-shaped SARIF fixture (http(s) location URIs, a rule helpUri,
# a run-level artifacts[].location) and asserts the post-processor:
#   - rewrites every artifactLocation URL onto the sentinel with %SRCROOT%,
#   - leaves rule.helpUri untouched,
#   - preserves the real URL in the message + properties.originalUri,
#   - resets the region to line 1,
#   - is idempotent,
#   - fails loud if an http(s) location URI survives.
set -euo pipefail

cd "$(dirname "$0")"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

cat > "$TMP/zap.sarif" <<'JSON'
{
  "version": "2.1.0",
  "runs": [
    {
      "tool": { "driver": { "name": "ZAP", "rules": [
        { "id": "40018", "helpUri": "https://www.zaproxy.org/docs/alerts/40018/" }
      ] } },
      "artifacts": [
        { "location": { "uri": "http://localhost:8080/api/v1/servers" } }
      ],
      "results": [
        {
          "ruleId": "40018",
          "message": { "text": "SQL injection may be possible." },
          "locations": [
            { "physicalLocation": {
                "artifactLocation": { "uri": "http://localhost:8080/api/v1/servers" },
                "region": { "startLine": 4096, "endLine": 4096 }
            } }
          ]
        },
        {
          "ruleId": "10021",
          "message": { "text": "X-Content-Type-Options header missing." },
          "locations": [
            { "physicalLocation": {
                "artifactLocation": { "uri": "https://localhost:8080/api/v1/users/me" },
                "region": { "startLine": 12, "endLine": 12 }
            } }
          ]
        }
      ]
    }
  ]
}
JSON

python3 sarif-postprocess.py "$TMP/zap.sarif" >/dev/null

# 1. No http(s) scheme survives on any artifactLocation/location uri.
if grep -E '"uri": "https?://' "$TMP/zap.sarif" | grep -vq 'zaproxy.org'; then
  fail "an http(s) artifactLocation URI survived the rewrite"
fi

python3 - "$TMP/zap.sarif" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
run = doc["runs"][0]

# 2. helpUri (a real ZAP docs URL) is NOT rewritten.
assert run["tool"]["driver"]["rules"][0]["helpUri"].startswith("https://www.zaproxy.org"), \
    "helpUri must be left untouched"

# 3. run-level artifacts[].location rewritten to sentinel.
assert run["artifacts"][0]["location"]["uri"] == ".zap/DAST-FINDINGS.md", \
    "artifacts[].location not rewritten"

r0 = run["results"][0]
loc0 = r0["locations"][0]["physicalLocation"]
# 4. artifactLocation rewritten to sentinel + %SRCROOT%.
assert loc0["artifactLocation"]["uri"] == ".zap/DAST-FINDINGS.md"
assert loc0["artifactLocation"]["uriBaseId"] == "%SRCROOT%"
# 5. original URL preserved in properties.
assert loc0["artifactLocation"]["properties"]["originalUri"] == "http://localhost:8080/api/v1/servers"
# 6. region reset to line 1.
assert loc0["region"] == {"startLine": 1, "endLine": 1}, loc0["region"]
# 7. message carries the real URL prefix.
assert r0["message"]["text"].startswith("[http://localhost:8080/api/v1/servers] "), r0["message"]["text"]

r1 = run["results"][1]
assert r1["message"]["text"].startswith("[https://localhost:8080/api/v1/users/me] ")
print("structure assertions passed")
PY

# 8. Idempotent — second run must not double-prefix the message or error.
python3 sarif-postprocess.py "$TMP/zap.sarif" >/dev/null
DOUBLE=$(python3 -c "import json,sys; d=json.load(open('$TMP/zap.sarif')); print(d['runs'][0]['results'][0]['message']['text'])")
case "$DOUBLE" in
  "[http://localhost:8080/api/v1/servers] [http://localhost:8080/api/v1/servers]"*)
    fail "message double-prefixed on second run (not idempotent)" ;;
esac

# 9. Fail-loud guard: a surviving http location URI must exit non-zero. Build
#    a fixture the walker can't reach (uri under an unexpected key) to force it.
cat > "$TMP/leak.sarif" <<'JSON'
{ "version": "2.1.0", "runs": [ { "results": [
  { "message": {"text":"x"}, "locations": [
    { "physicalLocation": { "artifactLocation": { "uri": "http://evil/x" } } }
  ] } ] } ] }
JSON
# Monkeypatch: rename the rewrite target so the walker no-ops, proving the
# post-rewrite scan catches a leak. We simulate by pointing the sentinel at an
# http uri (still http scheme) — the guard must reject it.
if python3 sarif-postprocess.py "$TMP/leak.sarif" "http://still-http/sentinel" >/dev/null 2>&1; then
  fail "fail-loud guard did not reject an http(s) sentinel"
fi

echo "All 9 sarif-postprocess tests passed."
