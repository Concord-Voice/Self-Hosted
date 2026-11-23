"""Exercise Semgrep target filtering with the installed dependency lock."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
SEMGREP = Path(sys.executable).parent / "semgrep"


class SemgrepCompatibilityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="semgrep-compat-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / ".semgrepignore").write_text("# Intentionally empty\n")
        self.environment = os.environ.copy()
        self.environment["PATH"] = str(SEMGREP.parent) + os.pathsep + os.defpath
        self.environment["SEMGREP_SEND_METRICS"] = "off"
        self.environment["PYTHONSAFEPATH"] = "1"
        self.environment.pop("PYTHONPATH", None)
        paths = (
            "targets/root.go",
            "targets/root_test.go",
            "targets/pkg/app.go",
            "targets/pkg/app_test.go",
            "targets/pkg/testhelpers/helper.go",
            "targets/pkg/tests/fixtures/sample.go",
            "targets/pkg/fixtures/production.go",
            "targets/scripts/fixtures/sample.go",
        )
        for path in paths:
            target = self.root / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(
                'package probe\nimport "net/http"\n'
                'func f() { dangerous(); http.ListenAndServe(":8080", nil) }\n'
            )

        rule = (
            "rules:\n"
            "  - id: probe\n"
            "    languages: [go]\n"
            "    message: probe\n"
            "    severity: WARNING\n"
            "    pattern: dangerous(...)\n"
        )
        (self.root / "plain.yml").write_text(rule)
        (self.root / "paths.yml").write_text(
            rule
            + "    paths:\n"
            + "      include: ['**/*.go']\n"
            + "      exclude: ['**/*_test.go', '**/testhelpers/**']\n"
        )

    def scan(self, config, excludes=()):
        args = [
            str(SEMGREP),
            "scan",
            "--config",
            str(config),
            "--json",
            "--quiet",
            "--metrics=off",
            "--disable-version-check",
            "--no-git-ignore",
            "--project-root=.",
        ]
        for pattern in excludes:
            args.extend(["--exclude", pattern])
        args.append("targets")
        result = subprocess.run(
            args, cwd=self.root, env=self.environment, text=True,
            capture_output=True, check=True, timeout=60,
        )
        data = json.loads(result.stdout)
        self.assertEqual(data["errors"], [])
        return sorted(item["path"] for item in data["results"])

    def test_rule_paths_and_repository_rule_filter_targets(self):
        expected = [
            "targets/pkg/app.go",
            "targets/pkg/fixtures/production.go",
            "targets/pkg/tests/fixtures/sample.go",
            "targets/root.go",
            "targets/scripts/fixtures/sample.go",
        ]
        self.assertEqual(self.scan(self.root / "paths.yml"), expected)
        self.assertEqual(
            self.scan(ROOT / ".semgrep/concord-go-security.yml"), expected
        )

    def test_cli_exclusions_keep_non_fixture_production_files(self):
        self.assertEqual(
            self.scan(
                self.root / "plain.yml",
                ("*_test.go", "**/tests/fixtures", "/targets/scripts/fixtures"),
            ),
            [
                "targets/pkg/app.go",
                "targets/pkg/fixtures/production.go",
                "targets/pkg/testhelpers/helper.go",
                "targets/root.go",
            ],
        )

    def test_rule_globs_match_representative_paths(self):
        from semgrep.target_manager import TargetManager

        cases = (
            ("src/app_test.go", "**/*_test.go", True),
            ("src/app.go", "**/*_test.go", False),
            ("src/testhelpers/helper.go", "**/testhelpers/**", True),
            ("src/fixtures/app.go", "**/testhelpers/**", False),
            ("packages/core/tests/input.ts", "packages/*/tests/**", True),
            ("packages/core/src/input.ts", "packages/*/tests/**", False),
            (".hidden/input.test.ts", "**/*.test.*", True),
            ("src/input.ts", "**/*.test.*", False),
        )
        for path, pattern, expected in cases:
            with self.subTest(path=path, pattern=pattern):
                self.assertEqual(TargetManager._globmatch(path, pattern), expected)

    def test_multistar_pattern_rejection_is_bounded(self):
        expression = (
            "from wcmatch import glob; "
            "assert not glob.globmatch('uploads/' + '_' * 200, "
            "'uploads/*_*_*_*_*.csv', flags=glob.GLOBSTAR | glob.DOTGLOB)"
        )
        subprocess.run(
            [sys.executable, "-c", expression], cwd=self.root,
            env=self.environment, check=True, timeout=3,
        )


if __name__ == "__main__":
    unittest.main()
