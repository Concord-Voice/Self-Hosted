# promote_unreleased_changelog.py — the body of promote-unreleased-changelog.sh.
#
# Run it through that wrapper, not directly: the wrapper validates the three
# arguments (X.Y.Z version, YYYY-MM-DD date, an existing file) before this runs,
# and its header is the contract -- exit codes, the emptiness and preamble rules,
# and what --help prints. This file assumes all three arguments are valid.
#
#   python3 promote_unreleased_changelog.py <version> <iso-date> <changelog-path>

import pathlib
import html
import re
import sys

version, date, path_str = sys.argv[1:4]
path = pathlib.Path(path_str)
# read_bytes, NOT read_text: text mode applies universal-newline translation, so
# a CRLF changelog was decoded to LF and then WRITTEN BACK as LF -- rewriting
# every line ending in the file, including the released sections this script is
# not supposed to touch at all. Measured on a CRLF fixture: 17 CRLF in, 0 out.
# That is the verbatim contract failing on the whole file rather than the body.
text = path.read_bytes().decode("utf-8")
# The line ending the file already uses, for the two headings inserted below.
# Preserving the body byte-for-byte while emitting LF headings into a CRLF file
# would just move the inconsistency rather than fix it.
NL = "\r\n" if "\r\n" in text else "\n"

UNRELEASED = re.compile(r"(?m)^## \[Prerelease\][^\n]*\n")
head = UNRELEASED.search(text)
if head is None:
    print(f"ERROR: {path}: no '## [Prerelease]' heading", file=sys.stderr)
    raise SystemExit(1)

if re.search(rf"(?m)^## \[{re.escape(version)}\]", text):
    print(f"ERROR: {path}: a '## [{version}]' section already exists", file=sys.stderr)
    raise SystemExit(1)

# The Prerelease body runs to the next `## [` heading, or to EOF.
nxt = re.compile(r"(?m)^## \[").search(text, head.end())
body_end = nxt.start() if nxt else len(text)
body = text[head.end():body_end]

if not body.strip():
    print("Nothing to release: '## [Prerelease]' is empty.")
    raise SystemExit(3)

# Ordering is load-bearing: this runs AFTER the emptiness check, so an empty
# section still exits 3 and is never reclassified as a hard failure. The WHY of
# exit 1 vs exit 3 is in the header, which is also what --help prints — do not
# restate it here, or the two copies will drift.
#
# PORTED, NOT APPROXIMATED — this is the part a future editor needs at the code.
# The source is extractPreamble in
# client/desktop/src/renderer/services/system/changelog.ts; keep the two in step.
# A simpler sketch — "everything above the first '### ', or the whole body when
# there is none" — is LOOSER than the TS rule in two places, not stricter: it
# reads a heading-less body as one big preamble, and it accepts a fenced '### '
# as a real boundary. Both shapes occur in this repo's own fixtures, and the
# CHANGELOG's 0.2.7, 0.2.1 and 0.2.0-Beta sections all exercise the third branch
# (an aside ALONGSIDE prose, which must still count as a preamble).
FENCE_RE = re.compile(r"^\s{0,3}(```|~~~)")


def category_boundary(lines):
    """Index of the first '### ' outside a code fence, or -1. Mirrors findCategoryBoundary."""
    marker = None
    for i, line in enumerate(lines):
        m = FENCE_RE.match(line)
        if m:
            # Open on the first marker seen; close only on that SAME marker, so a
            # ``` line inside a ~~~ block is content and leaves the fence open.
            if marker is None:
                marker = m.group(1)
            elif marker == m.group(1):
                marker = None
            continue
        if marker is None and line.startswith("### "):
            return i
    return -1


LINK_REF_DEF_RE = re.compile(r"^\[[^\]]+\]:\s*\S")
# ACCEPTED, NOT FIXED — quadratic scan on an unclosed construct.
#
# The lazy-dot-all patterns below rescan the tail from every start position when
# a construct is never closed: 21.5 s measured on a 240 KB preamble of `<img >`
# with no closing tag, extrapolating to ~17 min at 1.7 MB. That is past the
# 15-minute timeout of pr-ci's `workflow-lint` job, whose self-test runs this
# over every shipped section. The outcome is a slow run or a red job on a
# changelog the author committed themselves — no execution, no disclosure, no
# other PR affected.
#
# Two cheap fixes were tried and BOTH introduced a correctness artifact worse
# than the slowness:
#   * truncating the zone (`zone_text[:65536]`) can cut inside a comment, so the
#     unclosed remainder survives the strip and its filler reads as prose;
#   * bounding the lazy bodies (`[\s\S]{0,8192}?`) leaves an over-long
#     `<details>` unstripped, so ITS content reads as prose.
# Both trade a fail-SLOW for a fail-OPEN in the gate. A correct fix is a linear
# paired-tag scanner, which is new code with its own tests and is not worth it
# for a self-inflicted red job; it belongs with the other deferred work.

