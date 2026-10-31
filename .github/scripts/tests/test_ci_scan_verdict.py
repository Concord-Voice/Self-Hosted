"""Behavioral tests for the protected PR scan verdict seam.

Fixtures model GitHub's current API fields and the Task 2 artifact reports;
they intentionally contain no live identifiers or alert text.
"""
import copy
import contextlib
import io
import importlib.util
import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime
from types import SimpleNamespace
from email.message import Message
from http.client import IncompleteRead
from urllib.error import HTTPError, URLError
from pathlib import Path
from unittest.mock import patch


MODULE_PATH = Path(__file__).resolve().parents[1] / "verify-pr-scan-results.py"
SPEC = importlib.util.spec_from_file_location("scan_verdict", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("cannot load scan verdict module")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

HEAD, BASE, MERGE, POLICY = (letter * 40 for letter in "abcd")
PR = {"number": 3691, "head": {"sha": HEAD, "ref": "feature", "repo": {"full_name": "contributor/repo"}},
      "base": {"sha": BASE, "ref": "main", "repo": {"full_name": "Concord-Voice/Self-Hosted"}},
      "merge_commit_sha": MERGE}
RUN_ID, ATTEMPT = "7001", "2"
PR_REF = "refs/pull/3691/merge"
WRAPPER, MAIN = "e" * 40, "f" * 40


def main_ref(sha=MAIN, *, ref="refs/heads/main", kind="commit"):
    """GET /git/ref/heads/main, in the shape the live API returns."""
    return {"ref": ref, "object": {"type": kind, "sha": sha}}


def compare_response(status, ancestor, *, ahead=0, behind=0, merge_base=None):
    """GET /compare/<ancestor>...<tip>, reduced to the fields `_ancestor` reads.

    Shapes captured from the live API on 2026-10-14: `identical` and `ahead` carry
    the ancestor as both base and merge base; `behind` and `diverged` carry another
    commit as the merge base.
    """
    return {"status": status, "ahead_by": ahead, "behind_by": behind,
            "total_commits": ahead, "base_commit": {"sha": ancestor},
            "merge_base_commit": {"sha": merge_base or ancestor}}


def selection(categories=None, coverage=None, sonar=False):
    keys = ("codeql_actions", "codeql_go", "codeql_js", "eslint", "govulncheck", "osv",
            "semgrep_custom", "semgrep_community", "semgrep_canary", "sonar", "sonar_force_all",
            "sonar_desktop", "sonar_control_plane", "sonar_media_plane", "sonar_admin")
    cats = dict.fromkeys(keys, False)
    cats.update({"codeql_go": True} if categories is None else categories)
    required = dict.fromkeys(("desktop", "control_plane", "media_plane", "admin"), False)
    required.update(coverage or {})
    return {"version": 1, "known": True, "mode": "required-relevant",
            "head_sha": HEAD, "base_sha": BASE, "merge_sha": MERGE,
            "policy_sha": POLICY, "selected": cats,
            "coverage_required": required}


def analysis(tool="CodeQL", category="/language:go", *, aid=11, sarif_id="s-11",
             ref=PR_REF, commit=MERGE, sarif=None, status="complete", error=None):
    return {"id": aid, "analysis_key": ".github/workflows/pr-ci.yml:analyze",
            "category": category, "commit_sha": commit, "created_at": "2026-10-08T12:00:00Z",
            "environment": "github", "error": error, "warning": None, "ref": ref,
            "tool": tool, "sarif_id": sarif_id, "results_count": 0,
            "rules_count": 0, "processing_status": status,
            "sarif": sarif if sarif is not None else {"runs": [{"tool": {"driver": {"name": tool}},
                "results": []}]}}


def evidence_for(items=None):
    item = analysis() if items is None else items[0]
    payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
               "category": item["category"], "sarif_id": item["sarif_id"]}
    if item["category"] in ("semgrep-custom", "semgrep-community"):
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "uploads": [{"category": item["category"], "sarif_id": item["sarif_id"]}]}
    suffix = {"/language:actions": "codeql-actions", "/language:go": "codeql-go",
              "/language:javascript-typescript": "codeql-javascript-typescript",
              "eslint": "eslint", "govulncheck": "govulncheck",
              "semgrep-custom": "semgrep", "semgrep-community": "semgrep",
              ".github/workflows/osv-scanner.yml:osv-scan": "osv"}.get(item["category"])
    reports = {}
    osv_correlations = []
    if item["tool"] == "osv-scanner":
        osv_correlations = [{"workflow_path": ".github/workflows/osv-scanner.yml",
            "run_id": int(RUN_ID), "run_attempt": int(ATTEMPT), "ref": PR_REF,
            "commit_sha": MERGE, "category": item["category"], "status": "completed",
            "conclusion": "success", "unique": True}]
    if suffix:
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-{suffix}"
        reports[name] = {"report": payload, "artifact": {"id": 501, "name": name,
            "expired": False, "size_in_bytes": 128,
            "workflow_run": {"id": int(RUN_ID)}, "created_at": "2026-10-08T12:00:00Z"}}
    referenced_workflows = [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                             "sha": POLICY}]
    if any(scan.get("tool") == "osv-scanner" for scan in (items or [item])):
        referenced_workflows.append({"path": "Concord-Voice/Self-Hosted/.github/workflows/osv-scanner.yml@" + POLICY,
                                     "sha": POLICY})
    return {"run": {"id": RUN_ID, "run_attempt": int(ATTEMPT),
                "head_sha": HEAD,
                "referenced_workflows": referenced_workflows},
            "source_policy_sha": POLICY, "provenance_verified": True,
            "artifact_verified": True, "osv_source_verified": any(
                scan.get("tool") == "osv-scanner" for scan in (items or [item])), "reports": reports,
            "osv_correlations": osv_correlations,
            "analyses": items if items is not None else [item],
            "latest_analyses": copy.deepcopy(items if items is not None else [item]),
            "pagination_complete": True, "http_errors": [],
            "alerts": [], "main_alerts": [], "instances": {},
            "diff": {"complete": True, "added_lines": {}},
            "coverage_artifacts": [], "sonar": None}


def replace_report(ev, category, sarif_id="s-11"):
    suffix = {"/language:actions": "codeql-actions", "/language:go": "codeql-go",
              "/language:javascript-typescript": "codeql-javascript-typescript",
              "eslint": "eslint", "govulncheck": "govulncheck",
              "semgrep-custom": "semgrep", "semgrep-community": "semgrep",
              ".github/workflows/osv-scanner.yml:osv-scan": "osv"}[category]
    name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-{suffix}"
    payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
               "category": category, "sarif_id": sarif_id}
    if category.startswith("semgrep-"):
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "uploads": [{"category": category, "sarif_id": sarif_id}]}
    ev["reports"] = {name: {"report": payload, "artifact": {"id": 501,
        "name": name, "expired": False, "size_in_bytes": 128,
        "workflow_run": {"id": int(RUN_ID)}, "created_at": "2026-10-08T12:00:00Z"}}}


def result(rule_level="warning", security=None, *, result_level=None,
           path="main.go", start=2, end=None, alert=23, state="open",
           baseline=False):
    props = {"github/alertNumber": alert}
    if security is not None:
        props["security-severity"] = security
    item = {"ruleId": "RULE-1", "level": result_level,
            "message": {"text": "synthetic result"}, "properties": props,
            "locations": [{"physicalLocation": {"artifactLocation": {"uri": path},
                "region": {"startLine": start, **({"endLine": end} if end is not None else {})}}}]}
    return item


def with_finding(*, tool="CodeQL", category="/language:go", **kwargs):
    ev = evidence_for([analysis(tool, category)])
    ev["analyses"][0]["sarif"] = {"runs": [{"tool": {"driver": {"name": tool,
        "rules": [{"id": "RULE-1", "defaultConfiguration": {"level": "warning"},
                   "properties": {}}]}}, "results": [result(**kwargs)]}]}
    ev["analyses"][0]["results_count"] = 1
    ev["latest_analyses"] = copy.deepcopy(ev["analyses"])
    ev["alerts"] = [{"number": kwargs.get("alert", 23), "rule": {"id": "RULE-1",
        "severity": kwargs.get("result_level") or kwargs.get("rule_level", "warning"),
        "security_severity_level": ("high" if kwargs.get("security") and float(kwargs["security"]) >= 7
                                    else "medium" if kwargs.get("security") and float(kwargs["security"]) >= 4
                                    else None)},
        "state": kwargs.get("state", "open"), "most_recent_instance": {"ref": PR_REF}}]
    ev["instances"] = {str(kwargs.get("alert", 23)): [{"ref": PR_REF, "state": "open"}]}
    return ev

def run_git(root, *args):
    """Run git against a fixture repository with no inherited GIT_* variables."""
    return subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True,
                          text=True, env=MODULE.GIT_ENV).stdout.strip()


def init_fixture_repo(root):
    run_git(root, "init", "-q", "--template=")
    run_git(root, "config", "user.name", "Scan Verdict Test")
    run_git(root, "config", "user.email", "scan-verdict@example.invalid")
    run_git(root, "config", "commit.gpgsign", "false")
    run_git(root, "config", "tag.gpgsign", "false")
    run_git(root, "config", "core.autocrlf", "false")


def commit_files(root, message, files, deletions=()):
    for name, content in files.items():
        if isinstance(content, bytes):
            (root / name).write_bytes(content)
        else:
            (root / name).write_text(content)
    for name in deletions:
        (root / name).unlink()
    run_git(root, "add", "-A")
    run_git(root, "commit", "-q", "--no-verify", "-m", message)
    return run_git(root, "rev-parse", "HEAD")


def build_merge_fixture(root, main_files, head_first=False):
    """O forks into M (main, with main_files) and H (PR appends pr-line); X merges the two.

    X has parents (M, H), or (H, M) when head_first. Returns (M, H, X, O). Every commit is
    made on a detached HEAD.
    """
    init_fixture_repo(root)
    shared = "o1\no2\no3\no4\no5\no6\n"
    origin = commit_files(root, "fork point", {"shared.go": shared})
    run_git(root, "checkout", "-q", "--detach", origin)
    main_tip = commit_files(root, "main", main_files)
    run_git(root, "checkout", "-q", "--detach", origin)
    head = commit_files(root, "pr", {"shared.go": shared + "pr-line\n"})
    first, second = (head, main_tip) if head_first else (main_tip, head)
    run_git(root, "checkout", "-q", "--detach", first)
    run_git(root, "merge", "-q", "--no-ff", "--no-edit", "-m", "merge", second)
    merge = run_git(root, "rev-parse", "HEAD")
    return main_tip, head, merge, origin


def fake_git_diff(output):
    """Popen stand-in: a real child process that prints `output` where `git diff` would."""
    real_popen = subprocess.Popen

    def popen(command, **kwargs):
        return real_popen([sys.executable, "-c", f"import sys; sys.stdout.write({output!r})"], **kwargs)
    return popen


