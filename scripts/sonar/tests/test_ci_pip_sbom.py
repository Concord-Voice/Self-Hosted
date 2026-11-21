"""The main-branch Sonar SBOM must reflect the installed Linux CI pins."""

import importlib.util
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "ci-pip-sbom.py"
SPEC = importlib.util.spec_from_file_location("ci_pip_sbom", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class CiPipSbomTests(unittest.TestCase):
    def test_current_linux_pins_have_package_urls(self):
        components = MODULE.components()
        pins = {item["name"]: item["version"] for item in components}
        self.assertEqual(len(components), len(pins))
        self.assertEqual(pins["pyyaml"], "6.0.3")
        self.assertEqual(pins["squawk-cli"], "2.71.0")
        self.assertEqual(pins["semgrep"], "1.186.0")
        self.assertNotIn("pywin32", pins)
        for item in components:
            self.assertEqual(item["purl"], f"pkg:pypi/{item['name']}@{item['version']}")

    def test_unknown_marker_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "requirements.txt").write_text("example==1.0 ; python_version < '3.12' \\\n")
            old_root, old_manifests = MODULE.ROOT, MODULE.MANIFESTS
            try:
                MODULE.ROOT, MODULE.MANIFESTS = root, ("requirements.txt",)
                with self.assertRaisesRegex(ValueError, "unsupported CI Pip pin"):
                    MODULE.components()
            finally:
                MODULE.ROOT, MODULE.MANIFESTS = old_root, old_manifests


if __name__ == "__main__":
    unittest.main()
