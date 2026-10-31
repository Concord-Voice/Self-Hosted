#!/usr/bin/env python3
"""Prepare a Sonar PR scan from signed caller evidence and inert PR data."""

import argparse
import base64
from datetime import datetime, timezone
import io
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import time
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import HTTPRedirectHandler, Request, build_opener
import zipfile


REPO = "Concord-Voice/Self-Hosted"
API = f"https://api.github.com/repos/{REPO}"
ROOT = Path(__file__).resolve().parents[2]
GIT_REMOTE = f"https://github.com/{REPO}.git"
GIT_ENV = {k: v for k, v in os.environ.items()
           if not k.startswith("GIT_") and k not in ("GITHUB_TOKEN", "GH_TOKEN", "SONAR_TOKEN")}
SHA = re.compile(r"[0-9a-f]{40}\Z")
COMPONENTS = {
    # Self-Hosted: the `desktop` key is the packages/* lane (build.yml), whose
    # root vitest run writes coverage/lcov.info with packages/... sources.
    "desktop": ("desktop-coverage", "lcov.info", "coverage/lcov.info", "packages"),
    "control_plane": ("control-plane-coverage", "coverage.out", "services/control-plane/coverage.out", "services/control-plane"),
    "media_plane": ("media-plane-coverage", "lcov.info", "services/media-plane/coverage/lcov.info", "services/media-plane"),
    "admin": ("admin-coverage", "lcov.info", "client/admin/coverage/lcov.info", "client/admin"),
}
# Self-Hosted has no concord-bridge component; Alpha's bridge coverage stays there.
REPORT_PATHS = {spec[2] for spec in COMPONENTS.values()}
SOURCE_DIRS = (
    "packages/", "client/admin/src/", "client/admin/e2e/",
    "services/control-plane/", "services/media-plane/src/", "services/media-plane/tests/",
)
SELECTION_KEYS = {"codeql_actions", "codeql_go", "codeql_js", "eslint", "govulncheck", "osv",
                  "semgrep_custom", "semgrep_community", "semgrep_canary", "sonar",
                  "sonar_force_all", "sonar_desktop", "sonar_control_plane", "sonar_media_plane", "sonar_admin"}
SUPPORT = {"package.json", "packages/e2ee-client/tsconfig.json", "packages/e2ee-client/package.json",
           "packages/protocol/tsconfig.json", "packages/protocol/package.json",
           "client/admin/tsconfig.json", "client/admin/tsconfig.app.json",
           "client/admin/tsconfig.node.json", "client/admin/tsconfig.e2e.json",
           "client/admin/package.json", "services/media-plane/tsconfig.json",
           "services/media-plane/package.json", "services/control-plane/go.mod",
           "services/control-plane/go.sum"}
# GitHub reports a live run as any of these; a waiting downstream job can flip it off in_progress.
LIVE_RUN_STATUSES = ("queued", "in_progress", "requested", "waiting", "pending")
MAX_API = 2 * 1024 * 1024
MAX_PROOF_ZIP = 128 * 1024
MAX_PROOF = 64 * 1024
# shortcut: provisional bounds, replace with measured headroom after the live canary.
MAX_COVERAGE_ZIP = 64 * 1024 * 1024
MAX_COVERAGE = 128 * 1024 * 1024
MAX_SOURCE_FILES = 30000
MAX_SOURCE_FILE = 16 * 1024 * 1024
MAX_SOURCE_TOTAL = 512 * 1024 * 1024


class Reject(Exception):
    """Required provenance or data safety evidence is missing."""


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


OPEN = build_opener(NoRedirect()).open


def positive(value):
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def sha(value):
    return isinstance(value, str) and SHA.fullmatch(value) is not None


def timestamp(value):
    if not isinstance(value, str):
        raise Reject("time missing")
    try:
        date = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise Reject("time invalid") from None
    if date.tzinfo is None:
        raise Reject("time zone missing")
    return date.astimezone(timezone.utc)


def git(repo, *args, limit=MAX_API, env=GIT_ENV):
    try:
        result = subprocess.run(["git", "-C", str(repo), *args], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=180, check=True)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        raise Reject("Git evidence unavailable") from None
    if len(result.stdout) > limit:
        raise Reject("Git output bound exceeded")
    return result.stdout


