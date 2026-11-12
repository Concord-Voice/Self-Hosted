#!/usr/bin/env python3
"""Check that every image a tracked Dockerfile builds from is the one declared.

    check-base-images.py [--root DIR] [--manifest PATH] [FILE...]

Every FROM, COPY --from=, ADD --from= and RUN --mount=...,from= is compared with
the identity declared in .github/base-images.json. With no FILE the checked set
is every tracked path under --root whose text contains "dockerfile" in any
letter case. A FILE list replaces that set, so manifest entries that are not in
the list are reported as stale. Standard library only; no Docker daemon, no
network.

Exit 0: no violations. Exit 1: violations, one "<file>:<line>: <rule>: <message>"
line each on stdout. Exit 2: the check could not complete, message on stderr.

The parser reads lines, continuations and flag words the way BuildKit does, and
refuses what it cannot read that way: a heredoc, "# escape=", a line that starts
with "//", a control character, a non-ASCII character outside a comment line,
a quoted or escaped word in FROM or in the flags of COPY, ADD and RUN, and any
image source in an ONBUILD trigger.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import traceback
import unicodedata

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.realpath(__file__))))
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
DIRECTIVE = re.compile(r"#\s*(syntax|escape)\s*=\s*(.*)", re.IGNORECASE)
# BuildKit's own directive pattern. It reads directives from the top of the file
# and stops at the first line that does not match.
DIRECTIVE_LINE = re.compile(r"#\s*[a-zA-Z][a-zA-Z0-9]*\s*=\s*.+")
# BuildKit: a backslash ends the line as a continuation unless another backslash
# precedes it.
CONTINUATION = re.compile(r"(?<!\\)\\[ \t]*$")
QUOTING = re.compile(r"[\"'\\]")
BLANK = " \t"
ENTRY_KEYS = {"stages", "build_contexts", "syntax"}
IMAGE_KEYS = {"registry", "repository", "tag"}

violations = []  # (path, line, rule, message); main() empties it


class CheckError(Exception):
    """The check could not complete (exit 2)."""


def violation(path, line, rule, message):
    violations.append((path, line, rule, message))


def has_control(line):
    """True for a C0 control except tab (a lone CR included), DEL, a C1 control,
    a Unicode line separator or a byte order mark. Python and BuildKit disagree
    on which of these are blank or end a word.
    """
    return any(
        char != "\t" and (ord(char) < 0x20 or 0x7F <= ord(char) <= 0x9F or char in "\u2028\u2029\ufeff")
        for char in line
    )


# ── manifest ────────────────────────────────────────────────────────────────


def bad(path, where, why):
    raise CheckError(f"{path}: {where}: {why}")


def check_stage(path, where, stage):
    if not isinstance(stage, dict):
        bad(path, where, "must be an object")
    if "scratch" in stage:
        if stage["scratch"] is not True or not set(stage) <= {"scratch", "alias"}:
            bad(path, where, '"scratch" must be true and may only be combined with "alias"')
    elif not IMAGE_KEYS <= set(stage) or not set(stage) <= IMAGE_KEYS | {"alias"}:
        bad(path, where, 'must be {"scratch": true}, or have "registry", "repository" and "tag" (and optionally "alias")')
    for key, value in stage.items():
        if key != "scratch" and not (isinstance(value, str) and value):
            bad(path, where, f'"{key}" must be a non-empty string')
    tag = stage.get("tag")
    if tag is not None:
        if not (tag.startswith("^") and tag.endswith("$")):
            bad(path, where, f"tag pattern {tag} must start with ^ and end with $")
        try:
            re.compile(tag)
        except re.error as err:
            bad(path, where, f"tag pattern {tag} does not compile: {err}")


def load_manifest(path):
    """Return the manifest's "dockerfiles" object, or raise CheckError on a bad manifest."""
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except OSError as err:
        raise CheckError(f"cannot read manifest {path}: {err}") from err
    except ValueError as err:  # JSONDecodeError and UnicodeDecodeError
        raise CheckError(f"manifest {path} is not valid JSON: {err}") from err
    if not isinstance(data, dict) or set(data) != {"schema", "dockerfiles"}:
        bad(path, "top level", 'must be an object with "schema" and "dockerfiles"')
    if isinstance(data["schema"], bool) or data["schema"] != 1:
        bad(path, '"schema"', "must be 1")
    entries = data["dockerfiles"]
    if not isinstance(entries, dict):
        bad(path, '"dockerfiles"', "must be an object")
    for name, entry in entries.items():
        where = f'dockerfiles["{name}"]'
        if not isinstance(entry, dict) or "stages" not in entry or not set(entry) <= ENTRY_KEYS:
            bad(path, where, 'must be an object with "stages" and optionally "build_contexts" and "syntax"')
        if not isinstance(entry["stages"], list) or not entry["stages"]:
            bad(path, where + ".stages", "must be a non-empty list")
        for index, stage in enumerate(entry["stages"]):
            check_stage(path, f"{where}.stages[{index}]", stage)
        contexts = entry.get("build_contexts", [])
        if not isinstance(contexts, list) or not all(isinstance(c, str) and c for c in contexts):
            bad(path, where + ".build_contexts", "must be a list of non-empty strings")
        if "syntax" in entry and not (isinstance(entry["syntax"], str) and entry["syntax"]):
            bad(path, where + ".syntax", "must be a non-empty string")
    return entries


