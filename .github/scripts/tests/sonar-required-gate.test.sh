#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
if [[ ! -f "$ROOT/.github/workflows/sonarcloud.yml" ]]; then
  # F7: the Sonar workflow and Alpha ruleset appliers are still outside this
  # split checkout. Keep the shared scan and required-workflow suites active.
  python3 "$ROOT/.github/scripts/tests/test_sonar_ioc_conditions.py"
  python3 "$ROOT/.github/scripts/tests/test_codeql_required_workflow.py"
  python3 "$ROOT/.github/scripts/tests/test_ci_scan_verdict.py"
  python3 "$ROOT/.github/scripts/tests/test_verify_required_build_pins.py"
  bash "$ROOT/.github/scripts/tests/resolve-hotfix-label.test.sh"
  exit 0
fi
python3 "$ROOT/.github/scripts/tests/test_sonar_ioc_conditions.py"
python3 "$ROOT/.github/scripts/tests/test_codeql_required_workflow.py"
# Self-Hosted: test_ruleset_hardening.py covers Alpha's ruleset files under
# infrastructure/github, which stay with Alpha.
python3 -m unittest discover -s "$ROOT/.github/scripts/tests" -p test_ci_scan_verdict.py
python3 -m unittest discover -s "$ROOT/.github/scripts/tests" -p test_verify_required_build_pins.py
bash "$ROOT/.github/scripts/tests/resolve-hotfix-label.test.sh"
python3 - "$ROOT" <<'PY'
import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

root = Path(sys.argv[1])
relevance_apply = root / "infrastructure/github/apply-alpha-scan-relevance.sh"
assert relevance_apply.is_file(), (
    "guarded scan relevance migration must exist before its hermetic safety cases run")
pr_ci = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
assert "sonar" not in pr_ci["jobs"] and "sonar-profile-drift" not in pr_ci["jobs"]
assert "SONAR_TOKEN" not in json.dumps(pr_ci), "ordinary PR source must not receive Sonar credentials"
# Only the shared scanner and its explicit fail-closed credential check receive
# SONAR_TOKEN. The protected dispatch prepares inert PR data before either step.
sonar_workflow = yaml.load((root / ".github/workflows/sonarcloud.yml").read_text(), Loader=yaml.BaseLoader)
assert not {"env", "defaults"} & sonar_workflow.keys(), sonar_workflow.keys()
scanner = sonar_workflow["jobs"]["sonarcloud"]
assert not {"env", "container", "services", "defaults", "uses"} & scanner.keys()
steps = scanner["steps"]
scan = next(step for step in steps if step.get("name") == "SonarCloud Scan")
credential = next(step for step in steps if step.get("name") == "Require Sonar credential")
prepare = next(step for step in steps if step.get("name") == "Verify caller proof and prepare inert PR source")
assert steps.index(prepare) < steps.index(credential) < steps.index(scan)
assert credential["env"] == scan["env"] == {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"}
assert all("secrets.SONAR_TOKEN" not in json.dumps(step)
           for step in steps if step not in (credential, scan))
assert json.dumps(sonar_workflow).count("secrets.SONAR_TOKEN") == 2
assert re.fullmatch(r"SonarSource/sonarqube-scan-action@[0-9a-f]{40}", scan["uses"])
assert "sonar.projectBaseDir" in scan["with"]["args"]
assert "sonar.qualitygate.wait=true" in scan["with"]["args"]
semgrep_workflow = yaml.load((root / ".github/workflows/semgrep.yml").read_text(), Loader=yaml.BaseLoader)
semgrep_publish = next(step for step in semgrep_workflow["jobs"]["semgrep"]["steps"]
                       if step.get("name") == "Publish Semgrep results to PR")
assert semgrep_publish["if"] == "always()"
assert semgrep_publish["continue-on-error"] == "${{ inputs.external-verdict == true }}"
owner_job = pr_ci["jobs"]["detect-changes"]
owner_step = next(step for step in owner_job["steps"] if step.get("id") == "protected-owner")
assert owner_job["outputs"]["protected-owner"] == "${{ steps.protected-owner.outputs.confirmed }}"
for job_name in ("build", "semgrep", "eslint", "govulncheck"):
    assert "inputs.enforcement-only || needs.detect-changes.outputs.protected-owner != 'true'" in pr_ci["jobs"][job_name]["if"], job_name
assert "codeql" not in pr_ci["jobs"], "CodeQL is enforced by GitHub's native ruleset, not the protected job inventory"
assert "needs.pr-ci.result == 'success'" in yaml.load(
    (root / ".github/workflows/required-pr-build.yml").read_text(), Loader=yaml.BaseLoader
)["jobs"]["protected-sonar"]["if"]
with tempfile.TemporaryDirectory(prefix="pr-ci-owner-") as temporary:
    tmp = Path(temporary)
    gh = tmp / "gh"
    gh.write_text('#!/bin/sh\n[ "$GH_FAIL" != true ] || exit 1\ncat "$RULES_FIXTURE"\n')
    gh.chmod(0o755)
    rules = tmp / "rules.json"
    output = tmp / "output"
    active = [{"type": "workflows", "parameters": {"workflows": [{
        "repository_id": 1158152129, "path": ".github/workflows/required-pr-build.yml", "ref": "main"}]}}]
    for mode, hotfix, fixture, failure, expected in (
        ("legacy", False, active, False, False),
        ("required-full", False, active, False, True),
        ("required-relevant", False, active, False, True),
        ("required-relevant", True, active, False, True),
        ("required-full", False, [], False, False),
        ("required-relevant", False, active, True, False),
    ):
        rules.write_text(json.dumps(fixture))
        output.unlink(missing_ok=True)
        selection = {"known": True, "hotfix": hotfix, "mode": mode}
        env = {**os.environ, "MODE": mode, "SELECTION": json.dumps(selection),
               "GH_FAIL": "true" if failure else "false", "RULES_FIXTURE": str(rules),
               "GITHUB_OUTPUT": str(output), "RUNNER_TEMP": str(tmp),
               "GITHUB_REPOSITORY": "Concord-Voice/Self-Hosted",
               "PATH": f"{tmp}:{os.environ['PATH']}"}
        run = subprocess.run(["bash", "-c", owner_step["run"]], env=env,
                             capture_output=True, text=True)
        assert run.returncode == 0, (mode, hotfix, run.stderr)
        assert output.read_text().splitlines()[-1] == f"confirmed={str(expected).lower()}", (
            mode, hotfix, failure, run.stderr, output.read_text())
workflow = yaml.load(
    (root / ".github/workflows/required-pr-build.yml").read_text(),
    Loader=yaml.BaseLoader,
)
codeql_workflow = yaml.load((root / ".github/workflows/codeql.yml").read_text(), Loader=yaml.BaseLoader)
codeql_init = next(step for step in codeql_workflow["jobs"]["analyze"]["steps"]
                   if step.get("name") == "Initialize CodeQL")
codeql_config = yaml.load((root / ".github/codeql/codeql-config.yml").read_text(), Loader=yaml.BaseLoader)
codeowners = (root / "CODEOWNERS").read_text()
assert codeql_init["with"]["config-file"] == "remote=Concord-Voice/Self-Hosted@main:.github/codeql/codeql-config.yml", (
    "CodeQL configuration must come from protected main, not the PR checkout")
assert codeql_config["queries"][0]["uses"] == "Concord-Voice/Self-Hosted/.github/codeql/queries/go/LogInjectionUnstructured.ql@main", (
    "the custom CodeQL query must come from protected main")
assert any(line.split()[:1] == [".github/codeql/**"] and
           line.split()[1:] == ["@Concord-Voice/security"]
           for line in codeowners.splitlines()), "CodeQL configuration needs security-team ownership"
osv = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
assert set(osv["on"]) == {"pull_request", "schedule", "push", "workflow_call"}, "standalone and protected reusable OSV triggers stay intact"
selection_job = osv["jobs"]["select-osv"]
hotfix_step = next(step for step in selection_job["steps"] if step.get("id") == "hotfix")
assert hotfix_step["if"] == "github.event_name == 'push'"
assert hotfix_step["env"]["SHA"] == "${{ github.sha }}"
assert hotfix_step["env"]["COMMIT_SUBJECT"] == "${{ github.event.head_commit.message }}"
assert "resolve-hotfix-label.sh" in hotfix_step["run"]
metadata_step = next(step for step in selection_job["steps"] if step.get("name") == "Read event metadata")
assert metadata_step.get("continue-on-error") == "true"
assert "bash .github/scripts/validate-osv-push-provenance.sh" in metadata_step["run"]
assert metadata_step["env"]["PR_NUMBER"] == "${{ steps.hotfix.outputs.pr_number }}"
assert metadata_step["env"]["EVENT_PR_NUMBER"] == "${{ github.event.pull_request.number }}"
assert "valid=false" in metadata_step["run"]
assert "schedule" in metadata_step["run"] and "head_sha" in metadata_step["run"]
selection_step = next(step for step in selection_job["steps"] if step.get("id") == "selection")
assert selection_step["env"]["METADATA_VALID"] == "${{ steps.metadata.outputs.valid }}"
selection_run = selection_step["run"]
assert selection_run.index("echo 'known=false'") < selection_run.index('if [[ "$METADATA_VALID" != true || -z "$POLICY_SHA"')
assert selection_run.index('if [[ "$METADATA_VALID" != true || -z "$POLICY_SHA"') < selection_run.index("node policy/.github/scripts/ci-scan-relevance.mjs")
with tempfile.TemporaryDirectory() as temporary:
    tmp = Path(temporary)
    fixture = tmp / "pr.json"
    output = tmp / "metadata-output"
    gh = tmp / "gh"
    gh.write_text('#!/bin/sh\ncat "$PR_FIXTURE"\n')
    gh.chmod(0o755)
    valid_pr = {"base": {"ref": "main", "sha": "b" * 40},
                "head": {"sha": "a" * 40}, "merge_commit_sha": "c" * 40,
                "changed_files": 3, "labels": []}
    invalid_labels = [
        ("missing labels", {key: value for key, value in valid_pr.items() if key != "labels"}),
        ("null labels", {**valid_pr, "labels": None}),
        ("non-array labels", {**valid_pr, "labels": "risk: hotfix"}),
        ("missing label name", {**valid_pr, "labels": [{}]}),
        ("null label name", {**valid_pr, "labels": [{"name": None}]}),
        ("non-string label name", {**valid_pr, "labels": [{"name": 17}]}),
    ]
    if os.environ.get("CI_OSV_METADATA_FOCUS") != "hotfix-type":
        label_failures = []
        for label, pr in [("valid empty labels", valid_pr), *invalid_labels]:
            fixture.write_text(json.dumps(pr))
            output.unlink(missing_ok=True)
            run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", metadata_step["run"]],
                cwd=root, env={**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}",
                    "EVENT_NAME": "pull_request", "EVENT_PR_NUMBER": "42",
                    "GITHUB_REPOSITORY": "owner/repo", "RUNNER_TEMP": str(tmp),
                    "PR_FIXTURE": str(fixture), "GITHUB_OUTPUT": str(output)},
                capture_output=True, text=True)
            emitted = output.read_text().splitlines() if output.exists() else []
            if label == "valid empty labels":
                if not (run.returncode == 0 and "valid=true" in emitted and "hotfix=false" in emitted):
                    label_failures.append((label, run.returncode, emitted, run.stderr))
                continue
            if not (run.returncode == 0 and emitted == ["valid=false"]):
                label_failures.append((label, run.returncode, emitted, run.stderr))
        assert not label_failures, (
            "OSV PR metadata must reject malformed label shape atomically while accepting an empty array: "
            f"{label_failures}")

    # Exercise the exact selection step for absent and non-boolean HOTFIX values.
    keys_match = re.search(r"const keys = \[(.*?)\];", selection_run, re.S)
    assert keys_match, "could not extract the real OSV output contract"
    keys = re.findall(r"'([a-z0-9_]+)'", keys_match.group(1))
    filters_json = json.dumps({key: "false" for key in keys})
    policy_script = tmp / "policy/.github/scripts/ci-scan-relevance.mjs"
    policy_script.parent.mkdir(parents=True)
    policy_script.write_bytes((root / ".github/scripts/ci-scan-relevance.mjs").read_bytes())
    policy_map = tmp / "policy/.github/ci/scan-relevance.yml"
    policy_map.parent.mkdir(parents=True)
    policy_map.write_bytes((root / ".github/ci/scan-relevance.yml").read_bytes())
    runner_tmp = tmp / "runner"
    runner_tmp.mkdir()
    for hotfix in (None, "yes"):
        output.unlink(missing_ok=True)
        env = {**os.environ, "EVENT_NAME": "pull_request", "FILTER_OUTCOME": "success", "METADATA_VALID": "true",
            "PR_HEAD": "a" * 40, "PR_BASE": "b" * 40, "PR_MERGE": "c" * 40,
            "CHANGED_FILES": "3", "MODE": "required-relevant", "POLICY_SHA": "d" * 40,
            "FILTERS": filters_json, "RUNNER_TEMP": str(runner_tmp), "GITHUB_OUTPUT": str(output)}
        if hotfix is not None:
            env["HOTFIX"] = hotfix
        else:
            env.pop("HOTFIX", None)
        run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", selection_run],
            cwd=tmp, env=env, capture_output=True, text=True)
        assert run.returncode == 0, (hotfix, run)
        known_values = [output.read_text().splitlines()[index + 1]
            for index, line in enumerate(output.read_text().splitlines())
            if line.startswith("known<<") and index + 1 < len(output.read_text().splitlines())]
        assert known_values and known_values[-1] == "false", (
            f"absent/non-boolean Hotfix metadata must remain unknown, got {known_values}: {output.read_text()}")
