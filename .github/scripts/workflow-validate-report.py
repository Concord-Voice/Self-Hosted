#!/usr/bin/env python3
"""Render bounded V1/V7 workflow findings from Git objects, never PR code."""

import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import unicodedata
from pathlib import Path


MAX_FILES = 64
MAX_BLOB_BYTES = 1_048_576
MAX_TOTAL_BYTES = 8_388_608
MAX_COMMENT_BYTES = 32_768
SAFE_WORKFLOW = re.compile(rb"^\.github/workflows/[A-Za-z0-9._-]+\.yml$")
SHA = re.compile(r"^[0-9a-f]{40}$")
PARSER = Path(__file__).with_name("parse-dispatched-workflow.sh")
FOOTER = (
    "\n\nStatic analysis only — no workflow is dispatched. V1 surfaces environment "
    "scopes (⚠️ flags production-class envs); V7 surfaces referenced secret names."
)


class Refused(Exception):
    pass


def git(*args: str) -> bytes:
    try:
        return subprocess.run(
            ("git", *args), check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE
        ).stdout
    except (OSError, subprocess.CalledProcessError) as exc:
        raise Refused("Git object lookup failed") from exc


def resolve_commit(sha: str, label: str) -> str:
    if not SHA.fullmatch(sha):
        raise Refused(f"invalid {label} SHA")
    try:
        value = git("rev-parse", "--verify", f"{sha}^{{commit}}").decode("ascii").strip()
    except Refused as exc:
        raise Refused("invalid commit") from exc
    if value != sha:
        raise Refused("invalid commit")
    return value


def clean_cell(value: str) -> str:
    """Escape HTML and Markdown metacharacters, including mentions and links."""
    out = []
    for char in value:
        if char in "\r\n\u2028\u2029" or unicodedata.category(char) in {"Cc", "Cf"}:
            out.append(" ")
        elif char == "&":
            out.append("&amp;")
        elif char == "<":
            out.append("&lt;")
        elif char == ">":
            out.append("&gt;")
        elif char in ":.":
            out.append(f"&#{ord(char)};")
        elif char == "@":
            out.append("&#64;")
        elif char == "`":
            out.append("&#96;")
        elif char in r"\`*_{}[]()#+!|~":
            out.append("\\" + char)
        else:
            out.append(char)
    return "".join(out)


def parse_blob(path: bytes, blob: bytes, temp_dir: str):
    data_file = Path(temp_dir) / "workflow.yml"
    data_file.write_bytes(blob)
    try:
        proc = subprocess.Popen(
            ("bash", str(PARSER), str(data_file)),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )
    except OSError as exc:
        raise Refused("trusted parser could not start") from exc
    try:
        stdout, _ = proc.communicate(timeout=15)
    except subprocess.TimeoutExpired as exc:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            proc.communicate(timeout=1)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.communicate()
        raise Refused("workflow parser timed out") from exc
    if proc.returncode != 0:
        return path, "parse-error", [], []
    try:
        value = json.loads(stdout)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise Refused("invalid parser output") from exc
    if (
        not isinstance(value, dict)
        or set(value) != {"environments", "secrets"}
        or any(
            not isinstance(value[key], list)
            or any(not isinstance(item, str) for item in value[key])
            for key in ("environments", "secrets")
        )
    ):
        raise Refused("invalid parser output")
    conclusion = (
        "⚠️ production-env"
        if any("production" in env.lower() for env in value["environments"])
        else "static-ok"
    )
    return path, conclusion, value["environments"], value["secrets"]


