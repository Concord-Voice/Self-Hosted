#!/usr/bin/env python3
"""Exercise the guard's CLI, including planted Docker and runtime mismatches."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


GUARD = Path(__file__).with_name("check-go-toolchain.py")
LICENSE_HOOK = Path(__file__).with_name("check-license-check-go-pin.sh").resolve()


class GoToolchainGuardTest(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.module = self.root / "services/control-plane"
        self.module.mkdir(parents=True)
        (self.module / "go.mod").write_text("module example.test/service\n\ngo 1.26.6\n\ntoolchain go1.27.1\n")
        for filename in ("Dockerfile", "Dockerfile.ops-agent", "Dockerfile.redis-auth-controller", "Dockerfile.turn-secret-writer"):
            (self.module / filename).write_text("FROM golang:1.27.1-alpine@sha256:" + "a" * 64 + " AS builder\n")
        self.workflow = self.root / ".github/workflows/license-check.yml"
        self.workflow.parent.mkdir(parents=True)
        self.workflow.write_text(
            "jobs:\n  check-go-licenses:\n    steps:\n"
            "      - uses: actions/setup-go@" + "a" * 40 + "\n"
            "        with:\n          go-version-file: services/control-plane/go.mod\n"
        )

    def run_guard(self, *args, env=None):
        return subprocess.run(
            [sys.executable, str(GUARD), str(self.root), *args],
            capture_output=True, text=True, env=env, check=False,
        )

    def test_aligned_toolchain_overrides_go_minimum(self):
        result = self.run_guard()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Go 1.27.1", result.stdout)

    def test_go_directive_is_fallback(self):
        (self.module / "go.mod").write_text("module example.test/service\ngo 1.27.1\n")
        self.assertEqual(self.run_guard().returncode, 0)

    def test_planted_mismatch_in_each_dockerfile_fails(self):
        for filename in ("Dockerfile", "Dockerfile.ops-agent", "Dockerfile.redis-auth-controller", "Dockerfile.turn-secret-writer"):
            with self.subTest(filename=filename):
                path = self.module / filename
                original = path.read_text()
                path.write_text(original.replace("1.27.1", "1.26.6"))
                result = self.run_guard()
                self.assertEqual(result.returncode, 1)
                self.assertIn(filename, result.stderr)
                self.assertIn("disagrees", result.stderr)
                path.write_text(original)

    def test_unpinned_or_missing_builder_fails(self):
        for image in ("golang:latest", "golang:1.27-alpine", "golang@sha256:abc", "alpine:3.24"):
            with self.subTest(image=image):
                (self.module / "Dockerfile").write_text(f"FROM {image}\n")
                self.assertEqual(self.run_guard().returncode, 1)

    def test_all_go_stages_are_checked(self):
        path = self.module / "Dockerfile"
        path.write_text(path.read_text() + "FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS tools\n")
        self.assertEqual(self.run_guard().returncode, 1)

    def test_platform_flag_and_comments_are_supported(self):
        (self.module / "Dockerfile").write_text(
            "# FROM golang:1.26.6-alpine\nFROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS builder\n"
        )
        self.assertEqual(self.run_guard().returncode, 0)

    def test_missing_files_fail(self):
        for filename in ("go.mod", "Dockerfile", "Dockerfile.ops-agent", "Dockerfile.redis-auth-controller", "Dockerfile.turn-secret-writer"):
            with self.subTest(filename=filename):
                path = self.module / filename
                original = path.read_text()
                path.unlink()
                self.assertEqual(self.run_guard().returncode, 1)
                path.write_text(original)

    def test_invalid_directives_fail(self):
        for directives, diagnostic in (
            ("", "exactly one go directive"),
            ("go 1.27", "go must name a patch-pinned Go release"),
            ("go 1.26.6\ngo 1.27.1", "exactly one go directive"),
            ("go 1.26.6\ntoolchain default", "toolchain must name a patch-pinned Go release"),
            ("go 1.26.6\ntoolchain go1.27", "toolchain must name a patch-pinned Go release"),
            ("go 1.26.6\ntoolchain go1.26.5", "toolchain must not be older than the go directive"),
            ("go 1.26.6\ntoolchain go1.27.1\ntoolchain go1.27.1", "exactly one toolchain directive"),
        ):
            with self.subTest(directives=directives):
                (self.module / "go.mod").write_text("module example.test/service\n" + directives + "\n")
                result = self.run_guard()
                self.assertEqual(result.returncode, 1)
                self.assertIn(diagnostic, result.stderr)

    def test_license_hook_accepts_selected_module_toolchain(self):
        result = subprocess.run(
            ["bash", str(LICENSE_HOOK)], cwd=self.root, capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Go 1.27.1", result.stdout)

    def test_license_workflow_canonical_input_forms(self):
        original = self.workflow.read_text()
        for value in ("services/control-plane/go.mod", "'services/control-plane/go.mod'", '"services/control-plane/go.mod" # source'):
            with self.subTest(value=value):
                self.workflow.write_text(original.replace("services/control-plane/go.mod", value))
                result = self.run_guard("--check-license-workflow")
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_license_hook_rejects_planted_input_drift(self):
        original = self.workflow.read_text()
        for workflow in (
            original.replace("services/control-plane/go.mod", "other/go.mod"),
            original.replace("go-version-file: services/control-plane/go.mod", "go-version: '1.26.6'"),
            original + "          go-version: '1.27.1'\n",
            original + "          go-version-file: services/control-plane/go.mod\n",
            original.replace("          go-version-file: services/control-plane/go.mod\n", ""),
        ):
            with self.subTest(workflow=workflow):
                self.workflow.write_text(workflow)
                result = subprocess.run(
                    ["bash", str(LICENSE_HOOK)], cwd=self.root, capture_output=True, text=True, check=False,
                )
                self.assertEqual(result.returncode, 1)
                self.assertIn("setup-go must use only go-version-file: services/control-plane/go.mod", result.stderr)

    def test_license_workflow_missing_file_fails(self):
        self.workflow.unlink()
        result = self.run_guard("--check-license-workflow")
        self.assertEqual(result.returncode, 1)
        self.assertIn("license-check.yml", result.stderr)

    def test_license_input_on_unrelated_step_does_not_satisfy_guard(self):
        original = self.workflow.read_text()
        for replacement in (
            "      - uses: actions/checkout@" + "b" * 40 + "\n",
            "  unrelated:\n    steps:\n      - uses: actions/setup-go@" + "b" * 40 + "\n",
        ):
            with self.subTest(replacement=replacement):
                self.workflow.write_text(original.replace(
                    "        with:\n", replacement + "        with:\n",
                ))
                result = self.run_guard("--check-license-workflow")
                self.assertEqual(result.returncode, 1)
                self.assertIn("setup-go must use only go-version-file: services/control-plane/go.mod", result.stderr)

    def test_unrelated_go_consumer_does_not_override_license_job(self):
        self.workflow.write_text(self.workflow.read_text() + (
            "  unrelated:\n    steps:\n      - uses: actions/setup-go@" + "b" * 40 + "\n"
            "        with:\n          go-version: '1.25.0'\n"
        ))
        result = self.run_guard("--check-license-workflow")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_ambiguous_or_malformed_license_setup_fails(self):
        original = self.workflow.read_text()
        for workflow in (
            "jobs: {}\n",
            "jobs: []\n",
            original.replace("steps:", "steps: ["),
            original + original.split("    steps:\n", 1)[1],
        ):
            with self.subTest(workflow=workflow):
                self.workflow.write_text(workflow)
                result = self.run_guard("--check-license-workflow")
                self.assertEqual(result.returncode, 1)
                self.assertIn("setup-go must use only go-version-file: services/control-plane/go.mod", result.stderr)

    def test_runtime_is_printed_and_mismatch_fails(self):
        binary = self.root / "go"
        env = {**os.environ, "PATH": str(self.root) + os.pathsep + os.environ.get("PATH", "")}
        for version, expected_status in (("1.27.1", 0), ("1.26.6", 1)):
            with self.subTest(version=version):
                binary.write_text(f"#!{sys.executable}\nprint('go version go{version} linux/amd64')\n")
                binary.chmod(0o755)
                result = self.run_guard("--check-runtime", env=env)
                self.assertEqual(result.returncode, expected_status, result.stderr)
                self.assertIn(f"go version go{version}", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
