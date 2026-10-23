#!/usr/bin/env bash
#
# verify-changelog.sh — CHANGELOG.md discipline gate (#1857).
#
# Fail-closed pr-ci check with five rules:
#   1. A desktop version bump (client/desktop/package.json "version" changed
#      upward in this PR) must have a matching "## [<new-version>]" section
#      present in CHANGELOG.md at HEAD. Presence-at-HEAD (not diff-added) is
#      deliberate: it is immune to merged-main sweeps / pre-added entries /
#      stale anchors, and passes revert PRs naturally.
#   2. Lines ADDED to CHANGELOG.md in this PR must not contain private-tree
#      cross-references (PMIRROR_PRIVATE_REF_REGEX). The renderer bundles this
#      file into the publicly served SPA (#1857), bypassing the public-mirror
#      transform scrub — so the file itself must stay clean.
#   3. A `## [X.Y.Z]` heading may only be ADDED in a promotion-shaped diff:
#      [Unreleased] non-empty at the base, emptied at HEAD, heading matching
#      package.json. Version sections are the promoter's output, not a PR's.
#   4. A change under client/desktop/src, services/control-plane or
#      services/media-plane must add a line under `## [Unreleased]`, unless a
#      `Changelog-Exempt: <reason>` commit trailer says why a user cannot
#      observe it. Tests and dependency manifests are exempt by path.
#   5. Every `### ` line in CHANGELOG.md at HEAD must be structurally a
#      heading — a short section name — not a heading with prose appended to
#      it. That corruption is produced by a promotion racing an open branch,
#      not by an author, and git merges it without reporting a conflict.
#
# Rules 3, 4 and 5 run BEFORE Rule 1: every path through Rule 1 exits, so a
# rule placed after it is unreachable.
#
# Anchoring mirrors classify-release.sh: merge-base of origin/$GITHUB_BASE_REF
# and HEAD (BASE_SHA fallback for local runs / test fixtures). A raw BASE_SHA
# two-dot diff is the exact stale-base pattern retired by #1568/#1572.
# Requires a fetch-depth:0 checkout in CI (merge-base needs history).
#
# Inputs (environment variables):
#   BASE_SHA        — git SHA of the PR base (required)
#   GITHUB_BASE_REF — PR base branch name (optional; preferred anchor)
#
# Exit codes: 0 pass, 1 gate failure or usage error.

set -euo pipefail

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

if [[ "${1-}" == "--help" ]]; then
  grep '^#' "$0" | sed 's/^# \?//'
  exit 0
fi

if [[ -z "${BASE_SHA:-}" ]]; then
  echo "ERROR: BASE_SHA environment variable is required." >&2
  exit 1
fi

# Private-ref regex is defined once, in the public-mirror helpers — source it
# relative to THIS script's location (not the repo under inspection, which in
# tests is a bare fixture without scripts/).
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "$SCRIPT_DIR/../../scripts/public-mirror/lib.sh"

# ── Diff anchor (mirrors classify-release.sh #1568/#1572) ─────────────────────
BASE_REF="${GITHUB_BASE_REF:-}"
BASE_TIP="${BASE_SHA}"
if [[ -n "$BASE_REF" ]]; then
  if git rev-parse --verify --quiet "origin/${BASE_REF}" >/dev/null 2>&1; then
    BASE_TIP="origin/${BASE_REF}"
  elif git fetch --no-tags --quiet origin "$BASE_REF" 2>/dev/null; then
    BASE_TIP="FETCH_HEAD"
  fi
fi
MERGE_BASE=$(git merge-base "${BASE_TIP}" HEAD 2>/dev/null \
  || git merge-base "${BASE_SHA}" HEAD 2>/dev/null \
  || echo "${BASE_SHA}")

