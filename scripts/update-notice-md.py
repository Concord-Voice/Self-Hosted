#!/usr/bin/env python3
"""Concord Voice — NOTICE.md regenerator and drift guard.

NOTICE.md is a legal attribution artifact: it records every third-party
component Concord Voice distributes, so an omission understates the obligation
the file exists to discharge. It goes stale after every dependency bump, and
before this script that was only ever caught by hand.

Three modes, mirroring scripts/update-claude-md-counts.sh:

    update-notice-md.py                 dry run — print the diff, exit 0
    update-notice-md.py --check         gate    — see exit codes below
    update-notice-md.py --apply         write   — regenerate NOTICE.md in place

--check exit codes, and why they are not all failures:

    0   clean, OR version-only drift (emitted as ::warning::)
    1   an attribution defect — a shipped component with no row, a row for
        something no longer shipped, a licence that no longer matches, or a
        stated count that disagrees with its own table
    2   a change needing a human, OR any internal error

A component bumped from 4.4.3 to 4.5.4 is still attributed and its licence is
unchanged, so failing on it would turn every routine Dependabot bump red for no
compliance gain — while a component with NO row is precisely the exposure this
file exists to prevent (that was the jose@6.2.10 defect in #2886). Version drift
is therefore advisory and closed weekly by /weekly-deps Phase 6.3.

Exit 2 covers internal errors as well as escalations, deliberately. If an
unexpected exception escaped, Python would exit 1 — the code reserved for an
attribution defect — and CI would annotate a parser crash as a compliance
failure. That is the vacuity carrier `[internal]rules/tests.md` warns about: a
guard whose checks were deleted must not fail the same way as a guard that found
something. See main()'s catch-all.

`--scope` limits the work to one ecosystem so CI can check each half inside the
job that already paid for its toolchain (see .github/workflows/license-check.yml):

    --scope npm     the three npm bundles; needs workspace node_modules and the
                    locked license-checker tool installed
    --scope go      the Go control-plane table; needs a Go toolchain
    --scope all     both (the default, and the only scope --apply accepts)

WHY THIS SHELLS OUT TO license-checker RATHER THAN READING package-lock.json
---------------------------------------------------------------------------
The lockfile does carry `version` and `license` for ~99% of production entries,
which makes a lockfile-only guard tempting and fast. It is also wrong in three
ways that each produce a false mismatch:

  1. npm aliases — the lockfile holds `events-alias`, license-checker reports
     the real package name `events`.
  2. The `dev` flag is per tree path, so a package reachable from production at
     one path can be flagged dev at another (`@noble/hashes` is).
  3. ~1% carry no `license` field at all (`xmlhttprequest-ssl`).

Checking against the same generator that writes the file makes the guard exact
by construction and removes all three. The cost is installed application
dependencies plus the locked license-checker tool; the CI job that runs this
has already installed both.

WHAT ESCALATES TO A HUMAN (exit 2)
----------------------------------
Two cases, and they are symmetric — neither ecosystem may assert a licence the
tooling did not report:

  * A Go module linked into a shipped binary with no licence recorded in
    NOTICE.md. `go list` does not report a licence, so there is nothing to carry
    forward and inventing one would assert a licence determination — an audit
    action, not a cleanup (see #2886 and the precedent set by #1785).
  * An npm package whose licence license-checker could not identify (`UNKNOWN`)
    or which is a non-SPDX custom file (`Custom: <url>`). Writing either string
    verbatim into a legal artifact asserts exactly as much as inventing one.
  * A licence not in COMPATIBLE_LICENCES. The share table's third column asserts
    compatibility with CVSL 1.0 + AGPL-3.0, so writing a row for an unlisted
    licence would make a COMPATIBILITY determination — strictly larger than the
    licence name above. Certifying is not the script's call.
  * Any package name, licence, or repository URL containing `|` or a newline,
    which would forge a table column and silently defeat the licence comparison.

  Go licences are the case with no escalation available, because there is
  nothing to escalate ON: they are carried forward from the existing table by
  construction, so an upstream Go RELICENCE is structurally invisible here. npm
  relicensing IS caught (license-checker re-reads it every run). The manual
  compensating control is the `diff` recipe in /weekly-deps Phase 6.3.

KNOWN LIMITATION — npm's OWN VERSION CHANGES THE ANSWER
-------------------------------------------------------
`npm ci` is not reproducible across environments here. The three workspaces
carry 31 RANGE-valued `overrides` between them (16 desktop, 5 media-plane,
10 admin), and whether `npm ci` honours the lockfile pin or re-resolves the
range against the registry DEPENDS ON THE npm VERSION.

Measured 2026-09-08 on identical commits and lockfiles:

    npm 11.6.2  (Node 24, what CI runs) -> installs js-yaml 4.3.2
    npm 11.19.0 (Node 26, a dev machine) -> installs js-yaml 4.3.1

The lockfile pins 4.3.1; the override says `^4.3.1`; both versions exist. So the
same commit yields two different "correct" NOTICE.md files.

What this means in practice:

  * A version-only divergence surfaces as a ::warning:: a developer cannot clear
    locally — regenerating on a newer npm writes what CI will disagree with, and
    vice versa. Annoying, non-blocking, cleared whenever /weekly-deps Phase 6.3
    runs on a machine matching CI.
  * A divergence that ADDS OR DROPS a transitive package is blocking, and would
    turn a PR red in a way its author cannot reproduce. This has not been
    observed; it is the reason this limitation is written down rather than left
    to be rediscovered.

The root fix is not in this script: pinning those overrides to exact versions
removes the ambiguity entirely. That is a dependency-policy decision (see
scripts/security/override-range-lint.mjs, which polices a DIFFERENT override
hazard) and deliberately out of scope here.

Do not "fix" a divergence by suppressing it. Regenerate on a machine whose npm
matches the workflow's Node version, or leave the warning for Phase 6.3.
"""

