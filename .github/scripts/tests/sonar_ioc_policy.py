"""Structural IOC dependency policy shared by the checker and its tests."""
import json
import re


def check_ioc_path_policies(pr_ci):
    # The Docker-backed certificate acceptance suite must run only after the IOC
    # scan has succeeded. Resolve the workflow dependency graph (including scalar
    # and list `needs`) instead of pinning it to one job name. Status-function
    # conditions on the dependency path can override GitHub's default success
    # chaining. Only the two closed direct-IOC success forms below may override it.
    def needs_of(job):
        needs = job.get("needs", [])
        return [needs] if isinstance(needs, str) else needs

    def paths_to_ioc(job_name, path=()):
        if job_name == "ioc":
            return [path + ("ioc",)]
        if job_name in path or job_name not in pr_ci["jobs"]:
            return []
        job = pr_ci["jobs"][job_name]
        result = []
        for parent in needs_of(job):
            result.extend(paths_to_ioc(parent, path + (job_name,)))
        return result

    def explicit_ioc_success(job, condition):
        direct = needs_of(job)
        if (not isinstance(direct, list) or not all(isinstance(item, str) for item in direct)
                or len(direct) != len(set(direct))
                or set(direct) not in ({"ioc"}, {"ioc", "detect-changes"})):
            return False
        expression = condition.strip()
        if expression.startswith("${{") and expression.endswith("}}"):
            expression = expression[3:-2]
        # Anchored syntax recognition, not evaluation or substring matching.
        # Whitespace is accepted between tokens; quoted literals stay byte-exact.
        prefix = (r"\s*!\s*cancelled\s*\(\s*\)\s*&&\s*"
                  r"needs\s*\.\s*ioc\s*\.\s*result\s*==\s*'success'\s*")
        protected_advisory_prefix = (r"\s*!\s*inputs\s*\.\s*enforcement-only\s*&&\s*" + prefix)
        detector = (r"&&\s*\(\s*needs\s*\.\s*detect-changes\s*\.\s*result\s*!=\s*'success'\s*"
                    r"\|\|\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*selfhost\s*!=\s*'false'\s*\)\s*")
        selected_advisory = (r"&&\s*\(\s*needs\s*\.\s*detect-changes\s*\.\s*result\s*!=\s*'success'\s*"
                             r"\|\|\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*scan-known\s*!=\s*'true'\s*"
                             r"\|\|\s*(?:fromJSON\s*\(\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*scan-selection\s*\)\s*"
                             r"\.\s*hotfix\s*\|\|\s*)?"
                             r"fromJSON\s*\(\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*scan-selection\s*\)\s*"
                             r"\.\s*advisory\s*\.\s*[a-z_]+\s*"
                             r"(?:\|\|\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*spa\s*==\s*'true'\s*"
                             r"\|\|\s*needs\s*\.\s*detect-changes\s*\.\s*outputs\s*\.\s*services\s*==\s*'true'\s*)?"
                             r"\)\s*")
        if (re.fullmatch(prefix, expression) is not None
                or ("detect-changes" in direct and (
                    re.fullmatch(prefix + detector, expression) is not None
                    or re.fullmatch(prefix + selected_advisory, expression) is not None))):
            return True
        if re.fullmatch(protected_advisory_prefix, expression) is not None:
            check_optional_protected_override({'on': pr_ci.get('on', {})},
                                              "${{ inputs.enforcement-only == true }}", "IOC advisory job")
            return True
        if re.fullmatch(protected_advisory_prefix + selected_advisory, expression) is not None:
            check_optional_protected_override({'on': pr_ci.get('on', {})},
                                              "${{ inputs.enforcement-only == true }}", "IOC advisory job")
            return True
        return False

    provision_jobs = [name for name, job in pr_ci["jobs"].items()
                      if any("bash scripts/tests/test-provision-cert.sh" in step.get("run", "")
                             for step in job.get("steps", []))]
    assert provision_jobs, "CI must retain the Docker-backed certificate provisioner suite"
    for name in provision_jobs:
        ioc_paths = paths_to_ioc(name)
        assert ioc_paths, f"Docker-backed provisioner job {name} must depend on IOC"
        for path in ioc_paths:
            for node in path:
                path_job = pr_ci["jobs"][node]
                condition = path_job.get("if", "")
                assert (not re.search(r"\b(?:always|failure|cancelled|success)\s*\(", condition, re.IGNORECASE)
                        or explicit_ioc_success(path_job, condition)), (
                    f"status-function condition on IOC path can bypass the gate: {node}")
                assert path_job.get("continue-on-error", "false") == "false", (
                    f"continue-on-error on IOC path can hide a failed gate: {node}")


def check_optional_protected_override(pr_ci, value, owner):
    """Permit only the declared false-by-default enforcement input."""
    if value in (False, "false"):
        return
    assert value == "${{ inputs.enforcement-only == true }}", (
        f"ordinary PR caller must preserve legacy {owner} behavior")
    trigger = pr_ci.get("on", {})
    call = trigger.get("workflow_call", {}) if isinstance(trigger, dict) else {}
    definition = call.get("inputs", {}).get("enforcement-only")
    assert isinstance(definition, dict), "pr-ci must declare enforcement-only input"
    assert definition.get("type") == "boolean", "enforcement-only must be boolean"
    assert definition.get("default") in (False, "false"), "enforcement-only must default false"
    assert definition.get("required", "false") in (False, "false"), (
        "enforcement-only must remain optional for ordinary callers")


