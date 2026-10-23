#!/usr/bin/env bash
#
# verify-changelog.test.sh — hermetic tests for verify-changelog.sh (#1857).
# Builds throwaway git repos under mktemp and asserts exit codes. No network.
set -euo pipefail

# pre-commit exports GIT_DIR; a fixture `git init` under an inherited GIT_DIR
# would re-init the REAL repo (founding incident: the pre-commit GIT_DIR
# corruption class). Always unset both.
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/../verify-changelog.sh"

PASS=0
FAIL=0

# make_repo <version> — new fixture repo with package.json at <version> and a
# CHANGELOG containing an entry for 0.2.20 and the suffixed 0.2.0-Beta entry.
make_repo() {
  local version="$1"
  local dir
  dir=$(mktemp -d)
  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name test
  mkdir -p "$dir/client/desktop"
  printf '{\n  "name": "concord",\n  "version": "%s"\n}\n' "$version" > "$dir/client/desktop/package.json"
  cat > "$dir/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

## [0.2.20] — 2026-06-30

### Fixed

- Base entry.

## [0.2.0-Beta] — 2026-06-20

### Added

- Beta rollup.
EOF
  git -C "$dir" add -A
  git -C "$dir" commit -qm base
  echo "$dir"
}

# run_case <name> <expected_exit> <repo_dir>
run_case() {
  local name="$1" expected="$2" dir="$3" back="${4:-1}"
  local base
  # The `|| rev-parse HEAD` fallback exists for single-commit fixtures. With the
  # depth now variable it became a hazard: base == HEAD is an EMPTY diff, so
  # Rule 4 fast-exits and the gate returns 0 — turning a mis-specified fixture
  # into a green pass. Case 23 is the only back>1 case AND it expects 0, so it
  # was one commit away from vacuous. Fall back only at depth 1.
  if ! base=$(git -C "$dir" rev-parse "HEAD~${back}" 2>/dev/null); then
    if [[ "$back" != "1" ]]; then
      echo "FAIL: $name (fixture has no HEAD~${back} — case is mis-specified)"
      FAIL=$((FAIL + 1))
      return
    fi
    base=$(git -C "$dir" rev-parse HEAD)
  fi
  local actual=0
  (cd "$dir" && BASE_SHA="$base" GITHUB_BASE_REF="" bash "$SCRIPT") >/dev/null 2>&1 || actual=$?
  if [[ "$actual" == "$expected" ]]; then
    echo "PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name (expected exit $expected, got $actual)"
    FAIL=$((FAIL + 1))
  fi
}

# run_case with an explicit base revision rather than a depth. Needed for the
# merge-base anchoring case, whose base is a sibling branch tip, not an ancestor.
run_case_at() { # <name> <expected> <dir> <base_rev>
  local name="$1" expected="$2" dir="$3" base="$4"
  local actual=0
  (cd "$dir" && BASE_SHA="$base" GITHUB_BASE_REF="" bash "$SCRIPT") >/dev/null 2>&1 || actual=$?
  if [[ "$actual" == "$expected" ]]; then
    echo "PASS: $name"; PASS=$((PASS + 1))
  else
    echo "FAIL: $name (expected exit $expected, got $actual)"; FAIL=$((FAIL + 1))
  fi
}

set_version() { # <dir> <version>
  printf '{\n  "name": "concord",\n  "version": "%s"\n}\n' "$2" > "$1/client/desktop/package.json"
}

add_entry() { # <dir> <label>
  # Insert a version section directly BELOW the '## [Unreleased]' heading —
  # where promote-unreleased-changelog.sh puts it. This used to prepend at
  # NR==2, which was equivalent while the fixture had no [Unreleased]; once it
  # does, NR==2 would stack version sections ABOVE it and invert the file's
  # real shape, so the promotion-shape rule (#2996) would be tested against a
  # layout that never occurs.
  local dir="$1" label="$2"
  awk -v label="$label" '
    /^## \[Unreleased\]/ {
      print; print "";
      print "## [" label "] — 2026-07-02"; print "";
      print "### Added"; print ""; print "- New entry.";
      next
    }
    { print }
  ' "$dir/CHANGELOG.md" > "$dir/CHANGELOG.md.tmp" && mv "$dir/CHANGELOG.md.tmp" "$dir/CHANGELOG.md"
}

seed_unreleased() { # <dir> <bullet text>
  # Put a bullet under '## [Unreleased]'. Rule 3's "base [Unreleased] was
  # non-empty" precondition needs a fixture that HAS something to promote.
  awk -v line="- $2" '
    /^## \[Unreleased\]/ { print; print ""; print line; next }
    { print }
  ' "$1/CHANGELOG.md" > "$1/CHANGELOG.md.tmp" && mv "$1/CHANGELOG.md.tmp" "$1/CHANGELOG.md"
}

