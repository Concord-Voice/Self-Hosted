#!/usr/bin/env python3
"""Exercise copy-certs discovery against Docker's real Compose labels."""

import argparse
import json
import os
import re
import shutil
import subprocess
import tempfile
import uuid
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
DEFAULT_HOOK = ROOT / "infrastructure/deploy/copy-certs.sh"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def docker(*args, **kwargs):
    return subprocess.run(["docker", *args], text=True, capture_output=True, **kwargs)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hook", type=Path, default=DEFAULT_HOOK)
    args = parser.parse_args()
    require(shutil.which("docker"), "Docker CLI required for Compose label regression")
    require(docker("compose", "version").returncode == 0, "Docker Compose plugin required")

    project = "ctlabels" + uuid.uuid4().hex[:12]
    container_name = "ctlabels-container-" + uuid.uuid4().hex[:10]
    image = os.environ.get("SELFHOST_CERT_TEST_IMAGE", "ubuntu:24.04")
    require(docker("image", "inspect", image).returncode == 0,
            f"fixture image must already be cached: {image}")
    root = Path(tempfile.mkdtemp(prefix="copy-certs-labels-"))
    try:
        base = root / "compose.yml"
        overlay = root / "compose.override.yml"
        base.write_text(
            "services:\n"
            "  coturn:\n"
            f"    image: {image}\n"
            "    network_mode: none\n"
            "    command: [\"/bin/true\"]\n"
        )
        overlay.write_text(f"services:\n  coturn:\n    container_name: {container_name}\n")
        compose = ["compose", "-p", project, "-f", str(base), "-f", str(overlay)]
        created = docker(*compose, "create", "coturn")
        require(created.returncode == 0,
                f"stopped Compose fixture creation failed: {created.stderr.strip()}")
        cid = docker("inspect", "--format", "{{.Id}}", container_name)
        require(cid.returncode == 0 and re.fullmatch(r"[0-9a-f]{64}\n?", cid.stdout),
                "fixture must have a real Docker container ID")
        cid = cid.stdout.strip()

        hook = args.hook.read_text()
        format_match = re.search(r"--format\s+'([^']*)'", hook)
        require(format_match is not None, "could not extract shipped docker ps discovery template")
        template = format_match.group(1)

        # Keep the legacy Docker summary as a control even after production switches
        # to a selected projection.
        legacy_output = docker("ps", "--all", "--filter", f"id={cid}", "--format", "{{json .}}")
        require(legacy_output.returncode == 0, "Docker must render the legacy summary")
        legacy = json.loads(legacy_output.stdout)
        labels = legacy.get("Labels")
        require(isinstance(labels, str), "Docker's legacy Labels field must be flattened text")
        label_fragments = labels.split(",")
        config_index = next((index for index, part in enumerate(label_fragments)
                             if part.startswith("com.docker.compose.project.config_files=")), None)
        require(config_index is not None and config_index + 1 < len(label_fragments) and
                "=" not in label_fragments[config_index + 1],
                "two-file Compose config_files label must flatten into a comma-bearing summary")

        rendered = docker("ps", "--all", "--filter", f"id={cid}", "--format", template)
        require(rendered.returncode == 0 and rendered.stdout.strip(),
                f"Docker must render the shipped discovery template: {rendered.stderr.strip()}")
        row = json.loads(rendered.stdout)
        # Normalize only Docker's generated name; preserve the actual ID and labels.
        row["Names"] = "concordvoice-coturn"
        discovery = json.dumps(row, separators=(",", ":"))

        start = hook.index("if ! CANDIDATES=\"$(python3 -c '")
        end_marker = "\nfi\n\nCONSUMER_STATE=absent"
        end = hook.index(end_marker, start) + len("\nfi")
        parser_block = hook[start:end]
        runner = ("fail() { printf '%s\\n' \"$*\" >&2; exit 2; }\nDISCOVERY=$1\n" +
                  parser_block + "\nprintf '%s\\n' \"$CANDIDATES\"\n")

        def run_parser(payload):
            return subprocess.run(["bash", "-c", runner, "parser", payload],
                                  text=True, capture_output=True)

        project_label = legacy["Labels"].split(",")
        expected_project = next(part.split("=", 1)[1] for part in project_label
                                 if part.startswith("com.docker.compose.project="))
        expected = f"{cid[:12]}\tconcordvoice-coturn\t{expected_project}\n"

        if template == "{{json .}}":
            # Positive control for the original parser: removing just the broken
            # config_files label restores discovery.
            fragments = labels.split(",")
            kept = []
            removing_config_files = False
            for part in fragments:
                if part.startswith("com.docker.compose.project.config_files="):
                    removing_config_files = True
                    continue
                if removing_config_files and "=" not in part:
                    removing_config_files = False
                    continue
                removing_config_files = False
                kept.append(part)
            legacy["Labels"] = ",".join(kept)
            legacy["Names"] = "concordvoice-coturn"
            control = run_parser(json.dumps(legacy, separators=(",", ":")))
            require(control.returncode == 0 and control.stdout == expected,
                    "positive control failed after removing only comma-bearing config_files")
            print("positive control passed: removing config_files admits the legacy row")

        result = run_parser(discovery)
        require(result.returncode == 0 and result.stdout == expected,
                "valid multi-file Compose labels must yield exactly one validated coturn candidate "
                f"(parser rc={result.returncode}, stderr={result.stderr!r}, expected={expected!r})")
    finally:
        docker("rm", "-f", container_name)
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    main()