class DiffLinesTests(unittest.TestCase):
    def test_git_quoted_paths_keep_hunks_with_their_own_file(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-diff-") as temp:
            root = Path(temp)

            def git(*args):
                return subprocess.run(["git", "-C", str(root), *args], check=True,
                    capture_output=True, text=True).stdout.strip()

            git("init", "-q")
            git("config", "user.name", "Scan Verdict Test")
            git("config", "user.email", "scan-verdict@example.invalid")
            git("config", "core.quotepath", "true")
            names = ("plain.go", "sp ace.go", "café.go", 'quo"te.go', "tab\tname.go")
            for name in names:
                (root / name).write_text("old\n")
            git("add", ".")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            for name in names:
                (root / name).write_text("old\n" +
                    ("++ b/forged.go\n" if name == "plain.go" else "new\n"))
            git("add", ".")
            git("commit", "-qm", "head")
            head = git("rev-parse", "HEAD")

            with patch.object(MODULE, "REPO_ROOT", root):
                added = MODULE._diff_lines(base, head)
            self.assertEqual({name: [2] for name in names}, added)
            self.assertNotIn("forged.go", added)

            evidence = with_finding(path="café.go", start=2)
            evidence["diff"]["added_lines"] = added
            reasons = MODULE.verify_verdict(selection(),
                {"codeql": "success", "build": "success"}, evidence, PR)
            self.assertTrue(any("new blocking finding" in reason for reason in reasons), reasons)
            self.assertFalse(any("diff location unresolved" in reason for reason in reasons), reasons)

    def _linear_fixture(self, root, head_files, head_deletions):
        init_fixture_repo(root)
        base = commit_files(root, "base", {"a_gone.go": "x1\nx2\n",
                                           "m_kept.go": "k1\nk2\nk3\n", "z_gone.go": "y1\n"})
        head = commit_files(root, "head", head_files, head_deletions)
        return base, head

    def test_deleted_files_do_not_raise_and_do_not_swallow_neighbouring_hunks(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-delete-") as temp:
            root = Path(temp)
            base, head = self._linear_fixture(root, {"m_kept.go": "k1\nk2\nnew\nk3\n"},
                                              ("a_gone.go", "z_gone.go"))
            with patch.object(MODULE, "REPO_ROOT", root):
                try:
                    added = MODULE._diff_lines(base, head)
                except MODULE.Unknown as exc:
                    self.fail("a PR that deletes files must still yield its added lines "
                              f"(deleted sections skipped, neighbours kept): {exc}")
            self.assertEqual({"m_kept.go": [3]}, added)

    def test_pure_deletions_yield_no_added_lines(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-delete-only-") as temp:
            root = Path(temp)
            base, head = self._linear_fixture(root, {"m_kept.go": "k1\nk3\n"}, ("a_gone.go",))
            with patch.object(MODULE, "REPO_ROOT", root):
                try:
                    added = MODULE._diff_lines(base, head)
                except MODULE.Unknown as exc:
                    self.fail("a PR that only deletes files and lines must yield no added "
                              f"lines instead of raising: {exc}")
            self.assertEqual([], [number for numbers in added.values() for number in numbers])

    SEPARATORS = ("\x0b", "\x0c", "\x1c", "\x1d", "\x1e", "\x85", "\u2028", "\u2029", "\r")

    def test_line_separators_in_file_content_cannot_forge_diff_headers(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-separators-") as temp:
            root = Path(temp)
            init_fixture_repo(root)
            lines = [f"const l{i} = {i};" for i in range(1, 21)]
            base = commit_files(root, "base", {"app.js": ("\n".join(lines) + "\n").encode("utf-8")})
            for sep in self.SEPARATORS:
                for forged in ("+++ /dev/null", "+++ b/decoy.js"):
                    with self.subTest(sep=repr(sep), forged=forged):
                        run_git(root, "checkout", "-q", "--detach", base)
                        edited = list(lines)
                        edited[1] = "// note" + sep + "diff --git a/x b/x" + sep + forged
                        edited[15] = "eval(userInput);"
                        head = commit_files(root, f"head {sep!r} {forged}",
                                            {"app.js": ("\n".join(edited) + "\n").encode("utf-8")})
                        raw = subprocess.run(["git", "-C", str(root), "diff", "--unified=0", base, head],
                                             check=True, capture_output=True, env=MODULE.GIT_ENV).stdout
                        self.assertIn(sep.encode("utf-8") + b"diff --git a/x b/x" + sep.encode("utf-8"), raw,
                                      "fixture must put the separator and the forged header in git's raw output")
                        with patch.object(MODULE, "REPO_ROOT", root):
                            try:
                                added = MODULE._diff_lines(base, head)
                            except MODULE.Unknown as exc:
                                self.fail("a line separator inside file content must not forge a diff "
                                          f"header or abort the parse: {exc}")
                        self.assertEqual({"app.js": None}, added,
                                         "a separator inside file content must leave the path unresolved: "
                                         "no line numbers, no decoy path, no lost hunk")

    def test_global_git_config_cannot_change_the_diff_format(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-hostile-config-") as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            home = Path(temp) / "home"
            home.mkdir()
            (home / ".gitconfig").write_text("[diff]\n\tnoprefix = true\n[color]\n\tui = always\n")
            base, head = self._linear_fixture(root, {"m_kept.go": "k1\nk2\nnew\nk3\n"},
                                              ("a_gone.go", "z_gone.go"))
            hostile = dict(MODULE.GIT_ENV, HOME=str(home), XDG_CONFIG_HOME=str(home / "xdg"))
            plain = subprocess.run(["git", "-C", str(root), "diff", base, head], check=True,
                                   capture_output=True, text=True, env=hostile).stdout
            self.assertIn("+++ ", plain)
            self.assertNotIn("+++ b/", plain,
                             "positive control: the hostile global config must change git's diff format")
            with patch.object(MODULE, "REPO_ROOT", root), patch.object(MODULE, "GIT_ENV", hostile):
                try:
                    added = MODULE._diff_lines(base, head)
                except MODULE.Unknown as exc:
                    self.fail("the diff format must be pinned so a global git config cannot "
                              f"change how added lines are parsed: {exc}")
            self.assertEqual({"m_kept.go": [3]}, added,
                             "added lines must be the same under a hostile global git config")

    def _binary_fixture(self, root):
        init_fixture_repo(root)
        base = commit_files(root, "base", {"keep.txt": "keep\n"})
        head = commit_files(root, "head", {"evil.js": b"// \x00\neval(userInput);\n", "ok.js": "x = 1\n"})
        return base, head

    def test_binary_detected_files_are_unresolvable_not_empty(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-binary-") as temp:
            root = Path(temp)
            base, head = self._binary_fixture(root)
            with patch.object(MODULE, "REPO_ROOT", root):
                added = MODULE._diff_lines(base, head)
            self.assertEqual({"evil.js": None, "ok.js": [1]}, added,
                             "a file git treats as binary has no line information: it must be "
                             "recorded as None, not omitted or empty")

    def test_blocking_finding_in_a_binary_detected_file_is_unresolved(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-binary-finding-") as temp:
            root = Path(temp)
            base, head = self._binary_fixture(root)
            with patch.object(MODULE, "REPO_ROOT", root):
                added = MODULE._diff_lines(base, head)
            evidence = with_finding(path="evil.js", start=2)
            evidence["diff"]["added_lines"] = added
            reasons = MODULE.verify_verdict(selection(),
                {"codeql": "success", "build": "success"}, evidence, PR)
            self.assertTrue(any("diff location unresolved" in reason for reason in reasons),
                            f"a blocking finding in a binary-detected file must be unresolved: {reasons}")

    AMBIGUOUS_BREAKS = ("\r", "\r\r", "\r\r\r", "\x0b", "\x0c", "\x1c", "\x1d", "\x1e",
                        "\x85", "\u2028", "\u2029")

    def _ambiguous_break_base(self, root):
        init_fixture_repo(root)
        return commit_files(root, "base", {
            "wf.yml": b"name: ci\non: push\njobs:\n  a:\n    steps: []\n", "ok.yml": b"k: 1\n"})

    def _ambiguous_break_head(self, root, base, brk):
        """Check out base and commit a wf.yml whose added `# pinned` line holds `brk` mid-line."""
        run_git(root, "checkout", "-q", "--detach", base)
        content = ("name: ci\non: push\njobs:\n  a:\n    steps:\n      # pinned" + brk
                   + "      - uses: actions/checkout@main\n").encode("utf-8")
        head = commit_files(root, f"head {brk!r}", {"wf.yml": content, "ok.yml": b"k: 1\nz: 2\n"})
        raw = subprocess.run(["git", "-C", str(root), "diff", "--unified=0", base, head],
                             check=True, capture_output=True, env=MODULE.GIT_ENV).stdout
        return head, raw

    def test_added_lines_with_ambiguous_line_breaks_make_the_path_unresolvable(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-ambiguous-") as temp:
            root = Path(temp)
            base = self._ambiguous_break_base(root)
            for brk in self.AMBIGUOUS_BREAKS:
                with self.subTest(brk=repr(brk)):
                    head, raw = self._ambiguous_break_head(root, base, brk)
                    self.assertIn(b"\n+      # pinned" + brk.encode("utf-8") + b"      - uses:", raw,
                                  "fixture must put the break inside git's single added line")
                    with patch.object(MODULE, "REPO_ROOT", root):
                        try:
                            added = MODULE._diff_lines(base, head)
                        except MODULE.Unknown as exc:
                            self.fail("a character a scanner counts as a line break but git does not "
                                      f"must not abort the parse: {exc}")
                    self.assertIsNone(added.get("wf.yml", "absent"),
                                      "a path with an added line holding a scanner-only line break has "
                                      "no reliable line numbers: it must be recorded as None")
                    self.assertEqual([2], added.get("ok.yml"),
                                     "an unaffected path must keep its exact added lines")

    def test_crlf_line_endings_are_not_ambiguous(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-crlf-") as temp:
            root = Path(temp)
            init_fixture_repo(root)
            base = commit_files(root, "base", {"crlf.yml": b"a: 1\r\nb: 2\r\nc: 3\r\nd: 4\r\n"})
            head = commit_files(root, "head", {"crlf.yml": b"a: 1\r\nb: 2\r\nc: changed\r\nd: 4\r\n"})
            raw = subprocess.run(["git", "-C", str(root), "diff", "--unified=0", base, head],
                                 check=True, capture_output=True, env=MODULE.GIT_ENV).stdout
            self.assertIn(b"\n+c: changed\r\n", raw, "fixture must add a line that ends in CRLF")
            with patch.object(MODULE, "REPO_ROOT", root):
                added = MODULE._diff_lines(base, head)
            self.assertEqual({"crlf.yml": [3]}, added,
                             "a trailing CR is a line ending, not an ambiguous break")

    def test_blocking_finding_on_an_ambiguous_line_break_path_is_unresolved(self):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-ambiguous-finding-") as temp:
            root = Path(temp)
            base = self._ambiguous_break_base(root)
            head, _ = self._ambiguous_break_head(root, base, "\r\r\r")
            with patch.object(MODULE, "REPO_ROOT", root):
                added = MODULE._diff_lines(base, head)
            # git numbers the added "# pinned" line 6; a scanner counting each lone CR reports +3.
            evidence = with_finding(path="wf.yml", start=6 + 3)
            evidence["diff"]["added_lines"] = added
            reasons = MODULE.verify_verdict(selection(),
                {"codeql": "success", "build": "success"}, evidence, PR)
            self.assertTrue(any("diff location unresolved" in reason for reason in reasons),
                            "a blocking finding past a scanner-only line break must be unresolved, "
                            f"not judged against git's line numbers: {reasons}")

    # Skipping a deleted file's hunks must not loosen what counts as a malformed diff.
    DELETED_SECTION = ("diff --git a/gone.go b/gone.go\n"
                       "deleted file mode 100644\n"
                       "--- a/gone.go\n"
                       "+++ /dev/null\n"
                       "@@ -1 +0,0 @@\n"
                       "-x\n")

    def test_a_hunk_with_no_file_section_still_fails_closed(self):
        with patch.object(MODULE.subprocess, "Popen", fake_git_diff("@@ -1 +1 @@\n+x\n")), \
                self.assertRaisesRegex(MODULE.Unknown, "diff hunk invalid"):
            MODULE._diff_lines(BASE, MERGE)

    def test_a_headerless_section_after_a_deleted_file_still_fails_closed(self):
        diff = self.DELETED_SECTION + "diff --git a/next.go b/next.go\n@@ -1 +1 @@\n+y\n"
        with patch.object(MODULE.subprocess, "Popen", fake_git_diff(diff)), \
                self.assertRaisesRegex(MODULE.Unknown, "diff hunk invalid"):
            MODULE._diff_lines(BASE, MERGE)

    def test_a_headed_section_after_a_deleted_file_is_recorded(self):
        # Positive control for the two cases above: the same text with the header present parses.
        diff = self.DELETED_SECTION + ("diff --git a/next.go b/next.go\n"
                                       "--- a/next.go\n+++ b/next.go\n@@ -1 +1 @@\n-x\n+y\n")
        with patch.object(MODULE.subprocess, "Popen", fake_git_diff(diff)):
            try:
                added = MODULE._diff_lines(BASE, MERGE)
            except MODULE.Unknown as exc:
                self.fail(f"a well-formed deleted-then-edited diff must parse: {exc}")
        self.assertEqual({"next.go": [1]}, added)


class BaseParentTests(unittest.TestCase):
    """_base_parent reads the diff base off the merge commit and refuses any other shape."""

    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="scan-base-parent-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.main_tip, self.head, self.merge, self.origin = build_merge_fixture(
            self.root, {"main-only.md": "m1\n"})
        patcher = patch.object(MODULE, "REPO_ROOT", self.root)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_returns_the_parent_that_is_not_the_head(self):
        self.assertEqual(self.main_tip, MODULE._base_parent(self.head, self.merge))

    def test_refuses_a_commit_whose_only_parent_is_the_head(self):
        run_git(self.root, "checkout", "-q", "--detach", self.head)
        child = commit_files(self.root, "child", {"child.txt": "c\n"})
        with self.assertRaisesRegex(MODULE.Unknown, "does not join the PR head",
                                    msg="a one-parent commit is not a merge of base and head"):
            MODULE._base_parent(self.head, child)

    def test_refuses_an_octopus_merge(self):
        run_git(self.root, "checkout", "-q", "--detach", self.origin)
        third = commit_files(self.root, "third", {"third.txt": "t\n"})
        run_git(self.root, "checkout", "-q", "--detach", self.main_tip)
        run_git(self.root, "merge", "-q", "--no-ff", "--no-edit", "-m", "octopus", self.head, third)
        octopus = run_git(self.root, "rev-parse", "HEAD")
        self.assertEqual(4, len(run_git(self.root, "rev-list", "--parents", "-n", "1", octopus).split()))
        with self.assertRaisesRegex(MODULE.Unknown, "does not join the PR head",
                                    msg="a merge of more than two parents is not GitHub's PR merge"):
            MODULE._base_parent(self.head, octopus)


class ScanVerdictTests(unittest.TestCase):
    def verdict(self, ev=None, sel=None, jobs=None, pr=None):
        job_state = {"codeql": "success", "build": "success"} if jobs is None else dict(jobs)
        if jobs is not None:
            job_state.setdefault("build", "success")
        return MODULE.verify_verdict(selection() if sel is None else sel,
            job_state,
            evidence_for() if ev is None else ev, PR if pr is None else pr)

    def test_selected_codeql_without_current_analysis_is_rejected(self):
        ev = evidence_for(); ev["analyses"] = []; ev["latest_analyses"] = []
        self.assertIn("CodeQL: selected analysis missing", self.verdict(ev))

    def test_shared_semgrep_report_preserves_both_selected_upload_ids(self):
        selected = selection({"semgrep_custom": True, "semgrep_community": True})["selected"]
        report_name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-semgrep"
        reports = {report_name: {"report": {"run_id": RUN_ID, "run_attempt": ATTEMPT,
            "uploads": [{"category": "semgrep-custom", "sarif_id": "custom-id"},
                        {"category": "semgrep-community", "sarif_id": "community-id"}]}}}
        self.assertEqual({"semgrep-custom": "custom-id", "semgrep-community": "community-id"},
                         MODULE._report_ids(reports, selected, RUN_ID, ATTEMPT))
        invalid_uploads = (
            [{"category": "semgrep-custom", "sarif_id": "custom-id"}],
            [{"category": "semgrep-custom", "sarif_id": "custom-id"},
             {"category": "semgrep-community", "sarif_id": "custom-id"}],
            [{"category": "semgrep-custom", "sarif_id": "custom-id"},
             {"category": "semgrep-community", "sarif_id": "community-id"},
             {"category": "semgrep-other", "sarif_id": "extra-id"}],
        )
        for uploads in invalid_uploads:
            with self.subTest(uploads=uploads), self.assertRaises(MODULE.Unknown):
                reports[report_name]["report"]["uploads"] = uploads
                MODULE._report_ids(reports, selected, RUN_ID, ATTEMPT)

    def test_protected_osv_report_uses_the_established_category_and_current_attempt(self):
        selected = selection({"osv": True})["selected"]
        report_name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
        report = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                  "category": ".github/workflows/osv-scanner.yml:osv-scan", "sarif_id": "osv-id"}
        reports = {report_name: {"report": report, "artifact": {"id": 504, "name": report_name,
            "expired": False, "workflow_run": {"id": int(RUN_ID)}}}}
        self.assertEqual({report["category"]: "osv-id"},
                         MODULE._report_ids(reports, selected, RUN_ID, ATTEMPT))

    def test_standalone_osv_run_without_protected_report_cannot_satisfy_selection(self):
        selected = selection({"osv": True})
        evidence = evidence_for([analysis("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan",
                                          sarif_id="standalone-id")])
        evidence["osv_source_verified"] = False
        evidence["reports"] = {}
        reasons = MODULE.verify_verdict(selected, {"build": "success", "osv": "success"}, evidence, PR)
        self.assertTrue(any("OSV" in reason or "osv" in reason.lower() for reason in reasons), reasons)

    def test_selected_protected_osv_job_must_succeed(self):
        selected = selection({"osv": True})
        evidence = evidence_for([analysis("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan",
                                          sarif_id="osv-id")])
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
        evidence["reports"] = {name: {"report": {"run_id": RUN_ID, "run_attempt": ATTEMPT,
            "category": ".github/workflows/osv-scanner.yml:osv-scan", "sarif_id": "osv-id"},
            "artifact": {"id": 505, "name": name, "expired": False,
                "workflow_run": {"id": int(RUN_ID)}}}}
        evidence["osv_source_verified"] = True
        reasons = MODULE.verify_verdict(selected, {"build": "success", "osv": "failure"}, evidence, PR)
        self.assertTrue(any("OSV" in reason or "osv" in reason.lower() for reason in reasons), reasons)

    def test_protected_osv_evidence_remains_valid_when_pr_edits_standalone_workflow(self):
        selected = selection({"osv": True})
        item = analysis("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", sarif_id="protected-id")
        evidence = evidence_for([item])
        evidence["osv_correlations"] = []
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
        evidence["reports"] = {name: {"report": {"run_id": RUN_ID, "run_attempt": ATTEMPT,
            "category": ".github/workflows/osv-scanner.yml:osv-scan", "sarif_id": "protected-id"},
            "artifact": {"id": 506, "name": name, "expired": False,
                "workflow_run": {"id": int(RUN_ID)}}}}
        evidence["osv_source_verified"] = True
        evidence["diff"]["added_lines"] = {".github/workflows/osv-scanner.yml": [1]}
        reasons = MODULE.verify_verdict(selected, {"build": "success", "osv": "success"}, evidence, PR)
        self.assertEqual([], reasons)

    def test_protected_osv_accepts_server_parent_category_only_with_exact_sarif_identity(self):
        expected = ".github/workflows/osv-scanner.yml:osv-scan"
        selected = selection({"osv": True})
        evidence = evidence_for([analysis("osv-scanner", expected, sarif_id="protected-id")])
        item = evidence["analyses"][0]
        item["category"] = ".github/workflows"
        item["analysis_key"] = ".github/workflows/required-pr-build.yml:protected-pr-publish"
        item["sarif"]["runs"][0]["automationDetails"] = {"id": expected}
        evidence["latest_analyses"] = copy.deepcopy(evidence["analyses"])
        self.assertEqual([], self.verdict(evidence, selected, {"osv": "success"}))
        for field, wrong in (("analysis_key", ".github/workflows/pr-ci.yml:protected-pr-publish"),
                             ("category", ".github/other"),
                             ("automationDetails", {"id": ".github/workflows/other.yml:osv-scan"})):
            with self.subTest(field=field):
                candidate = copy.deepcopy(evidence)
                target = candidate["analyses"][0]
                if field == "automationDetails":
                    target["sarif"]["runs"][0][field] = wrong
                else:
                    target[field] = wrong
                self.assertTrue(any("selected analysis missing" in reason for reason in
                                    self.verdict(candidate, selected, {"osv": "success"})))

    def test_osv_source_provenance_rejects_a_pr_controlled_or_wrong_callee(self):
        selected = selection({"osv": True})
        evidence = evidence_for([analysis("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan",
                                          sarif_id="protected-id")])
        evidence["osv_source_verified"] = False
        reasons = MODULE.verify_verdict(selected, {"build": "success", "osv": "success"}, evidence, PR)
        self.assertTrue(any("OSV workflow source" in reason for reason in reasons), reasons)

    def test_selection_requires_closed_fifteen_keys_and_four_coverage_flags(self):
        cases = []
        missing_key = selection(); del missing_key["selected"]["sonar_admin"]
        cases.append((missing_key, "selection"))
        unknown_key = selection(); unknown_key["selected"]["surprise"] = False
        cases.append((unknown_key, "selection"))
        invalid_bool = selection(); invalid_bool["selected"]["osv"] = "false"
        cases.append((invalid_bool, "selection"))
        missing_coverage = selection(); del missing_coverage["coverage_required"]["admin"]
        cases.append((missing_coverage, "coverage"))
        invalid_coverage = selection(); invalid_coverage["coverage_required"]["admin"] = None
        cases.append((invalid_coverage, "coverage"))
        for selected, diagnostic in cases:
            with self.subTest(diagnostic=diagnostic, selection=selected):
                self.assertTrue(any(diagnostic in reason.lower() for reason in self.verdict(sel=selected)))

    def test_each_selected_scanner_category_requires_matching_analysis(self):
        cases = [
            ("codeql_actions", "CodeQL", "/language:actions"),
            ("codeql_js", "CodeQL", "/language:javascript-typescript"),
            ("semgrep_custom", "Semgrep OSS", "semgrep-custom"),
            ("semgrep_community", "Semgrep OSS", "semgrep-community"),
            ("eslint", "ESLint", "eslint"),
            ("govulncheck", "govulncheck", "govulncheck"),
            ("osv", "osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan"),
        ]
        job_names = {"codeql_actions": "codeql", "codeql_go": "codeql", "codeql_js": "codeql",
                     "semgrep_custom": "semgrep", "semgrep_community": "semgrep",
                     "eslint": "eslint", "govulncheck": "govulncheck", "osv": "osv"}
        for key, tool, category in cases:
            with self.subTest(category=category):
                sel = selection({key: True})
                ev = evidence_for([analysis(tool, category)])
                if category != ".github/workflows/osv-scanner.yml:osv-scan":
                    replace_report(ev, category)
                reasons = self.verdict(ev, sel, {job_names[key]: "success"})
                self.assertEqual([], reasons)

    def test_selected_skipped_failed_or_cancelled_job_is_rejected(self):
        for state in ("skipped", "failure", "cancelled"):
            with self.subTest(state=state):
                self.assertTrue(any("CodeQL" in r for r in self.verdict(jobs={"codeql": state})))

    def test_selected_scanner_without_job_record_is_rejected(self):
        ev = evidence_for()
        self.assertTrue(any("selected job missing" in r for
            r in self.verdict(ev, selection({"eslint": True}), {})))

    def test_semgrep_canary_only_requires_successful_aggregate_job_without_sarif(self):
        sel = selection({"semgrep_canary": True})
        ev = evidence_for()
        self.assertEqual([], self.verdict(ev, sel, {"semgrep": "success"}))
        for state in (None, "skipped", "failure", "cancelled"):
            jobs = {"build": "success"}
            if state is not None:
                jobs["semgrep"] = state
            with self.subTest(state=state):
                self.assertTrue(any("semgrep" in reason.lower() for reason in self.verdict(ev, sel, jobs)))

    def test_analysis_must_match_ref_revision_and_actual_upload(self):
        cases = [("ref", "refs/heads/main", "wrong ref"),
                 ("commit_sha", HEAD, "wrong revision"),
                 ("sarif_id", "old-upload", "upload")]
        for field, value, diagnostic in cases:
            with self.subTest(field=field):
                ev = evidence_for(); ev["analyses"][0][field] = value
                if field == "sarif_id":
                    ev["latest_analyses"][0][field] = value
                self.assertTrue(any(diagnostic.lower() in r.lower() for r in self.verdict(ev)))

    def test_protected_provenance_policy_and_artifact_metadata_are_required(self):
        cases = [("provenance_verified", False, "provenance"),
                 ("artifact_verified", False, "artifact")]
        for key, value, expected in cases:
            ev = evidence_for(); ev[key] = value
            with self.subTest(key=key):
                self.assertTrue(any(expected in reason.lower() for reason in self.verdict(ev)))
        ev = evidence_for(); ev["source_policy_sha"] = "e" * 40
        self.assertTrue(any("policy" in reason.lower() for reason in self.verdict(ev)))
        ev = evidence_for()
        artifact = next(iter(ev["reports"].values()))["artifact"]
        artifact["expired"] = True
        ev["artifact_verified"] = False
        self.assertTrue(any("artifact" in reason.lower() for reason in self.verdict(ev)))
        for workflows in ([],
            [{"path": "attacker/workflow.yml@" + POLICY, "ref": PR_REF, "sha": POLICY}],
            [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + "e" * 40,
              "ref": PR_REF, "sha": "e" * 40}]):
            ev = evidence_for(); ev["run"]["referenced_workflows"] = workflows
            with self.subTest(workflows=workflows):
                self.assertTrue(any("provenance" in reason.lower() or "workflow" in reason.lower()
                                    for reason in self.verdict(ev)))
        for ref in ("refs/heads/main", PR_REF):
            ev = evidence_for(); ev["run"]["referenced_workflows"][0]["ref"] = ref
            with self.subTest(allowed_provenance_ref=ref):
                self.assertEqual([], self.verdict(ev))
        ev = evidence_for(); ev["run"]["referenced_workflows"][0]["ref"] = "refs/pull/999/merge"
        self.assertTrue(any("provenance" in reason.lower() for reason in self.verdict(ev)))

    def test_platform_provenance_requires_protected_source_and_main_ancestry(self):
        run = {"path": ".github/workflows/required-pr-build.yml@refs/heads/main",
               "event": "pull_request", "head_sha": HEAD,
               "repository": {"full_name": "Concord-Voice/Self-Hosted"},
               "workflow_id": 77,
               "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/77",
               "referenced_workflows": [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                   "sha": POLICY}]}
        pr = {"number": 3691, "head": {"sha": HEAD},
              "base": {"ref": "main", "sha": POLICY,
                       "repo": {"full_name": "Concord-Voice/Self-Hosted"}}}
        identical = {"status": "identical", "ahead_by": 0, "behind_by": 0,
                     "total_commits": 0, "base_commit": {"sha": POLICY},
                     "merge_base_commit": {"sha": POLICY}}
        # After the comparison the arguments are policy_sha, local_sha (the wrapper commit) and
        # main_sha (main's live tip), which the comparison sets against the wrapper commit.
        self.assertTrue(MODULE._protected_provenance(run, pr, identical, POLICY, POLICY, POLICY))
        for path in (".github/workflows/required-pr-build.yml@main",
                     "Concord-Voice/Self-Hosted/.github/workflows/required-pr-build.yml@main"):
            with self.subTest(path=path):
                valid_run = dict(run, path=path)
                self.assertTrue(MODULE._protected_provenance(valid_run, pr, identical, POLICY, POLICY, POLICY))
        # Main moved on since the run began, so the wrapper is an older main commit.
        ahead = {"status": "ahead", "ahead_by": 2, "behind_by": 0,
                 "total_commits": 2, "base_commit": {"sha": POLICY},
                 "merge_base_commit": {"sha": POLICY}}
        self.assertTrue(MODULE._protected_provenance(run, pr, ahead, POLICY, POLICY, "e" * 40))
        # The PR's own base is no part of the proof: it lags main on every PR that has not merged it.
        for base in (BASE, "e" * 40, MAIN):
            stale = copy.deepcopy(pr); stale["base"]["sha"] = base
            with self.subTest(pr_base=base):
                self.assertTrue(MODULE._protected_provenance(run, stale, ahead, POLICY, POLICY, "e" * 40))
        mutations = []
        bad = copy.deepcopy(pr); bad["base"]["ref"] = "release"
        mutations.append((run, bad, identical, POLICY, POLICY, POLICY))
        bad = copy.deepcopy(pr); bad["base"]["repo"]["full_name"] = "attacker/Self-Hosted"
        mutations.append((run, bad, identical, POLICY, POLICY, POLICY))
        bad_run = copy.deepcopy(run); bad_run["path"] = "Concord-Voice/Self-Hosted/.github/workflows/required-pr-build.yml@refs/pull/3691/merge"
        mutations.append((bad_run, pr, identical, POLICY, POLICY, POLICY))
        bad_run = copy.deepcopy(run); bad_run["referenced_workflows"][0]["path"] = "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + HEAD
        mutations.append((bad_run, pr, identical, POLICY, POLICY, POLICY))
        bad_run = copy.deepcopy(run); bad_run["referenced_workflows"][0]["ref"] = "refs/pull/999/merge"
        mutations.append((bad_run, pr, identical, POLICY, POLICY, POLICY))
        bad_run = copy.deepcopy(run); bad_run["workflow_url"] = "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/workflows/77"
        mutations.append((bad_run, pr, identical, POLICY, POLICY, POLICY))
        divergent = dict(identical, status="diverged", behind_by=1)
        mutations.append((run, pr, divergent, POLICY, POLICY, POLICY))
        # An unmerged branch that contains main's tip and goes past it: the wrapper is ahead of main.
        beyond_main = {"status": "behind", "ahead_by": 0, "behind_by": 3, "total_commits": 0,
                       "base_commit": {"sha": POLICY}, "merge_base_commit": {"sha": "e" * 40}}
        mutations.append((run, pr, beyond_main, POLICY, POLICY, "e" * 40))
        mutations.append((run, pr, identical, POLICY, "e" * 40, POLICY))
        # A comparison vouches only for the tip it was made against, and the tip must be a commit id.
        mutations.append((run, pr, identical, POLICY, POLICY, "e" * 40))
        for tip in ("main", "", None):
            mutations.append((run, pr, ahead, POLICY, POLICY, tip))
        for args in mutations:
            with self.subTest(args=args):
                self.assertFalse(MODULE._protected_provenance(*args))

    def test_ancestry_proof_rejects_each_inconsistent_comparison(self):
        # Every condition `_ancestor` checks must matter on its own: a comparison that is valid
        # except for one field has to fail, whatever the other fields say.
        ahead = compare_response("ahead", WRAPPER, ahead=3)
        self.assertTrue(MODULE._ancestor(ahead, WRAPPER, MAIN))
        self.assertTrue(MODULE._ancestor(compare_response("identical", WRAPPER), WRAPPER, WRAPPER))
        for label, change in (
                ("the ancestor is behind the tip", {"behind_by": 1}),
                ("the behind count is missing", {"behind_by": None}),
                ("the commit count differs from the ahead count", {"total_commits": 2}),
                ("the counts are not integers", {"ahead_by": "3", "total_commits": "3"}),
                ("the base commit is another commit", {"base_commit": {"sha": MAIN}}),
                ("the merge base is another commit", {"merge_base_commit": {"sha": MAIN}}),
                ("the base commit is not an object", {"base_commit": WRAPPER}),
                ("the merge base is not an object", {"merge_base_commit": None}),
                ("the status is behind", {"status": "behind"}),
                ("the status is diverged", {"status": "diverged"}),
                ("the status is ahead with nothing ahead", {"ahead_by": 0, "total_commits": 0})):
            with self.subTest(label):
                self.assertFalse(MODULE._ancestor(dict(ahead, **change), WRAPPER, MAIN))
        # The status must also agree with the two commits being compared.
        self.assertFalse(MODULE._ancestor(ahead, WRAPPER, WRAPPER))
        self.assertFalse(MODULE._ancestor(compare_response("identical", WRAPPER), WRAPPER, MAIN))
        self.assertFalse(MODULE._ancestor(
            dict(compare_response("identical", WRAPPER), ahead_by=3, total_commits=3), WRAPPER, WRAPPER))
        for label, comparison in (("not an object", [ahead]), ("missing", None)):
            with self.subTest(label):
                self.assertFalse(MODULE._ancestor(comparison, WRAPPER, MAIN))
        # Both ends must be commit ids, even when the comparison echoes them back consistently.
        for ancestor, descendant in (("main", MAIN), (WRAPPER, "main"), (WRAPPER, None)):
            with self.subTest(ancestor=ancestor, descendant=descendant):
                self.assertFalse(MODULE._ancestor(compare_response("ahead", ancestor, ahead=3),
                                                  ancestor, descendant))

    def test_selected_build_failure_cannot_turn_required_scan_irrelevant(self):
        self.assertTrue(any("build" in reason.lower() for reason in
            self.verdict(jobs={"codeql": "success", "build": "failure"})))
        missing_build = MODULE.verify_verdict(selection(), {"codeql": "success"},
            evidence_for(), PR)
        self.assertTrue(any("build" in reason.lower() for reason in missing_build))

    def test_incomplete_pagination_http_and_sarif_fail_closed(self):
        cases = [("pagination_complete", False, "pagination"),
                 ("http_errors", [{"status": 403}], "403"),
                 ("http_errors", [{"status": 503}], "503")]
        for key, value, diagnostic in cases:
            with self.subTest(key=key, value=value):
                ev = evidence_for(); ev[key] = value
                self.assertTrue(any(diagnostic in r for r in self.verdict(ev)))
        ev = evidence_for(); ev["analyses"][0]["sarif"] = "not-json"
        self.assertTrue(any("SARIF" in r for r in self.verdict(ev)))

    def test_unmatched_alert_and_invalid_location_are_reported(self):
        ev = with_finding(); ev["alerts"] = []
        self.assertTrue(any("alert" in r.lower() for r in self.verdict(ev)))
        ev = with_finding(path="", start=0)
        self.assertTrue(any("location" in r.lower() or "line" in r.lower() for r in self.verdict(ev)))

    def test_warning_thresholds_and_security_severity_precedence(self):
        # CodeQL/Semgrep warning findings block; informational findings do not.
        ev = with_finding(rule_level="warning")
        ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertTrue(self.verdict(ev))
        ev = with_finding(rule_level="note", result_level="note")
        ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertEqual([], self.verdict(ev))
        ev = with_finding(rule_level="error")
        ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertTrue(self.verdict(ev))
        ev = with_finding(rule_level="error", security="7.0")
        ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertTrue(self.verdict(ev))

    def test_security_severity_thresholds_are_tool_specific_and_take_precedence(self):
        cases = [
            ("CodeQL", "/language:go", "codeql", "4.0", True),
            ("CodeQL", "/language:go", "codeql", "3.9", False),
            ("Semgrep OSS", "semgrep-community", "semgrep_community", "4.0", True),
            ("govulncheck", "govulncheck", "govulncheck", "7.0", True),
            ("govulncheck", "govulncheck", "govulncheck", "6.9", False),
            ("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", "osv", "7.0", True),
            ("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", "osv", "6.9", False),
        ]
        selected_keys = {"/language:go": "codeql_go", "semgrep-community": "semgrep_community",
                         "govulncheck": "govulncheck"}
        for tool, category, job, severity, blocks in cases:
            with self.subTest(tool=tool, severity=severity):
                ev = with_finding(tool=tool, category=category, rule_level="note",
                                  result_level="note", security=severity)
                diff_path = "services/control-plane/main.go" if tool == "govulncheck" else "main.go"
                ev["diff"]["added_lines"] = {diff_path: [2]}
                if category.startswith("semgrep-"):
                    replace_report(ev, category)
                job_key = "govulncheck" if tool == "govulncheck" else "semgrep" if tool == "Semgrep OSS" else "codeql"
                if tool == "osv-scanner":
                    job_key = "osv"
                    selected_key = "osv"
                else:
                    selected_key = selected_keys[category]
                reasons = self.verdict(ev, selection({selected_key: True}), {job_key: "success"})
                self.assertEqual(blocks, bool(reasons), reasons)

    def test_osv_and_govulncheck_use_error_threshold_without_security_score(self):
        for tool, category, key, level, blocks in (
                ("govulncheck", "govulncheck", "govulncheck", "warning", False),
                ("govulncheck", "govulncheck", "govulncheck", "error", True),
                ("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", "osv", "warning", False),
                ("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", "osv", "error", True)):
            with self.subTest(tool=tool, level=level):
                ev = with_finding(tool=tool, category=category, rule_level=level, result_level=level)
                diff_path = "services/control-plane/main.go" if tool == "govulncheck" else "main.go"
                ev["diff"]["added_lines"] = {diff_path: [2]}
                selected = selection({key: True})
                self.assertEqual(blocks, bool(self.verdict(ev, selected, {key: "success"})))

    def test_enriched_security_severity_enum_precedes_informational_sarif_level(self):
        ev = with_finding(rule_level="note", result_level="note")
        ev["analyses"][0]["sarif"]["runs"][0]["results"][0]["properties"].pop("security-severity", None)
        ev["alerts"][0]["rule"]["security_severity_level"] = "high"
        ev["instances"]["23"] = [{"ref": PR_REF, "state": "open"}]
        ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertTrue(self.verdict(ev))

    def test_eslint_is_presence_only_and_does_not_invent_blocking_severity(self):
        ev = with_finding(tool="ESLint", category="eslint", rule_level="error", result_level="error")
        self.assertEqual([], self.verdict(ev, selection({"eslint": True}), {"eslint": "success"}))

    def test_osv_and_govulncheck_actual_note_default_warning_is_nonblocking(self):
        for key, tool, category in (("govulncheck", "govulncheck", "govulncheck"),
                                    ("osv", "osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan")):
            with self.subTest(tool=tool):
                sel = selection({key: True})
                ev = evidence_for([analysis(tool, category)])
                if tool != "osv-scanner":
                    replace_report(ev, category)
                ev["analyses"][0]["sarif"] = {"runs": [{"tool": {"driver": {"name": tool,
                    "rules": [{"id": "VULN", "defaultConfiguration": {"level": "warning"}, "properties": {}}]}},
                    "results": [{"ruleId": "VULN", "level": "note", "properties": {"github/alertNumber": 616},
                    "locations": [{"physicalLocation": {"artifactLocation": {"uri": "go.mod"},
                    "region": {"startLine": 1, "endLine": 1}}}]}]}]}
                ev["analyses"][0]["results_count"] = 1
                ev["alerts"] = [{"number": 616, "rule": {"id": "VULN", "severity": "note",
                    "security_severity_level": None}, "state": "dismissed",
                    "most_recent_instance": {"ref": "refs/heads/main"}}]
                self.assertEqual([], self.verdict(ev, sel, {key: "success"}))

    def test_dismissed_and_preexisting_findings_do_not_block(self):
        ev = with_finding(state="dismissed"); ev["diff"]["added_lines"] = {"main.go": [2]}
        self.assertEqual([], self.verdict(ev))
        ev = with_finding(); ev["diff"]["added_lines"] = {"main.go": [2]}
        ev["main_alerts"] = [{"number": 23, "rule": {"id": "RULE-1"}, "state": "open",
                               "most_recent_instance": {"ref": "refs/heads/main"}}]
        self.assertEqual([], self.verdict(ev))

    def test_every_line_of_multiline_finding_must_be_in_diff(self):
        ev = with_finding(start=2, end=4)
        ev["diff"]["added_lines"] = {"main.go": [2, 3]}
        self.assertTrue(any("diff" in r.lower() or "line" in r.lower() for r in self.verdict(ev)))

    def test_blocking_finding_wholly_outside_complete_diff_passes(self):
        ev = with_finding(start=2, end=4)
        ev["diff"]["added_lines"] = {}
        self.assertEqual([], self.verdict(ev))
        ev["diff"]["added_lines"] = {"main.go": [8, 9]}
        self.assertEqual([], self.verdict(ev))

    def test_coverage_and_sonar_identity_and_freshness_are_required(self):
        sel = selection({"codeql_go": True, "sonar": True}, coverage={"desktop": True, "control_plane": False,
            "media_plane": False, "admin": False}, sonar=True)
        ev = evidence_for(); ev["coverage_artifacts"] = [{"component": "desktop", "present": True}]
        ev["sonar"] = {"app_id": 12526, "conclusion": "success", "status": "completed",
                       "head_sha": HEAD, "analysis_window_verified": True}
        ev["analysis_window_verified"] = True
        jobs = {"codeql": "success", "sonar": "success", "build": "success",
                "coverage_uploads": {"desktop": True}}
        self.assertEqual([], self.verdict(ev, sel, jobs))
        ev["sonar"]["app_id"] = 1
        reasons = self.verdict(ev, sel, jobs)
        self.assertTrue(any("Sonar" in r for r in reasons))
        ev["sonar"]["app_id"] = 12526
        ev["sonar"]["head_sha"] = "e" * 40
        self.assertTrue(any("Sonar" in r and ("stale" in r.lower() or "head" in r.lower()) for r in self.verdict(ev, sel, jobs)))
        ev["sonar"]["head_sha"] = HEAD
        ev["coverage_artifacts"] = []
        self.assertTrue(any("coverage" in r.lower() for r in self.verdict(ev, sel, jobs)))

    def test_external_pr_uses_trusted_sonar_app_check_when_tokenless_job_skips(self):
        sel = selection({"codeql_go": True, "sonar": True})
        ev = evidence_for()
        ev["sonar"] = {"app_id": 12526, "conclusion": "success", "status": "completed",
                       "head_sha": HEAD, "analysis_window_verified": True}
        jobs = {"build": "success", "codeql": "success", "sonar": "skipped"}
        self.assertEqual([], self.verdict(ev, sel, jobs, pr=PR))
        same_repo = copy.deepcopy(PR)
        same_repo["head"]["repo"]["full_name"] = "Concord-Voice/Self-Hosted"
        self.assertTrue(any("Sonar selected build or scan" in reason
                            for reason in self.verdict(ev, sel, jobs, pr=same_repo)))
        # Dependabot is same-repository, so the protected bridge scans it: a
        # skipped bridge blocks it, and a scanned one needs proof and child.
        same_repo["user"] = {"login": "dependabot[bot]"}
        self.assertTrue(any("Sonar selected build or scan" in reason
                            for reason in self.verdict(ev, sel, jobs, pr=same_repo)))
        scanned = dict(jobs, sonar="success")
        self.assertTrue(any("Sonar caller proof" in reason
                            for reason in self.verdict(ev, sel, scanned, pr=same_repo)))
        ev["sonar_bridge"] = {"proof_verified": True, "child_verified": True}
        self.assertEqual([], self.verdict(ev, sel, scanned, pr=same_repo))
        for field in ("proof_verified", "child_verified"):
            broken = copy.deepcopy(ev)
            broken["sonar_bridge"][field] = False
            self.assertTrue(any("Sonar caller proof" in reason
                                for reason in self.verdict(broken, sel, scanned, pr=same_repo)))
        # The external exception never consults bridge evidence.
        self.assertEqual([], self.verdict(dict(ev, sonar_bridge=None), sel, jobs, pr=PR))
        ev["sonar"] = None
        self.assertTrue(any("Sonar App check" in reason
                            for reason in self.verdict(ev, sel, jobs, pr=PR)))

    def test_head_base_and_merge_movement_is_rejected(self):
        for field, value in (("head", {"sha": "e" * 40}),
                             ("base", {"sha": "e" * 40}),
                             ("merge_commit_sha", "e" * 40)):
            current = copy.deepcopy(PR); current[field] = value
            with self.subTest(field=field):
                self.assertTrue(self.verdict(pr=current))


class GitHubClientTests(unittest.TestCase):
    class Response:
        def __init__(self, url, body, link=""):
            self.url, self.body = url, body
            self.headers = {"Link": link}

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self, size=-1):
            return self.body[:size]

    def test_client_uses_fixed_host_and_never_places_token_in_url(self):
        captured = []
        def open_request(request, timeout):
            captured.append((request, timeout))
            return self.Response(request.full_url, b'{"ok":true}')
        client = MODULE.GitHub("synthetic-test-token")
        with patch.object(MODULE, "urlopen", open_request):
            body, _ = client.get("/pulls/3691")
        request, timeout = captured[0]
        self.assertEqual({"ok": True}, body)
        self.assertEqual("https://api.github.com/repos/Concord-Voice/Self-Hosted/pulls/3691",
                         request.full_url)
        self.assertNotIn("synthetic-test-token", request.full_url)
        self.assertEqual("Bearer synthetic-test-token", request.get_header("Authorization"))
        self.assertEqual(15, timeout)
        with self.assertRaises(MODULE.Unknown):
            client.get("https://attacker.invalid/collect")

    def test_client_accepts_commit_comparison_without_allowing_parent_paths(self):
        client = MODULE.GitHub("synthetic-test-token")
        comparison = f"/compare/{POLICY}...{BASE}"
        with patch.object(MODULE, "urlopen", lambda request, timeout:
                          self.Response(request.full_url, b"{}")):
            self.assertEqual(({}, ""), client.get(comparison))
            with self.assertRaisesRegex(MODULE.Unknown, "API path invalid"):
                client.get("/pulls/../secrets")

    def test_client_rejects_redirect_response_to_prevent_token_egress(self):
        client = MODULE.GitHub("synthetic-test-token")
        with patch.object(MODULE, "urlopen", lambda request, timeout:
                          self.Response("https://attacker.invalid/collect", b"{}")):
            with self.assertRaisesRegex(MODULE.Unknown, "redirected"):
                client.get("/pulls/3691")

    def test_client_maps_a_body_read_that_dies_midway_to_unknown(self):
        # urlopen wraps connect-time failures in URLError, but a read that dies afterwards raises
        # the raw error. Unmapped, the run ends in a traceback instead of an `unknown:` verdict.
        client = MODULE.GitHub("synthetic-test-token")
        def tear(failure):
            def open_torn(request, timeout):
                response = self.Response(request.full_url, b"{}")
                def read(size=-1):
                    raise failure
                response.read = read
                return response
            return open_torn
        for failure in (IncompleteRead(b'{"items"', 40), ConnectionResetError("reset by peer")):
            with self.subTest(type(failure).__name__), patch.object(MODULE, "urlopen", tear(failure)):
                with self.assertRaisesRegex(MODULE.Unknown, "API request invalid"):
                    client.get("/pulls/3691")

    def test_client_maps_connect_failures_and_unparseable_bodies_to_unknown(self):
        # The same traceback hazard from the two other directions: urlopen itself failing, and the
        # parser giving up. Python 3.11-3.13 raise RecursionError for a deeply nested body (the
        # runner's 3.12 does) and 3.14 does not, so the parser is patched to hold on every interpreter.
        client = MODULE.GitHub("synthetic-test-token")
        for failure in (URLError("unreachable"), TimeoutError("timed out")):
            with self.subTest(type(failure).__name__), patch.object(MODULE, "urlopen", side_effect=failure):
                with self.assertRaisesRegex(MODULE.Unknown, f"API request invalid: {type(failure).__name__}"):
                    client.get("/pulls/3691")
        with self.subTest("RecursionError"), patch.object(MODULE, "urlopen", lambda request, timeout:
                                                           self.Response(request.full_url, b"[]")), \
                patch.object(MODULE.json, "loads", side_effect=RecursionError):
            with self.assertRaisesRegex(MODULE.Unknown, "API request invalid: RecursionError"):
                client.get("/pulls/3691")

    def test_pages_follows_complete_link_chain_and_checks_page_shape(self):
        client = MODULE.GitHub("synthetic-test-token")
        urls = []
        def open_page(request, timeout):
            urls.append(request.full_url)
            if request.full_url.endswith("page=1"):
                return self.Response(request.full_url, b'{"items":[1]}', '<next>; rel="next"')
            return self.Response(request.full_url, b'{"items":[2]}')
        with patch.object(MODULE, "urlopen", open_page):
            self.assertEqual([1, 2], client.pages("/things", "items"))
        self.assertEqual(2, len(urls))
        with patch.object(MODULE, "urlopen", lambda request, timeout:
                          self.Response(request.full_url, b'{"items":"bad"}')):
            with self.assertRaisesRegex(MODULE.Unknown, "page shape"):
                client.pages("/things", "items")

    def test_pages_rejects_malformed_json_http_errors_and_unterminated_bound(self):
        client = MODULE.GitHub("synthetic-test-token")
        with patch.object(MODULE, "urlopen", lambda request, timeout:
                          self.Response(request.full_url, b"not-json")):
            with self.assertRaisesRegex(MODULE.Unknown, "API request invalid"):
                client.pages("/things")
        def forbidden(request, timeout):
            error = HTTPError(request.full_url, 403, "forbidden", {}, None)
            error.close()
            raise error
        with patch.object(MODULE, "urlopen", forbidden):
            with self.assertRaisesRegex(MODULE.Unknown, "HTTP 403"):
                client.pages("/things")
        calls = []
        def unterminated(request, timeout):
            calls.append(request.full_url)
            return self.Response(request.full_url, b"[]", '<next>; rel="next"')
        with patch.object(MODULE, "urlopen", unterminated):
            with self.assertRaisesRegex(MODULE.Unknown, "pagination incomplete"):
                client.pages("/things")
        self.assertEqual(20, len(calls))

    def test_pages_collects_current_main_alert_volume(self):
        client = MODULE.GitHub("synthetic-test-token")
        def open_page(request, timeout):
            page = int(request.full_url.rsplit("page=", 1)[1])
            body = json.dumps(list(range(100 if page < 13 else 48))).encode()
            link = '<next>; rel="next"' if page < 13 else ""
            return self.Response(request.full_url, body, link)
        with patch.object(MODULE, "urlopen", open_page):
            self.assertEqual(1248, len(client.pages("/code-scanning/alerts?ref=refs/heads/main")))

    def test_client_enforces_request_and_response_bounds(self):
        client = MODULE.GitHub("synthetic-test-token")
        client.requests = MODULE.MAX_REQUESTS
        with self.assertRaisesRegex(MODULE.Unknown, "request bound"):
            client.get("/pulls/3691")
        client = MODULE.GitHub("synthetic-test-token")
        body = b"x" * (MODULE.MAX_BYTES + 1)
        with patch.object(MODULE, "urlopen", lambda request, timeout:
                          self.Response(request.full_url, body)):
            with self.assertRaisesRegex(MODULE.Unknown, "response too large"):
                client.get("/pulls/3691")

    def test_downloaded_reports_require_exact_filename_current_run_and_artifact_metadata(self):
        selected = selection()["selected"]
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-codeql-go"
        metadata = {"id": 501, "name": name, "expired": False,
                    "workflow_run": {"id": int(RUN_ID)}, "size_in_bytes": 128}
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "category": "/language:go", "sarif_id": "s-11"}
        with tempfile.TemporaryDirectory(prefix="scan-artifact-") as temp:
            root = Path(temp)
            report_path = root / "scan-evidence-codeql-go.json"
            report_path.write_text(json.dumps(payload))
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                reports = MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)
            self.assertEqual(payload, reports[name]["report"])
            report_path.rename(root / "unexpected.json")
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                with self.assertRaisesRegex(MODULE.Unknown, "artifact contents"):
                    MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)
            (root / "unexpected.json").rename(report_path)
            bad_metadata = dict(metadata, workflow_run={"id": 7002})
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                with self.assertRaisesRegex(MODULE.Unknown, "artifact inventory"):
                    MODULE._local_reports(str(root), [bad_metadata], RUN_ID, ATTEMPT, selected)

    def test_single_semgrep_artifact_downloads_directly_and_rejects_symlink(self):
        selected = selection({"semgrep_community": True})["selected"]
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-semgrep"
        metadata = {"id": 505, "name": name, "expired": False,
                    "workflow_run": {"id": int(RUN_ID)}}
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "uploads": [{"category": "semgrep-community", "sarif_id": "sg-11"}]}
        with tempfile.TemporaryDirectory(prefix="single-semgrep-artifact-") as temp:
            root = Path(temp) / "download"
            root.mkdir()
            report_path = root / "scan-evidence-semgrep.json"
            report_path.write_text(json.dumps(payload))
            with patch.dict("os.environ", {"RUNNER_TEMP": temp}):
                reports = MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)
            self.assertEqual({"semgrep-community": "sg-11"},
                             MODULE._report_ids(reports, selected, RUN_ID, ATTEMPT))
            actual = Path(temp) / "actual.json"
            report_path.rename(actual)
            report_path.symlink_to(actual)
            with patch.dict("os.environ", {"RUNNER_TEMP": temp}), \
                    self.assertRaisesRegex(MODULE.Unknown, "local evidence file"):
                MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)

    def test_retry_receipt_ignores_duplicate_unrelated_artifacts_but_rejects_duplicate_receipts(self):
        selected = selection()["selected"]
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-codeql-go"
        metadata = {"id": 501, "name": name, "expired": False,
                    "workflow_run": {"id": int(RUN_ID)}}
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "category": "/language:go", "sarif_id": "s-11"}
        unrelated = [{"id": 601, "name": "control-plane-coverage-shard-2"},
                     {"id": 602, "name": "control-plane-coverage-shard-2"},
                     {"id": 603, "name": f"scan-evidence-{RUN_ID}-1-codeql-go"}]
        with tempfile.TemporaryDirectory(prefix="scan-retry-artifact-") as temp:
            root = Path(temp)
            (root / "scan-evidence-codeql-go.json").write_text(json.dumps(payload))
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                reports = MODULE._local_reports(str(root), [metadata, *unrelated], RUN_ID, ATTEMPT, selected)
                self.assertEqual(payload, reports[name]["report"])
                with self.assertRaisesRegex(MODULE.Unknown, "current-attempt artifact inventory incomplete"):
                    MODULE._local_reports(str(root), unrelated, RUN_ID, ATTEMPT, selected)
                with self.assertRaisesRegex(MODULE.Unknown, "artifact inventory ambiguous"):
                    MODULE._local_reports(str(root), [metadata, dict(metadata, id=502), *unrelated],
                                          RUN_ID, ATTEMPT, selected)

    def test_downloaded_protected_osv_report_is_current_attempt_and_exact_category(self):
        selected = selection({"osv": True})["selected"]
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
        metadata = {"id": 502, "name": name, "expired": False,
                    "workflow_run": {"id": int(RUN_ID)}, "size_in_bytes": 128}
        payload = {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                   "category": ".github/workflows/osv-scanner.yml:osv-scan", "sarif_id": "osv-id"}
        with tempfile.TemporaryDirectory(prefix="protected-osv-artifact-") as temp:
            root = Path(temp)
            (root / "scan-evidence-osv.json").write_text(json.dumps(payload))
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                reports = MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)
            self.assertEqual({payload["category"]: "osv-id"},
                             MODULE._report_ids(reports, selected, RUN_ID, ATTEMPT))
            payload["run_attempt"] = "1"
            (root / "scan-evidence-osv.json").write_text(json.dumps(payload))
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}), \
                    self.assertRaisesRegex(MODULE.Unknown, "current-attempt report"):
                MODULE._local_reports(str(root), [metadata], RUN_ID, ATTEMPT, selected)

    def test_downloaded_protected_osv_report_rejects_symlinked_artifact_directory(self):
        selected = selection({"osv": True, "codeql_go": True})["selected"]
        name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
        metadata = {"id": 503, "name": name, "expired": False,
                    "workflow_run": {"id": int(RUN_ID)}, "size_in_bytes": 128}
        codeql_name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-codeql-go"
        codeql_metadata = {"id": 504, "name": codeql_name, "expired": False,
                           "workflow_run": {"id": int(RUN_ID)}, "size_in_bytes": 128}
        with tempfile.TemporaryDirectory(prefix="protected-osv-symlink-") as temp:
            root = Path(temp)
            folder = root / name
            folder.mkdir()
            (folder / "scan-evidence-osv.json").write_text(json.dumps({
                "run_id": RUN_ID, "run_attempt": ATTEMPT}))
            (root / codeql_name).mkdir()
            (root / codeql_name / "scan-evidence-codeql-go.json").write_text(json.dumps({
                "run_id": RUN_ID, "run_attempt": ATTEMPT}))
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}):
                reports = MODULE._local_reports(str(root), [metadata, codeql_metadata],
                                                RUN_ID, ATTEMPT, selected)
            self.assertEqual({name, codeql_name}, set(reports))
            folder.rename(root / "actual")
            folder.symlink_to(root / "actual", target_is_directory=True)
            with patch.dict("os.environ", {"RUNNER_TEMP": str(root)}), \
                    self.assertRaisesRegex(MODULE.Unknown, "artifact directory"):
                MODULE._local_reports(str(root), [metadata, codeql_metadata], RUN_ID, ATTEMPT, selected)


