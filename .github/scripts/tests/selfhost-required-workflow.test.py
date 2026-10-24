#!/usr/bin/env python3
"""Check the Self-Hosted protected scan wrapper during the bootstrap port."""
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
assert pr["secrets"] == {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"}
assert osv["needs"] == ["pr-ci"]
assert "always()" in osv["if"] and '"osv":true' in osv["if"]
assert jobs["publish-install-script-warnings"]["needs"] == ["pr-ci"]
assert jobs["publish-install-script-warnings"]["if"] == "${{ always() }}"

codeql = workflow("codeql.yml")
init = next(s for s in codeql["jobs"]["analyze"]["steps"] if s.get("name") == "Initialize CodeQL")
config_ref = init["with"]["config-file"]
assert re.fullmatch(
    r"remote=Concord-Voice/Self-Hosted@[0-9a-f]{40}:.github/codeql/codeql-config.yml",
    config_ref,
), "CodeQL config must use an immutable bootstrap commit"
config = yaml.safe_load((root / ".github/codeql/codeql-config.yml").read_text())
assert config["queries"][0]["uses"] == "./.github/codeql/queries/go/LogInjectionUnstructured.ql"
assert ".github/codeql/**" in (root / "CODEOWNERS").read_text()
print("Self-Hosted protected workflow bootstrap checks passed")
