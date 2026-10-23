#!/usr/bin/env python3
"""Keep control-plane Docker builders and the selected Go toolchain aligned."""

import argparse
from pathlib import Path
import re
import subprocess
import sys


def selected_version(go_mod: str) -> str:
    """Use the preferred toolchain, or the minimum Go version when absent."""
    directives = {}
    for name in ("go", "toolchain"):
        values = re.findall(rf"(?m)^\s*{name}[ \t]+(\S+)", go_mod)
        if len(values) > 1 or (name == "go" and not values):
            raise ValueError(f"go.mod must declare exactly one {name} directive")
        if values:
            value = values[0]
            if name == "toolchain":
                if not value.startswith("go"):
                    raise ValueError("toolchain must name a patch-pinned Go release")
                value = value[2:]
            if not re.fullmatch(r"1\.\d+\.\d+", value):
                raise ValueError(f"{name} must name a patch-pinned Go release")
            directives[name] = value
    version = directives.get("toolchain", directives["go"])
    if tuple(map(int, version.split("."))) < tuple(map(int, directives["go"].split("."))):
        raise ValueError("toolchain must not be older than the go directive")
    return version


def check_dockerfile(path: Path, version: str) -> None:
    images = re.findall(
        r"(?im)^\s*FROM\s+(?:--platform=\S+\s+)?(golang(?:[:@]\S*)?)(?=\s|$)",
        path.read_text(),
    )
    if not images:
        raise ValueError(f"{path.name}: missing golang builder")
    for image in images:
        tag = image.split("@", 1)[0].removeprefix("golang:")
        if not re.fullmatch(rf"{re.escape(version)}(?:-[a-zA-Z0-9.-]+)?", tag):
            raise ValueError(f"{path.name}: {image} disagrees with go.mod's Go {version}")


def check_license_workflow(path: Path) -> None:
    """Require the license job to install the module's selected Go directly."""
    source = "services/control-plane/go.mod"
    diagnostic = f"{path.name}: check-go-licenses setup-go must use only go-version-file: {source}"
    try:
        import yaml
    except ImportError as error:
        raise ValueError("license workflow validation requires the pinned PyYAML dependency") from error

    class UniqueKeyLoader(yaml.SafeLoader):
        def construct_mapping(self, node, deep=False):
            keys = [self.construct_object(key, deep=deep) for key, _ in node.value]
            if len(keys) != len(set(keys)):
                raise ValueError("duplicate YAML mapping key")
            return super().construct_mapping(node, deep=deep)

    try:
        workflow = yaml.load(path.read_text(), Loader=UniqueKeyLoader)
        steps = workflow["jobs"]["check-go-licenses"]["steps"]
        if not isinstance(steps, list):
            raise ValueError("steps must be a list")
        setup = [step for step in steps if isinstance(step, dict)
                 and isinstance(step.get("uses"), str)
                 and step["uses"].startswith("actions/setup-go@")]
        if len(setup) != 1:
            raise ValueError("exactly one setup-go step is required")
        inputs = setup[0].get("with")
        if not isinstance(inputs, dict) or inputs.get("go-version-file") != source or "go-version" in inputs:
            raise ValueError("noncanonical setup-go inputs")
    except (yaml.YAMLError, KeyError, TypeError, ValueError) as error:
        raise ValueError(diagnostic) from error


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--check-runtime", action="store_true", help="also print and verify go version")
    parser.add_argument("--check-license-workflow", action="store_true", help="verify the license job's Go input")
    args = parser.parse_args()
    module = args.root.resolve() / "services/control-plane"
    try:
        version = selected_version((module / "go.mod").read_text())
        for filename in ("Dockerfile", "Dockerfile.ops-agent", "Dockerfile.redis-auth-controller"):
            check_dockerfile(module / filename, version)
        if args.check_license_workflow:
            check_license_workflow(args.root.resolve() / ".github/workflows/license-check.yml")
        if args.check_runtime:
            runtime = subprocess.check_output(["go", "version"], cwd=module, text=True).strip()
            print(runtime)
            if not re.fullmatch(rf"go version go{re.escape(version)} \S+/\S+", runtime):
                raise ValueError(f"running Go disagrees with go.mod's Go {version}")
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Go toolchain drift: {error}", file=sys.stderr)
        return 1
    print(f"Go {version}: go.mod and all Docker builders agree")
    return 0


if __name__ == "__main__":
    sys.exit(main())