class CliTests(unittest.TestCase):
    def invoke(self, evidence, final_pr=None, mode="required-relevant"):
        with tempfile.TemporaryDirectory(prefix="scan-verdict-cli-") as temp:
            root = Path(temp)
            selection_file, jobs_file = root / "selection.json", root / "jobs.json"
            selected = selection()
            selected["mode"] = mode
            selection_file.write_text(json.dumps(selected))
            jobs_file.write_text(json.dumps({"codeql": "success", "build": "success"}))

            class Api:
                def __init__(self, token):
                    self.token = token

                def get(self, path):
                    return (PR if final_pr is None else final_pr), ""

            args = ["verify-pr-scan-results.py", "--selection", str(selection_file),
                    "--jobs", str(jobs_file), "--pr", "3691", "--run-id", RUN_ID]
            output = io.StringIO()
            with patch.object(MODULE, "GitHub", Api), patch.object(MODULE, "_collect",
                    return_value=(PR, evidence)), patch("sys.argv", args), \
                    patch.dict("os.environ", {"GITHUB_TOKEN": "synthetic-token"}, clear=False), \
                    contextlib.redirect_stdout(output):
                result = MODULE.main()
            return result, output.getvalue()

    def test_cli_maps_incomplete_to_exit_two_and_policy_violation_to_exit_one(self):
        ev = evidence_for(); ev["analyses"] = []; ev["latest_analyses"] = []
        code, output = self.invoke(ev)
        self.assertEqual(2, code)
        self.assertIn("CodeQL: selected analysis missing", output)
        ev = with_finding(); ev["diff"]["added_lines"] = {"main.go": [2]}
        code, output = self.invoke(ev)
        self.assertEqual(1, code)
        self.assertIn("new blocking finding", output)

    def test_cli_rechecks_pr_identity_before_success(self):
        changed = copy.deepcopy(PR)
        changed["head"]["sha"] = "e" * 40
        code, output = self.invoke(evidence_for(), changed)
        self.assertEqual(2, code)
        self.assertIn("PR revision moved during verification", output)

    def test_cli_returns_success_only_after_final_pr_recheck(self):
        code, output = self.invoke(evidence_for(), PR)
        self.assertEqual(0, code)
        self.assertIn("selected current-revision scan evidence verified", output)

    def test_cli_summary_binds_required_relevant_and_full_modes(self):
        for mode in ("required-relevant", "required-full"):
            with self.subTest(mode=mode):
                code, output = self.invoke(evidence_for(), PR, mode=mode)
                self.assertEqual(0, code)
                self.assertIn(f"policy={POLICY} head={HEAD} merge={MERGE} mode={mode}", output)
                self.assertIn("selected=codeql_go", output)
                self.assertIn("skipped=codeql_actions", output)

    def test_cli_ignores_mutable_repository_metadata_on_final_recheck(self):
        changed = copy.deepcopy(PR)
        changed["head"]["repo"]["updated_at"] = "2026-10-09T00:00:00Z"
        changed["base"]["repo"]["stargazers_count"] = 123
        code, _ = self.invoke(evidence_for(), changed)
        self.assertEqual(0, code)

    def test_cli_rechecks_all_stable_revision_fields(self):
        for side, field, value in (("head", "ref", "other"), ("base", "ref", "release"),
                                   ("head", "repo", {"full_name": "another/fork"}),
                                   ("base", "repo", {"full_name": "another/repo"})):
            changed = copy.deepcopy(PR)
            changed[side][field] = value
            with self.subTest(side=side, field=field):
                code, output = self.invoke(evidence_for(), changed)
                self.assertEqual(2, code)
                self.assertIn("PR revision moved", output)


