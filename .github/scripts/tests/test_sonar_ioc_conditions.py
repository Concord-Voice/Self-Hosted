"""Inert graph fixtures call the shared IOC policy through a normal module import."""
import ast
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import yaml

CHECKER = Path(__file__).with_name('sonar-required-gate.test.sh')
POLICY = Path(__file__).with_name('sonar_ioc_policy.py')
STATIC_CONDITION = "${{ !cancelled() && needs.ioc.result == 'success' }}"
ACCEPTANCE_CONDITION = "${{ !cancelled() && needs.ioc.result == 'success' && (needs.detect-changes.result != 'success' || needs.detect-changes.outputs.selfhost != 'false') }}"


def check_actual_policy(jobs):
    if not POLICY.is_file():
        raise AssertionError('missing shared IOC policy module: sonar_ioc_policy.py')
    spec = importlib.util.spec_from_file_location('tested_sonar_ioc_policy', POLICY)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.check_ioc_path_policies({'jobs': jobs})


def validate_consumer_wiring(text):
    """Inspect ordinary import wiring as syntax only, without running the heredoc."""
    marker = 'python3 - "$ROOT" <<\'PY\'\n'
    if text.count(marker) != 1:
        raise AssertionError('IOC consumer lacks its fixed Python heredoc')
    body = text.split(marker, 1)[1].rsplit('\nPY', 1)[0]
    tree = ast.parse(body)

    def name(node):
        if isinstance(node, ast.Name): return node.id
        if isinstance(node, ast.Attribute): return name(node.value) + '.' + node.attr
        return ''

    def fixed_path(node, expected):
        parts = []
        while isinstance(node, ast.BinOp) and isinstance(node.op, ast.Div):
            if not isinstance(node.right, ast.Constant) or not isinstance(node.right.value, str): return False
            parts.insert(0, node.right.value); node = node.left
        return isinstance(node, ast.Name) and node.id == 'root' and '/'.join(parts) == expected

    specs = {}
    modules = {}
    loaded = set()
    parsed = False
    connected = False
    for statement in tree.body:
        if isinstance(statement, ast.Assign) and len(statement.targets) == 1 and isinstance(statement.targets[0], ast.Name):
            target = statement.targets[0].id; value = statement.value
            if target == 'pr_ci':
                parsed = (isinstance(value, ast.Call) and name(value.func) == 'yaml.load'
                          and any(fixed_path(node, '.github/workflows/pr-ci.yml') for node in ast.walk(value)))
            elif isinstance(value, ast.Call) and name(value.func) == 'importlib.util.spec_from_file_location':
                if len(value.args) == 2 and fixed_path(value.args[1], '.github/scripts/tests/sonar_ioc_policy.py'):
                    specs[target] = True
            elif isinstance(value, ast.Call) and name(value.func) == 'importlib.util.module_from_spec':
                if len(value.args) == 1 and isinstance(value.args[0], ast.Name) and value.args[0].id in specs:
                    modules[target] = value.args[0].id
            else:
                specs.pop(target, None); modules.pop(target, None); loaded.discard(target)
        elif isinstance(statement, ast.Expr) and isinstance(statement.value, ast.Call):
            call = statement.value
            for module, spec in modules.items():
                if name(call.func) == spec + '.loader.exec_module' and len(call.args) == 1 and name(call.args[0]) == module:
                    loaded.add(module)
                if (name(call.func) == module + '.check_ioc_path_policies' and module in loaded
                        and len(call.args) == 1 and name(call.args[0]) == 'pr_ci' and parsed):
                    connected = True
    if not connected:
        raise AssertionError('IOC consumer must unconditionally call loaded fixed policy with parsed pr_ci')


def provision_job(*, needs='ioc', condition=None, continue_on_error=None):
    job = {'needs': needs,
           'steps': [{'run': 'bash scripts/tests/test-provision-cert.sh'}]}
    if condition is not None:
        job['if'] = condition
    if continue_on_error is not None:
        job['continue-on-error'] = continue_on_error
    return job