from __future__ import annotations

import argparse
import csv
import datetime
import difflib
import io
import re
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
NOTICE = REPO_ROOT / "NOTICE.md"

# Heading -> workspace directory. Order matters only for reporting.
NPM_FAMILIES = [
    ("Desktop Client Production Dependencies", "client/desktop"),
    ("Media-Plane Production Dependencies", "services/media-plane"),
    ("Admin Portal Production Dependencies", "client/admin"),
]
GO_HEADING = "Go Control-Plane Runtime Dependencies"
GO_DIR = "services/control-plane"

# EVERY binary the control-plane image ships, not just the server. The Dockerfile
# builds three (services/control-plane/Dockerfile:39-41) and copies all three into
# the runtime image (:57-59), so a module reachable from any of them is
# distributed and owes an attribution row. Enumerating only ./cmd/server would
# both miss such a module and — worse — actively delete a hand-added row for one,
# because the guard would see it in the table and not in the dependency graph.
GO_PACKAGES = ["./cmd/server", "./cmd/migrate-media", "./cmd/mfa-rekey"]

ALL_HEADINGS = [GO_HEADING] + [h for h, _ in NPM_FAMILIES]

# Each summary bullet that states a per-family count, keyed by its heading.
SUMMARY_PATTERNS = {
    GO_HEADING: r"- \*\*(\d+)\*\* Go modules linked",
    "Desktop Client Production Dependencies": r"- \*\*(\d+)\*\* npm packages in the desktop client production bundle",
    "Media-Plane Production Dependencies": r"- \*\*(\d+)\*\* npm packages in the media-plane production bundle",
    "Admin Portal Production Dependencies": r"- \*\*(\d+)\*\* npm packages in the admin portal production bundle",
}

# EVERY place the file states the grand total. This list was incomplete once
# (it covered two of four), and the result was an --apply that wrote 401 in one
# sentence and left 398 in two others — authoring precisely the "stated count
# disagrees with its own table" defect this script claims to block. The
# no-stale-total assertion in regenerate() is the backstop that makes a future
# omission fail loudly instead of silently.
TOTAL_PATTERNS = [
    r"distributes (\d+) third-party",
    r"exact counts over the (\d+) components",
    r"part of that (\d+) and therefore carry no share",
    r"not part of the (\d+)-component count",
]