# ── Dockerfile parsing ──────────────────────────────────────────────────────


def parse(text):
    """Split a Dockerfile into parser directives and logical instructions.

    Returns (directives, instructions): (line, name, value, at_top) and
    (line, text) tuples. Continuation, comment and blank-line handling follow
    Docker. A directive is collected anywhere in the leading comment block,
    before the first instruction. at_top is True only where BuildKit reads a
    "# syntax=" line: unindented, with nothing above it but other directive
    lines and an optional shebang on line 1.
    """
    directives, instructions = [], []
    pending = None  # [first line, text so far] while a continuation is open
    top = True  # every line so far is a directive line (or the shebang)
    for number, line in enumerate(text.split("\n"), 1):
        stripped = line.strip(BLANK)
        directive_line = top and DIRECTIVE_LINE.fullmatch(line.rstrip(BLANK)) is not None
        top = directive_line or (number == 1 and line.startswith("#!"))
        if pending is None:
            if stripped.startswith("#"):
                match = DIRECTIVE.fullmatch(stripped)
                if match and not instructions:
                    directives.append((number, match.group(1).lower(), match.group(2).strip(), directive_line))
                continue
            if not stripped:
                continue
            pending = [number, ""]
        elif not stripped or stripped.startswith("#"):
            continue
        pending[1] += CONTINUATION.sub("", line)
        if not CONTINUATION.search(line):
            instructions.append((pending[0], pending[1]))
            pending = None
    if pending is not None:
        instructions.append((pending[0], pending[1]))
    return directives, instructions


def parse_from(args):
    """Return (reference, lower-cased alias or None) from FROM's arguments, or None.

    None also for a quote or a backslash anywhere: BuildKit removes them from a
    flag word, so the words that follow cannot be told apart reliably.
    """
    if QUOTING.search(args):
        return None
    tokens = args.split()
    while tokens and tokens[0].startswith("--"):  # --platform=... and other flags
        tokens.pop(0)
    if len(tokens) == 1:
        return tokens[0], None
    if len(tokens) == 3 and tokens[1].lower() == "as":
        return tokens[0], tokens[2].lower()
    return None


def split_reference(ref):
    """Return (registry, repository, tag, digest) of an image reference; tag and digest may be None."""
    name, at, digest = ref.partition("@")
    tag = None
    if name.rfind(":") > name.rfind("/"):
        name, _, tag = name.rpartition(":")
    first = name.split("/")[0]
    if "/" in name and ("." in first or ":" in first or first == "localhost"):
        registry, _, repository = name.partition("/")
    else:
        registry, repository = "docker.io", name
    if registry == "docker.io" and "/" not in repository:
        repository = "library/" + repository
    return registry, repository, tag, (digest if at else None)


def sources(keyword, args):
    """Yield (name, quoted) for each --from= (COPY, ADD) and --mount=...,from= (RUN) in the leading flags.

    BuildKit removes quotes and backslashes from a flag word before it reads it.
    A leading word that starts with "--" once they are removed, and that held
    any, is yielded with quoted True; the caller refuses it instead of guessing
    its value. The caller passes COPY, ADD and RUN only.
    """
    for token in args.split():
        plain = QUOTING.sub("", token)
        if not plain.startswith("--"):
            return
        if plain != token:
            yield token, True
            continue
        flag, _, value = plain.partition("=")
        flag = flag.lower()
        if flag == "--from" and keyword in ("copy", "add"):
            yield value, False
        elif flag == "--mount" and keyword == "run":
            for field in value.split(","):
                key, _, name = field.partition("=")
                if key.lower() == "from":
                    yield name, False


# ── rules ───────────────────────────────────────────────────────────────────


