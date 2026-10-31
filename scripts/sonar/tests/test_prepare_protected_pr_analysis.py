"""Local Git, API and ZIP fixtures for the protected Sonar data boundary."""

import importlib.util
import io
import json
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile


SCRIPT = Path(__file__).resolve().parents[1] / "prepare-protected-pr-analysis.py"
SPEC = importlib.util.spec_from_file_location("prepare_protected_pr_analysis", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def run(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], stderr=subprocess.DEVNULL).decode().strip()


def zipped(name, content, mode=stat.S_IFREG | 0o644):
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED) as archive:
        entry = zipfile.ZipInfo(name)
        entry.external_attr = mode << 16
        entry.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(entry, content)
    return output.getvalue()


class FakeGitHub(MODULE.GitHub):
    def __init__(self, case):
        super().__init__("local-test-token")
        self.case = case

    def request(self, path, limit=MODULE.MAX_API, binary=False):
        self.case.requests.append(path)
        if path == "/actions/runs/42/attempts/2":
            return self.case.run_data
        if path == "/actions/runs/42/artifacts?per_page=100&page=1":
            return {"total_count": len(self.case.metadata), "artifacts": self.case.metadata}
        if path == "/pulls/7":
            return self.case.pr_data
        if path == "/actions/runs/42/attempts/2/jobs?per_page=100":
            return {"total_count": len(self.case.jobs_data), "jobs": self.case.jobs_data}
        if binary and path.startswith("/actions/artifacts/") and path.endswith("/zip"):
            return self.case.zips[int(path.split("/")[3])]
        raise AssertionError(f"unexpected API request: {path}")


class PrepareTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.upstream = self.root / "upstream"
        subprocess.run(["git", "init", "-q", "-b", "main", str(self.upstream)], check=True)
        run(self.upstream, "config", "user.email", "test@example.invalid")
        run(self.upstream, "config", "user.name", "Fixture")
        (self.upstream / "README.md").write_text("base\n")
        for name in MODULE.SUPPORT | {"sonar-project.properties"}:
            destination = self.upstream / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(MODULE.ROOT / name, destination)
        run(self.upstream, "add", ".")
        run(self.upstream, "commit", "-qm", "base")
        self.base = run(self.upstream, "rev-parse", "HEAD")
        self.trusted = self.root / "trusted"
        subprocess.run(["git", "clone", "-q", str(self.upstream), str(self.trusted)], check=True)
        source = {
            "packages/protocol/src/app.ts": "export const app = 1;\n",
            "client/admin/src/app.ts": "export const app = 1;\n",
            "services/control-plane/internal/x.go": "package internal\n",
            "services/control-plane/coverage.out": "mode: atomic\nservices/control-plane/evil.go:1.1,2.1 1 1\n",
            "services/media-plane/src/app.ts": "export const app = 1;\n",
            "scripts/evil.sh": "exit 1\n",
            "sonar-project.properties": "sonar.projectKey=attacker\n",
            "packages/protocol/tsconfig.json": '{"extends":"/etc/passwd"}\n',
        }
        for name, contents in source.items():
            target = self.upstream / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(contents)
        (self.upstream / "packages/protocol/src/app.ts").chmod(0o755)
        run(self.upstream, "add", ".")
        run(self.upstream, "commit", "-qm", "head")
        self.head = run(self.upstream, "rev-parse", "HEAD")
        run(self.upstream, "update-ref", "refs/pull/7/head", self.head)
        run(self.upstream, "update-ref", "refs/heads/main", self.base)
        self.candidate = {"caller_run_id": 42, "caller_run_attempt": 2, "pr_number": 7,
                          "expected_head_sha": self.head, "expected_base_sha": self.base}
        self.payload = {**self.candidate, "pr_ci_result": "success",
                        "selection": {"version": 1, "known": True, "mode": "required-full",
                                      "head_sha": self.head, "base_sha": self.base,
                                      "merge_sha": self.head, "policy_sha": self.base,
                                      "selected": {key: key == "sonar" for key in MODULE.SELECTION_KEYS},
                                      "coverage_required": {key: True for key in MODULE.COMPONENTS}},
                        "jobs": {key: "success" for key in ("ioc", "build", "semgrep", "eslint", "govulncheck")},
                        "coverage": {key: True for key in MODULE.COMPONENTS}}
        self.verified = {"payload": self.payload,
                         "claims": {"repository": MODULE.REPO, "repository_id": "1158152129",
                                    "event_name": "pull_request", "run_id": "42", "run_attempt": "2",
                                    "workflow_ref": MODULE.REPO + "/.github/workflows/required-pr-build.yml@refs/heads/main",
                                    "workflow_sha": self.base}}
        self.run_data = {"id": 42, "run_attempt": 2, "event": "pull_request", "head_sha": self.head,
                         "status": "in_progress", "conclusion": None,
                         "path": ".github/workflows/required-pr-build.yml",
                         "repository": {"full_name": MODULE.REPO, "id": 1158152129},
                         "pull_requests": [{"number": 7}], "run_started_at": "2026-10-17T00:00:00Z"}
        self.pr_data = {"number": 7, "state": "open", "head": {"sha": self.head, "ref": "feature/a:b"},
                        "base": {"sha": self.base, "ref": "main"}, "merge_commit_sha": self.head}
        names = ("report", "ioc / Supply-chain IOC scan", "semgrep / Semgrep SAST",
                 "eslint / ESLint SARIF", "govulncheck / govulncheck (control-plane)",
                 "build / Desktop / Merge Coverage", "build / Control Plane / Merge Coverage",
                 "build / Media Plane / Test + Coverage", "build / Admin Portal / Test + Coverage")
        self.jobs_data = [{"name": "Protected scan enforcement / " + name, "conclusion": "success",
                           "status": "completed", "run_id": 42, "run_attempt": 2, "head_sha": self.head}
                          for name in names]
        proof = zipped("proof.json", b'{"payload_b64":"fixture","oidc_jwt":"fixture"}')
        reports = {
            "desktop": b"TN:\nSF:src/app.ts\nDA:1,1\nend_of_record\n",
            "control_plane": b"mode: atomic\nservices/control-plane/internal/x.go:1.1,2.1 1 1\n",
            "media_plane": b"TN:\nSF:src/app.ts\nDA:1,1\nend_of_record\n",
            "admin": b"TN:\nSF:src/app.ts\nDA:1,1\nend_of_record\n",
        }
        self.zips = {1: proof}
        self.metadata = [self.artifact_meta(1, "protected-sonar-caller-proof-42-2", proof)]
        for index, (component, spec) in enumerate(MODULE.COMPONENTS.items(), 2):
            archive = zipped(spec[1], reports[component])
            self.zips[index] = archive
            self.metadata.append(self.artifact_meta(index, spec[0], archive))
        self.requests = []
        self.api = FakeGitHub(self)

    @staticmethod
    def artifact_meta(identity, name, archive):
        return {"id": identity, "name": name, "expired": False,
                "workflow_run": {"id": 42}, "size_in_bytes": len(archive),
                "created_at": "2026-10-17T00:01:00Z", "expires_at": "2099-01-01T00:00:00Z"}

    def verify(self, proof, verified):
        self.assertIn(b"oidc_jwt", proof.read_bytes())
        verified.write_text(json.dumps(self.verified))
        return self.verified

    def prepare(self):
        with patch.object(MODULE, "ROOT", self.trusted), patch.object(MODULE, "GIT_REMOTE", str(self.upstream)), patch.object(MODULE, "verify_proof", self.verify):
            MODULE.prepare(self.candidate, self.root / "analysis", self.api)

    def test_valid_proof_prepares_inert_sources_full_history_and_all_reports(self):
        self.prepare()
        analysis = self.root / "analysis"
        self.assertEqual(run(analysis, "rev-parse", "HEAD"), self.head)
        self.assertEqual(run(analysis, "rev-parse", "HEAD^"), self.base)
        self.assertEqual((analysis / "packages/protocol/src/app.ts").read_text(), "export const app = 1;\n")
        self.assertFalse((analysis / "scripts/evil.sh").exists())
        self.assertEqual((analysis / "packages/protocol/src/app.ts").stat().st_mode & 0o111, 0)
        self.assertIn("SF:packages/src/app.ts", (analysis / "coverage/lcov.info").read_text())
        self.assertTrue((analysis / "services/control-plane/coverage.out").is_file())
        props = (analysis / "sonar-project.properties").read_text()
        self.assertIn("sonar.pullrequest.key=7", props)
        self.assertIn("sonar.pullrequest.branch=feature/a\\:b", props)
        self.assertNotIn("attacker", props)
        self.assertNotIn("/etc/passwd", (analysis / "packages/protocol/tsconfig.json").read_text())
        self.assertIn("/pulls/7", self.requests)

    def test_invalid_proof_blocks_pr_fetch(self):
        with patch.object(MODULE, "verify_proof", side_effect=MODULE.Reject("invalid")):
            with self.assertRaises(MODULE.Reject):
                MODULE.prepare(self.candidate, self.root / "analysis", self.api)
        self.assertNotIn("/pulls/7", self.requests)
        self.assertFalse((self.root / "analysis").exists())

    def test_moved_pr_or_non_main_workflow_is_rejected(self):
        self.pr_data["head"]["sha"] = self.base
        with self.assertRaisesRegex(MODULE.Reject, "current PR revision moved"):
            self.prepare()
        self.pr_data["head"]["sha"] = self.head
        self.verified["claims"]["workflow_sha"] = self.head
        with self.assertRaisesRegex(MODULE.Reject, "first-parent history"):
            self.prepare()

    def test_protected_workflow_path_accepts_only_main_qualifiers(self):
        path = ".github/workflows/required-pr-build.yml"
        jobs = {"total_count": len(self.jobs_data), "jobs": self.jobs_data}
        for value in (path, f"{path}@main", f"{path}@refs/heads/main",
                      f"{MODULE.REPO}/{path}@main", f"{MODULE.REPO}/{path}@refs/heads/main"):
            with self.subTest(path=value):
                self.run_data["path"] = value
                MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)
        self.run_data["path"] = f"{path}@feature/untrusted"
        with self.assertRaisesRegex(MODULE.Reject, "caller run identity or state invalid: path$"):
            MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)

    def test_caller_run_rejection_names_every_failed_field(self):
        jobs = {"total_count": len(self.jobs_data), "jobs": self.jobs_data}
        self.run_data.update(status="completed", conclusion="success", pull_requests=[])
        with self.assertRaisesRegex(MODULE.Reject, "invalid: status,conclusion,pull_requests$"):
            MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)
        with self.assertRaisesRegex(MODULE.Reject, "invalid: run$"):
            MODULE.validate_evidence(self.candidate, self.verified, [], self.pr_data, jobs)

    def test_api_requests_the_version_that_returns_merge_commit_sha(self):
        calls = []

        class Response(io.BytesIO):
            def __init__(self, url):
                super().__init__(b'{"merge_commit_sha": "' + b"a" * 40 + b'"}')
                self.url = url

            def __exit__(self, *exc):
                self.close()

        def open_mock(request, timeout):
            calls.append(request)
            return Response(request.full_url)

        with patch.object(MODULE, "OPEN", open_mock):
            pr = MODULE.GitHub("token").request("/pulls/7")
        self.assertEqual(pr["merge_commit_sha"], "a" * 40)
        self.assertEqual(calls[0].get_header("X-github-api-version"), "2022-11-28")

    def test_caller_run_accepts_every_live_status(self):
        # A downstream job waiting on the canary can report the live run as queued.
        jobs = {"total_count": len(self.jobs_data), "jobs": self.jobs_data}
        for status in MODULE.LIVE_RUN_STATUSES:
            with self.subTest(status=status):
                self.run_data["status"] = status
                MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)
        self.run_data["status"] = "unknown"
        with self.assertRaisesRegex(MODULE.Reject, "invalid: status$"):
            MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)

    def test_signed_workflow_ref_accepts_only_ruleset_main_form(self):
        jobs = {"total_count": len(self.jobs_data), "jobs": self.jobs_data}
        ref = f"{MODULE.REPO}/.github/workflows/required-pr-build.yml"
        for value in (f"{ref}@main", f"{ref}@refs/pull/7/merge", f"{ref}@feature/untrusted"):
            with self.subTest(workflow_ref=value):
                self.verified["claims"]["workflow_ref"] = value
                with self.assertRaisesRegex(MODULE.Reject, "verified claims inconsistent"):
                    MODULE.validate_evidence(self.candidate, self.verified, self.run_data, self.pr_data, jobs)

    def test_selected_report_must_be_present_and_well_formed(self):
        self.metadata.pop(4)
        with self.assertRaisesRegex(MODULE.Reject, "artifact missing"):
            self.prepare()
        self.metadata.append(self.artifact_meta(5, "admin-coverage", self.zips[5]))
        self.zips[5] = zipped("lcov.info", b"SF:../../other/secrets.ts\n")
        with self.assertRaisesRegex(MODULE.Reject, "unsafe path"):
            self.prepare()

    def test_lagging_job_inventory_is_polled_until_settled(self):
        settled = [dict(job) for job in self.jobs_data]
        self.jobs_data[3].update(status="in_progress", conclusion=None)
        polls, pause = [], object()

        def settle(delay):
            if delay is pause:
                polls.append(1)
                if len(polls) == 2:
                    self.jobs_data[:] = settled
        with patch.object(MODULE, "JOB_SETTLE_DELAY", pause), patch.object(MODULE.time, "sleep", side_effect=settle):
            self.prepare()
        self.assertEqual(len(polls), 2)
        self.assertEqual(self.requests.count("/actions/runs/42/attempts/2/jobs?per_page=100"), 3)

    def test_job_inventory_that_never_settles_is_rejected_after_bounded_poll(self):
        self.jobs_data[3].update(status="completed", conclusion=None)
        pause = object()
        with patch.object(MODULE, "JOB_SETTLE_DELAY", pause), patch.object(MODULE.time, "sleep") as sleep:
            with self.assertRaisesRegex(MODULE.Reject, "signed job verdict differs"):
                self.prepare()
        self.assertEqual([c.args for c in sleep.call_args_list].count((pause,)), MODULE.JOB_SETTLE_ATTEMPTS - 1)
        self.jobs_data[3]["status"] = "queued"
        with patch.object(MODULE.time, "sleep"):
            with self.assertRaisesRegex(MODULE.Reject, "protected caller jobs incomplete"):
                self.prepare()

    def test_api_verdict_and_duplicate_proof_are_rejected(self):
        self.jobs_data[3]["conclusion"] = "failure"
        with self.assertRaisesRegex(MODULE.Reject, "signed job verdict differs"):
            self.prepare()
        self.jobs_data[3]["conclusion"] = "success"
        self.metadata.append(dict(self.metadata[0]))
        with self.assertRaisesRegex(MODULE.Reject, "artifact missing or duplicate"):
            self.prepare()

    def test_old_attempt_coverage_artifact_does_not_duplicate_current_attempt(self):
        old = dict(self.metadata[1], id=7, created_at="2026-10-16T23:59:00Z")
        self.metadata.append(old)
        self.prepare()
        self.assertNotIn("/actions/artifacts/7/zip", self.requests)

    def test_source_size_bound_rejects_tree(self):
        with patch.object(MODULE, "MAX_SOURCE_TOTAL", 1):
            with self.assertRaisesRegex(MODULE.Reject, "source size or count"):
                self.prepare()

    def test_pr_coverage_path_is_not_materialized_when_report_was_not_uploaded(self):
        self.payload["coverage"]["control_plane"] = False
        self.payload["selection"]["coverage_required"]["control_plane"] = False
        self.prepare()
        self.assertFalse((self.root / "analysis/services/control-plane/coverage.out").exists())

    def test_go_coverage_rejects_parent_traversal(self):
        self.zips[3] = zipped("coverage.out",
                              b"mode: atomic\nservices/control-plane/../../../../tmp/fictional-secret:1.1,2.1 1 1\n")
        with self.assertRaisesRegex(MODULE.Reject, "unsafe path"):
            self.prepare()

    def test_zip_rejects_extra_symlink_and_expansion_bounds(self):
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr("proof.json", "a")
            archive.writestr("extra", "b")
        with self.assertRaises(MODULE.Reject):
            MODULE.one_zip(output.getvalue(), "proof.json", 100)
        with self.assertRaises(MODULE.Reject):
            MODULE.one_zip(zipped("proof.json", "a", stat.S_IFLNK | 0o777), "proof.json", 100)
        with self.assertRaises(MODULE.Reject):
            MODULE.one_zip(zipped("proof.json", "x" * 101), "proof.json", 100)

    def test_git_symlink_and_executable_modes(self):
        target = self.upstream / "packages/protocol/src/link.ts"
        target.symlink_to("app.ts")
        run(self.upstream, "add", ".")
        run(self.upstream, "commit", "-qm", "link")
        self.head = run(self.upstream, "rev-parse", "HEAD")
        run(self.upstream, "update-ref", "refs/pull/7/head", self.head)
        self.candidate["expected_head_sha"] = self.head
        self.payload["expected_head_sha"] = self.head
        self.payload["selection"]["head_sha"] = self.head
        self.payload["selection"]["merge_sha"] = self.head
        self.run_data["head_sha"] = self.head
        self.jobs_data = [{**job, "head_sha": self.head} for job in self.jobs_data]
        self.pr_data["head"]["sha"] = self.head
        self.pr_data["merge_commit_sha"] = self.head
        with self.assertRaisesRegex(MODULE.Reject, "non-regular Git source"):
            self.prepare()


if __name__ == "__main__":
    unittest.main()