class ProtectedProvenanceCollectionTests(unittest.TestCase):
    """`_collect` must prove the workflow commit is on protected main, and nothing about the PR.

    The fake answers every compare the verifier could ask the way GitHub does, including
    the question the old proof asked (workflow commit against the PR's base), so a test
    cannot pass or fail merely on which paths happen to be served.
    """
    JOBS = {"build": "success", "semgrep": "success"}

    def collect(self, *, tip=MAIN, ref=None, tip_compare=None):
        pr = {"number": 3691, "head": {"sha": HEAD, "repo": {"full_name": "contributor/repo"}},
              "base": {"sha": BASE, "ref": "main",
                       "repo": {"full_name": "Concord-Voice/Self-Hosted"}},
              "merge_commit_sha": MERGE}
        run = {"id": int(RUN_ID), "run_attempt": int(ATTEMPT), "head_sha": HEAD,
               "path": ".github/workflows/required-pr-build.yml@refs/heads/main",
               "event": "pull_request", "repository": {"full_name": "Concord-Voice/Self-Hosted"},
               "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/77",
               "workflow_id": 77, "run_started_at": "2026-10-08T12:00:00Z",
               "updated_at": "2026-10-08T12:01:00Z",
               "referenced_workflows": [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                   "sha": POLICY}]}
        on_main = compare_response("identical", WRAPPER) if tip == WRAPPER \
            else compare_response("ahead", WRAPPER, ahead=1)
        answers = {
            "/pulls/3691": pr,
            f"/actions/runs/{RUN_ID}": run,
            "/git/ref/heads/main": main_ref(tip) if ref is None else ref,
            f"/compare/{WRAPPER}...{tip}": tip_compare or on_main,
            # The question the old proof asked. The PR's base is an older main commit,
            # so the workflow commit is ahead of it and GitHub answers "behind".
            f"/compare/{WRAPPER}...{BASE}": compare_response("behind", WRAPPER, behind=2, merge_base=BASE),
            f"/compare/{POLICY}...{WRAPPER}": compare_response("ahead", POLICY, ahead=1),
        }

        class Api:
            def __init__(self):
                self.paths = []

            def get(self, path, **_):
                self.paths.append(path)
                if path not in answers: raise AssertionError(f"unexpected API path: {path}")
                if isinstance(answers[path], Exception): raise answers[path]
                return answers[path], ""

            def pages(self, path, field=None):
                return []

        sel = selection({"semgrep_canary": True})
        with tempfile.TemporaryDirectory(prefix="scan-provenance-") as temp:
            env = {"GITHUB_RUN_ATTEMPT": ATTEMPT, "WORKFLOW_SHA": WRAPPER, "RUNNER_TEMP": temp,
                   "SCAN_EVIDENCE_DIR": temp}
            with patch.dict("os.environ", env), patch.object(MODULE, "_diff_lines", return_value={}), \
                    patch.object(MODULE, "_base_parent", return_value=BASE), \
                    patch.object(MODULE.subprocess, "run", return_value=SimpleNamespace(stdout=WRAPPER)):
                api = Api()
                current, evidence = MODULE._collect(api, sel, self.JOBS, 3691, int(RUN_ID))
        return sel, current, evidence, api.paths

    def test_a_pr_based_on_an_older_main_commit_passes_provenance(self):
        # A PR's recorded base moves only when the PR merges main. After main moves on,
        # every PR that has not merged it sits behind the workflow commit; that is not an error.
        for tip in (WRAPPER, MAIN):
            with self.subTest(main_tip_is_the_workflow_commit=tip == WRAPPER):
                sel, current, evidence, paths = self.collect(tip=tip)
                self.assertTrue(evidence["provenance_verified"])
                self.assertEqual([], MODULE.verify_verdict(sel, self.JOBS, evidence, current))
                self.assertIn("/git/ref/heads/main", paths)
                self.assertIn(f"/compare/{WRAPPER}...{tip}", paths)
                self.assertNotIn(f"/compare/{WRAPPER}...{BASE}", paths)

    def test_a_workflow_commit_that_is_not_on_main_fails_provenance(self):
        for label, tip_compare in (
                # an unmerged branch that contains main's tip and goes past it
                ("ahead of main", compare_response("behind", WRAPPER, behind=14, merge_base=MAIN)),
                # an unmerged branch that forked from main and lacks its later commits
                ("diverged from main", compare_response("diverged", WRAPPER, ahead=2, behind=34,
                                                        merge_base=BASE))):
            with self.subTest(label):
                sel, current, evidence, paths = self.collect(tip_compare=tip_compare)
                # A rejection proves nothing unless the proof asked its own question.
                self.assertIn(f"/compare/{WRAPPER}...{MAIN}", paths)
                self.assertFalse(evidence["provenance_verified"])
                reasons = MODULE.verify_verdict(sel, self.JOBS, evidence, current)
                self.assertTrue(any("provenance" in reason.lower() for reason in reasons))

    def test_an_unreadable_main_ref_fails_closed(self):
        for label, ref in (("another branch", main_ref(ref="refs/heads/release")),
                           ("a tag that is called main", main_ref(ref="refs/tags/main")),
                           ("a nested branch that ends in main", main_ref(ref="refs/heads/feature/main")),
                           ("no ref name", {"object": {"type": "commit", "sha": MAIN}}),
                           ("a tag object", main_ref(kind="tag")),
                           ("a tree object", main_ref(kind="tree")),
                           ("no object type", {"ref": "refs/heads/main", "object": {"sha": MAIN}}),
                           ("not a commit id", main_ref(sha="main")),
                           ("an uppercase commit id", main_ref(sha=MAIN.upper())),
                           ("no object", {"ref": "refs/heads/main"}),
                           ("a prefix-match list", [main_ref()]),
                           ("not an object at all", "refs/heads/main")):
            with self.subTest(label), self.assertRaisesRegex(MODULE.Unknown, "main ref"):
                self.collect(ref=ref)
        # A lookup that fails outright is surfaced, never swallowed into a weaker proof.
        with self.assertRaisesRegex(MODULE.Unknown, "no such ref"):
            self.collect(ref=MODULE.Unknown("no such ref"))