# The licences this project has determined are compatible with CVSL 1.0 +
# AGPL-3.0. MUST stay byte-identical to LICENSE_ALLOWLIST in
# .github/workflows/license-check.yml — scripts/test-update-notice-md.sh asserts
# the parity, because two drifting copies of a compliance allowlist is the
# failure mode, not a hypothetical.
#
# This exists because the share table's third column is headed "Compatible with
# CVSL 1.0 + AGPL-3.0". Emitting a bare "✓" for whatever licence happens to be
# present would have this script assert a COMPATIBILITY DETERMINATION — strictly
# more than the licence *name* it already refuses to invent. A licence outside
# this set escalates rather than being certified.
LICENCE_ALLOWLIST = (
    "MIT;ISC;Apache-2.0;BSD-2-Clause;BSD-3-Clause;0BSD;BlueOak-1.0.0;Python-2.0;"
    "Unlicense;CC0-1.0;CC-BY-3.0;CC-BY-4.0;MIT-0;MPL-2.0;(MIT OR CC0-1.0);"
    "(MIT OR Apache-2.0);(MIT OR GPL-2.0)"
)
COMPATIBLE_LICENCES = frozenset(LICENCE_ALLOWLIST.split(";"))

# A licence or package name carrying either of these would forge a column in the
# `|`-delimited tables, shifting every cell after it. parse_rows() reads the
# licence positionally as cells[2], so a forged column silently defeats the
# licence-change comparison — the guard's most important blocking condition.
ROW_FORBIDDEN = ("|", "\n", "\r")

# The licence-distribution table's header, used to locate it. It sits inside the
# Summary prose rather than under its own `##` heading.
SHARE_HEADER = "| License family | Share | Compatible with CVSL 1.0 + AGPL-3.0 |"
# Rows below the computed ones, describing dependencies that are NOT part of the
# component count (build-time / build-data). Preserved verbatim.
SHARE_TRAILING_PREFIXES = ("| MPL-2.0 (build-time only) |",
                           "| CC-BY-3.0 / CC-BY-4.0 (build-data only) |")


class Escalate(Exception):
    """A change a human must resolve; never written automatically."""


def reject_delimiters(where: str, **fields: str) -> None:
    """Refuse any value that would forge a column in a `|`-delimited table.

    Escaping would be the wrong call here: a licence identifier that genuinely
    contains a pipe (`MIT | GPL-2.0`, which some packages write instead of the
    SPDX `(MIT OR GPL-2.0)`) is a determination a human should make, not
    something this script should quietly normalise into a legal artifact.
    """
    bad = [f"{k}={v!r}" for k, v in fields.items()
           if any(ch in (v or "") for ch in ROW_FORBIDDEN)]
    if bad:
        raise Escalate(
            f"{where}: a field contains a table delimiter or newline, which "
            f"would forge a column and defeat the licence comparison:\n  "
            + "\n  ".join(bad) + "\n\n"
            "Resolve the real licence identifier by hand and record the decision "
            "on issue #2886."
        )


def run(cmd: list[str], cwd: Path) -> str:
    proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True,
                          encoding="utf-8")
    if proc.returncode != 0:
        raise RuntimeError(
            f"`{' '.join(cmd[:3])} ...` failed in {cwd} (exit {proc.returncode}):\n"
            f"{(proc.stderr or '').strip()[:800]}"
        )
    return proc.stdout


# ---------------------------------------------------------------- collectors


def npm_rows(workspace: str) -> list[str]:
    """Rows for one npm bundle, in license-checker's own CSV order.

    That order is by the full `name@version` key, which is why
    `@types/estree-jsx` precedes `@types/estree` (`-` sorts before `@`).
    Preserving it keeps regenerated output diffing cleanly.

    --excludePrivatePackages drops the first-party rows. Do NOT swap in
    --excludePackages with a name list: that flag matches the full
    `name@version` key, so bare names never match and the exclusion silently
    does nothing.
    """
    ws = REPO_ROOT / workspace
    if not (ws / "node_modules").exists():
        raise RuntimeError(
            f"{workspace}/node_modules is missing — run `npm ci` there first "
            f"(this script checks against license-checker, not the lockfile; "
            f"see the module docstring for why)."
        )
    checker = REPO_ROOT / ".github/tools/license-checker/node_modules/.bin/license-checker"
    if not checker.is_file():
        raise RuntimeError(
            "locked license-checker tool is missing — run "
            "`npm ci --ignore-scripts --no-audit --no-fund` in "
            "`.github/tools/license-checker` first."
        )
    out = run(
        [str(checker), "--production", "--csv", "--excludePrivatePackages"],
        cwd=ws,
    )
    rows, unidentified = [], []
    for rec in csv.DictReader(io.StringIO(out)):
        key = rec.get("module name")
        if not key or "@" not in key.lstrip("@"):
            raise RuntimeError(
                f"license-checker returned an unparseable module key {key!r} for "
                f"{workspace}; expected 'name@version'."
            )
        at = key.rfind("@")
        name, version = key[:at], key[at + 1:]
        licence = rec.get("license") or ""
        if licence == "UNKNOWN" or licence.startswith("Custom:"):
            unidentified.append(f"{name}@{version} -> {licence!r}")
            continue
        reject_delimiters(f"{workspace} package {name}@{version}",
                          licence=licence, name=name,
                          repository=rec.get("repository", ""))
        rows.append(f"| {name} | {version} | {licence} | {rec['repository']} |")
    if unidentified:
        raise Escalate(
            f"npm packages in {workspace} whose licence license-checker could not "
            "identify:\n  " + "\n  ".join(sorted(unidentified)) + "\n\n"
            "Writing 'UNKNOWN' or a 'Custom:' URL into a legal attribution file "
            "asserts as much as inventing one. Read each package's own licence "
            "file, add the row by hand, and record the decision on issue #2886."
        )
    if not rows:
        raise RuntimeError(f"license-checker returned no rows for {workspace}")
    return rows