append_unreleased_bullet() { # <dir> <bullet text>
  # Append to the END of the '## [Unreleased]' body, which is where a SECOND
  # PR's note lands. seed_unreleased inserts directly under the heading; on a
  # promotable fixture that displaces the preamble into the body and the
  # promoter then rejects it, so the two helpers are not interchangeable.
  awk -v line="- $2" '
    /^## \[Unreleased\]/ { inu = 1; print; next }
    inu && /^## \[/       { print line; print ""; inu = 0; print; next }
    { print }
  ' "$1/CHANGELOG.md" > "$1/CHANGELOG.md.tmp" && mv "$1/CHANGELOG.md.tmp" "$1/CHANGELOG.md"
}

seed_unreleased_promotable() { # <dir> <preamble> <bullet text>
  # Like seed_unreleased, but shaped so the PROMOTER accepts it too.
  #
  # seed_unreleased writes a bare bullet with NO category heading, which is all
  # verify-changelog.sh Rule 4 needs. promote-unreleased-changelog.sh is stricter
  # since #3139: it mirrors extractPreamble in
  # client/desktop/src/renderer/services/system/changelog.ts, where a body with
  # no `### ` heading is ALL detail and therefore has no preamble at all. Case 36
  # is the only case that runs the real promoter, so only it needs this shape.
  awk -v pre="$2" -v line="- $3" '
    /^## \[Unreleased\]/ {
      print; print ""; print pre; print ""; print "### Added"; print ""; print line; next
    }
    { print }
  ' "$1/CHANGELOG.md" > "$1/CHANGELOG.md.tmp" && mv "$1/CHANGELOG.md.tmp" "$1/CHANGELOG.md"
}

clear_unreleased() { # <dir>
  # Delete everything between '## [Unreleased]' and the next '## [', keeping
  # the heading. This is exactly what promotion does to the section.
  awk '
    /^## \[Unreleased\]/ { print; inu = 1; next }
    inu && /^## \[/      { inu = 0 }
    inu                  { next }
    { print }
  ' "$1/CHANGELOG.md" > "$1/CHANGELOG.md.tmp" && mv "$1/CHANGELOG.md.tmp" "$1/CHANGELOG.md"
}

commit_all() { git -C "$1" add -A && git -C "$1" commit -qm change; }

# make_repo produces NO include-set files, which is why every case above is
# unaffected by Rule 4 — its path filter matches nothing, so the rule fast-exits.
# Rule 4's own cases have to create them.
touch_include() { mkdir -p "$1/client/desktop/src" && echo "// $2" >> "$1/client/desktop/src/app.ts"; }
touch_test()    { mkdir -p "$1/client/desktop/src/__tests__" && echo "// $2" >> "$1/client/desktop/src/__tests__/app.test.ts"; }
# A file inside a test DIRECTORY that is not itself named *.test.ts. Without it
# the directory filter is unfalsifiable: app.test.ts is caught by the filename
# filter too, so deleting the directory filter changes no result.
touch_testdir()  { mkdir -p "$1/client/desktop/src/__tests__" && echo "// $2" >> "$1/client/desktop/src/__tests__/helpers.ts"; }
# Two-thirds of Rule 4's include set had NO fixture: nothing put a non-exempt
# file under services/control-plane or services/media-plane, so deleting either
# from the regex survived the whole suite. The Go backend is exactly where the
# 0.2.41 under-coverage happened, which makes that the worst place to be blind.
touch_backend()  { mkdir -p "$1/services/control-plane" && echo "// $2" >> "$1/services/control-plane/handler.go"; }
touch_media()    { mkdir -p "$1/services/media-plane/src" && echo "// $2" >> "$1/services/media-plane/src/router.ts"; }
touch_gotest()   { mkdir -p "$1/services/control-plane" && echo "// $2" >> "$1/services/control-plane/handler_test.go"; }
touch_testdata() { mkdir -p "$1/services/control-plane/testdata" && echo "$2" >> "$1/services/control-plane/testdata/fixture.json"; }
touch_gomod()    { mkdir -p "$1/services/control-plane" && printf 'module x\n// %s\n' "$2" > "$1/services/control-plane/go.mod"; }
touch_svcdoc()   { mkdir -p "$1/services/control-plane" && echo "$2" >> "$1/services/control-plane/README.md"; }
touch_testhelp() { mkdir -p "$1/services/control-plane/internal/testhelpers" && echo "// $2" >> "$1/services/control-plane/internal/testhelpers/fixtures.go"; }
touch_admin()    { mkdir -p "$1/client/admin/src" && echo "// $2" >> "$1/client/admin/src/panel.tsx"; }