def safe_path(raw):
    try:
        path = raw.decode("utf-8", "strict") if isinstance(raw, bytes) else raw
    except UnicodeDecodeError:
        raise Reject("Git path encoding invalid") from None
    if (not isinstance(path, str) or not path or len(path.encode()) > 4096 or path.startswith("/")
            or "\\" in path or any(part in ("", ".", "..") for part in path.split("/"))
            or any(part.casefold() == ".git" for part in path.split("/"))
            or any(ord(c) < 32 or ord(c) == 127 for c in path)):
        raise Reject("unsafe path")
    return path


def source_path(path):
    if (path in SUPPORT or path in REPORT_PATHS
            or path == "sonar-project.properties" or path.endswith("/sonar-project.properties")):
        return False
    if path.startswith("services/control-plane/migrations/"):
        return False
    if path.endswith((".woff2", ".otf")) or "/data/emoji/" in path:
        return False
    if any(path.startswith(prefix) for prefix in SOURCE_DIRS):
        return True
    return False


def one_zip(data, filename, expanded_limit):
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            entries = archive.infolist()
            if len(entries) != 1:
                raise Reject("artifact ZIP entry count invalid")
            entry = entries[0]
            if (safe_path(entry.filename) != filename or entry.is_dir()
                    or entry.flag_bits & 1 or entry.compress_type not in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)
                    or entry.file_size <= 0 or entry.file_size > expanded_limit
                    or entry.compress_size <= 0 or entry.compress_size > len(data)):
                raise Reject("artifact ZIP metadata invalid")
            mode = entry.external_attr >> 16
            if mode and stat.S_IFMT(mode) not in (0, stat.S_IFREG):
                raise Reject("artifact ZIP entry is not regular")
            chunks, size = [], 0
            with archive.open(entry) as stream:
                while chunk := stream.read(1024 * 1024):
                    size += len(chunk)
                    if size > expanded_limit:
                        raise Reject("artifact expansion bound exceeded")
                    chunks.append(chunk)
            if size != entry.file_size:
                raise Reject("artifact ZIP size mismatch")
            return b"".join(chunks)
    except (zipfile.BadZipFile, EOFError, OSError, ValueError):
        raise Reject("artifact ZIP invalid") from None


class GitHub:
    def __init__(self, token):
        if not token:
            raise Reject("GitHub token missing")
        self.token = token
        self.count = 0

    def request(self, path, limit=MAX_API, binary=False):
        if not path.startswith("/") or ".." in path.split("/"):
            raise Reject("API path invalid")
        self.count += 1
        if self.count > 30:
            raise Reject("API request bound exceeded")
        url = API + path
        request = Request(url, headers={"Accept": "application/vnd.github+json",
                                        "Authorization": f"Bearer {self.token}",
                                        # 2026-03-10 returns merge_commit_sha as null, which fails the
                                        # signed merge SHA check on every PR.
                                        "X-GitHub-Api-Version": "2022-11-28",
                                        "User-Agent": "Concord-Voice-protected-sonar"})
        try:
            with OPEN(request, timeout=20) as response:
                if response.url != url:
                    raise Reject("API redirected")
                result = response.read(limit + 1)
        except HTTPError as exc:
            if not binary or exc.code not in (302, 303, 307):
                raise Reject(f"GitHub API HTTP {exc.code}") from None
            location = exc.headers.get("Location", "")
            host = urlparse(location)
            if (host.scheme != "https" or not host.hostname
                    or not (host.hostname.endswith(".actions.githubusercontent.com")
                            or host.hostname.endswith(".blob.core.windows.net"))):
                raise Reject("artifact redirect host invalid") from None
            try:
                with OPEN(Request(location, headers={"User-Agent": "Concord-Voice-protected-sonar"}), timeout=30) as response:
                    if response.url != location:
                        raise Reject("artifact redirected again")
                    result = response.read(limit + 1)
            except (HTTPError, OSError):
                raise Reject("artifact download failed") from None
        except OSError:
            raise Reject("GitHub API unavailable") from None
        if len(result) > limit:
            raise Reject("API byte bound exceeded")
        if binary:
            return result
        try:
            return json.loads(result)
        except (ValueError, RecursionError):
            raise Reject("API JSON invalid") from None

    def artifacts(self, run_id):
        found = []
        for page in range(1, 6):
            body = self.request(f"/actions/runs/{run_id}/artifacts?per_page=100&page={page}")
            if not isinstance(body, dict) or not isinstance(body.get("artifacts"), list):
                raise Reject("artifact inventory malformed")
            found.extend(body["artifacts"])
            if len(found) > 500:
                raise Reject("artifact inventory bound exceeded")
            if len(body["artifacts"]) < 100:
                if body.get("total_count") != len(found):
                    raise Reject("artifact inventory incomplete")
                return found
        raise Reject("artifact inventory pagination incomplete")

    def artifact(self, artifacts, name, run_id, earliest, max_zip):
        matches = [a for a in artifacts if isinstance(a, dict) and a.get("name") == name
                   and timestamp(a.get("created_at")) >= earliest]
        if len(matches) != 1:
            raise Reject("artifact missing or duplicate")
        meta = matches[0]
        if (not positive(meta.get("id")) or meta.get("expired") is not False
                or meta.get("workflow_run", {}).get("id") != run_id
                or not positive(meta.get("size_in_bytes")) or meta["size_in_bytes"] > max_zip
                or timestamp(meta.get("expires_at")) <= datetime.now(timezone.utc)):
            raise Reject("artifact provenance invalid")
        return self.request(f"/actions/artifacts/{meta['id']}/zip", max_zip, binary=True)