def go_rows(recorded: dict[str, str]) -> list[str]:
    """Rows for the Go table, carrying each module's recorded licence forward.

    `go list` reports no licence, so a module already in NOTICE.md keeps its
    recorded one and a module that is not raises Escalate. The consequence,
    documented in the module docstring, is that an upstream Go relicence cannot
    be detected here.
    """
    out = run(
        ["go", "list", "-deps", "-e", "-f",
         "{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}", *GO_PACKAGES],
        cwd=REPO_ROOT / GO_DIR,
    )
    seen, rows, unknown = set(), [], []
    for line in out.splitlines():
        parts = line.split()
        # A single field is a first-party module (empty version): not a
        # third-party attribution. Several of the binaries share modules, so
        # dedupe across the union.
        if len(parts) != 2 or line in seen:
            continue
        seen.add(line)
        module, version = parts
        if module not in recorded:
            unknown.append(f"{module} {version}")
            continue
        reject_delimiters(f"Go module {module}", licence=recorded[module])
        rows.append(f"| {module} | {version} | {recorded[module]} |")
    if unknown:
        raise Escalate(
            "Go modules linked into a shipped binary with no licence recorded in "
            "NOTICE.md:\n  " + "\n  ".join(sorted(unknown)) + "\n\n"
            "`go list` does not report a licence, and inventing one would assert "
            "a licence determination — an audit action, not a cleanup. Determine "
            "each licence from the module's own LICENSE file, add the row by "
            "hand, and record the decision on issue #2886."
        )
    if not rows:
        raise RuntimeError(f"`go list` returned no third-party modules for {GO_PACKAGES}")
    return sorted(rows)


# ------------------------------------------------------------------ parsing


def section_bounds(text: str, heading: str) -> tuple[int, int]:
    """Byte range of one `## <heading>` section, up to its closing `---` rule.

    The search is anchored on a whole line: an unanchored `"## " + heading`
    also matches inside `### <heading>`, which silently retargets the rewrite
    onto the wrong region and leaves an orphaned `#` behind.
    """
    needle = "\n## " + heading + "\n"
    # Search a leading-newline-padded copy so a heading at offset 0 matches too;
    # otherwise it raises and blames a rename that never happened.
    try:
        start = ("\n" + text).index(needle)
    except ValueError:
        raise RuntimeError(
            f"NOTICE.md has no '## {heading}' section — a heading was renamed; "
            f"update NPM_FAMILIES/GO_HEADING to match."
        ) from None
    try:
        return start, text.index("\n---\n", start)
    except ValueError:
        raise RuntimeError(
            f"section '{heading}' is not terminated by a '---' rule."
        ) from None


def parse_rows(text: str, heading: str) -> dict[tuple[str, str], str]:
    """{(component, version): licence} for one section's table.

    Keyed on the PAIR, never the name alone. A bundle legitimately ships two
    versions of one package — `debug`, `ms`, `accepts`, `mime-types` and four
    others are duplicated in this file today — and a name-keyed map silently
    drops all but the last.

    Be precise about what the pair key buys, because an earlier draft of this
    docstring overstated it. It buys LICENCE detection on a duplicated name: a
    new version arriving under a different licence, or two versions whose
    licences trade places, both block. It does NOT make a per-version row gap
    blocking — a second version of an already-attributed name under the SAME
    licence stays advisory, on purpose. That component is still attributed under
    that licence, so no attribution obligation goes unrecorded; only the version
    list is stale, which is bookkeeping and clears at the next regeneration.
    """
    start, end = section_bounds(text, heading)
    out = {}
    for line in text[start:end].splitlines():
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 3 or cells[0] in ("Package", "Module") or set(cells[0]) <= set("- "):
            continue
        out[(cells[0], cells[1])] = cells[2]
    return out