# MIRRORS the `strip:` array in
# client/desktop/src/renderer/components/Markdown/sanitizeSchema.ts -- tags whose
# CONTENT the renderer removes, not just the tag. A partial copy is a silent
# hole (the first version carried ten of these and `button` was found in
# review), so the promoter's self-test PINS this list against that file: drift
# is a red test rather than a reviewer's lucky catch.
STRIP_TAGS = (
    "img iframe video audio embed object svg math script style link meta form "
    "input button textarea select canvas noscript base applet marquee details summary"
).split()
STRIP_CONTENT_RE = re.compile(
    r"<(%s)\b[^>]*>.*?</\1\s*>" % "|".join(STRIP_TAGS), re.S | re.I)
MD_IMAGE_RE = re.compile(r"!\[[^\]]*\](?:\([^)]*\)|\[[^\]]*\])")

# CommonMark autolink (a scheme-prefixed URI) and GFM's email form. Both render
# their inner text, so they must survive the tag strip rather than be eaten by it.
AUTOLINK_RE = re.compile(
    r"<([A-Za-z][A-Za-z0-9+.\-]{1,31}:[^<>\s]*|[^<>\s@]+@[^<>\s@]+)>")


def visible_text(zone_text):
    """The zone with everything the renderer discards removed.

    FIVE constructs reached review as "non-blank but renders nothing" -- an HTML
    comment, a link-reference definition, a raw `<br>`, an image, and an HTML
    entity that decodes to whitespace. Enumerating them was losing: each fix
    shipped and the next construct arrived.

    So the question is inverted. Remove what the renderer discards, decode
    entities, then require actual TEXT -- at least one alphanumeric character --
    rather than "something is left". A thematic break, a lone bullet marker and
    stray punctuation all fail that, and no new construct needs a new rule.

    The count above is the history, not the rule. Do not add a sixth bullet
    here when a sixth construct appears; if the rule is right it already
    fails, and if it is wrong the rule changes rather than the list.

    Two later findings were neither a sixth construct nor a wrong rule: they
    were defects in the REMOVAL step itself, one in each direction. An
    unterminated comment was under-removed (invisible text read as prose), and
    an autolink was over-removed (visible text deleted). Both are fixed where
    the removal happens, which is what "the rule changes" looks like in
    practice.
    """
    # The zone is NOT bounded before these lazy-dot-all regexes run, so they
    # are quadratic on UNCLOSED constructs -- measured 21.5 s on a 240 KB zone
    # of `<img >` with no closing tag. Truncating it was tried and rejected
    # because a cut can land inside a comment; the "ACCEPTED, NOT FIXED" note
    # above STRIP_TAGS has the reasoning.
    s = STRIP_CONTENT_RE.sub("", zone_text)
    # `\Z`, not just `-->`: an UNCLOSED comment renders nothing either, and
    # requiring the terminator left `<!-- summary never closed` intact, whose
    # own words then satisfied the prose test -- a fail-OPEN in the dangerous
    # direction, where this side accepts and the modal paints blank. It also
    # happens to be the cheaper branch: without the alternation an unclosed
    # comment is the exact input that forces the quadratic scan.
    s = re.sub(r"<!--.*?(?:-->|\Z)", "", s, flags=re.S)
    s = MD_IMAGE_RE.sub("", s)            # sanitizeSchema strips img entirely
    # A link renders its LABEL, never its destination: `[](url)` produces an
    # anchor with no children — visible in the DOM, invisible on screen — while
    # the URL's own characters made the raw source look like prose. Replace each
    # link with its label so the test measures rendered text, not source.
    s = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", s)
    # An autolink is not a tag. `<https://example.com/x>` is CommonMark, and it
    # renders its own URL as VISIBLE text -- for every scheme, not only the
    # three in ALLOWED_PROTOCOLS, because rehype-sanitize drops a rejected
    # `href` ATTRIBUTE and keeps the anchor's children. The generic tag strip
    # below deleted the whole thing and rejected a preamble the modal shows,
    # which refused a valid release note. Unwrap to the text first.
    s = AUTOLINK_RE.sub(r"\1", s)
    s = re.sub(r"<[^>]+>", "", s)         # remaining tags unwrap; their text stays
    # NOT unescaped here -- decoding happens per LINE in renders_as_prose(), and
    # the order is load-bearing in both directions:
    #
    #   `> an aside&#10;more words`  decoding first splits ONE blockquote line
    #                               into two, the second not starting with `>`,
    #                               so a blockquote passed the gate while the TS
    #                               extractPreamble returned '' -- the wall-of-
    #                               text outcome this check exists to prevent.
    #   `&gt; literal angle prose`  decoding first turns visible prose into an
    #                               apparent blockquote and REJECTED it, while
    #                               the TS mirror accepts it.
    #
    # An entity must never be able to create or destroy a line boundary, so the
    # boundaries are fixed before anything is decoded.
    return s


