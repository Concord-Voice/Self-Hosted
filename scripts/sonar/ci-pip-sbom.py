#!/usr/bin/env python3
"""Export the CI Pip pins for Sonar SCA without running package build code."""

import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MANIFESTS = (
    ".github/requirements-pyyaml.txt",
    ".github/requirements-squawk.txt",
    ".github/semgrep/requirements.txt",
)
PIN = re.compile(r"^([A-Za-z0-9][A-Za-z0-9_.-]*)==([^;\s\\]+)(?:\s*;\s*(.*?))?\s*\\$")
HASH = re.compile(r"^\s+--hash=sha256:[0-9a-f]{64}\s*\\?$")
MARKERS = {
    None: True,
    "platform_python_implementation != 'PyPy'": True,
    "implementation_name != 'PyPy' and platform_python_implementation != 'PyPy'": True,
    "sys_platform == 'win32'": False,
    "sys_platform != 'emscripten'": True,
}


def components():
    found = {}
    for manifest in MANIFESTS:
        for line in (ROOT / manifest).read_text().splitlines():
            if not line or line.startswith("#") or HASH.fullmatch(line):
                continue
            match = PIN.fullmatch(line)
            if not match or match[3] not in MARKERS:
                raise ValueError(f"unsupported CI Pip pin in {manifest}: {line}")
            if not MARKERS[match[3]]:
                continue
            name = re.sub(r"[-_.]+", "-", match[1]).lower()
            version = match[2]
            if name in found and found[name] != version:
                raise ValueError(f"conflicting CI Pip pin: {name}")
            found[name] = version
    return [dict(type="library", name=name, version=version,
                 purl=f"pkg:pypi/{name}@{version}") for name, version in sorted(found.items())]


if __name__ == "__main__":
    output = ROOT / "sonar-ci-pip.cdx.json"
    output.write_text(json.dumps(dict(bomFormat="CycloneDX", specVersion="1.5", version=1,
                                      components=components()), indent=2) + "\n")