def count_rows(text: str, heading: str) -> int:
    start, end = section_bounds(text, heading)
    rows = [l for l in text[start:end].splitlines() if l.startswith("|")]
    return len(rows) - 2  # drop the header row and its separator


def recorded_go_licences(text: str) -> dict[str, str]:
    return {name: lic for (name, _), lic in parse_rows(text, GO_HEADING).items()}


def classify(current: str, updated: str) -> tuple[list[str], list[str]]:
    """Split the difference into blocking findings and advisory ones.

    A component that ships with no attribution row is a compliance defect: the
    obligation NOTICE.md exists to discharge is simply unrecorded. That blocks.
    A component whose recorded version trails the installed one is still
    attributed, and its licence is unchanged — that is bookkeeping, and blocking
    it would turn every routine Dependabot bump red for no compliance gain. The
    weekly regeneration (/weekly-deps Phase 6.3) closes the bookkeeping loop.

    Comparison is name-level over a (name, version)-keyed map, so a package
    shipping at two versions is handled correctly in both directions.

    Returns (blocking, advisory) as human-readable lines.
    """
    blocking, advisory = [], []
    for heading in ALL_HEADINGS:
        was, now = parse_rows(current, heading), parse_rows(updated, heading)
        family = heading.split(" ")[0]
        was_names = {n for n, _ in was}
        now_names = {n for n, _ in now}
        for name in sorted(now_names - was_names):
            vs = ", ".join(sorted(v for n, v in now if n == name))
            blocking.append(f"{family}: {name} {vs} ships but has NO attribution row")
        for name in sorted(was_names - now_names):
            vs = ", ".join(sorted(v for n, v in was if n == name))
            blocking.append(f"{family}: {name} {vs} has a row but no longer ships")
        for name in sorted(was_names & now_names):
            was_pairs = {v: lic for (n, v), lic in was.items() if n == name}
            now_pairs = {v: lic for (n, v), lic in now.items() if n == name}
            was_ver, now_ver = set(was_pairs), set(now_pairs)
            # Compare the PAIRS, not a set of licences against a set of
            # versions. Two versions of one name whose licences trade places
            # have equal licence sets AND equal version sets, so a set-vs-set
            # comparison reports no change while NOTICE.md now records the
            # wrong licence against a specific shipped version.
            shared = was_ver & now_ver
            mismatched = sorted(v for v in shared if was_pairs[v] != now_pairs[v])
            if mismatched:
                blocking.append(
                    f"{family}: {name} licence recorded as "
                    + "; ".join(f"{v}={was_pairs[v]!r}" for v in mismatched)
                    + ", installed copy reports "
                    + "; ".join(f"{v}={now_pairs[v]!r}" for v in mismatched))
            elif set(was_pairs.values()) != set(now_pairs.values()):
                blocking.append(
                    f"{family}: {name} licence set recorded as "
                    f"{sorted(set(was_pairs.values()))}, installed copy reports "
                    f"{sorted(set(now_pairs.values()))}")
            elif was_ver != now_ver:
                advisory.append(
                    f"{family}: {name} {', '.join(sorted(was_ver))} -> "
                    f"{', '.join(sorted(now_ver))}")

    # A stated count that disagrees with its own table makes the file
    # self-inconsistent, which is one of #2886's acceptance criteria. Blocking.
    # Both checks read `current` alone, so they hold under any --scope.
    stated_total = sum(count_rows(current, h) for h in ALL_HEADINGS)
    for pattern in TOTAL_PATTERNS:
        m = re.search(pattern, current)
        if m and int(m.group(1)) != stated_total:
            blocking.append(
                f"summary states {m.group(1)} components in total, its four "
                f"tables hold {stated_total}")
            break
    for heading, pattern in SUMMARY_PATTERNS.items():
        m = re.search(pattern, current)
        actual = count_rows(current, heading)
        if m and int(m.group(1)) != actual:
            blocking.append(
                f"summary says {m.group(1)} for {heading!r}, its table holds {actual}")
    return blocking, advisory