push_validator = root / ".github/scripts/validate-osv-push-provenance.sh"
assert push_validator.exists(), "OSV push relevance uses its dedicated provenance validator"
with tempfile.TemporaryDirectory() as temporary:
    tmp = Path(temporary)
    fake_gh = tmp / "gh"
    fake_gh.write_text('''#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
if os.environ.get("GH_FAIL") == "true": raise SystemExit(1)
url = next((arg for arg in args if arg.startswith("repos/")), "")
if url.endswith("/pulls/42"):
    print(Path(os.environ["PR_JSON"]).read_text())
elif url.endswith("/commits/" + os.environ["AFTER_SHA"] + "/pulls"):
    if os.environ.get("ASSOC_FAIL") == "true": raise SystemExit(1)
    for item in json.loads(Path(os.environ["ASSOC_JSON"]).read_text()):
        print(json.dumps(item))
else:
    raise SystemExit("unexpected gh call")
''')
    fake_gh.chmod(0o755)
    after_sha = "a" * 40
    before_sha = "b" * 40
    base_pr = {"number": 42, "state": "closed", "merged": True,
               "merged_at": "2026-10-01T00:00:00Z", "base": {"ref": "main"},
               "merge_commit_sha": after_sha}

    def validate_push(label, pr=None, associations=None, via="subject", resolution="no-label",
                      hotfix="false", gh_fail=False, assoc_fail=False, valid=False,
                      pr_number="42", before=before_sha, after=after_sha):
        pr_path, assoc_path = tmp / "pr.json", tmp / "associations.json"
        pr_path.write_text(json.dumps(base_pr if pr is None else pr))
        assoc_path.write_text(json.dumps([] if associations is None else associations))
        output = tmp / "github-output"
        output.unlink(missing_ok=True)
        result = subprocess.run(
            ["bash", str(push_validator)],
            env={**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}",
                 "GITHUB_REPOSITORY": "Concord-Voice/Self-Hosted",
                 "BEFORE_SHA": before, "AFTER_SHA": after,
                 "RESOLVED_VIA": via, "HOTFIX_RESOLUTION": resolution,
                 "HOTFIX": hotfix, "PR_NUMBER": pr_number, "PR_JSON": str(pr_path),
                 "ASSOC_JSON": str(assoc_path), "GH_FAIL": str(gh_fail).lower(),
                 "ASSOC_FAIL": str(assoc_fail).lower(), "GITHUB_OUTPUT": str(output)},
            capture_output=True, text=True)
        assert result.returncode == 0, (label, result)
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
        assert values["valid"] == ("true" if valid else "false"), (label, values)
        selection_run = subprocess.run(
            ["node", "--input-type=module", "-e", '''
              import fs from 'node:fs';
              import { composeSelection, ANALYSIS_KEYS, COMPONENT_KEYS, ADVISORY_KEYS,
                WORKER_KEYS, NPM_LICENSE_KEYS } from './.github/scripts/ci-scan-relevance.mjs';
              const data = JSON.parse(process.env.PUSH_METADATA);
              const metadata = { event_name: 'push', before: data.before || 'b'.repeat(40),
                after: data.after || 'a'.repeat(40), hotfix: data.hotfix === 'true',
                history_available: data.valid === 'true' };
              const keys = [...ANALYSIS_KEYS, ...COMPONENT_KEYS,
                ...ADVISORY_KEYS.map(key => key === 'selfhost' ? 'selfhost_guard' : key),
                ...WORKER_KEYS, ...NPM_LICENSE_KEYS, 'license_go', 'all_changed'];
              const filters = Object.fromEntries(keys.map(key => [key, 'false']));
              const result = composeSelection(filters, metadata,
                { mode: 'required-relevant', policySha: 'd'.repeat(40) });
              process.stdout.write(JSON.stringify(result));
            '''.replace("              ", "")],
            env={**os.environ, "PUSH_METADATA": json.dumps(values)},
            capture_output=True, text=True)
        assert selection_run.returncode == 0, (label, selection_run)
        selection = json.loads(selection_run.stdout)
        assert selection["known"] is valid, (label, selection)
        if valid:
            assert selection["selected"]["osv"] is (hotfix == "true"), (label, selection)
        if valid:
            assert values["after"] == after_sha and values["pr_number"] == "42", (label, values)
            assert values["hotfix"] == hotfix, (label, values)
        else:
            assert "after" not in values and values["hotfix"] == "false", (label, values)

    validate_push("valid ordinary merged main PR", valid=True)
    validate_push("valid Hotfix merged main PR", resolution="subject", hotfix="true", valid=True)
    validate_push("wrong base", {**base_pr, "base": {"ref": "release"}})
    validate_push("unmerged PR", {**base_pr, "state": "open", "merged": False, "merged_at": None})
    validate_push("stale merge SHA", {**base_pr, "merge_commit_sha": "c" * 40})
    validate_push("missing PR API result", gh_fail=True)
    validate_push("malformed PR metadata", {"number": "42"})
    validate_push("missing PR number", pr_number="")
    validate_push("malformed after SHA", after="not-a-sha")
    association_pr = {**base_pr, "number": 42}
    validate_push("valid exact association", associations=[association_pr], via="association",
                  resolution="association", hotfix="true", valid=True)
    validate_push("ambiguous association", associations=[association_pr, association_pr],
                  via="association", resolution="association", hotfix="true")
    validate_push("association API error", via="association", resolution="association",
                  hotfix="true", assoc_fail=True)
assert "fail-on-vuln" not in osv["jobs"]["scan-scheduled"].get("with", {}), (
    "push and scheduled scans retain the reusable's blocking default")
pr_osv = osv["jobs"]["scan-pr"]["with"].get("fail-on-vuln", "")
policy_step = next(step for step in selection_job["steps"] if step.get("id") == "policy")
assert "delegate" not in selection_job.get("outputs", {})
assert all(step.get("id") != "delegate" for step in selection_job["steps"])
assert "fail-on-vuln=true" in policy_step["run"], (
    "standalone scans stay strict, including when mode or rule/API state is unknown")
with tempfile.TemporaryDirectory() as temporary:
  policy_output = Path(temporary) / "github-output"
  for event, mode, hotfix, expected in [
    ("pull_request", "required-full", False, "true"),
    ("pull_request", "required-relevant", False, "true"),
    ("pull_request", "legacy", False, "true"),
    ("pull_request", "required-full", True, "true"),
    ("push", "required-full", False, "true"),
    ("schedule", "required-relevant", False, "true"),
  ]:
    run = subprocess.run(
        ["bash", "-e", "-o", "pipefail", "-c", policy_step["run"]],
        env={**os.environ, "EVENT_NAME": event, "MODE": mode,
             "SELECTION": json.dumps({"known": True, "hotfix": hotfix,
                                      "mode": mode,
                                      "selected": {"osv": True}}),
             "GITHUB_OUTPUT": str(policy_output)},
        capture_output=True, text=True,
    )
    assert run.returncode == 0, (event, mode, hotfix, run)
    assert f"fail-on-vuln={expected}" in policy_output.read_text()
    policy_output.unlink()
assert "&& false || true" not in pr_osv and "&& false || true" not in pr_osv.lower(), (
    "false delegation cannot be collapsed by a true fallback")
assert "osv-scanner-reusable-pr.yml@b1d267aea5211fa648d9d442fa64b90dbdcbdd36" in osv["jobs"]["scan-pr"]["uses"]
scheduled_if = osv["jobs"]["scan-scheduled"]["if"]
assert "github.event_name == 'schedule'" in scheduled_if
assert "needs.select-osv.outputs.known != 'true'" in scheduled_if
assert "selected.osv" in scheduled_if
scan_pr_if = osv["jobs"]["scan-pr"]["if"]
assert "needs.select-osv.outputs.known != 'true'" in scan_pr_if and "selected.osv" in scan_pr_if
assert "delegate" not in scan_pr_if
assert "vars.CI_SCAN_RELEVANCE_MODE == 'required-full'" in scan_pr_if
assert "vars.CI_SCAN_RELEVANCE_MODE == 'required-relevant'" in scan_pr_if
pr_ci = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
static_guards = pr_ci["jobs"]["static-guards"]
assert "advisory.static_guards" in static_guards["if"]
assert "scan-selection).hotfix" in static_guards["if"]
assert "needs.detect-changes.outputs.scan-known != 'true'" in static_guards["if"]
workflow_lint = pr_ci["jobs"]["workflow-lint"]
assert "advisory.workflow_lint" in workflow_lint["if"]
assert "scan-selection).hotfix" in workflow_lint["if"]
assert "needs.detect-changes.outputs.scan-known != 'true'" in workflow_lint["if"]
assert not any(step.get("id") == "filter" for step in workflow_lint["steps"]), (
    "workflow-lint must use the shared selector without a duplicate dorny path filter")
assert not any("steps.filter.outputs.workflows" in step.get("if", "")
               for step in workflow_lint["steps"]), (
    "substantive workflow-lint steps must not fast-exit behind a second path filter")
assert any(step.get("name") == "Test suites are both invoked and reachable by their filter (#3139)"
           for step in workflow_lint["steps"]), (
    "the shared workflow-lint job must reach its substantive suite-wiring lint")
unit_call = pr_ci["jobs"]["unit-smoke"]
assert unit_call["with"]["dev-checks-relevant"].find("advisory.dev_checks") >= 0
assert "scan-selection).hotfix" in unit_call["if"]
assert "scan-selection).hotfix" in unit_call["with"]["dev-checks-relevant"]
assert "needs.detect-changes.outputs.scan-known != 'true'" in unit_call["with"]["dev-checks-relevant"]
unit_smoke = yaml.load((root / ".github/workflows/unit-smoke.yml").read_text(), Loader=yaml.BaseLoader)
assert unit_smoke["jobs"]["dev-checks"]["if"] == "${{ inputs.dev-checks-relevant }}"
advisory_families = ["go_toolchain", "openapi", "spa_env", "deployment", "selfhost",
    "feature_wiring", "adr_numbers", "secrets_contract", "dispatch_trust",
    "go_test_contracts", "review_contracts", "tooling_mirrors", "graphify_contracts",
    "native_ioc", "sonar_profile", "changelog"]
step_conditions = [step.get("if", "") for job in pr_ci["jobs"].values()
                   for step in job.get("steps", [])]
for family in advisory_families:
    consumers = [condition for condition in step_conditions if f"advisory.{family}" in condition]
    assert consumers, f"no step consumes advisory.{family}"
    assert any("needs.detect-changes.outputs.scan-known != 'true'" in condition
               for condition in consumers), (
        f"unknown detector output must conservatively select {family} steps")
    assert any("scan-selection).hotfix" in condition for condition in consumers), (
        f"Hotfix PR must run {family} guard steps inside static-guards")
worker_ci = yaml.load((root / ".github/workflows/worker-ci.yml").read_text(), Loader=yaml.BaseLoader)
worker_selector = worker_ci["jobs"]["select-workers"]
worker_metadata = next(step for step in worker_selector["steps"]
                       if step.get("name") == "Validate PR metadata before using a partial file list")
assert worker_metadata["if"] == "github.event_name == 'pull_request'"
assert "continue-on-error" not in worker_metadata
for claim in ('.base.ref == "main"', '.head.sha', '.base.sha', '.changed_files', '. <= 3000'):
    assert claim in worker_metadata["run"], f"worker selection validates {claim} before filtering"
for job_name, output in (("apple-sso-bridge", "apple"), ("invite-landing", "invite")):
    worker = worker_ci["jobs"][job_name]
    assert set(worker["needs"]) == {"ioc", "select-workers"}
    assert "!cancelled()" in worker["if"]
    assert "needs.ioc.result == 'success'" in worker["if"]
    assert "needs.select-workers.result != 'success'" in worker["if"], (
        f"{job_name} must run when its selector failed")
    assert f"needs.select-workers.outputs.{output} != 'false'" in worker["if"], (
        f"{job_name} must run if its filter is missing or true")
license = yaml.load((root / ".github/workflows/license-check.yml").read_text(), Loader=yaml.BaseLoader)
license_selector = license["jobs"]["select-licenses"]
license_metadata = next(step for step in license_selector["steps"]
                        if step.get("name") == "Validate PR metadata before using a partial file list")
assert license_metadata["if"] == "github.event_name == 'pull_request'"
assert "continue-on-error" not in license_metadata
for claim in ('.base.ref == "main"', '.head.sha', '.base.sha', '.changed_files', '. <= 3000'):
    assert claim in license_metadata["run"], f"license selection validates {claim} before filtering"
with tempfile.TemporaryDirectory() as temporary:
    tmp = Path(temporary)
    fake_gh = tmp / "gh"
    fake_gh.write_text('#!/bin/sh\ncat "$PR_FIXTURE"\n')
    fake_gh.chmod(0o755)
    for label, selector, metadata_step, downstream in (
        ("worker", worker_selector, worker_metadata,
         [worker_ci["jobs"]["apple-sso-bridge"], worker_ci["jobs"]["invite-landing"]]),
        ("license", license_selector, license_metadata,
         [license["jobs"]["check-npm-licenses"], license["jobs"]["check-go-licenses"]]),
    ):
        assert selector["outputs"]["full-scan"] == (
            "${{ steps.metadata-pr.outputs.full-scan || steps.metadata-push.outputs.full-scan }}")
        for job in downstream:
            assert "needs.select-" in job["if"] and "outputs.full-scan == 'true'" in job["if"]
            assert "result != 'success'" in job["if"], "selector failure runs the full consumer"
        if label == "worker":
            for job_name in ("apple-sso-bridge", "invite-landing"):
                assert "outputs.full-scan == 'true'" in worker_ci["jobs"][job_name]["if"], (
                    f"Hotfix full scan must run {job_name}")
        pr_path, output = tmp / f"{label}-pr-fixture.json", tmp / f"{label}-out"
        for tag, changed_files, labels, want_ok, want_full in (
            ("hotfix", 3000, [{"name": "risk: hotfix"}], True, "true"),
            ("ordinary", 8, [{"name": "risk: security"}], True, "false"),
            ("malformed-labels", 8, [{}], False, None),
            ("truncated", 3001, [], False, None),
        ):
            pr_path.write_text(json.dumps({"base": {"ref": "main", "sha": "b" * 40},
                "head": {"sha": "a" * 40}, "changed_files": changed_files, "labels": labels}))
            output.unlink(missing_ok=True)
            run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", metadata_step["run"]],
                env={**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}", "PR_FIXTURE": str(pr_path),
                     "PR_NUMBER": "42", "GITHUB_REPOSITORY": "owner/repo", "RUNNER_TEMP": str(tmp),
                     "GITHUB_OUTPUT": str(output)}, capture_output=True, text=True)
            assert (run.returncode == 0) == want_ok, (label, tag, run)
            if want_ok:
                assert f"full-scan={want_full}" in output.read_text(), (label, tag, output.read_text())
        push_step = next(step for step in selector["steps"] if step.get("id") == "metadata-push")
        for via, resolution, hotfix, want in (
            ("subject", "no-label", "false", "false"),
            ("subject", "subject", "true", "true"),
            ("association", "association", "true", "true"),
            ("", "api-error", "", "true"),
        ):
            output.unlink(missing_ok=True)
            run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", push_step["run"]],
                env={**os.environ, "RESOLVED_VIA": via, "RESOLUTION": resolution,
                     "HOTFIX": hotfix, "GITHUB_OUTPUT": str(output)}, capture_output=True, text=True)
            assert run.returncode == 0 and f"full-scan={want}" in output.read_text(), (
                label, via, resolution, hotfix, run)
