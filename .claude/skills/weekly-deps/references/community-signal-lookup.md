# Community Signal Lookup

Read this file when Phase 3.4 of `weekly-deps` needs to check whether a bumped package has known issues — security advisories, regression reports, or yanked versions — before deciding the PR's confidence label. The goal is a quick, programmatic check that catches the supply-chain class of incidents (malicious publish, accidental destructive release, upstream-maintainer-pulled package) that surface in the ~24h after a bot opens the PR.

## What this check does (and doesn't do)

It checks three sources:

1. **GitHub Advisory Database** — consolidates npm audit, Go vuln, GitHub Actions security advisories, and CVE feeds. Authoritative for known security issues with affected version ranges.
2. **Release-notes keyword scan** — reads the dependabot-supplied release notes already in the PR body, greps for regression / yank / hotfix markers. Catches self-reported issues from the upstream maintainer.
3. **Package repo issue spike (optional)** — counts recent issues opened against the package's GitHub repo since the version was published, filtered to ones matching crash/regression keywords. Only run for security-sensitive packages (per `security-sensitive-packages.md`) — too expensive for every Tier A patch bump.

It does NOT:

- Scan social media, Reddit, HN, or chat servers for vibe checks. Those are noisy and don't programmatically reduce to a yes/no signal.
- Run `npm audit` or `go list -m -u` against the local lockfile. The PR's CI already does that and the result lives in `statusCheckRollup`.
- Attempt to read package source-code diffs for malicious patterns. That's a `/review-pr` / subagent responsibility, not a routine check.

## Signal 1: GitHub Advisory Database

Query the `/advisories` endpoint per ecosystem and package. The `ecosystem` parameter maps from the PR's **detected** dependabot ecosystem (Phase 1.5) to the GH-advisories ecosystem name:

| Detected ecosystem | GH advisories ecosystem |
|---|---|
| `npm` | `npm` |
| `gomod` | `go` |
| `github-actions` | `actions` |
| `pip` | `pip` |
| `cargo` | `rust` |
| `rubygems` | `rubygems` |
| `composer` | `composer` |

```bash
# Per package, scoped to advisories published in the last 30 days
# (older advisories that affect the new version are caught by lockfile CI).
#
# Notes on the API call:
# - GitHub's /advisories endpoint does NOT support a `package=` query parameter for
#   server-side filtering; it accepts `ecosystem`, `severity`, `cve_id`, `ghsa_id`,
#   `type`, `published`, `updated`, `modified`, etc. We filter by package
#   client-side via the .vulnerabilities[].package.name field below.
# - Use GNU-style `date -d '30 days ago'` so the command works on both Linux
#   (autonomous runners) and macOS (`brew install coreutils` provides `gdate`
#   if BSD `date` differs; the GNU form is what Linux ships with).
SINCE=$(date -d '30 days ago' '+%Y-%m-%d' 2>/dev/null || date -u -v-30d '+%Y-%m-%d')

# $ECOSYSTEM holds the Phase-1.5 (dependabot) name. Translate to the GitHub Advisory DB ecosystem name.
case "$ECOSYSTEM" in
  gomod)          ADVISORY_ECO="go" ;;
  github-actions) ADVISORY_ECO="actions" ;;
  cargo)          ADVISORY_ECO="rust" ;;
  *)              ADVISORY_ECO="$ECOSYSTEM" ;;   # npm, pip, rubygems, composer map 1:1
esac

gh api "/advisories?ecosystem=$ADVISORY_ECO&published=>$SINCE" \
  | jq --arg pkg "$PKG" '
    [.[]
     | . as $adv
     | (.vulnerabilities // [])
       | map(select(.package.name == $pkg))
       | select(length > 0)
       | $adv + {
         affected_ranges: ($adv.vulnerabilities | map(select(.package.name == $pkg)) | map(.vulnerable_version_range))
       }
    ]
    | .[]
    | {ghsa_id, cve_id, severity, summary, published_at, affected_ranges}
  ' 2>/dev/null
```

The two-step pattern (server-side ecosystem+date filter, then client-side package match via jq) is necessary because the `/advisories` endpoint has no package-name query parameter. A naive `&package=$PKG` would be silently ignored by the server and return EVERY advisory in the ecosystem for the time window — a noisy false-positive flood.

For grouped PRs (`chore: bump the X group ...`), extract package names from the PR body's bullet list first:

