#!/usr/bin/env python3
"""Emit a protected PR caller proof, then dispatch and watch its exact Sonar run."""

import argparse
import base64
import binascii
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


REPO = "Concord-Voice/Concord-Voice-Alpha"
API = f"https://api.github.com/repos/{REPO}"
DISPATCH = f"{API}/actions/workflows/sonarcloud.yml/dispatches"
AUDIENCE = "concord-voice:protected-sonar:sha256:"
IDS = ("caller_run_id", "caller_run_attempt", "pr_number", "expected_head_sha",
       "expected_base_sha", "pr_ci_result")
COMPONENTS = ("desktop", "control_plane", "media_plane", "admin")
JOBS = ("ioc", "build", "semgrep", "eslint", "govulncheck")
SELECTED = ("codeql_actions", "codeql_go", "codeql_js", "eslint", "govulncheck",
            "osv", "semgrep_custom", "semgrep_community", "semgrep_canary", "sonar",
            "sonar_force_all", "sonar_desktop", "sonar_control_plane",
            "sonar_media_plane", "sonar_admin")
SELECTION_KEYS = ("version", "known", "mode", "head_sha", "base_sha", "merge_sha",
                  "changed_files", "policy_sha", "hotfix", "selected", "advisory",
                  "workers", "npm_workspaces", "license_go", "coverage_required",
                  "codeql_matrix")
SHA = re.compile(r"[0-9a-f]{40}\Z")
JWT = re.compile(r"[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\Z")
MAX_INPUT = 24 * 1024
MAX_PROOF = 64 * 1024
MAX_RESPONSE = 64 * 1024
MAX_POLLS = 270  # 45 minutes at 10 seconds; the child scanner may run for 40.
RESULTS = {"success", "failure", "cancelled", "skipped"}


class Invalid(Exception):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


urlopen = build_opener(NoRedirect()).open


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True,
                      allow_nan=False).encode("utf-8")


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Invalid("duplicate JSON key")
        result[key] = value
    return result


def decode(raw):
    try:
        return json.loads(raw, object_pairs_hook=unique_pairs)
    except (ValueError, UnicodeError, TypeError) as exc:
        raise Invalid("invalid JSON") from exc


def read(path, limit):
    with Path(path).open("rb") as stream:
        raw = stream.read(limit + 1)
    if not raw or len(raw) > limit:
        raise Invalid("file size invalid")
    return raw


def positive(value):
    return type(value) is int and 0 < value <= 2**63 - 1


def validate_payload(payload):
    if not isinstance(payload, dict) or set(payload) != set(IDS) | {"selection", "jobs", "coverage"}:
        raise Invalid("payload keys invalid")
    if not all(positive(payload[key]) for key in IDS[:3]):
        raise Invalid("candidate IDs invalid")
    if not all(isinstance(payload[key], str) and SHA.fullmatch(payload[key]) for key in IDS[3:5]):
        raise Invalid("candidate SHAs invalid")
    if payload["pr_ci_result"] != "success":
        raise Invalid("PR CI did not succeed")
    selection = payload["selection"]
    if (not isinstance(selection, dict) or set(selection) != set(SELECTION_KEYS)
            or type(selection["version"]) is not int or selection["version"] != 1
            or selection.get("known") is not True
            or selection.get("mode") not in ("required-full", "required-relevant")
            or type(selection["changed_files"]) is not int
            or not 0 <= selection["changed_files"] <= 3000
            or type(selection["hotfix"]) is not bool
            or type(selection["license_go"]) is not bool
            or not isinstance(selection["advisory"], dict)
            or not isinstance(selection["workers"], list)
            or not isinstance(selection["npm_workspaces"], list)
            or not isinstance(selection["codeql_matrix"], str)
            or selection.get("head_sha") != payload["expected_head_sha"]
            or selection.get("base_sha") != payload["expected_base_sha"]
            or not all(isinstance(selection.get(key), str) and SHA.fullmatch(selection[key])
                       for key in ("merge_sha", "policy_sha"))):
        raise Invalid("scan selection invalid")
    selected = selection.get("selected")
    required = selection.get("coverage_required")
    if (not isinstance(selected, dict) or set(selected) != set(SELECTED)
            or any(type(value) is not bool for value in selected.values())
            or selected["sonar"] is not True
            or not isinstance(required, dict) or set(required) != set(COMPONENTS)
            or any(type(value) is not bool for value in required.values())):
        raise Invalid("Sonar selection invalid")
    jobs = payload["jobs"]
    if (not isinstance(jobs, dict) or set(jobs) != set(JOBS)
            or any(value not in RESULTS for value in jobs.values())
            or jobs["build"] != "success"):
        raise Invalid("selected jobs invalid")
    coverage = payload["coverage"]
    if (not isinstance(coverage, dict) or set(coverage) != set(COMPONENTS)
            or any(type(value) is not bool for value in coverage.values())
            or any(required[key] and not coverage[key] for key in COMPONENTS)):
        raise Invalid("coverage flags invalid")
    if len(canonical(payload)) > MAX_INPUT:
        raise Invalid("payload too large")
    return payload


