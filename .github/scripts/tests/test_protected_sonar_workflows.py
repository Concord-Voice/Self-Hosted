"""The protected Sonar bridge is the required, single producer of a PR's Sonar scan."""

import json
import importlib.util
from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[3]


def workflow(name):
    return yaml.load((ROOT / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)


class ProtectedSonarWorkflowTests(unittest.TestCase):
    def test_bridge_is_required_and_feeds_the_verdict(self):
        wrapper = workflow("required-pr-build.yml")["jobs"]
        self.assertRegex(wrapper["pr-ci"]["uses"],
                         r"^Concord-Voice/Self-Hosted/\.github/workflows/pr-ci\.yml@[0-9a-f]{40}$")
        self.assertNotIn("secrets", wrapper["pr-ci"])
        bridge = wrapper["protected-sonar"]
        self.assertNotIn("continue-on-error", bridge)
        self.assertNotIn("PROTECTED_SONAR_CANARY", json.dumps(bridge))
        for clause in ("needs.pr-ci.result == 'success'",
                       "github.event.pull_request.head.repo.full_name == github.repository",
                       "'\"sonar\":true'"):
            self.assertIn(clause, bridge["if"])
        self.assertEqual(bridge["outputs"],
                         {"child-run-id": "${{ steps.dispatch.outputs.child-run-id }}"})
        result = wrapper["result"]
        self.assertIn("protected-sonar", result["needs"])
        verify = next(s for s in result["steps"] if s.get("name") == "Run protected scan verdict")
        self.assertEqual(verify["env"]["SONAR_RESULT"], "${{ needs.protected-sonar.result }}")
        self.assertEqual(verify["env"]["SONAR_CHILD_RUN_ID"],
                         "${{ needs.protected-sonar.outputs.child-run-id }}")
        self.assertIn("sonar:$sonar,sonar_child_run_id:$sonar_child", verify["run"])
        final = result["steps"][-1]
        self.assertEqual(final["env"]["SONAR_RESULT"], "${{ needs.protected-sonar.result }}")
        self.assertIn('"$SONAR_RESULT" != failure && "$SONAR_RESULT" != cancelled', final["run"])

    def test_only_the_bridge_can_mint_a_caller_proof(self):
        # The proof verifier pins workflow_ref, which every job in the caller
        # run shares, so no other wrapper job may hold id-token: write.
        source = (ROOT / ".github/workflows/required-pr-build.yml").read_text()
        self.assertEqual(source.count("id-token:"), 1)
        holders = [name for name, job in workflow("required-pr-build.yml")["jobs"].items()
                   if "id-token" in job.get("permissions", {})]
        self.assertEqual(holders, ["protected-sonar"])
        self.assertNotIn("id-token", workflow("required-pr-build.yml").get("permissions", {}))

    def test_caller_proof_is_uploaded_before_exact_dispatch(self):
        job = workflow("required-pr-build.yml")["jobs"]["protected-sonar"]
        self.assertEqual(job["needs"], ["pr-ci"])
        self.assertEqual(job["permissions"],
                         {"contents": "read", "actions": "write", "id-token": "write"})
        checkout, proof, upload, dispatch, summary = job["steps"]
        self.assertEqual(checkout["with"]["ref"], "${{ fromJSON(toJSON(job)).workflow_sha }}")
        self.assertNotIn("pull_request.head.sha", str(checkout))
        for value in ("RUN_ID", "RUN_ATTEMPT", "PR_NUMBER", "HEAD_SHA", "BASE_SHA",
                      "PR_CI_RESULT", "SELECTION_JSON", "JOBS_JSON", "COVERAGE_JSON"):
            self.assertIn(value, proof["env"])
        self.assertIn("emit-proof", proof["run"])
        self.assertIn("proof.json", upload["with"]["path"])
        self.assertIn("${{ github.run_id }}-${{ github.run_attempt }}", upload["with"]["name"])
        self.assertIn("dispatch-watch", dispatch["run"])
        self.assertIn("child-run-id=", dispatch["run"])
        self.assertEqual(summary["if"], "${{ always() }}")
        self.assertIn("steps.dispatch.outcome", str(summary))

    def test_dispatch_uses_one_scanner_and_only_from_main(self):
        sonar = workflow("sonarcloud.yml")
        self.assertNotIn("PROTECTED_SONAR_CANARY", json.dumps(sonar))
        dispatch = sonar["on"]["workflow_dispatch"]["inputs"]
        self.assertEqual(set(dispatch), {"caller_run_id", "caller_run_attempt", "pr_number",
                                         "expected_head_sha", "expected_base_sha"})
        self.assertTrue(all(item == {"description": item["description"], "type": "string",
                                     "required": "true"} for item in dispatch.values()))
        self.assertEqual(sonar["on"]["workflow_call"]["inputs"]["reusable-analysis"],
                         {"description": "True for the Main/Nightly reusable scanner call",
                          "type": "boolean", "required": "true"})
        jobs = sonar["jobs"]
        self.assertIn("github.ref != 'refs/heads/main'", jobs["reject-disabled-dispatch"]["if"])
        self.assertIn("!inputs.reusable-analysis", jobs["reject-disabled-dispatch"]["if"])
        self.assertIn("exit 1", str(jobs["reject-disabled-dispatch"]["steps"]))
        scanner = jobs["sonarcloud"]
        self.assertIn("inputs.reusable-analysis", scanner["if"])
        self.assertIn("github.ref == 'refs/heads/main'", scanner["if"])
        self.assertEqual(scanner["environment"], "protected-sonar")
        self.assertEqual(scanner["permissions"], {"contents": "read", "actions": "read",
                                                 "pull-requests": "read", "checks": "read"})
        steps = scanner["steps"]
        self.assertEqual(sum(step.get("name") == "SonarCloud Scan" for step in steps), 1)
        prepare = next(step for step in steps if step.get("name") == "Verify caller proof and prepare inert PR source")
        scan = next(step for step in steps if step.get("name") == "SonarCloud Scan")
        check = next(step for step in steps if step.get("name") == "Require fresh exact-head SonarCloud App check")
        self.assertLess(steps.index(prepare), steps.index(scan))
        self.assertLess(steps.index(scan), steps.index(check))
        self.assertNotIn("SONAR_TOKEN", json.dumps(prepare))
        self.assertIn("!inputs.reusable-analysis", prepare["if"])
        self.assertIn("sonar.projectBaseDir", scan["with"]["args"])
        self.assertIn("!inputs.reusable-analysis", scan["with"]["args"])
        self.assertIn("sonar.qualitygate.wait=true", scan["with"]["args"])
        for name in ("desktop", "control-plane", "media-plane", "Admin Portal"):
            download = next(step for step in steps if step.get("name") == f"Download {name} coverage")
            self.assertIn("inputs.reusable-analysis && inputs.", download["if"])
        self.assertNotIn("concord-bridge", json.dumps(sonar))
        self.assertNotIn("github.event_name == 'workflow_call'", str(sonar))

    def test_only_the_credential_check_and_scanner_receive_the_token(self):
        steps = workflow("sonarcloud.yml")["jobs"]["sonarcloud"]["steps"]
        named = {step.get("name"): step for step in steps}
        prepare = named["Verify caller proof and prepare inert PR source"]
        credential, scan = named["Require Sonar credential"], named["SonarCloud Scan"]
        self.assertLess(steps.index(prepare), steps.index(credential))
        self.assertLess(steps.index(credential), steps.index(scan))
        token = {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"}
        self.assertEqual(credential["env"], token)
        self.assertEqual(scan["env"], token)
        self.assertEqual(json.dumps(workflow("sonarcloud.yml")).count("secrets.SONAR_TOKEN"), 2)
        self.assertNotIn("SONAR_TOKEN", json.dumps(workflow("pr-ci.yml")))

    def test_nightly_preserves_named_token_and_read_ceiling(self):
        # Self-Hosted has no main-ci; the nightly is the main-branch analysis.
        for name in ("nightly-ci.yml",):
            with self.subTest(name=name):
                call = workflow(name)["jobs"]["sonar"]
                self.assertEqual(call["with"]["reusable-analysis"], "true")
                self.assertEqual(call["permissions"], {"contents": "read", "actions": "read",
                                                       "pull-requests": "read", "checks": "read"})
                self.assertEqual(call["secrets"], {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"})

    def test_caller_budget_exceeds_child_scan_and_app_check(self):
        caller = workflow("required-pr-build.yml")["jobs"]["protected-sonar"]
        child = workflow("sonarcloud.yml")["jobs"]["sonarcloud"]
        source = ROOT / "scripts/sonar/protected-pr-caller.py"
        spec = importlib.util.spec_from_file_location("protected_pr_caller", source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        self.assertGreater(module.MAX_POLLS * 10 / 60, int(child["timeout-minutes"]))
        self.assertGreater(int(caller["timeout-minutes"]), module.MAX_POLLS * 10 / 60 + 10)


if __name__ == "__main__":
    unittest.main()