touch_manifest() { mkdir -p "$1/services/media-plane" && printf '{"version":"1.0.%s"}\n' "$2" > "$1/services/media-plane/package.json"; }

commit_trailer() { # <dir> <trailer line>
  git -C "$1" add -A
  printf 'chore: refactor\n\n%s\n' "$2" > "$1/.msg"
  git -C "$1" commit -q -F "$1/.msg"
}

# ── Case 1: no version change → PASS ──────────────────────────────────────────
r=$(make_repo 0.2.20)
echo "extra" >> "$r/CHANGELOG.md" && commit_all "$r"
run_case "no-bump passes" 0 "$r"

# ── Case 2: bump WITH matching entry → PASS ───────────────────────────────────
# Promotion-shaped since #2996: base [Unreleased] carries something, HEAD's is
# emptied, and the heading matches package.json. Rule 3 requires all three, so
# this case now models a real promotion rather than a bare hand-written section.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && set_version "$r" 0.2.21 && add_entry "$r" 0.2.21 && commit_all "$r"
run_case "bump with entry passes" 0 "$r"

# ── Case 3: bump WITHOUT entry → FAIL ─────────────────────────────────────────
r=$(make_repo 0.2.20)
set_version "$r" 0.2.21 && commit_all "$r"
run_case "bump without entry fails" 1 "$r"

# ── Case 4: bump with WRONG-version entry → FAIL ──────────────────────────────
# The wrong-version section is PRE-EXISTING at the base rather than added by
# this PR. That keeps the case honest about which rule catches it: Rule 3 fires
# only on an ADDED heading, so it stays out of the way and Rule 1 — whose
# coverage this case exists for — is what fails. Written the old way (add the
# heading in the PR) it would now fail on Rule 3's version mismatch instead,
# passing for a reason its name does not describe.
r=$(make_repo 0.2.20)
add_entry "$r" 0.2.22 && commit_all "$r"
set_version "$r" 0.2.21 && commit_all "$r"
run_case "bump with wrong-version entry fails" 1 "$r"

# ── Case 5: downgrade (revert PR) → PASS ──────────────────────────────────────
r=$(make_repo 0.2.21)
set_version "$r" 0.2.20 && commit_all "$r"
run_case "downgrade/revert passes" 0 "$r"

# ── Case 6: added private-tree ref line → FAIL (no bump needed) ───────────────
r=$(make_repo 0.2.20)
echo "- See [internal]rules/backend.md for details." >> "$r/CHANGELOG.md" && commit_all "$r"
run_case "added private-ref line fails" 1 "$r"

# ── Case 7: suffixed label satisfies the numeric bump → PASS ──────────────────
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && set_version "$r" 0.3.0 && add_entry "$r" 0.3.0-Beta && commit_all "$r"
run_case "suffixed label satisfies bump" 0 "$r"

# ── Case 8: LARGE changelog — pipefail/SIGPIPE regression (#2034) ─────────────
# With `grep -q`, a match near the top of a >64KB file SIGPIPEs the still-
# writing `git show` under pipefail and the gate false-FAILs. Entry on top,
# ~150KB of filler below reproduces it deterministically.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && set_version "$r" 0.2.21 && add_entry "$r" 0.2.21
for i in $(seq 1 3000); do echo "- filler line $i for pipe-buffer overflow" >> "$r/CHANGELOG.md"; done
commit_all "$r"
run_case "large changelog does not false-fail (SIGPIPE)" 0 "$r"

# ── Case 9: version heading with an EMPTY base [Unreleased] → FAIL (#2996) ────
# This is desktop-v0.2.41's shape exactly: a feature PR bumps the version and
# hand-writes a section describing only itself, leaving [Unreleased] empty so
# the promoter exits 3 with nothing to promote.
r=$(make_repo 0.2.20)
set_version "$r" 0.2.21 && add_entry "$r" 0.2.21 && commit_all "$r"
run_case "version heading with empty base Unreleased fails" 1 "$r"

