"""Local proof and REST contract tests; no GitHub credentials or network access."""

import base64
from contextlib import redirect_stderr
from io import StringIO
import importlib.util
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit


SOURCE = Path(__file__).resolve().parents[1] / "protected-pr-caller.py"
SPEC = importlib.util.spec_from_file_location("protected_pr_caller", SOURCE)
CALLER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CALLER)
SHA_A = "a" * 40
SHA_B = "b" * 40


class Response:
    def __init__(self, status, value):
        self.status = status
        self.raw = json.dumps(value).encode() if isinstance(value, dict) else value

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass

    def read(self, size):
        return self.raw[:size]


class CallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.ids = {"caller_run_id": 123, "caller_run_attempt": 2, "pr_number": 2949,
                    "expected_head_sha": SHA_A, "expected_base_sha": SHA_B,
                    "pr_ci_result": "success"}
        self.selection = {"version": 1, "known": True, "mode": "required-full",
                          "head_sha": SHA_A, "base_sha": SHA_B, "merge_sha": "c" * 40,
                          "policy_sha": "d" * 40, "changed_files": 1, "hotfix": False,
                          "selected": {key: key == "sonar" for key in CALLER.SELECTED},
                          "advisory": {}, "workers": [], "npm_workspaces": [],
                          "license_go": False, "codeql_matrix": '{"include":[]}',
                          "coverage_required": {key: True for key in CALLER.COMPONENTS}}
        self.jobs = {key: "success" for key in CALLER.JOBS}
        self.coverage = {key: True for key in CALLER.COMPONENTS}

    def file(self, name, value):
        path = self.root / name
        path.write_text(json.dumps(value), encoding="utf-8")
        return path

    def emit(self):
        args = types.SimpleNamespace(
            ids_json=self.file("ids.json", self.ids),
            selection_json=self.file("selection.json", self.selection),
            jobs_json=self.file("jobs.json", {**self.jobs, "sonar": "failure"}),
            coverage_json=self.file("coverage.json", self.coverage),
            out=self.root / "proof.json")
        return args

    def test_emit_binds_canonical_payload_to_oidc_audience_without_logging_token(self):
        calls = []

        def open_mock(request, timeout):
            calls.append(request)
            return Response(200, {"value": "header.payload.signature"})

        args = self.emit()
        env = {"ACTIONS_ID_TOKEN_REQUEST_URL": "https://pipelines.actions.githubusercontent.com/token?x=1",
               "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}
        with patch.dict(CALLER.os.environ, env, clear=True), patch.object(CALLER, "urlopen", open_mock):
            CALLER.emit_proof(args)
        proof = json.loads(args.out.read_text())
        raw = base64.b64decode(proof["payload_b64"], validate=True)
        payload = json.loads(raw)
        self.assertEqual(raw, CALLER.canonical(payload))
        self.assertEqual(set(payload), set(CALLER.IDS) | {"selection", "jobs", "coverage"})
        self.assertEqual(payload["jobs"], self.jobs)
        self.assertEqual(proof["oidc_jwt"], "header.payload.signature")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0].get_header("Authorization"), "Bearer private-request-bearer")
        audience = parse_qs(urlsplit(calls[0].full_url).query)["audience"]
        self.assertEqual(audience, [CALLER.AUDIENCE + CALLER.hashlib.sha256(raw).hexdigest()])
        self.assertEqual(args.out.stat().st_mode & 0o777, 0o600)

    def test_emit_rejects_missing_coverage_before_oidc_request(self):
        args = self.emit()
        self.coverage["admin"] = False
        self.file("coverage.json", self.coverage)
        with patch.object(CALLER, "urlopen", side_effect=AssertionError("must not request token")):
            with self.assertRaises(CALLER.Invalid):
                CALLER.emit_proof(args)
        self.assertFalse(args.out.exists())

    def test_emit_accepts_pr2_jobs_without_sonar(self):
        args = self.emit()
        self.file("jobs.json", self.jobs)
        with patch.dict(CALLER.os.environ, {
                "ACTIONS_ID_TOKEN_REQUEST_URL": "https://pipelines.actions.githubusercontent.com/token",
                "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}, clear=True), \
                patch.object(CALLER, "urlopen", return_value=Response(200, {"value": "header.payload.signature"})):
            CALLER.emit_proof(args)
        self.assertEqual(CALLER.read_proof(args.out)["jobs"], self.jobs)

    def test_emit_accepts_hosted_runner_regional_oidc_hosts(self):
        for host in ("pipelinesghubeus22.actions.githubusercontent.com",
                     "run-actions-3-azure-eastus.actions.githubusercontent.com"):
            with self.subTest(host=host):
                args = self.emit()
                with patch.dict(CALLER.os.environ, {
                        "ACTIONS_ID_TOKEN_REQUEST_URL": f"https://{host}/abc/idtoken/1/2?api-version=2.0",
                        "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}, clear=True), \
                        patch.object(CALLER, "urlopen",
                                     return_value=Response(200, {"value": "header.payload.signature"})):
                    CALLER.emit_proof(args)
                self.assertEqual(CALLER.read_proof(args.out)["jobs"], self.jobs)
                args.out.unlink()

    def test_emit_rejects_oidc_url_outside_github_actions_domain(self):
        for url in ("https://actions.githubusercontent.com.attacker.example/token",
                    "https://evilactions.githubusercontent.com/token",
                    "https://actions.githubusercontent.com/token",
                    "http://pipelinesghubeus22.actions.githubusercontent.com/token",
                    "https://user@pipelinesghubeus22.actions.githubusercontent.com/token"):
            with self.subTest(url=url):
                args = self.emit()
                with patch.dict(CALLER.os.environ, {
                        "ACTIONS_ID_TOKEN_REQUEST_URL": url,
                        "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}, clear=True), \
                        patch.object(CALLER, "urlopen", side_effect=AssertionError("must not send bearer")):
                    with self.assertRaisesRegex(CALLER.Invalid, "OIDC request environment unavailable"):
                        CALLER.emit_proof(args)
                self.assertFalse(args.out.exists())

    def test_cli_prints_fixed_invalid_reason_only(self):
        args = self.emit()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {}, clear=True), \
                patch("sys.argv", [str(SOURCE), "emit-proof", "--ids-json", str(args.ids_json),
                                   "--selection-json", str(args.selection_json),
                                   "--jobs-json", str(args.jobs_json),
                                   "--coverage-json", str(args.coverage_json),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 1)
        self.assertEqual(output.getvalue().strip(),
                         "protected Sonar caller failed: OIDC request environment unavailable")

    def test_emit_rejects_duplicate_or_oversized_input(self):
        args = self.emit()
        args.ids_json.write_text('{"caller_run_id":1,"caller_run_id":2}', encoding="utf-8")
        with self.assertRaises(CALLER.Invalid):
            CALLER.emit_proof(args)
        args = self.emit()
        self.selection["selected"]["unexpected"] = True
        self.file("selection.json", self.selection)
        with self.assertRaises(CALLER.Invalid):
            CALLER.emit_proof(args)
        self.selection["selected"].pop("unexpected")
        args.selection_json.write_bytes(b" " * (CALLER.MAX_INPUT + 1))
        with self.assertRaises(CALLER.Invalid):
            CALLER.emit_proof(args)

    def test_dispatch_posts_only_five_keys_to_main_and_polls_exact_run(self):
        payload = CALLER.validate_payload({**self.ids, "selection": self.selection,
                                           "jobs": self.jobs, "coverage": self.coverage})
        proof = {"payload_b64": base64.b64encode(CALLER.canonical(payload)).decode(),
                 "oidc_jwt": "header.payload.signature"}
        path = self.file("proof.json", proof)
        calls = []

        def open_mock(request, timeout):
            calls.append(request)
            if len(calls) == 1:
                return Response(200, {"workflow_run_id": 777})
            if len(calls) == 2:
                return Response(200, {"id": 777, "run_attempt": 1, "event": "workflow_dispatch",
                                      "head_branch": "main", "status": "in_progress"})
            return Response(200, {"id": 777, "run_attempt": 1, "event": "workflow_dispatch",
                                  "head_branch": "main", "status": "completed", "conclusion": "success"})

        args = types.SimpleNamespace(proof=path, out=self.root / "child.json")
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", open_mock), patch.object(CALLER.time, "sleep"):
            CALLER.dispatch_watch(args)
        self.assertEqual(json.loads(args.out.read_text()), {"workflow_run_id": 777, "run_attempt": 1})
        self.assertEqual(calls[0].full_url, CALLER.DISPATCH)
        self.assertEqual(calls[0].get_header("X-github-api-version"), "2026-03-10")
        body = json.loads(calls[0].data)
        self.assertEqual(body, {"ref": "main", "inputs": {key: str(payload[key]) for key in CALLER.IDS[:5]}})
        self.assertEqual([request.full_url for request in calls[1:]], [f"{CALLER.API}/actions/runs/777"] * 2)
        self.assertTrue(all(request.get_header("Authorization") == "Bearer private-gh-token" for request in calls))

    def test_dispatch_accepts_success_after_old_thirty_minute_window(self):
        args = self._watch_args()
        calls = []

        def open_mock(request, timeout):
            calls.append(request)
            if len(calls) == 1:
                return Response(200, {"workflow_run_id": 777})
            done = len(calls) == 242  # Dispatch, then 241 exact-run polls.
            return Response(200, {"id": 777, "run_attempt": 1,
                                  "event": "workflow_dispatch", "head_branch": "main",
                                  "status": "completed" if done else "in_progress",
                                  "conclusion": "success" if done else None})

        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", open_mock), \
                patch.object(CALLER.time, "sleep") as sleep:
            CALLER.dispatch_watch(args)
        self.assertEqual(len(calls), 242)
        self.assertEqual(sleep.call_count, 240)
        self.assertEqual(json.loads(args.out.read_text()), {"workflow_run_id": 777, "run_attempt": 1})

    def test_dispatch_timeout_is_bounded_at_forty_five_minutes(self):
        args = self._watch_args()
        calls = []

        def open_mock(request, timeout):
            calls.append(request)
            if len(calls) == 1:
                return Response(200, {"workflow_run_id": 777})
            return Response(200, {"id": 777, "run_attempt": 1,
                                  "event": "workflow_dispatch", "head_branch": "main",
                                  "status": "in_progress"})

        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", open_mock), \
                patch.object(CALLER.time, "sleep") as sleep:
            with self.assertRaisesRegex(CALLER.Invalid, "timed out"):
                CALLER.dispatch_watch(args)
        self.assertEqual(len(calls), 271)  # One dispatch plus 270 polls.
        self.assertEqual(sleep.call_count, 269)

    def test_dispatch_rejects_noncanonical_or_malformed_proof_before_api(self):
        args = self._watch_args()
        proof = json.loads(args.proof.read_text())
        payload = CALLER.read_proof(args.proof)
        proof["payload_b64"] = base64.b64encode(json.dumps(payload).encode()).decode()
        self.file("proof.json", proof)
        with patch.object(CALLER, "urlopen", side_effect=AssertionError("no API request")):
            with self.assertRaises(CALLER.Invalid):
                CALLER.dispatch_watch(args)
        proof["payload_b64"] = "%%%"
        self.file("proof.json", proof)
        with self.assertRaises(CALLER.Invalid):
            CALLER.dispatch_watch(args)
        self.assertFalse(args.out.exists())

    def test_dispatch_rejects_legacy_204_or_missing_child_id(self):
        args = self._watch_args()
        for status, body in ((204, b""), (200, {})):
            with self.subTest(status=status, body=body), patch.dict(CALLER.os.environ,
                    {"GH_TOKEN": "private-gh-token"}, clear=True), \
                    patch.object(CALLER, "urlopen", return_value=Response(status, body)):
                with self.assertRaises(CALLER.Invalid):
                    CALLER.dispatch_watch(args)
            self.assertFalse(args.out.exists())

    def test_dispatch_rejects_other_attempt_and_failed_child(self):
        args = self._watch_args()
        for run in ({"id": 777, "run_attempt": 2, "event": "workflow_dispatch",
                     "head_branch": "main", "status": "completed", "conclusion": "success"},
                    {"id": 777, "run_attempt": 1, "event": "workflow_dispatch",
                     "head_branch": "main", "status": "completed", "conclusion": "failure"}):
            args.out.unlink(missing_ok=True)
            with self.subTest(run=run), patch.dict(CALLER.os.environ,
                    {"GH_TOKEN": "private-gh-token"}, clear=True), \
                    patch.object(CALLER, "urlopen", side_effect=[Response(200, {"workflow_run_id": 777}),
                                                             Response(200, run)]):
                with self.assertRaises(CALLER.Invalid):
                    CALLER.dispatch_watch(args)

    def test_cli_error_does_not_print_sensitive_api_response(self):
        args = self._watch_args()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=OSError("private-gh-token")), \
                patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 1)
        self.assertNotIn("private-gh-token", output.getvalue())

    def _watch_args(self):
        payload = CALLER.validate_payload({**self.ids, "selection": self.selection,
                                           "jobs": self.jobs, "coverage": self.coverage})
        proof = {"payload_b64": base64.b64encode(CALLER.canonical(payload)).decode(),
                 "oidc_jwt": "header.payload.signature"}
        return types.SimpleNamespace(proof=self.file("proof.json", proof), out=self.root / "child.json")


if __name__ == "__main__":
    unittest.main()