```bash
gh pr view $PR --json body --jq '.body' | \
  grep -oE '(Updates? `[^`]+` from|Bumps `[^`]+` from)' | \
  grep -oE '`[^`]+`' | tr -d '`' | sort -u
```

For each extracted package, run the advisory query.

**Interpretation:**

- **Critical or High severity advisory with the bumped-to version in `affected_range`** → set confidence to LOW, surface in the digest as 🔴 BLOCKER
- **Medium severity advisory matching** → set confidence to LOW, surface as 🟡 WARNING
- **Low / informational severity** → note in digest but don't gate confidence
- **No advisories** → no signal; confidence depends on other inputs

**Don't conflate "advisory exists for older versions" with "advisory affects the bumped-to version."** The version-range check is load-bearing. An advisory affecting `< 8.0.0` should not gate a `8.18.0 → 8.20.0` bump.

## Signal 2: Release-notes keyword scan

The dependabot PR body already contains the release notes for the new version. Scan that body ONLY for unambiguous yank / advisory markers. Generic version keywords ("major version", "deprecated", "BREAKING CHANGE in some other version") produce too many false positives because dependabot release-note summaries routinely include phrases like "if you have a major version requirement..." or "this fixes a regression that was introduced in 1.x".

```bash
gh pr view $PR --json body --jq '.body' > /tmp/pr-body.txt

# Strong negative signals (any match → LOW confidence)
# Must be unambiguous: explicit yank, do-not-use, or reverted-in-version markers.
grep -iE '\b(yanked|do[[:space:]]+not[[:space:]]+upgrade|do[[:space:]]+not[[:space:]]+use|critical[[:space:]]+bug|hotfix[[:space:]]+(needed|required)|reverted[[:space:]]+in[[:space:]]+v?[0-9])' /tmp/pr-body.txt

# Medium negative signals (any match → LOW confidence; these are also unambiguous markers)
grep -iE '(\bCVE-[0-9]{4}-[0-9]+\b|\bGHSA-[a-z0-9-]{15,}\b|security[[:space:]]+vulnerability|security[[:space:]]+advisory)' /tmp/pr-body.txt
```

**DO NOT scan for bare `BREAKING`, `MAJOR`, `deprecated`, or `regression`** — those are too common in normal release notes to produce useful signal:

- `regression` appears in routine release notes like "fixes a regression introduced in 1.4.2" — false-positives every patch release that fixes anything.
- `BREAKING` and `MAJOR` are caught more reliably by the version-shape detection in Phase 2 (any `X.Y → X+1.0` is already Tier C → LOW). Re-checking the release notes for the same signal duplicates work and produces false positives when the body mentions other versions' major bumps.
- `deprecated` alone appears in nearly every major library's release notes; deprecation in upstream's new version doesn't automatically mean the repo uses the deprecated API.

**History:** an early live run hit a regex that included `MAJOR\s+(change|version)` and matched the phrase "major version" in every PR body, marking all non-data PRs as LOW. This made the confidence tiering useless. The tightened patterns above (yank / do-not-use / CVE / GHSA only) restored a clean HIGH / MEDIUM / LOW split on the same batch.

**If a future incident shows that a different keyword would have caught a regression:** add it to STRONG_NEGATIVE only if it's specific enough that false-positive rate is < 5%. Generic words like "regression" or "breaking" should never go back in.

## Signal 3: Package repo issue spike (security-sensitive packages only)

For packages matching the effective security-sensitive list (`security-sensitive-packages.md` universal default ∪ `.weekly_deps.security_sensitive_extra`), additionally check whether the package's own GitHub repo has had a spike of bug reports since the new version was published. This catches cases where the advisory hasn't been filed yet but users are reporting crashes.

```bash
# Resolve the package's repository URL. The autonomous runner only has `gh`
# in its allowed tools — no curl/wget. For the npm path, use a static known-repo
# mapping for security-sensitive npm packages, OR skip Signal 3 entirely when the
# package isn't in the mapping. Most security-sensitive npm packages have their
# canonical repo at github.com/<owner>/<repo>, so the known-mapping covers the
# high-value cases; Signals 1+2 still cover anything outside it.
#
# The mapping is a small shared baseline below, UNIONED with the repo's
# `.weekly_deps.signal3_repo_map` (read via rc_get). A repo registers its own
# stack-specific package→repo URLs there rather than editing this file.
SIGNAL3_EXTRA="$(rc_get '.weekly_deps.signal3_repo_map' '{}')"   # JSON object: { "<pkg>": "<repo url>", ... }

case $ECOSYSTEM in
  npm)
    # Shared baseline of widely-used security-sensitive npm packages, then
    # overlay the repo's signal3_repo_map (repo entries win on key collision).
    REPO_URL=$(jq -nr --arg p "$PKG" --argjson extra "$SIGNAL3_EXTRA" '
      ({
        "ajv": "https://github.com/ajv-validator/ajv",
        "ajv-formats": "https://github.com/ajv-validator/ajv-formats",
        "yaml": "https://github.com/eemeli/yaml",
        "js-yaml": "https://github.com/nodeca/js-yaml",
        "jose": "https://github.com/panva/jose",
        "jsonwebtoken": "https://github.com/auth0/node-jsonwebtoken",
        "argon2": "https://github.com/ranisalt/node-argon2",
        "bcrypt": "https://github.com/kelektiv/node.bcrypt.js",
        "axios": "https://github.com/axios/axios",
        "undici": "https://github.com/nodejs/undici",
        "dompurify": "https://github.com/cure53/DOMPurify",
        "socket.io": "https://github.com/socketio/socket.io",
        "socket.io-client": "https://github.com/socketio/socket.io-client",
        "ws": "https://github.com/websockets/ws"
      } * $extra)[$p] // empty')
    [ -z "$REPO_URL" ] && { echo "Signal 3 skipped: $PKG not in known-mapping" >&2; return; }
    ;;
  gomod)
    # Go module path often IS the repo path; for github.com/foo/bar use that directly
    REPO_URL="https://$PKG"
    ;;
  github-actions)
    # GitHub Actions packages: ecosystem-name == owner/repo
    REPO_URL="https://github.com/$PKG"
    ;;
  *)
    # pip / rust / rubygems / composer etc.: check signal3_repo_map only.
    REPO_URL=$(jq -nr --arg p "$PKG" --argjson extra "$SIGNAL3_EXTRA" '$extra[$p] // empty')
    [ -z "$REPO_URL" ] && { echo "Signal 3 skipped: $PKG not in signal3_repo_map" >&2; return; }
    ;;
esac

# Extract owner/repo from a github.com URL
GH_PATH=$(echo "$REPO_URL" | grep -oE 'github\.com/[^/]+/[^/]+' | sed 's|github.com/||')

# Get the new version's publish date
PUB_DATE=$(gh api "/repos/$GH_PATH/releases/tags/v$NEW_VERSION" --jq '.published_at' 2>/dev/null || \
           gh api "/repos/$GH_PATH/releases/tags/$NEW_VERSION" --jq '.published_at' 2>/dev/null)

# Count issues opened since publish that mention the new version + crash/regression keywords
gh api "/search/issues?q=repo:$GH_PATH+is:issue+is:open+created:>$PUB_DATE+($NEW_VERSION+OR+crash+OR+regression+OR+broken)" \
  --jq '{count: .total_count, top: [.items[0:3] | .[].title]}'
```

**Interpretation:**

- `count > 5` for a popular package, or `count > 1` for a less-popular package → 🟡 WARNING in digest
- `count == 0` → no signal, no entry in digest

**Rate-limit awareness:** the `/search/issues` endpoint has a 10/min unauthenticated, 30/min authenticated rate limit. The autonomous runner uses a `gh` token so the higher 30/min limit applies. Only the subset of PRs whose packages hit the security-sensitive allowlist run Signal 3, so a typical weekly batch is well under the limit. Cap at 10 issue-spike queries per run anyway to avoid burning the rate limit on an exceptionally large batch.

## Output format consumed by Phase 3.5

The lookup should produce one record per PR, structured like:

```json
{
  "pr": 968,
  "package": "ajv",
  "bump": "8.18.0 → 8.20.0",
  "advisories": [
    {
      "ghsa_id": "GHSA-xxxx-yyyy-zzzz",
      "severity": "MEDIUM",
      "summary": "Prototype pollution in ajv",
      "affects_new_version": false
    }
  ],
  "release_notes": {
    "strong_negative_matches": [],
    "medium_negative_matches": ["CVE-2026-12345"],
    "breaking_change_matches": []
  },
  "issue_spike": {
    "count": 2,
    "top": ["8.20.0: schema validation throws on null values", "broken in 8.20"]
  },
  "verdict": "LOW",
  "reason": "issue_spike.count=2 + release_notes.medium_negative_matches=['CVE-2026-12345']"
}
```

Phase 3.5 (confidence assessment) reads `verdict` and `reason` and incorporates them into the final confidence label. If `verdict == "LOW"` from this lookup, the PR is LOW regardless of other signals.

## Failure modes

- **API rate limit exhausted** → log the affected PRs, skip the lookup for those, mark them as UNKNOWN confidence (more conservative than HIGH).
- **Package's repo URL is non-GitHub** (e.g., on GitLab or self-hosted) → skip Signal 3, rely on Signals 1+2 only.
- **`gh api` returns 404 for the release tag** (some packages don't use GitHub releases) → skip the issue-spike check, rely on Signals 1+2.
- **The dependabot PR body is empty or truncated** → skip Signal 2, rely on Signals 1+3. Should not happen but is worth handling.

In all failure modes, the lookup degrades gracefully — a missing signal sets confidence to UNKNOWN (which triggers Copilot review in Phase 3.6, the same as LOW), not HIGH. Failure of the lookup must never elevate confidence.
