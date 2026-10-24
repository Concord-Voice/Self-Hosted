#!/usr/bin/env bash
#
# Self-test for promote-unreleased-changelog.sh.
#
# The load-bearing assertions are the two that protect humans' writing:
#   * an EMPTY Unreleased must exit 3 and leave the file byte-identical — that
#     path is the release decision ("nobody wrote notes, so do not ship"), not
#     an error case, and a script that bumped anyway would ship empty releases;
#   * a non-empty body must survive promotion VERBATIM — this script exists
#     precisely so no machine text replaces editorial prose.
#
# The last case closes the loop with verify-changelog.sh (#1857), the gate that
# forced this design: its rule 1 greps `^## \[<version>\]` at HEAD, so the
# promoted heading must match that pattern or the automation reds its own PR.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PROMOTE="$REPO_ROOT/.github/scripts/promote-unreleased-changelog.sh"
# The logic the wrapper runs; the STRIP_TAGS pin below reads its source.
PROMOTE_PY="$REPO_ROOT/.github/scripts/promote_unreleased_changelog.py"
# Inline Python in this suite runs as `python3 -c '...'`, never as a heredoc on
# stdin: a heredoc goes through a pipe, which is the hang the promoter moved to a
# file to avoid (see the comment above its python3 call). Keep those bodies free
# of single quotes.
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
n=0

FIXTURE_HEAD='# Changelog

All notable changes to Concord Voice will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]
'
FIXTURE_TAIL='
## [0.2.40] — 2026-08-20

Prose summary in the house voice.

### Added

