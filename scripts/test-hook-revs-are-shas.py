#!/usr/bin/env python3
"""Exercise the actual pre-commit rev guard against accepted and rejected lines."""

import re
import unittest
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / ".pre-commit-config.yaml"
SHA = "0123456789abcdef" * 2 + "01234567"


def configured_guard() -> re.Pattern[str]:
    config = yaml.safe_load(CONFIG.read_text())
    hooks = [hook for repo in config["repos"] for hook in repo.get("hooks", [])
             if hook.get("id") == "hook-revs-are-shas"]
    if len(hooks) != 1 or hooks[0].get("language") != "pygrep":
        raise AssertionError("expected one active pygrep hook-revs-are-shas guard")
    if not re.search(hooks[0]["files"], ".pre-commit-config.yaml"):
        raise AssertionError("rev guard does not run on its own configuration")
    return re.compile(hooks[0]["entry"])


class HookRevGuardTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.guard = configured_guard()

    def test_valid_commit_sha_with_frozen_comment_is_accepted(self):
        # Existing public hook commits use this strict frozen-comment shape.
        accepted = (
            f"    rev: {SHA}  # frozen: v1.5.0",
            f"rev: {SHA} # frozen: v1.5.0",
            f"\trev:\t{SHA}\t# frozen: v1.5.0",
        )
        for line in accepted:
            with self.subTest(line=line):
                self.assertIsNone(self.guard.search(line), "valid frozen SHA was rejected")

        # Pin the real configured lines too; this is what the normal hook reads.
        for line in CONFIG.read_text().splitlines():
            if re.match(r"^[ \t]*rev:", line):
                with self.subTest(config_line=line):
                    self.assertIsNone(self.guard.search(line), "configured hook revision was rejected")

    def test_tag_malformed_or_unfrozen_rev_is_rejected(self):
        rejected = (
            "    rev: v1.5.0  # frozen: v1.5.0",
            f"    rev: {SHA[:-1]}  # frozen: v1.5.0",
            f"    rev: {SHA}0  # frozen: v1.5.0",
            f"    rev: {SHA[:-1]}g  # frozen: v1.5.0",
            f"    rev: {SHA.upper()}  # frozen: v1.5.0",
            f"    rev: {SHA}",
            f"    rev: {SHA}  # frozen:",
            f"    rev: {SHA}  # frozen: v1.5.0  # unexpected suffix",
            f"    rev: {SHA}  # frozen: v1.5.0  # pragma: allowlist secret",
            f"    rev: {SHA}  # frozen: v1.5.0  # pragma: allowlist secrets",
        )
        for line in rejected:
            with self.subTest(line=line):
                self.assertIsNotNone(self.guard.search(line), "invalid hook revision escaped guard")


if __name__ == "__main__":
    unittest.main()
