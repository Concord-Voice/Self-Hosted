#!/usr/bin/env bash
# Hermetic fixture tests for review-coverage.sh. No network: every case feeds a
# CV_COVERAGE_FIXTURE. Each RED case is a hole the check must not wave through.
set -uo pipefail
SCRIPT="$(cd "$(dirname "$0")" && pwd)/review-coverage.sh"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
H=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa   # head SHA
O=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb   # older SHA
PRJ="{\"head\":{\"sha\":\"$H\"},\"base\":{\"ref\":\"main\"},\"user\":{\"login\":\"dependabot[bot]\"}}"
CP='copilot-pull-request-reviewer[bot]'
NOCFG='{}'   # a variable, not "${7:-{\}}": bash 3.2 keeps the backslash

# run <name> <want-exit> <reviews-json> <comments-json> [pr-json] [expect-substring] [config-json]
run() {
  local name="$1" want="$2" reviews="$3" comments="$4" prj="${5:-$PRJ}" expect="${6:-}" cfg="${7:-$NOCFG}"
  printf '{"pr":%s,"reviews":%s,"comments":%s,"config":%s}' "$prj" "$reviews" "$comments" "$cfg" > "$TMP/f.json"
  local out got
  out=$(CV_COVERAGE_FIXTURE="$TMP/f.json" "$SCRIPT" 7 2>&1); got=$?
  if [[ "$got" -eq "$want" && ( -z "$expect" || "$out" == *"$expect"* ) ]]; then
    printf '  ok   %-52s (exit %d)\n' "$name" "$got"; pass=$((pass + 1))
  else
    printf '  FAIL %-52s (want %d, got %d) %s\n' "$name" "$want" "$got" "$out"; fail=$((fail + 1))
  fi
}
rev() { # rev <login> <sha> <state> [association] [type]
  printf '{"user":{"login":"%s","type":"%s"},"author_association":"%s","commit_id":"%s","state":"%s"}' "$1" "${5:-User}" "${4:-MEMBER}" "$2" "$3"
}
rec() { # rec <poster> <association> <sha> <reviewer> [prefix-before-marker] [type]
  printf '{"user":{"login":"%s","type":"%s"},"author_association":"%s","body":"First-party review\\n%s<!-- cv-review-coverage: sha=%s reviewer=%s suite=/review-pr -->"}' "$1" "${6:-User}" "$2" "${5:-}" "$3" "$4"
}