def verify_proof(proof, verified):
    try:
        subprocess.run(["go", "run", "./cmd/verify-sonar-caller-proof",
                        "--proof", str(proof), "--out", str(verified)], cwd=ROOT / "services/control-plane", env=GIT_ENV,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120, check=True)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        raise Reject("signed caller proof invalid") from None
    if verified.is_symlink() or not verified.is_file() or verified.stat().st_size > MAX_PROOF:
        raise Reject("verified proof output invalid")
    try:
        return json.loads(verified.read_bytes())
    except (ValueError, RecursionError):
        raise Reject("verified proof JSON invalid") from None


def validate_evidence(candidate, verified, run, pr, jobs):
    if not isinstance(verified, dict) or set(verified) != {"payload", "claims"}:
        raise Reject("verified proof shape invalid")
    payload, claims = verified["payload"], verified["claims"]
    if not isinstance(payload, dict) or not isinstance(claims, dict):
        raise Reject("verified proof malformed")
    for key in ("caller_run_id", "caller_run_attempt", "pr_number", "expected_head_sha", "expected_base_sha"):
        if payload.get(key) != candidate[key]:
            raise Reject("candidate differs from signed proof")
    if (claims.get("run_id") != str(candidate["caller_run_id"])
            or claims.get("run_attempt") != str(candidate["caller_run_attempt"])
            or claims.get("repository") != REPO or claims.get("event_name") != "pull_request"
            or claims.get("workflow_ref") != REPO + "/.github/workflows/required-pr-build.yml@main"
            or not sha(claims.get("workflow_sha"))):
        raise Reject("verified claims inconsistent")
    workflow_path = ".github/workflows/required-pr-build.yml"
    protected_paths = (workflow_path, f"{workflow_path}@main", f"{workflow_path}@refs/heads/main",
                       f"{REPO}/{workflow_path}@main", f"{REPO}/{workflow_path}@refs/heads/main")
    if not isinstance(run, dict):
        raise Reject("caller run identity or state invalid: run")
    repository = run.get("repository") if isinstance(run.get("repository"), dict) else {}
    pulls = run.get("pull_requests") if isinstance(run.get("pull_requests"), list) else []
    # Field names only, never values, so a live rejection says which check failed.
    failed = [name for name, ok in (
        ("id", run.get("id") == candidate["caller_run_id"]),
        ("run_attempt", run.get("run_attempt") == candidate["caller_run_attempt"]),
        ("event", run.get("event") == "pull_request"),
        ("head_sha", run.get("head_sha") == candidate["expected_head_sha"]),
        ("status", run.get("status") in LIVE_RUN_STATUSES),
        ("conclusion", run.get("conclusion") is None),
        ("path", run.get("path") in protected_paths),
        ("repository", repository.get("full_name") == REPO),
        ("repository_id", str(repository.get("id")) == claims.get("repository_id")),
        ("pull_requests", candidate["pr_number"] in [p.get("number") for p in pulls if isinstance(p, dict)]),
    ) if not ok]
    if failed:
        raise Reject("caller run identity or state invalid: " + ",".join(failed))
    if (not isinstance(pr, dict) or pr.get("number") != candidate["pr_number"]
            or pr.get("state") != "open" or pr.get("head", {}).get("sha") != candidate["expected_head_sha"]
            or pr.get("base", {}).get("sha") != candidate["expected_base_sha"]
            or pr.get("base", {}).get("ref") != "main"):
        raise Reject("current PR revision moved")
    selection = payload.get("selection")
    verdicts, coverage = payload.get("jobs"), payload.get("coverage")
    if (not isinstance(selection, dict) or selection.get("version") != 1 or selection.get("known") is not True
            or selection.get("mode") not in ("required-full", "required-relevant")
            or any(selection.get(k) != candidate[v] for k, v in (("head_sha", "expected_head_sha"),
                                                                   ("base_sha", "expected_base_sha")))
            or selection.get("merge_sha") != pr.get("merge_commit_sha")
            or not sha(selection.get("policy_sha")) or not sha(selection.get("merge_sha"))
            or not isinstance(selection.get("selected"), dict)
            or set(selection["selected"]) != SELECTION_KEYS
            or not all(isinstance(v, bool) for v in selection["selected"].values())
            or selection["selected"].get("sonar") is not True
            or not isinstance(selection.get("coverage_required"), dict)
            or set(selection["coverage_required"]) != set(COMPONENTS)
            or not all(isinstance(v, bool) for v in selection["coverage_required"].values())
            or not isinstance(verdicts, dict) or set(verdicts) != {"ioc", "build", "semgrep", "eslint", "govulncheck"}
            or payload.get("pr_ci_result") != "success" or verdicts["build"] != "success"
            or verdicts["ioc"] != "success" or not isinstance(coverage, dict)
            or any(v not in ("success", "skipped") for v in verdicts.values())
            or set(coverage) != set(COMPONENTS) or not all(isinstance(v, bool) for v in coverage.values())):
        raise Reject("signed scan selection or verdict invalid")
    if any(selection["coverage_required"][c] and not coverage[c] for c in COMPONENTS):
        raise Reject("selected coverage was not uploaded")
    selected = selection["selected"]
    if ((selected["eslint"] and verdicts["eslint"] != "success")
            or (selected["govulncheck"] and verdicts["govulncheck"] != "success")
            or (any(selected[k] for k in ("semgrep_custom", "semgrep_community", "semgrep_canary"))
                and verdicts["semgrep"] != "success")):
        raise Reject("selected scan job did not succeed")
    if (not isinstance(jobs, dict) or not isinstance(jobs.get("jobs"), list)
            or jobs.get("total_count") != len(jobs["jobs"]) or len(jobs["jobs"]) > 100):
        raise Reject("caller job inventory malformed")
    actual = jobs["jobs"]
    if any(not isinstance(j, dict) or j.get("run_id") != candidate["caller_run_id"]
           or j.get("run_attempt") != candidate["caller_run_attempt"]
           or j.get("head_sha") != candidate["expected_head_sha"] for j in actual):
        raise Reject("caller job identity mismatch")
    prefix = JOB_PREFIX
    protected = [j for j in actual if isinstance(j.get("name"), str) and j["name"].startswith(prefix)]
    if not protected or any(j.get("status") != "completed" for j in protected):
        raise Reject("protected caller jobs incomplete")
    # The report job is complete before the protected caller signs its outputs.
    reports = [j for j in protected if j["name"] == prefix + "report"]
    if len(reports) != 1 or reports[0].get("conclusion") != "success":
        raise Reject("caller report job not successful")
    expected = {"ioc": "ioc / Supply-chain IOC scan", "semgrep": "semgrep / Semgrep SAST",
                "eslint": "eslint / ESLint SARIF", "govulncheck": "govulncheck / govulncheck (control-plane)"}
    for key, suffix in expected.items():
        matches = [j for j in protected if j["name"] == prefix + suffix]
        if verdicts[key] == "success" and (len(matches) != 1 or matches[0].get("conclusion") != "success"):
            raise Reject("signed job verdict differs from API")
        if verdicts[key] == "skipped" and any(j.get("conclusion") != "skipped" for j in matches):
            raise Reject("signed job verdict differs from API")
    build = [j for j in protected if j["name"].startswith(prefix + "build / ")]
    if not build or any(j.get("conclusion") not in ("success", "skipped") for j in build):
        raise Reject("build jobs differ from signed success")
    producers = {"desktop": "Desktop / Merge Coverage", "control_plane": "Control Plane / Merge Coverage",
                 "media_plane": "Media Plane / Test + Coverage", "admin": "Admin Portal / Test + Coverage"}
    for component, producer in producers.items():
        if not coverage[component]:
            continue
        matches = [j for j in build if j["name"] == prefix + "build / " + producer]
        if len(matches) != 1 or matches[0].get("conclusion") != "success":
            raise Reject("coverage producer job not successful")
    return payload, claims


