#!/usr/bin/env python3
"""Fail-closed, read-only verdict for selected PR scanners.

The caller must run this from a checkout of the protected workflow revision.
Task 2's upload reports are data, never proof of that revision or of a finding's
disposition. API and local provenance checks establish those facts separately.
"""

import argparse
from datetime import datetime, timezone
from http.client import HTTPException
import json
import os
import re
import selectors
import subprocess
import sys
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode, urlparse
from urllib.request import HTTPRedirectHandler, Request, build_opener

import yaml


REPO = "Concord-Voice/Concord-Voice-Alpha"
API = f"https://api.github.com/repos/{REPO}"
REPO_ROOT = Path(__file__).resolve().parents[2]
GIT_ENV = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
SHA = re.compile(r"[0-9a-f]{40}\Z")
SARIF_ID = re.compile(r"[A-Za-z0-9_-]{1,128}\Z")
# Breaks a scanner may count that git does not: git ends a line only at LF. A CR is ambiguous
# unless it is the last character of the line (a CRLF ending). The two chr() calls are the
# Unicode line and paragraph separators, kept out of the source as literals.
AMBIGUOUS_BREAK = re.compile(r"[\x0b\x0c\x1c-\x1e\x85" + chr(0x2028) + chr(0x2029) + r"]|\r(?!$)")
TOOLS = {
    "codeql_actions": ("CodeQL", "/language:actions", "codeql", "codeql-actions"),
    "codeql_go": ("CodeQL", "/language:go", "codeql", "codeql-go"),
    "codeql_js": ("CodeQL", "/language:javascript-typescript", "codeql", "codeql-javascript-typescript"),
    "semgrep_custom": ("Semgrep OSS", "semgrep-custom", "semgrep", "semgrep"),
    "semgrep_community": ("Semgrep OSS", "semgrep-community", "semgrep", "semgrep"),
    "eslint": ("ESLint", "eslint", "eslint", "eslint"),
    "govulncheck": ("govulncheck", "govulncheck", "govulncheck", "govulncheck"),
    "osv": ("osv-scanner", ".github/workflows/osv-scanner.yml:osv-scan", "osv", "osv"),
}
COMPONENTS = ("desktop", "control_plane", "media_plane", "admin")
ANALYSIS_KEYS = frozenset(("codeql_actions", "codeql_go", "codeql_js", "eslint", "govulncheck",
                           "osv", "semgrep_custom", "semgrep_community", "semgrep_canary", "sonar",
                           "sonar_force_all", "sonar_desktop", "sonar_control_plane",
                           "sonar_media_plane", "sonar_admin"))
MAX_REQUESTS = 120
MAX_BYTES = 12 * 1024 * 1024


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


urlopen = build_opener(_NoRedirect()).open


class Unknown(Exception):
    """Required external evidence could not be established."""


def _sha(value):
    return isinstance(value, str) and SHA.fullmatch(value) is not None


def _protected_osv_pin():
    """Read the OSV callee from the checked-out protected wrapper revision."""
    try:
        workflow = yaml.load((REPO_ROOT / ".github/workflows/required-pr-build.yml").read_text(),
                             Loader=yaml.BaseLoader)
        uses = workflow["jobs"]["osv"]["uses"]
    except (OSError, KeyError, TypeError, yaml.YAMLError) as exc:
        raise Unknown("protected OSV pin unavailable") from exc
    prefix = f"{REPO}/.github/workflows/osv-scanner.yml@"
    pin = uses.removeprefix(prefix) if isinstance(uses, str) else None
    if not _sha(pin):
        raise Unknown("protected OSV pin invalid")
    return pin


def _number(value):
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def _selected(selection):
    return selection.get("selected") if isinstance(selection.get("selected"), dict) else {}


def _result_level(result, rules, alert):
    level = result.get("level")
    if level is None:
        level = rules.get(result.get("ruleId"), {}).get("defaultConfiguration", {}).get("level")
    if level is None:
        level = alert.get("rule", {}).get("severity")
    return level


def _severity(result, rule, alert):
    values = (result.get("properties", {}), rule.get("properties", {}),
              alert.get("rule", {}))
    for obj in values:
        value = obj.get("security-severity", obj.get("security_severity_level"))
        if value is not None:
            if isinstance(value, str) and value.lower() in ("low", "medium", "high", "critical"):
                return {"low": 1.0, "medium": 4.0, "high": 7.0, "critical": 9.0}[value.lower()]
            try:
                score = float(value)
            except (TypeError, ValueError):
                raise Unknown("unknown security severity") from None
            if not 0 <= score <= 10:
                raise Unknown("unknown security severity")
            return score
    return None