# ── Case 10: promoting without emptying [Unreleased] → FAIL (#2996) ───────────
# The second seed_unreleased is load-bearing and must come AFTER add_entry.
# add_entry inserts its section immediately below the '## [Unreleased]' heading,
# which pushes any already-seeded bullet down INTO the new section — leaving
# [Unreleased] empty at HEAD and quietly satisfying the very condition this case
# exists to violate. Seeding again afterwards puts a bullet back above the new
# heading, which is the state under test.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
set_version "$r" 0.2.21 && add_entry "$r" 0.2.21 && seed_unreleased "$r" "Still unreleased." \
  && commit_all "$r"
run_case "version heading without emptying Unreleased fails" 1 "$r"

# ── Case 11: heading version not matching package.json → FAIL (#2996) ─────────
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && set_version "$r" 0.2.21 && add_entry "$r" 0.2.99 && commit_all "$r"
run_case "promotion heading not matching package.json fails" 1 "$r"

# ── Case 12: bullets added to an EXISTING section → PASS (#2996) ──────────────
# The backfill case. Rule 3 keys on a heading being ADDED, so correcting an
# already-published section must not trip it — otherwise this change blocks
# its own 0.2.41 backfill.
r=$(make_repo 0.2.20)
awk '/^## \[0\.2\.20\]/ { print; print ""; print "- Backfilled bullet."; next } { print }' \
  "$r/CHANGELOG.md" > "$r/c.tmp" && mv "$r/c.tmp" "$r/CHANGELOG.md"
commit_all "$r"
run_case "adding bullets to an existing section passes" 0 "$r"

# ── Case 13: version heading with NO version bump → FAIL (#2996) ─────────────
# The only case where Rule 3's version-match check is load-bearing. Rule 1 exits
# 0 early on "no desktop version change", so with a bump absent it never looks
# at the heading at all — a PR can hand-write '## [0.9.9]' for a release that is
# not happening and Rule 1 waves it through. Found by falsification: dropping
# Rule 3's version check broke no test until this case existed, because every
# other mismatch case carried a bump and was caught by Rule 1 instead.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && add_entry "$r" 0.9.9 && commit_all "$r"
run_case "version heading without a version bump fails" 1 "$r"

# ── Case 14: include-set change with no [Unreleased] entry → FAIL (#2996) ─────
# Rule 4. The complement of Rule 3: Rule 3 stops notes being written in the
# wrong PLACE, Rule 4 stops them not being written at all. Without it a change
# a user can observe merges silently and the promoter has nothing to promote,
# which is the other half of how 0.2.41 shipped 98 commits behind one bullet.
r=$(make_repo 0.2.20); touch_include "$r" change; commit_all "$r"
run_case "include-set change without an Unreleased entry fails" 1 "$r"

# ── Case 15: the same change with an entry → PASS ────────────────────────────
r=$(make_repo 0.2.20); touch_include "$r" change; seed_unreleased "$r" "Described it."; commit_all "$r"
run_case "include-set change with an Unreleased entry passes" 0 "$r"

# ── Case 16: test-only paths are exempt → PASS ───────────────────────────────
r=$(make_repo 0.2.20); touch_test "$r" change; commit_all "$r"
run_case "test-only change is exempt" 0 "$r"

# ── Case 17: dependency manifests are exempt → PASS ──────────────────────────
# Bot dependency PRs would otherwise need a standing label ritual to stay green.
r=$(make_repo 0.2.20); touch_manifest "$r" 1; commit_all "$r"
run_case "dependency manifest change is exempt" 0 "$r"

# ── Case 18: Changelog-Exempt trailer WITH a reason → PASS ───────────────────
r=$(make_repo 0.2.20); touch_include "$r" change
commit_trailer "$r" "Changelog-Exempt: pure refactor, no observable behaviour change"
run_case "Changelog-Exempt trailer with a reason passes" 0 "$r"

# ── Case 19: Changelog-Exempt trailer with NO reason → FAIL ──────────────────
# git's own trailer parser emits an empty value for a bare `Key:`, so the escape
# hatch cannot be taken without writing down why. Verified against git directly
# rather than assumed: valueonly returns "" for `Changelog-Exempt:`.
r=$(make_repo 0.2.20); touch_include "$r" change
commit_trailer "$r" "Changelog-Exempt:"
run_case "Changelog-Exempt trailer with no reason fails" 1 "$r"

# ── Case 20: a non-include-set change never reaches Rule 4 → PASS ────────────
r=$(make_repo 0.2.20); echo "note" >> "$r/README.md"; commit_all "$r"
run_case "non-include-set change is unaffected" 0 "$r"