def check_image(path, line, ref, want):
    """Apply the reference rules; want is the manifest stage to compare with, or None."""
    if "$" in ref:
        violation(path, line, "variable-reference", f"image reference {ref} contains a variable")
        return
    registry, repository, tag, digest = split_reference(ref)
    if tag is None:
        violation(path, line, "missing-tag", f"image reference {ref} has no tag")
        return
    if digest is None or not DIGEST.fullmatch(digest):
        violation(path, line, "missing-digest", f"image reference {ref} has no @sha256: digest of 64 hex characters")
    if want is None:
        return
    if (registry, repository) != (want["registry"], want["repository"]):
        violation(
            path,
            line,
            "identity-mismatch",
            f"{registry}/{repository} is not the declared {want['registry']}/{want['repository']}",
        )
    if not re.fullmatch(want["tag"], tag):
        violation(path, line, "tag-mismatch", f"tag {tag} does not match {want['tag']}")


def check_file(path, text, entry):
    unreadable = False
    for number, line in enumerate(text.split("\n"), 1):
        start = line.lstrip(BLANK)
        if has_control(line) or not (line.isascii() or start.startswith("#")):
            violation(
                path,
                number,
                "unsupported-character",
                "a control character, or a non-ASCII character outside a comment line",
            )
            unreadable = True
        elif start.startswith("//"):
            violation(path, number, "syntax-directive", "a line that starts with // can carry a BuildKit directive")
    if unreadable:
        return  # the lines below cannot be split the way BuildKit splits them
    directives, instructions = parse(text)
    declared = entry.get("syntax")
    for line, key, value, at_top in directives:
        if key == "escape":
            violation(path, line, "escape-directive", "# escape= is not supported; continuation assumes a backslash")
        elif not at_top:
            violation(
                path,
                line,
                "syntax-directive",
                "BuildKit reads # syntax= only at the top of the file, unindented; here it is a comment",
            )
        elif declared is None:
            violation(path, line, "syntax-directive", f"# syntax={value} is not declared in the manifest")
        elif value != declared:
            violation(path, line, "syntax-directive", f"# syntax={value} differs from the manifest value {declared}")
    if declared is not None and not any(key == "syntax" for _, key, _, _ in directives):
        violation(path, 1, "syntax-directive", f"the manifest declares # syntax={declared} but the file has none")

    wanted = entry["stages"]
    contexts = entry.get("build_contexts", [])
    aliases, from_lines = [], []  # one item per FROM seen so far
    for line, body in instructions:
        if "<<" in body:
            violation(path, line, "heredoc", "a heredoc (or any <<) is not supported; its body can hide instructions")
        keyword, args = (body.split(None, 1) + ["", ""])[:2]  # body is empty for a lone backslash
        onbuild = keyword.lower() == "onbuild"
        if onbuild:
            keyword, args = (args.split(None, 1) + ["", ""])[:2]
        keyword = keyword.lower()
        if keyword != "from":
            for source, quoted in sources(keyword, args) if keyword in ("copy", "add", "run") else ():
                if quoted:
                    violation(
                        path,
                        line,
                        "external-copy-source",
                        f"flag {source} is quoted or escaped and cannot be checked",
                    )
                    continue
                if onbuild:  # BuildKit resolves a trigger's source against the child build's stages
                    violation(
                        path,
                        line,
                        "external-copy-source",
                        f"ONBUILD {keyword.upper()} source {source} is resolved by the child build, not by this file",
                    )
                    continue
                earlier = len(aliases) - 1  # stages 0..earlier-1 precede the current one
                # A stage index is valid for COPY and ADD only; in a mount it is an image name.
                by_index = keyword != "run" and re.fullmatch(r"[0-9]+", source) and int(source) < earlier
                if not (source.lower() in aliases[:-1] or by_index or source in contexts):
                    violation(
                        path,
                        line,
                        "external-copy-source",
                        f"{keyword.upper()} source {source} is not an earlier stage or a declared build context",
                    )
            continue
        want = wanted[len(aliases)] if len(aliases) < len(wanted) else None
        from_lines.append(line)
        parsed = parse_from(args)
        if parsed is None:
            aliases.append(None)
            violation(path, line, "stage-mismatch", "cannot parse the arguments of FROM")
            continue
        ref, alias = parsed
        aliases.append(alias)
        want_alias = alias if want is None else (want.get("alias", "").lower() or None)
        if alias != want_alias:
            violation(
                path,
                line,
                "stage-mismatch",
                f"alias {alias or '(none)'} differs from the manifest alias {want_alias or '(none)'}",
            )
        if ref == "scratch":
            if want is not None and not want.get("scratch"):
                violation(path, line, "stage-mismatch", "FROM scratch, but the manifest declares an image")
            continue
        if want is not None and want.get("scratch"):
            violation(path, line, "stage-mismatch", "an image, but the manifest declares scratch")
            want = None
        check_image(path, line, ref, want)

    if len(from_lines) != len(wanted):
        line = from_lines[len(wanted)] if len(from_lines) > len(wanted) else (from_lines[-1] if from_lines else 1)
        violation(
            path,
            line,
            "stage-mismatch",
            f"the file has {len(from_lines)} FROM lines and the manifest declares {len(wanted)}",
        )


