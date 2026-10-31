#!/bin/bash
# check-review-dispositions.sh — ships with the /enhanced-pr-review skill
#
# Merge gate enforcing the per-comment DISPOSITION LEDGER on a PR's review
# surface. Consumed by /enhanced-pr-review Phase 4 and /dev-lifecycle Step 9.1.
#
# The invariant: every review conversation carries a recorded human decision,
# and that decision is machine-checkable rather than inferred from the presence
# of prose.
#
#   Assertion 1 — thread coverage: every review thread that is unresolved OR
#     reviewer-authored (a Bot, or a login in the roster `.bots[]`) carries a
#     `<!-- cv-disposition: ... -->` marker written by a non-reviewer author.
#
#     Calibrated on the resolver-agnostic signal deliberately. The obvious
#     alternative — a thread resolved by a [bot] actor is blocking unless an
#     autofix re-review cleared it — fails in BOTH directions when measured.
#     Reviewer bots resolve their own threads as routine housekeeping once a
#     developer has replied, so that rule blocks a large majority of
#     well-handled threads; a gate that blocks the common good case gets
#     overridden as routine, and an overridden gate then stops catching the
#     threads that received no reply at all. Keying on whether a human
#     disposition exists clears the housekeeping and catches the silence.
#
#   Assertion 2 — deferral integrity: every `deferred` disposition names an
#     issue that actually exists. A follow-up issue that was promised but never
#     filed is an accepted finding wearing a deferral's clothes.
#
#   Assertion 3 — reconciliation post: exactly one `<!-- cv-review-reconciliation -->`
#     comment exists, authored by a repo member (not a bot — Assertion 1 fences
#     agents out of writing decisions and this would reopen that hole one level
#     up), and it names every bot that posted on ANY of the three surfaces:
#     review threads, top-level comments, and PR reviews. The `reviews` surface
#     is load-bearing — copilot-pull-request-reviewer appears there and nowhere
#     else. Matching is on the full login or a maintained display-name alias,
#     never a positional prefix. Skipped entirely when no reviewer spoke at all.
#     This is the
#     master-comment coverage clause: bots post a summary comment enumerating
#     their findings and then one inline comment per finding, and top-level
#     conversation comments have NO GitHub thread/resolve affordance at all — so
#     no per-thread gate can ever reach them. PRs are routinely seen with
#     ZERO review threads and 5-8 top-level bot comments each, so a thread-only
#     gate prints "all threads resolved" over a review surface it never read.
#
# WHAT THIS PROVES, AND WHAT IT DOES NOT.
#   Proves: a decision is recorded, addressable, and auditable for every review
#   conversation; that deferrals name issues that exist and are issues rather
#   than PRs; and that a repo member — not a drive-by account — recorded them.
#   It does NOT parse the reconciliation post's Decisions table or cross-check
#   its Thread handles; the table is house format, the coverage clause is teeth.
#   Does NOT prove a human made the decision. `gh` authenticates as the repo
#   owner, so an agent's reply is authored by a User and satisfies the non-bot
#   filter exactly as the developer's would. The gate cannot tell them apart and
#   does not try. What it changes is that the decisions are now WRITTEN DOWN in
#   one reviewable place — the reconciliation post — where a developer can read
#   five decisions and their reasoning in one pass. Human judgment enters at the
#   Phase 8 mode gate, not here. Do not cite a green run as evidence that a
#   person reviewed anything.
#
# Exit 0 if all assertions hold; exit 1 (with a per-violation message) otherwise.
# Exit 2 on a lookup failure — FAIL CLOSED, never silently "no violations".
#
# Env overrides (used by test-check-review-dispositions.sh fixtures):
#   CV_REVIEW_FIXTURE   path to a JSON file matching the GraphQL response shape,
#                       used instead of calling the API.
#   CV_SKIP_EXISTENCE_CHECKS 1 = skip the `gh issue view` existence probe (offline).
#
# Reviewer naming is encoded ONLY in the repo roster `.bots[]` (read from the
# merge base). Registering a reviewer, or disambiguating two personas that share
# a CI login, is a config edit there — never a change to this script.
# Portable to bash 3.2 (macOS pre-commit) — no mapfile/readarray/assoc arrays.
set -euo pipefail

