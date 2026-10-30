"""Guard GitHub's static reusable-workflow graph from CodeQL action calls.

GitHub validates CodeQL action usage in the required workflow's recursive
local-call graph even when the containing job has ``if: false``.
"""

from pathlib import Path
import json
import os
import re
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml


ROOT = Path(__file__).resolve().parents[3]
REQUIRED_WORKFLOW = ".github/workflows/required-pr-build.yml"
FORBIDDEN_ACTIONS = (
    "github/codeql-action/init",
    "github/codeql-action/analyze",
)


def bootstrap_workflow_source(root: Path, ref: str, relative_path: str) -> str | None:
    """Read a proposed callee while protected main still predates this CI port."""
    git = lambda *args: subprocess.run(
        ["git", "-C", str(root), *args], capture_output=True, text=True,
        env={k: v for k, v in os.environ.items() if not k.startswith("GIT_")},
    )
    if (git("cat-file", "-e", f"origin/main:{REQUIRED_WORKFLOW}").returncode == 0
            or git("merge-base", "--is-ancestor", ref, "origin/main").returncode != 0
            or git("cat-file", "-e", f"{ref}:{relative_path}").returncode == 0):
        return None
    proposed = root / relative_path
    return proposed.read_text() if proposed.is_file() else None


def reachable_actions(root: Path, workflow_path: str) -> list[tuple[str, str, tuple[str, ...]]]:
    """Return forbidden action refs and their local-workflow call paths."""
    found = []
    visiting = set()

    def visit(relative_path: str, call_path: tuple[str, ...], ref: str | None = None) -> None:
        if (ref, relative_path) in visiting:
            return
        visiting.add((ref, relative_path))
        if ref:
            pinned = subprocess.run(
                ["git", "-C", str(root), "show", f"{ref}:{relative_path}"],
                capture_output=True, text=True,
                env={k: v for k, v in os.environ.items() if not k.startswith("GIT_")},
            )
            source = pinned.stdout if pinned.returncode == 0 else bootstrap_workflow_source(root, ref, relative_path)
            if source is None:
                pinned.check_returncode()
        else:
            source = (root / relative_path).read_text()
        document = yaml.load(source, Loader=yaml.BaseLoader)
        for job_id, job in document.get("jobs", {}).items():
            job_path = call_path + (f"{relative_path}:jobs.{job_id}",)
            callee = job.get("uses", "")
            if callee.startswith("./.github/workflows/"):
                visit(callee[2:], job_path, ref)
            elif callee.startswith("Concord-Voice/Self-Hosted/.github/workflows/"):
                path, _, pinned_ref = callee.removeprefix("Concord-Voice/Self-Hosted/").rpartition("@")
                if re.fullmatch(r"[0-9a-f]{40}", pinned_ref):
                    visit(path, job_path, pinned_ref)
            for step_index, step in enumerate(job.get("steps", [])):
                action = step.get("uses", "")
                if any(action.startswith(forbidden + "@") for forbidden in FORBIDDEN_ACTIONS):
                    found.append((action, f"{relative_path}:jobs.{job_id}.steps[{step_index}]", job_path))

    visit(workflow_path, ())
    return found


def write_workflow(root: Path, name: str, body: str) -> None:
    path = root / ".github/workflows" / f"{name}.yml"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)


def compose_selection(metadata: dict, mode: str, filters: dict | None = None) -> dict:
    module = (ROOT / ".github/scripts/ci-scan-relevance.mjs").as_posix()
    script = f"""import {{ composeSelection, ANALYSIS_KEYS, COMPONENT_KEYS,
  ADVISORY_KEYS, WORKER_KEYS, NPM_LICENSE_KEYS }} from {json.dumps(Path(module).as_uri())};
const keys = [...ANALYSIS_KEYS, ...COMPONENT_KEYS,
  ...ADVISORY_KEYS.map(key => key === 'selfhost' ? 'selfhost_guard' : key),
  ...WORKER_KEYS, ...NPM_LICENSE_KEYS, 'license_go', 'all_changed'];
let filters = JSON.parse(process.argv[1]);
if (filters === null) filters = Object.fromEntries(keys.map(key => [key, 'true']));
console.log(JSON.stringify(composeSelection(filters, JSON.parse(process.argv[2]),
  {{mode: process.argv[3], policySha: 'd'.repeat(40)}})));
"""
    values = filters
    result = subprocess.run(
        ["node", "--input-type=module", "-e", script, json.dumps(values),
         json.dumps(metadata), mode],
        cwd=ROOT, capture_output=True, text=True, check=True,
    )
    return json.loads(result.stdout)


def run_codeql_matrix_step(select_job: dict, **env: str) -> dict:
    """Run codeql-pr.yml's matrix step with the given filter env; return its matrix."""
    step = next(step for step in select_job["steps"] if step.get("id") == "matrix")
    with tempfile.TemporaryDirectory(prefix="codeql-matrix-") as directory:
        output = Path(directory) / "output"
        summary = Path(directory) / "summary"
        subprocess.run(["bash", "-c", step["run"]], check=True, capture_output=True, text=True,
                       env={"PATH": os.environ["PATH"], "GITHUB_OUTPUT": str(output),
                            "GITHUB_STEP_SUMMARY": str(summary), **env})
        line = next(l for l in output.read_text().splitlines() if l.startswith("matrix="))
        return json.loads(line.removeprefix("matrix="))


