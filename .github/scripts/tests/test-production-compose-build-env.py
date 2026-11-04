#!/usr/bin/env python3
"""Exercise the cache-cold build job's real env against production Compose config.

Config interpolation needs no Docker daemon. Keep this test separate from the
full self-host deploy hook so its missing-job-env RED can be committed first.
"""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

try:
    import yaml
except ImportError:
    print("FAIL: prerequisite unavailable: PyYAML")
    sys.exit(2)


ROOT = Path(__file__).resolve().parents[3]
BUDGET_KEYS = ("WS_EVENT_BUDGET_ENABLED", "WS_EVENT_BUDGET_SCALE_PERCENT")
failures = 0


def check(name, condition):
    global failures
    print(("PASS: " if condition else "FAIL: ") + name)
    failures += not condition
    return condition


def config(cli, values, home):
    # No inherited env or repository .env can fill a missing job-scoped value.
    env = {"PATH": os.environ.get("PATH", ""), "HOME": str(home),
           "DOCKER_CONFIG": str(home), "LC_ALL": "C",
           "COMPOSE_DISABLE_ENV_FILE": "1"}
    env.update(values)
    return subprocess.run(
        cli + ["-p", "concord", "-f", str(ROOT / "docker-compose.yml"),
               "-f", str(ROOT / "docker-compose.production.yml"),
               "--profile", "services", "config", "--format", "json"],
        cwd=ROOT, env=env, capture_output=True, text=True, check=False)


def check_model(name, result, expected):
    if not check(name + " renders production Compose", result.returncode == 0):
        return
    try:
        services = json.loads(result.stdout)["services"]
        control = services["control-plane"]["environment"]
    except (ValueError, TypeError, KeyError):
        check(name + " has a readable control-plane model", False)
        return
    check(name + " maps exact budget strings to control-plane",
          all(control.get(key) == expected[key] for key in BUDGET_KEYS))
    check(name + " leaks no budget keys to other services", all(
          not any(key in service.get("environment", {}) for key in BUDGET_KEYS)
          for service_name, service in services.items()
          if service_name != "control-plane"))


def main():
    docker = shutil.which("docker")
    standalone = shutil.which("docker-compose")
    cli = [docker, "compose"] if docker else [standalone] if standalone else None
    if cli is None:
        print("FAIL: prerequisite unavailable: Docker Compose >= 2.24.5")
        return 2
    version = subprocess.run(cli + ["version", "--short"], cwd=ROOT,
                             capture_output=True, text=True, check=False)
    match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?\s*", version.stdout)
    if version.returncode or match is None or tuple(map(int, match.groups())) < (2, 24, 5):
        print("FAIL: prerequisite unavailable: Docker Compose >= 2.24.5")
        return 2
    try:
        workflow = yaml.safe_load((ROOT / ".github/workflows/build.yml").read_text())
        job_env = workflow["jobs"]["production-compose-docker"]["env"]
    except (OSError, ValueError, TypeError, KeyError, yaml.YAMLError):
        check("cache-cold job environment is readable", False)
        return 1
    if not check("cache-cold build job env contains only literal values",
                 isinstance(job_env, dict) and all(
                     isinstance(key, str) and isinstance(value, str)
                     for key, value in job_env.items())):
        return 1

    # Compose reports its first required-key error, so remove each key alone.
    absent = {key: value for key, value in job_env.items() if key not in BUDGET_KEYS}
    repaired = dict(absent, WS_EVENT_BUDGET_ENABLED="true",
                    WS_EVENT_BUDGET_SCALE_PERCENT="100")
    with tempfile.TemporaryDirectory(prefix="production-compose-build-env-") as private:
        home = Path(private)
        for key in BUDGET_KEYS:
            one_missing = dict(repaired)
            one_missing.pop(key)
            missing = config(cli, one_missing, home)
            check("production Compose names missing " + key,
                  missing.returncode != 0 and key + " must be set" in missing.stderr)
        check_model("repaired job env", config(cli, repaired, home), repaired)
        check("actual cache-cold job carries exact synthetic budget pair",
              tuple(job_env.get(key) for key in BUDGET_KEYS) == ("true", "100"))
        check_model("actual cache-cold job env", config(cli, job_env, home), job_env)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