def _finding_reasons(tool, analysis, alerts, main_alerts, instances, added_lines, ref):
    reasons = []
    sarif = analysis.get("sarif")
    if not isinstance(sarif, dict) or not isinstance(sarif.get("runs"), list):
        return ["unknown: SARIF invalid"]
    if len(sarif["runs"]) != 1:
        return ["unknown: SARIF run count invalid"]
    run = sarif["runs"][0]
    if not isinstance(run, dict) or not isinstance(run.get("results"), list):
        return ["unknown: SARIF results invalid"]
    if analysis.get("results_count") != len(run["results"]):
        return ["unknown: SARIF result count mismatch"]
    rules = {r.get("id"): r for r in run.get("tool", {}).get("driver", {}).get("rules", [])
             if isinstance(r, dict) and isinstance(r.get("id"), str)}
    by_number = {a.get("number"): a for a in alerts if isinstance(a, dict)}
    main_by_number = {a.get("number"): a for a in main_alerts if isinstance(a, dict)}
    for result in run["results"]:
        if not isinstance(result, dict) or not isinstance(result.get("properties"), dict):
            reasons.append("unknown: SARIF result invalid")
            continue
        number = result["properties"].get("github/alertNumber")
        alert = by_number.get(number)
        if not _number(number) or alert is None:
            reasons.append("unknown: SARIF alert identity unmatched")
            continue
        if alert.get("rule", {}).get("id") != result.get("ruleId"):
            reasons.append("unknown: SARIF alert rule mismatch")
            continue
        if alert.get("state") not in ("open", "dismissed", "fixed"):
            reasons.append("unknown: PR alert disposition invalid")
            continue
        baseline = main_by_number.get(number)
        if baseline and baseline.get("state") not in ("open", "dismissed", "fixed"):
            reasons.append("unknown: baseline alert disposition invalid")
            continue
        if baseline and baseline.get("state") == "dismissed" and alert.get("state") == "open":
            reasons.append("unknown: baseline dismissal differs on PR")
            continue
        if alert.get("state") == "dismissed":
            continue
        if baseline and baseline.get("state") in ("open", "dismissed"):
            continue  # Existing alert identity on main is the observable baseline.
        try:
            score = _severity(result, rules.get(result.get("ruleId"), {}), alert)
        except Unknown as exc:
            reasons.append(f"unknown: {exc}")
            continue
        if score is not None:
            blocking = score >= (7.0 if tool in ("govulncheck", "osv-scanner") else 4.0)
        else:
            level = _result_level(result, rules, alert)
            if level not in ("none", "note", "warning", "error"):
                reasons.append("unknown: finding severity missing or invalid")
                continue
            blocking = level == "error" if tool in ("govulncheck", "osv-scanner") else level in ("warning", "error")
        if not blocking:
            continue
        locations = result.get("locations")
        if not isinstance(locations, list) or not locations:
            reasons.append("unknown: blocking finding location missing")
            continue
        physical = locations[0].get("physicalLocation", {}) if isinstance(locations[0], dict) else {}
        artifact = physical.get("artifactLocation", {})
        path = artifact.get("uri")
        if artifact.get("uriBaseId"):
            reasons.append("unknown: blocking finding URI base unresolved")
            continue
        region = physical.get("region", {})
        start = region.get("startLine")
        end = region.get("endLine", start)
        if (not isinstance(path, str) or not path or path.startswith("/") or ".." in Path(path).parts
                or not _number(start) or not _number(end) or end < start or end - start > 10000):
            reasons.append("unknown: blocking finding location invalid")
            continue
        # Known scanner roots are explicit; an unfamiliar URI cannot be guessed.
        if tool == "ESLint" and not path.startswith("client/desktop/"):
            path = f"client/desktop/{path}"
        if tool == "govulncheck" and not path.startswith("services/control-plane/"):
            path = f"services/control-plane/{path}"
        lines = added_lines.get(path, [])
        if not isinstance(lines, list) or not all(_number(n) for n in lines):
            reasons.append("unknown: blocking finding diff location unresolved")
            continue
        if not any(n in lines for n in range(start, end + 1)):
            # A complete diff proves this finding lies wholly outside the PR.
            continue
        observed = instances.get(str(number)) if isinstance(instances, dict) else None
        if not isinstance(observed, list) or len([i for i in observed if isinstance(i, dict) and i.get("ref") == ref]) != 1:
            reasons.append("unknown: PR alert instance missing or ambiguous")
            continue
        reasons.append(f"policy: {tool} new blocking finding at changed diff line {path}:{start}")
    return reasons


