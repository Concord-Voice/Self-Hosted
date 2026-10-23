#!/usr/bin/env python3
"""Require a fresh SonarCloud App check for one protected PR analysis."""

import argparse
from datetime import datetime, timezone
from http.client import HTTPException
import json
import os
import re
import sys
import time
from urllib.error import HTTPError
from urllib.request import HTTPRedirectHandler, Request, build_opener


REPO = "Concord-Voice/Concord-Voice-Alpha"
API = f"https://api.github.com/repos/{REPO}"
APP_ID = 12526
CHECK_NAME = "SonarCloud Code Analysis"
SHA = re.compile(r"[0-9a-f]{40}\Z")
MAX_BYTES = 256 * 1024
MAX_CHECKS = 1000


class Reject(Exception):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


OPEN = build_opener(NoRedirect()).open


def timestamp(value):
    if not isinstance(value, str):
        raise Reject("check time missing")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise Reject("check time invalid") from None
    if parsed.tzinfo is None:
        raise Reject("check time lacks timezone")
    return parsed.astimezone(timezone.utc)


class GitHub:
    def __init__(self, token, base=API):
        if not token:
            raise Reject("GitHub token missing")
        self.token = token
        self.base = base

    def get(self, path, timeout=10):
        if not path.startswith("/") or ".." in path.split("/"):
            raise Reject("API path invalid")
        url = self.base + path
        request = Request(url, headers={"Accept": "application/vnd.github+json",
                                        "Authorization": f"Bearer {self.token}",
                                        "X-GitHub-Api-Version": "2026-03-10",
                                        "User-Agent": "Concord-Voice-protected-sonar-verdict"})
        try:
            with OPEN(request, timeout=timeout) as response:
                if response.url != url:
                    raise Reject("GitHub API redirected")
                data = response.read(MAX_BYTES + 1)
        except HTTPError as exc:
            code = exc.code
            exc.close()
            raise Reject(f"GitHub API HTTP {code}") from None
        except (OSError, HTTPException):
            raise Reject("GitHub API unavailable") from None
        if len(data) > MAX_BYTES:
            raise Reject("GitHub API response too large")
        try:
            return json.loads(data)
        except (ValueError, UnicodeError, RecursionError):
            raise Reject("GitHub API response invalid") from None

    def checks(self, head_sha, timeout):
        checks = []
        for page in range(1, 12):
            body = self.get(f"/commits/{head_sha}/check-runs?app_id={APP_ID}&filter=all&per_page=100&page={page}", timeout)
            if (not isinstance(body, dict) or type(body.get("total_count")) is not int
                    or not 0 <= body["total_count"] <= MAX_CHECKS
                    or not isinstance(body.get("check_runs"), list)):
                raise Reject("check inventory invalid")
            items = body["check_runs"]
            if len(items) > 100 or any(not isinstance(item, dict) for item in items):
                raise Reject("check page invalid")
            checks.extend(items)
            if len(checks) > MAX_CHECKS or len(checks) > body["total_count"]:
                raise Reject("check inventory bound exceeded")
            if len(checks) == body["total_count"]:
                return checks
            if len(items) != 100:
                raise Reject("check inventory incomplete")
        raise Reject("check pagination incomplete")


def current_pr(api, pr_number, head_sha, base_sha, base_ref, timeout):
    pr = api.get(f"/pulls/{pr_number}", timeout)
    if (not isinstance(pr, dict) or pr.get("number") != pr_number or pr.get("state") != "open"
            or not isinstance(pr.get("head"), dict) or pr["head"].get("sha") != head_sha
            or not isinstance(pr.get("base"), dict) or pr["base"].get("sha") != base_sha
            or pr["base"].get("ref") != base_ref):
        raise Reject("current PR revision moved or invalid")


def eligible_checks(checks, pr_number, head_sha, base_sha, base_ref, started_at):
    eligible = []
    seen = set()
    now = datetime.now(timezone.utc)
    for check in checks:
        app = check.get("app")
        if (not isinstance(app, dict) or app.get("id") != APP_ID or check.get("name") != CHECK_NAME
                or check.get("head_sha") != head_sha):
            continue
        links = check.get("pull_requests")
        if not isinstance(links, list):
            raise Reject("Sonar check PR links invalid")
        if not any(isinstance(link, dict) and link.get("number") == pr_number
                   and isinstance(link.get("head"), dict) and link["head"].get("sha") == head_sha
                   and isinstance(link.get("base"), dict) and link["base"].get("sha") == base_sha
                   and link["base"].get("ref") == base_ref for link in links):
            continue
        # shortcut: start time cannot prove scan origin when external_id is empty;
        # bind a Sonar analysis ID before concurrent same-head scans are allowed.
        started = (None if check.get("status") == "queued" and check.get("started_at") is None
                   else timestamp(check.get("started_at")))
        if started is not None:
            if started < started_at:
                continue
            if started > now:
                raise Reject("Sonar check time invalid")
        identity = check.get("id")
        if type(identity) is not int or identity <= 0 or identity in seen:
            raise Reject("Sonar check identity ambiguous")
        seen.add(identity)
        eligible.append(check)
    if len(eligible) > 1:
        raise Reject("multiple fresh Sonar checks")
    return eligible[0] if eligible else None


def verify(api, pr_number, head_sha, base_sha, base_ref, started_at, timeout_seconds, poll_seconds=5):
    if (type(pr_number) is not int or pr_number <= 0 or not SHA.fullmatch(head_sha)
            or not SHA.fullmatch(base_sha) or base_ref != "main"):
        raise Reject("candidate PR identity invalid")
    start = timestamp(started_at)
    if start > datetime.now(timezone.utc):
        raise Reject("analysis window in future")
    deadline = time.monotonic() + timeout_seconds
    while True:
        remaining = max(0.1, min(10, deadline - time.monotonic()))
        current_pr(api, pr_number, head_sha, base_sha, base_ref, remaining)
        check = eligible_checks(api.checks(head_sha, remaining), pr_number, head_sha, base_sha, base_ref, start)
        if check is not None:
            if check.get("status") == "completed":
                completed = timestamp(check.get("completed_at"))
                if not timestamp(check["started_at"]) <= completed <= datetime.now(timezone.utc):
                    raise Reject("Sonar check completion time invalid")
                if check.get("conclusion") != "success":
                    raise Reject("Sonar check did not succeed")
                current_pr(api, pr_number, head_sha, base_sha, base_ref, remaining)
                return
            if check.get("status") not in ("queued", "in_progress"):
                raise Reject("Sonar check status invalid")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise Reject("fresh Sonar check timed out")
        time.sleep(min(poll_seconds, remaining))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pr", type=int, required=True)
    parser.add_argument("--head-sha", required=True)
    parser.add_argument("--base-sha", required=True)
    parser.add_argument("--base-ref", required=True)
    parser.add_argument("--started-at", required=True)
    parser.add_argument("--timeout-seconds", type=int, default=180)
    args = parser.parse_args(argv)
    try:
        if os.environ.get("GITHUB_REPOSITORY") != REPO or not 1 <= args.timeout_seconds <= 600:
            raise Reject("repository or timeout invalid")
        verify(GitHub(os.environ.get("GITHUB_TOKEN")), args.pr, args.head_sha,
               args.base_sha, args.base_ref, args.started_at, args.timeout_seconds)
    except Reject as exc:
        print(f"Protected Sonar result rejected: {exc}", file=sys.stderr)
        return 1
    print("Protected Sonar App check verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