def prepare_git(repo, claims, payload, candidate, token):
    subprocess.run(["git", "init", "-q", str(repo)], env=GIT_ENV, stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, timeout=30, check=True)
    git(repo, "remote", "add", "origin", GIT_REMOTE)
    basic = base64.b64encode(f"x-access-token:{token}".encode()).decode()
    fetch_env = {**GIT_ENV, "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_COUNT": "1",
                 "GIT_CONFIG_KEY_0": "http.https://github.com/.extraheader",
                 "GIT_CONFIG_VALUE_0": "AUTHORIZATION: basic " + basic}
    git(repo, "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main",
        limit=1024, env=fetch_env)
    main = git(repo, "rev-parse", "refs/remotes/origin/main").decode().strip()
    if not sha(main):
        raise Reject("main revision invalid")
    lineage = git(repo, "rev-list", "--first-parent", "refs/remotes/origin/main", limit=20 * 1024 * 1024)
    ancestors = set(lineage.decode().splitlines())
    local_main = git(ROOT, "rev-parse", "HEAD").decode().strip()
    if (claims["workflow_sha"] not in ancestors or payload["selection"]["policy_sha"] not in ancestors
            or candidate["expected_base_sha"] not in ancestors or local_main not in ancestors):
        raise Reject("protected workflow or policy is not on main first-parent history")
    git(repo, "fetch", "--no-tags", "origin", f"+refs/pull/{candidate['pr_number']}/head:refs/remotes/origin/pr",
        limit=1024, env=fetch_env)
    head = git(repo, "rev-parse", "refs/remotes/origin/pr").decode().strip()
    if head != candidate["expected_head_sha"]:
        raise Reject("fetched PR revision changed")
    git(repo, "update-ref", "refs/heads/analysis", head)
    git(repo, "symbolic-ref", "HEAD", "refs/heads/analysis")
    git(repo, "read-tree", "HEAD")