def _verify_verdict(selection, jobs, evidence, current_pr):
    """Return stable blocking reasons from normalized, independently sourced evidence."""
    reasons = []
    if not isinstance(selection, dict) or not isinstance(jobs, dict) or not isinstance(evidence, dict) or not isinstance(current_pr, dict):
        return ["unknown: malformed verdict input"]
    if (selection.get("version") != 1 or selection.get("known") is not True
            or selection.get("mode") not in ("required-full", "required-relevant")
            or not all(_sha(selection.get(k)) for k in ("head_sha", "base_sha", "merge_sha", "policy_sha"))):
        return ["unknown: selection invalid"]
    selected = _selected(selection)
    if set(selected) != ANALYSIS_KEYS or any(not isinstance(v, bool) for v in selected.values()):
        return ["unknown: selection categories invalid"]
    coverage_required = selection.get("coverage_required")
    if (not isinstance(coverage_required, dict) or set(coverage_required) != set(COMPONENTS)
            or any(not isinstance(v, bool) for v in coverage_required.values())):
        return ["unknown: coverage selection invalid"]
    if (selected["sonar_force_all"] or any(selected[f"sonar_{c}"] for c in COMPONENTS)) and not selected["sonar"]:
        return ["unknown: Sonar selection inconsistent"]
    for label, actual in (("head", current_pr.get("head", {}).get("sha")),
                          ("base", current_pr.get("base", {}).get("sha")),
                          ("merge", current_pr.get("merge_commit_sha"))):
        if actual != selection[f"{label}_sha"]:
            reasons.append(f"unknown: PR {label} revision moved")
    run = evidence.get("run", {})
    if (not isinstance(run, dict) or str(run.get("id", "")) == ""
            or not _number(run.get("run_attempt")) or run.get("head_sha") != selection["head_sha"]):
        reasons.append("unknown: Actions run identity invalid")
    ref = f"refs/pull/{current_pr.get('number')}/merge"
    refs = run.get("referenced_workflows", []) if isinstance(run, dict) else []
    if evidence.get("source_policy_sha") != selection["policy_sha"]:
        reasons.append("unknown: protected policy SHA mismatch")
    matches = [w for w in refs if isinstance(w, dict)
               and w.get("path") == f"{REPO}/.github/workflows/pr-ci.yml@{selection['policy_sha']}"
               and w.get("ref") in (None, ref, "refs/heads/main")
               and w.get("sha") == selection["policy_sha"]] if isinstance(refs, list) else []
    if evidence.get("provenance_verified") is not True or len(matches) != 1:
        reasons.append("unknown: protected workflow provenance unproven")
    if evidence.get("artifact_verified") is not True:
        reasons.append("unknown: current-attempt artifact provenance unproven")
    if evidence.get("pagination_complete") is not True:
        reasons.append("unknown: pagination incomplete")
    for err in evidence.get("http_errors", []):
        reasons.append(f"unknown: HTTP {err.get('status', 'error')}")
    diff = evidence.get("diff", {})
    if not isinstance(diff, dict) or diff.get("complete") is not True or not isinstance(diff.get("added_lines"), dict):
        reasons.append("unknown: diff incomplete")
    reports = evidence.get("reports", {})
    analyses = evidence.get("analyses", [])
    latest = evidence.get("latest_analyses", [])
    if not isinstance(reports, dict) or not all(isinstance(v, list) for v in (analyses, latest)):
        return reasons + ["unknown: scan evidence malformed"]
    if "build" not in jobs:
        reasons.append("unknown: selected build missing")
    elif jobs["build"] != "success":
        reasons.append("policy: selected build failed")
    if selected["semgrep_canary"] and jobs.get("semgrep") != "success":
        reasons.append("policy: Semgrep canary selected job did not succeed")
    if selected["osv"] and evidence.get("osv_source_verified") is not True:
        reasons.append("unknown: protected OSV workflow source unproven")
    seen = set()
    for key, (tool, category, job, suffix) in TOOLS.items():
        if selected.get(key) is not True:
            continue
        if job not in jobs:
            reasons.append(f"unknown: {tool} selected job missing")
        elif jobs[job] != "success":
            reasons.append(f"policy: {tool} selected job {jobs.get(job, 'missing')}")
        matches = [a for a in analyses if isinstance(a, dict) and a.get("tool") == tool
                   and _category_matches(key, a, category)]
        if len(matches) != 1:
            reasons.append(f"{tool}: selected analysis missing" if not matches else f"unknown: {tool} analysis ambiguous")
            continue
        a = matches[0]
        if a.get("ref") != ref:
            reasons.append(f"unknown: {tool} wrong ref")
        if a.get("commit_sha") != selection["merge_sha"]:
            reasons.append(f"unknown: {tool} wrong revision")
        if a.get("error") or a.get("processing_status") != "complete":
            reasons.append(f"unknown: {tool} analysis processing incomplete")
        same_latest = [x for x in latest if isinstance(x, dict)
                       and x.get("category") == a.get("category") and x.get("tool") == tool]
        if len(same_latest) != 1 or same_latest[0].get("sarif_id") != a.get("sarif_id"):
            reasons.append(f"unknown: {tool} latest upload mismatch")
        name = f"scan-evidence-{run.get('id')}-{run.get('run_attempt')}-{suffix}"
        wrapper = reports.get(name)
        if not isinstance(wrapper, dict):
            reasons.append(f"unknown: {tool} report artifact missing")
            continue
        meta, report = wrapper.get("artifact"), wrapper.get("report")
        if (not isinstance(meta, dict) or meta.get("name") != name or meta.get("expired") is not False
                or not _number(meta.get("id")) or meta.get("workflow_run", {}).get("id") != int(run["id"])
                or not isinstance(report, dict) or str(report.get("run_id")) != str(run.get("id"))
                or str(report.get("run_attempt")) != str(run.get("run_attempt"))):
            reasons.append(f"unknown: {tool} current-attempt report invalid")
            continue
        uploads = report.get("uploads", []) if suffix == "semgrep" else [report]
        matching = [u for u in uploads if isinstance(u, dict) and u.get("category") == category]
        if len(matching) != 1 or not isinstance(matching[0].get("sarif_id"), str) or not matching[0]["sarif_id"]:
            reasons.append(f"unknown: {tool} upload report missing or ambiguous")
        elif matching[0]["sarif_id"] != a.get("sarif_id"):
            reasons.append(f"unknown: {tool} wrong upload")
        if a.get("id") in seen:
            reasons.append("unknown: duplicate analysis identity")
        seen.add(a.get("id"))
        if tool != "ESLint" and diff.get("complete") is True:
            reasons.extend(_finding_reasons(tool, a, evidence.get("alerts", []), evidence.get("main_alerts", []),
                                            evidence.get("instances", {}), diff["added_lines"], ref))
    if selected["sonar"]:
        head_repo = current_pr.get("head", {}).get("repo")
        external_pr = not isinstance(head_repo, dict) or head_repo.get("full_name") != REPO
        # Fork PRs have no SONAR_TOKEN; Dependabot PRs read it from their own
        # secret store and must scan. The trusted App check remains mandatory
        # and independently bound to this PR head.
        sonar_job_ok = jobs.get("sonar") == "success" or (external_pr and jobs.get("sonar") == "skipped")
        if not sonar_job_ok or jobs.get("build") != "success":
            reasons.append("policy: Sonar selected build or scan did not succeed")
        for component in COMPONENTS:
            if selection.get("coverage_required", {}).get(component):
                artifacts = evidence.get("coverage_artifacts", [])
                flags = jobs.get("coverage_uploads", {})
                if not isinstance(flags, dict) or flags.get(component) is not True:
                    reasons.append(f"unknown: {component} coverage upload flag missing")
                if not any(isinstance(x, dict) and x.get("component") == component and x.get("present") is True for x in artifacts):
                    reasons.append(f"unknown: {component} coverage artifact missing")
        check = evidence.get("sonar")
        if (not isinstance(check, dict) or check.get("app_id") != 12526
                or check.get("conclusion") != "success" or check.get("status", "completed") != "completed"
                or check.get("head_sha") != selection["head_sha"]
                or check.get("analysis_window_verified") is not True):
            reasons.append("unknown: Sonar App check stale or identity invalid")
    return reasons