# ── driver ──────────────────────────────────────────────────────────────────


def read_text(path):
    """Return the file's text with CRLF as LF and a leading BOM dropped, as BuildKit reads it.

    Binary mode on purpose: text mode would turn a lone CR into a line break.
    """
    try:
        with open(path, "rb") as fh:
            text = fh.read().decode("utf-8-sig")
    except (OSError, ValueError) as err:
        raise CheckError(f"cannot read {path}: {err}") from err
    return text.replace("\r\n", "\n")


def names_dockerfile(path):
    """True when the path contains "dockerfile" once compatibility forms, letter case and accents are folded.

    A case-insensitive checkout (macOS, Windows) writes "dockerfile", or a name
    with U+212A KELVIN SIGN for K, over the real Dockerfile. Such a path must be
    listed so that it is refused as unlisted.
    """
    folded = unicodedata.normalize("NFKD", path).casefold()
    return "dockerfile" in "".join(char for char in folded if not unicodedata.combining(char))


def tracked_files(root):
    """Return the tracked Dockerfile paths; a symbolic link or a submodule among them is a CheckError."""
    try:
        proc = subprocess.run(["git", "-C", root, "ls-files", "-s", "-z"], capture_output=True, check=False)
    except OSError as err:
        raise CheckError(f"cannot run git ls-files in {root}: {err}") from err
    if proc.returncode != 0:
        detail = proc.stderr.decode(errors="replace").strip()
        raise CheckError(f"git ls-files failed in {root} (exit {proc.returncode}): {detail}")
    found = set()
    for record in proc.stdout.split(b"\0"):
        if not record:
            continue
        meta, _, name = record.partition(b"\t")  # "<mode> <object> <stage>\t<path>"
        path = os.fsdecode(name)
        if not names_dockerfile(path):
            continue
        if not meta.startswith(b"100"):
            raise CheckError(f"{path} is tracked with mode {meta[:6].decode()}, not a regular file")
        found.add(path)
    return found


def run(root, manifest_path, files):
    entries = load_manifest(manifest_path)
    if files:
        tracked = {os.path.relpath(os.path.join(root, name), root) for name in files}
    else:
        tracked = tracked_files(root)
    rel = os.path.relpath(manifest_path, root)
    shown = manifest_path if rel.startswith("..") else rel
    for path in sorted(tracked):
        if path not in entries:
            violation(path, 1, "unlisted-dockerfile", f"no entry for {path} in {shown}")
            continue
        check_file(path, read_text(os.path.join(root, path)), entries[path])
    for path in sorted(set(entries) - tracked):
        violation(shown, 1, "stale-entry", f"entry {path} names a file that is not tracked")


def main(argv=None):
    parser = argparse.ArgumentParser(description="Check Dockerfile base images against .github/base-images.json.")
    parser.add_argument("--root", default=REPO_ROOT, help="repository root (default: the root holding this script)")
    parser.add_argument("--manifest", help="manifest path (default: <root>/.github/base-images.json)")
    parser.add_argument(
        "files",
        nargs="*",
        metavar="FILE",
        help='the complete set of Dockerfiles to check, relative to --root; manifest entries outside it are '
        'reported as stale (default: every tracked path that contains "dockerfile" in any letter case)',
    )
    args = parser.parse_args(argv)
    manifest_path = args.manifest or os.path.join(args.root, ".github", "base-images.json")
    violations.clear()
    try:
        run(args.root, manifest_path, args.files)
    except CheckError as err:
        print(f"check-base-images: {err}", file=sys.stderr)
        return 2
    except Exception:  # an internal error must not read as "violations found"
        traceback.print_exc()
        return 2
    for path, line, rule, message in sorted(violations, key=lambda v: (v[0], v[1])):
        # A path that is not valid UTF-8 carries surrogates; print it escaped.
        print(f"{path}:{line}: {rule}: {message}".encode(errors="backslashreplace").decode())
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main())
