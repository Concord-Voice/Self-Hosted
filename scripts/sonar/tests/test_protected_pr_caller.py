"""Local proof and REST contract tests; no GitHub credentials or network access."""

import base64
from contextlib import redirect_stderr, redirect_stdout
from http.client import IncompleteRead
from io import BytesIO, StringIO
import importlib.util
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError
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


RUN = {"id": 777, "run_attempt": 1, "event": "workflow_dispatch", "head_branch": "main"}
DISPATCHED = Response(200, {"workflow_run_id": 777})
RUNNING = Response(200, {**RUN, "status": "in_progress"})
DONE = Response(200, {**RUN, "status": "completed", "conclusion": "success"})


def http_error(code, headers=None):
    return HTTPError(CALLER.API, code, "private-body", headers or {}, BytesIO(b""))


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

    def test_schema_v2_payload_round_trips_its_merge_base(self):
        """Schema 2 proves the selected merge's parent without weakening v1's exact shape."""
        self.selection.update(version=2, merge_base_sha=SHA_B)
        args = self.emit()
        with patch.dict(CALLER.os.environ, {
                "ACTIONS_ID_TOKEN_REQUEST_URL": "https://pipelines.actions.githubusercontent.com/token",
                "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}, clear=True), \
                patch.object(CALLER, "urlopen", return_value=Response(200, {"value": "header.payload.signature"})):
            CALLER.emit_proof(args)
        proof = json.loads(args.out.read_text())
        payload = json.loads(base64.b64decode(proof["payload_b64"], validate=True))
        self.assertEqual(payload["selection"]["merge_base_sha"], SHA_B)

    def test_schema_payload_rejects_missing_malformed_extra_or_downgraded_merge_base(self):
        """A merge base is mandatory only for v2 and cannot be smuggled into v1."""
        cases = (
            (2, None, False),
            (2, "not-a-sha", False),
            (2, SHA_B, True),
            (1, SHA_B, False),
        )
        for version, merge_base, extra in cases:
            with self.subTest(version=version, merge_base=merge_base, extra=extra):
                selection = dict(self.selection, version=version)
                if merge_base is not None:
                    selection["merge_base_sha"] = merge_base
                if extra:
                    selection["unexpected"] = True
                with self.assertRaises(CALLER.Invalid):
                    CALLER.validate_payload({**self.ids, "selection": selection,
                                             "jobs": self.jobs, "coverage": self.coverage})

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

    def test_cli_http_error_reports_only_validated_status_without_dispatch_retry(self):
        class PrivateCode:
            def __repr__(self):
                raise AssertionError("must not format unvalidated HTTP status")

            __str__ = __repr__

        class UnreadableBody(BytesIO):
            def read(self, *args):
                raise AssertionError("must not read HTTP error response body")

        cases = [(403, "4xx"), (422, "4xx"), (429, "429"), (500, "5xx")]
        cases += [(code, "other") for code in
                  (None, True, False, "403", 403.0, 0, 99, 600, -1, PrivateCode())]
        for index, (code, expected) in enumerate(cases):
            with self.subTest(case=index):
                args = self._watch_args()
                args.out.unlink(missing_ok=True)
                output, stdout = StringIO(), StringIO()
                private_url = "https://private-api.invalid/dispatch?token=private-url-token"
                failure = HTTPError(private_url, 0, "private-http-message",
                                    {"Authorization": "Bearer private-header-token",
                                     "X-Private": "private-response-header"},
                                    UnreadableBody(b"private-response-body"))
                # Set the code after construction: Python 3.14+ formats repr(self) while building
                # the HTTPError, which would trip PrivateCode before the caller ever sees it.
                failure.code = code
                with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                        patch.object(CALLER, "urlopen", side_effect=failure) as request, \
                        patch.object(CALLER.time, "sleep") as sleep, \
                        patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                           "--out", str(args.out)]), \
                        redirect_stderr(output), redirect_stdout(stdout):
                    self.assertEqual(CALLER.main(), 1)
                request.assert_called_once()
                sleep.assert_not_called()
                self.assertFalse(args.out.exists(), "failed dispatch must not record a child run")
                self.assertEqual(stdout.getvalue(), "")
                note = " (a child run may have started)" if expected == "5xx" else ""
                self.assertEqual(output.getvalue(),
                                 "protected Sonar caller failed; inspect local input and API status "
                                 f"[dispatch_http_{expected}]{note}\n",
                                 "HTTPError must expose only a fixed status class instead of the generic OSError banner")

    def _watch(self, responses):
        """Run dispatch_watch over a urlopen script; returns (urlopen mock, sleep mock)."""
        args = self._watch_args()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=responses) as opener, \
                patch.object(CALLER.time, "sleep") as sleep:
            self.opener, self.sleep, self.child = opener, sleep, args.out
            CALLER.dispatch_watch(args)
        return opener, sleep

    def test_poll_retries_404_before_new_run_is_visible(self):
        opener, sleep = self._watch([DISPATCHED, http_error(404), DONE])
        self.assertEqual(opener.call_count, 3)
        sleep.assert_called_once_with(10)

    def test_poll_404_after_grace_window_fails_at_once(self):
        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([DISPATCHED] + [http_error(404)] * CALLER.NOT_VISIBLE_POLLS
                        + [http_error(404), DONE])
        self.assertEqual(ctx.exception.reason, "poll_http_404")
        self.assertEqual(self.opener.call_count, CALLER.NOT_VISIBLE_POLLS + 2)  # Dispatch, then the failing poll.
        self.assertEqual(self.sleep.call_count, CALLER.NOT_VISIBLE_POLLS)

    def test_poll_retries_5xx_in_mid_watch(self):
        opener, sleep = self._watch([DISPATCHED, RUNNING, http_error(503), URLError("private-host"), DONE])
        self.assertEqual(opener.call_count, 5)
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [10, 10, 10])

    def test_poll_retries_429_and_honours_retry_after(self):
        _, sleep = self._watch([DISPATCHED, http_error(429, {"Retry-After": "60"}),
                                http_error(429, {"Retry-After": "99999"}),
                                http_error(429, {"Retry-After": "soon"}), DONE])
        self.assertEqual([call.args[0] for call in sleep.call_args_list],
                         [60, CALLER.RETRY_AFTER_MAX, 10])

    def test_poll_404_after_the_run_was_seen_fails_at_once(self):
        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([DISPATCHED, RUNNING, http_error(404), DONE])
        self.assertEqual(ctx.exception.reason, "poll_http_404")
        self.assertEqual(self.opener.call_count, 3)  # DONE was never polled.
        self.sleep.assert_called_once_with(10)  # Only the sleep after RUNNING.

    def test_poll_malformed_response_fails_without_retry(self):
        for response in (Response(202, RUN), Response(200, b""), Response(200, b"[]")):
            with self.subTest(status=response.status, raw=response.raw), \
                    self.assertRaises(CALLER.Invalid):
                self._watch([DISPATCHED, response, DONE])
            self.assertEqual(self.opener.call_count, 2)
            self.sleep.assert_not_called()
            self.child.unlink()

    def test_poll_retry_after_is_honoured_on_5xx_too(self):
        _, sleep = self._watch([DISPATCHED, http_error(503, {"Retry-After": "60"}), DONE])
        sleep.assert_called_once_with(60)

    def test_poll_retries_the_top_of_the_5xx_range(self):
        opener, _ = self._watch([DISPATCHED, http_error(599), DONE])
        self.assertEqual(opener.call_count, 3)

    def test_poll_403_fails_at_once_without_retry_or_sleep(self):
        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([DISPATCHED, http_error(403, {"Retry-After": "60"}), DONE])
        self.assertEqual(ctx.exception.reason, "poll_http_4xx")
        self.assertEqual(self.opener.call_count, 2)
        self.sleep.assert_not_called()

    def test_dispatch_post_error_is_not_retried(self):
        args = self._watch_args()
        for error, reason in ((http_error(502), "dispatch_http_5xx"), (http_error(429), "dispatch_http_429"),
                              (TimeoutError("private-host"), "dispatch_network")):
            with self.subTest(reason=reason), patch.dict(CALLER.os.environ, {"GH_TOKEN": "t"}, clear=True), \
                    patch.object(CALLER, "urlopen", side_effect=[error, DISPATCHED, DONE]) as opener, \
                    patch.object(CALLER.time, "sleep") as sleep, self.assertRaises(CALLER.ApiError) as ctx:
                CALLER.dispatch_watch(args)
            self.assertEqual(ctx.exception.reason, reason)
            self.assertEqual(opener.call_count, 1)
            sleep.assert_not_called()
            self.assertFalse(args.out.exists())

    def test_poll_retry_keeps_identity_and_result_checks(self):
        for run, message in (({**RUN, "run_attempt": 2, "status": "completed", "conclusion": "success"}, "identity"),
                             ({**RUN, "status": "completed", "conclusion": "failure"}, "failed")):
            with self.subTest(message=message), self.assertRaisesRegex(CALLER.Invalid, message):
                self._watch([DISPATCHED, http_error(500), Response(200, run)])
            self.child.unlink(missing_ok=True)

    def test_poll_retries_still_end_at_the_forty_five_minute_budget(self):
        # Alternate errors and running polls so the consecutive-error cap is never reached.
        with self.assertRaisesRegex(CALLER.Invalid, "timed out"):
            self._watch([DISPATCHED] + [http_error(502), RUNNING] * (CALLER.MAX_POLLS // 2))
        self.assertEqual(self.opener.call_count, CALLER.MAX_POLLS + 1)
        self.assertEqual(self.sleep.call_count, CALLER.MAX_POLLS - 1)

    def test_poll_running_to_the_last_slot_times_out_without_a_final_sleep(self):
        with self.assertRaisesRegex(CALLER.Invalid, "timed out"):
            self._watch([DISPATCHED] + [RUNNING] * CALLER.MAX_POLLS)
        self.assertEqual(self.sleep.call_count, CALLER.MAX_POLLS - 1)

    def test_poll_consecutive_transient_errors_fail_with_their_reason_class(self):
        for error, reason in ((http_error(503), "poll_http_5xx"), (URLError("private-host"), "poll_network")):
            with self.subTest(reason=reason), self.assertRaises(CALLER.ApiError) as ctx:
                self._watch([DISPATCHED] + [error] * (CALLER.MAX_TRANSIENT_RUN + 1) + [DONE])
            self.assertEqual(ctx.exception.reason, reason)
            self.assertEqual(self.opener.call_count, CALLER.MAX_TRANSIENT_RUN + 2)  # DONE never consumed.
            self.child.unlink(missing_ok=True)

    def test_poll_a_good_poll_resets_the_consecutive_error_count(self):
        script = ([DISPATCHED] + [http_error(502)] * CALLER.MAX_TRANSIENT_RUN + [RUNNING]
                  + [http_error(502)] * CALLER.MAX_TRANSIENT_RUN + [DONE])
        self._watch(script)
        self.assertEqual(self.opener.call_count, len(script))

    def test_poll_wall_clock_deadline_ends_the_watch(self):
        limit = CALLER.MAX_POLLS * CALLER.POLL_SECONDS + CALLER.DEADLINE_SLACK
        clock = iter([0, 0])
        with patch.object(CALLER.time, "monotonic", lambda: next(clock, limit + 1)):
            with self.assertRaisesRegex(CALLER.Invalid, "timed out"):
                self._watch([DISPATCHED, http_error(502), DONE])
        self.assertEqual(self.opener.call_count, 2)  # DONE was never polled.
        self.sleep.assert_not_called()  # No time left, so no (negative) sleep either.
        # Positive control: with the clock standing still the same script succeeds.
        self.child.unlink()
        with patch.object(CALLER.time, "monotonic", lambda: 0):
            self._watch([DISPATCHED, http_error(502), DONE])

    def test_poll_retry_sleep_is_clamped_to_the_time_left(self):
        limit = CALLER.MAX_POLLS * CALLER.POLL_SECONDS + CALLER.DEADLINE_SLACK
        # deadline calc, top-of-loop check, then 5 s left when the retry sleep is chosen.
        clock = iter([0, 0, limit - 5])
        with patch.object(CALLER.time, "monotonic", lambda: next(clock, limit + 1)):
            with self.assertRaisesRegex(CALLER.Invalid, "timed out"):
                self._watch([DISPATCHED, http_error(429, {"Retry-After": "120"}), DONE])
        self.sleep.assert_called_once_with(5)
        self.assertEqual(self.opener.call_count, 2)  # DONE was never polled.

    def test_poll_identity_is_compared_by_type_as_well_as_value(self):
        for field, value in (("id", 777.0), ("run_attempt", True), ("run_attempt", 1.0)):
            with self.subTest(field=field, value=value), \
                    self.assertRaisesRegex(CALLER.Invalid, "identity invalid"):
                self._watch([DISPATCHED, Response(200, {**RUN, field: value, "status": "completed",
                                                         "conclusion": "success"})])
            self.child.unlink(missing_ok=True)

    def test_poll_body_read_failures_are_network_errors_and_retried(self):
        class Broken(Response):
            def __init__(self, error):
                super().__init__(200, {})
                self.error = error

            def read(self, size):
                raise self.error

        for error in (IncompleteRead(b""), ConnectionResetError("private-host")):
            with self.subTest(error=type(error).__name__):
                self._watch([DISPATCHED, Broken(error), DONE])
                self.assertEqual(self.opener.call_count, 3)
                self.sleep.assert_called_once_with(10)
                self.child.unlink()

    def test_dispatch_body_read_failure_is_a_network_error_without_retry(self):
        class Broken(Response):
            def read(self, size):
                raise IncompleteRead(b"")

        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([Broken(200, {}), DISPATCHED, DONE])
        self.assertEqual(ctx.exception.reason, "dispatch_network")
        self.assertEqual(self.opener.call_count, 1)

    def test_redirects_are_not_retried(self):
        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([DISPATCHED, http_error(302), DONE])
        self.assertEqual(ctx.exception.reason, "poll_http_other")
        self.sleep.assert_not_called()
        self.child.unlink()
        with self.assertRaises(CALLER.ApiError) as ctx:
            self._watch([http_error(301), DISPATCHED, DONE])
        self.assertEqual(ctx.exception.reason, "dispatch_http_other")

    def test_http_class_is_a_closed_set(self):
        for status, expected in ((404, "404"), (429, "429"), (500, "5xx"), (503, "5xx"), (599, "5xx"),
                                 (400, "4xx"), (403, "4xx"), (499, "4xx"), (302, "other"), (200, "other")):
            self.assertEqual(CALLER.http_class(status), expected, status)

    def test_retry_after_accepts_only_small_ascii_integers(self):
        for value, expected in (("60", 60), ("0", 0), ("3", 3), ("999999", 999999), ("", None), ("-5", None),
                                ("1.5", None), ("soon", None), ("1000000", None), ("\u0663", None)):
            self.assertEqual(CALLER.retry_after({"Retry-After": value}), expected, value)
        self.assertIsNone(CALLER.retry_after({}))
        self.assertIsNone(CALLER.retry_after(None))

    def test_retry_after_below_the_cadence_keeps_the_ten_second_poll(self):
        _, sleep = self._watch([DISPATCHED, http_error(429, {"Retry-After": "3"}), DONE])
        sleep.assert_called_once_with(10)

    def test_oidc_failures_carry_the_oidc_reason_class(self):
        env = {"ACTIONS_ID_TOKEN_REQUEST_URL": "https://pipelines.actions.githubusercontent.com/token?x=1",
               "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "private-request-bearer"}
        for error, reason in ((http_error(500), "oidc_http_5xx"), (URLError("private-host"), "oidc_network")):
            with self.subTest(reason=reason), patch.dict(CALLER.os.environ, env, clear=True), \
                    patch.object(CALLER, "urlopen", side_effect=error), \
                    self.assertRaises(CALLER.ApiError) as ctx:
                CALLER.oidc_token(b"{}")
            self.assertEqual(ctx.exception.reason, reason)

    def test_transient_retries_are_logged_by_reason_class_only(self):
        args = self._watch_args()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=[DISPATCHED, http_error(503), URLError("private-host"), DONE]), \
                patch.object(CALLER.time, "sleep"), \
                patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 0)
        self.assertEqual(output.getvalue().splitlines(),
                         ["protected Sonar caller: transient poll error [poll_http_5xx]; retrying",
                          "protected Sonar caller: transient poll error [poll_network]; retrying"])

    def test_cli_dispatch_gateway_5xx_says_a_child_may_exist(self):
        args = self._watch_args()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=http_error(504)), \
                patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 1)
        self.assertEqual(output.getvalue().strip(),
                         "protected Sonar caller failed; inspect local input and API status "
                         "[dispatch_http_5xx] (a child run may have started)")

    def test_cli_dispatch_network_error_says_a_child_may_exist(self):
        args = self._watch_args()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=TimeoutError("private-host")), \
                patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 1)
        self.assertEqual(output.getvalue().strip(),
                         "protected Sonar caller failed; inspect local input and API status "
                         "[dispatch_network] (a child run may have started)")

    def test_cli_prints_only_the_fixed_reason_class(self):
        args = self._watch_args()
        output = StringIO()
        with patch.dict(CALLER.os.environ, {"GH_TOKEN": "private-gh-token"}, clear=True), \
                patch.object(CALLER, "urlopen", side_effect=[DISPATCHED, http_error(403)]), \
                patch("sys.argv", [str(SOURCE), "dispatch-watch", "--proof", str(args.proof),
                                   "--out", str(args.out)]), redirect_stderr(output):
            self.assertEqual(CALLER.main(), 1)
        self.assertEqual(output.getvalue().strip(),
                         "protected Sonar caller failed; inspect local input and API status [poll_http_4xx]")

    def _watch_args(self):
        payload = CALLER.validate_payload({**self.ids, "selection": self.selection,
                                           "jobs": self.jobs, "coverage": self.coverage})
        proof = {"payload_b64": base64.b64encode(CALLER.canonical(payload)).decode(),
                 "oidc_jwt": "header.payload.signature"}
        return types.SimpleNamespace(proof=self.file("proof.json", proof), out=self.root / "child.json")


if __name__ == "__main__":
    unittest.main()