- **A shipped thing** ([#945](https://example.invalid/945)) — description.
'

# make_changelog <unreleased-body>
make_changelog() {
  # mktemp, NOT a counter: this function is always called inside $( ), so
  # `n=$((n+1))` increments in a SUBSHELL and never reaches the parent. Every
  # fixture then resolved to case-1 and each call overwrote the last — proven
  # live, and harmless only because every fixture happened to be consumed
  # immediately. The twin suite hit exactly this and fixed it the same way.
  CASE=$(mktemp -d "$TMP/case-XXXXXX")
  echo "$CASE" >> "$TMP/builder-calls"
  printf '%s%s%s' "$FIXTURE_HEAD" "$1" "$FIXTURE_TAIL" > "$CASE/CHANGELOG.md"
  echo "$CASE/CHANGELOG.md"
}

check() {
  local expected="$1" label="$2"
  shift 2
  local out actual
  set +e
  out=$("$@" 2>&1)
  actual=$?
  set -e
  if [[ "$actual" == "$expected" ]]; then
    echo "  PASS: $label"
    pass=$((pass + 1))
  else
    echo "  FAIL: $label (expected exit $expected, got $actual)" >&2
    printf '    %s\n' "$out" >&2
    fail=$((fail + 1))
  fi
}

# check_msg <expected-exit> <grep-ERE> <label> <cmd...>
# `check` asserts the exit code only, which cannot tell the preamble gate apart
# from a Python traceback (both exit 1) and leaves the OPERATOR-FACING TEXT
# unpinned. That text is duplicated into classify-release.yml and the release
# checklist, so an unasserted copy here is the one that drifts first.
check_msg() {
  local expected="$1" want="$2" label="$3"
  shift 3
  local out actual
  set +e
  out=$("$@" 2>&1)
  actual=$?
  set -e
  if [[ "$actual" == "$expected" ]] && grep -Eq "$want" < <(printf '%s' "$out"); then
    echo "  PASS: $label"
    pass=$((pass + 1))
  else
    echo "  FAIL: $label (exit $actual, wanted $expected matching /$want/)" >&2
    printf '    %s\n' "$out" >&2
    fail=$((fail + 1))
  fi
}

assert_true() {
  local label="$1" cond="$2"
  if [[ "$cond" == "yes" ]]; then
    echo "  PASS: $label"
    pass=$((pass + 1))
  else
    echo "  FAIL: $label" >&2
    fail=$((fail + 1))
  fi
}

echo "Nothing-to-release (exit 3, file untouched):"

f=$(make_changelog '')
before=$(shasum "$f" | cut -d' ' -f1)
check 3 "empty Unreleased exits 3" "$PROMOTE" 0.2.41 2026-08-26 "$f"
after=$(shasum "$f" | cut -d' ' -f1)
assert_true "empty Unreleased leaves the file byte-identical" \
  "$([[ "$before" == "$after" ]] && echo yes || echo no)"

f=$(make_changelog '


')
check 3 "whitespace-only Unreleased exits 3" "$PROMOTE" 0.2.41 2026-08-26 "$f"

echo ""
echo "Malformed input:"

f=$(make_changelog 'x')
python3 -c '
import pathlib, re, sys
p = pathlib.Path(sys.argv[1])
p.write_text(re.sub(r"(?m)^## \[Unreleased\]\n", "", p.read_text(), count=1))
' "$f"
check 1 "no Unreleased heading exits 1" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# The preamble is LOAD-BEARING here, and its absence made this case vacuous the
# moment the preamble gate was added: the old fixture started at `### Added`, so
# with the duplicate-version guard deleted the section merely failed the NEW gate
# and still exited 1. Mutation-deleting that guard left all 28 cases green — a
# regression permitting duplicate release sections would have shipped unseen.
# With a valid preamble the only remaining reason for exit 1 is the duplicate,
# and the message assertion separates that diagnosis from a crash.
f=$(make_changelog '
A summary sentence so only the duplicate-version guard can reject this.

### Added

- thing
')
check_msg 1 "already exists" "target version already present exits 1" \
  "$PROMOTE" 0.2.40 2026-08-26 "$f"
check 2 "non-semver version exits 2" "$PROMOTE" v0.2.41 2026-08-26 "$f"
check 2 "malformed date exits 2" "$PROMOTE" 0.2.41 26-08-2026 "$f"
check 2 "missing changelog exits 2" "$PROMOTE" 0.2.41 2026-08-26 "$TMP/nope.md"
check 2 "no arguments exits 2" "$PROMOTE"

echo ""
echo "Promotion:"

# The hard break and the trailing blank lines are LOAD-BEARING fixture content:
# in Markdown two trailing spaces are a hard line break, so a body.rstrip() would
# change how a human's release note renders.
#
# The two spaces are built at RUNTIME rather than typed literally. Typed
# literally, the repo's own `trailing-whitespace` pre-commit hook strips them out
# of this file and quietly disarms the assertion they exist to support — observed
# happening on this very file, which then went green while testing nothing. A
# fixture that cannot survive its own repo's hooks is not a fixture.
HARD_BREAK=$(printf '  ')
BODY='
Sign-in got steadier and a friend link now goes somewhere useful.

### Added

- **A friend link now opens a real page** ([#945](https://example.invalid/945)) — deliberate house voice with an em dash — and a nested [link](https://example.invalid).
- A hard break lives at the end of this line.'"$HARD_BREAK"'
  Continued after the hard break.

### Fixed

- Something that was broken.


'
f=$(make_changelog "$BODY")
check 0 "non-empty Unreleased promotes" "$PROMOTE" 0.2.41 2026-08-26 "$f"

python3 -c '
import pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text()
body = sys.argv[2]
problems = []

if not re.search(r"(?m)^## \[0\.2\.41\] — 2026-08-26$", text):
    problems.append("dated heading missing or wrong em-dash/format")
# verify-changelog.sh:105 greps exactly this
if not re.search(r"(?m)^## \[0\.2\.41\](-[A-Za-z0-9.]+)?", text):
    problems.append("heading does not satisfy verify-changelog rule 1")
if not re.search(r"(?m)^## \[Unreleased\]\s*\n+## \[0\.2\.41\]", text):
    problems.append("fresh empty Unreleased not left above the release")
if text.index("## [Unreleased]") > text.index("## [0.2.41]"):
    problems.append("Unreleased is not first")
if text.index("## [0.2.41]") > text.index("## [0.2.40]"):
    problems.append("new release not above the previous one")
# BYTE-EXACT: the promoted file must contain the body as an unbroken,
# unmodified substring. A line-presence loop passes even when trailing
# whitespace was stripped, which is how a body.rstrip() survived review once.
if body not in text:
    problems.append("body was NOT preserved byte-for-byte")
    stripped = body.rstrip()
    if stripped and stripped in text:
        problems.append("body appears rstrip()-normalised — trailing whitespace lost")
if "## [0.2.40] — 2026-08-20" not in text:
    problems.append("previous release section damaged")
if problems:
    print("      " + "; ".join(problems), file=sys.stderr)
    raise SystemExit(1)
' "$f" "$BODY" && RESULT=yes || RESULT=no
assert_true "promoted file is structurally correct and preserves the body verbatim" "$RESULT"

# Promotion must be a one-shot: the fresh Unreleased is empty, so a second run
# on the same file is the nothing-to-release path, not a double promotion.
check 3 "re-running on the promoted file exits 3 (no double promotion)" \
  "$PROMOTE" 0.2.42 2026-08-27 "$f"

echo ""
echo "Preamble gate (#3139):"

# EVERY rejection below asserts the MESSAGE, not just exit 1. A crash and a
# diagnosis are both exit 1, so an exit-only assertion is satisfied by a
# traceback: a mutant raising only on the mismatched-fence input left this whole
# suite green at 28/0. The five branches deliberately share one message — the
# operator's next action is the same for all of them — so the pattern is shared;
# what it separates is the promoter REJECTING from the promoter CRASHING.

# The rule mirrors extractPreamble in
# client/desktop/src/renderer/services/system/changelog.ts -- structurally, not
# byte-for-byte: this side additionally models what the renderer paints, so it
# rejects some preambles the mirror accepts. These cases pin the
# two branches a NAIVE port gets wrong in the LOOSE direction — a heading-less
# body, and a fenced heading standing in for a real one. Both shapes exist in
# this repo already: verify-changelog.test.sh's own seed_unreleased writes the
# first one.

f=$(make_changelog '
### Added

- A bullet with no summary above it.
')
before=$(shasum "$f" | cut -d' ' -f1)
check_msg 1 "has notes but no preamble" "notes with no preamble exit 1" "$PROMOTE" 0.2.41 2026-08-26 "$f"
after=$(shasum "$f" | cut -d' ' -f1)
assert_true "a rejected changelog is left byte-identical" \
  "$([[ "$before" == "$after" ]] && echo yes || echo no)"

# A zone holding only an HTML comment is truthy to the TS extractPreamble, so the
# renderer picks it over the body and the modal shows a BLANK release note --
# worse than the wall of text the preamble exists to prevent. The promoter is
# deliberately stricter than the function it mirrors: the mirror decides what to
# display, this decides what may ship.
c=$(make_changelog '<!-- TODO: add release summary -->

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an HTML-comment-only preamble is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$c"

# A link-reference DEFINITION is non-blank and renders nothing -- ReactMarkdown
# emits output for it only where a matching [label] reference is used. Same
# blank-modal outcome as the HTML comment, one construct further out, which is
# why the predicate is now a positive "renders as prose" test rather than a
# denylist that would need a new entry per construct.
d=$(make_changelog '[summary]: https://example.invalid

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a link-reference-only preamble is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$d"

# Over-fire control: a preamble that USES a reference is real prose and must pass.
e=$(make_changelog 'See the [summary] for what changed in this release.

[summary]: https://example.invalid

### Fixed

- something
')
check 0 "prose that uses a link reference is still a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$e"

# Third construct in the same class: ChangelogMarkdown sanitises raw HTML away,
# so a `<br>`-only zone renders nothing at all.
g=$(make_changelog '<br>

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a raw-HTML-only preamble is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$g"

# Over-fire control: stripping a tag keeps its TEXT, so prose carrying inline
# markup is still prose.
h=$(make_changelog 'Fixed the <code>promoter</code> preamble check.

### Fixed

- something
')
check 0 "prose containing inline markup is still a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$h"

# Fourth construct in the same class: sanitizeSchema lists `img` under `strip`,
# which removes the CONTENT too, so an image-only zone renders nothing.
i=$(make_changelog '![Release summary](https://example.invalid/release.png)

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an image-only preamble is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$i"

# The rule is renderer-visible TEXT, not "something is left", so constructs
# nobody has reported yet also fail without needing their own rule.
j=$(make_changelog '---

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a thematic break alone is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$j"

# Over-fire control: an image ALONGSIDE prose is fine -- the prose still renders.
k=$(make_changelog 'This release fixes the preamble check.

![screenshot](https://example.invalid/x.png)

### Fixed

- something
')
check 0 "prose accompanied by an image is still a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$k"

# Decoding must not CREATE a line boundary. `&#10;` split one blockquote source
# line into two, the second not starting with `>`, so a blockquote passed the
# gate while the TS extractPreamble returned '' — the wall-of-text outcome this
# check exists to prevent. Structure is decided on the raw line.
q1=$(make_changelog '> an aside&#10;more words here

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an entity cannot manufacture a line out of a blockquote" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q1"

# ...and must not DESTROY one either: `&gt; x` is visible prose, not a
# blockquote, and the TS mirror accepts it.
q2=$(make_changelog '&gt; literal angle prose about this release.

### Fixed

- something
')
check 0 "an escaped angle bracket is prose, not a blockquote" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q2"

# A preamble in a non-Latin script is prose. The ASCII-only class rejected it and
# would have refused every such promotion, telling the author to write the
# paragraph they had already written.
q3=$(make_changelog 'この リリース で 修正 しました。

### Fixed

- something
')
check 0 "a non-Latin preamble is prose" "$PROMOTE" 0.2.41 2026-08-26 "$q3"

# The over-fire control for the DECODE itself. The previous one (`Caf&eacute;
# mode is fixed.`) could not fail for the reason it named — `Caf`, `mode`, `is`
# and `fixed` already supply the letters, so replacing unescape with
# delete-entities still passed. Here every word character comes from an entity.
q4=$(make_changelog '&#82;&#101;&#108;&#101;&#97;&#115;&#101;&#100;.

### Fixed

- something
')
check 0 "a preamble whose only word characters come from entities is prose" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q4"

# A link renders its LABEL, not its destination. `[](url)` is an anchor with no
# children: present in the DOM, invisible on screen, and the URL's characters
# made the raw source look like prose.
q9=$(make_changelog '[](https://example.invalid)

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an empty-label link renders nothing" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q9"

# Over-fire control: a link WITH a label is prose, and the label is what counts —
# not the URL, which must not be able to supply the word characters on its own.
q10=$(make_changelog '[Read the notes](https://example.invalid)

### Fixed

- something
')
check 0 "a labelled link is prose" "$PROMOTE" 0.2.41 2026-08-26 "$q10"

# An UNCLOSED comment renders nothing either, and requiring the `-->` terminator
# left the text intact so its own words satisfied the prose test. Fail-open in
# the dangerous direction: this side accepted, the modal paints blank.
q11=$(make_changelog '<!-- Release summary never closed

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an unterminated comment renders nothing" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q11"

# Over-fire control for that alternation: the terminator must still END the
# removal. Without this, `<!--.*?\Z` would eat the rest of the preamble and the
# fix above would pass by deleting the prose it is meant to find.
q12=$(make_changelog '<!-- a note --> Real prose after a closed comment.

### Fixed

- something
')
check 0 "a closed comment does not eat the prose after it" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q12"

# A CommonMark autolink is not a tag: it renders its own URL as visible text, so
# the generic tag strip was deleting a preamble the modal shows and refusing a
# valid release note.
q13=$(make_changelog '<https://concordvoice.com/releases/0.2.47>

### Fixed

- something
')
check 0 "a URL autolink is prose" "$PROMOTE" 0.2.41 2026-08-26 "$q13"

# GFM's email form of the same construct.
q14=$(make_changelog '<mark@example.invalid>

### Fixed

- something
')
check 0 "an email autolink is prose" "$PROMOTE" 0.2.41 2026-08-26 "$q14"

# Over-fire control for the autolink unwrap: a REAL tag must still strip. Without
# this, widening AUTOLINK_RE until it swallowed `<br>` would go unnoticed.
q15=$(make_changelog '<br>

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a raw br still renders nothing" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q15"

# Over-fire control for STRIP_CONTENT_RE's non-greedy body: greedy would span
# from the first stripped tag to the LAST, eating the prose between them.
q5=$(make_changelog '<details>a</details>

This release fixes the preamble check.

<details>b</details>

### Fixed

- something
')
check 0 "prose between two content-stripped tags survives" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q5"

# The reference-style image arm of MD_IMAGE_RE is written; nothing reached it.
q6=$(make_changelog '![Release summary][shot]

[shot]: https://example.invalid/x.png

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a reference-style image is not prose either" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q6"

# FENCE_RE encodes CommonMark's 0-3 space fence indent. At 4+ spaces the line is
# an indented code line, NOT a fence, so the `### ` below stays a real boundary.
q7=$(make_changelog 'A real summary of this release.

    ```

### Fixed

- something
')
check 0 "a 4-space-indented backtick run does not open a fence" \
  "$PROMOTE" 0.2.41 2026-08-26 "$q7"

# Fifth and sixth constructs in the same class. `button` is in the renderer's
# `strip:` list with its content; entities are SOURCE, not text.
l=$(make_changelog '<button>Release summary</button>

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "a stripped-tag-only preamble is not a preamble" \
  "$PROMOTE" 0.2.41 2026-08-26 "$l"

m=$(make_changelog '&nbsp;

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "an entity that decodes to whitespace is not prose" \
  "$PROMOTE" 0.2.41 2026-08-26 "$m"

n2=$(make_changelog '&#32;&#x20;

### Fixed

- something
')
check_msg 1 "has notes but no preamble" "numeric and hex space entities are not prose either" \
  "$PROMOTE" 0.2.41 2026-08-26 "$n2"

# Over-fire control: an entity that decodes to a LETTER is prose.
o=$(make_changelog 'Caf&eacute; mode is fixed.

### Fixed

- something
')
check 0 "an entity that decodes to a letter is still prose" \
  "$PROMOTE" 0.2.41 2026-08-26 "$o"

# The anti-drift pin against client/desktop's sanitizeSchema.ts runs in Desktop.
# Self-Hosted retains the promoter's behavioral cases below.

# The wrapper must CALL the .py rather than carry the Python itself. With a
# heredoc back in the wrapper and the old .py left on disk, the pin above would
# compare against dead code and every behavioural case would still pass.
assert_true "the wrapper runs promote_unreleased_changelog.py and has no heredoc" \
  "$(grep -Eq '^[^#]*promote_unreleased_changelog\.py' "$PROMOTE" \
     && ! awk '!/^[[:space:]]*#/ && /<</ {f=1} END {exit !f}' "$PROMOTE" && echo yes || echo no)"

# THE -1 BRANCH. A body with no category heading is ALL detail, not one large
# preamble. A port that treats "no boundary" as "the whole body is the zone"
# passes this and is LOOSER than the TS rule it claims to mirror.
f=$(make_changelog '
- A human wrote this, but never added a category heading.
')
check_msg 1 "has notes but no preamble" "a body with no ### heading has no preamble" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# A leading blockquote is an editorial aside about the release, not a summary of
# it; promoting one would show the aside and DROP every change bullet.
f=$(make_changelog '
> Versions 0.2.9-0.2.11 never reached you.

### Added

- A bullet.
')
check_msg 1 "has notes but no preamble" "a blockquote-only zone does not count as a preamble" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# FENCE AWARENESS, both directions. A fenced heading is not a boundary, so this
# body has none at all -> no preamble, even though prose sits above the fence.
f=$(make_changelog '
Prose that sits above a fenced block.

```
### not a real heading
```
')
check_msg 1 "has notes but no preamble" "a fenced ### is not a category boundary" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# ...and the same awareness must not over-fire: a real heading after a fence is
# still the boundary, so this one promotes.
f=$(make_changelog '
Prose that sits above a fenced block.

```
### not a real heading
```

### Added

- A bullet.
')
check 0 "a real ### after a fence still promotes" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# THE ACCEPT DIRECTION. Every case above pins a branch's REJECT side; a mutant
# that rejected any zone containing a blockquote survived the whole suite. That
# is not a hypothetical shape — CHANGELOG.md's 0.2.7, 0.2.1 and 0.2.0-Beta
# sections all open with an aside and then summarise, and the TS rule keeps such
# a zone whole (changelog.test.ts, "keeps an aside that sits alongside real
# prose"). A tightened port would reject a real release while telling its author
# to write a preamble they had already written.
f=$(make_changelog '
> Versions 0.2.9-0.2.11 never reached you.

A human also summarised what actually changed.

### Added

- A bullet.
')
check 0 "an aside ALONGSIDE real prose is still a preamble" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# Fence sub-branches. A fence closes only on its OWN marker, so a ``` inside a
# ~~~ block is content and leaves the fence open — which means the '###' below
# is swallowed and the body has no boundary at all.
f=$(make_changelog '
Prose above the block.

~~~
```
### swallowed by the still-open ~~~ fence
~~~
')
check_msg 1 "has notes but no preamble" "a fence closes only on its own marker" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# '### ' keeps its TRAILING SPACE, so '####' is not a category boundary — it is
# ordinary content in the preamble zone, exactly as the TS rule treats it.
# The expectation is 0, and that is the whole point: put the '####' line above
# the real heading as the only prose there, and dropping the trailing space from
# the match turns it INTO a boundary, leaving an empty zone above it and
# rejecting the section. An exit-1 expectation here would pass either way.
f=$(make_changelog '
#### A sub-heading is content, not a boundary

### Added

- A bullet.
')
check 0 "#### is content in the zone, not a category boundary" "$PROMOTE" 0.2.41 2026-08-26 "$f"

# The operator-facing text, not just the exit code.
f=$(make_changelog '
- A bare bullet with no category heading.
')
check_msg 1 "add one \\(e\\.g\\. .### Fixed" \
  "the rejection tells the author to ADD a category heading" \
  "$PROMOTE" 0.2.41 2026-08-26 "$f"

# Asserting the HEADING alone was vacuous: truncating the block right after it --
# removing all four descriptions -- still passed. Each documented code is now
# pinned, because the exit-code block is the operator-facing contract and a
# silent truncation of it is exactly the drift this suite exists to catch.
check_msg 0 "Exit codes:" "--help prints the exit-code block" "$PROMOTE" --help
check_msg 0 "0 — promoted" "--help documents exit 0" "$PROMOTE" --help
check_msg 0 "3 — .## \\[Unreleased\\]. is empty" "--help documents exit 3" "$PROMOTE" --help
check_msg 0 "1 — the file does not have the structure" "--help documents exit 1" "$PROMOTE" --help
check_msg 0 "2 — usage error" "--help documents exit 2 (the LAST entry)" "$PROMOTE" --help

# ...and it must STOP at the header. This PR changed `sed -n '2,30p'` to
# `sed -n '2,/^$/p'`; reverting that dumps ~80 lines of internal design
# commentary into operator-facing output, and every assertion above still
# passes because they only pin presence.
HELPOUT=$(bash "$PROMOTE" --help 2>&1)
if grep -qF -- "PORTED, NOT APPROXIMATED" < <(printf '%s\n' "$HELPOUT") || grep -qF -- "set -euo pipefail" < <(printf '%s\n' "$HELPOUT"); then
  echo "  FAIL: --help leaked internal design commentary past the header" >&2; fail=$((fail + 1))
else
  echo "  PASS: --help stops at the header block"; pass=$((pass + 1))
fi

# Every fixture must have gone to its OWN directory. With the counter bug each
# call returned the same path; compared against BUILDER CALLS rather than a
# hand-written total, which goes stale on the next commit.
calls=$(wc -l < "$TMP/builder-calls" | tr -d ' ')
built=$(find "$TMP" -maxdepth 1 -type d -name 'case-*' | wc -l | tr -d ' ')
if [[ "$built" == "$calls" ]]; then
  echo "  PASS: each fixture built its own tree ($built trees / $calls calls)"; pass=$((pass + 1))
else
  echo "  FAIL: $calls fixture builds produced only $built distinct trees" >&2; fail=$((fail + 1))
fi

echo ""
echo "Line-ending fidelity:"

# The verbatim contract is about BYTES, and text-mode I/O quietly broke it for the
# whole file rather than just the body: read_text() applies universal-newline
# translation, so a CRLF changelog was decoded to LF and written back as LF —
# rewriting every line ending, including the released sections this script must
# not touch. Measured before the fix: 17 CRLF in, 0 out.
CRLF_CASE="$TMP/crlf"; mkdir -p "$CRLF_CASE"
python3 -c '
import pathlib, sys
lines = ["# Changelog", "", "## [Unreleased]", "", "A summary sentence.", "",
         "### Added", "", "- A thing.", "",
         "## [0.2.40] - 2026-08-20", "", "Prior release prose.", "",
         "### Added", "", "- Old thing.", ""]
pathlib.Path(sys.argv[1]).write_bytes("\r\n".join(lines).encode())
' "$CRLF_CASE/CHANGELOG.md"
check 0 "a CRLF changelog promotes" "$PROMOTE" 0.2.41 2026-08-26 "$CRLF_CASE/CHANGELOG.md"

python3 -c '
import pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_bytes()
problems = []
# Every ending must still be CRLF: no lone LF survives once CRLF pairs are removed.
if raw.replace(b"\r\n", b"").count(b"\n"):
    problems.append("mixed endings: a lone LF was introduced")
if b"\r\n" not in raw:
    problems.append("all CRLF endings were rewritten to LF")
# The inserted headings must carry the ending the file uses, not a hardcoded LF.
for needle in (b"## [Unreleased]\r\n", b"## [0.2.41] \xe2\x80\x94 2026-08-26\r\n"):
    if needle not in raw:
        problems.append("inserted heading is not CRLF-terminated: %r" % needle)
# And the body itself must survive with its CRLF intact.
if b"A summary sentence.\r\n" not in raw:
    problems.append("body line endings were normalised")
if problems:
    print("      " + "; ".join(problems), file=sys.stderr)
    raise SystemExit(1)
' "$CRLF_CASE/CHANGELOG.md" && CRLF_OK=yes || CRLF_OK=no
assert_true "CRLF is preserved end-to-end, headings included" "$CRLF_OK"

# The LF direction must not regress into emitting CRLF.
LF_CASE="$TMP/lf"; mkdir -p "$LF_CASE"
printf '%s%s%s' "$FIXTURE_HEAD" '
A summary sentence.

### Added

- A thing.
' "$FIXTURE_TAIL" > "$LF_CASE/CHANGELOG.md"
check 0 "an LF changelog promotes" "$PROMOTE" 0.2.41 2026-08-26 "$LF_CASE/CHANGELOG.md"
assert_true "an LF changelog gains no CR bytes" \
  "$(grep -q $'\r' "$LF_CASE/CHANGELOG.md" && echo no || echo yes)"

echo ""
echo "Port vs. the TypeScript it mirrors (real CHANGELOG.md):"

# NOTHING otherwise pins the port to extractPreamble — a change to either side
# alone breaks no test. This runs the real gate over content that has actually
# shipped: every 0.2.x section of the committed CHANGELOG must be acceptable to
# the promoter, which is the direct counterpart of changelog.test.ts's
# "every 0.2.x entry in the REAL CHANGELOG.md has a non-blank preamble".
# It also closes a second hole: until now nothing asserted the promoter accepts
# real repo content at all, because the live [Unreleased] is empty.
REAL_CHANGELOG="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/CHANGELOG.md"
if [[ -f "$REAL_CHANGELOG" ]]; then
  REJECTED=$(python3 -c '
import pathlib, re, subprocess, sys, tempfile
src, promote, tmp = sys.argv[1], sys.argv[2], sys.argv[3]
text = pathlib.Path(src).read_text()
heads = list(re.finditer(r"(?m)^## \[(\d+\.\d+\.\d+)[^\]]*\][^\n]*\n", text))
bad = []
checked = 0
for i, h in enumerate(heads):
    version = h.group(1)
    if not version.startswith("0.2."):
        continue          # 0.1.x predates the convention; the TS rule exempts it too
    end = heads[i + 1].start() if i + 1 < len(heads) else len(text)
    body = text[h.end():end]
    d = tempfile.mkdtemp(dir=tmp)
    f = pathlib.Path(d) / "CHANGELOG.md"
    f.write_text("# Changelog\n\n## [Unreleased]\n" + body + "\n## [0.0.1] - 2020-01-01\n\n- seed.\n")
    rc = subprocess.run(["bash", promote, "9.9.9", "2026-01-01", str(f)],
                        capture_output=True).returncode
    if rc != 0:
        bad.append(f"{version}(exit {rc})")
    checked += 1
# The COUNT is printed with the failures. Reporting only failures let the
# selector match nothing and still pass: change "0.2." to "9.9." and `bad` stays
# empty, so the sole cross-language pin to extractPreamble evaporates silently —
# which is what happens on its own at GA when the file moves to 1.x.
print("%d %s" % (checked, " ".join(bad)))
' "$REAL_CHANGELOG" "$PROMOTE" "$TMP"
)
  CHECKED="${REJECTED%% *}"; REJECTED="${REJECTED#* }"
  assert_true "every shipped 0.2.x section is accepted by the port (checked $CHECKED)${REJECTED:+ — REJECTED: $REJECTED}" \
    "$([[ -z "$REJECTED" ]] && echo yes || echo no)"
  if [[ "${CHECKED:-0}" -ge 1 ]]; then
    echo "  PASS: the port pin ran over $CHECKED real section(s)"; pass=$((pass + 1))
  else
    echo "  FAIL: the port pin matched ZERO sections — it verified nothing" >&2; fail=$((fail + 1))
  fi
else
  echo "  FAIL: real CHANGELOG.md not found at $REAL_CHANGELOG — the port pin verified nothing" >&2
  fail=$((fail + 1))
fi

echo ""
echo "─────────────────────────────────────────"
echo "PASS: $pass  FAIL: $fail"
[[ "$fail" == "0" ]]