def verify_verdict(selection, jobs, evidence, current_pr):
    """Return blocking reasons; malformed evidence is always an unknown verdict."""
    try:
        return _verify_verdict(selection, jobs, evidence, current_pr)
    except (AttributeError, IndexError, KeyError, TypeError, ValueError):
        return ["unknown: malformed normalized evidence"]


class GitHub:
    def __init__(self, token):
        if not token:
            raise Unknown("GitHub token missing")
        self.token = token
        self.requests = 0

    def get(self, path, *, accept="application/vnd.github+json"):
        url = API + path
        if (not path.startswith("/") or ".." in urlparse(url).path.split("/")
                or urlparse(url).hostname != "api.github.com"):
            raise Unknown("API path invalid")
        self.requests += 1
        if self.requests > MAX_REQUESTS:
            raise Unknown("API request bound exceeded")
        req = Request(url, headers={"Accept": accept, "Authorization": f"Bearer {self.token}",
                                    "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "Concord-Voice-scan-verdict"})
        try:
            with urlopen(req, timeout=15) as response:
                if response.url != url:  # No API redirect, including signed artifact storage.
                    raise Unknown("API redirected")
                data = response.read(MAX_BYTES + 1)
                if len(data) > MAX_BYTES:
                    raise Unknown("API response too large")
                return json.loads(data), response.headers.get("Link", "")
        except HTTPError as exc:
            raise Unknown(f"HTTP {exc.code}") from None
        # OSError covers URLError and TimeoutError. A body read that dies after the connection is up
        # raises the raw error, not URLError, and Python 3.11-3.13 raise RecursionError for a deeply
        # nested body, so both are named here rather than left as a traceback.
        except (OSError, HTTPException, ValueError, RecursionError) as exc:
            raise Unknown(f"API request invalid: {type(exc).__name__}") from None

    def pages(self, path, field=None):
        values = []
        # shortcut: 20 pages cover the current baseline; narrow the query if it exceeds 2,000 alerts.
        for page in range(1, 21):
            delimiter = "&" if "?" in path else "?"
            body, links = self.get(f"{path}{delimiter}per_page=100&page={page}")
            items = body.get(field) if field else body
            if not isinstance(items, list):
                raise Unknown("API page shape invalid")
            values.extend(items)
            if len(values) > 2000:
                raise Unknown("API pagination bound exceeded")
            if 'rel="next"' not in links:
                return values
        raise Unknown("API pagination incomplete")


def _read_json(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 1024 * 1024:
        raise Unknown("local evidence file invalid")
    try:
        return json.loads(path.read_text())
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise Unknown("local evidence JSON invalid") from None


def _time(value):
    if not isinstance(value, str):
        raise Unknown("analysis time missing")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise Unknown("analysis time invalid") from None
    if parsed.tzinfo is None:
        raise Unknown("analysis time lacks timezone")
    return parsed.astimezone(timezone.utc)


def _git_text(args):
    """Run a read-only git query in the protected checkout and return its bounded output."""
    command = ["git", "-C", str(REPO_ROOT), *args]
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=GIT_ENV)
    chunks = []
    size = 0
    stderr_seen = False
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    selector.register(process.stderr, selectors.EVENT_READ)
    deadline = time.monotonic() + 20
    try:
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Unknown("diff time bound exceeded")
            ready = selector.select(remaining)
            if not ready:
                raise Unknown("diff time bound exceeded")
            for key, _ in ready:
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                size += len(chunk)
                if size > MAX_BYTES:
                    raise Unknown("diff size bound exceeded")
                if key.fileobj is process.stdout:
                    chunks.append(chunk)
                else:
                    stderr_seen = True
        process.wait(timeout=max(0.1, deadline - time.monotonic()))
    except Unknown:
        process.kill()
        process.wait()
        raise
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
        raise Unknown("diff time bound exceeded") from None
    finally:
        selector.close()
        process.stdout.close()
        process.stderr.close()
    if process.returncode or stderr_seen:
        raise Unknown("diff incomplete")
    try:
        return b"".join(chunks).decode("utf-8")
    except UnicodeDecodeError:
        raise Unknown("diff encoding invalid") from None


def _base_parent(head, merge):
    """Return the base GitHub merged the PR head into: the other parent of its merge commit.

    The selection's base SHA comes from the PR API, which can lag the merge commit it was
    served with, so the diff base is read from the merge commit itself. Requiring the head
    as one of exactly two parents also ties the scanned tree to the head under verification.
    """
    if not _sha(head) or not _sha(merge):
        raise Unknown("diff revision invalid")
    try:
        fields = _git_text(["rev-list", "--parents", "-n", "1", merge]).split()
    except Unknown:
        raise Unknown("merge revision unavailable") from None
    if len(fields) != 3 or head not in fields[1:]:
        raise Unknown("merge revision does not join the PR head")
    return fields[2] if fields[1] == head else fields[1]


def _diff_lines(base, merge):
    """Map each path to the lines the PR adds, numbered as in the merge commit.

    Scanners analyse GitHub's merge commit, so a finding's line numbers belong to that
    tree, and the merge against the base it merged into is the PR's own contribution in
    those numbers. The base against the PR head is not: a head behind the base reads every
    file the base gained since the fork point as deleted, and base edits above a hunk move
    its numbers away from the scanned tree's. A path git treats as binary, or with an added
    line holding a break a scanner may count and git does not, has no reliable line
    information, so it maps to None and a blocking finding there stays unresolved instead
    of reading as outside the PR.
    """
    if not _sha(base) or not _sha(merge):
        raise Unknown("diff revision invalid")
    # Pin every format option a global git config could change: the parser reads this shape.
    flags = ["--no-ext-diff", "--no-textconv", "--no-color", "--no-renames"]
    output = _git_text(["diff", *flags, "--unified=0", "--src-prefix=a/", "--dst-prefix=b/",
                        base, merge, "--"])
    added = {}
    suspect = set()
    path = None
    in_hunk = False
    # Only "\n" ends a git diff line. splitlines() also breaks on \r, \x0b, \x0c, \x1c-\x1e, \x85
    # and the Unicode line and paragraph separators, so file content could forge a header line.
    for line in output.split("\n"):
        if line.startswith("diff --git "):
            path = None
            in_hunk = False
        elif line.startswith("+++ ") and not in_hunk:
            target = line[4:].rstrip("\t")
            if target == "/dev/null":
                path = ""  # deleted file: no added lines, so its hunks are valid but unrecorded
                continue
            if target.startswith('"'):
                if not target.endswith('"'):
                    raise Unknown("diff path invalid")
                encoded = target[1:-1].encode("utf-8")
                decoded = bytearray()
                escapes = {ord('a'): 7, ord('b'): 8, ord('f'): 12, ord('n'): 10,
                           ord('r'): 13, ord('t'): 9, ord('v'): 11,
                           ord('"'): 34, ord('\\'): 92}
                index = 0
                while index < len(encoded):
                    char = encoded[index]
                    if char != 92:
                        decoded.append(char)
                        index += 1
                    elif index + 1 < len(encoded) and encoded[index + 1] in escapes:
                        decoded.append(escapes[encoded[index + 1]])
                        index += 2
                    elif (index + 3 < len(encoded) and
                          all(48 <= digit <= 55 for digit in encoded[index + 1:index + 4])):
                        decoded.append(int(encoded[index + 1:index + 4], 8))
                        index += 4
                    else:
                        raise Unknown("diff path invalid")
                try:
                    target = decoded.decode("utf-8")
                except UnicodeDecodeError:
                    raise Unknown("diff path encoding invalid") from None
            if not target.startswith("b/") or len(target) == 2:
                raise Unknown("diff path invalid")
            path = target[2:]
            added.setdefault(path, [])
        elif line.startswith("@@"):
            match = re.match(r"@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@", line)
            if match is None or path is None:
                raise Unknown("diff hunk invalid")
            in_hunk = True
            if path:
                start, count = int(match[1]), int(match[2] or "1")
                added[path].extend(range(start, start + count))
        elif in_hunk and path and line.startswith("+") and AMBIGUOUS_BREAK.search(line, 1):
            suspect.add(path)  # a scanner may number this file's lines differently from git
    for name in suspect:
        added[name] = None
    for record in _git_text(["diff", *flags, "--numstat", "-z", base, merge, "--"]).split("\0"):
        if record.startswith("-\t-\t"):
            added[record[4:]] = None
    return added


def _local_reports(directory, artifacts, run_id, attempt, selected):
    runner_temp = os.environ.get("RUNNER_TEMP")
    if (not directory or not runner_temp or not Path(directory).is_dir() or Path(directory).is_symlink()
            or not Path(directory).resolve().is_relative_to(Path(runner_temp).resolve())):
        raise Unknown("SCAN_EVIDENCE_DIR invalid")
    expected = set()
    for key, (_, _, _, suffix) in TOOLS.items():
        if selected.get(key) and suffix:
            expected.add(f"scan-evidence-{run_id}-{attempt}-{suffix}")
    if any(not isinstance(a, dict) for a in artifacts):
        raise Unknown("artifact inventory ambiguous")
    relevant = [a for a in artifacts if isinstance(a.get("name"), str) and a["name"] in expected]
    available = {a["name"]: a for a in relevant}
    if len(available) != len(relevant):
        raise Unknown("artifact inventory ambiguous")
    reports = {}
    for name in expected:
        meta = available.get(name)
        if (not isinstance(meta, dict) or meta.get("expired") is not False or not _number(meta.get("id"))
                or meta.get("workflow_run", {}).get("id") != int(run_id)):
            raise Unknown("current-attempt artifact inventory incomplete")
        # The pinned download action extracts one pattern match directly into path.
        folder = Path(directory) if len(expected) == 1 else Path(directory) / name
        if folder.is_symlink() or not folder.is_dir():
            raise Unknown("downloaded artifact directory invalid")
        files = list(folder.iterdir())
        suffix = name.split(f"scan-evidence-{run_id}-{attempt}-", 1)[1]
        expected_file = f"scan-evidence-{suffix}.json"
        if len(files) != 1 or files[0].name != expected_file:
            raise Unknown("downloaded artifact contents invalid")
        report = _read_json(files[0])
        if (not isinstance(report, dict) or str(report.get("run_id")) != str(run_id)
                or str(report.get("run_attempt")) != str(attempt)):
            raise Unknown("current-attempt report mismatch")
        reports[name] = {"report": report, "artifact": meta}
    return reports


def _ancestor(comparison, ancestor, descendant):
    if (not isinstance(comparison, dict) or not _sha(ancestor) or not _sha(descendant)
            or not isinstance(comparison.get("base_commit"), dict)
            or not isinstance(comparison.get("merge_base_commit"), dict)
            or comparison["base_commit"].get("sha") != ancestor
            or comparison["merge_base_commit"].get("sha") != ancestor
            or comparison.get("behind_by") != 0):
        return False
    ahead, total = comparison.get("ahead_by"), comparison.get("total_commits")
    if not isinstance(ahead, int) or not isinstance(total, int) or ahead != total:
        return False
    return ((comparison.get("status") == "identical" and ahead == 0 and ancestor == descendant)
            or (comparison.get("status") == "ahead" and ahead > 0 and ancestor != descendant))


def _main_tip(ref):
    """The commit refs/heads/main points at, read by exact name from the git refs API."""
    obj = ref.get("object") if isinstance(ref, dict) else None
    if (not isinstance(obj, dict) or ref.get("ref") != "refs/heads/main"
            or obj.get("type") != "commit" or not _sha(obj.get("sha"))):
        raise Unknown("protected main ref invalid")
    return obj["sha"]


def _protected_provenance(run, pr, comparison, policy_sha, local_sha, main_sha,
                          policy_comparison=None):
    """Prove the wrapper and its pinned callee belong to protected-main ancestry.

    `comparison` sets the wrapper commit against `main_sha`, the live tip of main. The PR's own
    base is deliberately no anchor: it does not follow main between pushes, so it trails the
    wrapper commit on any PR that has not merged main since main moved.
    """
    if not all(isinstance(v, dict) for v in (run, pr, comparison)):
        return False
    if not _sha(policy_sha) or not _sha(local_sha):
        return False
    base = pr.get("base", {})
    head = pr.get("head", {})
    if (not isinstance(base, dict) or not isinstance(head, dict)
            or base.get("ref") != "main" or not isinstance(base.get("repo"), dict)
            or base["repo"].get("full_name") != REPO or not _sha(head.get("sha"))):
        return False
    workflow_path = ".github/workflows/required-pr-build.yml"
    required_url = f"https://api.github.com/repos/{REPO}/actions/required_workflows/{run.get('workflow_id')}"
    if (run.get("path") not in (workflow_path, f"{workflow_path}@main",
                                f"{workflow_path}@refs/heads/main",
                                f"{REPO}/{workflow_path}@main",
                                f"{REPO}/{workflow_path}@refs/heads/main")
            or run.get("event") != "pull_request" or run.get("head_sha") != head["sha"]
            or not isinstance(run.get("repository"), dict)
            or run["repository"].get("full_name") != REPO
            or not _number(run.get("workflow_id")) or run.get("workflow_url") != required_url):
        return False
    refs = run.get("referenced_workflows")
    pr_ref = f"refs/pull/{pr.get('number')}/merge"
    matches = [w for w in refs if isinstance(w, dict)
               and w.get("path") == f"{REPO}/.github/workflows/pr-ci.yml@{policy_sha}"
               and w.get("sha") == policy_sha and w.get("ref") in (None, "refs/heads/main", pr_ref)] if isinstance(refs, list) else []
    if len(matches) != 1:
        return False
    return (_ancestor(comparison, local_sha, main_sha)
            and (policy_sha == local_sha or _ancestor(policy_comparison, policy_sha, local_sha)))


def _report_ids(reports, selected, run_id, attempt):
    ids = {}
    expected = {}
    for key, (_, category, _, suffix) in TOOLS.items():
        if selected[key] and suffix:
            expected[category] = f"scan-evidence-{run_id}-{attempt}-{suffix}"
    for name in set(expected.values()):
        report = reports[name]["report"]
        if name.endswith("-semgrep"):
            if set(report) != {"run_id", "run_attempt", "uploads"} or not isinstance(report["uploads"], list):
                raise Unknown("Semgrep report schema invalid")
            uploads = report["uploads"]
        else:
            if set(report) != {"run_id", "run_attempt", "category", "sarif_id"}:
                raise Unknown("upload report schema invalid")
            uploads = [{"category": report["category"], "sarif_id": report["sarif_id"]}]
        for upload in uploads:
            if (not isinstance(upload, dict) or set(upload) != {"category", "sarif_id"}
                    or upload.get("category") not in expected or expected[upload["category"]] != name
                    or not isinstance(upload.get("sarif_id"), str) or SARIF_ID.fullmatch(upload["sarif_id"]) is None):
                raise Unknown("upload report category or ID invalid")
            if upload["category"] in ids or upload["sarif_id"] in ids.values():
                raise Unknown("upload report identity ambiguous")
            ids[upload["category"]] = upload["sarif_id"]
    if set(ids) != set(expected):
        raise Unknown("upload report selected categories incomplete")
    return ids


def _tool_name(analysis):
    tool = analysis.get("tool")
    return tool.get("name") if isinstance(tool, dict) else tool


def _category_matches(key, item, expected):
    if item.get("category") == expected:
        return True
    # Required workflows can report the parent category while SARIF keeps the requested ID.
    if (key != "osv" or item.get("category") != ".github/workflows"
            or item.get("analysis_key") != ".github/workflows/required-pr-build.yml:protected-pr-publish"):
        return False
    sarif = item.get("sarif")
    runs = sarif.get("runs") if isinstance(sarif, dict) else None
    if not isinstance(runs, list) or len(runs) != 1 or not isinstance(runs[0], dict):
        return False
    details = runs[0].get("automationDetails")
    return isinstance(details, dict) and details.get("id") == expected


def _latest_analyses(raw):
    latest = {}
    for item in raw:
        if not isinstance(item, dict) or not _number(item.get("id")):
            raise Unknown("analysis listing invalid")
        key = (_tool_name(item), item.get("category"))
        if key not in latest or item["id"] > latest[key]["id"]:
            latest[key] = item
    return [dict(item, tool=_tool_name(item)) for item in latest.values()]


def _instance_alert_numbers(analyses, alerts, main_alerts):
    """Fetch instances only for new, non-dismissed findings checked by the verdict."""
    pr_alerts = {a.get("number"): a for a in alerts if isinstance(a, dict)}
    baseline = {a.get("number"): a for a in main_alerts if isinstance(a, dict)}
    numbers = set()
    for analysis in analyses:
        if analysis.get("tool") == "ESLint":
            continue  # ESLint is selected evidence, but its findings are advisory.
        for sarif_run in analysis.get("sarif", {}).get("runs", []):
            for result in sarif_run.get("results", []):
                if not isinstance(result, dict) or not isinstance(result.get("properties"), dict):
                    continue
                number = result["properties"].get("github/alertNumber")
                alert = pr_alerts.get(number)
                old = baseline.get(number)
                if (_number(number) and isinstance(alert, dict)
                        and alert.get("state") != "dismissed"
                        and not (isinstance(old, dict) and old.get("state") in ("open", "dismissed"))):
                    numbers.add(number)
    return numbers


def _collect(api, selection, jobs, number, run_id):
    pr, _ = api.get(f"/pulls/{number}")
    if not isinstance(pr, dict) or pr.get("number") != number:
        raise Unknown("PR identity invalid")
    run, _ = api.get(f"/actions/runs/{run_id}")
    attempt = int(os.environ.get("GITHUB_RUN_ATTEMPT", "0"))
    if not _number(attempt) or run.get("id") != run_id or run.get("run_attempt") != attempt:
        raise Unknown("Actions attempt mismatch")
    attempt_started = _time(run.get("run_started_at"))
    local_sha = subprocess.run(["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"],
                               capture_output=True, text=True, timeout=10, check=True, env=GIT_ENV).stdout.strip()
    policy_sha = selection.get("policy_sha")
    if (not _sha(local_sha) or not _sha(policy_sha)
            or local_sha != os.environ.get("WORKFLOW_SHA")):
        raise Unknown("protected source invalid")
    # The result job runs from the wrapper commit; scan selection runs from its pinned callee.
    # GitHub resolves the wrapper from main when the run is created, so it was main's tip then
    # and is an ancestor of main's tip now. The PR's base is no part of that proof: it does not
    # follow main between pushes, and tying the proof to it fails every PR behind main.
    main_ref, _ = api.get("/git/ref/heads/main")
    main_sha = _main_tip(main_ref)
    comparison, _ = api.get(f"/compare/{local_sha}...{main_sha}")
    policy_comparison = None
    if policy_sha != local_sha:
        policy_comparison, _ = api.get(f"/compare/{policy_sha}...{local_sha}")
    provenance = _protected_provenance(run, pr, comparison, policy_sha,
                                       local_sha, main_sha, policy_comparison)
    artifacts = api.pages(f"/actions/runs/{run_id}/artifacts", "artifacts")
    selected = _selected(selection)
    refs = run.get("referenced_workflows")
    osv_source_verified = not selected.get("osv")
    if selected.get("osv") and isinstance(refs, list):
        osv_pin = _protected_osv_pin()
        osv_source_verified = sum(
            1 for item in refs if isinstance(item, dict)
            and item.get("path") == f"{REPO}/.github/workflows/osv-scanner.yml@{osv_pin}"
            and item.get("sha") == osv_pin
            and item.get("ref") in (None, "refs/heads/main", f"refs/pull/{number}/merge")) == 1
    reports = _local_reports(os.environ.get("SCAN_EVIDENCE_DIR"), artifacts, run_id, attempt, selected)
    for report in reports.values():
        if _time(report["artifact"].get("created_at")) <= attempt_started:
            raise Unknown("current-attempt artifact not after run start")
    report_ids = _report_ids(reports, selected, run_id, attempt)
    merge = selection.get("merge_sha")
    diff = _diff_lines(_base_parent(selection.get("head_sha"), merge), merge)
    ref = f"refs/pull/{number}/merge"
    all_analyses = api.pages(f"/code-scanning/analyses?{urlencode({'ref': ref})}")
    latest_items = _latest_analyses(all_analyses)
    normalized = []
    expected = [(key, value) for key, value in TOOLS.items() if selected.get(key)]
    for key, (tool, category, _, _) in expected:
        sid = report_ids[category]
        matches = api.pages(f"/code-scanning/analyses?{urlencode({'sarif_id': sid})}")
        if len(matches) != 1 or _tool_name(matches[0]) != tool:
            raise Unknown(f"{tool} selected analysis missing or ambiguous")
        item = dict(matches[0], tool=tool)
        if _time(item.get("created_at")) <= attempt_started:
            raise Unknown(f"{tool} selected analysis not after current attempt start")
        sid = item.get("sarif_id")
        if not isinstance(sid, str) or not sid:
            raise Unknown("analysis SARIF ID missing")
        if sid != report_ids[category]:
            raise Unknown("analysis upload ID mismatch")
        status, _ = api.get(f"/code-scanning/sarifs/{sid}")
        item["processing_status"] = status.get("processing_status")
        sarif, _ = api.get(f"/code-scanning/analyses/{item['id']}", accept="application/sarif+json")
        item["sarif"] = sarif
        if not _category_matches(key, item, category):
            raise Unknown(f"{tool} selected analysis missing or ambiguous")
        normalized.append(item)
    # Omission returns all alert states; `state=all` is not a valid REST enum.
    alerts = api.pages(f"/code-scanning/alerts?{urlencode({'ref': ref})}")
    main_alerts = api.pages(f"/code-scanning/alerts?{urlencode({'ref': 'refs/heads/main'})}")
    alert_numbers = _instance_alert_numbers(normalized, alerts, main_alerts)
    instances = {str(n): api.pages(f"/code-scanning/alerts/{n}/instances?{urlencode({'ref': ref})}")
                 for n in alert_numbers}
    coverage = []
    for component in COMPONENTS:
        name = component.replace("_", "-") + "-coverage"
        coverage.append({"component": component, "present": any(a.get("name") == name and a.get("expired") is False
                         and isinstance(a.get("size_in_bytes"), int) and a["size_in_bytes"] > 0 for a in artifacts)})
    sonar = None
    if selected.get("sonar"):
        checks = api.pages(f"/commits/{selection['head_sha']}/check-runs?app_id=12526&filter=all", "check_runs")
        # SonarCloud and the protected workflow start independently. A valid
        # App check may precede this workflow run; bind it to the current PR
        # head and base instead of imposing an ordering between the two.
        opened = _time(pr.get("created_at"))
        now = datetime.now(timezone.utc)
        sonar_checks = [c for c in checks if c.get("app", {}).get("id") == 12526
                        and c.get("name") == "SonarCloud Code Analysis"
                        and c.get("head_sha") == selection["head_sha"]
                        and opened <= _time(c.get("started_at")) <= now
                        and any(isinstance(link, dict) and link.get("number") == number
                                and link.get("head", {}).get("sha") == selection["head_sha"]
                                and link.get("base", {}).get("sha") == pr.get("base", {}).get("sha")
                                and link.get("base", {}).get("ref") == "main"
                                for link in c.get("pull_requests", []))]
        # Retries retain earlier App checks on the same PR revision.
        if sonar_checks:
            newest = max(_time(c["started_at"]) for c in sonar_checks)
            sonar_checks = [c for c in sonar_checks if _time(c["started_at"]) == newest]
        if len(sonar_checks) == 1:
            c = sonar_checks[0]
            completed = c.get("completed_at")
            in_window = (isinstance(completed, str)
                         and _time(c.get("started_at")) <= _time(completed) <= now)
            sonar = {"app_id": 12526, "conclusion": c.get("conclusion"), "status": c.get("status"),
                     "head_sha": c.get("head_sha"), "analysis_window_verified": in_window}
    policy_refs = [item.get("sha") for item in refs if isinstance(item, dict)
                   and item.get("path") == f"{REPO}/.github/workflows/pr-ci.yml@{policy_sha}"] if isinstance(refs, list) else []
    return pr, {"run": run, "source_policy_sha": policy_refs[0] if len(policy_refs) == 1 else None,
                "provenance_verified": provenance,
                "artifact_verified": True, "reports": reports, "analyses": normalized,
                "osv_source_verified": osv_source_verified,
                "latest_analyses": latest_items, "pagination_complete": True,
                "http_errors": [], "alerts": alerts, "main_alerts": main_alerts, "instances": instances,
                "diff": {"complete": True, "added_lines": diff}, "coverage_artifacts": coverage,
                "sonar": sonar}


def _pr_revision(pr):
    """Compare stable PR identity, not mutable nested repository metadata."""
    if not isinstance(pr, dict) or not _number(pr.get("number")) or not _sha(pr.get("merge_commit_sha")):
        raise Unknown("PR revision identity invalid")
    identity = [pr["number"], pr["merge_commit_sha"]]
    for side in ("head", "base"):
        value = pr.get(side)
        if (not isinstance(value, dict) or not _sha(value.get("sha"))
                or not isinstance(value.get("ref"), str) or not value["ref"]
                or not isinstance(value.get("repo"), dict)
                or not isinstance(value["repo"].get("full_name"), str)
                or not value["repo"]["full_name"]):
            raise Unknown("PR revision identity invalid")
        identity.extend((value["sha"], value["ref"], value["repo"]["full_name"]))
    return tuple(identity)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--selection", required=True, type=Path)
    parser.add_argument("--jobs", required=True, type=Path)
    parser.add_argument("--pr", required=True, type=int)
    parser.add_argument("--run-id", required=True, type=int)
    args = parser.parse_args()
    selection = None
    try:
        selection = _read_json(args.selection)
        jobs = _read_json(args.jobs)
        api = GitHub(os.environ.get("GITHUB_TOKEN"))
        current_pr, evidence = _collect(api, selection, jobs, args.pr, args.run_id)
        reasons = verify_verdict(selection, jobs, evidence, current_pr)
        if not reasons:
            final_pr, _ = api.get(f"/pulls/{args.pr}")
            if _pr_revision(final_pr) != _pr_revision(current_pr):
                reasons.append("unknown: PR revision moved during verification")
    except (Unknown, OSError, subprocess.SubprocessError, ValueError, TypeError, KeyError, AttributeError) as exc:
        reasons = [f"unknown: {exc}"]
    if (isinstance(selection, dict) and set(_selected(selection)) == ANALYSIS_KEYS
            and all(_sha(selection.get(k)) for k in ("policy_sha", "head_sha", "merge_sha"))):
        chosen = ", ".join(sorted(k for k, v in selection["selected"].items() if v)) or "none"
        skipped = ", ".join(sorted(k for k, v in selection["selected"].items() if not v)) or "none"
        summary = (f"policy={selection.get('policy_sha')} head={selection.get('head_sha')} "
                   f"merge={selection.get('merge_sha')} mode={selection.get('mode')} "
                   f"selected={chosen} skipped={skipped}")
        print(summary)
        summary_file = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary_file:
            try:
                with open(summary_file, "a", encoding="utf-8") as output:
                    output.write(summary + "\n")
            except OSError:
                reasons.append("unknown: step summary unavailable")
    for reason in reasons:
        print(reason)
    if not reasons:
        print("selected current-revision scan evidence verified")
        return 0
    return 2 if any(r.startswith("unknown:") or r.endswith(": selected analysis missing") for r in reasons) else 1


if __name__ == "__main__":
    sys.exit(main())