def materialize(repo):
    tree = git(repo, "ls-tree", "-r", "-z", "--full-tree", "HEAD", limit=32 * 1024 * 1024)
    selected, seen, total = [], set(), 0
    for record in tree.split(b"\0"):
        if not record:
            continue
        try:
            header, raw_path = record.split(b"\t", 1)
            mode, kind, oid = header.decode("ascii").split(" ")
        except (ValueError, UnicodeDecodeError):
            raise Reject("Git tree malformed") from None
        path = safe_path(raw_path)
        if any(
                prefix.rstrip("/").startswith(path + "/") or path == prefix.rstrip("/")
                for prefix in SOURCE_DIRS):
            raise Reject("Sonar source directory replaced")
        if mode not in ("100644", "100755") and any(path.startswith(prefix) for prefix in SOURCE_DIRS):
            raise Reject("non-regular Git source entry")
        if path in SUPPORT and (mode not in ("100644", "100755") or kind != "blob"):
            raise Reject("non-regular analyzer support entry")
        if not source_path(path):
            continue
        folded = path.casefold()
        if folded in seen:
            raise Reject("duplicate source path")
        seen.add(folded)
        if mode not in ("100644", "100755") or kind != "blob" or not sha(oid):
            raise Reject("non-regular Git source entry")
        size = int(git(repo, "cat-file", "-s", oid, limit=100).decode())
        total += size
        if size > MAX_SOURCE_FILE or total > MAX_SOURCE_TOTAL or len(selected) >= MAX_SOURCE_FILES:
            raise Reject("source size or count bound exceeded")
        selected.append((path, oid, size))
    if not selected:
        raise Reject("no source files selected")
    for path, oid, size in selected:
        destination = repo / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        content = git(repo, "cat-file", "blob", oid, limit=MAX_SOURCE_FILE)
        if len(content) != size:
            raise Reject("Git blob size mismatch")
        destination.write_bytes(content)
        destination.chmod(0o644)