PR="${1:-}"
if [ -z "$PR" ]; then
    echo "usage: $0 <pr-number>" >&2
    exit 2
fi

VALID_DISPOSITIONS="accepted rejected deferred undecided"
# Set on the live path; may stay empty under CV_REVIEW_FIXTURE. Every use is
# conditional, so an unset repo degrades to "no -R" rather than tripping set -u.
OWNER=""
REPO=""
fail=0

# --- Fetch -------------------------------------------------------------------
# One query covers both surfaces: reviewThreads (inline findings) and comments
# (top-level master/summary comments). They are distinct GitHub objects; a gate
# that reads only the first is structurally blind to the second.
RAW=""
if [ -n "${CV_REVIEW_FIXTURE:-}" ]; then
    # Error-checked: an unreadable fixture exited 1 under `set -e`, which the
    # caller reads as "blocking violations found" rather than "could not run".
    if ! RAW=$(cat "$CV_REVIEW_FIXTURE" 2>&1); then
        echo "STOP: could not read CV_REVIEW_FIXTURE '${CV_REVIEW_FIXTURE}': ${RAW}" >&2
        exit 2
    fi
else
    # Error-checked deliberately: under `set -e` an unguarded failure here exits
    # with gh's own code (1), which the caller reads as "blocking violations
    # found" when the truth is "the check could not run". Fail-closed means exit
    # 2 — UNKNOWN — not a 1 that sends the developer hunting a list that is empty.
    if ! OWNER_REPO=$(gh repo view --json nameWithOwner --jq '.nameWithOwner' 2>&1); then
        echo "STOP: could not resolve the repository: ${OWNER_REPO}" >&2
        echo "      Cannot determine the review surface. Treat as UNKNOWN, not clean." >&2
        exit 2
    fi
    OWNER="${OWNER_REPO%%/*}"
    REPO="${OWNER_REPO##*/}"
    if ! RAW=$(gh api graphql \
            -F owner="$OWNER" -F repo="$REPO" -F pr="$PR" \
            -f query='
query($owner:String!,$repo:String!,$pr:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$pr){
      reviewThreads(first:100){
        pageInfo{hasNextPage}
        nodes{
          id isResolved
          resolvedBy{login}
          path line
          comments(first:100){pageInfo{hasNextPage} nodes{author{__typename login} authorAssociation body}}
        }
      }
      comments(first:100){pageInfo{hasNextPage} nodes{author{__typename login} authorAssociation body}}
      reviews(first:100){pageInfo{hasNextPage} nodes{author{__typename login} body}}
    }
  }
}' 2>&1); then
        echo "STOP: could not read review surface for PR #${PR}: ${RAW}" >&2
        echo "      Treating this as UNKNOWN, not as clean. Re-run before merging." >&2
        exit 2
    fi
fi

# An API error body is valid JSON with an `errors` key and a null pullRequest.
# Without this probe, the jq below yields zero rows and the gate reports PASS.
if ! echo "$RAW" | jq -e '.data.repository.pullRequest != null' >/dev/null 2>&1; then
    echo "STOP: review-surface query returned no pullRequest for #${PR}." >&2
    echo "$RAW" | head -5 >&2
    exit 2
fi