# The last fallback above can yield a SHA this clone does not have. Every rule
# below reads the diff through `... || true`, so an unresolvable anchor made all
# three silently no-op and the script exited 0 printing PASS — a gate reporting
# success having verified nothing. Rule 1 did not save it either: OLD_VERSION
# degrades to 0.0.0, so the run always looked like a bump and passed whenever
# HEAD carried a section for the current version, which is the normal state of
# main. Validate once, here, and fail closed.
if ! git rev-parse --verify --quiet "${MERGE_BASE}^{commit}" >/dev/null; then
  echo "ERROR: could not resolve a merge base (BASE_SHA=${BASE_SHA}, BASE_REF=${BASE_REF:-none})." >&2
  echo "       verify-changelog.sh requires a fetch-depth: 0 checkout." >&2
  exit 1
fi

# ── Rule 2: private-ref hygiene on added CHANGELOG lines ─────────────────────
ADDED_LINES=$(git diff "${MERGE_BASE}" HEAD -- CHANGELOG.md | grep -E '^\+' | grep -vE '^\+\+\+' || true)
# grep WITHOUT -q (redirected instead): under pipefail, `grep -q` exits at the
# first match and SIGPIPEs a still-writing producer, turning the pipeline
# non-zero — which here would FAIL-OPEN (skip the private-ref FAIL) on large
# input. Non-quiet grep consumes all input, so the producer never SIGPIPEs.
if [[ -n "$ADDED_LINES" ]] && printf '%s\n' "$ADDED_LINES" | grep -E "$PMIRROR_PRIVATE_REF_REGEX" >/dev/null; then
  echo "FAIL: CHANGELOG.md additions reference private-tree paths (the file is bundled into the public SPA):" >&2
  printf '%s\n' "$ADDED_LINES" | grep -E "$PMIRROR_PRIVATE_REF_REGEX" >&2
  echo "Reword these lines without [internal] / internal-docs / [internal] path tokens." >&2
  echo "See docs/release/desktop-release-checklist.md (CHANGELOG entry step)." >&2
  exit 1
fi

# ── Rule 3: a version heading may only appear in a promotion-shaped diff ─────
# (#2996) Every `## [X.Y.Z]` section is written by
# promote-unreleased-changelog.sh, which moves the `## [Unreleased]` body under
# a dated heading verbatim. A feature PR that hand-writes one describes only
# itself AND leaves [Unreleased] empty, so the promoter exits 3 with nothing to
# promote — which is how desktop-v0.2.41 (cut by the since-removed nightly
# `bump` job) shipped five merges and described one.
#
# ORDERING: this runs BEFORE Rule 1, not after, and that is structural rather
# than stylistic. Every path through Rule 1 exits — 0 on no-change, downgrade
# and matching-section, 1 otherwise — so a rule placed after it is unreachable.
#
# It checks the SHAPE of the diff, never who authored it: no branch-name or
# actor coupling, so the sanctioned manual path (run the promoter locally, open
# a PR) passes unchanged, and a stale-base merge that sweeps in someone else's
# promotion is not evaluated because it is not in this PR's own diff.
ALL_ADDED_HEADINGS=$(git diff "${MERGE_BASE}" HEAD -- CHANGELOG.md \
  | grep -E '^\+## \[[0-9]+\.[0-9]+\.[0-9]+' \
  | sed 's/^+## \[\([^]]*\)\].*/\1/' || true)

# Lines between `## [Unreleased]` and the next `## [`, at a given ref. Used by
# BOTH Rule 3 (was it emptied?) and Rule 4 (did this PR add to it?), so it lives
# at file scope rather than inside Rule 3's conditional.
unreleased_body() {
  git show "$1:CHANGELOG.md" 2>/dev/null | awk '
    /^## \[Unreleased\]/ { inu = 1; next }
    inu && /^## \[/      { inu = 0 }
    inu                  { print }
  '
}

