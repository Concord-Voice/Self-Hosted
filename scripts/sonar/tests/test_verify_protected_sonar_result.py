"""Local HTTP fixtures for the protected Sonar check boundary."""

from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
from pathlib import Path
import threading
import unittest
from urllib.parse import parse_qs, urlsplit


SCRIPT = Path(__file__).resolve().parents[1] / "verify-protected-sonar-result.py"
SPEC = importlib.util.spec_from_file_location("verify_protected_sonar_result", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
HEAD = "a" * 40
BASE = "b" * 40


def stamp(when):
    return when.isoformat().replace("+00:00", "Z")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        case = self.server.case
        path = urlsplit(self.path)
        if path.path == "/repos/test/repo/pulls/7":
            body = case.pr
        elif path.path == f"/repos/test/repo/commits/{HEAD}/check-runs":
            if case.redirect_check:
                self.send_response(302)
                self.send_header("Location", "https://example.invalid/redirect")
                self.end_headers()
                return
            query = parse_qs(path.query)
            if query.get("app_id") != ["12526"] or query.get("filter") != ["all"]:
                self.send_error(400)
                return
            case.check_requests += 1
            if case.check_error:
                self.send_error(case.check_error)
                return
            checks = case.check_sequences[min(case.check_requests - 1, len(case.check_sequences) - 1)]
            body = {"total_count": len(checks), "check_runs": checks}
            if case.move_after_check:
                case.pr["base"]["sha"] = HEAD
        else:
            self.send_error(404)
            return
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, _format, *_args):
        pass


class ProtectedSonarResultTest(unittest.TestCase):
    def setUp(self):
        now = datetime.now(timezone.utc)
        self.window = stamp(now - timedelta(minutes=5))
        self.pr = {"number": 7, "state": "open", "head": {"sha": HEAD},
                   "base": {"sha": BASE, "ref": "main"}}
        self.check = {"id": 123, "app": {"id": 12526}, "name": "SonarCloud Code Analysis",
                      "head_sha": HEAD, "status": "completed", "conclusion": "success",
                      "created_at": stamp(now - timedelta(minutes=4)),
                      "started_at": stamp(now - timedelta(minutes=3)),
                      "completed_at": stamp(now - timedelta(minutes=2)),
                      "pull_requests": [{"number": 7, "head": {"sha": HEAD},
                                         "base": {"sha": BASE, "ref": "main"}}]}
        self.check_sequences = [[self.check]]
        self.check_requests = 0
        self.check_error = None
        self.redirect_check = False
        self.move_after_check = False
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.case = self
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.thread.join)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.api = MODULE.GitHub("local-test-token", f"http://127.0.0.1:{self.server.server_port}/repos/test/repo")

    def verify(self, timeout=0):
        MODULE.verify(self.api, 7, HEAD, BASE, "main", self.window, timeout, poll_seconds=0)

    def test_fresh_completed_success_and_pending_poll(self):
        self.check_sequences = [[{**self.check, "status": "queued", "conclusion": None,
                                  "started_at": None, "completed_at": None}], [self.check]]
        self.verify(timeout=1)
        self.assertEqual(self.check_requests, 2)

    def test_real_check_shape_without_created_at(self):
        check = {key: value for key, value in self.check.items() if key != "created_at"}
        check["external_id"] = ""
        self.check_sequences = [[check]]
        self.verify()

    def test_old_same_head_success_and_wrong_app_do_not_pass(self):
        old = {key: value for key, value in self.check.items() if key != "created_at"}
        old["started_at"] = stamp(datetime.now(timezone.utc) - timedelta(minutes=9))
        old["external_id"] = ""
        self.check_sequences = [[old]]
        with self.assertRaisesRegex(MODULE.Reject, "timed out"):
            self.verify()
        self.check_sequences = [[{**self.check, "app": {"id": 999}}]]
        with self.assertRaisesRegex(MODULE.Reject, "timed out"):
            self.verify()

    def test_duplicate_and_failed_current_checks_block(self):
        self.check_sequences = [[self.check, {**self.check, "id": 124}]]
        with self.assertRaisesRegex(MODULE.Reject, "multiple fresh"):
            self.verify()
        queued = {key: value for key, value in self.check.items() if key != "created_at"}
        queued.update(id=124, status="queued", conclusion=None, started_at=None, completed_at=None)
        self.check_sequences = [[self.check, queued]]
        with self.assertRaisesRegex(MODULE.Reject, "multiple fresh"):
            self.verify()
        self.check_sequences = [[{**self.check, "conclusion": "failure"}]]
        with self.assertRaisesRegex(MODULE.Reject, "did not succeed"):
            self.verify()

    def test_moved_head_or_base_and_wrong_pr_link_block(self):
        self.pr["head"]["sha"] = BASE
        with self.assertRaisesRegex(MODULE.Reject, "revision moved"):
            self.verify()
        self.pr["head"]["sha"] = HEAD
        self.pr["base"]["sha"] = HEAD
        with self.assertRaisesRegex(MODULE.Reject, "revision moved"):
            self.verify()
        self.pr["base"]["sha"] = BASE
        self.check_sequences = [[{**self.check, "pull_requests": [{"number": 8,
                                 "head": {"sha": HEAD}, "base": {"sha": BASE, "ref": "main"}}]}]]
        with self.assertRaisesRegex(MODULE.Reject, "timed out"):
            self.verify()

    def test_pr_move_after_check_is_rechecked(self):
        self.move_after_check = True
        with self.assertRaisesRegex(MODULE.Reject, "revision moved"):
            self.verify()

    def test_api_failure_and_malformed_time_block(self):
        self.check_error = 503
        with self.assertRaisesRegex(MODULE.Reject, "HTTP 503"):
            self.verify()
        self.check_error = None
        self.redirect_check = True
        with self.assertRaisesRegex(MODULE.Reject, "HTTP 302"):
            self.verify()
        self.redirect_check = False
        self.check_sequences = [[{**self.check, "started_at": "bad"}]]
        with self.assertRaisesRegex(MODULE.Reject, "check time invalid"):
            self.verify()


if __name__ == "__main__":
    unittest.main()