# ── Case 21: non-empty base [Unreleased], no new entry → FAIL (#2996) ────────
# THE PRODUCTION SHAPE, and the only case that exercises the `== BASE` half of
# Rule 4. Once main carries release notes for the next version — which it does
# from this PR onward — [Unreleased] is never empty at a PR's merge base, so the
# `-z` half stops firing entirely and this clause does all the work. Found by
# falsification: replacing the condition with the `-z` test alone broke nothing.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "Notes for the next release." && commit_all "$r"
touch_include "$r" change && commit_all "$r"
run_case "include-set change on a populated Unreleased base fails" 1 "$r"

# ── Case 22: non-test file inside a test directory → PASS ───────────────────
r=$(make_repo 0.2.20); touch_testdir "$r" change; commit_all "$r"
run_case "test-directory helper is exempt" 0 "$r"

# ── Case 23: trailer on an EARLIER commit of a multi-commit PR → PASS ───────
# git emits one line per commit, blank where the trailer is absent, and `git
# log` is REVERSE-chronological — so the blank that can mask a real reason is
# the NEWEST commit's. Ordering matters and was got wrong first time: written
# with the trailer on the newest commit the case passes either way, because
# `head -1` reaches the real value before any blank, and the mutation survived.
# The realistic shape is this one: record the exemption, then push a follow-up
# (a lint fix, a review nit) that carries no trailer. Unfiltered, `head -1`
# reads that commit's blank and the exemption is silently lost.
r=$(make_repo 0.2.20)
touch_include "$r" first
commit_trailer "$r" "Changelog-Exempt: pure refactor, no observable behaviour change"
touch_include "$r" second && commit_all "$r"
run_case "Changelog-Exempt on an earlier commit still passes" 0 "$r" 2

# ── Cases 24-25: the other two-thirds of the include set (#3006 review) ──────
# Deleting `services/control-plane/` or `services/media-plane/` from Rule 4's
# include regex survived all 23 prior cases: no fixture put a NON-EXEMPT file
# under either. Silent non-enforcement on the Go backend is the 0.2.41 shape.
r=$(make_repo 0.2.20); touch_backend "$r" change; commit_all "$r"
run_case "control-plane change without an Unreleased entry fails" 1 "$r"

r=$(make_repo 0.2.20); touch_media "$r" change; commit_all "$r"
run_case "media-plane change without an Unreleased entry fails" 1 "$r"

# ── Cases 26-28: the exemptions, each pinned individually ───────────────────
# The prior mutation pass deleted whole filters; no single ALTERNATIVE inside
# one was pinned. control-plane is Go with hundreds of _test.go files, so a
# Go-test-only PR is the likeliest exempt PR in the repo and nothing covered it.
r=$(make_repo 0.2.20); touch_gotest "$r" change; commit_all "$r"
run_case "Go test-only change is exempt" 0 "$r"

r=$(make_repo 0.2.20); touch_testdata "$r" change; commit_all "$r"
run_case "Go testdata change is exempt" 0 "$r"

r=$(make_repo 0.2.20); touch_gomod "$r" change; commit_all "$r"
run_case "go.mod change is exempt" 0 "$r"

# ── Case 29: docs/config under a service tree are not user-observable ───────
r=$(make_repo 0.2.20); touch_svcdoc "$r" note; commit_all "$r"
run_case "service README change is exempt" 0 "$r"

# ── Case 30: client/admin is deliberately OUTSIDE the include set ───────────
# The admin console is operator-facing; its changes are not rendered in the
# desktop post-update modal. The plan named this case and it was never written,
# so ADDING client/admin to the include set broke nothing.
r=$(make_repo 0.2.20); touch_admin "$r" change; commit_all "$r"
run_case "client/admin change is not gated" 0 "$r"

# ── Case 31: a DELETION is not an addition (#3006 review, severity 7) ───────
# The implementation tested body INEQUALITY while the contract, the comment and
# the failure message all said ADDITION. A PR that clobbers someone else's
# bullet and writes none of its own passed — which is not contrived, it is how a
# merge conflict on this single anchor gets resolved, so it was the LIKELY path.
r=$(make_repo 0.2.20)
seed_unreleased "$r" "Note two." && seed_unreleased "$r" "Note one." && commit_all "$r"
touch_include "$r" change && clear_unreleased "$r" && seed_unreleased "$r" "Note one."
commit_all "$r"
run_case "deleting an Unreleased bullet does not satisfy Rule 4" 1 "$r"

# ── Case 32: a blank line is not an entry either ────────────────────────────
r=$(make_repo 0.2.20)
seed_unreleased "$r" "Existing note." && commit_all "$r"
touch_include "$r" change
printf '\n' >> "$r/CHANGELOG.md"
commit_all "$r"
run_case "adding only whitespace does not satisfy Rule 4" 1 "$r"