class IOCConditionPolicyTests(unittest.TestCase):
    def graph(self, job=None, **other_jobs):
        return {'ioc': {}, 'detect-changes': {},
                'provisioner': job if job is not None else provision_job(),
                **other_jobs}

    def test_sonar_build_sentinel_executes_against_checked_in_workflows(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        step = next(step for step in workflow["jobs"]["workflow-lint"]["steps"]
                    if step.get("name") == "SonarCloud path-skip build sentinel")
        run = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", step["run"]],
                             cwd=root, capture_output=True, text=True)
        self.assertEqual(run.returncode, 0, run.stderr)

    def test_adr_number_lint_and_self_test_share_the_advisory_selector(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        steps = workflow["jobs"]["static-guards"]["steps"]
        lint = next(step for step in steps if step.get("name") == "ADR-number lint (#1559)")
        self_test = next(step for step in steps if step.get("name") == "Test ADR-number lint (#1559 self-test)")
        self.assertIn("advisory.adr_numbers", lint.get("if", ""))
        self.assertEqual(lint.get("if"), self_test.get("if"))

    def test_protected_required_workflow_calls_the_protected_osv_reusable(self):
        root = CHECKER.parents[3]
        required = yaml.load((root / ".github/workflows/required-pr-build.yml").read_text(), Loader=yaml.BaseLoader)
        osv = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
        call = required["jobs"].get("osv")
        self.assertIsNotNone(call, "protected workflow must own the selected OSV execution")
        self.assertEqual("Concord-Voice/Concord-Voice-Alpha/.github/workflows/osv-scanner.yml@06bc23adc8c82a1eb55e5d6988b42f42f463fb83",
                         call.get("uses"))
        needs = call.get("needs", [])
        self.assertIn("pr-ci", needs if isinstance(needs, list) else [needs])
        self.assertIn("workflow_call", osv.get("on", {}),
                      "the reusable OSV source must execute from the protected workflow commit")

    def test_ordinary_pr_passes_booleans_to_reusable_workflows(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        jobs = workflow["jobs"]
        self.assertEqual("${{ inputs.trusted-source == true }}", jobs["ioc"]["with"]["trusted-source"])
        self.assertIn("inputs.trusted-source == true &&", jobs["build"]["with"]["force-coverage"])
        self.assertNotIn("sonar", jobs)
        for job in ("semgrep", "eslint", "govulncheck"):
            with self.subTest(job=job):
                self.assertEqual("${{ inputs.enforcement-only == true }}",
                                 jobs[job]["with"]["external-verdict"])

    def test_standalone_osv_keeps_native_pr_configuration_with_protected_rule(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
        job = workflow["jobs"]["select-osv"]
        self.assertNotIn("delegate", job.get("outputs", {}))
        steps = job.get("steps", [])
        self.assertFalse(any(step.get("id") == "delegate" for step in steps))
        self.assertIn("!inputs.protected-pr", job.get("if", ""))
        scan_pr = workflow["jobs"]["scan-pr"]
        self.assertIn("!inputs.protected-pr", scan_pr.get("if", ""))
        self.assertNotIn("delegate", scan_pr.get("if", ""))
        self.assertIn("vars.CI_SCAN_RELEVANCE_MODE == 'required-full'", scan_pr.get("if", ""))
        self.assertIn("vars.CI_SCAN_RELEVANCE_MODE == 'required-relevant'", scan_pr.get("if", ""))
        self.assertIn("needs.select-osv.result != 'success'", scan_pr.get("if", ""))
        self.assertIn("needs.select-osv.outputs.known != 'true'", scan_pr.get("if", ""))
        condition = scan_pr["if"].removeprefix("${{").removesuffix("}}").strip()
        def scan_pr_runs(event, mode, protected):
            values = {
                "!cancelled()": True,
                "!inputs.protected-pr": not protected,
                "github.event_name == 'pull_request'": event == "pull_request",
                "needs.select-osv.result != 'success'": False,
                "needs.select-osv.outputs.known != 'true'": False,
                "vars.CI_SCAN_RELEVANCE_MODE == 'required-full'": mode == "required-full",
                "vars.CI_SCAN_RELEVANCE_MODE == 'required-relevant'": mode == "required-relevant",
                "fromJSON(needs.select-osv.outputs.selection).selected.osv": False,
            }
            expression = condition
            for term, value in values.items():
                expression = expression.replace(term, str(value))
            expression = expression.replace("&&", "and").replace("||", "or")
            self.assertIsNotNone(re.fullmatch(r"(?:True|False|and|or|[()\s])*", expression),
                                 "unexpected OSV condition term")
            def evaluate(node):
                if isinstance(node, ast.Expression):
                    return evaluate(node.body)
                if isinstance(node, ast.Constant) and isinstance(node.value, bool):
                    return node.value
                if isinstance(node, ast.BoolOp):
                    values = [evaluate(value) for value in node.values]
                    if isinstance(node.op, ast.And):
                        return all(values)
                    if isinstance(node.op, ast.Or):
                        return any(values)
                raise AssertionError("unexpected OSV condition syntax")
            return evaluate(ast.parse(expression, mode="eval"))

        for mode in ("required-full", "required-relevant"):
            with self.subTest(mode=mode):
                self.assertTrue(scan_pr_runs("pull_request", mode, False))
                self.assertFalse(scan_pr_runs("workflow_call", mode, False))
                self.assertFalse(scan_pr_runs("pull_request", mode, True))
                self.assertFalse(scan_pr_runs("workflow_call", mode, True))
        policy_step = next(step for step in steps if step.get("id") == "policy")
        self.assertIn("fail-on-vuln=true", policy_step.get("run", ""),
                      "unknown standalone selection must preserve strict vulnerability failure")
        required = yaml.load((root / ".github/workflows/required-pr-build.yml").read_text(), Loader=yaml.BaseLoader)
        protected_call = required["jobs"]["osv"]
        self.assertEqual("${{ fromJSON(needs.pr-ci.outputs.report-selection).hotfix }}",
                         protected_call.get("with", {}).get("hotfix"))
        protected_steps = workflow["jobs"]["protected-pr-scan"]["steps"]
        reporter = next(step for step in protected_steps if "osv-reporter-action" in step.get("uses", ""))
        self.assertIn("--fail-on-vuln=true", reporter.get("with", {}).get("scan-args", ""),
                      "trusted protected scan must block new vulnerabilities on every PR")
        self.assertIn("github.workflow", workflow.get("concurrency", {}).get("group", ""),
                      "protected and standalone runs need separate concurrency groups during overlap")

    def test_standalone_osv_missing_base_policy_or_failed_filter_keeps_full_scan(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
        steps = workflow["jobs"]["select-osv"]["steps"]
        filter_step = next(step for step in steps if step.get("id") == "filter")
        selection_step = next(step for step in steps if step.get("id") == "selection")

        for case in ("missing-policy", "failed-filter"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                temp = Path(directory)
                if case == "failed-filter":
                    for file in ("policy/.github/ci/scan-relevance.yml",
                                 "policy/.github/scripts/ci-scan-relevance.mjs"):
                        path = temp / file
                        path.parent.mkdir(parents=True, exist_ok=True)
                        path.write_text("placeholder")
                output = temp / "github-output"
                env = {**os.environ, "GITHUB_OUTPUT": str(output), "RUNNER_TEMP": directory,
                       "METADATA_VALID": "true", "POLICY_SHA": "a" * 40,
                       "FILTER_OUTCOME": "skipped" if case == "missing-policy" else "failure",
                       "FILTERS": "{}", "EVENT_NAME": "pull_request"}
                result = subprocess.run(["bash", "-c", selection_step["run"]], cwd=temp,
                                        env=env, capture_output=True, text=True)
                self.assertEqual(0, result.returncode, result.stderr)
                self.assertIn('known=false', output.read_text())
                self.assertIn('selection={"version":1,"known":false}', output.read_text())

        self.assertIn("hashFiles('policy/.github/ci/scan-relevance.yml')", filter_step.get("if", ""))
        self.assertEqual("true", filter_step.get("continue-on-error"))
        self.assertIn("steps.filter.outcome", selection_step.get("env", {}).get("FILTER_OUTCOME", ""))

    def test_protected_osv_results_stay_outside_the_pr_checkout(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
        steps = workflow["jobs"]["protected-pr-scan"]["steps"]
        checkout = next(step for step in steps if step.get("uses", "").startswith("actions/checkout@")
                        and step.get("with", {}).get("ref") == "${{ inputs.merge-sha }}")
        old_scan = next(step for step in steps if step.get("name") == "Scan base dependencies")
        new_scan = next(step for step in steps if step.get("name") == "Scan test-merge dependencies")
        reporter = next(step for step in steps if "osv-reporter-action" in step.get("uses", ""))
        upload = next(step for step in steps if "upload-artifact" in step.get("uses", "")
                      and "protected-osv-sarif-" in step.get("with", {}).get("name", ""))
        self.assertEqual("scan", checkout.get("with", {}).get("path"),
                         "the PR tree must be isolated in its own subdirectory")
        for step in (old_scan, new_scan, reporter, upload):
            self.assertNotRegex(str(step), r"scan/(?:old-results|new-results|results\.sarif)",
                f"{step.get('name', step.get('uses'))} must write outside PR checkout")
        self.assertIn("$RUNNER_TEMP/osv-results", old_scan.get("run", ""))
        self.assertIn("$RUNNER_TEMP/osv-results", new_scan.get("run", ""))
        for scan in (old_scan, new_scan):
            run = scan.get("run", "")
            self.assertIn("scan_status=$?", run)
            self.assertIn("scan --config=/results/config.toml", run,
                          "protected scans must ignore PR-controlled OSV suppressions")
            self.assertIn('[[ "$scan_status" == 0 || "$scan_status" == 1 ]]', run,
                          "only clean scan and vulnerability findings are accepted")
            self.assertIn(":ro", run, "the checked-out PR tree must be mounted read-only")
            self.assertIn("ghcr.io/google/osv-scanner@sha256:", run)
        self.assertIn('> "$RUNNER_TEMP/osv-results/config.toml"', old_scan.get("run", ""))
        self.assertIn("old-results.json", str(reporter.get("with", {})))
        self.assertIn("new-results.json", str(reporter.get("with", {})))
        self.assertIn("results.sarif", str(upload.get("with", {}).get("path", "")))

    def test_protected_osv_upload_is_bound_to_selected_test_merge(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/osv-scanner.yml").read_text(), Loader=yaml.BaseLoader)
        steps = workflow["jobs"]["protected-pr-publish"]["steps"]
        upload = next(step for step in steps if step.get("name") == "Upload protected OSV SARIF")
        self.assertEqual("refs/pull/${{ github.event.pull_request.number }}/merge",
                         upload["with"].get("ref"))
        self.assertEqual("${{ inputs.merge-sha }}", upload["with"].get("sha"))

    def reject_status(self, condition, *, needs=None):
        job = provision_job(needs=['ioc','detect-changes'] if needs is None else needs,
                            condition=condition)
        with self.assertRaisesRegex(AssertionError,
                r'status-function condition on IOC path can bypass the gate: provisioner'):
            check_actual_policy(self.graph(job))

    def test_default_success_direct_scalar_ioc_dependency_passes(self):
        check_actual_policy(self.graph(provision_job(needs='ioc')))

    def test_default_success_direct_list_ioc_dependency_passes(self):
        check_actual_policy(self.graph(provision_job(needs=['ioc'])))

    def test_default_success_transitive_dependency_passes(self):
        check_actual_policy(self.graph(provision_job(needs='intermediate'),
                                       intermediate={'needs':['ioc']}))

    def test_explicit_ioc_success_runs_static_checks_despite_detector_failure(self):
        check_actual_policy(self.graph(provision_job(needs=['ioc','detect-changes'],
                                                    condition=STATIC_CONDITION)))

    def test_explicit_ioc_success_and_conservative_detector_gate_passes(self):
        check_actual_policy(self.graph(provision_job(needs=['ioc','detect-changes'],
                                                    condition=ACCEPTANCE_CONDITION)))

    def test_safe_form_is_structural_and_does_not_need_a_job_name_waiver(self):
        jobs = self.graph(provision_job(needs=['ioc'],condition=STATIC_CONDITION))
        jobs['another-provisioner'] = jobs.pop('provisioner')
        check_actual_policy(jobs)

    def test_missing_ioc_dependency_retains_named_reachability_refusal(self):
        with self.assertRaisesRegex(AssertionError,
                r'Docker-backed provisioner job provisioner must depend on IOC'):
            check_actual_policy(self.graph(provision_job(needs=['detect-changes'],
                                                        condition=STATIC_CONDITION)))

    def test_missing_explicit_ioc_result_guard_is_not_a_status_exception(self):
        self.reject_status('${{ !cancelled() }}')

    def test_ioc_failure_skipped_and_cancelled_cannot_authorize(self):
        for result in ('failure','skipped','cancelled'):
            with self.subTest(ioc_result=result):
                self.reject_status("${{ !cancelled() && needs.ioc.result == '"+result+"' }}")

    def test_ioc_inequality_guard_cannot_authorize(self):
        self.reject_status("${{ !cancelled() && needs.ioc.result != 'success' }}")

    def test_outer_disjunction_cannot_escape_ioc_success(self):
        self.reject_status("${{ (!cancelled() && needs.ioc.result == 'success') || true }}")

    def test_ioc_guard_on_one_or_branch_is_insufficient(self):
        self.reject_status("${{ !cancelled() && (needs.ioc.result == 'success' || needs.detect-changes.result == 'failure') }}")

    def test_always_failure_and_positive_cancelled_remain_blocked(self):
        for function in ('always()','failure()','cancelled()'):
            with self.subTest(status_function=function):
                self.reject_status("${{ "+function+" && needs.ioc.result == 'success' }}")

    def test_success_status_function_cannot_override_implicit_ioc_success(self):
        # `success() || true` admits the consumer after an IOC dependency fails.
        self.reject_status("${{ success() || true }}")

    def test_mixed_case_status_functions_cannot_bypass_direct_ioc_gate(self):
        for function in ('Always()', 'aLwAyS()', 'FAILURE()', 'Failure()',
                         'Cancelled()', 'cAnCeLlEd()'):
            for suffix in ('', " && needs.ioc.result == 'success'"):
                with self.subTest(status_function=function,explicit_result=bool(suffix)):
                    self.reject_status('${{ ' + function + suffix + ' }}')

    def test_mixed_case_status_functions_cannot_bypass_transitive_ioc_gate(self):
        for function in ('Always()', 'aLwAyS()', 'FAILURE()', 'Failure()',
                         'Cancelled()', 'cAnCeLlEd()'):
            with self.subTest(status_function=function):
                with self.assertRaisesRegex(AssertionError,
                        r'status-function condition on IOC path can bypass the gate: intermediate'):
                    check_actual_policy(self.graph(provision_job(needs='intermediate'),
                        intermediate={'needs':'ioc','if':'${{ ' + function + ' }}'}))

    def test_unknown_extra_conjunct_is_not_blanket_approved(self):
        self.reject_status("${{ !cancelled() && needs.ioc.result == 'success' && github.event_name == 'pull_request' }}")

    def test_continue_on_error_is_rejected_even_with_a_default_safe_path(self):
        with self.assertRaisesRegex(AssertionError,
                r'continue-on-error on IOC path can hide a failed gate: provisioner'):
            check_actual_policy(self.graph(provision_job(continue_on_error='true')))

    def test_continue_on_error_expression_is_not_treated_as_false(self):
        with self.assertRaisesRegex(AssertionError,
                r'continue-on-error on IOC path can hide a failed gate: provisioner'):
            check_actual_policy(self.graph(provision_job(continue_on_error='${{ true }}')))

    def test_recognized_safe_condition_still_checks_continue_on_error(self):
        with self.assertRaisesRegex(AssertionError,
                r'continue-on-error on IOC path can hide a failed gate: provisioner'):
            check_actual_policy(self.graph(provision_job(needs=['ioc','detect-changes'],
                condition=STATIC_CONDITION,continue_on_error='true')))

    def test_an_extra_scanner_prerequisite_cannot_be_bypassed(self):
        self.reject_status(STATIC_CONDITION,needs=['ioc','detect-changes','security-scanner'])

    def test_an_unsafe_intermediate_node_retains_its_own_named_refusal(self):
        with self.assertRaisesRegex(AssertionError,
                r'status-function condition on IOC path can bypass the gate: intermediate'):
            check_actual_policy(self.graph(provision_job(needs='intermediate'),
                intermediate={'needs':'ioc','if':'${{ always() }}'}))

    def test_intermediate_continue_on_error_is_still_rejected(self):
        with self.assertRaisesRegex(AssertionError,
                r'continue-on-error on IOC path can hide a failed gate: intermediate'):
            check_actual_policy(self.graph(provision_job(needs=['intermediate']),
                intermediate={'needs':'ioc','continue-on-error':'true'}))


class IOCConsumerWiringTests(unittest.TestCase):
    FIXTURE = '''python3 - "$ROOT" <<'PY'
import importlib.util
from pathlib import Path
import yaml
root = Path("owned inert fixture")
pr_ci = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
ioc_spec = importlib.util.spec_from_file_location("sonar_ioc_policy", root / ".github/scripts/tests/sonar_ioc_policy.py")
ioc_policy = importlib.util.module_from_spec(ioc_spec)
ioc_spec.loader.exec_module(ioc_policy)
ioc_policy.check_ioc_path_policies(pr_ci)
PY
'''

    def test_actual_shell_consumer_calls_fixed_module_on_actual_parsed_workflow(self):
        validate_consumer_wiring(CHECKER.read_text())

    def test_comment_dead_conditional_disconnected_and_wrong_module_wires_are_refused(self):
        validate_consumer_wiring(self.FIXTURE)
        call = 'ioc_policy.check_ioc_path_policies(pr_ci)'
        mutations = {
            'comment-only': self.FIXTURE.replace(call, '# ' + call),
            'dead-conditional': self.FIXTURE.replace(call, 'if False:\n    ' + call),
            'conditional': self.FIXTURE.replace(call, 'if pr_ci:\n    ' + call),
            'disconnected-graph': self.FIXTURE.replace(call, 'ioc_policy.check_ioc_path_policies({"jobs": {}})'),
            'wrong-module': self.FIXTURE.replace('tests/sonar_ioc_policy.py', 'tests/another_policy.py'),
            'not-loaded': self.FIXTURE.replace('ioc_spec.loader.exec_module(ioc_policy)', '# module not loaded'),
            'unparsed-graph': self.FIXTURE.replace('pr_ci = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)', 'pr_ci = {"jobs": {}}'),
            'replaced-module': self.FIXTURE.replace(call, 'ioc_policy = disconnected\n' + call),
            'replaced-graph': self.FIXTURE.replace(call, 'pr_ci = {"jobs": {}}\n' + call),
        }
        for label, text in mutations.items():
            with self.subTest(wiring=label):
                with self.assertRaisesRegex(AssertionError, 'unconditionally call loaded fixed policy with parsed pr_ci'):
                    validate_consumer_wiring(text)

    def test_actual_reusable_workflows_consume_selection_and_protect_verdicts(self):
        root = CHECKER.parents[3]
        names = ("pr-ci.yml", "main-ci.yml", "nightly-ci.yml", "codeql.yml", "semgrep.yml",
                 "eslint-sarif.yml", "govulncheck.yml", "build.yml", "sonarcloud.yml")
        workflows = {name: yaml.load((root / ".github/workflows" / name).read_text(),
                                     Loader=yaml.BaseLoader) for name in names}
        spec = importlib.util.spec_from_file_location("tested_scan_consumer_policy", POLICY)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        consumers = {name: workflows[name] for name in names
                     if name not in ("pr-ci.yml", "main-ci.yml", "nightly-ci.yml")}
        # The shared static policy predates the protected caller input. Validate
        # its ordinary-call behavior using the input's declared default, then
        # separately pin the actual caller expression below.
        ordinary_pr = copy.deepcopy(workflows["pr-ci.yml"])
        for name in ("semgrep", "eslint", "govulncheck"):
            ordinary_pr["jobs"][name]["with"]["external-verdict"] = "false"
        module.check_scan_consumer_policies(ordinary_pr, consumers,
                                             workflows["main-ci.yml"], workflows["nightly-ci.yml"])
        for name in ("semgrep", "eslint", "govulncheck"):
            self.assertEqual(workflows["pr-ci.yml"]["jobs"][name]["with"]["external-verdict"],
                             "${{ inputs.enforcement-only == true }}")

    def test_native_codeql_is_absent_from_the_protected_pr_caller(self):
        root = CHECKER.parents[3]
        pr = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        self.assertNotIn("codeql", pr["jobs"],
                         "native CodeQL runs in its ordinary PR caller, outside protected graph")

    def test_pr_scanners_are_scheduled_after_ioc_and_detector_selection(self):
        root = CHECKER.parents[3]
        pr = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        for name in ("semgrep", "eslint", "govulncheck"):
            job = pr["jobs"][name]
            needs = job.get("needs", [])
            needs = [needs] if isinstance(needs, str) else needs
            self.assertIn("ioc", needs, f"{name} must not install or execute before IOC")
            self.assertIn("detect-changes", needs, f"{name} must not allocate a runner before selection")
            condition = job.get("if", "")
            self.assertIn("needs.ioc.result == 'success'", condition,
                          f"{name} must keep the IOC prerequisite under a status override")

    def test_codeql_is_not_selected_or_matrix_gated_inside_pr_ci(self):
        root = CHECKER.parents[3]
        pr = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        self.assertNotIn("codeql", pr["jobs"])

    def test_semgrep_scan_parse_and_sarif_upload_follow_selected_subscans(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/semgrep.yml").read_text(), Loader=yaml.BaseLoader)
        steps = workflow["jobs"]["semgrep"]["steps"]
        for name in ("custom", "community", "canary"):
            step = next((step for step in steps if step.get("id") == name), None)
            self.assertIsNotNone(step, f"{name} must remain an independently selectable scan")
            self.assertIn(f"inputs.run-{name}", step.get("if", ""))
        for category, selection in (("semgrep-custom", "run-custom"),
                                    ("semgrep-community", "run-community")):
            upload = next((step for step in steps if step.get("with", {}).get("category") == category), None)
            self.assertIsNotNone(upload, f"{category} upload must remain present")
            self.assertIn(f"inputs.{selection}", upload.get("if", ""))

    def test_protected_scan_evidence_is_bound_to_real_action_ids_and_attempt(self):
        root = CHECKER.parents[3]
        files = ("codeql.yml", "semgrep.yml", "eslint-sarif.yml", "govulncheck.yml")
        for filename in files:
            with self.subTest(workflow=filename):
                workflow = yaml.load((root / ".github/workflows" / filename).read_text(), Loader=yaml.BaseLoader)
                job_name = {"codeql.yml": "analyze", "semgrep.yml": "semgrep",
                            "eslint-sarif.yml": "eslint", "govulncheck.yml": "govulncheck"}[filename]
                steps = workflow["jobs"][job_name]["steps"]
                actions = [step for step in steps if "upload-sarif@" in step.get("uses", "")]
                if filename == "codeql.yml":
                    actions += [step for step in steps if "codeql-action/analyze@" in step.get("uses", "")]
                self.assertTrue(actions and all(step.get("id") for step in actions),
                                "pinned upload/analyze action IDs must expose sarif-id")
                evidence = [step for step in steps if "scan-evidence-" in str(step)]
                evidence_text = str(evidence)
                self.assertTrue(evidence, "protected call must upload a small evidence artifact")
                for token in ("sarif-id", "runner.temp", "run_id", "run_attempt", "if-no-files-found", "error"):
                    self.assertIn(token, evidence_text)

    def test_sonar_quality_gate_is_required_for_protected_dispatch(self):
        root = CHECKER.parents[3]
        sonar = yaml.load((root / ".github/workflows/sonarcloud.yml").read_text(), Loader=yaml.BaseLoader)
        pr = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        self.assertNotIn("sonar", pr["jobs"])
        sonar_inputs = sonar["on"]["workflow_call"].get("inputs", {})
        self.assertIn("require-quality-gate", sonar_inputs)
        self.assertEqual(sonar_inputs["require-quality-gate"]["default"], "false")
        scan = next(step for step in sonar["jobs"]["sonarcloud"]["steps"] if step.get("name") == "SonarCloud Scan")
        self.assertIn("require-quality-gate", scan.get("with", {}).get("args", ""))
        self.assertIn("!inputs.reusable-analysis", scan.get("with", {}).get("args", ""))

    def test_pr_build_keeps_raw_component_filters_separate_from_coverage_force(self):
        root = CHECKER.parents[3]
        pr = yaml.load((root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        build = pr["jobs"]["build"]
        with_args = build.get("with", {})
        for name in ("desktop-changed", "control-plane-changed", "media-plane-changed",
                     "media-plane-tests-changed", "admin-changed", "production-docker-changed"):
            self.assertIn("needs.detect-changes.outputs", with_args.get(name, ""),
                          f"{name} must continue using the raw component matcher")
        self.assertIn("force-coverage", with_args,
                      "full/Hotfix/global-Sonar coverage needs a dedicated test-only input")

    def test_actual_semgrep_custom_step_distinguishes_findings_engine_and_parse_failure(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/semgrep.yml").read_text(), Loader=yaml.BaseLoader)
        step = next(step for step in workflow["jobs"]["semgrep"]["steps"] if step.get("id") == "custom")
        with tempfile.TemporaryDirectory(prefix="semgrep-step-contract-") as tmp:
            work = Path(tmp)
            bindir = work / "bin"
            bindir.mkdir()
            fake = bindir / "semgrep"
            fake.write_text('''#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    --json-output=*) if [ "${MOCK_WRITE_JSON:-true}" = true ]; then printf '%s' "$MOCK_JSON" > "${arg#*=}"; fi ;;
    --sarif-output=*) printf '{"version":"2.1.0","runs":[{"results":[]}]}' > "${arg#*=}" ;;
  esac
done
exit "$MOCK_SCAN_EXIT"
''')
            fake.chmod(0o755)

            def run(scan_exit, report):
                output = work / "github-output"
                output.write_text("")
                env = {**os.environ, "PATH": f"{bindir}:{os.environ['PATH']}",
                       "GITHUB_OUTPUT": str(output), "MOCK_SCAN_EXIT": str(scan_exit),
                       "MOCK_JSON": report, "BASELINE_COMMIT": "a" * 40,
                       "SEMGREP_VERSION": "1.121.0", "MOCK_WRITE_JSON": "true"}
                result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", step["run"]],
                                        cwd=work, env=env, text=True, capture_output=True)
                return result, output.read_text()

            finding_report = '{"results":[{"extra":{"severity":"ERROR","message":"fixture"}}]}'
            findings, outputs = run(1, finding_report)
            self.assertEqual(findings.returncode, 0, findings.stderr)
            self.assertIn("error_count=1", outputs)
            engine, _ = run(2, finding_report)
            self.assertEqual(engine.returncode, 2, engine.stderr)
            malformed, _ = run(0, "not-json")
            self.assertNotEqual(malformed.returncode, 0, "parse failure cannot be reported as a clean scan")

    def test_semgrep_community_engine_or_missing_report_fails_only_protected_mode(self):
        root = CHECKER.parents[3]
        workflow = yaml.load((root / ".github/workflows/semgrep.yml").read_text(), Loader=yaml.BaseLoader)
        step = next(step for step in workflow["jobs"]["semgrep"]["steps"] if step.get("id") == "community")
        with tempfile.TemporaryDirectory(prefix="semgrep-community-contract-") as tmp:
            work = Path(tmp)
            bindir = work / "bin"
            bindir.mkdir()
            fake = bindir / "semgrep"
            fake.write_text('''#!/bin/sh
exit "$MOCK_SCAN_EXIT"
''')
            fake.chmod(0o755)

            def run(protected):
                output = work / "github-output"
                output.write_text("")
                env = {**os.environ, "PATH": f"{bindir}:{os.environ['PATH']}",
                       "GITHUB_OUTPUT": str(output), "MOCK_SCAN_EXIT": "2",
                       "BASELINE_COMMIT": "a" * 40, "EXTERNAL_VERDICT": protected}
                result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", step["run"]],
                                        cwd=work, env=env, text=True, capture_output=True)
                return result, output.read_text()

            legacy, legacy_output = run("false")
            self.assertEqual(legacy.returncode, 0, legacy.stderr)
            self.assertIn("finding_count=unavailable", legacy_output)
            protected, _ = run("true")
            self.assertNotEqual(protected.returncode, 0,
                                "protected verdict cannot accept community engine failure or absent report")


class ProtectedRequiredWorkflowWiringTests(unittest.TestCase):
    """Exercise the actual Actions definitions that own the protected verdict."""

    @classmethod
    def setUpClass(cls):
        cls.root = CHECKER.parents[3]
        cls.pr = yaml.load((cls.root / ".github/workflows/pr-ci.yml").read_text(), Loader=yaml.BaseLoader)
        cls.wrapper = yaml.load((cls.root / ".github/workflows/required-pr-build.yml").read_text(), Loader=yaml.BaseLoader)

    def test_actual_blocking_findings_step_reads_count_as_shell_data(self):
        workflow = yaml.load((self.root / ".github/workflows/semgrep.yml").read_text(), Loader=yaml.BaseLoader)
        step = next(step for step in workflow["jobs"]["semgrep"]["steps"]
                    if step.get("name") == "Fail if blocking findings")
        expected_binding = "${{ steps.custom.outputs.error_count }}"
        environment = step.get("env", {})
        with tempfile.TemporaryDirectory(prefix="semgrep-blocking-findings-") as tmp:
            work = Path(tmp)
            sentinel = work / "command-substitution-ran"

            def run(error_count):
                return subprocess.run(
                    ["bash", "-e", "-o", "pipefail", "-c", step["run"]], cwd=work,
                    env={**os.environ, "ERROR_COUNT": error_count}, text=True, capture_output=True)

            result = run("1")
            self.assertEqual(result.returncode, 1, "blocking findings must fail the actual workflow step")
            self.assertIn("::error::Semgrep found 1 ERROR-level findings.", result.stdout,
                          "actual blocking diagnostic must consume the delivered count")
            self.assertEqual(environment.get("ERROR_COUNT"), expected_binding,
                             "blocking error count must cross the Actions boundary through the step environment")

            hostile = f"1$(touch {sentinel})"
            result = run(hostile)
            self.assertEqual(result.returncode, 1, "hostile count remains a blocking finding")
            self.assertIn(f"Semgrep found {hostile} ERROR-level findings.", result.stdout,
                          "expanded count must remain literal data in the diagnostic")
            self.assertFalse(sentinel.exists(), "count output must never execute shell command substitution")

    def test_callable_pr_ci_defaults_to_advisory_without_sonar_secret(self):
        call = self.pr["on"]["workflow_call"]
        self.assertEqual(call["inputs"]["trusted-source"]["default"], "false")
        self.assertEqual(call["inputs"]["enforcement-only"]["default"], "false")
        self.assertNotIn("secrets", call)
        self.assertNotIn("SONAR_TOKEN", str(self.pr))

    def test_required_wrapper_delegates_nested_ownership_and_has_no_duplicate_scanners(self):
        jobs = self.wrapper["jobs"]
        call = jobs["pr-ci"]
        self.assertEqual(call["uses"],
                         "Concord-Voice/Concord-Voice-Alpha/.github/workflows/pr-ci.yml@cb10844a7860092011f96b4607682ef2e6c55a80")
        self.assertEqual(call["with"], {"trusted-source": "true", "enforcement-only": "true"})
        self.assertEqual(call["secrets"], {"SONAR_TOKEN": "${{ secrets.SONAR_TOKEN }}"})
        self.assertNotIn("ioc", jobs)
        self.assertNotIn("build", jobs)
        self.assertEqual(jobs["result"]["if"], "${{ always() }}")
        self.assertIn("pr-ci", jobs["result"]["needs"])

    def test_required_verdict_uses_actual_report_and_protected_source_provenance(self):
        jobs = self.wrapper["jobs"]
        self.assertIn("report", self.pr["jobs"])
        report = self.pr["jobs"]["report"]
        self.assertEqual(report["outputs"]["selection"], "${{ steps.report.outputs.selection }}")
        self.assertEqual(jobs["result"]["needs"], ["pr-ci", "osv", "publish-install-script-warnings"])
        result_text = str(jobs["result"])
        for token in ("needs.pr-ci.outputs.report-selection", "needs.pr-ci.outputs.report-jobs",
                      "needs.pr-ci.outputs.coverage-uploads"):
            self.assertIn(token, result_text)
        serialized = str(self.pr["jobs"])
        for token in ("fromJSON(toJSON(job)).workflow_sha", "POLICY_SHA"):
            self.assertIn(token, serialized)
        wrapper_result = str(jobs["result"])
        for token in ("verify-pr-scan-results.py", "GITHUB_RUN_ATTEMPT", "SCAN_EVIDENCE_DIR"):
            self.assertIn(token, wrapper_result)
        self.assertIn("needs.osv.result", str(jobs["result"]))

    def test_protected_pr_ci_keeps_ioc_first_and_limits_enforcement_mode(self):
        jobs = self.pr["jobs"]
        serialized = str(jobs)
        for advisory in ("static-guards", "workflow-lint", "golangci-lint", "unit-smoke"):
            self.assertIn(advisory, jobs, f"missing existing advisory job {advisory}; jobs={list(jobs)}")
        static = jobs["static-guards"]
        self.assertIn("needs.detect-changes.outputs.spa == 'true'", static["if"])
        self.assertIn("needs.detect-changes.outputs.services == 'true'", static["if"])
        for name, family in (("Run deploy-spa.sh --dry-run --target=staging", "spa"),
                             ("Run deploy.sh --dry-run --target=staging", "services")):
            step = next(step for step in static["steps"] if step.get("name") == name)
            self.assertIn(f"needs.detect-changes.outputs.{family} == 'true'", step["if"])
        for token in ("inputs.enforcement-only", "inputs.trusted-source", "force-coverage",
                      "needs.ioc.result == 'success'", "coverage_required"):
            self.assertIn(token, serialized)
        self.assertNotIn("coverage_required.desktop }}", str(jobs["build"].get("with", {})))
        self.assertIn("pr-ci-ordinary-", str(self.pr.get("concurrency", {})))
        self.assertIn("pr-ci-protected-", str(self.pr.get("concurrency", {})))
        self.assertIn("!inputs.enforcement-only", str(self.pr["concurrency"].get("cancel-in-progress", "")))

    def test_detector_runs_only_protected_policy_source_before_ioc(self):
        detect = self.pr["jobs"]["detect-changes"]
        self.assertNotIn("needs", detect)
        policy = next(step for step in detect["steps"] if step.get("id") == "policy")
        self.assertEqual(policy["with"]["ref"],
                         "${{ inputs.trusted-source && fromJSON(toJSON(job)).workflow_sha || steps.metadata.outputs.base_sha }}")
        filters = next(step for step in detect["steps"] if step.get("id") == "filter")
        filter_source = filters["with"]["filters"]
        self.assertEqual(filter_source,
                         "${{ inputs.trusted-source && 'policy/.github/ci/scan-relevance.yml' || '.github/ci/scan-relevance.yml' }}",
                         "protected calls must read the immutable map; ordinary calls need the native map")
        selection = next(step for step in detect["steps"] if step.get("id") == "selection")
        self.assertIn('"$GITHUB_WORKSPACE/policy/.github/scripts/ci-scan-relevance.mjs"', selection["run"])
        self.assertNotIn("pull_request.head.sha }}", str(policy))
        self.assertNotIn("pull_request.head.sha }}", selection["run"])

    def test_actual_selection_defaults_protected_legacy_to_full_but_ordinary_legacy_keeps_coverage_unforced(self):
        node = '''
          import { pathToFileURL } from 'node:url';
          import { resolve } from 'node:path';
          const { composeSelection, ANALYSIS_KEYS } = await import(
            pathToFileURL(resolve('.github/scripts/ci-scan-relevance.mjs')));
          const policy = (await import('node:fs')).readFileSync('.github/ci/scan-relevance.yml', 'utf8');
          const keys = [...policy.matchAll(/^([a-z][a-z0-9_]*):$/gm)].map(match => match[1]);
          const filters = Object.fromEntries(keys.map(k => [k, 'false']));
          const metadata = {event_name:'pull_request', head_sha:'a'.repeat(40), base_sha:'b'.repeat(40),
            merge_sha:'c'.repeat(40), changed_files:0, hotfix:false};
          const policySha = 'd'.repeat(40);
          const full = composeSelection(filters, metadata, {mode:'required-full', policySha});
          const legacy = composeSelection(filters, metadata, {mode:'legacy', policySha});
          const hotfix = composeSelection(filters, {...metadata, hotfix:true}, {mode:'required-relevant', policySha});
          const unknown = composeSelection(filters, metadata, {mode:'invalid', policySha});
          console.log(JSON.stringify({full, legacy, hotfix, unknown}));
        '''
        run = subprocess.run(["node", "--input-type=module", "-e", node], cwd=self.root,
                             text=True, capture_output=True, check=True)
        selections = json.loads(run.stdout)
        self.assertTrue(selections["full"]["known"])
        scanner_keys = ("codeql_actions", "codeql_go", "codeql_js", "eslint", "govulncheck",
                        "osv", "semgrep_custom", "semgrep_community", "semgrep_canary", "sonar")
        protected_codeql = ("codeql_actions", "codeql_go", "codeql_js")
        self.assertTrue(all(selections["full"]["selected"][key]
                            for key in scanner_keys if key not in protected_codeql))
        self.assertTrue(all(not selections["full"]["selected"][key] for key in protected_codeql))
        self.assertEqual(selections["full"]["coverage_required"],
                         {key: True for key in ("desktop", "control_plane", "media_plane", "admin")})
        self.assertTrue(selections["legacy"]["known"])
        self.assertTrue(all(selections["legacy"]["selected"][key]
                            for key in scanner_keys if key not in protected_codeql))
        self.assertTrue(all(not selections["legacy"]["selected"][key] for key in protected_codeql))
        self.assertTrue(all(not selections["hotfix"]["selected"][key] for key in protected_codeql))
        self.assertTrue(selections["hotfix"]["selected"]["osv"],
                        "Hotfix keeps standalone OSV selected even when component filters are empty")
        self.assertEqual(selections["legacy"]["coverage_required"],
                         {key: False for key in ("desktop", "control_plane", "media_plane", "admin")})
        self.assertFalse(selections["unknown"]["known"])

    def test_detector_normalizes_only_protected_legacy_and_unknown_is_fail_closed(self):
        detect = self.pr["jobs"]["detect-changes"]
        selection = next(step for step in detect["steps"] if step.get("id") == "selection")
        mode = selection.get("env", {}).get("SCAN_MODE", "")
        self.assertIn("inputs.trusted-source", mode)
        self.assertIn("required-full", mode)
        self.assertIn("CI_SCAN_RELEVANCE_MODE", mode)
        self.assertIn("vars.CI_SCAN_RELEVANCE_MODE", mode)
        self.assertNotIn("vars.CI_SCAN_RELEVANCE_MODE == 'required-relevant'", mode)
        self.assertIn("scan-known", str(detect.get("outputs", {})))

    def test_report_carries_actual_needs_results_and_four_coverage_upload_flags(self):
        report = self.pr["jobs"]["report"]
        self.assertIn("needs", report)
        outputs = report.get("outputs", {})
        for key in ("selection", "jobs", "coverage-uploads"):
            self.assertIn(key, outputs)
        emit = next(step for step in report["steps"] if step.get("id") == "report")
        for job in ("ioc", "build", "semgrep", "eslint", "govulncheck"):
            self.assertEqual(emit["env"][f"{job.upper()}_RESULT"], f"${{{{ needs.{job}.result }}}}")
        for component in ("desktop", "control_plane", "media_plane", "admin"):
            self.assertIn(f"{component.upper()}_COVERAGE", emit["env"])
        self.assertNotIn("OSV_RESULT", emit["env"])

    def test_final_verifier_reads_actual_report_and_checks_out_immutable_workflow_source(self):
        result = self.wrapper["jobs"]["result"]
        self.assertEqual(result.get("if"), "${{ always() }}")
        self.assertIn("contents", result.get("permissions", {}))
        text = str(result)
        self.assertIn("verify-pr-scan-results.py", text)
        self.assertIn("workflow_sha", text)
        self.assertIn("WORKFLOW_SHA", text)
        self.assertIn("SCAN_EVIDENCE_DIR", text)
        self.assertIn("GITHUB_RUN_ATTEMPT", text)
        self.assertIn("git -C protected fetch", text)
        self.assertIn("refs/scan/pr-head", text)
        checkout = next(step for step in result["steps"] if "actions/checkout@" in step.get("uses", ""))
        self.assertEqual(checkout["with"]["ref"], "${{ fromJSON(toJSON(job)).workflow_sha }}")
        self.assertNotIn("ref: ${{ github.event.pull_request.head.sha }}", text)
        download = next(step for step in result["steps"] if step.get("name") == "Download current-attempt scan evidence")
        self.assertIn("actions/download-artifact@", download["uses"])
        self.assertIn("always()", download["if"])
        self.assertIn("github.run_id", download["with"]["pattern"])
        self.assertIn("github.run_attempt", download["with"]["pattern"])
        self.assertIn("runner.temp", download["with"]["path"])

    def test_advisory_jobs_are_conditional_on_not_enforcement_only(self):
        for name in ("static-guards", "workflow-lint", "golangci-lint", "native-audiocap", "unit-smoke"):
            with self.subTest(job=name):
                job = self.pr["jobs"][name]
                self.assertIn("inputs.enforcement-only", job.get("if", ""),
                              f"{name} must be omitted only on the protected enforcement path")

    def test_secret_scanning_is_ordinary_pr_only_and_keeps_full_diff_including_markdown(self):
        job = self.pr["jobs"]["secret-scanning"]
        self.assertEqual(job.get("if"), "${{ !inputs.enforcement-only }}",
                         "secret-scanning must be omitted from the protected enforcement-only DAG")
        self.assertEqual(job["needs"], ["ioc"])
        self.assertEqual(job["uses"], "./.github/workflows/secret-scanning.yml")
        self.assertEqual(job["permissions"], {"contents": "read"})
        secret = yaml.load((self.root / ".github/workflows/secret-scanning.yml").read_text(),
                           Loader=yaml.BaseLoader)
        self.assertIn("workflow_call", secret["on"])
        scan = next(step for step in secret["jobs"]["trufflehog-ci"]["steps"]
                    if step.get("name") == "TruffleHog scan PR diff")
        self.assertEqual(scan["with"]["base"], "${{ github.event.pull_request.base.sha }}")
        self.assertEqual(scan["with"]["head"], "${{ github.event.pull_request.head.sha }}")
        self.assertEqual(scan["with"]["extra_args"], "--only-verified")
        self.assertNotIn("paths", secret["on"].get("pull_request", {}))
        self.assertIn("trufflesecurity/trufflehog@", scan["uses"])

    def test_optional_protected_verdict_override_rejects_default_and_shape_drift(self):
        spec = importlib.util.spec_from_file_location("tested_sonar_ioc_policy_override", POLICY)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        valid = {"on": {"workflow_call": {"inputs": {
            "enforcement-only": {"type": "boolean", "default": "false", "required": "false"}}}}}
        module.check_optional_protected_override(valid, "false", "test")
        module.check_optional_protected_override(valid, "${{ inputs.enforcement-only == true }}", "test")
        invalid = (
            (True, valid, "legacy"),
            ("${{ inputs.enforcement-only }}", valid, "legacy"),
            ("${{ inputs.anything }}", valid, "legacy"),
            ("${{ inputs.enforcement-only == true }}", {"on": {"workflow_call": {"inputs": {}}}}, "declare"),
            ("${{ inputs.enforcement-only == true }}", {"on": {"workflow_call": {"inputs": {
                "enforcement-only": {"type": "string", "default": "false"}}}}}, "boolean"),
            ("${{ inputs.enforcement-only == true }}", {"on": {"workflow_call": {"inputs": {
                "enforcement-only": {"type": "boolean", "default": "true"}}}}}, "default false"),
            ("${{ inputs.enforcement-only == true }}", {"on": {"workflow_call": {"inputs": {
                "enforcement-only": {"type": "boolean", "default": "false", "required": "true"}}}}}, "optional"),
        )
        for value, workflow, message in invalid:
            with self.subTest(value=value, message=message):
                with self.assertRaisesRegex(AssertionError, message):
                    module.check_optional_protected_override(workflow, value, "test")

    def test_ioc_advisory_status_override_is_closed_and_keeps_ioc_success(self):
        spec = importlib.util.spec_from_file_location("tested_sonar_ioc_policy_ioc", POLICY)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        actual = copy.deepcopy(self.pr)
        module.check_ioc_path_policies(actual)
        mutations = (
            ("${{ !inputs.enforcement-only && !cancelled() }}", "bypass"),
            ("${{ inputs.enforcement-only || !cancelled() && needs.ioc.result == 'success' }}", "bypass"),
        )
        for expression, label in mutations:
            with self.subTest(mutation=label, expression=expression):
                mutant = copy.deepcopy(actual)
                mutant["jobs"]["static-guards"]["if"] = expression
                with self.assertRaisesRegex(AssertionError, "status-function condition on IOC path"):
                    module.check_ioc_path_policies(mutant)


if __name__ == '__main__':
    unittest.main()