def trusted_support(repo):
    # shortcut: main's analyzer configs can lag a PR, so live canaries must confirm SCM and coverage mapping.
    for path in SUPPORT:
        source = ROOT / path
        if source.is_symlink() or not source.is_file() or source.stat().st_size > MAX_SOURCE_FILE:
            raise Reject("trusted analyzer support unavailable")
        destination = repo / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(source.read_bytes())
        destination.chmod(0o644)


def validate_report(content, component):
    try:
        lines = content.decode("utf-8").splitlines(keepends=True)
    except UnicodeDecodeError:
        raise Reject("coverage encoding invalid") from None
    prefix = COMPONENTS[component][3]
    if component == "control_plane":
        if not lines or not lines[0].startswith("mode: "):
            raise Reject("Go coverage header missing")
        records = [line.split(":", 1)[0] for line in lines[1:] if line.strip()]
        if not records:
            raise Reject("Go coverage empty")
        for path in records:
            safe_path(path)
            if not (path.startswith(prefix + "/") or path.startswith("github.com/Concord-Voice/Concord-Voice-Alpha/" + prefix + "/")):
                raise Reject("Go coverage source outside component")
        return content
    found = 0
    output = []
    for line in lines:
        if not line.startswith("SF:"):
            output.append(line)
            continue
        raw = line[3:].rstrip("\r\n")
        if raw.startswith("/"):
            marker = "/" + prefix + "/"
            if marker not in raw:
                raise Reject("LCOV source outside component")
            path = prefix + "/" + raw.rsplit(marker, 1)[1]
        elif raw.startswith(prefix + "/"):
            path = raw
        else:
            path = prefix + "/" + raw
        safe_path(path)
        if not path.startswith(prefix + "/"):
            raise Reject("LCOV source outside component")
        output.append("SF:" + path + line[len(line.rstrip("\r\n")):])
        found += 1
    if not found:
        raise Reject("LCOV source records missing")
    return "".join(output).encode()


def property_value(value):
    if not isinstance(value, str) or len(value) > 256:
        raise Reject("PR metadata invalid")
    result = []
    for ch in value:
        code = ord(ch)
        if code < 32 or code == 127:
            raise Reject("PR branch contains control characters")
        if code > 0xFFFF:
            code -= 0x10000
            result.extend((f"\\u{0xD800 + (code >> 10):04x}", f"\\u{0xDC00 + (code & 0x3FF):04x}"))
        elif code > 126:
            result.append(f"\\u{code:04x}")
        elif ch in "\\=:# !":
            result.append("\\" + ch)
        else:
            result.append(ch)
    return "".join(result)


def trusted_properties(repo, pr, candidate):
    source = ROOT / "sonar-project.properties"
    if source.is_symlink() or not source.is_file() or source.stat().st_size > MAX_SOURCE_FILE:
        raise Reject("trusted Sonar properties unavailable")
    text = source.read_text(encoding="utf-8")
    if any(line.lstrip().startswith(("sonar.pullrequest.", "sonar.scm.revision=")) for line in text.splitlines()):
        raise Reject("trusted properties unexpectedly override PR metadata")
    branch = pr.get("head", {}).get("ref")
    values = (f"sonar.pullrequest.key={candidate['pr_number']}",
              f"sonar.pullrequest.branch={property_value(branch)}",
              "sonar.pullrequest.base=main",
              f"sonar.scm.revision={candidate['expected_head_sha']}")
    (repo / "sonar-project.properties").write_text(text.rstrip() + "\n" + "\n".join(values) + "\n", encoding="utf-8")


JOB_PREFIX = "Protected scan enforcement / "
JOB_SETTLE_ATTEMPTS, JOB_SETTLE_DELAY = 13, 10