def analyze(base_sha: str, head_sha: str):
    base = resolve_commit(base_sha, "base")
    head = resolve_commit(head_sha, "head")
    merge_base = git("merge-base", base, head).decode("ascii").strip()
    if not SHA.fullmatch(merge_base):
        raise Refused("invalid merge base")
    diff_args = (
        "diff",
        "--no-renames",
        "--name-only",
        "-z",
        "--diff-filter=AMT",
        merge_base,
        head,
        "--",
        ":(glob).github/workflows/*.yml",
    )
    changed = [path for path in git(*diff_args).split(b"\0") if path]
    if len(changed) > MAX_FILES:
        raise Refused("too many changed workflow files")
    added = {
        path
        for path in git(
            "diff",
            "--no-renames",
            "--name-only",
            "-z",
            "--diff-filter=A",
            merge_base,
            head,
            "--",
            ":(glob).github/workflows/*.yml",
        ).split(b"\0")
        if path
    }
    rows = []
    new_files = []
    total = 0
    with tempfile.TemporaryDirectory(prefix="workflow-validate-") as temp_dir:
        for path in changed:
            if not SAFE_WORKFLOW.fullmatch(path):
                raise Refused("unsupported workflow path")
            entry = git("ls-tree", "-z", head, "--", path.decode("ascii"))
            try:
                metadata, listed_path = entry.rstrip(b"\0").split(b"\t", 1)
                mode, kind, oid = metadata.split(b" ")
            except ValueError as exc:
                raise Refused("malformed Git tree entry") from exc
            if listed_path != path or mode not in (b"100644", b"100755") or kind != b"blob":
                raise Refused("non-regular workflow file")
            size_data = git("cat-file", "-s", oid.decode("ascii")).strip()
            if not size_data.isdigit():
                raise Refused("invalid workflow object size")
            size = int(size_data)
            total += size
            if size > MAX_BLOB_BYTES or total > MAX_TOTAL_BYTES:
                raise Refused("workflow input too large")
            blob = git("cat-file", "blob", oid.decode("ascii"))
            if len(blob) != size:
                raise Refused("workflow object length changed")
            if path in added:
                new_files.append(path.decode("ascii"))
            else:
                rows.append(parse_blob(path, blob, temp_dir))
    return rows, new_files


def render(rows, new_files) -> str:
    parts = []
    if new_files:
        listed = ", ".join(clean_cell(name) for name in new_files)
        parts.append(
            "**Newly-added workflows cannot be dispatched by L3.**\n\n"
            "The GitHub Actions registry is anchored on the default branch. Review "
            f"these additions with L2 static lints and manual inspection: {listed}."
        )
    if rows:
        table = [
            "**Workflow-validate static analysis (V1/V7)**",
            "",
            "| Workflow | V1 status | Env scopes | Secrets accessed |",
            "|---|---|---|---|",
        ]
        for path, conclusion, environments, secrets in rows:
            def joined(items):
                return ", ".join(items) if items else "(none)"

            table.append(
                "| "
                + " | ".join(
                    clean_cell(value)
                    for value in (
                        path.decode("ascii"),
                        conclusion,
                        joined(environments),
                        joined(secrets),
                    )
                )
                + " |"
            )
        parts.append("\n".join(table))
    if parts:
        parts.append(FOOTER.strip())
    return "\n\n".join(parts)


def write_outputs(summary_path: str, comment_path: str, summary: str, comment: str):
    Path(comment_path).write_bytes(comment.encode("utf-8"))
    Path(summary_path).write_text(summary + "\n", encoding="utf-8")


def main(argv) -> int:
    if len(argv) != 5:
        print("usage: workflow-validate-report.py BASE_SHA HEAD_SHA SUMMARY_PATH COMMENT_PATH", file=sys.stderr)
        return 2
    _, base_sha, head_sha, summary_path, comment_path = argv
    try:
        Path(comment_path).write_bytes(b"")
        rows, new_files = analyze(base_sha, head_sha)
        comment = render(rows, new_files)
        if len(comment.encode("utf-8")) > MAX_COMMENT_BYTES:
            raise Refused("comment too large")
        summary = comment if comment else "No qualifying workflow changes found."
        write_outputs(summary_path, comment_path, summary, comment)
        return 0
    except Refused as exc:
        try:
            write_outputs(summary_path, comment_path, f"Workflow validation refused: {exc}.", "")
        except OSError:
            pass
        return 1
    except Exception:
        try:
            write_outputs(
                summary_path,
                comment_path,
                "Workflow validation refused: report generation failed.",
                "",
            )
        except OSError:
            pass
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
