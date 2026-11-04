#!/usr/bin/env python3
"""Exercise the actual publisher bootstrap without apt, network or root effects."""

import json
from copy import deepcopy
from contextlib import redirect_stderr, redirect_stdout
import io
import runpy
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/publish-minio-image.yml"
FINGERPRINT = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
PACKAGES = (
    "docker-ce=5:29.9.2-1~ubuntu.24.04~noble",
    "docker-ce-cli=5:29.9.2-1~ubuntu.24.04~noble",
    "docker-ce-rootless-extras=5:29.9.2-1~ubuntu.24.04~noble",
    "containerd.io=2.4.2-3~ubuntu.24.04~noble",
    "docker-buildx-plugin=0.36.1-1~ubuntu.24.04~noble",
    "docker-compose-plugin=5.5.0-1~ubuntu.24.04~noble",
)

DOUBLE = r'''#!/usr/bin/python3
import json, os, pathlib, sys
kind = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["PIN_TRACE"], "a") as trace:
    trace.write(json.dumps([kind, args]) + "\n")
mode = os.environ["PIN_MODE"]
if kind == "sudo":
    if args[:1] == ["tee"]:
        with open(os.environ["PIN_TRACE"], "a") as trace:
            trace.write(json.dumps(["source", sys.stdin.read()]) + "\n")
    if mode == "apt-fail" and any(a.startswith("docker-ce=") for a in args):
        sys.exit(42)
    if mode == "start-fail" and args[:2] == ["systemctl", "start"]:
        sys.exit(42)
    if mode == "restart-fail" and args[:2] == ["systemctl", "restart"]:
        sys.exit(42)
elif kind == "curl":
    if mode == "curl-fail":
        sys.exit(42)
    pathlib.Path(args[args.index("-o") + 1]).write_text("fixture-public-key")
elif kind == "gpg":
    good = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
    def key(fingerprint):
        print("pub:::::::::")
        print("fpr:::::::::" + fingerprint + ":")
    if mode != "empty":
        key("A" * 40 if mode == "wrong" else good)
    if mode in ("extra", "duplicate"):
        key("B" * 40 if mode == "extra" else good)
    print("sub:::::::::")
    print("fpr:::::::::" + "C" * 40 + ":")
    if mode == "gpg-fail":
        sys.exit(42)
elif kind == "dpkg":
    print(os.environ["PIN_ARCH"])
elif kind == "dpkg-query":
    sys.exit(1)
'''