# Lines under a GIVEN version heading, at a given ref. Rule 4 needs it to see a
# note that this PR promoted out of [Unreleased] and into the new section.
# Prefix-matched with index() rather than a regex so the version's dots need no
# escaping, and so a heading's trailing date never has to be modelled here.
section_body() { # <ref> <version label>
  git show "$1:CHANGELOG.md" 2>/dev/null | awk -v want="## [$2]" '
    index($0, want) == 1 { ins = 1; next }
    ins && /^## \[/      { ins = 0 }
    ins                  { print }
  '
}

# A unified diff marks a MODIFIED line `+` exactly as it marks a new one, so
# correcting a published heading — a date typo, a dash normalisation — is
# indistinguishable from adding one. Drop any version that already had a section
# at the merge base: Rule 3 governs where notes are WRITTEN, not how a shipped
# section is corrected. Without this, a one-character date fix failed the gate
# with a message telling the author to move it under [Unreleased], which would
# not fix a date; and because main now always carries a populated [Unreleased],
# that was the branch every such edit landed in.
NEW_HEADINGS=""
while IFS= read -r _h; do
  [[ -n "$_h" ]] || continue
  _h_esc=$(printf '%s' "$_h" | sed 's/\./\\./g')
  if git show "${MERGE_BASE}:CHANGELOG.md" 2>/dev/null \
     | grep -E "^## \[${_h_esc}\]" >/dev/null; then
    continue
  fi
  NEW_HEADINGS="${NEW_HEADINGS}${_h}"$'\n'
done < <(printf '%s\n' "${ALL_ADDED_HEADINGS}")
NEW_HEADINGS=$(printf '%s' "$NEW_HEADINGS" | grep -vE '^[[:space:]]*$' || true)

# File scope, because Rule 4 reads it too: under `set -u` a reference to a
# variable assigned only inside Rule 3's conditional aborts the script on every
# PR that adds no heading, which is nearly all of them.
ADDED_HEADING=""

if [[ -n "$NEW_HEADINGS" ]]; then
  # Count BEFORE picking one. `head -1` alone validated the first heading and
  # let every later one through, so a promotion-shaped diff could smuggle an
  # arbitrary extra section past the rule. A promotion writes exactly one.
  HEADING_COUNT=$(printf '%s\n' "$NEW_HEADINGS" | wc -l | tr -d ' ')
  ADDED_HEADING=$(printf '%s\n' "$NEW_HEADINGS" | first_line)
  # `|| true` on both: unreleased_body is a `git show | awk` pipeline, and under
  # pipefail a missing CHANGELOG.md at either ref made the bare substitution
  # return 128, aborting the script under `set -e` with no message at all —
  # undiagnosable in a CI log, and outside this file's documented exit contract.
  BASE_UNREL=$(unreleased_body "$MERGE_BASE" | tr -d '[:space:]' || true)
  HEAD_UNREL=$(unreleased_body HEAD | tr -d '[:space:]' || true)
  HEAD_PKG_VERSION=$(git show HEAD:client/desktop/package.json 2>/dev/null \
    | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | first_line || true)
  # Compare the numeric triple only. Heading labels carry suffixes that
  # package.json never does — `## [0.3.0-Beta]` against a manifest reading
  # "0.3.0" is a real, passing shape (Rule 1 allows it explicitly), so a
  # literal string compare here would reject a correct promotion.
  ADDED_TRIPLE="${ADDED_HEADING%%-*}"

  RULE3_FAIL=""
  if [[ "$HEADING_COUNT" -gt 1 ]]; then
    RULE3_FAIL="it adds ${HEADING_COUNT} version headings ($(printf '%s' "$NEW_HEADINGS" | tr '\n' ' ')) and a promotion adds exactly one"
  elif [[ -z "$BASE_UNREL" ]]; then
    RULE3_FAIL="'## [Unreleased]' was empty at the merge base, so there was nothing to promote"
  elif [[ -n "$HEAD_UNREL" ]]; then
    RULE3_FAIL="'## [Unreleased]' is still populated at HEAD — a promotion empties it"
  elif [[ "$HEAD_PKG_VERSION" != "$ADDED_TRIPLE" ]]; then
    RULE3_FAIL="the added heading is [${ADDED_HEADING}] but client/desktop/package.json is ${HEAD_PKG_VERSION}"
  fi

  if [[ -n "$RULE3_FAIL" ]]; then
    echo "FAIL: this PR adds a '## [${ADDED_HEADING}]' section, but ${RULE3_FAIL}." >&2
    echo "Version sections are written by .github/scripts/promote-unreleased-changelog.sh," >&2
    echo "never by hand. If this PR is not cutting a release, record the change under" >&2
    echo "'## [Unreleased]' and delete the heading — the promoter moves it under a dated" >&2
    echo "heading later, so nothing is lost and the notes cover the whole tag range." >&2
    echo "If it IS cutting one, run the promoter rather than editing the file:" >&2
    echo "  ./.github/scripts/promote-unreleased-changelog.sh <version>" >&2
    echo "  cd client/desktop && npm version <version> --no-git-tag-version" >&2
    echo "See docs/release/desktop-release-checklist.md." >&2
    exit 1
  fi