assert license_selector["outputs"]["npm-workspaces"] == "${{ steps.compose.outputs.workspaces }}"
assert license_selector["outputs"]["go"] == "${{ steps.filter.outputs.license_go }}"
composer = next(step for step in license_selector["steps"] if step.get("id") == "compose")
assert "incomplete or malformed" in composer["run"]
assert composer["env"]["FULL_SCAN"] == "${{ steps.metadata-pr.outputs.full-scan == 'true' || steps.metadata-push.outputs.full-scan == 'true' }}"
with tempfile.TemporaryDirectory() as temporary:
    workspace_output = Path(temporary) / "github-output"
    for selected, expected in [
        ({"license_desktop": "true", "license_media": "false", "license_admin": "false"}, ["client/desktop"]),
        ({"license_desktop": "false", "license_media": "false", "license_admin": "true"}, ["client/admin"]),
        ({"license_desktop": "true", "license_media": "true", "license_admin": "true"}, ["client/desktop", "services/media-plane", "client/admin"]),
        ({"license_desktop": "false", "license_media": "false", "license_admin": "false"}, []),
    ]:
        run = subprocess.run(
            ["node", "-e", composer["run"].split("node - <<'NODE'\n", 1)[1].rsplit("\nNODE", 1)[0]],
            env={**os.environ, "FILTERS": json.dumps(selected), "FULL_SCAN": "false", "GITHUB_OUTPUT": str(workspace_output)},
            capture_output=True, text=True)
        assert run.returncode == 0, (selected, run)
        assert json.loads(workspace_output.read_text().strip().split("=", 1)[1]) == expected
        workspace_output.unlink()
    hotfix_full = {"license_desktop": "false", "license_media": "false", "license_admin": "false"}
    run = subprocess.run(
        ["node", "-e", composer["run"].split("node - <<'NODE'\n", 1)[1].rsplit("\nNODE", 1)[0]],
        env={**os.environ, "FILTERS": json.dumps(hotfix_full), "FULL_SCAN": "true", "GITHUB_OUTPUT": str(workspace_output)},
        capture_output=True, text=True)
    assert run.returncode == 0, ("Hotfix full scan", run)
    assert json.loads(workspace_output.read_text().strip().split("=", 1)[1]) == ["client/desktop", "services/media-plane", "client/admin"]
    workspace_output.unlink()
    run = subprocess.run(
        ["node", "-e", composer["run"].split("node - <<'NODE'\n", 1)[1].rsplit("\nNODE", 1)[0]],
        env={**os.environ, "FILTERS": '{"license_desktop":"true"}', "GITHUB_OUTPUT": str(workspace_output)},
        capture_output=True, text=True)
    assert run.returncode != 0 and "incomplete or malformed" in run.stderr
npm_license = license["jobs"]["check-npm-licenses"]
assert set(npm_license["needs"]) == {"ioc", "select-licenses"}
assert "!cancelled()" in npm_license["if"]
assert "needs.ioc.result == 'success'" in npm_license["if"]
assert "needs.select-licenses.result != 'success'" in npm_license["if"]
assert "npm-workspaces != '[]'" in npm_license["if"]
assert "needs.select-licenses.result != 'success'" in npm_license["if"]
assert "needs.select-licenses.outputs.npm-workspaces ||" in npm_license["env"]["NPM_WORKSPACES"]
workspace_runner = next(step["run"] for step in npm_license["steps"]
                        if step.get("name") == "Check selected npm workspaces and NOTICE tables")
assert '--workspace "$workspace"' in workspace_runner
assert "client/desktop|services/media-plane|client/admin" in workspace_runner
assert 'selected = allowed' in workspace_runner, "missing or malformed selection scans all workspaces"
go_license = license["jobs"]["check-go-licenses"]
assert set(go_license["needs"]) == {"ioc", "select-licenses"}
assert "!cancelled()" in go_license["if"]
assert "needs.ioc.result == 'success'" in go_license["if"]
assert "needs.select-licenses.result != 'success'" in go_license["if"]
assert "needs.select-licenses.outputs.go != 'false'" in go_license["if"]
jobs = workflow["jobs"]
assert workflow["on"]["pull_request"]["branches"] == ["__ruleset_only__"]
assert "concurrency" not in workflow
assert jobs["pr-ci"]["uses"] == "Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@cb10844a7860092011f96b4607682ef2e6c55a80"
script_pin = re.search(r"^readonly PR_CI_SHA='([0-9a-f]{40})'", relevance_apply.read_text(), re.M)
assert script_pin and jobs["pr-ci"]["uses"].endswith("@" + script_pin.group(1)), (
    "scan cutover verifier must use the protected workflow's pinned pr-ci SHA")
assert jobs["pr-ci"]["name"] == "Protected scan enforcement"
assert jobs["pr-ci"]["with"] == {"trusted-source": "true", "enforcement-only": "true"}
assert jobs["pr-ci"]["secrets"] == {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"}
assert jobs["osv"]["name"] == "Protected OSV dependency scan"
assert jobs["osv"]["uses"] == "Concord-Voice/Self-Hosted/.github/workflows/osv-scanner.yml@06bc23adc8c82a1eb55e5d6988b42f42f463fb83"
assert jobs["osv"]["needs"] == ["pr-ci"]
assert jobs["osv"]["if"] == "${{ always() && contains(needs.pr-ci.outputs.report-selection, '\"osv\":true') }}", (
    "selected protected OSV must run even when another pr-ci scan fails; unselected OSV stays skipped")
assert "ioc" not in jobs and "build" not in jobs
publisher = jobs["publish-install-script-warnings"]
assert publisher["needs"] == ["pr-ci"] and publisher["if"] == "${{ always() }}"
assert publisher["concurrency"] == {
    "group": "required-pr-install-script-comments-${{ github.event.pull_request.number }}",
    "cancel-in-progress": "false",
    "queue": "max",
}
assert publisher["permissions"] == {"actions": "read", "pull-requests": "write"}
assert [s["name"] for s in publisher["steps"]] == [
    "Download install-script warnings", "Publish install-script warnings to PR",
]
assert publisher["steps"][0]["with"]["name"] == "ioc-install-script-warnings"
assert "download-artifact@" in publisher["steps"][0]["uses"]
assert "github-script@" in publisher["steps"][1]["uses"]
assert all("run" not in s and "checkout@" not in s["uses"] for s in publisher["steps"])
# Execute the write-capable consumer with a fake GitHub API. PR-derived data
# must be validated before any mutation, and only the bot's marker is mutable.
with tempfile.TemporaryDirectory() as temporary:
    artifact = Path(temporary) / "warnings.json"
    js = '''
      const actions = [];
      const notices = [];
      const comments = JSON.parse(process.env.MOCK_COMMENTS);
      const issues = {
        listComments() {},
        createComment: async args => actions.push({kind: 'create', ...args}),
        updateComment: async args => actions.push({kind: 'update', ...args}),
        deleteComment: async args => actions.push({kind: 'delete', ...args}),
      };
      const pulls = {get: async () => ({data: {head: {
        sha: process.env.CURRENT_HEAD_SHA,
        repo: JSON.parse(process.env.CURRENT_HEAD_REPO),
      }, base: {ref: process.env.CURRENT_BASE_REF,
                repo: {full_name: 'Concord-Voice/Self-Hosted'}},
      user: {login: process.env.MOCK_PR_AUTHOR}}})};
      const github = {paginate: async () => comments, rest: {issues, pulls}};
      const core = {notice: message => notices.push(message),
                    info: message => notices.push(message)};
      const context = {repo: {owner: 'Concord-Voice', repo: 'Self-Hosted'},
                       issue: {number: 123}, payload: {pull_request: {head: {sha: 'a'.repeat(40)},
                                                                     base: {ref: 'main'},
                                                                     user: {login: process.env.MOCK_PR_AUTHOR}}}};
      const AsyncFunction = Object.getPrototypeOf(async function(){}).constructor;
      new AsyncFunction('github', 'context', 'core', 'require', 'process', process.env.PUBLISHER_SCRIPT)
        (github, context, core, require, process)
        .then(() => console.log(JSON.stringify({actions, notices})))
        .catch(error => {
          console.log(JSON.stringify({actions, notices, error: error.message}));
          process.exitCode = 1;
        });
    '''
    marker = "<!-- supply-chain-install-scripts -->"
    attacker = {"id": 1, "user": {"login": "attacker"}, "body": marker}
    bot = {"id": 2, "user": {"login": "github-actions[bot]"}, "body": marker}
    warning = "[WARN] New install script: example@1.0.0"

    def publish_case(payload, comments, expected_kind, expected_id=None,
                     current_sha="a" * 40, current_base="main",
                     author="contributor", head_repo="Concord-Voice/Self-Hosted",
                     expect_notice=False):
        artifact.write_text(json.dumps(payload))
        run = subprocess.run(
            ["node", "-e", js],
            env={**os.environ, "WARNINGS_PATH": str(artifact),
                 "PUBLISHER_SCRIPT": publisher["steps"][1]["with"]["script"],
                 "MOCK_COMMENTS": json.dumps(comments),
                 "CURRENT_HEAD_SHA": current_sha,
                 "CURRENT_BASE_REF": current_base,
                 "CURRENT_HEAD_REPO": json.dumps(
                     None if head_repo is None else {"full_name": head_repo}),
                 "MOCK_PR_AUTHOR": author},
            capture_output=True, text=True,
        )
        result = json.loads(run.stdout)
        if expected_kind is None:
            assert run.returncode != 0 and result["actions"] == [], (run, result)
            assert result["error"] == "Invalid IOC warning artifact", result
        elif expected_kind == "skip":
            assert run.returncode == 0 and result["actions"] == [], (run, result)
            if expect_notice:
                assert any("artifact" in note.lower() or "log" in note.lower()
                           for note in result["notices"]), result
        else:
            assert run.returncode == 0 and len(result["actions"]) == 1, (run, result)
            action = result["actions"][0]
            assert action["kind"] == expected_kind, action
            if expected_id is not None:
                assert action["comment_id"] == expected_id, action
            if expected_kind != "delete":
                assert f"\n    {warning}\n" in action["body"], action

    publish_case({"count": 1, "warnings": [warning]}, [attacker, bot], "update", 2)
    publish_case({"count": 1, "warnings": [warning]}, [attacker], "create")
    publish_case({"count": 0, "warnings": []}, [attacker, bot], "delete", 2)
    publish_case({"count": 1, "warnings": [warning]}, [attacker, bot], "skip", current_sha="b" * 40)
    publish_case({"count": 0, "warnings": []}, [attacker, bot], "skip", current_sha="b" * 40)
    publish_case({"count": 1, "warnings": [warning]}, [attacker, bot], "skip", current_base="release")
    publish_case({"count": 1, "warnings": [warning]}, [attacker], "skip",
                 author="dependabot[bot]", expect_notice=True)
    publish_case({"count": 1, "warnings": [warning]}, [attacker], "skip",
                 head_repo="contributor/Self-Hosted", expect_notice=True)
    publish_case({"count": 1, "warnings": [warning]}, [attacker], "skip",
                 head_repo=None, expect_notice=True)
    publish_case({"count": 0, "warnings": []}, [attacker], "skip",
                 author="dependabot[bot]")
    publish_case({"count": 1, "warnings": ["untrusted text"]}, [attacker], None,
                 author="dependabot[bot]")
    publish_case({"count": 2, "warnings": [warning]}, [attacker, bot], None)
    publish_case({"count": 1, "warnings": ["untrusted text"]}, [attacker, bot], None)
    publish_case({"count": 1, "warnings": [warning + "x" * 1000]}, [attacker, bot], None)
result = jobs["result"]
assert result["needs"] == ["pr-ci", "osv", "publish-install-script-warnings", "migration-lint"]
assert result["if"] == "${{ always() }}"
assert result["permissions"] == {
    "actions": "read", "checks": "read", "contents": "read",
    "pull-requests": "read", "security-events": "read",
}
assert not result.get("continue-on-error", False)
verifier = next(step for step in result["steps"] if step.get("name") == "Run protected scan verdict")
assert verifier["if"] == "${{ always() }}"
assert verifier["env"]["SELECTION_JSON"] == "${{ needs.pr-ci.outputs.report-selection || '{}' }}"
assert verifier["env"]["JOBS_JSON"] == "${{ needs.pr-ci.outputs.report-jobs || '{}' }}"
assert verifier["env"]["COVERAGE_JSON"] == "${{ needs.pr-ci.outputs.coverage-uploads || '{}' }}"
assert verifier["env"]["GITHUB_RUN_ATTEMPT"] == "${{ github.run_attempt }}"
assert "verify-pr-scan-results.py" in verifier["run"]
pins = next(step for step in result["steps"] if step.get("id") == "pins")
assert pins["if"] == "${{ always() }}"
assert pins["env"] == {"HEAD_SHA": "${{ github.event.pull_request.head.sha }}"}
assert "rev-parse 'refs/scan/pr-merge^2'" in pins["run"]
assert "--require-hashes" in pins["run"] and "protected/.github/requirements-pyyaml.txt" in pins["run"]
assert "+refs/heads/main:refs/scan/main" in pins["run"]
assert "refs/scan/pr-merge:.github/workflows/required-pr-build.yml" in pins["run"]
assert "protected/.github/scripts/verify-required-build-pins.py" in pins["run"]
assert "--main-ref refs/scan/main" in pins["run"]
assert "'refs/scan/pr-merge^1:.github/workflows/required-pr-build.yml'" in pins["run"]
assert '--base-workflow "$RUNNER_TEMP/base-required-pr-build.yml"' in pins["run"]
fetch = next(step for step in result["steps"] if step.get("name") == "Verify immutable workflow identity and fetch PR objects as data")
assert result["steps"].index(fetch) < result["steps"].index(pins) < result["steps"].index(verifier)
# Run the step itself against a fixture origin, as the result job would after
# its checkout (fetch-depth 0, so branch-only commits are present) and its fetch
# of the PR merge ref. A hook's GIT_DIR must not reach the fixture's git, and a
# python3 shim records the PyYAML install instead of reaching PyPI.
git_env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
def fixture_git(repo, *args):
    return subprocess.run(
        ["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@example.invalid",
         "-c", "commit.gpgsign=false", *args],
        env=git_env, check=True, capture_output=True, text=True).stdout.strip()
with tempfile.TemporaryDirectory() as tmp:
    tmp = Path(tmp)
    origin = tmp / "origin"
    subprocess.run(["git", "init", "-q", "-b", "main", str(origin)], env=git_env, check=True)
    def commit_file(repo, path, text, message):
        (repo / path).parent.mkdir(parents=True, exist_ok=True)
        (repo / path).write_text(text)
        fixture_git(repo, "add", path)
        fixture_git(repo, "commit", "-q", "-m", message)
        return fixture_git(repo, "rev-parse", "HEAD")
    def required_build(pin):
        return ("permissions:\n  contents: read\n"
                "jobs:\n  pr-ci:\n    uses: Concord-Voice/Self-Hosted/.github/workflows/"
                f"pr-ci.yml@{pin}\n")
    older = commit_file(origin, ".github/workflows/pr-ci.yml", "on: workflow_call\n", "older seed")
    seed = commit_file(origin, ".github/workflows/pr-ci.yml", "on: workflow_call # seed\n", "seed")
    # The base every fixture merge ref builds on pins the seed, so a PR pinning
    # `older` moves back from its base.
    reviewed = commit_file(origin, ".github/workflows/required-pr-build.yml", required_build(seed), "reviewed")
    fixture_git(origin, "switch", "-q", "-c", "side")
    branch_only = commit_file(origin, ".github/workflows/pr-ci.yml", "on: workflow_call # side\n", "branch only")
    fixture_git(origin, "switch", "-q", "main")
    protected = tmp / "protected"
    subprocess.run(["git", "clone", "-q", str(origin), str(protected)], env=git_env, check=True)
    # The merge ref supplies its head object even when GitHub has no separate head ref.
    tree = fixture_git(origin, "rev-parse", "HEAD^{tree}")
    pr_head = fixture_git(origin, "commit-tree", tree, "-p", reviewed, "-m", "PR head")
    pr_merge = fixture_git(origin, "commit-tree", tree, "-p", reviewed, "-p", pr_head, "-m", "PR merge")
    fixture_git(origin, "update-ref", "refs/pull/123/merge", pr_merge)
    assert subprocess.run(["git", "-C", str(origin), "show-ref", "--verify", "refs/pull/123/head"],
                          env=git_env, capture_output=True).returncode != 0
    assert subprocess.run(["git", "-C", str(protected), "cat-file", "-e", pr_head],
                          env=git_env, capture_output=True).returncode != 0
    fetch_env = {**git_env, "WORKFLOW_REPOSITORY": "Concord-Voice/Concord-Voice-Alpha",
                 "WORKFLOW_SHA": reviewed, "BASE_SHA": reviewed, "HEAD_SHA": pr_head, "PR_NUMBER": "123"}
    fetched = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", fetch["run"]], cwd=tmp,
                             env=fetch_env, capture_output=True, text=True)
    assert fetched.returncode == 0, ("merge-only PR fetch must succeed", fetched)
    assert fixture_git(protected, "rev-parse", "refs/scan/pr-merge") == pr_merge
    assert fixture_git(protected, "rev-parse", "refs/scan/pr-merge^2") == pr_head
    for override, message in [({"WORKFLOW_SHA": pr_head}, "Protected verifier source or PR revisions"),
                              ({"PR_NUMBER": "124"}, "couldn't find remote ref refs/pull/124/merge")]:
        refused = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", fetch["run"]], cwd=tmp,
                                 env={**fetch_env, **override}, capture_output=True, text=True)
        assert refused.returncode != 0 and message in refused.stdout + refused.stderr, refused
    (protected / ".github/scripts").mkdir(parents=True)
    (protected / ".github/scripts/verify-required-build-pins.py").write_text(
        (root / ".github/scripts/verify-required-build-pins.py").read_text())
    shim = tmp / "bin"
    shim.mkdir()
    pip_log = tmp / "pip.log"
    (shim / "python3").write_text(
        "#!/bin/sh\n"
        f'if [ "$1 $2" = "-m pip" ]; then printf "%s\\n" "$*" >> "{pip_log}"; exit 0; fi\n'
        f'exec "{sys.executable}" "$@"\n')
    (shim / "python3").chmod(0o755)
    base = fixture_git(protected, "rev-parse", "HEAD")
    # main can move after the checkout: GitHub computes the merge ref on its own
    # clock, so it may already pin a commit newer than workflow_sha.
    later = commit_file(origin, ".github/workflows/pr-ci.yml", "on: workflow_call # later\n",
                        "merged after checkout")
    heads = []
    for pin, reported, passes in [(reviewed, 0, True), (later, 0, True), (branch_only, 0, False),
                                  (older, 0, False), (reviewed, -1, False)]:
        (protected / ".github/workflows/required-pr-build.yml").write_text(required_build(pin))
        fixture_git(protected, "add", ".github/workflows/required-pr-build.yml")
        fixture_git(protected, "commit", "-q", "--allow-empty", "-m", f"pin {pin}")
        heads.append(fixture_git(protected, "rev-parse", "HEAD"))
        tree = fixture_git(protected, "rev-parse", "HEAD^{tree}")
        merge = fixture_git(protected, "commit-tree", tree, "-p", base, "-p", heads[-1], "-m", "merge")
        fixture_git(protected, "update-ref", "refs/scan/pr-merge", merge)
        # reported -1: the run reports an earlier head than the merge ref builds on.
        head_sha = heads[-1] if reported == 0 else heads[-2]
        pip_log.unlink(missing_ok=True)
        run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", pins["run"]], cwd=tmp,
                             env={**git_env, "RUNNER_TEMP": str(tmp), "HEAD_SHA": head_sha,
                                  "PATH": f"{shim}:{os.environ['PATH']}"},
                             capture_output=True, text=True)
        assert (run.returncode == 0) == passes, (pin, reported, run)
        assert ("::error::" in run.stdout) != passes, (pin, reported, run)
        if pin == older:
            assert "moves back from the base's pin" in run.stdout, run
        if reported == -1:
            assert "supersedes" in run.stdout and not pip_log.exists(), run
        else:
            install = pip_log.read_text()
            assert "--require-hashes" in install and "protected/.github/requirements-pyyaml.txt" in install, install
