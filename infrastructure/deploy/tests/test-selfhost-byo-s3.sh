#!/usr/bin/env bash
# Real, config-only Compose models and deployment packaging for fresh BYO storage.
# Run --require-compose in the completion gate; --compose-bin selects a supported
# standalone CLI (for example the 2.24.5 floor). No daemon/container/provider calls.
# Literal Docker Config.Env equality is separate Task-1 boundary evidence. Compose
# JSON is reusable serialization: never undouble dollars here to claim byte parity.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
STRICT=0
COMPOSE_BIN=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --require-compose) STRICT=1; shift ;;
    --compose-bin)
      [[ $# -ge 2 ]] || { echo 'FAIL: --compose-bin requires a CLI path'; exit 2; }
      COMPOSE_BIN="$2"; shift 2 ;;
    *) echo 'FAIL: unknown test option'; exit 2 ;;
  esac
done
# This is the only feature-absence RED. It cannot be satisfied by a broken render
# fixture, parser exception, or accidentally contacting Docker.
if [[ ! -f "$ROOT/docker-compose.byo-s3.yml" ]]; then
  echo 'FAIL: feature unavailable: canonical docker-compose.byo-s3.yml is missing'
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  if [[ "$STRICT" == 1 ]]; then
    echo 'FAIL: prerequisite unavailable: Python 3 is required'; exit 2
  fi
  echo 'SKIP: Python 3 unavailable; BYO model and packaging checks unexecuted'; exit 0
fi
umask 077
WORK="$(mktemp -d)" || { echo 'FAIL: private fixture setup unavailable'; exit 2; }
WORK="$(cd "$WORK" && pwd -P)"
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/home" "$WORK/docker-config"
# env -i also clears Git hook context, plugin/image controls and default dotenv
# selection. Only private synthetic inputs are passed to real config renders.
env -i PATH="$PATH" HOME="$WORK/home" DOCKER_CONFIG="$WORK/docker-config" LC_ALL=C \
  python3 - "$ROOT" "$WORK" "$STRICT" "$COMPOSE_BIN" <<'PY'
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys

ROOT, WORK = map(Path, sys.argv[1:3])
STRICT, COMPOSE_BIN = sys.argv[3] == "1", sys.argv[4]
PASS = FAIL = 0


def check(name, condition):
    global PASS, FAIL
    if condition:
        PASS += 1
        print("PASS: " + name)
    else:
        FAIL += 1
        print("FAIL: " + name)
    return bool(condition)


def prerequisite(name):
    print(("FAIL: prerequisite unavailable: " if STRICT else "SKIP: ") + name)
    print("INFO: BYO model and packaging checks unexecuted")
    sys.exit(2 if STRICT else 0)


try:
    import yaml
except ImportError:
    prerequisite("PyYAML is required for packaging checks")

# Reviewed managed/base bytes include the shared-listener opt-out, authenticated
# Redis healthcheck and managed runtime, TURN relay bind and public-address mapping,
# NATS port closure, the dedicated feedback screenshot R2 destination,
# control-plane GID 10002 secret-file group_add, Redis noeviction for
# persistent deny state, and the coturn credential-source entrypoint with
# softened TURN_SECRET guards. Keep explicit digests so source
# changes require baseline review. Snapshot all inputs to detect test-side mutations.
SOURCE_HASHES = {
    "docker-compose.yml": "".join(('bdad506e', '1c166bec', '88960d3e', '1fcc62c1', '6be3f8ec', 'a9e58f62', '19d455a6', '252fa641')),
    "docker-compose.production.yml": "".join(('9fb8be64', '804f352d', '22b376de', '93bbf3d7', 'bf3642b8', 'b9fc36ee', '16b7ea3b', '36ed56a1')),
}
SOURCE_FILES = tuple(SOURCE_HASHES) + ("docker-compose.selfhost.yml",)
if not check("all shared Compose source inputs are available",
             all((ROOT / name).is_file() for name in SOURCE_FILES)):
    sys.exit(1)
try:
    SOURCE_BYTES = {name: (ROOT / name).read_bytes() for name in SOURCE_FILES}
except OSError:
    check("shared Compose source inputs can be snapshotted", False)
    sys.exit(1)
STORAGE = {
    "STORAGE_BACKEND": "s3",
    "STORAGE_ENDPOINT": "objects.example.invalid:9443",
    "STORAGE_REGION": "",
    "STORAGE_ACCESS_KEY": ' -n synthetic-access = "quoted" $literal \\ end ',
    "STORAGE_SECRET_KEY": ' synthetic-secret = \'quoted\' "double" $literal \\ end ',
    "STORAGE_USE_SSL": "true",
    "STORAGE_BUCKET": "synthetic-private-media",
}
REQUIRED = tuple(key for key in STORAGE if key != "STORAGE_REGION")
METADATA = tuple(key for key in STORAGE if key not in
                 ("STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY"))
SYNTHETIC = {
    "DOMAIN": "byo.example.invalid",
    "PUBLIC_IP": "203.0.113.12",
    "FEEDBACK_REPO": "synthetic-owner/synthetic-feedback",
    "TRUSTED_PROXY_CIDRS": "10.0.0.0/8",
    "POSTGRES_PASSWORD": "-".join(('synthetic', 'postgres', 'password')),
    "REDIS_PASSWORD": "-".join(('synthetic', 'redis', 'password')),
    "JWT_SECRET": "-".join(('synthetic', 'jwt', 'secret')),
    "TURN_SECRET": "-".join(('synthetic', 'turn', 'secret')),
    "MINIO_ROOT_USER": "synthetic-bundled-user",
    "MINIO_ROOT_PASSWORD": "-".join(('synthetic', 'bundled', 'password')),
    "MINIO_BUCKET": "synthetic-bundled-bucket",
    "FEEDBACK_PAT": "synthetic-feedback-token",
    "MFA_ENCRYPTION_KEY": "synthetic-mfa-key",
    "SMTP_HOST": "smtp.byo.example.invalid",
    "SMTP_FROM": "byo@example.invalid",
    "INSTANCE_TYPE": "self-hosted",
    "ACTIVITY_HISTORY_CLUSTER_ENABLED": "false",
    "CONTROL_PLANE_REPLICA_COUNT": "1",
    "ACTIVITY_HISTORY_OPERATOR_NAME": "",
    "ACTIVITY_HISTORY_PRIVACY_POLICY_URL": "",
}


def dotenv(values, path):
    # Independent implementation of the agreed writer grammar for test data.
    # Only synthetic literals enter this encoder; it never reads a real env file.
    lines = []
    for key, value in values.items():
        encoded = value.replace("\\", "\\\\").replace('"', '\\"').replace("$", "$$")
        lines.append(key + '="' + encoded + '"\n')
    path.write_text("".join(lines), encoding="utf-8")


def capture(name, argv, env, stdin=None, cwd=None):
    result = subprocess.run(argv, input=stdin, text=True, env=env,
                            cwd=cwd or WORK, capture_output=True)
    (WORK / (name + ".stdout")).write_text(result.stdout, encoding="utf-8")
    (WORK / (name + ".stderr")).write_text(result.stderr, encoding="utf-8")
    return result


def model(result, name):
    # A prerequisite/render failure is never an expected negative assertion.
    if not check(name + " renders successfully", result.returncode == 0):
        raise RuntimeError()
    try:
        value = json.loads(result.stdout)
    except (ValueError, TypeError):
        check(name + " has valid JSON", False)
        raise RuntimeError() from None
    if not check(name + " has a service model", isinstance(value, dict) and
                 isinstance(value.get("services"), dict)):
        raise RuntimeError()
    return value


def packaging():
    manifest = (ROOT / "infrastructure/deploy/deploy-manifest.txt").read_text().splitlines()
    for name in ("docker-compose.byo-s3.yml", "infrastructure/deploy/selfhost-storage.sh"):
        check("deployment ships complete " + name, manifest.count(name) == 1)
    owners = (ROOT / "CODEOWNERS").read_text().splitlines()
    check("canonical BYO overlay has security ownership", any(re.fullmatch(
        r"docker-compose\.byo-s3\.yml\s+@Concord-Voice/security\s*", line) for line in owners))
    check("helper retains infrastructure security ownership", any(re.fullmatch(
        r"infrastructure/\*\*\s+@Concord-Voice/security\s*", line) for line in owners))
    config = yaml.safe_load((ROOT / ".pre-commit-config.yaml").read_text())
    hooks = [hook for repo in config["repos"] for hook in repo["hooks"]]
    generic = next(h for h in hooks if h["id"] == "check-yaml" and not h.get("alias"))
    syntax = next(h for h in hooks if h.get("alias") == "check-yaml-compose-merge-tags")
    tagged = ("docker-compose.selfhost.yml", "docker-compose.byo-s3.yml")
    for name in tagged:
        check("tagged file leaves safe YAML loader: " + name,
              re.search(generic.get("exclude", r"(?!)"), name) is not None)
        check("tagged file receives syntax YAML check: " + name,
              re.search(syntax.get("files", r"(?!)"), name) is not None)
    ordinary = ("docker-compose.untrusted.yml", "nested/docker-compose.byo-s3.yml",
                "docker-compose.byo-s3.yaml", ".github/workflows/synthetic.yml")
    for name in ordinary:
        check("YAML carve-out stays narrow: " + name,
              re.search(generic.get("exclude", r"(?!)"), name) is None and
              re.search(syntax.get("files", r"(?!)"), name) is None)
    check("tagged YAML remains syntax checked", syntax.get("args") == ["--unsafe"])
    check("ordinary YAML retains safe loading", "--unsafe" not in generic.get("args", []))

    selfhost = next(h for h in hooks if h["id"] == "selfhost-deploy-tests")
    suites = ("scripts/tests/test-install-selfhost.sh", "scripts/tests/test-provision-cert.sh",
              "scripts/tests/test-concord-selfhost.sh",
              "infrastructure/deploy/tests/test-selfhost-storage.sh",
              "infrastructure/deploy/tests/test-selfhost-byo-s3.sh",
              "infrastructure/deploy/tests/test-selfhost-cert-delivery.sh",
              "infrastructure/deploy/tests/test-selfhost-tls-readiness.sh",
              "infrastructure/deploy/tests/test-selfhost-tls-callers.sh",
              "infrastructure/deploy/tests/test-copy-certs.sh",
              "infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.sh")
    inputs = ("docker-compose.byo-s3.yml", "infrastructure/deploy/selfhost-storage.sh",
              "infrastructure/deploy/tests/test-selfhost-storage.sh",
              "infrastructure/deploy/tests/test-selfhost-byo-s3.sh")
    for hook in (selfhost,):
        for name in inputs:
            check(hook["id"] + " triggers for " + name,
                  re.search(hook.get("files", r"(?!)"), name) is not None)
        check(hook["id"] + " does not trigger for unrelated application source",
              re.search(hook.get("files", r"(?!)"), "client/desktop/src/main/index.ts") is None)
    # Every new TLS implementation/test surface and the actual coturn config
    # must continue to select the complete self-host suite.
    tls_inputs = (
        "infrastructure/deploy/provision-cert.sh",
        "infrastructure/deploy/copy-certs.sh",
        "infrastructure/deploy/selfhost-tls.sh",
        "infrastructure/deploy/selfhost-tls.py",
        "infrastructure/deploy/tests/test-selfhost-cert-delivery.sh",
        "infrastructure/deploy/tests/test-selfhost-tls-readiness.sh",
        "infrastructure/deploy/tests/test-selfhost-tls-callers.sh",
        "infrastructure/deploy/tests/test-copy-certs.sh",
        "infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.sh",
        "infrastructure/deploy/tests/test-selfhost-tls-selected-consumer.py",
        "infrastructure/deploy/tests/test-copy-certs-labels.py",
        "infrastructure/deploy/tests/selfhost-cert-test-tools.sh",
        "infrastructure/deploy/tests/test-copy-certs.sh",
        "infrastructure/deploy/tests/test_selfhost_tls.py",
        "infrastructure/docker/coturn/turnserver.conf",
        "docker-compose.yml",
        "docker-compose.production.yml",
        "infrastructure/deploy/deploy-manifest.txt",
    )
    for name in tls_inputs:
        check("self-host hook covers TLS input " + name,
              re.search(selfhost.get("files", r"(?!)"), name) is not None)
    for name in ("scripts/tests/run-selfhost-deploy-tests.sh",
                 "infrastructure/deploy/tests/Dockerfile.selfhost-hook",
                 "infrastructure/deploy/provision-cert.sh"):
        check("self-host hook covers runner input " + name,
              re.search(selfhost.get("files", r"(?!)"), name) is not None)
    # Execute the actual self-host hook command against private logging suites.
    # This catches a listed-but-never-invoked suite rather than grepping an entry.
    hook_root = WORK / "hook-fixture"
    hook_root.mkdir()
    for name in suites:
        path = hook_root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("#!/usr/bin/env bash\nprintf '%s\\n' " + shlex.quote(name) +
                        ' >> "$TEST_ENTRY_LOG"\n', encoding="utf-8")
    runner = hook_root / "scripts/tests/run-selfhost-deploy-tests.sh"
    shutil.copyfile(ROOT / runner.relative_to(hook_root), runner)
    runner_source = runner.read_text()
    check("macOS bridge is limited to the two TLS observer suites",
          re.search(r'network=none\s+bridge_env=\(\)\s+if \[\[ \$suite == infrastructure/deploy/tests/test-copy-certs\.sh \|\|\s+\$suite == infrastructure/deploy/tests/test-selfhost-tls-selected-consumer\.sh \]\]; then\s+network=bridge', runner_source) is not None and
          '--network "$network"' in runner_source)
    bin_dir = hook_root / "bin"
    bin_dir.mkdir()
    (bin_dir / "uname").write_text("#!/bin/sh\nprintf 'Linux\\n'\n", encoding="utf-8")
    (bin_dir / "uname").chmod(0o700)
    entry = shlex.split(selfhost["entry"])
    if check("self-host hook has a supported shell entry",
             entry == ["bash", "scripts/tests/run-selfhost-deploy-tests.sh"]):
        log = WORK / "hook-invocations"
        result = capture("hook-entry", entry,
                         dict(BASE_ENV, TEST_ENTRY_LOG=str(log),
                              PATH=str(bin_dir) + ":" + BASE_ENV["PATH"]), cwd=hook_root)
        called = log.read_text().splitlines() if log.exists() else []
        check("self-host hook entry succeeds", result.returncode == 0)
        for name in suites:
            check("self-host hook invokes " + name, called.count(name) == 1)
    workflow = yaml.safe_load((ROOT / ".github/workflows/pr-ci.yml").read_text())
    runs = [step.get("run", "") for job in workflow["jobs"].values() for step in job.get("steps", [])]
    check("CI discovers deployment test scripts", any(
        re.search(r"for\s+\w+\s+in\s+infrastructure/deploy/tests/test-\*\.sh", run) for run in runs))
    for name in ("test-concord-selfhost.sh", "test-install-selfhost.sh",
                 "test-provision-cert.sh"):
        check("CI retains explicit " + name, any("bash scripts/tests/" + name in run for run in runs))


def run():
    global BASE_ENV
    BASE_ENV = {key: os.environ[key] for key in ("PATH", "HOME", "DOCKER_CONFIG", "LC_ALL")}
    BASE_ENV["COMPOSE_DISABLE_ENV_FILE"] = "1"
    if COMPOSE_BIN:
        binary = Path(COMPOSE_BIN).resolve()
        if not binary.is_file() or not os.access(binary, os.X_OK):
            prerequisite("selected Compose CLI is unavailable")
        cli = [str(binary)]
    elif shutil.which("docker"):
        cli = [shutil.which("docker"), "compose"]
    elif shutil.which("docker-compose"):
        cli = [shutil.which("docker-compose")]
    else:
        prerequisite("Docker Compose >= 2.24.5 is required (no daemon needed)")
    version = capture("compose-version", cli + ["version", "--short"], BASE_ENV)
    match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?\s*", version.stdout)
    if version.returncode != 0 or match is None or tuple(map(int, match.groups())) < (2, 24, 5):
        prerequisite("supported Compose >= 2.24.5 is required")
    print("INFO: config-only Compose " + ".".join(match.groups()))
    for name, digest in SOURCE_HASHES.items():
        check("managed/base accepted source bytes preserved: " + name,
              hashlib.sha256((ROOT / name).read_bytes()).hexdigest() == digest)
    helper = ROOT / "infrastructure/deploy/selfhost-storage.sh"
    if not check("shared storage helper is available", helper.is_file()):
        return
    fixture = WORK / "model-fixture"
    fixture.mkdir()
    files = SOURCE_FILES + ("docker-compose.byo-s3.yml",)
    for name in files:
        shutil.copyfile(ROOT / name, fixture / name)
    (fixture / "spa.env").write_text("")
    all_values = dict(SYNTHETIC, **STORAGE, SELFHOST_STORAGE_MODE="byo-s3")
    dotenv(all_values, fixture / ".env")
    # Every direct render has explicit env-file/project-directory/files/profiles.
    # PATH Docker calls from the real helper must pass the same config-only shim.
    shim_dir = WORK / "bin"
    shim_dir.mkdir()
    (WORK / "compose-argv.json").write_text(json.dumps(cli))
    shim = shim_dir / "docker"
    shim.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
argv = sys.argv[1:]
if not argv or argv.pop(0) != "compose":
    sys.exit(97)
options = {"--env-file", "--project-directory", "--project-name", "--profile", "-f"}
i = 0
while i < len(argv) and argv[i] in options:
    i += 2
if i >= len(argv) or argv[i:] not in (["config", "--format", "json"], ["config", "--services"]):
    sys.exit(97)
if "P2_UNRELATED_CONTROL" in os.environ:
    with (pathlib.Path(__file__).parent.parent / "unrelated-control.jsonl").open("a") as output:
        output.write(json.dumps(os.environ["P2_UNRELATED_CONTROL"] == "synthetic-unrelated") + "\\n")
cli = json.loads((pathlib.Path(__file__).parent.parent / "compose-argv.json").read_text())
os.execv(cli[0], cli + argv)
''', encoding="utf-8")
    shim.chmod(0o700)
    BASE_ENV["PATH"] = str(shim_dir) + os.pathsep + BASE_ENV["PATH"]

    def render(name, count=4, profile="services", mode="json", values=None, extra=()):
        env_file = fixture / (name + ".env")
        dotenv(all_values if values is None else values, env_file)
        argv = cli + ["--env-file", str(env_file), "--project-directory", str(fixture),
                      "--profile", profile]
        for index, path in enumerate(files[:count]):
            argv += ["-f", str(fixture / path)]
            # Probe the canonical consumer before any overlay gets a chance to
            # replace it, matching the existing MinIO guard's probe boundary.
            if index == 0:
                for probe_path in extra:
                    argv += ["-f", str(probe_path)]
        argv += ["config"] + (["--format", "json"] if mode == "json" else ["--services"])
        return capture(name, argv, BASE_ENV)

    bundled = model(render("bundled", count=3), "bundled positive control")
    managed = model(render("managed", count=2), "managed positive control")
    base = model(render("base", count=1), "base positive control")
    byo = model(render("byo"), "BYO")
    bundled_all = model(render("bundled-all", count=3, profile="*"), "bundled all-profile")
    byo_all = model(render("byo-all", profile="*"), "BYO all-profile")
    for name, count, expected in (("base-repeat", 1, base), ("managed-repeat", 2, managed),
                                  ("bundled-repeat", 3, bundled)):
        check(name + " model is unchanged with fourth file unselected",
              model(render(name, count=count), name) == expected)
    selected = []
    for name, count in (("bundled-services", 3), ("byo-services", 4)):
        result = render(name, count=count, mode="services")
        if not check(name + " selection succeeds", result.returncode == 0):
            return
        selected.append(set(result.stdout.splitlines()))
    check("bundled active services include MinIO", "minio" in selected[0])
    check("BYO active services equal bundled minus MinIO", selected[1] == selected[0] - {"minio"})
    check("BYO active model excludes MinIO", "minio" not in byo["services"])
    minio = byo_all["services"].get("minio", {})
    check("all-profile model retains dormant MinIO", bool(minio))
    check("dormant MinIO has only its inactive profile",
          minio.get("profiles") == ["selfhost-bundled-minio-inactive"])
    expected_minio = copy.deepcopy(bundled_all["services"]["minio"])
    actual_minio = copy.deepcopy(minio)
    expected_minio.pop("profiles", None)
    actual_minio.pop("profiles", None)
    check("dormant MinIO preserves the complete canonical service", actual_minio == expected_minio)
    for key in ("image", "pull_policy", "build"):
        check("dormant MinIO retains canonical " + key,
              (key in minio) == (key in expected_minio) and minio.get(key) == expected_minio.get(key))
    probe = fixture / "minio-probe.yml"
    probe.write_text('services:\n  minio:\n    image: "registry.example.invalid/synthetic-probe:contract"\n')
    bundled_probe = model(render("bundled-all-probe", count=3, profile="*", extra=(probe,)),
                          "bundled all-profile probe")
    probe_model = model(render("byo-all-probe", profile="*", extra=(probe,)), "BYO all-profile probe")
    probe_minio = probe_model["services"].get("minio", {})
    check("all-profile MinIO probe remains visible",
          probe_minio.get("image") == "registry.example.invalid/synthetic-probe:contract")
    for key in ("pull_policy", "build"):
        check("all-profile probe preserves MinIO " + key,
              (key in probe_minio) == (key in minio) and probe_minio.get(key) == minio.get(key))
    expected_probe = copy.deepcopy(bundled_probe["services"]["minio"])
    actual_probe = copy.deepcopy(probe_minio)
    expected_probe.pop("profiles", None)
    actual_probe.pop("profiles", None)
    check("all-profile probe retains the complete shared MinIO consumer", actual_probe == expected_probe)
    bundled_deps = bundled["services"]["control-plane"]["depends_on"]
    deps = byo["services"]["control-plane"]["depends_on"]
    check("BYO dependencies equal shared dependencies minus MinIO",
          deps == {key: value for key, value in bundled_deps.items() if key != "minio"})
    check("BYO has exactly PostgreSQL Redis NATS healthy dependencies",
          set(deps) == {"postgres", "redis", "nats"} and
          all(value.get("condition") == "service_healthy" and value.get("required") is True
              for value in deps.values()))
    environment = byo["services"]["control-plane"]["environment"]
    check("BYO has exactly seven storage client keys",
          {key for key in environment if key.startswith("STORAGE_")} == set(STORAGE))
    for key in METADATA:
        check("BYO metadata matches synthetic input: " + key, environment.get(key) == STORAGE[key])
    # Opaque values are present in reusable JSON; no equality with original dollar
    # literals is claimed. A separate ASCII probe pins the actual field mappings.
    for key in ("STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY"):
        check("opaque credential is mapped in the model: " + key,
              isinstance(environment.get(key), str) and bool(environment[key]))
    probe_values = dict(all_values, STORAGE_ACCESS_KEY="synthetic-distinct-access-probe",
                        STORAGE_SECRET_KEY="-".join(('synthetic', 'distinct', 'secret', 'probe')))
    credential_probe = model(render("credential-mapping-probe", values=probe_values), "credential mapping probe")
    for key in ("STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY"):
        check("credential interpolation mapping obeyed: " + key,
              credential_probe["services"]["control-plane"]["environment"].get(key) == probe_values[key])
    for backend in ("minio", "r2", "b2"):
        backend_model = model(render("backend-" + backend, values=dict(all_values, STORAGE_BACKEND=backend)),
                              "external backend " + backend)
        check("external backend remains BYO: " + backend,
              backend_model["services"]["control-plane"]["environment"].get("STORAGE_BACKEND") == backend and
              "minio" not in backend_model["services"])
    for key in REQUIRED:
        for case in ("missing", "empty"):
            values = dict(all_values)
            if case == "missing":
                del values[key]
            else:
                values[key] = ""
            result = render("required-" + key + "-" + case, values=values)
            check(case + " required field refuses with its BYO diagnostic: " + key,
                  result.returncode != 0 and key + " must be set for BYO storage" in result.stderr)
    no_region = dict(all_values)
    del no_region["STORAGE_REGION"]
    optional = model(render("optional-region", values=no_region), "missing optional region")
    check("missing optional region remains an empty assignment",
          optional["services"]["control-plane"]["environment"].get("STORAGE_REGION") == "")
    expected = copy.deepcopy(bundled_all)
    actual = copy.deepcopy(byo_all)
    for value in (expected, actual):
        value["services"]["minio"].pop("profiles", None)
        cp = value["services"]["control-plane"]
        cp.pop("depends_on", None)
        for key in STORAGE:
            cp["environment"].pop(key, None)
    check("all nonstorage service and network behavior is preserved", actual == expected)
    for name, service in byo["services"].items():
        check("publication parity: " + name,
              service.get("ports", []) == bundled["services"][name].get("ports", []))
    media = byo["services"]["media-plane"]
    for key in ("RTC_MIN_PORT", "RTC_MAX_PORT", "MEDIASOUP_ENABLE_TCP"):
        check("RTC setting parity: " + key,
              media["environment"].get(key) == bundled["services"]["media-plane"]["environment"].get(key))
    posture = capture("byo-posture", [sys.executable, str(ROOT / "infrastructure/deploy/selfhost-port-posture.py"),
                                    "--rendered"], BASE_ENV, stdin=json.dumps(byo))
    check("BYO passes the real rendered port-posture consumer", posture.returncode == 0)

    runner = WORK / "resolve-and-render.sh"
    runner.write_text('''#!/usr/bin/env bash
set -euo pipefail
source "$1"
selfhost_storage_resolve "$2"
selfhost_storage_compose config --format json
''')
    baseline = capture("resolver-clean", ["bash", str(runner), str(helper), str(fixture)], BASE_ENV)
    resolved = model(baseline, "real resolver positive control")
    check("resolver chooses the real BYO model", resolved == byo)
    poison = dict(BASE_ENV, SELFHOST_STORAGE_MODE="bundled", COMPOSE_FILE="/synthetic-absent.yml",
                  COMPOSE_PROFILES="*", COMPOSE_PROJECT_NAME="synthetic-ambient-project",
                  COMPOSE_ENV_FILES="/synthetic-absent.env", COMPOSE_DISABLE_ENV_FILE="0",
                  INSTANCE_TYPE="saas", CONTROL_PLANE_IMAGE="synthetic-ambient-image:unused",
                  SELFHOST_STORAGE_TRUSTED_IMAGE="synthetic-forged-checkpoint:unused",
                  ATTACHMENT_WRITE_BACKEND="r2", CONTROL_PLANE_REPLICA_COUNT="9",
                  ACTIVITY_HISTORY_CLUSTER_ENABLED="true", MINIO_ROOT_USER="synthetic-ambient-root",
                  MINIO_ROOT_PASSWORD="-".join(('synthetic', 'ambient', 'root', 'secret')), MINIO_BUCKET="synthetic-ambient-bucket",
                  MINIO_ENDPOINT="synthetic-ambient.invalid", MINIO_ACCESS_KEY="synthetic-ambient-access",
                  MINIO_SECRET_KEY="-".join(('synthetic', 'ambient', 'secret')), MINIO_USE_SSL="false")
    poison.update({key: "synthetic-ambient-storage" for key in STORAGE})
    drift = capture("resolver-ambient-drift", ["bash", str(runner), str(helper), str(fixture)], poison)
    check("ambient controls cannot redirect resolved storage profile project or image",
          model(drift, "real resolver with ambient controls") == resolved)
    check("resolver preserves canonical project identity", resolved.get("name") == fixture.name)
    # Root .env owns active/retired MFA rotation as well as the active key.
    # These are actual selected three/four-file config renders, without a daemon.
    for selection in ("bundled", "byo-s3"):
        for rotation in ("defaults", "durable-rotation"):
            durable = dict(all_values, SELFHOST_STORAGE_MODE=selection)
            if selection == "bundled":
                for key in STORAGE:
                    durable.pop(key, None)
            expected_version, expected_retired = "1", ""
            if rotation == "durable-rotation":
                expected_version, expected_retired = "7", "6:" + "11" * 32
                durable.update(MFA_ENCRYPTION_KEY_VERSION=expected_version,
                               MFA_ENCRYPTION_KEYS_RETIRED=expected_retired)
            dotenv(durable, fixture / ".env")
            name = "MFA " + selection + " " + rotation
            (WORK / "unrelated-control.jsonl").write_text("")
            clean_env = dict(BASE_ENV, P2_UNRELATED_CONTROL="synthetic-unrelated")
            control = model(capture("mfa-" + selection + "-" + rotation + "-clean",
                                    ["bash", str(runner), str(helper), str(fixture)], clean_env),
                            name + " clean selected model")
            control_env = control["services"]["control-plane"]["environment"]
            check(name + " uses durable/default active version and retired keys",
                  control_env.get("MFA_ENCRYPTION_KEY_VERSION") == expected_version and
                  control_env.get("MFA_ENCRYPTION_KEYS_RETIRED") == expected_retired)
            conflicting = dict(clean_env, MFA_ENCRYPTION_KEY_VERSION="99",
                               MFA_ENCRYPTION_KEYS_RETIRED="98:" + "22" * 32)
            actual = model(capture("mfa-" + selection + "-" + rotation + "-ambient",
                                   ["bash", str(runner), str(helper), str(fixture)], conflicting),
                           name + " conflicting ambient selected model")
            check(name + " ignores inherited rotation without altering selected model", actual == control)
            # The real config-only Docker boundary records only a boolean canary.
            observations = (WORK / "unrelated-control.jsonl").read_text().splitlines()
            check(name + " preserves unrelated inherited variable at Compose boundary",
                  len(observations) == 6 and all(value == "true" for value in observations))
    packaging()
    for name, digest in SOURCE_HASHES.items():
        check("managed/base accepted source bytes remain preserved: " + name,
              hashlib.sha256((ROOT / name).read_bytes()).hexdigest() == digest)
    for name, source_bytes in SOURCE_BYTES.items():
        check("test leaves shared source bytes unchanged: " + name,
              (ROOT / name).read_bytes() == source_bytes)
    print("INFO: literal Docker Config.Env parity uses separate Task-1 evidence; unexecuted here")


try:
    run()
except (OSError, ValueError, TypeError, KeyError, StopIteration, RuntimeError, yaml.YAMLError):
    # Never expose parser errors, raw render JSON, env data, stderr, or differences.
    check("model/packaging prerequisite or fixture parsing failed; behavior gate incomplete", False)
print("passed: %d  failed: %d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
PY