# ---------------------------------------------------------------- rewriting


def replace_table(text: str, heading: str, rows: list[str]) -> str:
    start, end = section_bounds(text, heading)
    block = text[start:end]
    # splitlines() discards a trailing newline, and "\n".join() will not put it
    # back. Losing it silently deletes the blank line between the last table row
    # and the section's `---` rule — a whitespace-only rewrite of a legal
    # artifact that no reader asked for. Carry the terminator through verbatim.
    terminator = "\n" if block.endswith("\n") else ""
    lines = block.splitlines()
    table = [i for i, l in enumerate(lines) if l.startswith("|")]
    if len(table) < 2:
        raise RuntimeError(
            f"section '{heading}' has no table — check for a stray '---' rule "
            f"inside it, which would truncate the section early."
        )
    header, last = table[0], table[-1]
    if table != list(range(header, last + 1)):
        raise RuntimeError(
            f"section '{heading}' contains more than one table; this rewriter "
            f"would merge them and destroy everything between."
        )
    # Keep the header row and its separator; swap everything after.
    rebuilt = lines[: header + 2] + rows + lines[last + 1:]
    return text[:start] + "\n".join(rebuilt) + terminator + text[end:]


def replace_share_table(text: str, counts: dict[str, int], total: int) -> str:
    """Rewrite the licence-distribution table from the four regenerated tables.

    This table is not decoration: the prose under it asserts the shares are
    "exact counts over the N components tabulated below". If --apply moves a
    count without moving this table, the file asserts something false about
    itself. One row per distinct licence string, most common first — the former
    hand-made groupings (`BSD-2-Clause / BSD-3-Clause`) were an editorial choice
    that cannot survive mechanical regeneration.

    The trailing build-time / build-data rows describe dependencies OUTSIDE the
    count and are preserved verbatim.
    """
    try:
        start = text.index(SHARE_HEADER)
    except ValueError:
        raise RuntimeError(
            "NOTICE.md has no licence-distribution table header; update "
            "SHARE_HEADER to match."
        ) from None
    lines = text[start:].splitlines(keepends=True)
    body_len = 0
    trailing = []
    unrecognised = []
    for i, line in enumerate(lines):
        if not line.startswith("|"):
            break
        body_len += len(line)
        bare = line.rstrip("\n")
        if line.startswith(SHARE_TRAILING_PREFIXES):
            trailing.append(bare)
        elif i > 1 and not re.match(r"\| .+ \| \d+\.\d% \(\d+\) \| ✓ \|$", bare):
            # Anything that is neither the header, its separator, a computed
            # row, nor a known trailing row would be dropped on rebuild. Refuse
            # rather than delete: silently destroying a hand-added row in a
            # legal artifact is worse than failing loudly, and the deletion
            # would arrive inside a routine-looking regeneration diff.
            unrecognised.append(bare)
    if unrecognised:
        raise RuntimeError(
            "the licence-share table holds rows this rewriter does not "
            "recognise and would delete:\n  " + "\n  ".join(unrecognised) + "\n\n"
            "Add its prefix to SHARE_TRAILING_PREFIXES if it is a deliberate "
            "out-of-count row, or remove it."
        )
    uncertified = sorted(set(counts) - COMPATIBLE_LICENCES)
    if uncertified:
        raise Escalate(
            "licences present in the component tables that this project has NOT "
            "determined to be compatible with CVSL 1.0 + AGPL-3.0:\n  "
            + "\n  ".join(uncertified) + "\n\n"
            "The share table's third column asserts compatibility. Writing a '✓' "
            "for these would have this script make a compatibility determination "
            "it has no basis for — a larger claim than the licence name it "
            "already refuses to invent. Either the component does not belong in "
            "a CVSL artifact, or the licence belongs in LICENCE_ALLOWLIST here "
            "AND in LICENSE_ALLOWLIST in .github/workflows/license-check.yml. "
            "Both are human decisions; record them on issue #2886."
        )
    rebuilt = [SHARE_HEADER, "|---|---|---|"]
    for lic, n in sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])):
        rebuilt.append(f"| {lic} | {100 * n / total:.1f}% ({n}) | ✓ |")
    rebuilt.extend(trailing)
    return text[:start] + "\n".join(rebuilt) + "\n" + text[start + body_len:]