require = next(step for step in result["steps"] if step.get("name") == "Require protected verdict and warning publication")
assert require["if"] == "${{ always() }}"
assert require["env"] == {
    "PR_CI_RESULT": "${{ needs.pr-ci.result }}",
    "PUBLISH_RESULT": "${{ needs.publish-install-script-warnings.result }}",
    "OSV_RESULT": "${{ needs.osv.result }}",
    "VERIFIER_OUTCOME": "${{ steps.verify.outcome }}",
    "PINS_OUTCOME": "${{ steps.pins.outcome }}",
    "MIGRATION_LINT_RESULT": "${{ needs.migration-lint.result }}",
}

ioc_workflow = yaml.load(
    (root / ".github/workflows/ioc-scanning.yml").read_text(),
    Loader=yaml.BaseLoader,
)
assert ioc_workflow["permissions"] == {"contents": "read"}
assert all("write" not in j.get("permissions", {}).values() for j in ioc_workflow["jobs"].values())
scan = ioc_workflow["jobs"]["supply-chain-scan"]
assert scan["env"] == {"PYTHONSAFEPATH": "1", "PYTHONPATH": ""}
assert any(s.get("name") == "Require Python safe-path support" and "(3, 11)" in s["run"]
           for s in scan["steps"])
assert scan["permissions"] == {"contents": "read", "pull-requests": "read"}
identity_step = next(s for s in scan["steps"] if s.get("name") == "Verify protected workflow identity")
assert identity_step["if"] == "${{ inputs.trusted-source }}"
assert "workflow_repository" in identity_step["env"]["WORKFLOW_REPOSITORY"]
assert "workflow_sha" in identity_step["env"]["WORKFLOW_SHA"]
for repo, sha, passes in [
    ("Concord-Voice/Self-Hosted", "a" * 40, True),
    ("Concord-Voice/Self-Hosted", "", False),
    ("Concord-Voice/Self-Hosted", "a" * 39, False),
    ("attacker/repo", "a" * 40, False),
]:
    check = subprocess.run(
        ["bash", "-e", "-o", "pipefail", "-c", identity_step["run"]],
        env={**os.environ, "WORKFLOW_REPOSITORY": repo, "WORKFLOW_SHA": sha},
        capture_output=True, text=True,
    )
    assert (check.returncode == 0) == passes, (repo, sha, check)
target_checkout = next(s for s in scan["steps"] if s.get("name") == "Check out target for trusted scan")
source_checkout = next(s for s in scan["steps"] if s.get("name") == "Check out protected scanner and policy")
assert target_checkout["with"]["path"] == "target" and "ref" not in target_checkout["with"]
assert source_checkout["with"]["path"] == "scanner"
assert "workflow_repository" in source_checkout["with"]["repository"]
assert "workflow_sha" in source_checkout["with"]["ref"]
assert source_checkout["with"]["persist-credentials"] == "false"
scan_step = next(s for s in scan["steps"] if s.get("id") == "scan")
runner_step = next(s for s in scan["steps"] if s.get("name") == "Check self-hosted runner allowlist")
assert "scanner/scripts/security/scan-supply-chain-iocs.sh" in scan_step["run"]
assert "scanner/scripts/security/iocs/shai-hulud-2.0.txt" in scan_step["env"]["IOC_LIST_PATH"]
assert "scanner/scripts/security/iocs/false-positive-allowlist.txt" in scan_step["env"]["IOC_ALLOWLIST_PATH"]
assert "scanner/scripts/security/check-self-hosted-runners.sh" in runner_step["run"]
assert "scanner/scripts/security/self-hosted-runner-allowlist.txt" in runner_step["env"]["RUNNER_ALLOWLIST_PATH"]
upload = next(s for s in scan["steps"] if s.get("name") == "Upload install-script warnings")
block = next(s for s in scan["steps"] if s.get("name") == "Fail on blocking findings")
assert scan["steps"].index(upload) < scan["steps"].index(block)
assert "upload-artifact@" in upload["uses"]
assert upload["with"]["name"] == "ioc-install-script-warnings"
assert upload["with"]["if-no-files-found"] == "error"
assert ioc_workflow["jobs"]["supply-chain-scan-selftest"]["permissions"] == {"contents": "read"}

for pr_ci, publish, verifier_outcome, pins_outcome, migration, passes in [
    ("success", "success", "success", "success", "success", True),
    ("failure", "success", "success", "success", "skipped", False),
    ("success", "failure", "success", "success", "success", False),
    ("success", "success", "failure", "success", "success", False),
    ("success", "success", "success", "failure", "success", False),
    ("success", "success", "success", "", "success", False),
    ("cancelled", "skipped", "skipped", "skipped", "skipped", False),
    ("", "success", "success", "success", "success", False),
    # The protected migration lint (#4175) skips PRs without control-plane changes;
    # skipped passes only because pr-ci itself must succeed. A failure or a missing
    # result blocks.
    ("success", "success", "success", "success", "skipped", True),
    ("success", "success", "success", "success", "failure", False),
    ("success", "success", "success", "success", "cancelled", False),
    ("success", "success", "success", "success", "", False),
]:
    case = (pr_ci, publish, verifier_outcome, pins_outcome, migration)
    run = subprocess.run(
        ["bash", "-e", "-o", "pipefail", "-c", require["run"]],
        env={**os.environ, "PR_CI_RESULT": pr_ci, "PUBLISH_RESULT": publish,
             "VERIFIER_OUTCOME": verifier_outcome, "PINS_OUTCOME": pins_outcome,
             "MIGRATION_LINT_RESULT": migration},
        capture_output=True,
        text=True,
    )
    assert (run.returncode == 0) == passes, (case, run)
    if not passes:
        assert "::error::" in run.stdout, (case, run)
        assert f"pr-ci={pr_ci}" in run.stdout, (case, run)
        assert f"publisher={publish}" in run.stdout, (case, run)
        assert f"verifier={verifier_outcome}" in run.stdout, (case, run)
        assert f"pins={pins_outcome}" in run.stdout, (case, run)

pr_ci = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
assert "edited" in pr_ci["on"]["pull_request"]["types"]
metadata_edit = "github.event.action == 'edited' && github.event.changes.base == null && (github.event.changes.title != null || github.event.changes.body != null)"
assert metadata_edit in pr_ci["concurrency"]["group"]
assert "pr-ci-metadata-" in pr_ci["concurrency"]["group"]
assert "pr-ci-ordinary-" in pr_ci["concurrency"]["group"]
for job in ("ioc", "detect-changes", "report"):
    assert metadata_edit in pr_ci["jobs"][job]["if"], f"{job} must skip only title/body-only edits"
    assert "inputs.enforcement-only ||" in pr_ci["jobs"][job]["if"], f"{job} must preserve protected calls"
assert "always()" in pr_ci["jobs"]["report"]["if"]
assert "build-sonar-gate" not in pr_ci["jobs"]
assert pr_ci["permissions"]["contents"] == "read"
assert pr_ci["jobs"]["ioc"]["permissions"] == {"contents": "read", "pull-requests": "read"}