def settled_jobs(api, path):
    # The jobs API trails the caller's report job by seconds, and longer for the jobs a
    # re-run copies forward, so a child dispatched at once can see unfinished jobs.
    # ponytail: fixed 2-minute poll; validate_evidence still rejects whatever stays unsettled.
    for remaining in range(JOB_SETTLE_ATTEMPTS - 1, -1, -1):
        jobs = api.request(path)
        listed = jobs.get("jobs") if isinstance(jobs, dict) else None
        if not isinstance(listed, list):
            return jobs
        protected = [j for j in listed if isinstance(j, dict) and isinstance(j.get("name"), str)
                     and j["name"].startswith(JOB_PREFIX)]
        if protected and all(j.get("status") == "completed" and j.get("conclusion") for j in protected):
            return jobs
        if remaining:
            time.sleep(JOB_SETTLE_DELAY)
    return jobs


def prepare(candidate, directory, api):
    if directory.exists() or not directory.parent.is_dir():
        raise Reject("analysis directory must be absent with an existing parent")
    run_id, attempt = candidate["caller_run_id"], candidate["caller_run_attempt"]
    run = api.request(f"/actions/runs/{run_id}/attempts/{attempt}")
    if not isinstance(run, dict) or not isinstance(run.get("run_started_at"), str):
        raise Reject("caller attempt unavailable")
    started = timestamp(run["run_started_at"])
    inventory = api.artifacts(run_id)
    name = f"protected-sonar-caller-proof-{run_id}-{attempt}"
    proof_zip = api.artifact(inventory, name, run_id, started, MAX_PROOF_ZIP)
    proof_bytes = one_zip(proof_zip, "proof.json", MAX_PROOF)
    with tempfile.TemporaryDirectory(prefix="sonar-proof-", dir=directory.parent) as temporary:
        temp = Path(temporary)
        proof = temp / "proof.json"
        verified_file = temp / "verified.json"
        proof.write_bytes(proof_bytes)
        verified = verify_proof(proof, verified_file)
        pr = api.request(f"/pulls/{candidate['pr_number']}")
        jobs = settled_jobs(api, f"/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100")
        payload, claims = validate_evidence(candidate, verified, run, pr, jobs)
        repo = temp / "analysis"
        prepare_git(repo, claims, payload, candidate, api.token)
        materialize(repo)
        trusted_support(repo)
        for component, (artifact_name, report_name, report_path, _) in COMPONENTS.items():
            if not payload["coverage"][component]:
                continue
            archive = api.artifact(inventory, artifact_name, run_id, started, MAX_COVERAGE_ZIP)
            report = validate_report(one_zip(archive, report_name, MAX_COVERAGE), component)
            destination = repo / report_path
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(report)
            destination.chmod(0o644)
        trusted_properties(repo, pr, candidate)
        # A PR move during download invalidates even a completely prepared tree.
        current = api.request(f"/pulls/{candidate['pr_number']}")
        final_run = api.request(f"/actions/runs/{run_id}/attempts/{attempt}")
        if (current.get("state") != "open"
                or current.get("head", {}).get("sha") != candidate["expected_head_sha"]
                or current.get("head", {}).get("ref") != pr["head"]["ref"]
                or current.get("base", {}).get("sha") != candidate["expected_base_sha"]
                or current.get("merge_commit_sha") != payload["selection"]["merge_sha"]
                or final_run.get("status") not in LIVE_RUN_STATUSES or final_run.get("run_attempt") != attempt):
            raise Reject("PR moved during preparation")
        os.replace(repo, directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--caller-run-id", type=int, required=True)
    parser.add_argument("--caller-run-attempt", type=int, required=True)
    parser.add_argument("--pr-number", type=int, required=True)
    parser.add_argument("--head-sha", required=True)
    parser.add_argument("--base-sha", required=True)
    parser.add_argument("--analysis-dir", type=Path, required=True)
    args = parser.parse_args()
    candidate = {"caller_run_id": args.caller_run_id, "caller_run_attempt": args.caller_run_attempt,
                 "pr_number": args.pr_number, "expected_head_sha": args.head_sha,
                 "expected_base_sha": args.base_sha}
    if not all(positive(candidate[k]) for k in ("caller_run_id", "caller_run_attempt", "pr_number")) or not all(
            sha(candidate[k]) for k in ("expected_head_sha", "expected_base_sha")):
        parser.error("candidate identifiers invalid")
    try:
        prepare(candidate, args.analysis_dir.resolve(), GitHub(os.environ.get("GITHUB_TOKEN", "")))
    except Reject as exc:
        parser.exit(1, f"protected Sonar preparation rejected: {exc}\n")


if __name__ == "__main__":
    main()