def sub_once(text: str, pattern: str, value: int, label: str) -> str:
    """Substitute a figure that must appear exactly once.

    re.subn(count=1) reports n == 1 even when the pattern matched five times, so
    the match count is asserted separately — otherwise a second occurrence is
    silently left stale, which is how the total drifted in the first place.
    """
    hits = len(re.findall(pattern, text))
    if hits != 1:
        raise RuntimeError(
            f"expected exactly one {label} figure in NOTICE.md, found {hits} "
            f"(pattern: {pattern}). The summary wording changed; update "
            f"SUMMARY_PATTERNS/TOTAL_PATTERNS."
        )
    m = re.search(pattern, text)
    # Replace the captured SPAN, not the first occurrence of the captured
    # string inside the match — the latter rewrites the wrong digits the moment
    # a pattern gains a digit before its capture group.
    return text[: m.start(1)] + str(value) + text[m.end(1):]


def regenerate(text: str, scope: str, today: str | None,
               workspace: str | None = None) -> str:
    known_workspaces = {target for _, target in NPM_FAMILIES}
    if workspace is not None and (scope != "npm" or workspace not in known_workspaces):
        raise RuntimeError("--workspace requires --check --scope npm and a known npm workspace")
    original_total = sum(count_rows(text, h) for h in ALL_HEADINGS)

    if scope in ("go", "all"):
        text = replace_table(text, GO_HEADING, go_rows(recorded_go_licences(text)))
    if scope in ("npm", "all"):
        for heading, target in NPM_FAMILIES:
            if workspace is None or workspace == target:
                text = replace_table(text, heading, npm_rows(target))

    # Every per-family bullet is rewritten regardless of scope. count_rows reads
    # the file and needs no toolchain, so a scoped run can still keep the other
    # families' bullets honest — without this, a desktop bullet that disagrees
    # with the desktop table would be invisible to `--scope go`.
    for heading, pattern in SUMMARY_PATTERNS.items():
        text = sub_once(text, pattern, count_rows(text, heading), heading)

    total = sum(count_rows(text, h) for h in ALL_HEADINGS)
    for pattern in TOTAL_PATTERNS:
        text = sub_once(text, pattern, total, "total component count")

    counts: dict[str, int] = {}
    for heading in ALL_HEADINGS:
        for lic in parse_rows(text, heading).values():
            counts[lic] = counts.get(lic, 0) + 1
    text = replace_share_table(text, counts, total)

    # Backstop for an incomplete TOTAL_PATTERNS: if any mention of the OLD total
    # survives in the summary prose, a pattern is missing and the file would ship
    # self-contradictory. Scoped to the summary so a coincidental version string
    # further down cannot trip it.
    if original_total != total:
        # Bound the search to the prose preamble — everything before the first
        # component table. Every total mention lives there, and the tables
        # themselves are full of version numbers that would false-positive.
        # Anchoring on a named section (`## License Texts`) instead was brittle:
        # it raised ValueError on any NOTICE.md lacking that exact heading.
        # The WHOLE file minus table rows, not just the preamble. Bounding the
        # scan at the first component heading made a total mention added below
        # it — in "Build-Time and Development Dependencies", say — invisible to
        # both TOTAL_PATTERNS and this backstop, which is precisely the class
        # the backstop exists to catch.
        #
        # Table rows are dropped because the licence-share table carries
        # percentages, and `\b8\b` happily matches the 8 in "77.8%" — which
        # made the backstop fire on its own correct output. Note the preamble
        # also cites Dockerfile line numbers (8, 39, 41, 57, 59, 66), so a
        # total that happens to equal one of those would false-positive; that
        # is unreachable at this project's scale but is why the message says to
        # extend TOTAL_PATTERNS rather than to edit the file by hand.
        prose = "\n".join(l for l in text.splitlines() if not l.startswith("|"))
        if re.search(rf"\b{original_total}\b", prose):
            raise RuntimeError(
                f"the previous total {original_total} still appears in NOTICE.md's "
                f"summary after regenerating to {total}. TOTAL_PATTERNS is missing "
                f"a case — add it rather than editing the file by hand."
            )

    if today:
        text = sub_once(text, r"\*\*Generated:\*\* (\d{4}-\d{2}-\d{2})",
                        today, "Generated date")
    return text