# ── Case 33: every added heading is checked, not just the first ─────────────
r=$(make_repo 0.2.20)
seed_unreleased "$r" "A human wrote this." && commit_all "$r"
clear_unreleased "$r" && set_version "$r" 0.2.21 && add_entry "$r" 0.9.9 && add_entry "$r" 0.2.21
commit_all "$r"
run_case "a second added heading is not smuggled past Rule 3" 1 "$r"

# ── Case 34: correcting a PUBLISHED heading is not a promotion ──────────────
# A unified diff marks a modified line `+`, so a date typo fix looked exactly
# like adding a section — and since main now always carries a populated
# [Unreleased], it landed in the "still populated at HEAD" branch every time,
# with a message telling the author to move it under [Unreleased].
r=$(make_repo 0.2.20)
seed_unreleased "$r" "Notes for next." && commit_all "$r"
sed 's/## \[0\.2\.20\] — 2026-06-30/## [0.2.20] — 2026-06-29/' "$r/CHANGELOG.md" > "$r/c.tmp" \
  && mv "$r/c.tmp" "$r/CHANGELOG.md"
commit_all "$r"
run_case "editing an already-published heading passes" 0 "$r"

# ── Case 35: merge-base anchoring, the only case with a real branch ─────────
# Every other case runs on linear history with GITHUB_BASE_REF="", so
# MERGE_BASE is identically BASE_SHA and the two-dot mutant survives the suite.
# Rules 3 and 4 newly depend on that anchoring (#1568/#1572 stale-base class):
# a two-dot diff attributes main's files to this PR and demands an entry for a
# file it never touched.
r=$(make_repo 0.2.20)
FORK=$(git -C "$r" rev-parse HEAD)
touch_svcdoc "$r" "our own exempt change" && commit_all "$r"
PRTIP=$(git -C "$r" rev-parse HEAD)
git -C "$r" checkout -q -b othermain "$FORK"
touch_backend "$r" "someone else's un-noted backend change" && commit_all "$r"
OTHER=$(git -C "$r" rev-parse HEAD)
git -C "$r" checkout -q --detach "$PRTIP"
run_case_at "merge-base anchoring ignores main's own un-noted files" 0 "$r" "$OTHER"

# ── Case 36: the real promoter's output must satisfy the real gate ─────────
# The spec required this and it was never written. It is the one case that
# proves a release-cutting PR that runs the promoter is not blocked by the gate;
# verifying it by hand is not a regression pin.
r=$(make_repo 0.2.20)
seed_unreleased_promotable "$r" "A human summarised the release." "A human wrote this." \
  && commit_all "$r"
# Fail LOUDLY. The promoter runs under this suite's `set -euo pipefail`, so a
# non-zero exit here aborts the whole file and takes Cases 37-38 with it — which
# is precisely what happened when #3139's preamble gate met the old bare-bullet
# fixture: 35 cases reported, 36-38 silently never ran. Name the failure instead.
# Keep stderr. Discarding it made the failure line read "exit 1" while throwing
# away the promoter's own actionable message, which is the one thing that tells
# you WHY the fixture stopped satisfying it.
promoter_err=$( ( cd "$r" && bash "$SCRIPT_DIR/../promote-unreleased-changelog.sh" 0.2.21 2>&1 >/dev/null ) ) \
  || { printf 'FAIL: the promoter rejected Case 36'"'"'s fixture (exit %s)\n%s\n' "$?" "$promoter_err" >&2
       FAIL=$((FAIL + 1)); }
set_version "$r" 0.2.21 && commit_all "$r"
run_case "the real promoter's output satisfies Rule 3" 0 "$r"

# ── Case 37: testhelpers/ is exempt (mutation survivor) ────────────────────
# This repo's Go test-helper directory is `testhelpers`, not `testdata`. Added
# to the exemption during review with no case behind it, so deleting it again
# broke nothing.
r=$(make_repo 0.2.20); touch_testhelp "$r" change; commit_all "$r"
run_case "Go testhelpers change is exempt" 0 "$r"

# ── Case 38: an unresolvable merge base fails CLOSED (mutation survivor) ───
# The whole point of the anchor validation, and nothing exercised it: every
# other case supplies a real revision. Without the check this exits 0 with
# Rules 2/3/4 silently skipped — a gate reporting PASS having verified nothing.
r=$(make_repo 0.2.20); touch_include "$r" change; commit_all "$r"
run_case_at "an unresolvable merge base fails closed" 1 "$r" "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

