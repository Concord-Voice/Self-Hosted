#!/usr/bin/env python3
"""Exercise locked runner provisioning at the OS and daemon process boundary."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
INSTALL = ROOT / "scripts/ci/install-docker-engine.sh"
PACKAGES = [
    "docker-ce=5:29.9.2-1~ubuntu.24.04~noble",
    "docker-ce-cli=5:29.9.2-1~ubuntu.24.04~noble",
    "docker-ce-rootless-extras=5:29.9.2-1~ubuntu.24.04~noble",
    "containerd.io=2.4.2-3~ubuntu.24.04~noble",
    "docker-buildx-plugin=0.36.1-1~ubuntu.24.04~noble",
    "docker-compose-plugin=5.5.0-1~ubuntu.24.04~noble",
]
KEY = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"  # pragma: allowlist secret -- public Docker primary fingerprint
DOUBLE = r'''#!/usr/bin/python3
import json, os, pathlib, sys
kind = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
mode = os.environ['ENGINE_MODE']
with open(os.environ['ENGINE_TRACE'], 'a') as f:
    f.write(json.dumps([kind, args]) + '\n')
if kind == 'lsb_release':
    print(('Debian' if mode == 'wrong-os' else 'Ubuntu') if args == ['-is'] else 'noble')
elif kind == 'dpkg':
    print('arm64' if mode == 'wrong-arch' else 'amd64')
elif kind == 'curl':
    if mode == 'download-fail': sys.exit(42)
    pathlib.Path(args[args.index('-o') + 1]).write_text('fixture-public-key')
elif kind == 'gpg':
    key = '9DC858229FC7DD38854AE2D88D81803C0EBFCD88'  # pragma: allowlist secret -- public Docker fingerprint
    if mode != 'empty-key':
        print('pub:::::::::'); print('fpr:::::::::' + ('A'*40 if mode == 'wrong-key' else key) + ':')
    if mode in ('extra-key', 'duplicate-key'):
        print('pub:::::::::'); print('fpr:::::::::' + (key if mode == 'duplicate-key' else 'B'*40) + ':')
    if mode == 'gpg-fail': sys.exit(42)
elif kind == 'sudo':
    if args[:1] == ['tee']:
        with open(os.environ['ENGINE_TRACE'], 'a') as f: f.write(json.dumps(['source', sys.stdin.read()])+'\n')
    if mode == 'trust-fail' and args[:3] == ['install', '-m', '0644']: sys.exit(42)
    if mode == 'apt-fail' and any(x.startswith('docker-ce=') for x in args): sys.exit(42)
    if mode == 'restart-fail' and args[:2] == ['systemctl', 'restart']: sys.exit(42)
    if mode == 'inactive' and args[:2] == ['systemctl', 'is-active']: sys.exit(42)
elif kind == 'docker':
    if args == ['context', 'show']: print('remote' if mode == 'remote-context' else 'default')
    elif args[:1] == ['version']:
        print('28.0.4' if mode == 'old-server' else '29.9.2')
        if mode == 'query-fail': sys.exit(42)
    elif args[:1] == ['run']:
        if mode == 'service-run-fail': sys.exit(42)
        print('fixture-container-id')
    elif args[:1] == ['inspect']:
        name = args[-1]
        inspections = sum(1 for line in pathlib.Path(os.environ['ENGINE_TRACE']).read_text().splitlines()
                          if json.loads(line) == [kind, args])
        if mode == 'service-never-ready' or (mode == 'service-starting' and inspections == 1): print('starting')
        elif mode == 'service-unhealthy' or mode == 'unhealthy-' + name: print('unhealthy')
        else: print('healthy')
'''


class RunnerEngine(unittest.TestCase):
    def execute(self, mode, environment=None, script=INSTALL):
        with tempfile.TemporaryDirectory() as temporary:
            p = Path(temporary)
            trace = p / "trace"
            trace.touch()
            for name in ("sudo", "curl", "gpg", "dpkg", "lsb_release", "docker", "sleep"):
                command = p / name
                command.write_text(DOUBLE)
                command.chmod(0o755)
            env = {**os.environ, "PATH": str(p) + ":" + os.environ["PATH"],
                   "ENGINE_TRACE": str(trace), "ENGINE_MODE": mode,
                   "GITHUB_ACTIONS": "true", "RUNNER_OS": "Linux", "RUNNER_ARCH": "X64"}
            for name in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
                env.pop(name, None)
            env.update(environment or {})
            result = subprocess.run(["bash", str(script)], env=env, capture_output=True, cwd=ROOT)
            calls = [json.loads(line) for line in trace.read_text().splitlines()]
            return result, calls

    def test_lock_and_actual_installer_spend_exact_package_pins_before_admission(self):
        self.assertEqual((ROOT / ".github/ci/docker-engine-packages.lock").read_text().splitlines(), PACKAGES)
        result, calls = self.execute("good")
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        install = next(args for kind, args in calls if kind == "sudo" and any(x.startswith("docker-ce=") for x in args))
        self.assertEqual(install, ["apt-get", "install", "-y", "-q", "--no-install-recommends", "--no-remove", *PACKAGES])
        source = next(args for kind, args in calls if kind == "source")
        self.assertIn("Signed-By: /etc/apt/keyrings/docker.asc " + KEY, source)
        download = next(args for kind, args in calls if kind == "curl")
        downloaded = download[download.index("-o") + 1]
        verified = next(args for kind, args in calls if kind == "gpg")
        trusted = next(args for kind, args in calls if kind == "sudo" and args[:3] == ["install", "-m", "0644"])
        self.assertEqual(verified[-1], downloaded)
        self.assertEqual(trusted, ["install", "-m", "0644", downloaded, "/etc/apt/keyrings/docker.asc"])
        restart = next(i for i, c in enumerate(calls) if c == ["sudo", ["systemctl", "restart", "docker"]])
        version = next(i for i, c in enumerate(calls) if c[0] == "docker" and c[1][:1] == ["version"])
        self.assertLess(restart, version)
        self.assertIn(b"Docker Engine server 29.9.2 satisfies >=29.8.2", result.stdout)

    def test_bad_key_or_failed_download_never_reaches_root_trust(self):
        for mode in ("wrong-key", "extra-key", "duplicate-key", "empty-key", "gpg-fail", "download-fail"):
            with self.subTest(mode=mode):
                result, calls = self.execute(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(k == "sudo" and a[:3] == ["install", "-m", "0644"] for k, a in calls))
                self.assertFalse(any(k == "sudo" and "docker-ce=" in " ".join(a) for k, a in calls))
                downloads = [a for k, a in calls if k == "curl"]
                if downloads:
                    self.assertFalse(Path(downloads[0][downloads[0].index("-o") + 1]).parent.exists())

    def test_install_startup_and_guard_failures_are_fatal(self):
        for mode in ("trust-fail", "apt-fail", "restart-fail", "inactive", "old-server", "query-fail"):
            with self.subTest(mode=mode):
                result, calls = self.execute(mode)
                self.assertNotEqual(result.returncode, 0)
                if mode in ("trust-fail", "apt-fail", "restart-fail", "inactive"):
                    self.assertFalse(any(k == "docker" and a[:1] == ["version"] for k, a in calls))

    def test_wrong_runner_or_remote_daemon_refuses_without_package_mutation(self):
        for mode, env in (("wrong-os", {}), ("wrong-arch", {}), ("remote-context", {}),
                          ("good", {"GITHUB_ACTIONS": "false"}), ("good", {"DOCKER_HOST": "tcp://fixture:2375"})):
            with self.subTest(mode=mode, env=env):
                result, calls = self.execute(mode, env)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(k == "sudo" for k, _ in calls))

    def test_service_processes_start_only_after_admission_and_retain_health_contracts(self):
        script = ROOT / "scripts/ci/start-control-plane-services.sh"
        result, calls = self.execute("good", script=script)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        docker = [args for kind, args in calls if kind == "docker"]
        self.assertEqual(docker[0], ["version", "--format", "{{.Server.Version}}"])
        starts = [args for args in docker if args[:1] == ["run"]]
        self.assertEqual(len(starts), 3)
        for args, name, image, port, health in zip(starts,
                ("postgres", "redis", "nats"), ("postgres:16-alpine", "redis:8-alpine", "nats:2-alpine"),
                (5432, 6379, 4222), ("pg_isready -U concord -d concord_test", "redis-cli ping", "nc -z 127.0.0.1 4222 || exit 1")):
            self.assertEqual(args[args.index("--name") + 1], "concord-ci-" + name)
            self.assertEqual(args[-1], image)
            self.assertIn("127.0.0.1:" + str(port) + ":" + str(port), args)
            self.assertEqual(args[args.index("--health-cmd") + 1], health)
        self.assertIn("POSTGRES_DB=concord_test", starts[0])
        self.assertEqual([args[-1] for args in docker if args[:1] == ["inspect"]],
                         ["concord-ci-postgres", "concord-ci-redis", "concord-ci-nats"])
        result, calls = self.execute("service-starting", script=script)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertEqual(sum(k == "sleep" for k, _ in calls), 3)
        for mode in ("old-server", "query-fail", "service-run-fail", "service-unhealthy",
                     "unhealthy-concord-ci-redis", "unhealthy-concord-ci-nats", "service-never-ready"):
            with self.subTest(mode=mode):
                result, calls = self.execute(mode, script=script)
                self.assertNotEqual(result.returncode, 0)
                if mode in ("old-server", "query-fail"):
                    self.assertFalse(any(k == "docker" and a[:1] == ["run"] for k, a in calls))
                if mode == "service-never-ready":
                    self.assertEqual(sum(k == "sleep" for k, _ in calls), 60)


class JobOrdering(unittest.TestCase):
    JOBS = {
        "build.yml": ("control-plane", "media-plane-docker-warm", "production-compose-docker"),
        "container-image-scan.yml": ("scan",),
        "unit-smoke.yml": ("concord-dev-smoke", "concord-dev-smoke-native"),
        "osv-scanner.yml": ("protected-pr-scan",),
        "zap-api-dast.yml": ("zap-api-dast",),
        "selfhost-acceptance.yml": ("fresh-host-selfhost-acceptance",),
        "selfhost-release-guards.yml": ("selfhost-release-guards", "selfhost-runtime-guards"),
        "docker-runner-inventory.yml": ("inventory",),
    }

    def test_bootstrap_is_fatal_before_job_docker_effects_and_automatic_services(self):
        for filename, jobs in self.JOBS.items():
            workflow = yaml.safe_load((ROOT / ".github/workflows" / filename).read_text())
            for job_id in jobs:
                with self.subTest(workflow=filename, job=job_id):
                    job = workflow["jobs"][job_id]
                    self.assertNotIn("services", job)
                    self.assertNotIn("container", job)
                    steps = job["steps"]
                    installs = [(i, step) for i, step in enumerate(steps)
                                if "scripts/ci/install-docker-engine.sh" in step.get("run", "")]
                    self.assertEqual(len(installs), 1)
                    index, step = installs[0]
                    self.assertNotIn("if", step)
                    self.assertFalse(step.get("continue-on-error", False))
                    self.assertTrue(any(s.get("uses", "").startswith("actions/checkout@") for s in steps[:index]))
                    for prior in steps[:index]:
                        run = prior.get("run", "")
                        self.assertNotRegex(run, r"(?m)^\s*(?:sudo )?docker\s+(?:run|pull|build|buildx|compose)\b")
                    if filename == "osv-scanner.yml":
                        self.assertIn("bash engine-bootstrap/scripts/ci/install-docker-engine.sh", step["run"])
                        bootstrap = next(s for s in steps[:index] if s.get("uses", "").startswith("actions/checkout@"))
                        self.assertEqual(bootstrap["with"]["repository"], "Concord-Voice/Concord-Voice-Alpha")
                        self.assertEqual(bootstrap["with"]["ref"], "${{ github.workflow_sha }}")
                        self.assertEqual(bootstrap["with"]["path"], "engine-bootstrap")
                        self.assertIs(bootstrap["with"]["persist-credentials"], False)
                        self.assertIn(".github/ci", bootstrap["with"]["sparse-checkout"])
                        self.assertIn("scripts/ci", bootstrap["with"]["sparse-checkout"])
                        inputs = [i for i, item in enumerate(steps) if "inputs." in str(item.get("with", {}).get("ref", ""))]
                        self.assertTrue(inputs)
                        self.assertLess(index, min(inputs), "caller-selected code must not precede privileged bootstrap")
                        self.assertEqual(step["env"]["EXPECTED_SHA"], "${{ github.workflow_sha }}")
                        admission_index, admission = next((i, item) for i, item in enumerate(steps)
                            if item.get("name") == "Validate protected workflow bootstrap source")
                        self.assertLess(admission_index, steps.index(bootstrap))
                        self.assertEqual(admission["env"]["CALLER_REF"], "${{ github.workflow_ref }}")
                        self.assertEqual(admission["env"]["EXPECTED_SHA"], "${{ github.workflow_sha }}")
                        self.assertNotIn("if", admission)
                        self.assertNotIn("continue-on-error", admission)

    def test_control_plane_uses_guarded_services_and_cleans_up_on_failure(self):
        job = yaml.safe_load((ROOT / ".github/workflows/build.yml").read_text())["jobs"]["control-plane"]
        start = next(s for s in job["steps"] if "start-control-plane-services.sh" in s.get("run", ""))
        self.assertEqual(start["id"], "ci-services")
        self.assertNotIn("if", start)
        metrics = next(s for s in job["steps"] if s.get("name", "").startswith("Confine operations metrics"))
        self.assertEqual(metrics["env"]["POSTGRES_CONTAINER_ID"], "concord-ci-postgres")
        cleanup = next(s for s in job["steps"] if s.get("name") == "Stop CI service containers")
        self.assertIn("always()", cleanup["if"])
        for name in ("postgres", "redis", "nats"):
            self.assertIn("concord-ci-" + name, cleanup["run"])

    def test_actual_protected_osv_revision_check_stops_before_privileged_bootstrap(self):
        job = yaml.safe_load((ROOT / ".github/workflows/osv-scanner.yml").read_text())["jobs"]["protected-pr-scan"]
        admission = next(s["run"] for s in job["steps"] if s.get("name") == "Validate protected workflow bootstrap source")
        script = next(s["run"] for s in job["steps"] if "bash engine-bootstrap/scripts/ci/install-docker-engine.sh" in s.get("run", ""))
        trusted = "Concord-Voice/Concord-Voice-Alpha/.github/workflows/required-pr-build.yml@refs/heads/main"
        cases = [(trusted, "a" * 40, True), (trusted, "b" * 40, False),
                 (trusted, "", False), (trusted, "A" * 40, False), (trusted, "refs/heads/main", False)]
        cases += [(ref, "a" * 40, False) for ref in
                  (trusted.replace("Concord-Voice-Alpha/", "other/"),
                   trusted.replace("required-pr-build.yml", "attacker.yml"),
                   trusted.replace("refs/heads/main", "refs/pull/4213/merge"),
                   trusted.replace("refs/heads/main", "refs/heads/feature"),
                   trusted.replace("refs/heads/main", "refs/tags/main"), "")]
        for caller, expected, accepted in cases:
            with self.subTest(caller=caller, expected=expected), tempfile.TemporaryDirectory() as temporary:
                p = Path(temporary)
                for tree, value in (("engine-bootstrap", "trusted"), ("scan", "UNTRUSTED")):
                    helper = p / tree / "scripts/ci/install-docker-engine.sh"
                    helper.parent.mkdir(parents=True)
                    helper.write_text("printf " + value + ' > "$PRIVILEGED_EFFECT"\n')
                git = p / "git"
                git.write_text("#!/bin/sh\nprintf '%s\\n' " + "a" * 40 + "\n")
                git.chmod(0o755)
                marker = p / "effect"
                env = {**os.environ, "PATH": str(p) + ":" + os.environ["PATH"],
                       "EXPECTED_SHA": expected, "CALLER_REF": caller, "PRIVILEGED_EFFECT": str(marker)}
                result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", admission + "\n" + script],
                                        env=env, cwd=p, capture_output=True)
                self.assertEqual(result.returncode == 0, accepted, result.stderr.decode())
                self.assertEqual(marker.exists(), accepted)
                if accepted:
                    self.assertEqual(marker.read_text(), "trusted", "scan helper must never execute")

    def test_static_guard_bootstrap_uses_exact_docker_suite_applicability(self):
        job = yaml.safe_load((ROOT / ".github/workflows/pr-ci.yml").read_text())["jobs"]["static-guards"]
        installs = [(i, s) for i, s in enumerate(job["steps"])
                    if s.get("run") == "bash scripts/ci/install-docker-engine.sh"]
        self.assertEqual(len(installs), 1)
        index, install = installs[0]
        suite_index, suite = next((i, s) for i, s in enumerate(job["steps"])
                                  if s.get("name", "").startswith("Run all test-"))
        self.assertLess(index, suite_index)
        self.assertEqual(install["if"], suite["if"])
        self.assertNotIn("continue-on-error", install)
        self.assertEqual(job["permissions"], {"contents": "read"})


if __name__ == "__main__":
    unittest.main(verbosity=2)