spec = importlib.util.spec_from_file_location("sonar_ioc_policy", root / ".github/scripts/tests/sonar_ioc_policy.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.check_ioc_path_policies(pr_ci)
worker_ci = yaml.load((root / ".github/workflows/worker-ci.yml").read_text(), Loader=yaml.BaseLoader)
assert worker_ci["jobs"]["ioc"]["permissions"] == {"contents": "read", "pull-requests": "read"}
assert "concurrency:" not in (root / ".github/workflows/build.yml").read_text()
scanner = json.loads((root / "infrastructure/github/alpha-code-scanning-ruleset.json").read_text())
assert [rule["type"] for rule in scanner["rules"]] == ["code_scanning"]
assert len(scanner["rules"][0]["parameters"]["code_scanning_tools"]) == 5

ruleset_path = root / "infrastructure/github/alpha-required-workflow-ruleset.json"
ruleset = json.loads(ruleset_path.read_text())
assert ruleset["name"] == "Alpha Required PR Build"
assert ruleset["enforcement"] == "active"
assert ruleset["conditions"] == {
    "repository_id": {"repository_ids": [1158152129]},
    "ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []},
}
assert ruleset["bypass_actors"] == [
    {"actor_id": 3967879, "actor_type": "Integration", "bypass_mode": "always"}
]
assert sorted(ruleset["rules"], key=lambda rule: rule["type"]) == [{
    "type": "required_status_checks",
    "parameters": {
        "strict_required_status_checks_policy": True,
        "do_not_enforce_on_create": False,
        "required_status_checks": [{"context": "Required PR build result", "integration_id": 15368}],
    },
}, {
    "type": "workflows",
    "parameters": {
        "do_not_enforce_on_create": False,
        "workflows": [{
            "repository_id": 1158152129,
            "path": ".github/workflows/required-pr-build.yml",
            "ref": "refs/heads/main",
        }],
    },
}]

# The apply command must create only a missing, reviewed ruleset, and only with
# explicit --apply after the workflow exists on main. The stub has no network.
with tempfile.TemporaryDirectory() as temporary:
    tmp = Path(temporary)
    fake = tmp / "gh"
    marker = tmp / "posted"
    state = tmp / "ruleset.json"
    calls = tmp / "calls"
    reads = tmp / "reads"
    source_reads = tmp / "source-reads"
    fake.write_text('''#!/usr/bin/env python3
import base64, hashlib, json, os, sys
from pathlib import Path
a = sys.argv[1:]
desired = json.loads(Path(os.environ["DESIRED"]).read_text())
state = Path(os.environ["STATE"])
calls = Path(os.environ["CALLS"])
reads = Path(os.environ["READS"])
source_reads = Path(os.environ["SOURCE_READS"])
mode = os.environ["LIVE"]
source = os.environ["SOURCE"]
def record(event):
    with calls.open("a") as f:
        f.write(event + "\\n")
def read_count():
    count = int(reads.read_text()) + 1 if reads.exists() else 1
    reads.write_text(str(count))
    return count
def source_count():
    count = int(source_reads.read_text()) + 1 if source_reads.exists() else 1
    source_reads.write_text(str(count))
    return count
if a == ["api", "--paginate", "orgs/Concord-Voice/rulesets"]:
    if mode == "listfail":
        sys.exit(1)
    missing = mode not in {"desired", "drift"}
    print(json.dumps([] if missing else [{"id": 123, "name": desired["name"]}]))
elif a == ["api", "orgs/Concord-Voice/rulesets/123"]:
    if state.exists():
        count = read_count()
        # Reads of the ruleset, in order: 1 after the POST, 2 before the activation,
        # 3 after it. A rollback then reads the rule once before the DELETE (4) and once
        # before the disabling fallback (5) to prove it is still the rule this run wrote,
        # so the read after that fallback, the one this mode makes fail, is the sixth.
        if ((mode == "getfailpost" and count == 1) or
            (mode == "getfailactive" and count == 3) or
            (mode == "fallbackreadfail" and count == 6)):
            sys.exit(1)
        live = json.loads(state.read_text())
        if ((mode == "stalepost" and count == 1) or
            (mode == "driftpreactivate" and count == 2) or
            (mode in {"driftactive", "deletefail", "bothfail", "fallbackreadfail"} and count == 3)):
            live["name"] = "unexpected drift"
        print(json.dumps(live))
    else:
        if mode == "drift":
            desired["enforcement"] = "disabled"
        print(json.dumps(desired))
elif any("repos/Concord-Voice/Self-Hosted/commits/main" in arg for arg in a):
    count = source_count()
    sha = "c" * 40 if source == "move" and count > 1 else "a" * 40
    if "--jq" in a and a[a.index("--jq") + 1] == ".sha":
        print(sha)
    else:
        print(json.dumps({"sha": sha}))
elif any("repos/Concord-Voice/Self-Hosted/contents/" in arg for arg in a):
    path = next(arg.split("/contents/", 1)[1].split("?", 1)[0]
                for arg in a if "/contents/" in arg)
    if source == "missing" and path == ".github/workflows/required-pr-build.yml":
        sys.exit(1)
    content = Path(os.environ["REPO_ROOT"], path).read_bytes()
    if source == "old" and path == ".github/workflows/required-pr-build.yml":
        content = b"name: Required Build\\njobs: {build: {runs-on: ubuntu-latest}}\\n"
    blob = hashlib.sha1(b"blob " + str(len(content)).encode() + b"\\0" + content).hexdigest()
    print(json.dumps({"path": path, "sha": blob,
        "content": base64.b64encode(content).decode(), "encoding": "base64"}))
elif a[:4] == ["api", "-X", "POST", "orgs/Concord-Voice/rulesets"]:
    assert a[4] == "--input"
    payload = json.loads(Path(a[5]).read_text())
    assert payload == {**desired, "enforcement": "disabled"}
    Path(os.environ["MARKER"]).write_text("posted")
    state.write_text(json.dumps(payload))
    record("POST disabled")
    if mode == "postfail":
        sys.exit(1)
    if mode == "idmissing":
        print('{}')
        sys.exit(0)
    print('{"id":123}')
elif a[:4] == ["api", "-X", "PUT", "orgs/Concord-Voice/rulesets/123"]:
    assert a[4] == "--input"
    payload = json.loads(Path(a[5]).read_text())
    assert payload == desired or payload == {**desired, "enforcement": "disabled"}
    stage = payload["enforcement"]
    record(f"PUT {stage}")
    if (mode == "putfail" and stage == "active") or (mode == "bothfail" and stage == "disabled"):
        sys.exit(1)
    state.write_text(json.dumps(payload))
    print(json.dumps(payload))
elif a == ["api", "-X", "DELETE", "orgs/Concord-Voice/rulesets/123"]:
    record("DELETE")
    if mode in {"deletefail", "bothfail", "fallbackreadfail"}:
        sys.exit(1)
    state.unlink()
else:
    raise SystemExit(f"unexpected gh call: {a}")
''')
    fake.chmod(0o755)
    apply = root / "infrastructure/github/apply-alpha-required-workflow-ruleset.sh"

    def case(live, source, flag, passes, posts, final=None, events=(), critical=False, diagnostic=None):
        marker.unlink(missing_ok=True)
        state.unlink(missing_ok=True)
        calls.unlink(missing_ok=True)
        reads.unlink(missing_ok=True)
        source_reads.unlink(missing_ok=True)
        run = subprocess.run(
            ["bash", str(apply), flag],
            env={**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}",
                 "DESIRED": str(ruleset_path), "LIVE": live,
                 "SOURCE": source, "MARKER": str(marker),
                 "STATE": str(state), "CALLS": str(calls), "READS": str(reads),
                 "SOURCE_READS": str(source_reads), "REPO_ROOT": str(root)},
            capture_output=True, text=True,
        )
        assert (run.returncode == 0) == passes, (live, source, flag, run)
        assert marker.exists() == posts, (live, source, flag, run)
        observed = calls.read_text().splitlines() if calls.exists() else []
        assert all(event in observed for event in events), (live, observed, run)
        assert ("CRITICAL:" in run.stderr) == critical, (live, run)
        if diagnostic is not None:
            assert diagnostic in run.stderr, (live, source, diagnostic, run)
        if live == "fallbackreadfail":
            assert "ROLLBACK: ruleset 123 is disabled" not in run.stderr, run
        if live == "missing" and source == "yes" and flag == "--apply":
            assert int(reads.read_text()) == 3, (observed, run)
            assert observed.index("POST disabled") < observed.index("PUT active"), observed
        if live in {"stalepost", "driftpreactivate"}:
            assert "PUT active" not in observed, (live, observed)
        if final is not None:
            assert (json.loads(state.read_text())["enforcement"] if state.exists() else "absent") == final, (live, run)

    case("missing", "no", "--dry-run", True, False, "absent")
    case("missing", "missing", "--apply", False, False, "absent")
    case("missing", "yes", "--apply", True, True, "active", ("POST disabled", "PUT active"))
    case("listfail", "yes", "--apply", False, False, "absent")
    case("stalepost", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("getfailpost", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("driftpreactivate", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("putfail", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("getfailactive", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("driftactive", "yes", "--apply", False, True, "absent", ("DELETE",))
    case("deletefail", "yes", "--apply", False, True, "disabled", ("DELETE", "PUT disabled"))
    case("fallbackreadfail", "yes", "--apply", False, True, "disabled",
         ("DELETE", "PUT disabled"), critical=True)
    case("bothfail", "yes", "--apply", False, True, "active", ("DELETE", "PUT disabled"), critical=True)
    case("postfail", "yes", "--apply", False, True, "disabled", critical=True)
    case("idmissing", "yes", "--apply", False, True, "disabled", critical=True)
    case("desired", "yes", "--apply", True, False, "absent")
    case("desired", "old", "--apply", False, False, "absent",
         diagnostic="protected source differs from reviewed local gate files")
    case("desired", "missing", "--apply", False, False, "absent",
         diagnostic="protected source is unavailable")
    case("desired", "move", "--apply", False, False, "absent",
         diagnostic="protected main changed while validating workflow source")
    case("missing", "old", "--apply", False, False, "absent",
         diagnostic="protected source differs from reviewed local gate files")
    case("missing", "missing", "--apply", False, False, "absent",
         diagnostic="protected source is unavailable")
    case("missing", "move", "--apply", False, False, "absent",
         diagnostic="protected main changed while validating workflow source")
    case("drift", "yes", "--apply", False, False, "absent")

# Task 6: migration dry-run uses only a hermetic gh API shim and must not issue
# any mutation requests. Snapshots are synthetic and bound to current main.
with tempfile.TemporaryDirectory() as temporary:
    tmp = Path(temporary)
    fake_gh = tmp / "gh"
    calls = tmp / "calls.jsonl"
    fake_gh.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
Path(os.environ["GH_CALLS"]).open("a").write(json.dumps(args) + "\\n")
path = next((arg for arg in args if arg.startswith(("orgs/", "repos/"))), "")
if path == "orgs/Concord-Voice/rulesets/17153062":
    result = json.loads(Path(os.environ["ORG_RULE"]).read_text())
elif path == "orgs/Concord-Voice/rulesets/24555855":
    result = json.loads(Path(os.environ["WORKFLOW_SOURCE_RULE"]).read_text())
elif path == "repos/Concord-Voice/Self-Hosted/rulesets/17541948":
    override = os.environ.get("REPO_RULE_OVERRIDE", os.environ["REPO_RULE"])
    result = json.loads(Path(override).read_text())
elif path == "repos/Concord-Voice/Self-Hosted/rules/branches/main":
    pages = json.loads(Path(os.environ["EFFECTIVE_PAGES"]).read_text())
    if os.environ.get("LIVE_EFFECTIVE_WRAPPED") == "1":
        print(json.dumps({"rules": [rule for page in pages for rule in page]}))
        raise SystemExit(0)
    if os.environ.get("LIVE_EFFECTIVE_MIXED_EMPTY"):
        populated = [rule for page in pages for rule in page]
        pages = ([[], populated] if os.environ["LIVE_EFFECTIVE_MIXED_EMPTY"] == "empty-first"
                 else [populated, []])
    if "--paginate" in args:
        for page in pages:
            print(json.dumps(page))
        raise SystemExit(0)
    else:
        result = pages[0]
elif path == "orgs/Concord-Voice/rulesets?per_page=100":
    result = [{"id": 123, "name": "Alpha Required PR Build"}]
elif path == "orgs/Concord-Voice/rulesets/123":
    result = json.loads(Path(os.environ["WORKFLOW_RULE"]).read_text())
elif path == "repos/Concord-Voice/Self-Hosted/commits/main":
    result = {"sha": "a" * 40}
elif path == "repos/Concord-Voice/Self-Hosted/actions/workflows/required-pr-build.yml":
    result = {"id": 369488263, "path": ".github/workflows/required-pr-build.yml"}
elif path == "repos/Concord-Voice/Self-Hosted/pulls?state=open&per_page=100":
    result = json.loads(Path(os.environ["OPEN_PRS"]).read_text()) if os.environ.get("OPEN_PRS") else []
elif path.endswith("actions/variables/" + "CI_SCAN_RELEVANCE_MODE"):
    result = {"name": "CI_SCAN_RELEVANCE_MODE", "value": "required-full"}
else:
    raise SystemExit("unexpected synthetic gh API path: " + path)
if "--jq" in args:
    filter_expr = args[args.index("--jq") + 1]
    filtered = subprocess.run(["jq", "-c", filter_expr], input=json.dumps(result),
                              text=True, capture_output=True, check=True)
    sys.stdout.write(filtered.stdout)
else:
    print(json.dumps(result))
''')
    fake_gh.chmod(0o755)
    org_rule = {
        "id": 17153062, "name": "Default Branch Ruleset", "target": "branch",
        "enforcement": "active",
        "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []},
                       "repository_name": {"include": ["~ALL"], "exclude": []}},
        "bypass_actors": [{"actor_id": 7, "actor_type": "Integration", "bypass_mode": "always"}],
        "rules": [
            {"type": "pull_request", "parameters": {"required_approving_review_count": 1}},
            {"type": "code_scanning", "parameters": {"code_scanning_tools": [
                {"tool": "CodeQL", "alerts_threshold": "errors_and_warnings", "security_alerts_threshold": "medium_or_higher"},
                {"tool": "Semgrep", "alerts_threshold": "errors_and_warnings", "security_alerts_threshold": "medium_or_higher"},
                {"tool": "OSV-Scanner", "alerts_threshold": "errors", "security_alerts_threshold": "high_or_higher"},
                {"tool": "GovulnCheck", "alerts_threshold": "errors", "security_alerts_threshold": "high_or_higher"}] }},
            {"type": "required_status_checks", "parameters": {"required_status_checks": [
                {"context": "SonarCloud Code Analysis"},
                {"context": "SonarCloud Organization Compliance"}]}},
            {"type": "code_quality", "parameters": {"severity": "warnings"}},
        ],
    }
    repo_rule = json.loads(Path(root, "infrastructure/github/alpha-code-scanning-ruleset.json").read_text())
    repo_rule.update({"id": 17541948})
    effective_pages = [
        [{**rule, "ruleset_id": 17153062} for rule in org_rule["rules"]],
        [{**rule, "ruleset_id": 17541948} for rule in repo_rule["rules"]] + [
            {"type": "workflows", "ruleset_id": 123, "parameters": {"workflows": [
                {"repository_id": 1158152129, "path": ".github/workflows/required-pr-build.yml",
                 "ref": "refs/heads/main"}]}},
            {"type": "file_path_restriction", "ruleset_id": 24555855,
             "parameters": {"ignored_file_paths": [], "restricted_file_paths": [".github/workflows/**/*"]}}],
    ]
    effective = effective_pages[0] + effective_pages[1]
    workflow_rule = json.loads(Path(root, "infrastructure/github/alpha-required-workflow-ruleset.json").read_text())
    workflow_rule.update({"id": 123, "updated_at": "2026-10-08T12:00:00.987Z"})
    workflow_source_rule = {"id": 24555855, "name": "Global Workflow Definition Custody",
        "target": "push", "enforcement": "active",
        "conditions": {"repository_name": {"exclude": [], "include": ["~ALL"]}},
        "bypass_actors": [{"actor_id": 29110, "actor_type": "Integration", "bypass_mode": "always"},
            {"actor_id": 19157787, "actor_type": "Team", "bypass_mode": "always"}],
        "rules": [{"type": "file_path_restriction", "parameters": {
            "ignored_file_paths": [], "restricted_file_paths": [".github/workflows/**/*"]}}]}
    inputs = {}
    for name, value in (("ORG_RULE", org_rule), ("REPO_RULE", repo_rule),
                        ("EFFECTIVE", effective), ("EFFECTIVE_PAGES", effective_pages),
                        ("WORKFLOW_RULE", workflow_rule), ("WORKFLOW_SOURCE_RULE", workflow_source_rule)):
        source = tmp / f"{name}.json"
        source.write_text(json.dumps(value))
        inputs[name] = str(source)
    snapshots = tmp / "reviewed"
    env = {**os.environ, **inputs, "PATH": f"{tmp}:{os.environ['PATH']}",
           "GH_BIN": str(fake_gh), "GH_CALLS": str(calls)}
    # The production script currently uses gh directly; put the shim first.
    run = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir", str(snapshots)],
                         cwd=root, env=env, capture_output=True, text=True)
    assert run.returncode == 0, (run, calls.read_text() if calls.exists() else "no calls")
    assert "Read-only snapshots written" in run.stdout
    requests = [json.loads(line) for line in calls.read_text().splitlines()]
    assert not any("-X" in request for request in requests), requests
    assert ["api", "-H", "Time-Zone: UTC", "orgs/Concord-Voice/rulesets/123"] in requests, requests
    effective_reads = [request for request in requests if any("rules/branches/main" in arg for arg in request)]
    assert effective_reads and all("--paginate" in request for request in effective_reads), effective_reads
    assert not (snapshots / "proposal-alpha-ruleset.json").exists()
    assert not (snapshots / "proposal-universal-ruleset.json").exists()
    effective_snapshot = json.loads((snapshots / "effective-branch-rules.json").read_text())
    assert effective_snapshot == effective, effective_snapshot
    assert any(rule["type"] == "required_status_checks" for rule in effective_snapshot)
    assert any(rule["type"] == "code_quality" for rule in effective_snapshot)
    assert any(rule["type"] == "workflows" for rule in effective_snapshot)
    workflow_source_snapshot = json.loads((snapshots / "org-workflow-custody-24555855.json").read_text())
    assert workflow_source_snapshot == workflow_source_rule

    invalid_custody = json.loads(json.dumps(workflow_source_rule))
    invalid_custody["conditions"]["ref_name"] = {"include": ["refs/heads/main"], "exclude": []}
    invalid_custody_path = tmp / "invalid-workflow-custody.json"
    invalid_custody_path.write_text(json.dumps(invalid_custody))
    invalid_custody_calls = tmp / "invalid-workflow-custody-calls.jsonl"
    invalid_custody_run = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir",
        str(tmp / "invalid-workflow-custody")], cwd=root,
        env={**env, "WORKFLOW_SOURCE_RULE": str(invalid_custody_path),
             "GH_CALLS": str(invalid_custody_calls)}, capture_output=True, text=True)
    assert invalid_custody_run.returncode != 0, invalid_custody_run
    assert "workflow custody push rule is missing or changed" in invalid_custody_run.stderr, invalid_custody_run.stderr
    invalid_custody_requests = [json.loads(line) for line in invalid_custody_calls.read_text().splitlines()]
    assert not any("-X" in request for request in invalid_custody_requests), invalid_custody_requests

    def relevance_with(name, **overrides):
        paths = {}
        for key, value in overrides.items():
            paths[key] = tmp / f"{name}-{key}.json"
            paths[key].write_text(json.dumps(value))
        call_log = tmp / f"{name}-calls.jsonl"
        outcome = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir", str(tmp / name)],
            cwd=root, env={**env, **{key: str(path) for key, path in paths.items()},
                           "GH_CALLS": str(call_log)}, capture_output=True, text=True)
        assert not any("-X" in json.loads(line) for line in call_log.read_text().splitlines()), outcome
        return outcome

    # The rule list has no meaningful order, and the committed explicit ref is part of the contract.
    reordered_rule = json.loads(json.dumps(workflow_rule))
    reordered_rule["rules"].reverse()
    reordered = relevance_with("rules-reordered", WORKFLOW_RULE=reordered_rule)
    assert reordered.returncode == 0, reordered
    # The API may omit a default-false parameter; the committed contract states it.
    omitted_rule = json.loads(json.dumps(workflow_rule))
    status_rule = next(rule for rule in omitted_rule["rules"] if rule["type"] == "required_status_checks")
    assert status_rule["parameters"].pop("do_not_enforce_on_create") is False
    omitted = relevance_with("default-omitted", WORKFLOW_RULE=omitted_rule)
    assert omitted.returncode == 0, omitted
    previous_rule = json.loads(json.dumps(workflow_rule))
    previous_rule["rules"] = [rule for rule in previous_rule["rules"] if rule["type"] == "workflows"]
    previous_rule["rules"][0]["parameters"]["workflows"][0]["ref"] = "main"
    previous = relevance_with("rules-previous-state", WORKFLOW_RULE=previous_rule)
    assert previous.returncode != 0 and "differs from committed Alpha contract" in previous.stderr, previous
    short_ref_pages = json.loads(json.dumps(effective_pages))
    for page in short_ref_pages:
        for rule in page:
            if rule["type"] == "workflows":
                rule["parameters"]["workflows"][0]["ref"] = "main"
    short_ref = relevance_with("effective-short-ref", EFFECTIVE_PAGES=short_ref_pages)
    assert short_ref.returncode != 0 and "not effective on main" in short_ref.stderr, short_ref

    mismatched_org = json.loads(json.dumps(org_rule))
    codeql_tool = next(tool for rule in mismatched_org["rules"] if rule["type"] == "code_scanning"
                       for tool in rule["parameters"]["code_scanning_tools"] if tool["tool"] == "CodeQL")
    codeql_tool["security_alerts_threshold"] = "critical_only"
    mismatched_org_path = tmp / "org-codeql-drift.json"
    mismatched_org_path.write_text(json.dumps(mismatched_org))
    mismatch_calls = tmp / "org-codeql-drift-calls.jsonl"
    mismatch_run = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir", str(tmp / "org-codeql-drift")],
        cwd=root, env={**env, "ORG_RULE": str(mismatched_org_path), "GH_CALLS": str(mismatch_calls)},
        capture_output=True, text=True)
    assert mismatch_run.returncode != 0 and "CodeQL native requirement" in mismatch_run.stderr, mismatch_run
    assert not any("-X" in json.loads(line) for line in mismatch_calls.read_text().splitlines()), mismatch_run
    wrapped_calls = tmp / "wrapped-calls.jsonl"
    wrapped_run = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir", str(tmp / "wrapped-live")],
        cwd=root, env={**env, "LIVE_EFFECTIVE_WRAPPED": "1", "GH_CALLS": str(wrapped_calls)},
        capture_output=True, text=True)
    assert wrapped_run.returncode != 0 and "cannot read complete effective main rules" in wrapped_run.stderr, wrapped_run
    assert not any("-X" in json.loads(line) for line in wrapped_calls.read_text().splitlines()), wrapped_run
    mixed_empty_initial_failures = []
    for order in ("empty-first", "empty-last"):
        mixed_calls = tmp / f"mixed-empty-{order}.jsonl"
        mixed_run = subprocess.run(["bash", str(relevance_apply), "--snapshot-dir", str(tmp / f"mixed-{order}")],
            cwd=root, env={**env, "LIVE_EFFECTIVE_MIXED_EMPTY": order, "GH_CALLS": str(mixed_calls)},
            capture_output=True, text=True)
        writes = [json.loads(line) for line in mixed_calls.read_text().splitlines()
                  if "-X" in json.loads(line)]
        if mixed_run.returncode == 0 or writes:
            mixed_empty_initial_failures.append((order, mixed_run.returncode, writes, mixed_run.stderr))
    assert json.loads((snapshots / "open-pr-heads.json").read_text()) == []
    assert "branch-protection.json" not in relevance_apply.read_text(), "never read the stale committed org template"
    native_spec = json.loads((root / "infrastructure/github/alpha-code-scanning-ruleset.json").read_text())
    native_codeql = [tool for rule in native_spec["rules"] if rule["type"] == "code_scanning"
                     for tool in rule["parameters"]["code_scanning_tools"] if tool["tool"] == "CodeQL"]
    assert native_codeql == [{"tool": "CodeQL", "alerts_threshold": "errors_and_warnings",
                              "security_alerts_threshold": "medium_or_higher"}], native_codeql
    org_codeql = [tool for rule in org_rule["rules"] if rule["type"] == "code_scanning"
                  for tool in rule["parameters"]["code_scanning_tools"] if tool["tool"] == "CodeQL"]
    assert org_codeql == native_codeql, org_codeql
    assert not mixed_empty_initial_failures, mixed_empty_initial_failures
    bad_mode = subprocess.run(["bash", str(relevance_apply), "--bogus", "--snapshot-dir", str(snapshots)],
        cwd=root, env=env, capture_output=True, text=True)
    assert bad_mode.returncode == 2 and "unknown argument" in bad_mode.stderr, bad_mode
    no_evidence = subprocess.run(["bash", str(relevance_apply), "--apply", "--snapshot-dir", str(snapshots)],
        cwd=root, env=env, capture_output=True, text=True)
    assert no_evidence.returncode == 2 and "lasting --apply cutover is disabled" in no_evidence.stderr, no_evidence

    # Synthetic evidence binds every current-head canary to a protected run,
    # exact changed paths, complete job inventory, and native CodeQL check.
    case_contract = [("docs-baseline-pass", "pass")]
    case_contract += [(f"selector-mask-{mask:02d}", "pass") for mask in range(1, 29)]
    case_contract += [("failed-build-blocked", "blocked")]
    selector_files = ["client/desktop/src/renderer/config.ts",
        "services/control-plane/pkg/clientversion/version.go",
        "services/media-plane/src/lib/activeSpeakerSet.ts",
        "client/admin/src/formatMetric.ts",
        "docs/adr/0018-ci-workflow-orchestration.md"]
    main_sha = "a" * 40
    pinned_pr_ci_sha = "cb10844a7860092011f96b4607682ef2e6c55a80"  # pragma: allowlist secret -- public workflow commit
    full_pr = {"number": 100, "head": {"sha": "e" * 40}, "base": {"sha": main_sha}}
    prs = [full_pr]
    heads = []
    canaries = []
    run_data = {}
    pr_data = {}
    pr_files = {}
    for index, (case_id, protected) in enumerate(case_contract, start=1):
        number = index
        head_sha = f"{index:040x}"
        check_sha = f"{index + 1000:040x}"
        run_id = 2000 + index
        conclusion = "success" if protected == "pass" else "failure"
        prs.append({"number": number, "head": {"sha": head_sha}, "base": {"sha": main_sha}})
        heads.append({"number": number, "head_sha": head_sha, "protected_run_id": run_id,
                      "run_attempt": 1, "check_sha": check_sha, "conclusion": conclusion})
        canary = {"case_id": case_id, "number": number, "head_sha": head_sha,
            "protected_run_id": run_id, "run_attempt": 1, "protected_observed": protected}
        canaries.append(canary)
        if case_id == "docs-baseline-pass":
            changed = ["docs/ci/canaries/2026-10-11-required-workflow-added-pass.md"]
        elif case_id == "failed-build-blocked":
            changed = ["client/admin/src/protectedBuildFailureCanary.ts"]
        else:
            mask = int(case_id.rsplit("-", 1)[1])
            changed = [path for bit, path in enumerate(selector_files) if mask & (1 << bit)]
        pr_files[str(number)] = [{"filename": path,
            "status": "modified" if case_id.startswith("selector-mask-") else "added"} for path in changed]
        run_data[str(run_id)] = {"id": run_id, "run_attempt": 1, "status": "completed", "workflow_id": 380888328,
            "created_at": "2026-10-08T12:00:01Z" if case_id == "docs-baseline-pass" else "2026-10-09T12:00:00Z",
            "workflow_url": "https://api.github.com/repos/Concord-Voice/Self-Hosted/actions/required_workflows/380888328",
            "path": ".github/workflows/required-pr-build.yml", "event": "pull_request",
            "head_sha": head_sha, "conclusion": conclusion, "check_suite_id": run_id + 1000,
            "repository": {"full_name": "Concord-Voice/Self-Hosted"},
            "pull_requests": [{"number": number, "head": {"sha": head_sha, "repo": {"id": 1158152129}},
                               "base": {"sha": main_sha, "ref": "main", "repo": {"id": 1158152129}}}],
            "referenced_workflows": [{"path": f"Concord-Voice/Self-Hosted/.github/workflows/pr-ci.yml@{pinned_pr_ci_sha}",
                "sha": pinned_pr_ci_sha}]}
        pr_data[str(number)] = {"number": number, "state": "open", "head": {"ref": f"canary-{number}", "sha": head_sha,
            "repo": {"id": 1158152129}},
            "base": {"ref": "main", "repo": {"id": 1158152129,
                "full_name": "Concord-Voice/Self-Hosted"}, "sha": main_sha},
            "merge_commit_sha": check_sha, "changed_files": len(changed)}
    evidence = {"schema_version": 3, "repository_id": 1158152129, "main_sha": main_sha,
        "protected": {"ruleset_id": 123, "source_path": ".github/workflows/required-pr-build.yml",
            "source_sha": main_sha, "mode": "required-full", "run_id": 2001,
            "run_attempt": 1, "conclusion": "success"},
        "open_pr_heads": heads, "canaries": canaries}
    apply_prs = tmp / "apply-prs.json"
    apply_prs.write_text(json.dumps(prs))
    evidence_path = tmp / "canaries.json"
    evidence_path.write_text(json.dumps(evidence))
    run_path = tmp / "runs.json"
    for run in run_data.values():
        run["path"] = ".github/workflows/required-pr-build.yml@main"
    run_path.write_text(json.dumps(run_data))
    pr_path = tmp / "pr-details.json"
    pr_files_path = tmp / "pr-files.json"
    pr_files_path.write_text(json.dumps(pr_files))
    pr_path.write_text(json.dumps(pr_data))
    apply_snapshots = tmp / "apply-reviewed"
    env["OPEN_PRS"] = str(apply_prs)
    run = subprocess.run(["bash", str(relevance_apply), "--dry-run", "--snapshot-dir", str(apply_snapshots)],
                         cwd=root, env=env, capture_output=True, text=True)
    assert run.returncode == 0, run
    live_heads = json.loads((apply_snapshots / "open-pr-heads.json").read_text())
    assert {(entry["number"], entry["head_sha"]) for entry in heads}.issubset(
        {(entry["number"], entry["head_sha"]) for entry in live_heads})
    assert len(heads) == 30 and len(live_heads) == 31  # Unrelated PR needs no protected-run receipt.
    assert len(canaries) == 30 and len({item["case_id"] for item in canaries}) == 30
    apply_fake = tmp / "apply-gh"
    apply_fake.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
a = sys.argv[1:]
state_path = Path(os.environ["APPLY_STATE"])
state = json.loads(state_path.read_text())
calls = Path(os.environ["APPLY_CALLS"])
with calls.open("a") as f: f.write(json.dumps(a) + "\\n")
path = next((x for x in a if x.startswith(("orgs/", "repos/"))), "")
case = os.environ.get("APPLY_CASE", "success")
def save(): state_path.write_text(json.dumps(state))
def output(value):
    if "--jq" in a:
        import subprocess
        print(subprocess.run(["jq", "-c", a[a.index("--jq") + 1]], input=json.dumps(value),
            text=True, capture_output=True, check=True).stdout, end="")
    else: print(json.dumps(value))
if a[:2] == ["api", "-X"]:
    verb, endpoint = a[2], a[3]
    payload = json.loads(sys.stdin.read()) if "--input" in a and a[a.index("--input") + 1] == "-" else {}
    if verb == "POST" and endpoint.endswith("/rerun"):
        run_id = endpoint.split("/actions/runs/")[1].split("/")[0]
        runs_path = Path(os.environ["RUNS_JSON"])
        runs = json.loads(runs_path.read_text())
        run = runs[run_id]
        relevant_mode = state["variable"]["value"].startswith("required-relevant:")
        if case == "shadow-success" and relevant_mode:
            # GitHub accepted the rerun, but the new attempt is not visible yet.
            state["pending_attempt"] = int(run_id)
            state["pending_attempt_polls"] = 0
            state["affected_run"] = int(run_id)
            save(); output({}); raise SystemExit(0)
        run["run_attempt"] += 1
        run["status"] = "in_progress" if case == "shadow-timeout" and relevant_mode else "completed"
        run["conclusion"] = "failure" if case == "shadow-relevant-fails" and relevant_mode else "success"
        run["created_at"] = "2026-10-12T12:00:00Z"
        runs_path.write_text(json.dumps(runs))
        state["affected_run"] = int(run_id)
        save(); output({})
    elif verb == "POST" and endpoint == "orgs/Concord-Voice/rulesets":
        payload["id"] = 9001; state["alpha"] = payload; save(); output({"id":9001})
    elif verb == "PUT" and endpoint == "orgs/Concord-Voice/rulesets/17153062":
        payload["id"] = 17153062; state["org"] = payload; save(); output(state["org"])
    elif verb == "PUT" and endpoint == "orgs/Concord-Voice/rulesets/9001":
        payload["id"] = 9001; state["alpha"] = payload; save(); output(state["alpha"])
    elif verb == "DELETE" and endpoint == "repos/Concord-Voice/Self-Hosted/rulesets/17541948":
        state["repo_present"] = False; save(); output({})
    elif verb == "POST" and endpoint == "repos/Concord-Voice/Self-Hosted/rulesets":
        payload["id"] = 8001; state["repo"] = payload; state["repo_present"] = True; save(); output({"id":8001})
    elif verb == "DELETE" and endpoint == "orgs/Concord-Voice/rulesets/9001":
        state["alpha"] = None; save(); output({})
    elif verb == "PATCH" and endpoint.endswith("actions/variables/" + "CI_SCAN_RELEVANCE_MODE"):
        if case == "shadow-full-restore-fails" and payload.get("value") == "required-full":
            raise SystemExit(1)
        if case == "shadow-final-mode-drift" and payload.get("value") == "required-full":
            state["restored_full_reads_at_patch"] = state.get("variable_reads", 0)
        state["variable"] = payload; save(); output(payload)
    else: raise SystemExit("unexpected mutation: " + repr(a))
    raise SystemExit(0)
if path == "orgs/Concord-Voice/rulesets/17153062":
    value = dict(state["org"])
    if case == "org-drift": value["name"] = "admin changed"
elif path == "orgs/Concord-Voice/rulesets/24555855":
    value = dict(state["workflow_source_rule"])
    if case == "workflow-custody-drift": value["name"] = "unexpected policy"
    if case == "workflow-custody-extra-condition":
        value["conditions"]["ref_name"] = {"include": ["refs/heads/main"], "exclude": []}
elif path in {"repos/Concord-Voice/Self-Hosted/rulesets/17541948", "repos/Concord-Voice/Self-Hosted/rulesets/8001"}:
    if not state["repo_present"]: raise SystemExit(1)
    value = state["repo"]
    tamper = os.environ.get("APPLY_TAMPER", "")
    if tamper.startswith("threshold:"):
        _, tool_name, field = tamper.split(":", 2)
        tool = next(item for item in value["rules"][0]["parameters"]["code_scanning_tools"]
                    if item["tool"] == tool_name)
        tool[field] = "unexpected"
    elif tamper == "extra-setting":
        value["rules"][0]["parameters"]["code_scanning_tools"][0]["unexpected_setting"] = "x"
elif path == "repos/Concord-Voice/Self-Hosted/rules/branches/main":
    baseline = [rule for page in state["effective_pages"] for rule in page
                if rule["ruleset_id"] not in {17153062, 17541948, 9001}]
    current = []
    if "Self-Hosted" not in state["org"]["conditions"]["repository_name"].get("exclude", []):
        current += [{**rule, "ruleset_id": 17153062} for rule in state["org"]["rules"]]
    if state["repo_present"]:
        current += [{**rule, "ruleset_id": 17541948} for rule in state["repo"]["rules"]]
    if state.get("alpha") and state["alpha"]["enforcement"] == "active":
        current += [{**rule, "ruleset_id": 9001} for rule in state["alpha"]["rules"]]
    current += baseline
    current = json.loads(json.dumps(current))
    if case == "shadow-effective-drift" and state.get("full_replay_job_read"):
        current = [rule for rule in current if rule.get("type") != "code_quality"]
    if case == "effective-native-codeql-missing":
        current = [rule for rule in current if not (
            rule.get("ruleset_id") == 9001 and rule.get("type") == "code_scanning" and
            any(tool.get("tool") == "CodeQL" for tool in
                rule.get("parameters", {}).get("code_scanning_tools", []))) ]
    if case == "effective-native-codeql-weakened":
        for rule in current:
            if rule.get("ruleset_id") == 9001 and rule.get("type") == "code_scanning":
                for tool in rule.get("parameters", {}).get("code_scanning_tools", []):
                    if tool.get("tool") == "CodeQL":
                        tool["alerts_threshold"] = "errors"
    if case == "effective-native-repo-missing" and state.get("alpha") and state["alpha"]["enforcement"] == "active":
        current = [rule for rule in current if not (
            rule.get("ruleset_id") == 17541948 and rule.get("type") == "code_scanning")]
    if case == "effective-native-repo-weakened" and state.get("alpha") and state["alpha"]["enforcement"] == "active":
        for rule in current:
            if rule.get("ruleset_id") == 17541948 and rule.get("type") == "code_scanning":
                for tool in rule.get("parameters", {}).get("code_scanning_tools", []):
                    if tool.get("tool") == "Semgrep OSS":
                        tool["alerts_threshold"] = "errors"
    if state["variable"]["value"] == "required-relevant":
        if case == "preserved-rule-dropped":
            current = [rule for rule in current if rule["type"] != "file_path_restriction"]
        elif case == "alpha-rule-weakened":
            current = [rule for rule in current if rule["type"] != "pull_request"]
    pages = [current[:len(current)//2], current[len(current)//2:]]
    if case == "effective-quality-missing" and state["variable"]["value"] == "required-relevant":
        pages = [pages[0], [rule for rule in pages[1] if rule["type"] != "code_quality"]]
    if case.startswith("mixed-empty-final-") and state["variable"]["value"] == "required-relevant":
        populated = [rule for page in pages for rule in page]
        pages = ([[], populated] if case == "mixed-empty-final-first" else [populated, []])
        Path(os.environ["APPLY_EFFECTIVE_INJECTED"]).write_text(case)
    if "--paginate" in a:
        for page in pages:
            print(json.dumps(page))
        raise SystemExit(0)
    value = pages[0]
elif path == "orgs/Concord-Voice/rulesets?per_page=100":
    value = [state["workflow_rule"]]
    if state.get("alpha"): value.append({"id":9001,"name":"Alpha Scan Relevance"})
    if case == "protected-missing": value = []
    if case == "duplicate": value.append({"id":9002,"name":"Alpha Scan Relevance"})
elif path == "orgs/Concord-Voice/rulesets/123":
    state["workflow_rule_reads"] = state.get("workflow_rule_reads", 0) + 1; save()
    value = json.loads(json.dumps(state["workflow_rule"]))
    if ((case == "workflow-rule-drift-before-delete" and state["workflow_rule_reads"] >= 3) or
            (case == "workflow-rule-drift-final" and state["workflow_rule_reads"] >= 4)):
        value["updated_at"] = "2026-10-09T12:00:00Z"
elif path == "orgs/Concord-Voice/rulesets/9001": value = state["alpha"]
elif path == "repos/Concord-Voice/Self-Hosted/commits/main":
    state["main_reads"] = state.get("main_reads", 0) + 1; save()
    value = {"sha": ("b" if case == "main-moved-before-write" and state["main_reads"] >= 2 else "a") * 40}
elif path == "repos/Concord-Voice/Self-Hosted/actions/workflows/required-pr-build.yml": value = {"id":369488263,"path":".github/workflows/required-pr-build.yml"}
elif path.startswith("repos/Concord-Voice/Self-Hosted/actions/runs?"):
    if "status=" in path:
        value = {"workflow_runs": []}
    else:
        runs = json.loads(Path(os.environ["RUNS_JSON"]).read_text())
        recent_runs = []
        for run_id in (state.get("affected_run"), state.get("concurrent_run")):
            if run_id is not None:
                recent_run = runs[str(run_id)].copy()
                recent_run["path"] = ".github/workflows/required-pr-build.yml"
                recent_runs.append(recent_run)
        value = {"workflow_runs": recent_runs}
elif path.startswith("repos/Concord-Voice/Self-Hosted/actions/jobs/") and path.endswith("/logs"):
    job_id = int(path.split("/jobs/")[1].split("/")[0])
    run_id = str(job_id // 100)
    run = json.loads(Path(os.environ["RUNS_JSON"]).read_text())[run_id]
    pr = json.loads(Path(os.environ["PR_DETAILS"]).read_text())[str(run["pull_requests"][0]["number"])]
    mode = "required-relevant" if state["variable"]["value"].startswith("required-relevant:") else state["variable"]["value"]
    if case == "shadow-wrong-mode" and mode == "required-relevant": mode = "required-full"
    skipped = ("codeql_actions, codeql_go, codeql_js, eslint, govulncheck, osv, semgrep_canary, semgrep_custom, sonar, sonar_admin, sonar_control_plane, sonar_desktop, sonar_force_all, sonar_media_plane"
               if mode == "required-relevant" else "none")
    selected = "semgrep_community" if mode == "required-relevant" else "all"
    print(f"policy=cb10844a7860092011f96b4607682ef2e6c55a80 head={run['head_sha']} merge={pr['merge_commit_sha']} mode={mode} selected={selected} skipped={skipped}")
    raise SystemExit(0)
elif path.endswith("actions/variables/" + "CI_SCAN_RELEVANCE_MODE"):
    state["variable_reads"] = state.get("variable_reads", 0) + 1; save()
    if case == "rollback-readfail" and state.get("relevant_patch_failed"): raise SystemExit(1)
    value = state["variable"]
    if case == "shadow-final-mode-drift" and state.get("restored_full_reads_at_patch") is not None and \
            state["variable_reads"] > state["restored_full_reads_at_patch"] + 1:
        value = {**value, "value": "required-relevant:2001:3:9999999999"}
elif path == "repos/Concord-Voice/Self-Hosted/pulls?state=open&per_page=100":
    state["open_pr_reads"] = state.get("open_pr_reads", 0) + 1; save()
    value = json.loads(Path(os.environ["OPEN_PRS"]).read_text())
    if case == "head-moved-before-write" and state["open_pr_reads"] >= 2:
        value[0]["head"]["sha"] = "b" * 40
elif "/attempts/" in path and "/jobs?per_page=100" in path:
    attempt = int(path.split("/attempts/")[1].split("/", 1)[0])
    run_id = int(path.split("/actions/runs/")[1].split("/")[0])
    names = ["detect-changes", "report", "sonar / SonarCloud", "eslint / ESLint SARIF",
        "semgrep / Semgrep SAST", "govulncheck / govulncheck (control-plane)",
        "build / Desktop / Test + Coverage (shard 1)",
        "build / Desktop / Test + Coverage (shard 2)",
        "build / Control Plane / Test + Coverage (shard 1)",
        "build / Control Plane / Test + Coverage (shard 2)",
        "build / Media Plane / Test + Coverage", "build / Admin Portal / Test + Coverage",
        "build / Desktop / Merge Coverage", "build / Control Plane / Merge Coverage",
        "build / Production-compose / Docker build (cache-cold)",
        "osv / protected-pr-scan"]
    names = ["Protected OSV dependency scan / " + name.split(" / ", 1)[1]
             if name.startswith("osv / ") else "Protected scan enforcement / " + name
             for name in names]
    names.append("Required PR build result")
    if case == "full-mode-inventory-missing": names.remove("Protected scan enforcement / semgrep / Semgrep SAST")
    if case == "full-mode-inventory-foreign-pr-ci-family":
        names = [name.replace("Protected scan enforcement / ", "Unprotected scan enforcement / ")
                 for name in names]
    if case == "full-mode-inventory-foreign-osv-name":
        names = [name.replace("Protected OSV dependency scan / protected-pr-scan",
                              "Protected OSV dependency scan / other-scan") for name in names]
    jobs = [{"id": run_id * 100 + index, "name": name,
             "conclusion": "failure" if name == "Required PR build result" and
                 case == "shadow-relevant-fails" and run_id == 2001 and attempt == 2 else "success",
             "status": "completed",
             "run_attempt": 2 if case == "full-mode-inventory-stale" else attempt} for index, name in enumerate(names)]
    semgrep_job = next((job for job in jobs if job["name"] ==
        "Protected scan enforcement / semgrep / Semgrep SAST"), None)
    if semgrep_job is not None:
        semgrep_job["steps"] = [{"name": name, "conclusion": "success"} for name in (
            "Run Semgrep (custom rules)", "Run Semgrep (community rulesets)",
            "Canary — mutable-action-ref detector is still live")]
        if case.startswith("full-mode-semgrep-step-skipped-"):
            index = int(case.rsplit("-", 1)[1])
            semgrep_job["steps"][index]["conclusion"] = "skipped"
    if state["variable"]["value"].startswith("required-relevant:") and attempt == 2:
        for job in jobs:
            if job["name"].startswith(("Protected scan enforcement / eslint", "Protected scan enforcement / govulncheck", "Protected OSV dependency scan")):
                job["conclusion"] = "success" if case == "shadow-summary-job-mismatch" and "eslint" in job["name"] else "skipped"
            if job["name"].startswith("Protected scan enforcement / sonar"):
                job["conclusion"] = "skipped"
            if job["name"].startswith("Protected scan enforcement / build /") and "Coverage" in job["name"]:
                job["conclusion"] = "skipped"
            if job["name"] == "Protected scan enforcement / semgrep / Semgrep SAST":
                job["conclusion"] = "success"
                job["steps"] = [
                    {"name": "Run Semgrep (community rulesets)", "conclusion": "success"},
                    {"name": "Run Semgrep (custom rules)", "conclusion": "skipped"},
                    {"name": "Canary — mutable-action-ref detector is still live", "conclusion": "skipped"},
                ]
                if case == "shadow-custom-step-ran":
                    job["steps"][1]["conclusion"] = "success"
                if case == "shadow-canary-step-ran":
                    job["steps"][2]["conclusion"] = "success"
                if case == "shadow-community-step-skipped":
                    job["steps"][0]["conclusion"] = "skipped"
        if case == "shadow-relevant-sonar-ran":
            next(job for job in jobs if job["name"] ==
                "Protected scan enforcement / sonar / SonarCloud")["conclusion"] = "success"
        if case == "shadow-relevant-coverage-ran":
            next(job for job in jobs if job["name"] ==
                "Protected scan enforcement / build / Desktop / Test + Coverage (shard 1)")["conclusion"] = "success"
    if case == "full-mode-inventory-skipped":
        next(job for job in jobs if job["name"] ==
             "Protected scan enforcement / semgrep / Semgrep SAST")["conclusion"] = "skipped"
    if case == "full-mode-inventory-osv-skipped":
        next(job for job in jobs if job["name"] ==
             "Protected OSV dependency scan / protected-pr-scan")["conclusion"] = "skipped"
    if run_id == 2030:
        for name in ("build / Admin Portal / Test + Coverage",
                     "build / Production-compose / Docker build (cache-cold)"):
            if case in ("blocked-only-admin-fails", "canary-admin-only-fails") and "Docker build" in name: continue
            if case in ("blocked-only-docker-fails", "canary-docker-only-fails") and "Admin Portal" in name: continue
            next(job for job in jobs if job["name"] == "Protected scan enforcement / " + name)["conclusion"] = "failure"
    if run_id == 2001 and attempt == 3:
        state["full_replay_job_read"] = True
        save()
    value = {"total_count": len(jobs) + (1 if case == "full-mode-inventory-truncated" else 0),
             "jobs": jobs}
    if "--paginate" in a:
        print(json.dumps(value))
        raise SystemExit(0)
elif path.startswith("repos/Concord-Voice/Self-Hosted/actions/runs/"):
    runs_path = Path(os.environ["RUNS_JSON"])
    runs = json.loads(runs_path.read_text())
    run_id = path.rsplit("/",1)[1]
    if case == "shadow-success" and state.get("pending_attempt") == int(run_id):
        state["pending_attempt_polls"] += 1
        if state["pending_attempt_polls"] >= 2:
            runs[run_id]["run_attempt"] += 1
            runs[run_id]["status"] = "completed"
            runs[run_id]["conclusion"] = "success"
            runs[run_id]["created_at"] = "2026-10-12T12:00:00Z"
            state.pop("pending_attempt", None)
            state["affected_run"] = int(run_id)
            runs_path.write_text(json.dumps(runs))
        save()
    value = runs[run_id]
    if case == "shadow-timeout" and state["variable"]["value"] == "required-full":
        value = dict(value); value["status"] = "completed"
elif path.startswith("repos/Concord-Voice/Self-Hosted/pulls/") and "/files?per_page=100" in path:
    number = path.split("/pulls/")[1].split("/")[0]
    value = json.loads(Path(os.environ["PR_FILES"]).read_text())[number]
    if case == "canary-wrong-file-set" and number == "1": value[0]["filename"] = "docs/other.md"
    if case == "canary-wrong-file-status" and number == "1": value[0]["status"] = "modified"
elif path.startswith("repos/Concord-Voice/Self-Hosted/pulls/"):
    value=json.loads(Path(os.environ["PR_DETAILS"]).read_text())[path.rsplit("/",1)[1]]
    value.setdefault("user", {"login": "pr-author", "type": "User"})
elif path.startswith("repos/Concord-Voice/Self-Hosted/compare/"):
    value={"base_commit":{"sha":"a"*40},"merge_base_commit":{"sha":"a"*40},"behind_by":0,"ahead_by":0,"total_commits":0,"status":"identical"}
elif ("/commits/" in path and ("/check-runs?filter=all" in path or
      ("/check-runs?filter=latest" in path and "&per_page=100" not in path))):
    check_sha = path.split("/commits/")[1].split("/")[0]
    details = json.loads(Path(os.environ["PR_DETAILS"]).read_text()).values()
    detail = next((d for d in details if d["head"]["sha"] == check_sha), None)
    if case == "run-check-only-on-merge":
        details = json.loads(Path(os.environ["PR_DETAILS"]).read_text()).values()
        detail = next((d for d in details if d["merge_commit_sha"] == check_sha), None)
    if detail is None:
        value={"check_runs":[]}
    else:
        run = next(r for r in json.loads(Path(os.environ["RUNS_JSON"]).read_text()).values()
                   if r["pull_requests"][0]["number"] == detail["number"])
        result_sha = detail["merge_commit_sha"] if case == "run-check-only-on-merge" else check_sha
        checks=[{"name":"Required PR build result","head_sha":result_sha,
            "conclusion":run["conclusion"],"check_suite":{"id":run["check_suite_id"]}}]
        if case == "canary-rerun-duplicate" and detail["number"] == 30 and "filter=all" in path:
            checks.append(dict(checks[0]))
        value={"check_runs":checks}
elif "/commits/" in path and "/check-runs?filter=latest&per_page=100" in path:
    head_sha = path.split("/commits/")[1].split("/")[0]
    detail = next(d for d in json.loads(Path(os.environ["PR_DETAILS"]).read_text()).values()
                  if d["head"]["sha"] == head_sha)
    native_pr = {"number": detail["number"], "head": {"sha": head_sha},
        "base": {"sha": detail["base"]["sha"], "ref": "main",
                 "repo": {"id": 1158152129}}}
    if case == "native-wrong-pr": native_pr["number"] += 1
    if case == "native-wrong-base": native_pr["base"]["sha"] = "0" * 40
    if case == "native-wrong-base-ref": native_pr["base"]["ref"] = "release"
    pull_requests = [] if case == "native-no-pr" else [native_pr]
    if case == "native-multiple-prs": pull_requests.append({**native_pr, "number": native_pr["number"] + 1})
    current_attempt = json.loads(Path(os.environ["RUNS_JSON"]).read_text())["2001"]["run_attempt"]
    native_drift = ((case == "shadow-native-attempt2-drift" and current_attempt == 2) or
                    (case == "shadow-native-final-drift" and current_attempt == 3))
    native_conclusion = "failure" if native_drift and state.get("affected_run") == 2001 else "success"
    value={"check_runs":[{"name":"CodeQL","app":{"id":57789,"slug":"github-advanced-security"},
        "head_sha":head_sha,"status":"completed","conclusion":native_conclusion,
        "pull_requests":pull_requests}]}
elif path == "repos/Concord-Voice/Self-Hosted/rulesets?per_page=100":
    value=[state["repo"]] if state["repo_present"] else []
else: raise SystemExit("unexpected read: " + path)
output(value)
''')
    apply_fake.chmod(0o755)
    apply_bin = tmp / "apply-bin"
    apply_bin.mkdir()
    (apply_bin / "gh").symlink_to(apply_fake)
    state_path = tmp / "apply-state.json"
    state = {"org": org_rule, "repo": repo_rule, "repo_present": True,
        "effective_pages": effective_pages, "workflow_rule": workflow_rule,
        "workflow_source_rule": workflow_source_rule, "alpha": None,
        "variable": {"name":"CI_SCAN_RELEVANCE_MODE","value":"required-full"}}
    apply_calls = tmp / "apply-calls"
    (apply_bin / "sleep").write_text("#!/bin/sh\nexit 0\n")
    (apply_bin / "sleep").chmod(0o755)

    def reset_shadow_state(case):
        current_state = json.loads(json.dumps(state))
        current_state["variable"] = {"name": "CI_SCAN_RELEVANCE_MODE", "value": "required-full"}
        current_state.pop("affected_run", None)
        current_state.pop("main_reads", None)
        current_state.pop("open_pr_reads", None)
        state_path.write_text(json.dumps(current_state))
        runs = json.loads(json.dumps(run_data))
        if case == "canary-rule-same-second":
            runs["2001"]["created_at"] = "2026-10-08T12:00:00Z"
        run_path.write_text(json.dumps(runs))
        pr_path.write_text(json.dumps(pr_data))
        evidence_path.write_text(json.dumps(evidence))
        apply_calls.unlink(missing_ok=True)
        return current_state

    def shadow_case(label, expected):
        reset_shadow_state(label)
        candidate = evidence
        if label == "shadow-bad-evidence":
            candidate = json.loads(json.dumps(evidence)); candidate["schema_version"] = 2
            evidence_path.write_text(json.dumps(candidate))
        case_env = {**env, "GH_BIN":str(apply_fake), "PATH":f"{apply_bin}:{os.environ['PATH']}",
            "OPEN_PRS":str(apply_prs), "RUNS_JSON":str(run_path), "PR_DETAILS":str(pr_path),
            "PR_FILES":str(pr_files_path), "APPLY_STATE":str(state_path),
            "APPLY_CALLS":str(apply_calls), "APPLY_CASE":label,
            "APPLY_TAMPER":"threshold:CodeQL:alerts_threshold" if label == "shadow-rule-drift" else ""}
        run = subprocess.run(["bash", str(relevance_apply), "--shadow-trial", "--snapshot-dir",
            str(apply_snapshots), "--canary-evidence", str(evidence_path), "--run-id", "2001"],
            cwd=root, env=case_env, capture_output=True, text=True)
        final = json.loads(state_path.read_text())
        calls = [json.loads(line) for line in apply_calls.read_text().splitlines()]
        writes = [call for call in calls if "-X" in call]
        assert not any("rulesets" in " ".join(call) for call in writes), (label, writes)
        assert final["repo"] == state["repo"] and final["org"] == org_rule, (label, final)
        assert final["repo_present"] is True, (label, final)

        if expected == "success":
            assert run.returncode == 0, (label, run.stdout, run.stderr, calls[-12:])
            assert final["variable"]["value"] == "required-full", final
            mode_writes = [call for call in writes if "CI_SCAN_RELEVANCE_MODE" in " ".join(call)]
            assert len(mode_writes) == 2 and run.stdout.count("VERIFIED:") >= 2, (label, writes, run.stdout)
            assert "required-relevant" in run.stdout and "required-full" in run.stdout, run.stdout
            reruns = [call for call in writes if call[2:4] == ["POST", "repos/Concord-Voice/Self-Hosted/actions/runs/2001/rerun"]]
            assert len(reruns) == 2, (label, reruns)
        elif expected == "restored-failure":
            assert run.returncode != 0, (label, run)
            assert final["variable"]["value"] == "required-full", (label, final)
            assert "RESTORED: CI_SCAN_RELEVANCE_MODE=required-full" in run.stderr, run.stderr
            assert "CRITICAL:" not in run.stderr, run.stderr
            assert len([call for call in writes if call[2:4] == ["POST", "repos/Concord-Voice/Self-Hosted/actions/runs/2001/rerun"]]) == 2, (label, writes)
        elif expected == "preflight":
            assert run.returncode != 0, (label, run)
            assert final["variable"]["value"] == "required-full", final
            assert not writes, (label, writes)
            if label == "shadow-bad-evidence":
                assert "canary evidence" in run.stderr, run.stderr
            elif label == "shadow-rule-drift":
                assert "live Alpha scanner ruleset changed" in run.stderr, run.stderr
            elif label == "canary-rule-same-second":
                assert "required workflow run source, PR head, attempt, or conclusion is not current" in run.stderr, run.stderr
            else:
                assert "canary run lacks expected full-mode scan and build jobs" in run.stderr, run.stderr
        elif expected == "critical":
            assert run.returncode != 0 and "CRITICAL:" in run.stderr, (label, run)
            assert final["variable"]["value"].startswith("required-relevant:"), final
            assert "required-full mode could not be restored" in run.stderr, run.stderr
        elif expected in ("native-critical", "effective-critical"):
            assert run.returncode != 0 and "CRITICAL: protected or native rules changed during the shadow trial" in run.stderr, (label, run.stderr)
            assert final["variable"]["value"] == "required-full", final
            assert len([call for call in writes if call[2:4] == ["POST", "repos/Concord-Voice/Self-Hosted/actions/runs/2001/rerun"]]) == 2, (label, writes)
            if expected == "native-critical":
                assert "native CodeQL check is missing, ambiguous, or stale for PR #1" in run.stderr, run.stderr
        elif expected == "native-restored-failure":
            assert run.returncode != 0, (label, run)
            assert final["variable"]["value"] == "required-full", final
            assert "RESTORED: CI_SCAN_RELEVANCE_MODE=required-full" in run.stderr, run.stderr
            assert "native CodeQL check is missing, ambiguous, or stale for PR #1" in run.stderr, run.stderr
            assert "CRITICAL:" not in run.stderr, run.stderr
            assert len([call for call in writes if call[2:4] == ["POST", "repos/Concord-Voice/Self-Hosted/actions/runs/2001/rerun"]]) == 2, (label, writes)
        elif expected == "final-drift":
            assert run.returncode != 0 and "CRITICAL: protected or native rules changed during the shadow trial" in run.stderr, (label, run)
            assert final["variable"]["value"] == "required-full", final

    shadow_cases = [
        ("shadow-success", "success"),
        ("canary-rerun-duplicate", "success"),
        ("canary-rule-same-second", "preflight"),
        # A timed-out post-patch attempt must restore full mode and replay it.
        ("shadow-timeout", "restored-failure"),
        ("shadow-custom-step-ran", "restored-failure"),
        ("shadow-canary-step-ran", "restored-failure"),
        ("shadow-community-step-skipped", "restored-failure"),
        ("shadow-relevant-sonar-ran", "restored-failure"),
        ("shadow-relevant-coverage-ran", "restored-failure"),
        ("shadow-effective-drift", "effective-critical"),
        ("shadow-native-attempt2-drift", "native-restored-failure"),
        ("shadow-native-final-drift", "native-critical"),
        ("shadow-final-mode-drift", "final-drift"),
        # These two distinct blocked-run checks exercise isolated build failures.
        ("canary-admin-only-fails", "preflight"),
        ("canary-docker-only-fails", "preflight"),
        ("full-mode-semgrep-step-skipped-0", "preflight"),
        ("full-mode-semgrep-step-skipped-1", "preflight"),
        ("full-mode-semgrep-step-skipped-2", "preflight"),
    ]
    if os.environ.get("SHADOW_TEST_CASE"):
        shadow_cases = [case for case in shadow_cases if case[0] == os.environ["SHADOW_TEST_CASE"]]
    for label, expected in shadow_cases:
        shadow_case(label, expected)

# Exercise the actual community step at the scanner process boundary. No scanner
# install, registry access, or repository checkout is needed for this contract.
community = next(step for step in semgrep_workflow["jobs"]["semgrep"]["steps"]
                 if step.get("name") == "Run Semgrep (community rulesets)")
with tempfile.TemporaryDirectory(prefix="semgrep-community-budget-") as temporary:
    tmp = Path(temporary)
    fake_semgrep = tmp / "semgrep"
    fake_semgrep.write_text("""#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ["SEMGREP_ARGS"]).write_text(json.dumps(sys.argv[1:]))
Path("semgrep-community.json").write_text(Path(os.environ["SEMGREP_REPORT"]).read_text())
raise SystemExit(int(os.environ.get("SEMGREP_EXIT", "0")))
""")
    fake_semgrep.chmod(0o755)
    clean_report = tmp / "clean.json"
    clean_report.write_text(json.dumps({"results": [], "errors": []}))
    error_report = tmp / "errors.json"
    error_report.write_text(json.dumps({"results": [], "errors": [{"message": "scanner failure"}]}))

    def run_community(label, report):
        work = tmp / label
        work.mkdir()
        output = work / "github-output"
        result = subprocess.run(
            ["bash", "-e", "-o", "pipefail", "-c", community["run"]], cwd=work,
            env={**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}",
                 "BASELINE_COMMIT": "a" * 40, "EXTERNAL_VERDICT": "true",
                 "GITHUB_OUTPUT": str(output), "SEMGREP_ARGS": str(work / "args.json"),
                 "SEMGREP_REPORT": str(report)}, capture_output=True, text=True)
        return result, json.loads((work / "args.json").read_text()), output

    result, scanner_args, output = run_community("clean", clean_report)
    assert "--timeout=45" in scanner_args, (
        "community Semgrep must receive the approved 45-second analysis budget; "
        f"scanner argv was {scanner_args}")
    assert result.returncode == 0, result.stderr
    assert output.read_text() == "finding_count=0\n", output.read_text()

    result, _, _ = run_community("scanner-errors", error_report)
    assert result.returncode != 0 and "community scan reported 1 scanner error(s)" in result.stderr, (
        "external verdict must reject scanner errors even when Semgrep exits 0: "
        f"exit={result.returncode}, stderr={result.stderr}")

print("PASS: trusted build workflow, Alpha-only ruleset, and fail-closed apply")
PY
