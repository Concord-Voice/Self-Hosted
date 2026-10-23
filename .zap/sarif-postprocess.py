#!/usr/bin/env python3
"""sarif-postprocess.py — make ZAP's DAST SARIF ingestible by GitHub Code
Scanning (#1465).

ZAP reports finding locations as the URL it scanned
(http://localhost:8080/api/v1/...). GitHub Code Scanning anchors every alert
to a file in the repository checkout, and rejects SARIF whose location URIs
carry a non-file scheme:

    SARIF URI scheme "http" did not match the checkout URI scheme "file"

This rewrites every finding's physical location to a single committed
sentinel file (.zap/DAST-FINDINGS.md, line 1) with uriBaseId %SRCROOT% — the
repo-relative shape Code Scanning expects — while PRESERVING the real scanned
URL by (a) prepending it to the alert message and (b) stashing it in
artifactLocation.properties.originalUri.

Discipline notes:
  - Only `artifactLocation.uri` values with an http(s) scheme are rewritten.
    rule.helpUri, versionControlProvenance.repositoryUri, and any other URI
    key are left untouched (we never rewrite a bare "uri" outside an
    artifactLocation context).
  - region is reset to line 1 wherever we rewrite, because ZAP's byte-offset
    regions are meaningless against the sentinel markdown file.
  - Idempotent: a uri already equal to the sentinel is skipped.

Stdlib only. Usage: sarif-postprocess.py <sarif-file> [sentinel-path]
"""

import json
import sys

DEFAULT_SENTINEL = ".zap/DAST-FINDINGS.md"


def _is_http(uri: object) -> bool:
    return isinstance(uri, str) and (uri.startswith("http://") or uri.startswith("https://"))


def rewrite_artifact_location(al: object, sentinel: str) -> str | None:
    """Rewrite an artifactLocation dict in place. Returns the original http(s)
    URI if one was rewritten, else None."""
    if not isinstance(al, dict):
        return None
    uri = al.get("uri")
    if not _is_http(uri):
        return None
    al.setdefault("properties", {})["originalUri"] = uri
    al["uri"] = sentinel
    al["uriBaseId"] = "%SRCROOT%"
    return uri


def walk(node: object, sentinel: str) -> None:
    """Recursively rewrite artifactLocations under physicalLocation (result
    locations) and under artifacts[].location (run-level artifact table)."""
    if isinstance(node, dict):
        pl = node.get("physicalLocation")
        if isinstance(pl, dict):
            if rewrite_artifact_location(pl.get("artifactLocation"), sentinel) is not None:
                pl["region"] = {"startLine": 1, "endLine": 1}
        loc = node.get("location")
        if isinstance(loc, dict) and "uri" in loc:
            rewrite_artifact_location(loc, sentinel)
        for value in node.values():
            walk(value, sentinel)
    elif isinstance(node, list):
        for item in node:
            walk(item, sentinel)


def enrich_messages(doc: dict) -> None:
    """Prepend the real scanned URL(s) to each result's message BEFORE the
    locations are rewritten, so the human reading the alert still sees the
    actual target."""
    for run in doc.get("runs", []):
        for res in run.get("results", []):
            urls = []
            for loc in res.get("locations", []):
                if not isinstance(loc, dict):
                    continue
                al = (loc.get("physicalLocation") or {}).get("artifactLocation") or {}
                if _is_http(al.get("uri")):
                    urls.append(al["uri"])
            if urls:
                msg = res.setdefault("message", {})
                base = msg.get("text", "") or ""
                prefix = "[" + ", ".join(dict.fromkeys(urls)) + "] "
                if not base.startswith(prefix):
                    msg["text"] = prefix + base


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: sarif-postprocess.py <sarif-file> [sentinel-path]", file=sys.stderr)
        return 2
    path = sys.argv[1]
    sentinel = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_SENTINEL

    with open(path) as f:
        doc = json.load(f)

    enrich_messages(doc)
    walk(doc, sentinel)

    # Fail loud if any http(s) artifactLocation survived — that would be
    # re-rejected by Code Scanning, and a silent pass-through would hide it.
    leaked = []

    def scan(node: object) -> None:
        if isinstance(node, dict):
            pl = node.get("physicalLocation")
            if isinstance(pl, dict):
                al = pl.get("artifactLocation") or {}
                if _is_http(al.get("uri")):
                    leaked.append(al["uri"])
            loc = node.get("location")
            if isinstance(loc, dict) and _is_http(loc.get("uri")):
                leaked.append(loc["uri"])
            for v in node.values():
                scan(v)
        elif isinstance(node, list):
            for it in node:
                scan(it)

    scan(doc)
    if leaked:
        print(f"[ERROR] {len(leaked)} http(s) artifactLocation URI(s) survived rewrite:",
              file=sys.stderr)
        for u in leaked[:10]:
            print(f"  {u}", file=sys.stderr)
        return 1

    with open(path, "w") as f:
        json.dump(doc, f, indent=2)

    results = sum(len(run.get("results", [])) for run in doc.get("runs", []))
    print(f"[OK] rewrote DAST locations onto {sentinel}; {results} result(s) preserved")
    return 0


if __name__ == "__main__":
    sys.exit(main())
