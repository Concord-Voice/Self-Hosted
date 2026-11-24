#!/usr/bin/env python3
"""Check the Self-Hosted protected scan wrapper and CodeQL config reference."""
from pathlib import Path
import re
import yaml

root = Path(__file__).resolve().parents[3]

def workflow(name):
    return yaml.load((root / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)

required = workflow("required-pr-build.yml")
assert required["on"]["pull_request"]["branches"] == ["__ruleset_only__"]
assert "concurrency" not in required
jobs = required["jobs"]
pr = jobs["pr-ci"]
osv = jobs["osv"]
for job, path in ((pr, "pr-ci.yml"), (osv, "osv-scanner.yml")):
    assert re.fullmatch(
        rf"Concord-Voice/Self-Hosted/\.github/workflows/{path}@[0-9a-f]{{40}}",
        job["uses"],
    ), f"{path} must use a full Self-Hosted commit pin"
assert pr["with"] == {"trusted-source": "true", "enforcement-only": "true"}
assert "secrets" not in pr, "pr-ci.yml declares no secrets to receive"
assert osv["needs"] == ["pr-ci"]
assert "always()" in osv["if"] and '"osv":true' in osv["if"]
assert jobs["publish-install-script-warnings"]["needs"] == ["pr-ci"]
assert jobs["publish-install-script-warnings"]["if"] == "${{ always() }}"

codeql = workflow("codeql.yml")
init = next(s for s in codeql["jobs"]["analyze"]["steps"] if s.get("name") == "Initialize CodeQL")
config_ref = init["with"]["config-file"]
assert re.fullmatch(
    r"remote=Concord-Voice/Self-Hosted@main:.github/codeql/codeql-config.yml",
    config_ref,
), "CodeQL config must come from protected main, as in Alpha"
config = yaml.safe_load((root / ".github/codeql/codeql-config.yml").read_text())
assert config["queries"][0]["uses"] == "./.github/codeql/queries/go/LogInjectionUnstructured.ql"
assert ".github/codeql/**" in (root / "CODEOWNERS").read_text()
codeowners = [line.split() for line in (root / "CODEOWNERS").read_text().splitlines()
              if line.strip() and not line.lstrip().startswith("#")]
assert ["/scripts/ci/", "@Concord-Voice/security"] in codeowners
assert all(owners == ["@Concord-Voice/security"] for pattern, *owners in codeowners
           if pattern.lstrip("/").startswith("scripts/ci")), "a later rule re-owns scripts/ci"
# CODEOWNERS matches case-sensitively, so a case variant of an owned path gets no owner, and on a
# case-insensitive checkout it lands on top of the reviewed file. Refuse any case collision.
import subprocess
tracked = subprocess.run(["git", "ls-files", "-z"], cwd=root, check=True, capture_output=True).stdout.decode().split("\0")
folded = {}
for path in filter(None, tracked):
    folded.setdefault(path.casefold(), []).append(path)
collisions = [paths for paths in folded.values() if len(paths) > 1]
assert not collisions, f"case-colliding tracked paths: {collisions}"
print("Self-Hosted protected workflow checks passed")
