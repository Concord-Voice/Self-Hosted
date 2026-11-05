#!/usr/bin/env python3
"""Run only reviewed MinIO Engine installation/admission, without publication."""

from pathlib import Path
import subprocess
import sys

import yaml

ROOT = Path(__file__).resolve().parents[2]
RUNNERS = {"publish": "ubuntu-24.04", "smoke-arm64": "ubuntu-24.04-arm",
           "finalize": "ubuntu-24.04"}


def select_steps(document, job_id, runner):
    if job_id not in RUNNERS or runner != RUNNERS[job_id]:
        raise ValueError("unadmitted publisher probe")
    job = document["jobs"][job_id]
    if job["runs-on"] != runner:
        raise ValueError("publisher runner changed")
    steps = job["steps"]
    installs = [(i, step) for i, step in enumerate(steps)
                if step.get("name", "").startswith("Install official Docker CE")]
    expected_guard = ("sudo " if job_id == "smoke-arm64" else "") + "bash scripts/ci/require-docker-engine.sh"
    guards = [(i, step) for i, step in enumerate(steps)
              if step.get("run", "").strip() == expected_guard]
    if len(installs) != 1 or len(guards) != 1 or installs[0][0] >= guards[0][0]:
        raise ValueError("publisher installation/admission changed")
    selected = [installs[0][1], guards[0][1]]
    if any("if" in step or step.get("continue-on-error", False) for step in selected):
        raise ValueError("conditional or advisory publisher admission")
    return [step["run"] for step in selected]


def main(argv):
    try:
        if len(argv) != 2:
            raise ValueError("expected fixed job and runner")
        document = yaml.safe_load((ROOT / ".github/workflows/publish-minio-image.yml").read_text())
        for script in select_steps(document, *argv):
            subprocess.run(["bash", "-e", "-o", "pipefail", "-c", script], cwd=ROOT, check=True)
    except (OSError, ValueError, KeyError, TypeError, yaml.YAMLError, subprocess.CalledProcessError):
        print("error: publisher Engine preflight failed", file=sys.stderr)
        return 1
    print("Publisher Engine preflight complete")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