class MinioEnginePins(unittest.TestCase):
    def setUp(self):
        self.jobs = yaml.safe_load(WORKFLOW.read_text())["jobs"]

    def install(self, job_id):
        return next(step["run"] for step in self.jobs[job_id]["steps"]
                    if step.get("name", "").startswith("Install official Docker CE"))

    def test_probe_selects_only_exact_installation_and_admission(self):
        module = runpy.run_path(str(ROOT / "scripts/ci/probe-publisher-engine.py"))
        select = module["select_steps"]
        for job_id, runner in module["RUNNERS"].items():
            with self.subTest(job=job_id):
                selected = select({"jobs": self.jobs}, job_id, runner)
                self.assertEqual(selected[0], self.install(job_id))
                self.assertEqual(len(selected), 2)
                self.assertEqual(selected[1].strip(), ("sudo " if job_id == "smoke-arm64" else "")
                                 + "bash scripts/ci/require-docker-engine.sh")
                self.assertNotIn("docker login", "\n".join(selected))
                self.assertNotIn("docker push", "\n".join(selected))
        for job_id, runner in (("unknown", "ubuntu-24.04"), ("publish", "unadmitted")):
            with self.assertRaises(ValueError):
                select({"jobs": self.jobs}, job_id, runner)
        for mutation in ("runner", "missing", "conditional", "advisory", "duplicate", "order"):
            with self.subTest(mutation=mutation):
                document = {"jobs": deepcopy(self.jobs)}
                job = document["jobs"]["publish"]
                installer = next(step for step in job["steps"]
                                 if step.get("name", "").startswith("Install official Docker CE"))
                guard = next(step for step in job["steps"]
                             if step.get("run", "").strip() == "bash scripts/ci/require-docker-engine.sh")
                if mutation == "runner":
                    job["runs-on"] = "unadmitted"
                elif mutation == "missing":
                    job["steps"].remove(guard)
                elif mutation == "conditional":
                    guard["if"] = "false"
                elif mutation == "advisory":
                    guard["continue-on-error"] = True
                elif mutation == "duplicate":
                    job["steps"].append(deepcopy(installer))
                else:
                    job["steps"].remove(guard)
                    job["steps"].insert(0, guard)
                with self.assertRaises(ValueError):
                    select(document, "publish", "ubuntu-24.04")

    def test_probe_executes_in_order_and_stops_on_failure(self):
        module = runpy.run_path(str(ROOT / "scripts/ci/probe-publisher-engine.py"))
        expected = [self.install("publish"), "bash scripts/ci/require-docker-engine.sh"]
        for failure_at in (None, 0, 1):
            with self.subTest(failure_at=failure_at), patch("subprocess.run") as run:
                if failure_at is not None:
                    run.side_effect = [None] * failure_at + [subprocess.CalledProcessError(42, "fixture")]
                with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    code = module["main"](["publish", "ubuntu-24.04"])
                self.assertEqual(code, 0 if failure_at is None else 1)
                count = 2 if failure_at is None else failure_at + 1
                self.assertEqual(run.call_count, count)
                for call, script in zip(run.call_args_list, expected[:count]):
                    self.assertEqual(call.args[0], ["bash", "-e", "-o", "pipefail", "-c", script])
                    self.assertEqual(call.kwargs, {"cwd": ROOT, "check": True})
        with patch("subprocess.run") as run, redirect_stderr(io.StringIO()):
            self.assertEqual(module["main"]([]), 1)
            run.assert_not_called()

    def test_each_bootstrap_requires_the_reviewed_key_and_exact_package_set(self):
        for job_id in ("publish", "smoke-arm64", "finalize"):
            with self.subTest(job=job_id):
                job = self.jobs[job_id]
                expected_runner = "ubuntu-24.04-arm" if job_id == "smoke-arm64" else "ubuntu-24.04"
                self.assertEqual(job["runs-on"], expected_runner)
                script = self.install(job_id)
                self.assertIn("Signed-By: /etc/apt/keyrings/docker.asc " + FINGERPRINT, script)
                for package in PACKAGES:
                    self.assertIn(package, script)
                self.assertIn("--no-install-recommends", script)

    def test_actual_bootstraps_refuse_before_trusting_bad_keys_or_crossing_failed_installs(self):
        modes = ("good", "wrong", "extra", "duplicate", "empty", "gpg-fail",
                 "curl-fail", "apt-fail", "start-fail", "restart-fail")
        for job_id in ("publish", "smoke-arm64", "finalize"):
            for mode in modes:
                if (job_id == "smoke-arm64" and mode == "restart-fail") or (
                        job_id != "smoke-arm64" and mode == "start-fail"):
                    continue
                with self.subTest(job=job_id, mode=mode), tempfile.TemporaryDirectory() as temporary:
                    directory = Path(temporary)
                    bindir = directory / "bin"
                    bindir.mkdir()
                    for name in ("sudo", "curl", "gpg", "dpkg", "dpkg-query"):
                        command = bindir / name
                        command.write_text(DOUBLE)
                        command.chmod(0o755)
                    trace = directory / "trace"
                    marker = directory / "next-step"
                    env = {**os.environ, "PATH": str(bindir) + os.pathsep + os.environ["PATH"],
                           "TMPDIR": str(directory), "PIN_MODE": mode, "PIN_TRACE": str(trace),
                           "PIN_ARCH": "arm64" if job_id == "smoke-arm64" else "amd64",
                           "UBUNTU_CODENAME": "noble"}
                    script = self.install(job_id) + f"\nprintf reached > '{marker}'\n"
                    result = subprocess.run(["bash", "-c", script], env=env, cwd=ROOT,
                                            text=True, capture_output=True, timeout=10)
                    calls = [json.loads(line) for line in trace.read_text().splitlines()]
                    self.assertEqual(result.returncode == 0, mode == "good", result.stderr)
                    self.assertEqual(marker.exists(), mode == "good")
                    key_refused = mode in ("wrong", "extra", "duplicate", "empty", "gpg-fail", "curl-fail")
                    sources = [args for kind, args in calls if kind == "source"]
                    package_calls = [args for kind, args in calls if kind == "sudo"
                                     and any(arg.startswith("docker-ce=") for arg in args)]
                    key_installs = [args for kind, args in calls if kind == "sudo"
                                    and args[:3] == ["install", "-m", "0644"]]
                    self.assertEqual(bool(sources), not key_refused)
                    self.assertEqual(bool(package_calls), not key_refused)
                    self.assertEqual(bool(key_installs), not key_refused)
                    if package_calls:
                        self.assertEqual(package_calls[0][-len(PACKAGES):], list(PACKAGES))
                    self.assertNotIn("fixture-public-key", result.stdout + result.stderr)
                    self.assertEqual(list(directory.glob("tmp.*")), [], "owned key scratch must be cleaned")


if __name__ == "__main__":
    unittest.main(verbosity=2)