# ── Case 39: CHANGELOG.md absent at the base (mutation survivor) ───────────
# unreleased_body is a `git show | awk` pipeline; under pipefail a missing file
# at either ref returns 128, and without `|| true` the bare substitution aborted
# the script with no message at all — outside the documented exit contract and
# undiagnosable from a CI log. Expect a normal Rule 3 failure (1), not 128.
r=$(mktemp -d)
git -C "$r" init -q
git -C "$r" config user.email test@example.com
git -C "$r" config user.name test
mkdir -p "$r/client/desktop"
printf '{\n  "name": "concord",\n  "version": "0.2.20"\n}\n' > "$r/client/desktop/package.json"
git -C "$r" add -A && git -C "$r" commit -qm "base without a changelog"
printf '# Changelog\n\n## [Unreleased]\n\n## [0.2.20] — 2026-06-30\n\n- New file.\n' > "$r/CHANGELOG.md"
commit_all "$r"
run_case "a PR that creates CHANGELOG.md exits 1, not 128" 1 "$r"

# ── Cases 40-41: one PR may carry a source change AND its own promotion ───
# Rules 3 and 4 used to be mutually exclusive for a source-touching PR: Rule 3
# requires a promotion to EMPTY [Unreleased], Rule 4 required a source change to
# ADD to it. A PR doing both satisfied neither, so the bump could only ever live
# in a source-free PR of its own — which is the shape the nightly `bump` job
# existed to produce, and why it was the only way to cut a release.
#
# Rule 4 now counts a note that landed in the section this PR promoted. Its
# subject is whether a user-facing change wrote a note, not which heading the
# note came to rest under, and Rule 3 has already proven the diff is a real
# promotion before Rule 4 looks at it.
promote_fixture() { # <dir> <version> — run the real promoter, fail loudly
  local err
  err=$( ( cd "$1" && bash "$SCRIPT_DIR/../promote-unreleased-changelog.sh" "$2" 2>&1 >/dev/null ) ) \
    || { printf 'FAIL: the promoter rejected a fixture (exit %s)\n%s\n' "$?" "$err" >&2
         FAIL=$((FAIL + 1)); }
}

# Case 40: the whole shape, end to end — an earlier PR's note is already
# pending, this PR adds a source change plus its own note, then promotes and
# bumps. This is a developer cutting a release from the PR that finishes it.
r=$(make_repo 0.2.20)
seed_unreleased_promotable "$r" "A human summarised the release." "An earlier PR's note." \
  && commit_all "$r"
touch_include "$r" "this PR's own feature"
append_unreleased_bullet "$r" "This PR's own note."
promote_fixture "$r" 0.2.21
set_version "$r" 0.2.21 && commit_all "$r"
run_case "a source-touching PR may bump and promote in one diff" 0 "$r"

# Case 41: the converse, and the reason Case 40 is not a bypass. Same promotion,
# same bump, same source change — but this PR contributes no note of its own, so
# it is shipping an un-described change under cover of someone else's notes.
# Without this case, Rule 4 could be satisfied by ANY promoted section and the
# fix would silently retire the rule for every bumping PR.
r=$(make_repo 0.2.20)
seed_unreleased_promotable "$r" "A human summarised the release." "An earlier PR's note." \
  && commit_all "$r"
touch_include "$r" "an un-noted change"
promote_fixture "$r" 0.2.21
set_version "$r" 0.2.21 && commit_all "$r"
run_case "a promoting PR that notes nothing of its own still fails Rule 4" 1 "$r"

# ── Cases 42-46: Rule 5, heading-line integrity (#2372) ─────────────────────
# set_added_heading rewrites the bare '### Added' line. The `!seen` guard plus
# `next` is what stops after the first match — NOT any awk per-file reset, which
# applies to FNR and not to user variables. make_repo's fixture carries exactly
# one `^### Added$` line, so the guard is belt-and-braces rather than load-
# bearing. Note the target sits under the OLDEST section at the bottom of the
# file, while the real corruption lands on the NEWEST section's heading at the
# top: Rule 5 scans every line regardless of position, so no assertion is
# weakened, but these fixtures model the SHAPE of the corruption, not the
# promotion race's geometry. If Rule 5 ever gains section awareness, they stop
# modelling the bug and must be repointed.
set_added_heading() { # <dir> <replacement line>
  awk -v repl="$2" '
    !seen && /^### Added$/ { print repl; seen = 1; next }
    { print }
  ' "$1/CHANGELOG.md" > "$1/CHANGELOG.md.tmp" && mv "$1/CHANGELOG.md.tmp" "$1/CHANGELOG.md"
}

