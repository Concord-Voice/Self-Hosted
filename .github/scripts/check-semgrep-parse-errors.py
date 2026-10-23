#!/usr/bin/env python3
"""Allow only the known Alpha TypeScript parser gaps during the split bootstrap."""
import json
import os
import sys

BOOTSTRAP_BASE = "85722fdb95a4dcccdef1d01a5501cd0a7e85631f"
KNOWN = {
    "client/admin/src/workspaces.tsx": ("`const satisfies` was unexpected", [96, 125, 137, 710, 715]),
    "client/admin/src/webauthn.ts": ("`T` was unexpected", [78]),
}


def accepted(error):
    path = error.get("path")
    if os.environ.get("BASELINE_COMMIT") != BOOTSTRAP_BASE or path not in KNOWN:
        return False
    fragment, lines = KNOWN[path]
    return (
        error.get("type", [None])[0] == "PartialParsing"
        and fragment in error.get("message", "")
        and [s["start"]["line"] for s in error.get("spans", [])] == lines
    )


errors = json.load(open(sys.argv[1], encoding="utf-8")).get("errors", [])
unexpected = [error for error in errors if not accepted(error)]
for error in unexpected:
    print(json.dumps(error, sort_keys=True))
if unexpected:
    raise SystemExit(f"Semgrep reported {len(unexpected)} unexpected scanner error(s)")
if errors:
    print(f"Semgrep: {len(errors)} known Alpha TypeScript parser gaps during split bootstrap")