# Truncation is UNKNOWN, never clean. `first:100` on any connection that has more
# pages means part of the review surface was never read — and a gate whose whole
# contract is "never report clean over an unread surface" must not exit 0 there.
# Exit 2 rather than paginate: UNKNOWN is cheap, already in the contract, and a
# PR with >100 threads wants a human anyway.
TRUNCATED=$(echo "$RAW" | jq -r '
  [ (.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage      // false | if . then "reviewThreads"  else empty end),
    (.data.repository.pullRequest.comments.pageInfo.hasNextPage           // false | if . then "comments"       else empty end),
    (.data.repository.pullRequest.reviews.pageInfo.hasNextPage            // false | if . then "reviews"        else empty end),
    (.data.repository.pullRequest.reviewThreads.nodes[]?.comments.pageInfo.hasNextPage // false
        | if . then "thread-comments" else empty end)
  ] | unique | join(", ")' 2>/dev/null || echo "JQERR")
if [ "$TRUNCATED" = "JQERR" ]; then
    echo "STOP: could not evaluate pagination state for #${PR}." >&2
    exit 2
fi
if [ -n "$TRUNCATED" ]; then
    echo "STOP: review surface is truncated (more pages on: ${TRUNCATED})." >&2
    echo "      Part of the review was never read. UNKNOWN, not clean." >&2
    exit 2
fi

# --- Reviewer roster -----------------------------------------------------------
# Display names come from the SAME place the triage table gets them: the per-repo
# roster `.bots[]`. Hardcoding a second alias list here would create a divergent
# copy of data that already exists, and the two would drift the first time
# someone registered a bot in only one of them.
#
# READ FROM THE MERGE BASE, NOT THE WORKING TREE. This gate runs from the
# checkout of the PR head, so a working-tree roster is supplied by the very
# branch the gate is judging: a one-line `.bots[]` diff could declare an alias
# that satisfies coverage for every reviewer, and the PR would clear its own
# review. The roster that governs a review must come from the branch being
# merged INTO. `CV_REPO_CONFIG` overrides for tests only.
ROSTER_JSON='[]'
ROSTER_SOURCE='(none — login-only matching)'
if [ -n "${CV_REPO_CONFIG:-}" ]; then
    if [ -f "$CV_REPO_CONFIG" ]; then
        ROSTER_RAW=$(cat "$CV_REPO_CONFIG" 2>&1) || ROSTER_RAW=''
        ROSTER_SOURCE="$CV_REPO_CONFIG (override)"
    else
        ROSTER_RAW=''
        ROSTER_SOURCE="$CV_REPO_CONFIG (override, absent)"
    fi
elif git rev-parse --git-dir >/dev/null 2>&1; then
    # Prefer the PR's ACTUAL base. A PR targeting a release branch must be
    # judged against that branch's roster; falling back to the repo default
    # silently reads the wrong one, and on a default branch with no roster the
    # output is indistinguishable from a legitimately roster-less repo.
    # /dev-lifecycle knows this from `gh pr view --json baseRefName`.
    BASE_REF="${CV_BASE_REF:-${GITHUB_BASE_REF:-}}"
    [ -n "$BASE_REF" ] && BASE_REF="origin/${BASE_REF#origin/}"
    if [ -z "$BASE_REF" ]; then
        BASE_REF=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null) \
            || BASE_REF="origin/${CV_DEFAULT_BRANCH:-main}"
    fi
    if ROSTER_RAW=$(git show "${BASE_REF}:.claude/repo-config.json" 2>/dev/null); then
        ROSTER_SOURCE="${BASE_REF}:.claude/repo-config.json"
    else
        ROSTER_RAW=''
        ROSTER_SOURCE="${BASE_REF} (no roster on the base branch)"
    fi
else
    ROSTER_RAW=''
fi

if [ -n "$ROSTER_RAW" ]; then
    # A malformed roster is a config error, not a reason to quietly fall back to
    # login-only: silent degradation blocks correctly-written posts while telling
    # the operator the accepted spelling is the raw login, with no hint why.
    if ! ROSTER_JSON=$(printf '%s' "$ROSTER_RAW" | jq -c 'if (.bots|type) == "array" then .bots else [] end' 2>&1); then
        echo "STOP: could not parse the reviewer roster at ${ROSTER_SOURCE}: ${ROSTER_JSON}" >&2
        echo "      Reviewer coverage cannot be evaluated. UNKNOWN, not clean." >&2
        exit 2
    fi
    [ -z "$ROSTER_JSON" ] && ROSTER_JSON='[]'

    # Validate before trusting. An unvalidated `.name` is a universal key: a
    # single-space name matches any prose body, and a newline inside one splits
    # it into several aliases of which the shortest fragment wins. `.match` is
    # equally load-bearing because it selects WHICH name becomes an accepted
    # alias — one over-broad entry donates its alias to the whole fleet.
    ROSTER_BAD=$(printf '%s' "$ROSTER_JSON" | jq -r '
        .[] | . as $e
        | ((($e.name // "") | tostring) as $n
           | (($e.match // "") | tostring) as $m
           | if ($n != "") and (($n | test("[\r\n]")) or (($n | gsub("\\s";"") | length) < 3))
             then "name=\"\($n)\" (must be >=3 non-space chars, no newlines)"
             elif ($m != "") and (($m | gsub("\\s";"") | length) < 2)
             then "match=\"\($m)\" (must be >=2 non-space chars)"
             else empty end)' 2>/dev/null)
    if [ -n "$ROSTER_BAD" ]; then
        echo "STOP: reviewer roster at ${ROSTER_SOURCE} has unusable entries:" >&2
        printf '%s\n' "$ROSTER_BAD" | sed 's/^/      /' >&2
        echo "      An over-broad alias satisfies coverage for reviewers it does not name." >&2
        exit 2
    fi
fi

# --- Assertion 1 — thread coverage -------------------------------------------
# A thread is IN SCOPE if it is unresolved or bot-authored. A human-authored
# thread a human already resolved is self-evidently decided; requiring a marker
# there would be ceremony, not coverage.
# A rostered machine USER account is a reviewer too (see `rostered` below), so its
# resolved threads stay in scope and its own comments never count as a disposition.
THREAD_ROWS=$(echo "$RAW" | jq -r --argjson roster "$ROSTER_JSON" '
  def rostered($login):
      ($login // "" | ascii_downcase) as $l
    | ($roster | map(. as $e | select(
          ((($e.login // "") | ascii_downcase | sub("\\[bot\\]$";"")) == $l)
          or ((($e.match // "") != "") and ($l | contains($e.match | ascii_downcase)))))
       | length) > 0;
  def reviewer: .author.__typename == "Bot" or rostered(.author.login);
  .data.repository.pullRequest.reviewThreads.nodes[]
  | select(.isResolved == false or (.comments.nodes[0] | reviewer))
  | . as $t
  # The marker must come from a NON-bot author. A bot echoing a marker back into
  # the thread (quoting a reply, or an autofix agent mimicking the format) must
  # never satisfy the gate on the human decision-makers behalf. `last` wins so a
  # corrected disposition supersedes an earlier one in the same thread.
  | ([$t.comments.nodes[]
      | select(reviewer | not)
      # Identity class alone is not authorization. Without this, ANY GitHub
      # account that can comment could clear every finding on a PR. COLLABORATOR
      # is deliberately NOT accepted: GitHub returns it for read and triage
      # access, so it would admit someone who cannot push. Repos that genuinely
      # want an outside collaborator deciding findings should add them to a team
      # (making them MEMBER) rather than widening this set. Harmless
      # today (the gate runs from a developer session on a private repo), but
      # the name "merge gate" invites wiring it into a workflow, and this repo
      # goes AGPL in 2030 — cheap now, load-bearing later.
      | select((.authorAssociation // "NONE") | IN("OWNER","MEMBER"))
      | .body
      | capture("<!--\\s*cv-disposition:\\s*(?<d>[a-z]+)(?<rest>[^>]*?)-->"; "g")
     ] | last) as $m
  | [ (if $m == null then "MISSING" else $m.d end),
      # Sentinel EVERY empty case, not just the observed one. An empty @tsv field
      # collapses against its neighbouring tab in `read` (tab is IFS whitespace) and
      # shifts the row. `evidence` has TWO empty paths — no marker at all, and a
      # marker carrying no `evidence:` key — and the first patch here covered only
      # the first. The second collapsed the field, handed `evidence` the path
      # (never empty), and made the `-z "$evidence"` guard below UNREACHABLE, so
      # `rejected` / `superseded` with no evidence exited 0: a fail-open in a gate
      # whose whole thesis is that it never reports clean over an undecided thread.
      (if $m == null then "-"
       # Capture to the END of `rest`, not to the first space. `rest` already
       # excludes `-->` via the outer regex, so this is bounded. The prior
       # `[^\\s>]+` truncated at the first whitespace — and because a truncated
       # capture is still non-empty, the `-z "$evidence"` guard never fired, so
       # multi-word evidence (`duplicate of row 3`, `waiting on the retry-policy
       # call`) was silently accepted in mangled form. Both are shapes this
       # skill documents as normal in its own disposition table.
       else ((($m.rest | capture("evidence:\\s*(?<e>[^>]+?)\\s*$") | .e) // "")
             | if . == "" then "-" else . end)
       end),
      (($t.path // "—") | if . == "" then "—" else . end),
      (($t.line // 0)|tostring),
      (($t.comments.nodes[0].author.login // "unknown") | if . == "" then "unknown" else . end),
      (($t.resolvedBy.login // "NONE") | if . == "" then "NONE" else . end) ]
  | @tsv' 2>&1) || THREAD_ROWS="JQERR"
if [ "$THREAD_ROWS" = "JQERR" ] || printf '%s' "$THREAD_ROWS" | grep -q '^jq: error'; then
    echo "STOP: could not parse review threads for #${PR}: ${THREAD_ROWS}" >&2
    exit 2
fi

if [ -n "$THREAD_ROWS" ]; then
    while IFS=$'\t' read -r disp evidence path line bot resolved_by; do
        [ -z "${disp:-}" ] && continue
        # Decode the jq sentinel. `-` means "this field was empty"; the sentinel
        # exists only to stop an empty @tsv field collapsing against its tab in
        # `read`. Decoding it here is what makes the `-z "$evidence"` guard below
        # reachable — without this line the guard is dead code and a disposition
        # with no evidence exits 0.
        [ "$evidence" = "-" ] && evidence=""
        loc="${path}:${line}"
        case "$disp" in
            MISSING)
                echo "BLOCKING: no human disposition — ${loc} (${bot}, resolved by ${resolved_by})" >&2
                fail=1
                ;;
            accepted|rejected|deferred|undecided)
                if [ -z "$evidence" ]; then
                    echo "BLOCKING: disposition '${disp}' has no evidence: field — ${loc} (${bot})"
                    fail=1
                    continue
                fi
                if [ "$disp" = "undecided" ]; then
                    # Blocks, like MISSING — but for the opposite reason. MISSING is
                    # "nobody decided and nobody noticed"; undecided is "we know this
                    # is open and here is what it waits on". Distinguishing them is
                    # what stops an open question being mis-filed as rejected or
                    # deferred just to clear the gate.
                    echo "BLOCKING (open): awaiting ${evidence} — ${loc} (${bot})"
                    fail=1
                    continue
                fi
                # Assertion 2 — a deferral is only a deferral if the issue exists.
                if [ "$disp" = "deferred" ]; then
                    issue="${evidence#\#}"
                    case "$issue" in
                        ''|*[!0-9]*)
                            echo "BLOCKING: deferred disposition needs 'evidence: #NNN', got '${evidence}' — ${loc}"
                            fail=1
                            ;;
                        *)
                            if [ "${CV_SKIP_EXISTENCE_CHECKS:-0}" != "1" ]; then
                                # -R pins the repo: without it `gh` resolves against
                                # cwd, which in a fork is the PARENT, so a deferral
                                # could name an upstream issue and pass. `--json
                                # number,url` then confirms it is an ISSUE, not a PR
                                # — `gh issue view` succeeds on a PR number, so
                                # `deferred evidence: #<some PR>` used to satisfy
                                # "the issue exists".
                                # Capture output and status SEPARATELY. The earlier
                                # `|| ISSUE_URL=""` blanked the message on every failure,
                                # so "not found" and "the token expired" were the same
                                # value — and the script then asserted the issue did not
                                # exist, a fact it had not established, sending the
                                # developer to file an issue that was already there.
                                # `|| ISSUE_RC=$?` rather than a bare assignment: under
                                # `set -e` an assignment from a failing command
                                # substitution aborts the script before the next line
                                # runs, so a separate `ISSUE_RC=$?` never executes and
                                # every transport failure exited 1 instead of 2.
                                ISSUE_RC=0
                                if [ -n "$OWNER" ] && [ -n "$REPO" ]; then
                                    ISSUE_OUT=$(gh issue view "$issue" -R "${OWNER}/${REPO}" --json url --jq '.url' 2>&1) || ISSUE_RC=$?
                                else
                                    ISSUE_OUT=$(gh issue view "$issue" --json url --jq '.url' 2>&1) || ISSUE_RC=$?
                                fi
                                case "${ISSUE_RC}:${ISSUE_OUT}" in
                                    0:*/issues/*) ;;
                                    0:*/pull/*)
                                        echo "BLOCKING: deferred to #${issue}, which is a pull request, not an issue — ${loc}" >&2
                                        fail=1 ;;
                                    0:*)
                                        echo "BLOCKING: deferred to #${issue} resolved to an unexpected URL '"'"'${ISSUE_OUT}'"'"' — ${loc}" >&2
                                        fail=1 ;;
                                    *"Could not resolve"*|*"not found"*|*"Not Found"*|*"NOT_FOUND"*)
                                        echo "BLOCKING: deferred to #${issue} but that issue does not exist — ${loc}" >&2
                                        echo "          A follow-up that was never filed is an accepted finding." >&2
                                        fail=1 ;;
                                    *)  echo "STOP: could not verify issue #${issue} (${loc}): ${ISSUE_OUT}" >&2
                                        echo "      A transport or auth failure is not a missing issue. UNKNOWN, not clean." >&2
                                        exit 2 ;;
                                esac
                            fi
                            ;;
                    esac
                fi
                if [ "$disp" = "accepted" ]; then
                    # 'accepted' means a fix landed; evidence is the commit that landed it.
                    if ! echo "$evidence" | grep -qE '^[0-9a-f]{7,40}$'; then
                        echo "BLOCKING: accepted disposition needs 'evidence: <commit-sha>', got '${evidence}' — ${loc}"
                        fail=1
                    elif [ "${CV_SKIP_EXISTENCE_CHECKS:-0}" != "1" ] \
                         && ! git rev-parse --git-dir >/dev/null 2>&1; then
                        echo "NOTE: not inside a git checkout — the accepted-SHA sanity" >&2
                        echo "      check was SKIPPED for ${evidence}. Absence of a WARN" >&2
                        echo "      here is not a verification." >&2
                    elif [ "${CV_SKIP_EXISTENCE_CHECKS:-0}" != "1" ] \
                         && ! git cat-file -e "${evidence}^{commit}" 2>/dev/null; then
                        # WARN, never BLOCK: a shallow/fresh checkout legitimately
                        # lacks the object. A typo'd SHA is the case worth catching,
                        # and it voids the pointer the disposition exists to provide.
                        echo "WARN: accepted evidence '${evidence}' does not resolve locally — ${loc}"
                        echo "      Typo, or the commit is simply not fetched here. Confirm it is real."
                    fi
                fi
                ;;
            *)
                echo "BLOCKING: unknown disposition '${disp}' — ${loc}"
                echo "          Valid: ${VALID_DISPOSITIONS}"
                fail=1
                ;;
        esac
    done <<< "$THREAD_ROWS"
fi

# --- Assertion 3 — reconciliation post ---------------------------------------
# Reviewer coverage is matched on the FULL login plus a maintained alias table —
# never a positional prefix. The predecessor took `cut -c1-6` of the login and
# grepped for it, which was wrong in both directions and was caught by four
# independent reviewers:
#   sonarqubecloud -> "sonarq" — absent from the prose "SonarCloud", so a
#     correctly-written post was BLOCKED (3 of the 4 real logins did this,
#     including gitar-bot -> "gitar-", whose trailing hyphen misses "Gitar").
#   github-actions -> "github" — present in ANY github.com URL, so the Release
#     Classifier (per SKILL.md the single most-missed signal in PR triage) was
#     silently satisfied by a link. A false PASS on the worst reviewer to lose.
# GraphQL `author.login` carries NO `[bot]` suffix (verify against your own
# repo before assuming otherwise) even though `resolvedBy.login` does — so the old sed was a
# no-op and the prefix was the whole check. The strip below is defensive only.
# --- Reviewer personas ---------------------------------------------------------
# What must be named in the reconciliation post is a PERSONA, not a login. One
# login can carry several: a shared CI login posts as both a SAST reporter and a
# release classifier, disambiguated by the `<!-- marker -->` its workflow writes.
# Keying coverage on the login alone collapses them, so naming one satisfies the
# other — the exact false PASS this gate exists to prevent, and the roster
# already carries the disambiguator the earlier version discarded.
#
# Resolution order per comment: roster entry whose login matches AND whose marker
# appears in the body; else whose `match` matches the login; else the bare login.
REQUIRED=$(echo "$RAW" | jq -r --argjson roster "$ROSTER_JSON" '
    # EVERY roster predicate binds the entry with `. as $e` first. Inside
    # contains(), `.` is the string being tested — so a bare `.marker`/`.match`
    # indexes a string and jq aborts the whole program. This exact mistake was
    # made twice while writing this file; the binding is not stylistic.
    # A roster-registered login is a reviewer whatever its GraphQL type. A CI
    # reviewer implemented as a machine USER account is `__typename == "User"`,
    # so the type fence alone treated it as a person: its threads fell out of
    # scope, it owed no coverage row, and a PR reviewed only by it reported
    # "no reviewer posted".
    def rostered($login):
        ($login | ascii_downcase) as $l
      | ($roster | map(. as $e | select(
            ((($e.login // "") | ascii_downcase | sub("\\[bot\\]$";"")) == $l)
            or ((($e.match // "") != "") and ($l | contains($e.match | ascii_downcase)))))
         | length) > 0;
    def persona($login; $body):
        ($login | ascii_downcase) as $l
      | ($roster | map(. as $e | select(
            ((($e.login // "") | ascii_downcase | sub("\\[bot\\]$";"")) == $l)
            and (($e.marker // "") != "") and ($body | contains($e.marker))))) as $bymarker
      | ($roster | map(. as $e | select(
            ((($e.login // "") | ascii_downcase | sub("\\[bot\\]$";"")) == $l)
            and (($e.marker // "") == "")))) as $bylogin
      | ($roster | map(. as $e | select(
            (($e.match // "") != "") and ($l | contains($e.match | ascii_downcase))))) as $bymatch
      # Emit ALL matched personas, not just the first. One comment body can
      # carry two markers (a CI job that reports twice), and `first` silently
      # dropped the second persona so the post never had to name it — a gap in
      # the very multi-persona feature this resolver exists to provide.
      | (($bymarker + $bylogin + $bymatch) | map(.name // empty)) as $names
      | if ($names | length) > 0 then $names[] else $login end;
    [ (.data.repository.pullRequest.comments.nodes[]?  | select(.author.__typename=="Bot" or rostered(.author.login))
        | persona(.author.login; (.body // ""))),
      (.data.repository.pullRequest.reviews.nodes[]?   | select(.author.__typename=="Bot" or rostered(.author.login))
        | persona(.author.login; (.body // ""))),
      (.data.repository.pullRequest.reviewThreads.nodes[]?.comments.nodes[]?
        | select(.author.__typename=="Bot" or rostered(.author.login))
        | persona(.author.login; (.body // "")))
    ] | map(select(. != null and . != "")) | unique | .[]' 2>&1) || REQUIRED="JQERR"
if [ "$REQUIRED" = "JQERR" ] || printf '%s' "$REQUIRED" | grep -q '^jq: error'; then
    echo "STOP: could not enumerate reviewers for #${PR}: ${REQUIRED}" >&2
    exit 2
fi

# Every bot that spoke, from ALL THREE surfaces. `reviews` is load-bearing:
# copilot-pull-request-reviewer appears there and nowhere else (verify on your own repo), so a review-summary-only bot was previously invisible to this check —
# the exact "summary comment nobody answered" case Assertion 3 exists for.
# Thread authors come from every comment in the thread, not just nodes[0]: a bot
# that replies into a human-opened thread is a reviewer that spoke.
# The reconciliation post must be human-authored, for the same reason a
# disposition must be: Assertion 1 fences `__typename != "Bot"` so an agent
# cannot mint its own decision, and a bot-authored reconciliation post would
# reopen that hole one level up. Codesmith is installed on this repo and posts.
RECON_COUNT=$(echo "$RAW" | jq '[.data.repository.pullRequest.comments.nodes[]?
    | select(.author.__typename != "Bot")
    | select((.authorAssociation // "NONE") | IN("OWNER","MEMBER"))
    | select(.body | test("<!--\\s*cv-review-reconciliation\\s*-->"))] | length' 2>/dev/null || echo "JQERR")
if [ "$RECON_COUNT" = "JQERR" ]; then
    echo "STOP: could not read reconciliation posts for #${PR}." >&2
    exit 2
fi

if [ -z "$REQUIRED" ] && [ "$RECON_COUNT" -eq 0 ]; then
    # Nobody reviewed anything. Requiring a post that reconciles nothing is
    # ceremony, and Step 9.1 runs this gate on EVERY PR including ones no bot
    # ever touches. Assertion 1 still governs any human thread on such a PR.
    echo "INFO: no reviewer posted on this PR; reconciliation post not required."
elif [ "$RECON_COUNT" -eq 0 ]; then
    echo "BLOCKING: no /enhanced-pr-review reconciliation post on this PR."
    echo "          Post it per /enhanced-pr-review Phase 3.5 before the merge gate."
    fail=1
elif [ "$RECON_COUNT" -gt 1 ]; then
    echo "BLOCKING: ${RECON_COUNT} reconciliation posts found — expected exactly 1."
    echo "          Phase 3.5 edits its post in place (gh api PATCH); it never re-posts."
    fail=1
else
    RECON_BODY=$(echo "$RAW" | jq -r '.data.repository.pullRequest.comments.nodes[]?
        | select(.author.__typename != "Bot")
        | select((.authorAssociation // "NONE") | IN("OWNER","MEMBER"))
        | select(.body | test("<!--\\s*cv-review-reconciliation\\s*-->")) | .body')
    while IFS= read -r want; do
        [ -z "$want" ] && continue
        # -F: fixed string, never a pattern. --: a value starting with a hyphen
        # can never be read as a grep option. Both verified by review.
        if ! printf '%s' "$RECON_BODY" | grep -qiF -- "$want"; then
            echo "BLOCKING: reconciliation post does not account for reviewer '${want}'." >&2
            echo "          Every reviewer that posted — including summary-only ones, and" >&2
            echo "          each persona of a shared CI login — needs a row naming it." >&2
            fail=1
        fi
    done <<< "$REQUIRED"
fi

# --- Verdict -----------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
    echo "✅ Review dispositions complete for PR #${PR} — every in-scope thread carries a decision."
    exit 0
fi
echo ""
echo "❌ Review-disposition gate FAILED for PR #${PR}. Do not merge."
exit 1