run "Copilot review at head counts"                0 "[$(rev "$CP" "$H" COMMENTED NONE Bot)]" '[]' "" "covered: $CP (COMMENTED) (fixture)"
run "insider review at head counts"                0 "[$(rev alice "$H" APPROVED MEMBER)]" '[]' "" "alice (APPROVED)"
run "changes-requested review at head counts"      0 "[$(rev alice "$H" CHANGES_REQUESTED OWNER)]" '[]' "" "alice (CHANGES_REQUESTED)"
run "auto-approve workflow does not count"         1 "[$(rev 'github-actions[bot]' "$H" APPROVED NONE Bot)]" '[]'
run "unlisted bot's review does not count"         1 "[$(rev 'github-advanced-security[bot]' "$H" COMMENTED NONE Bot)]" '[]'
run "unlisted bot with insider association no"    1 "[$(rev 'auto-approver[bot]' "$H" APPROVED COLLABORATOR Bot)]" '[]'
run "allowlisted bot with insider association ok"  0 "[$(rev "$CP" "$H" COMMENTED MEMBER Bot)]" '[]'
run "base-config allowlisted bot counts"           0 "[$(rev 'gitar-bot[bot]' "$H" COMMENTED NONE Bot)]" '[]' "" "gitar-bot[bot]" '{"review_coverage":{"reviewer_bots":["gitar-bot[bot]"]}}'
run "drive-by user's review does not count"        1 "[$(rev stranger "$H" APPROVED NONE)]" '[]'
run "past contributor's review does not count"     1 "[$(rev stranger "$H" COMMENTED CONTRIBUTOR)]" '[]'
run "ghost (null user) review does not count"      1 '[{"user":null,"author_association":"MEMBER","commit_id":"'"$H"'","state":"APPROVED"}]' '[]'
run "no reviews is zero"                           1 '[]' '[]' "" "ZERO coverage"
run "review on older SHA does not count"           1 "[$(rev alice "$O" APPROVED)]" '[]' "" "stale: alice@bbbbbbb"
run "first-party record at head counts"            0 '[]' "[$(rec mark MEMBER "$H" mark)]" "" "mark (first-party /review-pr)"
run "first-party record on older SHA is stale"     1 '[]' "[$(rec mark MEMBER "$O" mark)]" "" "stale: mark@bbbbbbb"
run "record naming someone else does not count"    1 '[]' "[$(rec mallory MEMBER "$H" mark)]"
run "record by an outsider does not count"         1 '[]' "[$(rec mark NONE "$H" mark)]"
run "unlisted insider bot's record does not count" 1 '[]' "[$(rec 'auto-approver[bot]' MEMBER "$H" 'auto-approver[bot]' '' Bot)]"
run "PR author's own record does not count"      1 '[]' "[$(rec mark OWNER "$H" mark)]" "{\"head\":{\"sha\":\"$H\"},\"base\":{\"ref\":\"main\"},\"user\":{\"login\":\"mark\"}}"
run "marker quoted mid-line does not count"        1 '[]' "[$(rec mark MEMBER "$H" mark '> ')]"
run "head marker after an old one in one comment"  0 '[]' "[$(rec mark MEMBER "$H" mark "<!-- cv-review-coverage: sha=$O reviewer=mark suite=/review-pr -->\\n")]"
run "author's own review does not count"           1 "[$(rev 'dependabot[bot]' "$H" COMMENTED NONE Bot)]" '[]' "" "" '{"review_coverage":{"reviewer_bots":["dependabot[bot]"]}}'
run "pending review does not count"                1 "[$(rev alice "$H" PENDING)]" '[]'
run "dismissed review does not count"              1 "[$(rev alice "$H" DISMISSED)]" '[]'
run "old review plus head record is covered"       0 "[$(rev alice "$O" APPROVED)]" "[$(rec mark OWNER "$H" mark)]"
run "missing head SHA is UNKNOWN"                  2 '[]' '[]' '{"head":{},"user":{"login":"x"}}'

out=$("$SCRIPT" abc 2>&1); got=$?
if [[ "$got" -eq 2 ]]; then echo "  ok   non-numeric PR is UNKNOWN (exit 2)"; pass=$((pass + 1)); else echo "  FAIL non-numeric PR: $got $out"; fail=$((fail + 1)); fi
out=$(CV_COVERAGE_FIXTURE="$TMP/missing.json" "$SCRIPT" 7 2>&1); got=$?
if [[ "$got" -eq 2 ]]; then echo "  ok   unreadable fixture is UNKNOWN (exit 2)"; pass=$((pass + 1)); else echo "  FAIL unreadable fixture: $got $out"; fail=$((fail + 1)); fi

# Live path through a stub `gh`: multi-page --slurp reviews, empty comments, base config 404.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<STUB
#!/usr/bin/env bash
case "\$*" in
  "repo view"*) echo o/r ;;
  *"contents/.claude/repo-config.json?ref=main"*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  *"pulls/7/reviews"*) echo '[[{"user":{"login":"x","type":"User"},"author_association":"NONE","commit_id":"$O","state":"COMMENTED"}],[$(rev "$CP" "$H" COMMENTED NONE Bot)]]' ;;
  *"issues/7/comments"*) echo '[[]]' ;;
  "api repos/o/r/pulls/7") echo '$PRJ' ;;
  *) echo "unexpected gh \$*" >&2; exit 1 ;;
esac
STUB
chmod +x "$TMP/bin/gh"
out=$(PATH="$TMP/bin:$PATH" "$SCRIPT" 7 2>&1); got=$?
case "$got:$out" in "0:#7 @aaaaaaa covered: $CP (COMMENTED)") echo "  ok   live path via stub gh (paginated, 404 config)"; pass=$((pass + 1)) ;;
  *) echo "  FAIL live path via stub gh: $got $out"; fail=$((fail + 1)) ;; esac
sed -i.bak 's/Not Found (HTTP 404)/Server Error (HTTP 500)/' "$TMP/bin/gh"
out=$(PATH="$TMP/bin:$PATH" "$SCRIPT" 7 2>&1); got=$?
if [[ "$got" -eq 2 ]]; then echo "  ok   unreadable base config is UNKNOWN (exit 2)"; pass=$((pass + 1)); else echo "  FAIL unreadable base config: $got $out"; fail=$((fail + 1)); fi

echo "review-coverage: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