class ProtectedCollectionTests(unittest.TestCase):
    def test_instance_queries_exclude_advisory_eslint_and_baseline_alerts(self):
        eslint = {"tool": "ESLint", "sarif": {"runs": [{"results": [
            {"properties": {"github/alertNumber": n}} for n in range(1, 151)]}]}}
        codeql = {"tool": "CodeQL", "sarif": {"runs": [{"results": [
            {"properties": {"github/alertNumber": n}} for n in (151, 152, 153)]}]}}
        alerts = [{"number": n, "state": "open"} for n in range(1, 154)]
        alerts[151]["state"] = "dismissed"  # alert 152
        baseline = [{"number": 153, "state": "open"}]
        self.assertEqual({151}, MODULE._instance_alert_numbers([eslint, codeql], alerts, baseline))

    def test_collect_uses_fixed_compare_paths_and_rejects_diverged_ancestry(self):
        with tempfile.TemporaryDirectory(prefix="scan-provenance-") as temp:
            wrapper_sha = "e" * 40
            pr = {"number": 3691, "head": {"sha": HEAD, "repo": {"full_name": "contributor/repo"}},
                  "base": {"sha": BASE, "ref": "main",
                           "repo": {"full_name": "Concord-Voice/Self-Hosted"}},
                  "merge_commit_sha": MERGE}
            run = {"id": int(RUN_ID), "run_attempt": int(ATTEMPT), "head_sha": HEAD,
                   "path": ".github/workflows/required-pr-build.yml@refs/heads/main",
                   "event": "pull_request", "repository": {"full_name": "Concord-Voice/Self-Hosted"},
                   "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/77",
                   "workflow_id": 77, "run_started_at": "2026-10-08T12:00:00Z",
                   "updated_at": "2026-10-08T12:01:00Z",
                   "referenced_workflows": [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                       "sha": POLICY}]}
            comparison = {"status": "ahead", "ahead_by": 1, "behind_by": 0,
                          "total_commits": 1, "base_commit": {"sha": wrapper_sha},
                          "merge_base_commit": {"sha": wrapper_sha}}
            pinned_comparison = {"status": "ahead", "ahead_by": 1, "behind_by": 0,
                                 "total_commits": 1, "base_commit": {"sha": POLICY},
                                 "merge_base_commit": {"sha": POLICY}}
            class Api:
                def __init__(self, compare, pinned_compare=pinned_comparison):
                    self.compare = compare
                    self.pinned_compare = pinned_compare
                    self.paths = []

                def get(self, path, **_):
                    self.paths.append(path)
                    if path == "/pulls/3691": return pr, ""
                    if path == f"/actions/runs/{RUN_ID}": return run, ""
                    if path == "/git/ref/heads/main": return main_ref(), ""
                    if path == f"/compare/{wrapper_sha}...{MAIN}": return self.compare, ""
                    if path == f"/compare/{POLICY}...{wrapper_sha}": return self.pinned_compare, ""
                    raise AssertionError(f"unexpected API path: {path}")

                def pages(self, path, field=None):
                    return []

            env = {"GITHUB_RUN_ATTEMPT": ATTEMPT, "WORKFLOW_SHA": wrapper_sha, "RUNNER_TEMP": temp,
                   "SCAN_EVIDENCE_DIR": temp}
            sel = selection({"semgrep_canary": True})
            with patch.dict("os.environ", env), patch.object(MODULE, "_diff_lines", return_value={}), \
                    patch.object(MODULE, "_base_parent", return_value=BASE), \
                    patch.object(MODULE.subprocess, "run", return_value=SimpleNamespace(stdout=wrapper_sha)):
                api = Api(comparison)
                current, evidence = MODULE._collect(api, sel, {"build": "success", "semgrep": "success"},
                                                    3691, int(RUN_ID))
                self.assertTrue(evidence["provenance_verified"])
                self.assertIn("/git/ref/heads/main", api.paths)
                self.assertIn(f"/compare/{wrapper_sha}...{MAIN}", api.paths)
                self.assertNotIn(f"/compare/{wrapper_sha}...{BASE}", api.paths)
                self.assertIn(f"/compare/{POLICY}...{wrapper_sha}", api.paths)
                self.assertEqual([], MODULE.verify_verdict(sel,
                    {"build": "success", "semgrep": "success"}, evidence, current))
                divergent = dict(comparison, status="diverged", behind_by=1)
                api = Api(divergent)
                _, bad_evidence = MODULE._collect(api, sel, {"build": "success", "semgrep": "success"},
                                                   3691, int(RUN_ID))
                self.assertFalse(bad_evidence["provenance_verified"])
                with patch.dict("os.environ", {"WORKFLOW_SHA": POLICY}):
                    with self.assertRaisesRegex(MODULE.Unknown, "protected source"):
                        MODULE._collect(Api(comparison), sel, {"build": "success", "semgrep": "success"},
                                        3691, int(RUN_ID))
                reasons = MODULE.verify_verdict(sel, {"build": "success", "semgrep": "success"},
                                                bad_evidence, current)
                self.assertTrue(any("provenance" in reason.lower() for reason in reasons))
                api = Api(comparison, dict(pinned_comparison, status="diverged", behind_by=1))
                _, bad_evidence = MODULE._collect(api, sel, {"build": "success", "semgrep": "success"},
                                                   3691, int(RUN_ID))
                self.assertFalse(bad_evidence["provenance_verified"])


    TOP_INSERT = {"shared.go": "t1\nt2\nt3\no1\no2\no3\no4\no5\no6\n"}

    def _collected_diff_evidence(self, main_files, selection_overrides=None, head_first=False,
                                 raise_unknown=False):
        real_run = subprocess.run
        with tempfile.TemporaryDirectory(prefix="scan-merge-diff-") as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            main_tip, head, merge, origin = build_merge_fixture(root, main_files, head_first)
            parents = f"{merge} {head} {main_tip}" if head_first else f"{merge} {main_tip} {head}"
            self.assertEqual(parents, run_git(root, "rev-list", "--parents", "-n", "1", merge))
            wrapper_sha = "e" * 40
            pr = {"number": 3691, "head": {"sha": head, "repo": {"full_name": "contributor/repo"}},
                  "base": {"sha": main_tip, "ref": "main",
                           "repo": {"full_name": "Concord-Voice/Self-Hosted"}},
                  "merge_commit_sha": merge}
            run = {"id": int(RUN_ID), "run_attempt": int(ATTEMPT), "head_sha": head,
                   "path": ".github/workflows/required-pr-build.yml@refs/heads/main",
                   "event": "pull_request", "repository": {"full_name": "Concord-Voice/Self-Hosted"},
                   "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/77",
                   "workflow_id": 77, "run_started_at": "2026-10-08T12:00:00Z",
                   "updated_at": "2026-10-08T12:01:00Z",
                   "referenced_workflows": [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                       "sha": POLICY}]}
            ahead = {"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1,
                     "base_commit": {"sha": wrapper_sha}, "merge_base_commit": {"sha": wrapper_sha}}
            pinned = {"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1,
                      "base_commit": {"sha": POLICY}, "merge_base_commit": {"sha": POLICY}}

            class Api:
                def get(self, path, **_):
                    if path == "/pulls/3691": return pr, ""
                    if path == f"/actions/runs/{RUN_ID}": return run, ""
                    # Provenance asks for main's live tip, which here is the commit the merge was built on.
                    if path == "/git/ref/heads/main": return main_ref(main_tip), ""
                    if path == f"/compare/{wrapper_sha}...{main_tip}": return ahead, ""
                    if path == f"/compare/{POLICY}...{wrapper_sha}": return pinned, ""
                    raise AssertionError(f"unexpected API path: {path}")

                def pages(self, path, field=None):
                    return []

            sel = selection({"semgrep_canary": True})
            sel.update(base_sha=main_tip, head_sha=head, merge_sha=merge)
            if selection_overrides is not None:
                sel.update(selection_overrides(SimpleNamespace(base=main_tip, head=head,
                                                               merge=merge, origin=origin)))
            pr["base"]["sha"] = sel["base_sha"]  # the PR API reports the base the selection recorded

            def fake_run(command, *args, **kwargs):
                # Only the workflow's own `rev-parse HEAD` is faked; every other git call is real.
                if "rev-parse" in command and "HEAD" in command:
                    return SimpleNamespace(stdout=wrapper_sha)
                return real_run(command, *args, **kwargs)
            env = {"GITHUB_RUN_ATTEMPT": ATTEMPT, "WORKFLOW_SHA": wrapper_sha,
                   "RUNNER_TEMP": temp, "SCAN_EVIDENCE_DIR": temp}
            with patch.dict("os.environ", env), patch.object(MODULE, "REPO_ROOT", root), \
                    patch.object(MODULE.subprocess, "run", fake_run):
                try:
                    _, evidence = MODULE._collect(Api(), sel, {"build": "success", "semgrep": "success"},
                                                  3691, int(RUN_ID))
                except MODULE.Unknown as exc:
                    if raise_unknown:
                        raise
                    self.fail("added-lines evidence for a PR whose head is behind its base must be "
                              f"built without raising: {exc}")
            return evidence

    def test_collect_ignores_files_added_on_main_after_the_fork_point(self):
        evidence = self._collected_diff_evidence({"main-only.md": "m1\nm2\n"})
        self.assertIs(True, evidence["diff"]["complete"])
        self.assertNotIn("main-only.md", evidence["diff"]["added_lines"])
        self.assertEqual({"shared.go": [7]}, evidence["diff"]["added_lines"])

    def test_collect_numbers_added_lines_as_in_the_scanned_merge_commit(self):
        evidence = self._collected_diff_evidence(self.TOP_INSERT)
        self.assertIs(True, evidence["diff"]["complete"])
        self.assertEqual({"shared.go": [10]}, evidence["diff"]["added_lines"])

    def test_collect_ignores_a_stale_selection_base(self):
        evidence = self._collected_diff_evidence(
            self.TOP_INSERT, lambda shas: {"base_sha": shas.origin})
        self.assertEqual({"shared.go": [10]}, evidence["diff"]["added_lines"],
                         "the diff base must be the merge commit's other parent, not selection base_sha")

    def test_collect_admits_a_pr_behind_main_and_still_numbers_lines_in_the_merge_commit(self):
        # The provenance anchor does not require the PR to be current with main, so a PR whose recorded
        # base trails the commit GitHub merged it into reaches the diff step. That step must number
        # lines in the scanned merge commit. Numbered from the recorded base, main's edit above the
        # PR's hunk moves a new finding out of the "added lines" and the verdict lets it through.
        evidence = self._collected_diff_evidence(
            self.TOP_INSERT, lambda shas: {"base_sha": shas.origin})
        self.assertIs(True, evidence["provenance_verified"])
        self.assertEqual({"shared.go": [10]}, evidence["diff"]["added_lines"])

    def test_collect_refuses_a_merge_commit_that_does_not_join_the_head(self):
        with self.assertRaisesRegex(MODULE.Unknown, "does not join the PR head",
                                    msg="a merge commit whose parents exclude the PR head must be refused"):
            self._collected_diff_evidence(self.TOP_INSERT, lambda shas: {"head_sha": shas.origin},
                                          raise_unknown=True)

    def test_collect_refuses_a_single_parent_merge_revision(self):
        with self.assertRaisesRegex(MODULE.Unknown, "does not join the PR head",
                                    msg="a one-parent merge revision must be refused"):
            self._collected_diff_evidence(self.TOP_INSERT, lambda shas: {"merge_sha": shas.head},
                                          raise_unknown=True)

    def test_collect_names_an_unavailable_merge_revision(self):
        with self.assertRaisesRegex(MODULE.Unknown, "merge revision unavailable",
                                    msg="a merge revision absent from the repository must be named"):
            self._collected_diff_evidence(self.TOP_INSERT, lambda shas: {"merge_sha": "f" * 40},
                                          raise_unknown=True)

    def test_collect_accepts_either_parent_order(self):
        evidence = self._collected_diff_evidence(self.TOP_INSERT, head_first=True)
        self.assertEqual({"shared.go": [10]}, evidence["diff"]["added_lines"])