fi

# ── Rule 4: client/service-facing changes record an [Unreleased] entry (#2996) ──
# Rule 3's complement. Rule 3 stops release notes being written in the wrong
# PLACE; Rule 4 stops them not being written at all. Both failures produced
# desktop-v0.2.41, which shipped 98 commits behind a single bullet.
#
# The include set is what the desktop post-update modal renders to users
# (#1857); the exempt set is everything whose change a user cannot observe.
# Dependency manifests are exempt so bot PRs stay green without a standing
# label ritual.
#
# ORDERING: like Rule 3, this must precede Rule 1 — every path through Rule 1
# exits, so anything after it is unreachable.
CHANGED=$(git diff --name-only "${MERGE_BASE}" HEAD || true)
RELEVANT=$(printf '%s\n' "$CHANGED" \
  | grep -E '^(client/desktop/src/|services/control-plane/|services/media-plane/)' \
  | grep -vE '(^|/)(tests?|testdata|testhelpers|testutil|__tests__|__mocks__|fixtures)/' \
  | grep -vE '\.(test|spec)\.(ts|tsx|js|jsx)$|_test\.go$' \
  | grep -vE '(^|/)(package(-lock)?\.json|go\.(mod|sum))$' \
  | grep -vE '\.(md|ya?ml)$|(^|/)(Dockerfile[^/]*|Makefile|\.dockerignore|\.env\.example)$' || true)
# The last two exclusions are noise control, and noise here is corrosive rather
# than merely annoying: a lint-config or README-only PR told to write a
# user-facing release note teaches authors to paste a boilerplate trailer, which
# erodes the rule far more than the odd missed entry would. `.sql` is
# deliberately NOT exempt — a migration is closer to user-observable than a
# Dockerfile is, and the trailer is one line when it genuinely is not.