def renders_as_prose(line):
    """Does this line put readable text on the screen?

    Structure tests run on the RAW line and the text test on the DECODED one.
    `&gt; x` is visible prose, not a blockquote; `&nbsp;` is a blockquote-free
    line that is nonetheless blank.
    """
    s = line.strip()
    if not s:
        return False
    if s.startswith(">"):                 # an editorial aside, not a summary
        return False
    if LINK_REF_DEF_RE.match(s):          # a definition; renders only when used
        return False
    # `\w` with re.UNICODE, not `[A-Za-z0-9]`: a preamble written in a
    # non-Latin script is prose, and the ASCII class rejected it -- which would
    # have refused every such promotion with a diagnostic telling the author
    # to write the paragraph they had already written.
    return bool(re.search(r"\w", html.unescape(s), re.UNICODE))


def has_preamble(body_text):
    """Mirrors extractPreamble: a falsey result is the '' the TS function returns.

    Named body_text, not text: at module scope `text` is the ENTIRE changelog,
    and a later line in here that meant the file would silently get one section.
    """
    lines = body_text.split("\n")
    boundary = category_boundary(lines)
    # -1 is NOT "the whole body is the preamble". A body with no category heading
    # is ALL detail — the TS comment says so explicitly, and dropping this branch
    # is what makes a naive port looser than the rule it mirrors.
    if boundary == -1:
        return False
    zone = lines[:boundary]
    # The rule and its history live in visible_text()'s docstring; three stacked
    # revisions of it accumulated here and disagreed with each other on how many
    # constructs there were. One statement, one place.
    #
    # This is deliberately STRICTER than the TS extractPreamble it mirrors, and
    # that asymmetry is the right way round: the mirror decides what to DISPLAY,
    # this decides what may SHIP.
    visible = visible_text("\n".join(zone)).split("\n")
    # A zone holding only a blockquote is an editorial aside about the release
    # ("versions 0.2.9-0.2.11 never shipped"), not a summary of it.
    return any(renders_as_prose(l) for l in visible)


if not has_preamble(body):
    print(
        f"ERROR: {path}: '## [Prerelease]' has notes but no preamble.\n"
        "       Write 1-3 sentences directly under '## [Prerelease]', above the\n"
        "       first '###' category heading. If the section has no category heading\n"
        "       yet, add one (e.g. '### Fixed') below that paragraph — a body with no\n"
        "       category heading is read as all detail and has no preamble at all.\n"
        "       That paragraph is the only part the in-client post-update modal\n"
        "       renders; without it the modal shows the whole bullet list. A\n"
        "       blockquote-only aside does not count as a preamble, and neither\n"
        "       does a zone with no readable TEXT: an HTML comment, a raw tag\n"
        "       such as <br>, a link-reference definition ([label]: ...), or an\n"
        "       image -- the renderer discards all of them, so each ships a\n"
        "       blank release note.",
        file=sys.stderr,
    )
    raise SystemExit(1)

# The one sanctioned rewrite: the tally line. PRs add bullets without touching
# it (editing it in every PR would re-create the merge conflicts the one-line
# entry rule removes), so it drifts between releases and is recounted here.
# Only an existing tally line in the preamble zone is replaced; a body without
# one is emitted unchanged. Counts are top-level '- ' bullets per '### '
# category, outside code fences, in Keep a Changelog order. Other
# sections, such as known issues, are not counted.
# The lookahead admits a CR before the line end so CRLF files are recounted too.
TALLY_RE = re.compile(r"(?m)^\*\*\d+ Total Changes\*\*[^\r\n]*(?=\r?$)")
CATEGORY_ORDER = ["Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"]


def tally_line(body_text):
    counts, current, marker = {}, None, None
    for line in body_text.splitlines():
        m = FENCE_RE.match(line)
        if m:
            marker = m.group(1) if marker is None else (None if marker == m.group(1) else marker)
            continue
        if marker is not None:
            continue
        if line.startswith("### "):
            current = line[4:].strip()
        elif current in CATEGORY_ORDER and line.startswith("- "):
            counts[current] = counts.get(current, 0) + 1
    names = [c for c in CATEGORY_ORDER if c in counts]
    parts = [f"**{sum(counts.values())} Total Changes**"] + [f"{counts[c]} {c}" for c in names]
    return " | ".join(parts)


_boundary = category_boundary(body.split("\n"))
_zone_end = len("\n".join(body.split("\n")[:_boundary]))
_tally = TALLY_RE.search(body, 0, _zone_end)
if _tally:
    body = body[:_tally.start()] + tally_line(body) + body[_tally.end():]

# Apart from the tally line above, the body is emitted BYTE-FOR-BYTE. An earlier revision used body.rstrip(),
# which silently contradicted the verbatim contract this script exists to keep:
# in Markdown two trailing spaces are a hard line break, so stripping them
# changes how someone's release note RENDERS. `body` already carries its own
# leading and trailing whitespace (it is the slice between the Prerelease
# heading and the next `## [`), so re-emitting it unmodified reproduces the
# original spacing exactly. Do not normalise it.
#
# Keep a Changelog 1.1 heading, matching every existing one: `## [0.2.48] - 2026-10-04`.
promoted = (
    f"## [Prerelease]{NL}"
    f"{NL}"
    f"## [{version}] - {date}{NL}"
    f"{body}"
)
path.write_bytes((text[:head.start()] + promoted + text[body_end:]).encode("utf-8"))
print(f"Promoted {len(body.strip())} chars of release notes to [{version}] - {date}")