def check_scan_consumer_policies(pr_ci, workflows, main_ci, nightly_ci):
    """Validate real consumer workflow inputs, gates, artifacts, and defaults."""
    codeql, semgrep = workflows["codeql.yml"], workflows["semgrep.yml"]
    eslint, govuln = workflows["eslint-sarif.yml"], workflows["govulncheck.yml"]
    # Self-Hosted: sonarcloud.yml arrives with the Sonar wiring (F7), so it is
    # optional during bootstrap. main-ci.yml stays with Desktop (main_ci is None).
    build, sonar = workflows["build.yml"], workflows.get("sonarcloud.yml")

    def call_inputs(workflow, owner):
        trigger = workflow.get("on", {})
        call = trigger.get("workflow_call", {}) if isinstance(trigger, dict) else {}
        assert isinstance(call, dict), f"{owner} must declare workflow_call inputs"
        return call.get("inputs", {})

    def require_default(workflow, name, expected, owner):
        actual = call_inputs(workflow, owner).get(name)
        assert actual is not None, f"{owner} missing input {name}"
        assert actual.get("default") == expected, f"{owner}.{name} default must remain {expected}"

    full_matrix = [
        {"language": "actions", "runner": "blacksmith-2vcpu-ubuntu-2404", "build-mode": "none"},
        {"language": "go", "runner": "blacksmith-4vcpu-ubuntu-2404", "build-mode": "manual"},
        {"language": "javascript-typescript", "runner": "blacksmith-4vcpu-ubuntu-2404", "build-mode": "none"},
        {"language": "python", "runner": "blacksmith-2vcpu-ubuntu-2404", "build-mode": "none"},
    ]
    matrix_input = call_inputs(codeql, "CodeQL").get("codeql-matrix")
    assert matrix_input is not None, "CodeQL needs selectable matrix input"
    default_matrix = json.loads(matrix_input["default"])
    assert isinstance(default_matrix, dict) and default_matrix.get("include") == full_matrix, (
        "default CodeQL matrix must retain the approved include object with every approved leg")
    require_default(codeql, "external-verdict", "false", "CodeQL")
    assert codeql["jobs"]["analyze"]["strategy"]["matrix"] == "${{ fromJSON(inputs.codeql-matrix) }}"
    require_default(semgrep, "run-custom", "true", "Semgrep")
    require_default(semgrep, "run-community", "true", "Semgrep")
    require_default(semgrep, "run-canary", "true", "Semgrep")
    for workflow, owner in ((semgrep, "Semgrep"), (eslint, "ESLint"), (govuln, "govulncheck")):
        require_default(workflow, "external-verdict", "false", owner)
    require_default(build, "force-coverage", "false", "build")
    if sonar is not None:
        require_default(sonar, "require-quality-gate", "false", "Sonar")

    jobs = pr_ci["jobs"]
    selected_calls = {"semgrep": ("run-custom", None),
                      "eslint": (None, "eslint"), "govulncheck": (None, "govulncheck")}
    for job_name, (input_name, selection_key) in selected_calls.items():
        job = jobs[job_name]
        needs = job.get("needs", [])
        needs = [needs] if isinstance(needs, str) else needs
        assert {"ioc", "detect-changes"}.issubset(needs), f"{job_name} must depend on IOC and selection"
        condition = job.get("if", "")
        assert "needs.ioc.result == 'success'" in condition, f"{job_name} can run before successful IOC"
        assert ("needs.detect-changes.result != 'success'" in condition
                or ("scan-known" in condition and "!= 'true'" in condition)), (
            f"{job_name} must preserve full scan fallback on detector failure/unknown")
        with_args = job.get("with", {})
        if input_name:
            assert input_name in with_args, f"{job_name} caller must set {input_name}"
            assert "needs.detect-changes.outputs" in with_args[input_name], (
                f"{job_name}.{input_name} must use actual detector outputs")
        else:
            assert f"selected.{selection_key}" in condition, (
                f"{job_name} must use detector selection to schedule its reusable call")
        check_optional_protected_override(pr_ci, with_args.get("external-verdict", "false"),
                                          f"{job_name} finding verdict")

    assert all(key in str(jobs["semgrep"].get("with", {})) for key in
               ("run-custom", "run-community", "run-canary")), "Semgrep caller must pass all three selections"
    for job_name in ("build", "unit-smoke", "semgrep", "eslint", "govulncheck"):
        needs = jobs[job_name].get("needs", [])
        needs = [needs] if isinstance(needs, str) else needs
        assert "ioc" in needs, f"{job_name} may install dependencies or execute repository code before IOC"

    semgrep_steps = semgrep["jobs"]["semgrep"]["steps"]
    assert "inputs.run-custom" in semgrep["jobs"]["semgrep"].get("if", "")
    assert "inputs.run-community" in semgrep["jobs"]["semgrep"].get("if", "")
    for subscan in ("custom", "community", "canary"):
        scan = next((step for step in semgrep_steps if step.get("id") == subscan), None)
        assert scan is not None and f"inputs.run-{subscan}" in scan.get("if", ""), (
            f"Semgrep {subscan} scan must obey its selected input")
    for category, input_name in (("semgrep-custom", "run-custom"), ("semgrep-community", "run-community")):
        upload = next((step for step in semgrep_steps if step.get("with", {}).get("category") == category), None)
        assert upload is not None and f"inputs.{input_name}" in upload.get("if", ""), (
            f"{category} upload must follow the matching selected scan")
    summary = next((step for step in semgrep_steps if step.get("name") == "Publish Semgrep results to PR"), None)
    assert summary is not None
    summary_text = str(summary)
    assert "run-custom" in summary_text and "run-community" in summary_text and "skipped" in summary_text, (
        "Semgrep summary must mark omitted subscans as skipped instead of zero findings")

    # These workflows deliberately retain their all-scanner defaults. Verify
    # real Nightly/main callers omit new opt-in inputs instead of mirroring a scheduler.
    old_defaults = {"codeql-matrix", "run-custom", "run-community", "run-canary",
                    "external-verdict", "force-coverage", "require-quality-gate"}
    for label, caller in (("main-ci", main_ci), ("nightly-ci", nightly_ci)):
        if caller is None:
            continue
        for job in caller["jobs"].values():
            uses = job.get("uses", "")
            if any(uses.endswith(name) for name in ("codeql.yml", "eslint-sarif.yml", "govulncheck.yml",
                                                    "build.yml", "sonarcloud.yml", "semgrep.yml")):
                supplied = old_defaults & set(job.get("with", {}))
                assert not supplied, f"{label} changed preserved reusable defaults: {supplied}"

    # Keep raw component filters separate from coverage force: changed flags also
    # select Docker cache work and the Electron host probe.
    build_inputs = jobs["build"].get("with", {})
    for key in ("desktop-changed", "control-plane-changed", "media-plane-changed",
                "media-plane-tests-changed", "admin-changed", "production-docker-changed"):
        assert "needs.detect-changes.outputs" in build_inputs.get(key, ""), f"preserve raw build input {key}"
    assert "force-coverage" in build_inputs and "scan-selection" in build_inputs["force-coverage"]
    coverage_jobs = {"desktop", "control-plane", "media-plane", "admin"}
    for name, job in build["jobs"].items():
        forced = "inputs.force-coverage" in job.get("if", "")
        assert forced == (name in coverage_jobs), f"force-coverage should select coverage tests only: {name}"
    producer_jobs = {"desktop": "desktop-coverage-merge", "control-plane": "control-plane-coverage-merge",
                     "media-plane": "media-plane", "admin": "admin"}
    for suite, producer_name in producer_jobs.items():
        producer = build["jobs"][producer_name]
        assert "uploaded" in str(producer.get("outputs", {})), (
            f"{suite} coverage producer must report actual uploaded evidence")
        if producer_name.endswith("-coverage-merge"):
            assert f"needs.{suite}.result == 'success'" in producer.get("if", ""), (
                f"{suite} skipped/failed coverage producer must not publish uploaded=true")
        if sonar is not None:
            assert f"{suite}-coverage" in str(sonar["jobs"])
    assert "sonar" not in jobs, "PR-owned workflow must not invoke the credentialed scanner"
    if sonar is not None:
        sonar_scan = next(step for step in sonar["jobs"]["sonarcloud"]["steps"] if step.get("name") == "SonarCloud Scan")
        assert "require-quality-gate" in sonar_scan.get("with", {}).get("args", "") and "!inputs.reusable-analysis" in sonar_scan.get("with", {}).get("args", ""), (
            "protected Sonar must wait for the real quality gate")

    # Each evidence report must derive from an identified, real pinned action output.
    for workflow, job_name, label in ((codeql, "analyze", "CodeQL"), (semgrep, "semgrep", "Semgrep"),
                                      (eslint, "eslint", "ESLint"), (govuln, "govulncheck", "govulncheck")):
        steps = workflow["jobs"][job_name]["steps"]
        actions = [step for step in steps if "upload-sarif@" in step.get("uses", "")]
        if label == "CodeQL":
            actions += [step for step in steps if "codeql-action/analyze@" in step.get("uses", "")]
        assert actions and all(step.get("id") for step in actions), f"{label} upload/analyze actions need ids"
        evidence = [step for step in steps if "scan-evidence-" in str(step)]
        assert evidence, f"{label} must emit protected scan evidence"
        evidence_text = str(evidence)
        assert "sarif-id" in evidence_text and "runner.temp" in evidence_text
        assert "run_id" in evidence_text and "run_attempt" in evidence_text
        assert "if-no-files-found" in evidence_text and "error" in evidence_text
        assert all(step.get("continue-on-error", "false") != "true" for step in actions), (
            f"{label} SARIF upload failure must remain fatal")