# Case 42: the real corruption from PR #3350, verbatim. This is the regression.
# It pins the SHAPE, not the race: the fixture rewrites a heading in an already
# published section, whereas the race lands one on a freshly promoted heading.
# Rule 5 is position-independent so the assertion holds; the 'git reports no
# conflict' half of the mechanism is not testable here (it would be testing git)
# and its evidence is the three real PRs.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added If creating the invite does fail anyway — the list is a snapshot, so a permission removed while you had the chat open is not reflected until it refreshes — the reason now appears above the message box instead of the button quietly doing nothing."
commit_all "$r"
run_case "a '### ' heading carrying appended prose fails" 1 "$r"

# Case 43: the UPPER BOUNDARY, exactly 80 bytes -> PASS. This replaces a control
# that measured as vacuous: it called set_added_heading with the identical string
# '### Added', a byte-for-byte no-op, and the property it claimed to pin ("a rule
# rejecting every '### ' line would look like a working one") is already killed by
# 23 pre-existing cases, since every make_repo fixture carries '### Fixed' and
# '### Added'. The boundary is the thing nothing else pins: without this case the
# mutant `> 80` -> `>= 80` survives the whole suite.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added $(printf 'x%.0s' $(seq 1 70))"
commit_all "$r"
run_case "a heading of exactly 80 bytes passes" 0 "$r"

# Case 44: the shape rule is not a keyword allowlist, AND it is the sole pin on
# the threshold's upper side. The string is the LONGEST heading in the real
# CHANGELOG at 59 BYTES (57 characters — the en dash costs 3). The 57-byte
# '### Fixed' variant shipped here first and left the mutant `> 80` -> `> 58`
# alive: suite green, while the real CHANGELOG carried a line the gate rejects,
# i.e. main red on every PR with no test saying so. It sits alongside
# '### Deferred / follow-up' and '### Migration from v0.1.0-Alpha'. Narrowing
# Rule 5 to the Keep-a-Changelog six would fail the gate against main itself.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Changed (carried over from unshipped v0.1.36–v0.1.38)"
commit_all "$r"
run_case "a long-but-legitimate parenthesised heading passes" 0 "$r"

# Case 45: falsifies the TRAILING-PERIOD arm on its own. 54 chars, so it is
# under the length cap and only the period arm can catch it. Case 42's line is
# both long AND period-terminated, so without this case deleting `|| /\.$/`
# survives the suite.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added The reason now appears above the box."
commit_all "$r"
run_case "a short appended sentence on a heading fails" 1 "$r"

# Case 46: falsifies the LENGTH arm on its own — 100+ chars, no trailing
# period, so only `length($0) > 80` can catch it. The converse of case 45;
# together they pin both arms independently.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added the reason now appears above the message box instead of the button quietly doing nothing"
commit_all "$r"
run_case "a long appended clause with no trailing period fails" 1 "$r"

# Case 47: trailing whitespace must not defeat the sentence arm. 49 bytes, so
# the length arm cannot see it. TWO trailing spaces is markdown's hard-break
# idiom, and `.pre-commit-config.yaml` excludes `*.md` from the trailing-
# whitespace hook, so nothing in the repo strips it before the gate looks.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added The reason now appears above the box.  "
commit_all "$r"
run_case "trailing whitespace does not defeat the sentence arm" 1 "$r"

# Case 48: a sentence can end in '?' or '!', not only '.'. 44 bytes, under the
# length arm. Pins the character class against being narrowed back to /\.$/.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added Did the invite actually fail here?"
commit_all "$r"
run_case "a question-mark sentence on a heading fails" 1 "$r"

# Case 49: ')' is deliberately OUTSIDE the class and this is the case that says
# so. Two real headings end in ')', so admitting it would red main on the first
# run. 44 bytes, so only the sentence arm could fire — it must not.
r=$(make_repo 0.2.20)
set_added_heading "$r" "### Added the reason appears above (finally)"
commit_all "$r"
run_case "a heading ending in ')' is not treated as a sentence" 0 "$r"

# The allowed case Rule 5 must not regress — correcting an already-published
# '## [X.Y.Z] — <date>' heading — is CASE 34 above, and it is a real pin rather
# than a note: its fixture carries '### Fixed' and '### Added', so a Rule 5 that
# mis-fired on ordinary headings would turn that case red. Rule 5 reads only
# '^### ' lines and never sees a '## [' heading at all.

echo "----"
echo "verify-changelog.test.sh: $PASS passed, $FAIL failed"
[[ "$FAIL" == "0" ]]