class StandaloneScanCollectionTests(unittest.TestCase):
    OSV_CATEGORY = ".github/workflows/osv-scanner.yml:osv-scan"

    def collect(self, *, selected_key, osv_runs=None, sonar_checks=None,
                osv_api_category=None, osv_analysis_key=None, osv_sarif_category=None,
                osv_newer_analysis=False, artifact_created="2026-10-08T12:00:20Z",
                analysis_created="2026-10-08T12:00:40Z",
                run_status="in_progress", run_updated="2026-10-08T12:02:00Z",
                osv_pin=POLICY, osv_reference_sha=None, same_repo=False,
                child=None, proof=True, child_run_id="777"):
        head_repo = "Concord-Voice/Self-Hosted" if same_repo else "contributor/repo"
        pr = {"number": 3691, "created_at": "2026-10-08T11:30:00Z",
              "head": {"sha": HEAD, "repo": {"full_name": head_repo}},
              "base": {"sha": BASE, "ref": "main",
                       "repo": {"full_name": "Concord-Voice/Self-Hosted"}},
              "merge_commit_sha": MERGE}
        run = {"id": int(RUN_ID), "run_attempt": int(ATTEMPT), "head_sha": HEAD,
               "path": ".github/workflows/required-pr-build.yml@refs/heads/main",
               "event": "pull_request", "repository": {"full_name": "Concord-Voice/Self-Hosted"},
               "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/77",
               "workflow_id": 77, "run_started_at": "2026-10-08T12:00:00Z",
               "status": run_status, "updated_at": run_updated,
               "referenced_workflows": [{"path": "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@" + POLICY,
                   "sha": POLICY},
                   {"path": "Concord-Voice/Self-Hosted/.github/workflows/osv-scanner.yml@" + (osv_reference_sha or osv_pin),
                    "sha": osv_reference_sha or osv_pin}]}
        comparison = {"status": "ahead", "ahead_by": 1, "behind_by": 0,
                      "total_commits": 1, "base_commit": {"sha": POLICY},
                      "merge_base_commit": {"sha": POLICY}}
        osv_workflow = {"id": 88}
        analysis_item = {"id": 19, "tool": {"name": "osv-scanner"},
                         "category": osv_api_category or self.OSV_CATEGORY,
                         "analysis_key": osv_analysis_key or ".github/workflows/osv-scanner.yml:osv-scan",
                         "commit_sha": MERGE, "created_at": analysis_created,
                         "environment": "github", "error": None, "warning": None,
                         "ref": PR_REF, "sarif_id": "osv-sarif-19", "results_count": 0,
                         "rules_count": 0}
        expected_osv_category = self.OSV_CATEGORY
        self_test = self

        class Api:
            def __init__(self):
                self.paths = []

            def get(self, path, **_):
                self.paths.append(path)
                if path == "/pulls/3691": return pr, ""
                if path == f"/actions/runs/{RUN_ID}": return run, ""
                if path == "/git/ref/heads/main": return main_ref(), ""
                if path == f"/compare/{POLICY}...{MAIN}": return comparison, ""
                if path == "/actions/workflows/osv-scanner.yml": return osv_workflow, ""
                if path == "/actions/runs/777": return dict(self_test.child_run(), **(child or {})), ""
                if path == "/code-scanning/sarifs/osv-sarif-19": return {"processing_status": "complete"}, ""
                if path == "/code-scanning/analyses/19":
                    return {"runs": [{"tool": {"driver": {"name": "osv-scanner", "rules": []}},
                                      "automationDetails": {"id": osv_sarif_category or expected_osv_category},
                                      "results": []}]}, ""
                raise AssertionError(f"unexpected API path: {path}")

            def pages(self, path, field=None):
                self.paths.append(path)
                if "/artifacts" in path:
                    return [{"name": f"protected-sonar-caller-proof-{RUN_ID}-{ATTEMPT}",
                             "expired": False, "size_in_bytes": 2466}] if proof else []
                if "/code-scanning/analyses?" in path:
                    if "sarif_id=" not in path and selected_key == "osv" and osv_api_category:
                        items = [analysis_item, dict(analysis_item, id=18,
                            category=expected_osv_category, sarif_id="older-standalone-id")]
                        if osv_newer_analysis:
                            items.append(dict(analysis_item, id=20, sarif_id="newer-osv-id"))
                        return items
                    return [analysis_item]
                if "/code-scanning/alerts?" in path:
                    return []
                if "/actions/workflows/88/runs?" in path:
                    return list(osv_runs or [])
                if "/check-runs?" in path:
                    return list(sonar_checks or [])
                raise AssertionError(f"unexpected paginated API path: {path}")

        with tempfile.TemporaryDirectory(prefix="scan-collection-") as temp:
            selected = {"osv": True} if selected_key == "osv" else {"sonar": True}
            selection_data = selection(selected)
            report_name = f"scan-evidence-{RUN_ID}-{ATTEMPT}-osv"
            reports = {report_name: {"report": {"run_id": RUN_ID, "run_attempt": ATTEMPT,
                "category": self.OSV_CATEGORY, "sarif_id": "osv-sarif-19"},
                "artifact": {"id": 501, "name": report_name, "expired": False,
                    "size_in_bytes": 128, "workflow_run": {"id": int(RUN_ID)},
                    "created_at": artifact_created}}} if selected_key == "osv" else {}
            env = {"GITHUB_RUN_ATTEMPT": ATTEMPT, "WORKFLOW_SHA": POLICY, "RUNNER_TEMP": temp,
                   "SCAN_EVIDENCE_DIR": temp}
            api = Api()
            class FrozenDateTime(datetime):
                @classmethod
                def now(cls, tz=None):
                    current = cls.fromisoformat("2026-10-08T12:03:00+00:00")
                    return current if tz is None else current.astimezone(tz)
            with patch.dict("os.environ", env), patch.object(MODULE, "_diff_lines", return_value={}), \
                    patch.object(MODULE, "_base_parent", return_value=BASE), \
                    patch.object(MODULE.subprocess, "run", return_value=SimpleNamespace(stdout=POLICY)), \
                    patch.object(MODULE, "datetime", FrozenDateTime), \
                    patch.object(MODULE, "_protected_osv_pin", return_value=osv_pin), \
                    patch.object(MODULE, "_local_reports", return_value=reports):
                current_pr, evidence = MODULE._collect(api, selection_data,
                    {"build": "success", selected_key: "success", "sonar_child_run_id": child_run_id},
                    3691, int(RUN_ID))
            return selection_data, current_pr, evidence, api.paths

    def test_independent_osv_pin_must_match_the_protected_wrapper(self):
        promoted = "e" * 40
        _, _, accepted, _ = self.collect(selected_key="osv", osv_pin=promoted)
        self.assertTrue(accepted["osv_source_verified"])
        _, _, rejected, _ = self.collect(selected_key="osv", osv_pin=promoted,
                                          osv_reference_sha=POLICY)
        self.assertFalse(rejected["osv_source_verified"])

    def test_protected_osv_pin_reads_exactly_the_wrapper_call(self):
        promoted = "e" * 40
        with tempfile.TemporaryDirectory() as temporary:
            workflow = Path(temporary) / ".github/workflows/required-pr-build.yml"
            workflow.parent.mkdir(parents=True)
            workflow.write_text("jobs:\n  osv:\n    uses: Concord-Voice/Self-Hosted/.github/workflows/osv-scanner.yml@" + promoted + "\n")
            with patch.object(MODULE, "REPO_ROOT", Path(temporary)):
                self.assertEqual(promoted, MODULE._protected_osv_pin())
                workflow.write_text(workflow.read_text().replace("osv-scanner.yml", "pr-ci.yml"))
                with self.assertRaises(MODULE.Unknown):
                    MODULE._protected_osv_pin()

    def test_required_workflow_osv_category_normalization_matches_live_api(self):
        key = ".github/workflows/required-pr-build.yml:protected-pr-publish"
        selected, pr, evidence, _ = self.collect(selected_key="osv",
            osv_api_category=".github/workflows", osv_analysis_key=key)
        self.assertEqual([], MODULE.verify_verdict(selected,
            {"build": "success", "osv": "success"}, evidence, pr))
        with self.assertRaises(MODULE.Unknown):
            self.collect(selected_key="osv", osv_api_category=".github/workflows",
                osv_analysis_key=key, osv_sarif_category=".github/workflows/other.yml:osv-scan")

    def test_required_workflow_osv_rejects_newer_upload_in_parent_category(self):
        selected, pr, evidence, _ = self.collect(selected_key="osv",
            osv_api_category=".github/workflows",
            osv_analysis_key=".github/workflows/required-pr-build.yml:protected-pr-publish",
            osv_newer_analysis=True)
        self.assertEqual(["unknown: osv-scanner latest upload mismatch"],
            MODULE.verify_verdict(selected, {"build": "success", "osv": "success"}, evidence, pr))

    def test_retry_rejects_preseeded_receipt_and_prior_attempt_analysis(self):
        with self.assertRaisesRegex(MODULE.Unknown, "current-attempt artifact not after"):
            self.collect(selected_key="osv", artifact_created="2026-10-08T11:59:59Z")
        with self.assertRaisesRegex(MODULE.Unknown, "current-attempt artifact not after"):
            self.collect(selected_key="osv", artifact_created="2026-10-08T12:00:00Z")
        with self.assertRaisesRegex(MODULE.Unknown, "selected analysis not after"):
            self.collect(selected_key="osv", analysis_created="2026-10-08T11:59:59Z")
        with self.assertRaisesRegex(MODULE.Unknown, "selected analysis not after"):
            self.collect(selected_key="osv", analysis_created="2026-10-08T12:00:00Z")

    def sonar_check(self, *, check_id=551, app_id=12526, head=HEAD, base=BASE,
                    conclusion="success", status="completed",
                    started="2026-10-08T12:00:20Z", completed="2026-10-08T12:01:20Z",
                    name="SonarCloud Code Analysis"):
        return {"id": check_id, "name": name, "app": {"id": app_id}, "head_sha": head,
                "conclusion": conclusion, "status": status,
                "started_at": started, "completed_at": completed,
                "pull_requests": [{"number": 3691, "head": {"sha": head},
                                   "base": {"sha": base, "ref": "main"}}]}

    def test_alert_collection_requests_all_states_without_invalid_filter(self):
        _, _, _, paths = self.collect(selected_key="sonar")
        alerts = [path for path in paths if path.startswith("/code-scanning/alerts?")]
        self.assertEqual(2, len(alerts))
        self.assertTrue(all("state=" not in path for path in alerts))

    def sonar_reasons(self, checks):
        selected, pr, evidence, _ = self.collect(selected_key="sonar", sonar_checks=checks)
        return MODULE.verify_verdict(selected, {"build": "success", "sonar": "success"}, evidence, pr), evidence

    def test_stale_then_fresh_sonar_check_on_unchanged_head_passes_collection(self):
        stale = self.sonar_check(started="2026-10-08T11:00:00Z", completed="2026-10-08T11:01:00Z")
        fresh = self.sonar_check()
        selected, pr, evidence, _ = self.collect(selected_key="sonar", sonar_checks=[stale, fresh],
            run_status="in_progress", run_updated="2026-10-08T12:00:30Z")
        reasons = MODULE.verify_verdict(selected, {"build": "success", "sonar": "success"}, evidence, pr)
        self.assertEqual([], reasons)

    def test_sonar_check_before_protected_run_but_on_current_pr_revision_passes(self):
        check = self.sonar_check(started="2026-10-08T11:45:00Z",
                                 completed="2026-10-08T11:46:00Z")
        reasons, evidence = self.sonar_reasons([check])
        self.assertEqual([], reasons)
        self.assertTrue(evidence["sonar"]["analysis_window_verified"])

    def test_retry_uses_unique_newest_sonar_check_on_same_pr_revision(self):
        older = self.sonar_check(conclusion="failure")
        newer = self.sonar_check(check_id=552, started="2026-10-08T12:01:30Z",
                                 completed="2026-10-08T12:02:30Z")
        for checks in ([newer, older], [older, newer]):
            reasons, _ = self.sonar_reasons(checks)
            self.assertEqual([], reasons)
        newer_failed = dict(newer, conclusion="failure")
        older_passed = dict(older, conclusion="success")
        for checks in ([newer_failed, older_passed], [older_passed, newer_failed]):
            reasons, _ = self.sonar_reasons(checks)
            self.assertTrue(any("Sonar" in reason for reason in reasons), reasons)

    @staticmethod
    def child_run():
        return {"id": 777, "run_attempt": 1, "path": ".github/workflows/sonarcloud.yml",
                "event": "workflow_dispatch", "head_branch": "main",
                "repository": {"full_name": "Concord-Voice/Self-Hosted"},
                "status": "completed", "conclusion": "success",
                "run_started_at": "2026-10-08T12:00:10Z", "updated_at": "2026-10-08T12:02:00Z"}

    def bridge_reasons(self, checks, **kwargs):
        selected, pr, evidence, _ = self.collect(selected_key="sonar", sonar_checks=checks,
                                                 same_repo=True, **kwargs)
        return MODULE.verify_verdict(selected, {"build": "success", "sonar": "success"}, evidence, pr), evidence

    def test_same_repo_app_check_inside_exact_child_window_passes(self):
        reasons, evidence = self.bridge_reasons([self.sonar_check()])
        self.assertEqual([], reasons)
        self.assertEqual({"proof_verified": True, "child_verified": True}, evidence["sonar_bridge"])

    def test_same_repo_app_check_outside_child_window_blocks(self):
        # Fine for a fork PR, but a same-repository PR's only producer is the child.
        for started, completed in (("2026-10-08T11:45:00Z", "2026-10-08T11:46:00Z"),
                                   ("2026-10-08T12:01:30Z", "2026-10-08T12:02:30Z")):
            with self.subTest(started=started):
                reasons, _ = self.bridge_reasons([self.sonar_check(started=started, completed=completed)])
                self.assertTrue(any("Sonar App check" in reason for reason in reasons), reasons)

    def test_two_app_checks_in_one_child_window_block(self):
        # One producer per window: a second in-window check is ambiguous even
        # when the newer one passed (red-team F1 on #4214).
        failed = self.sonar_check(conclusion="failure", started="2026-10-08T12:00:30Z")
        foreign = self.sonar_check(check_id=552, started="2026-10-08T12:01:00Z",
                                   completed="2026-10-08T12:01:40Z")
        for checks in ([failed, foreign], [foreign, failed], [self.sonar_check(), foreign]):
            with self.subTest(checks=checks):
                reasons, _ = self.bridge_reasons(checks)
                self.assertTrue(any("Sonar App check" in reason for reason in reasons), reasons)

    def test_wrong_or_failed_child_and_missing_proof_block(self):
        for child in ({"conclusion": "failure"}, {"status": "in_progress"}, {"run_attempt": 2},
                      {"path": ".github/workflows/other.yml"}, {"event": "push"},
                      {"head_branch": "feature"}, {"repository": {"full_name": "contributor/repo"}},
                      {"id": 778}, {"run_started_at": "2026-10-08T11:59:59Z"}):
            with self.subTest(child=child):
                reasons, _ = self.bridge_reasons([self.sonar_check()], child=child)
                self.assertTrue(any("exact child run" in reason for reason in reasons), reasons)
        reasons, _ = self.bridge_reasons([self.sonar_check()], proof=False)
        self.assertTrue(any("Sonar caller proof" in reason for reason in reasons), reasons)

    def test_missing_child_run_id_is_unknown(self):
        for child_run_id in ("", None, "0", "-1", "77x", "\u0667\u0667\u0667", "0777"):
            with self.subTest(child_run_id=child_run_id), self.assertRaisesRegex(MODULE.Unknown, "child run ID"):
                self.bridge_reasons([self.sonar_check()], child_run_id=child_run_id)

    def test_stale_only_two_current_wrong_app_head_and_failed_sonar_checks_block(self):
        stale = self.sonar_check(started="2026-10-08T11:00:00Z", completed="2026-10-08T11:01:00Z")
        other_pr = self.sonar_check()
        other_pr["pull_requests"][0]["number"] = 999
        candidates = [[stale], [self.sonar_check(), self.sonar_check(check_id=552)],
                      [self.sonar_check(app_id=1)], [self.sonar_check(head="e" * 40)],
                      [self.sonar_check(base="e" * 40)], [other_pr],
                      [self.sonar_check(conclusion="failure")],
                      [self.sonar_check(status="in_progress")]]
        for checks in candidates:
            with self.subTest(checks=checks):
                reasons, _ = self.sonar_reasons(checks)
                self.assertTrue(any("Sonar" in reason for reason in reasons), reasons)


# Self-Hosted: CutoverCanaryContractTests and NativeCodeQLCheckTests read Alpha's
# ruleset applier under infrastructure/github, which stays with Alpha.


if __name__ == "__main__":
    unittest.main()