if [[ -n "$RELEVANT" ]]; then
  # A bare `Changelog-Exempt:` yields an EMPTY value from git's own trailer
  # parser (verified against git, not assumed), so filtering blanks is what
  # makes the reason mandatory rather than decorative.
  #
  # The blank filter is pinned by the MULTI-COMMIT case, not by the bare-trailer
  # one. `git log` is reverse-chronological and emits a line per commit, so the
  # blank that can mask a real reason is the NEWEST commit's; on a single-commit
  # range `head -1` yields empty with or without the filter and the case passes
  # either way. An earlier revision of this comment credited the bare-trailer
  # case, which would have left the filter unfalsifiable if the other were ever
  # deleted.
  EXEMPT_REASON=$(git log --format='%(trailers:key=Changelog-Exempt,valueonly)' \
    "${MERGE_BASE}..HEAD" | grep -vE '^[[:space:]]*$' | first_line || true)

  if [[ -n "$EXEMPT_REASON" ]]; then
    echo "PASS: Changelog-Exempt — ${EXEMPT_REASON}"
  else
    # Compare the SECTION BODY at base vs HEAD rather than scanning the diff for
    # added lines inside an `[Unreleased]` region. In a unified diff that heading
    # is a CONTEXT line whenever a bullet is appended below it, so a region
    # tracker keyed on `^[+-]## \[Unreleased\]` never enters the region and the
    # rule fails every compliant PR. Body comparison has no such dependence on
    # diff formatting.
    BASE_UNREL_R4=$(unreleased_body "$MERGE_BASE" | grep -vE '^[[:space:]]*$' || true)

    # HEAD's notes are [Unreleased] PLUS any section this PR promoted. Without
    # the second half, Rules 3 and 4 are mutually exclusive for a source-
    # touching PR — Rule 3 requires a promotion to EMPTY [Unreleased], Rule 4
    # required a source change to ADD to it — so a PR doing both satisfied
    # neither and the version bump could only ever live in a source-free PR of
    # its own. This rule's subject is whether a user-facing change wrote a note,
    # not which heading the note came to rest under, and Rule 3 has already
    # proven this is a real promotion before Rule 4 looks at it.
    HEAD_NOTES_R4=$(unreleased_body HEAD || true)
    if [[ -n "$ADDED_HEADING" ]]; then
      HEAD_NOTES_R4="${HEAD_NOTES_R4}"$'\n'"$(section_body HEAD "$ADDED_HEADING" || true)"
    fi
    HEAD_UNREL_R4=$(printf '%s\n' "$HEAD_NOTES_R4" | grep -vE '^[[:space:]]*$' || true)

    # An ADDITION, not merely a difference. Testing inequality accepted a PR that
    # DELETED someone else's bullet and added none of its own — and that is not a
    # contrived edit, it is how a merge conflict on this one file gets resolved.
    # The spec's own Open Risk 2 (contention on a single anchor at this repo's
    # velocity) therefore turned straight into a gate bypass that discards
    # release notes, which is the outcome #2996 exists to prevent.
    #
    # Whole-line set difference, blanks stripped from BOTH sides first, so adding
    # a blank line is not an entry either. That subsumes the former `-z` clause —
    # an empty HEAD body contributes no lines and so cannot produce an addition.
    ADDED_UNREL=$(printf '%s\n' "$HEAD_UNREL_R4" \
      | grep -vE '^[[:space:]]*$' \
      | grep -Fxv -f <(printf '%s\n' "$BASE_UNREL_R4") || true)

    if [[ -z "$ADDED_UNREL" ]]; then
      echo "FAIL: this PR changes a client- or service-facing surface but adds no line under" >&2
      echo "'## [Unreleased]' in CHANGELOG.md. That section is what the desktop post-update" >&2
      echo "modal renders to users after they update (#1857), and the promoter turns" >&2
      echo "it into the release notes." >&2
      printf '%s\n' "$RELEVANT" | sed 's/^/  /' >&2
      echo "If this change is genuinely not user-observable, add a commit trailer:" >&2
      echo "  Changelog-Exempt: <why a user cannot observe this>" >&2
      exit 1
    fi
  fi
fi