def request_json(url, token, *, data=None, expected=200, limit=MAX_RESPONSE):
    headers = {"Accept": "application/vnd.github+json", "Authorization": f"Bearer {token}",
               "X-GitHub-Api-Version": "2026-03-10", "User-Agent": "Concord-Voice-protected-sonar"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    request = Request(url, data=data, headers=headers, method="POST" if data is not None else "GET")
    with urlopen(request, timeout=20) as response:
        if response.status != expected:
            raise Invalid("unexpected API response status")
        raw = response.read(limit + 1)
    if not raw or len(raw) > limit:
        raise Invalid("API response size invalid")
    value = decode(raw)
    if not isinstance(value, dict):
        raise Invalid("API response invalid")
    return value


def oidc_token(payload_bytes):
    base = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_URL", "")
    bearer = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
    parts = urlsplit(base)
    if (parts.scheme != "https" or parts.hostname not in
            ("pipelines.actions.githubusercontent.com", "token.actions.githubusercontent.com")
            or parts.username or parts.password or parts.fragment or not bearer):
        raise Invalid("OIDC request environment unavailable")
    audience = AUDIENCE + hashlib.sha256(payload_bytes).hexdigest()
    original_query = parse_qsl(parts.query, keep_blank_values=True)
    if any(key == "audience" for key, _ in original_query):
        raise Invalid("OIDC request audience already set")
    query = parts.query + ("&" if parts.query else "") + urlencode({"audience": audience})
    url = urlunsplit((parts.scheme, parts.netloc, parts.path, query, ""))
    result = request_json(url, bearer, limit=MAX_PROOF)
    token = result.get("value")
    if not isinstance(token, str) or len(token) > 24 * 1024 or not JWT.fullmatch(token):
        raise Invalid("OIDC response invalid")
    return token


def write_private(path, data):
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(path, flags, 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
    except BaseException:
        Path(path).unlink(missing_ok=True)
        raise


def emit_proof(args):
    ids = decode(read(args.ids_json, MAX_INPUT))
    if not isinstance(ids, dict) or set(ids) != set(IDS):
        raise Invalid("IDs invalid")
    jobs = decode(read(args.jobs_json, MAX_INPUT))
    if (not isinstance(jobs, dict) or set(jobs) not in (set(JOBS), set(JOBS) | {"sonar"})
            or ("sonar" in jobs and jobs["sonar"] not in RESULTS)):
        raise Invalid("job verdicts invalid")
    jobs.pop("sonar", None)  # The old pin's Sonar result is not the bridge verdict.
    payload = validate_payload({**ids, "selection": decode(read(args.selection_json, MAX_INPUT)),
                                "jobs": jobs, "coverage": decode(read(args.coverage_json, MAX_INPUT))})
    raw = canonical(payload)
    proof = {"payload_b64": base64.b64encode(raw).decode("ascii"), "oidc_jwt": oidc_token(raw)}
    encoded = canonical(proof)
    if len(encoded) > MAX_PROOF:
        raise Invalid("proof too large")
    write_private(args.out, encoded)


def read_proof(path):
    proof = decode(read(path, MAX_PROOF))
    if not isinstance(proof, dict) or set(proof) != {"payload_b64", "oidc_jwt"}:
        raise Invalid("proof invalid")
    try:
        raw = base64.b64decode(proof["payload_b64"], validate=True)
    except (binascii.Error, ValueError, TypeError) as exc:
        raise Invalid("proof payload invalid") from exc
    if len(raw) > MAX_INPUT or base64.b64encode(raw).decode("ascii") != proof["payload_b64"]:
        raise Invalid("proof payload invalid")
    payload = validate_payload(decode(raw))
    if (raw != canonical(payload) or not isinstance(proof["oidc_jwt"], str)
            or len(proof["oidc_jwt"]) > 24 * 1024 or not JWT.fullmatch(proof["oidc_jwt"])):
        raise Invalid("proof encoding invalid")
    return payload


def dispatch_watch(args):
    payload = read_proof(args.proof)
    if Path(args.out).exists():
        raise Invalid("child output already exists")
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        raise Invalid("GitHub token unavailable")
    inputs = {key: str(payload[key]) for key in IDS[:5]}
    response = request_json(DISPATCH, token, data=canonical({"ref": "main", "inputs": inputs}))
    child_id = response.get("workflow_run_id")
    if not positive(child_id):
        raise Invalid("dispatch child ID missing")
    write_private(args.out, canonical({"workflow_run_id": child_id, "run_attempt": 1}))
    url = f"{API}/actions/runs/{child_id}"
    for attempt in range(MAX_POLLS):
        run = request_json(url, token)
        if (run.get("id") != child_id or run.get("run_attempt") != 1
                or run.get("event") != "workflow_dispatch" or run.get("head_branch") != "main"):
            raise Invalid("dispatch child identity invalid")
        if run.get("status") == "completed":
            if run.get("conclusion") != "success":
                raise Invalid("dispatch child failed")
            return
        if run.get("status") not in ("queued", "in_progress", "requested", "waiting", "pending"):
            raise Invalid("dispatch child status invalid")
        if attempt < MAX_POLLS - 1:
            time.sleep(10)
    raise Invalid("dispatch child timed out")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    emit = commands.add_parser("emit-proof")
    for name in ("ids-json", "selection-json", "jobs-json", "coverage-json", "out"):
        emit.add_argument(f"--{name}", required=True)
    watch = commands.add_parser("dispatch-watch")
    for name in ("proof", "out"):
        watch.add_argument(f"--{name}", required=True)
    args = parser.parse_args()
    try:
        if args.command == "emit-proof":
            emit_proof(args)
        else:
            dispatch_watch(args)
    except (Invalid, OSError, UnicodeError, TypeError, ValueError, OverflowError, RecursionError):
        print("protected Sonar caller failed; inspect local input and API status", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