class RequiredWorkflowCodeQLGraphTests(unittest.TestCase):
    def test_required_workflow_cannot_reach_codeql_actions(self):
        reachable = reachable_actions(ROOT, REQUIRED_WORKFLOW)
        by_action = {action.split("@", 1)[0]: (location, call_path)
                     for action, location, call_path in reachable}
        for forbidden in FORBIDDEN_ACTIONS:
            with self.subTest(action=forbidden):
                self.assertNotIn(
                    forbidden,
                    by_action,
                    f"forbidden action {forbidden} is reachable from {REQUIRED_WORKFLOW}: "
                    f"{by_action.get(forbidden)}",
                )

    def test_graph_walk_includes_nested_actions_in_skipped_jobs(self):
        with tempfile.TemporaryDirectory(prefix="required-codeql-graph-") as directory:
            fixture = Path(directory)
            write_workflow(fixture, "root", """name: root
on: {pull_request: {}}
jobs:
  call:
    uses: ./.github/workflows/middle.yml
""")
            write_workflow(fixture, "middle", """name: middle
on: {workflow_call: {}}
jobs:
  skipped:
    if: false
    uses: ./.github/workflows/leaf.yml
""")
            write_workflow(fixture, "leaf", """name: leaf
on: {workflow_call: {}}
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: github/codeql-action/init@v4
      - uses: github/codeql-action/analyze@v4
""")

            reachable = reachable_actions(fixture, ".github/workflows/root.yml")
            actions = {action.split("@", 1)[0] for action, _, _ in reachable}
            self.assertEqual(set(FORBIDDEN_ACTIONS), actions)
            self.assertTrue(all("jobs.skipped" in " ".join(path)
                                for _, _, path in reachable))

    def test_graph_walk_includes_sha_pinned_calls(self):
        with tempfile.TemporaryDirectory(prefix="required-pinned-graph-") as directory:
            fixture = Path(directory)
            write_workflow(fixture, "leaf", """name: leaf
on: {workflow_call: {}}
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: github/codeql-action/init@v4
""")
            git_env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
            subprocess.run(["git", "-C", str(fixture), "init", "-q"], check=True, env=git_env)
            subprocess.run(["git", "-C", str(fixture), "add", ".github/workflows/leaf.yml"],
                           check=True, env=git_env)
            tree = subprocess.run(["git", "-C", str(fixture), "write-tree"],
                                  check=True, capture_output=True, text=True,
                                  env=git_env).stdout.strip()
            pinned = subprocess.run(["git", "-C", str(fixture), "-c", "user.name=Test",
                                     "-c", "user.email=test@example.invalid",
                                     "commit-tree", tree, "-m", "pinned fixture"],
                                    check=True, capture_output=True, text=True,
                                    env=git_env).stdout.strip()
            write_workflow(fixture, "leaf", """name: leaf
on: {workflow_call: {}}
jobs:
  clean:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
""")
            write_workflow(fixture, "root", f"""name: root
on: {{pull_request: {{}}}}
jobs:
  call:
    uses: Concord-Voice/Self-Hosted/.github/workflows/leaf.yml@{pinned}
""")
            with patch.dict(os.environ, {"GIT_DIR": str(fixture / "missing"),
                                      "GIT_INDEX_FILE": str(fixture / "missing-index")}):
                reachable = reachable_actions(fixture, ".github/workflows/root.yml")
            self.assertEqual(1, len(reachable))

    def test_clean_nested_call_graph_is_accepted(self):
        with tempfile.TemporaryDirectory(prefix="required-clean-graph-") as directory:
            fixture = Path(directory)
            write_workflow(fixture, "root", """name: root
on: {pull_request: {}}
jobs:
  call:
    uses: ./.github/workflows/leaf.yml
""")
            write_workflow(fixture, "leaf", """name: leaf
on: {workflow_call: {}}
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
""")

            self.assertEqual([], reachable_actions(fixture, ".github/workflows/root.yml"))

    def test_ordinary_pr_workflow_runs_ioc_before_selected_codeql_on_retarget(self):
        caller_path = ROOT / ".github/workflows/codeql-pr.yml"
        self.assertTrue(caller_path.is_file(),
                        "ordinary CodeQL PR caller .github/workflows/codeql-pr.yml is missing")
        caller = yaml.load(caller_path.read_text(), Loader=yaml.BaseLoader)
        triggers = caller.get("on", {}).get("pull_request", {})
        self.assertEqual(["opened", "synchronize", "reopened", "edited"], triggers.get("types"))
        self.assertEqual(["main"], triggers.get("branches"))
        jobs = caller.get("jobs", {})
        self.assertEqual("./.github/workflows/ioc-scanning.yml", jobs.get("ioc", {}).get("uses"))
        self.assertEqual("./.github/workflows/codeql.yml", jobs.get("codeql", {}).get("uses"))
        self.assertEqual(["ioc", "select"], jobs.get("codeql", {}).get("needs"))
        for job in ("ioc", "select"):
            condition = jobs.get(job, {}).get("if", "")
            with self.subTest(job=job):
                self.assertIn("github.event.action == 'edited'", condition)
                self.assertIn("github.event.changes.base == null", condition,
                              "base retarget edits must not be treated as metadata-only")
                self.assertIn("github.event.changes.title != null", condition)
                self.assertIn("github.event.changes.body != null", condition)
                self.assertIn("!", condition,
                              "only title/body-only edits may skip the IOC dependency")
        self.assertNotIn("if", jobs["codeql"], "native CodeQL cannot be skipped as a whole")
        self.assertEqual("${{ needs.select.outputs.matrix }}",
                         jobs["codeql"].get("with", {}).get("codeql-matrix"))
        self.assertNotIn("paths", triggers, "native CodeQL must not use trigger path filters")
        select = jobs["select"]
        checkout = next(step for step in select["steps"] if step.get("id") == "policy")
        self.assertEqual("${{ github.event.pull_request.base.sha }}", checkout["with"]["ref"],
                         "the language filter must come from the base commit, not the PR")
        dorny = next(step for step in select["steps"] if step.get("id") == "filter")
        self.assertEqual("policy/.github/ci/scan-relevance.yml", dorny["with"]["filters"])
        codeql = yaml.load((ROOT / ".github/workflows/codeql.yml").read_text(),
                           Loader=yaml.BaseLoader)
        default = json.loads(codeql["on"]["workflow_call"]["inputs"]["codeql-matrix"]["default"])
        self.assertEqual({"actions", "go", "javascript-typescript"},
                         {leg["language"] for leg in default["include"]})
        full = run_codeql_matrix_step(select, FILTER_OK="false", HOTFIX="false", GO="", JS="")
        self.assertEqual(default, full, "selected legs must match codeql.yml's default legs")

    def test_codeql_language_selection(self):
        select = yaml.load((ROOT / ".github/workflows/codeql-pr.yml").read_text(),
                           Loader=yaml.BaseLoader)["jobs"]["select"]
        cases = [
            ({"FILTER_OK": "true", "HOTFIX": "false", "GO": "false", "JS": "false"}, ["actions"]),
            ({"FILTER_OK": "true", "HOTFIX": "false", "GO": "true", "JS": "false"}, ["actions", "go"]),
            ({"FILTER_OK": "true", "HOTFIX": "false", "GO": "false", "JS": "true"},
             ["actions", "javascript-typescript"]),
            ({"FILTER_OK": "true", "HOTFIX": "true", "GO": "false", "JS": "false"},
             ["actions", "go", "javascript-typescript"]),
            # A failed or skipped filter (empty outputs) fails open to the full matrix.
            ({"FILTER_OK": "false", "HOTFIX": "false", "GO": "", "JS": ""},
             ["actions", "go", "javascript-typescript"]),
            ({"FILTER_OK": "true", "HOTFIX": "false", "GO": "", "JS": ""},
             ["actions", "go", "javascript-typescript"]),
        ]
        for env, languages in cases:
            with self.subTest(**env):
                matrix = run_codeql_matrix_step(select, **env)
                self.assertEqual(languages, [leg["language"] for leg in matrix["include"]])

    def test_valid_pr_selection_never_claims_protected_codeql_ownership(self):
        metadata = {"event_name": "pull_request", "hotfix": False,
                    "head_sha": "a" * 40, "base_sha": "b" * 40,
                    "merge_sha": "c" * 40, "changed_files": 2}
        for mode in ("legacy", "required-full", "required-relevant"):
            with self.subTest(mode=mode):
                selection = compose_selection(metadata, mode)
                self.assertTrue(selection["known"], f"valid PR metadata stays known in {mode}")
                for key in ("codeql_actions", "codeql_go", "codeql_js"):
                    self.assertFalse(selection["selected"][key],
                                     f"protected selector must not own {key} in PR {mode}")

    def test_push_and_schedule_codeql_selection_stays_full(self):
        for event, metadata in (
            ("push", {"event_name": "push", "hotfix": False, "before": "b" * 40,
                      "after": "c" * 40, "history_available": True}),
            ("schedule", {"event_name": "schedule", "hotfix": False, "head_sha": "a" * 40}),
        ):
            with self.subTest(event=event):
                selection = compose_selection(metadata, "required-relevant")
                self.assertTrue(selection["known"], f"valid {event} remains known")
                for key in ("codeql_actions", "codeql_go", "codeql_js"):
                    self.assertTrue(selection["selected"][key],
                                    f"{event} keeps full {key} selection")

    def test_unknown_pr_selection_remains_fail_closed(self):
        metadata = {"event_name": "pull_request", "hotfix": False,
                    "head_sha": "a" * 40, "base_sha": "b" * 40,
                    "merge_sha": "c" * 40, "changed_files": 2}
        selection = compose_selection(metadata, "required-relevant", {"codeql_go": "yes"})
        self.assertFalse(selection["known"], "malformed or incomplete metadata cannot authorize a skip")


if __name__ == "__main__":
    unittest.main()