# ── Rule 5: a `### ` line at HEAD is a heading, not prose (#2372) ────────────
# Observed three times (PRs #3350, #3353, #3355), each landing a line reading
# `### Added <a full sentence of release-note prose>`.
#
# NOT author error, which is why no rule keyed on "content in the wrong
# section" can see it. Each branch appended a sentence to the END of the
# `## [Unreleased]` body — correct when written. Main then PROMOTED that body
# under a dated heading while the branch was open, so the position the branch
# appended to is now occupied by the NEW section's `### Added` heading, and the
# merge applies the append to that line. Git reports no conflict: one side
# appended to a line the other side moved. A promotion racing an open branch.
#
# Checked at HEAD rather than in the diff, and for the same reason Rule 1 is:
# the corruption ARRIVES BY MERGE, so it is frequently not in this PR's own
# added lines at all. Presence-at-HEAD sees it regardless of who merged what.
#
# The test is SHAPE, not vocabulary. This file's `### ` lines are not confined
# to the Keep-a-Changelog six — it also carries `### Deferred / follow-up`,
# `### Documentation`, `### Known Issues`, `### Migration from v0.1.0-Alpha`
# and two `### Fixed (carried over from unshipped v0.1.36–v0.1.38)` variants —
# so a keyword allowlist would fail against main on its first run. A heading is
# a short title; the corruption is a prose sentence. The longest real heading
# is 59 BYTES and the observed corruption was 259, so 80 (the ordinary markdown
# line width) separates them with 21 bytes of margin. That heading is 57
# characters — its en dash costs 3 — so an em-dash-heavy heading spends the
# margin three times faster than a character count implies.
#
# `LC_ALL=C` is what makes "bytes" a GUARANTEE rather than an observation. mawk
# (CI) and macOS awk (the local suite) both count bytes regardless, but gawk in
# a UTF-8 locale counts CHARACTERS, so an 80-character heading could be 81+
# bytes and slip through on a runner that happens to ship gawk. Pinning the
# locale costs one token and removes the dependency on which awk is installed.
# Deliberately NOT accompanied by a regression case: measured on both available
# runtimes, the assertion is identical with and without the pin, so such a test
# could not fail if the pin were deleted — a vacuous test, which is the defect
# this suite has already had to remove once (see case 43).
#
# The sentence arm catches a short append the length arm cannot see. It is
# DELIBERATELY NOT the obvious character class: `)` is excluded because two real
# headings end in one (`### Changed (carried over from unshipped …v0.1.38)`), so
# admitting it would red main on the first run — the exact false positive the
# keyword-allowlist idea was rejected for. Trailing whitespace is absorbed
# because `.pre-commit-config.yaml` excludes `*.md` from the trailing-whitespace
# hook, so nothing in the repo normalises it, and two trailing spaces is the
# markdown hard-break idiom that ordinary release-note prose carries.
#
# RESIDUAL, stated rather than implied: a short append with no terminal
# punctuation ("### Added the reason now appears above") passes both arms. The
# arm narrows the gap; it does not close it, and no test should be read as
# claiming otherwise.
#
# SCOPE, recorded so it is a decision rather than an oversight. Rule 5 reads
# `^### ` only. The same merge race can land on the `## [X.Y.Z] — <date>` line
# the promoter writes directly above, and Rule 1's check is a PREFIX match, so
# trailing prose after the `]` reds nothing. That variant is uncovered by this
# gate. Not fixed here: it needs its own rule with its own evidence, and no
# instance of it has been observed.
#
# Also recorded: the `|| true` means an unreadable CHANGELOG.md yields an empty
# result and Rule 5 passes. That is the CORRECT answer — a file with no `### `
# lines has no bad ones — and unlike the MERGE_BASE defect above there is no
# degraded value manufacturing a verdict. A broken awk program would also pass,
# but cases 42/45/46 red the suite when it is, so the script is pinned.
#
# Deliberately NOT fence-aware, unlike the promoter's category_boundary in
# promote_unreleased_changelog.py next door — MEASURED, not overlooked:
# CHANGELOG.md has zero code fences at HEAD and no fenced `### ` line in any of
# its last 40 revisions, so the tracking would guard a case that has never
# occurred. Add it if a fenced example ever needs to carry one.
#
# ORDERING: like Rules 3 and 4, this must precede Rule 1 — every path through
# Rule 1 exits, so a rule placed after it is unreachable.
BAD_HEADINGS=$(git show HEAD:CHANGELOG.md 2>/dev/null | LC_ALL=C awk '
  /^### / && (length($0) > 80 || /[.!?][[:space:]]*$/) { printf "  line %d: %s\n", NR, $0 }
' || true)

if [[ -n "$BAD_HEADINGS" ]]; then
  echo "FAIL: CHANGELOG.md has a '### ' line carrying prose instead of a section name:" >&2
  printf '%s\n' "$BAD_HEADINGS" >&2
  echo "You almost certainly did not write this. A promotion on main moved the line your" >&2
  echo "branch appended to, and git merged the append onto the new section heading without" >&2
  echo "reporting a conflict." >&2
  echo "Fix: split the line. Leave the heading bare ('### Added') and move the prose down" >&2
  echo "to a '- ' bullet in whichever section it belongs to — usually '## [Unreleased]'," >&2
  echo "since that is where your branch was writing it." >&2
  exit 1
fi

# ── Rule 1: version bump requires a matching section at HEAD ─────────────────
# sed parse (not jq) for portability with the hermetic test suite (macOS).
OLD_VERSION=$(git show "${MERGE_BASE}":client/desktop/package.json 2>/dev/null \
  | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | first_line || true)
OLD_VERSION="${OLD_VERSION:-0.0.0}"
NEW_VERSION=$(git show HEAD:client/desktop/package.json 2>/dev/null \
  | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | first_line || true)
NEW_VERSION="${NEW_VERSION:-0.0.0}"

if [[ "$OLD_VERSION" == "$NEW_VERSION" ]]; then
  echo "PASS: no desktop version change (${OLD_VERSION})."
  exit 0
fi

# Downgrade (revert PR): the target version's section already exists — pass.
# ASSUMPTION (deliberate): package.json versions are plain x.y.z triples — the
# repo has never shipped a suffixed manifest version (suffixes like "-Beta"
# exist only in CHANGELOG heading labels). `sort -V` orders "0.2.0-Beta" BELOW
# "0.2.0", so a hypothetical plain→suffixed bump would misread as a downgrade;
# with plain triples the comparison is exact. `sort -V` also requires GNU-ish
# sort (present on ubuntu-latest CI and modern macOS for the local test suite).
# `head -1` KEPT here, deliberately, where every other site in this file moved to
# first_line. sort cannot emit a line until it has read all of its input, so it
# writes its two-line result once and the first write into head always lands —
# there is no second write to take SIGPIPE. The race needs a producer that is
# still writing when head exits; sort structurally is not one.
LOWER=$(printf '%s\n%s\n' "$NEW_VERSION" "$OLD_VERSION" | sort -V | head -1)
if [[ "$LOWER" == "$NEW_VERSION" ]]; then
  echo "PASS: version downgrade ${OLD_VERSION} -> ${NEW_VERSION} (revert); no new entry required."
  exit 0
fi

NEW_ESC=$(printf '%s' "$NEW_VERSION" | sed 's/\./\\./g')
# grep WITHOUT -q (see Rule 2 note): with -q, a match near the top of a large
# CHANGELOG SIGPIPEs `git show` under pipefail and the pipeline reads as FAIL
# despite the section existing (found live on PR #2034's own bump commit).
if git show HEAD:CHANGELOG.md 2>/dev/null | grep -E "^## \[${NEW_ESC}(-[A-Za-z0-9.]+)?\]" >/dev/null; then
  echo "PASS: version bump ${OLD_VERSION} -> ${NEW_VERSION} has a matching CHANGELOG.md section."
  exit 0
fi

echo "FAIL: desktop version bumped ${OLD_VERSION} -> ${NEW_VERSION} but CHANGELOG.md has no '## [${NEW_VERSION}]' section at HEAD." >&2
echo "Add a Keep-a-Changelog entry for ${NEW_VERSION} (see docs/release/desktop-release-checklist.md)." >&2
echo "Tip: the release-notes skill generates categorized content to seed the entry." >&2
exit 1