def _main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--apply", action="store_true",
                      help="rewrite NOTICE.md in place")
    mode.add_argument("--check", action="store_true",
                      help="exit 1 on an attribution defect (for CI)")
    ap.add_argument("--scope", choices=("npm", "go", "all"), default="all",
                    help="which ecosystems to regenerate (default: all)")
    ap.add_argument("--workspace", choices=tuple(target for _, target in NPM_FAMILIES),
                    help="limit --check --scope npm to one workspace")
    args = ap.parse_args()

    if args.apply and args.scope != "all":
        ap.error("--apply regenerates the whole file; it does not take --scope")
    if args.workspace is not None and (not args.check or args.apply or args.scope != "npm"):
        ap.error("--workspace requires --check --scope npm")

    current = NOTICE.read_text(encoding="utf-8")
    # The Generated: date is only stamped on a real write. A --check run must not
    # report "drift" merely because today is not the day the file was generated.
    today = datetime.date.today().isoformat() if args.apply else None

    try:
        updated = regenerate(current, args.scope, today, args.workspace)
    except Escalate as exc:
        print(f"NOTICE.md needs a human decision:\n\n{exc}\n", file=sys.stderr)
        return 2
    except RuntimeError as exc:
        print(f"NOTICE.md regeneration failed: {exc}", file=sys.stderr)
        return 2
    except Exception as exc:  # noqa: BLE001 — see the docstring's exit-code note
        # Without this, an unexpected exception exits 1: the code reserved for an
        # attribution defect. A parser crash would be annotated as a compliance
        # failure, and a guard whose checks were deleted would fail exactly like
        # a guard that found something.
        print(f"NOTICE.md regeneration hit an internal error "
              f"({type(exc).__name__}): {exc}", file=sys.stderr)
        return 2

    sub = "" if args.scope == "all" else f" (scope: {args.scope})"
    if args.workspace is not None:
        sub = f" (scope: npm, workspace: {args.workspace})"
    if updated == current:
        print(f"NOTICE.md is up to date{sub}. No changes needed.")
        return 0

    diff = "".join(difflib.unified_diff(
        current.splitlines(keepends=True), updated.splitlines(keepends=True),
        fromfile="NOTICE.md (committed)", tofile="NOTICE.md (regenerated)"))

    if args.apply:
        NOTICE.write_text(updated, encoding="utf-8")
        print(f"NOTICE.md regenerated.\n\n{diff}")
        return 0

    blocking, advisory = classify(current, updated)
    fix = ("Regenerate with:  scripts/update-notice-md.py --apply\n"
           "(needs `npm ci` in client/desktop, services/media-plane and "
           "client/admin first.)")

    if not args.check:
        print(f"NOTICE.md has drifted from the installed dependency set{sub}.\n\n"
              f"{diff}\n{fix}")
        return 0

    if blocking:
        for line in blocking:
            print(f"::error::NOTICE.md attribution defect — {line}")
        print(f"\nNOTICE.md does not attribute the set of components actually "
              f"shipped{sub}.\n\n{diff}\n{fix}", file=sys.stderr)
        return 1

    # Version-only drift, or a difference this classifier does not model
    # (ordering, whitespace, the licence-share table). Neither leaves a component
    # unattributed, so both are reported without failing the build.
    for line in advisory or ["tables differ from the regenerated output"]:
        print(f"::warning::NOTICE.md is stale — {line}")
    print(f"\nNOTICE.md is behind the installed dependency set, but every "
          f"shipped component is still attributed and no licence changed, so "
          f"this does not block.\n\n{diff}\n{fix}")
    return 0


def main() -> int:
    """Exit-code firewall around _main().

    Every path must land on 0/1/2, with 1 reserved for an attribution defect.
    An exception escaping to the interpreter exits 1, so a missing or unreadable
    NOTICE.md would be annotated by CI as a compliance failure — and a guard
    whose checks were deleted would fail indistinguishably from a guard that
    found something. _main()'s inner try covers regenerate(); this covers
    reading, diffing, classifying and writing.
    """
    try:
        return _main()
    except Escalate as exc:
        print(f"NOTICE.md needs a human decision:\n\n{exc}\n", file=sys.stderr)
        return 2
    except Exception as exc:  # noqa: BLE001 — see the docstring's exit-code note
        print(f"NOTICE.md check hit an internal error "
              f"({type(exc).__name__}): {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
