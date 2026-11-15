---
name: release-checklist
description: Run release verification checks for a new version
argument-hint: [version]
disable-model-invocation: true
allowed-tools:
  - Bash(git *)
  - Bash(gh *)
  - Bash(npm *)
  - Bash(go *)
  - Read
  - Grep
  - Glob
---

Release checklist for version **$0**.

## 0. Resolve this repo's layout

Read the layout from `.claude/repo-config.json` (`.release.*`) once. Every command
below uses these values. A repo that has not set them gets generic defaults
(`frontend`, `backend`, `v`, `release.yml`). Check the printed values match this
repo before running anything below: a default can name a real but wrong path.

```bash
# Load the helper from the default branch, never this checkout (on a PR branch it is the PR's code).
RC_REF="refs/remotes/origin/$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null)"   # the API is the authority; origin/HEAD goes stale after a rename
[ "$RC_REF" != refs/remotes/origin/ ] || RC_REF="$(git symbolic-ref --quiet refs/remotes/origin/HEAD || echo refs/remotes/origin/main)"
case "$RC_REF" in refs/remotes/origin/HEAD) RC_REF= ;; refs/remotes/origin/?*) ;; *) RC_REF= ;; esac
RC_OID="$([ -n "$RC_REF" ] && git show-ref --verify --hash "$RC_REF" 2>/dev/null)"   # exact refname, no DWIM: a same-named branch or tag cannot stand in
case "$([ -n "$RC_OID" ] && git ls-tree "$RC_OID" -- scripts/repo-config.sh | cut -d' ' -f1)" in
  100644|100755) ;;
  *) echo "FATAL: ${RC_REF:-the default branch} is not fetched or has no regular scripts/repo-config.sh" >&2; return 1 2>/dev/null || exit 1 ;;
esac
RC_SRC="$(git show "$RC_OID:scripts/repo-config.sh")" || { echo "FATAL: could not read the default-branch helper" >&2; return 1 2>/dev/null || exit 1; }
eval "$RC_SRC"   # the commit that was checked; the checkout's copy is never sourced
unset RC_TRUSTED_STATE RC_TRUSTED_CFG   # nothing carries over from an earlier run in this shell
# Presence comes from the tree, not from a read: a blob that is listed but cannot be read is an
# error, never "absent". RC_TRUSTED_STATE: absent = no config on the default branch; present = read.
RC_ENT="$(git ls-tree "$RC_OID" -- .claude/repo-config.json)" || { echo "FATAL: could not list the default-branch tree" >&2; return 1 2>/dev/null || exit 1; }
case "${RC_ENT%% *}" in
  "") RC_TRUSTED_STATE=absent ;;
  100644|100755)
    RC_TRUSTED_CFG="$(git show "$RC_OID:.claude/repo-config.json")" && [ -n "$RC_TRUSTED_CFG" ] \
      || { echo "FATAL: could not read the default-branch .claude/repo-config.json" >&2; return 1 2>/dev/null || exit 1; }
    RC_TRUSTED_STATE=present ;;
  *) echo "FATAL: the default-branch .claude/repo-config.json is not a regular file" >&2; return 1 2>/dev/null || exit 1 ;;
esac
DEFAULT_BRANCH="${RC_REF#refs/remotes/origin/}"
FRONTEND_DIR="$(rc_get '.release.frontend_dir' 'frontend')"     # e.g. apps/desktop
BACKEND_DIR="$(rc_get '.release.backend_dir' 'backend')"         # e.g. services/api
TAG_PREFIX="$(rc_get '.release.tag_prefix' 'v')"                 # e.g. v, or app-v
RELEASE_WORKFLOW="$(rc_get '.release.workflow' 'release.yml')"   # the workflow that builds the release
echo "frontend=$FRONTEND_DIR backend=$BACKEND_DIR tag_prefix=$TAG_PREFIX workflow=$RELEASE_WORKFLOW"
for d in "$FRONTEND_DIR" "$BACKEND_DIR"; do [ -n "$d" ] && [ -d "$d" ] || echo "WARN: release dir '$d' not found; set .release.*_dir"; done
git tag -l "${TAG_PREFIX}*" | head -3   # must list real tags; empty means TAG_PREFIX is wrong
```

## 1. Version Consistency

- [ ] Check current version: `jq .version "$FRONTEND_DIR/package.json"`
- [ ] Verify tag doesn't exist: `git tag -l "${TAG_PREFIX}$0"`. Step 0 checked that
      the prefix matches real tags: a pattern that matches no tag in the repo
      returns empty for every version and can never fire.
- [ ] Verify no release is published for it: `gh release view "${TAG_PREFIX}$0"`
      (expect `release not found`). Do NOT check with `gh release list` or
      `GET /releases/latest` — if every release is a prerelease, that endpoint
      404s and the check proves nothing.
- [ ] Desktop version matches: `$FRONTEND_DIR/package.json`

## 2. CI Status

- [ ] All GitHub Actions passing on main: `gh run list --branch main -L 5`
- [ ] SonarQube Quality Gate passing
- [ ] No open release-gating issues: `gh issue list --label "risk: release-gate" --state open --json number,title,milestone`

      Do **not** add `--milestone "v$0"` when `$0` is a PATCH version
      (`0.2.42`) and milestones exist only at MINOR granularity (`v1.0.0`,
      `v1.1.0`, …). A patch-version milestone filter matches nothing and exits 0,
      so the check passes vacuously while release-gate issues are open. List them
      all and confirm by eye that none belongs to the milestone this tag ships
      toward.

## 3. Test Suite

- [ ] Go tests pass: `(cd "$BACKEND_DIR" && go test ./... -race)`
- [ ] Frontend tests pass: `(cd "$FRONTEND_DIR" && npx vitest run)`
- [ ] Coverage >= 80% on new code (SonarQube enforced)

## 4. Security Checks

- [ ] No critical Dependabot alerts: `gh api repos/{owner}/{repo}/dependabot/alerts --jq '[.[] | select(.state=="open" and .security_advisory.severity=="critical")] | length'`
- [ ] Pre-commit hooks pass on HEAD: `pre-commit run --all-files`
- [ ] No secrets in codebase: `gitleaks detect --source .`

## 5. Documentation

- [ ] CHANGELOG.md updated for ${TAG_PREFIX}$0 (`$0` is the bare version, without the prefix)
- [ ] CLAUDE.md counts current (migrations, stores, routes)
- [ ] API docs reflect any new endpoints

## 6. Build Verification

- [ ] Desktop build succeeds locally: `(cd "$FRONTEND_DIR" && npm run build)`
- [ ] Go build succeeds: `(cd "$BACKEND_DIR" && go build ./...)`

## 7. Release Steps

Once all checks pass:
1. Bump version in `$FRONTEND_DIR/package.json` to `$0`
2. Commit: `git commit -m "chore: bump version to $0"`
3. Push to main and let the repo's release workflow run. If the main-branch CI
   workflow **dispatches** the release build through the REST API (rather than via
   `workflow_call`, e.g. because a called workflow cannot reach the release
   environment's secrets), the build is a separate run.
4. Monitor build: `gh run list --branch main -L 1 --workflow "$RELEASE_WORKFLOW"`.
   **A dispatched release run is `event_name == 'workflow_dispatch'` and appears
   under the release workflow, not the dispatcher.** The dispatcher shows only its
   dispatch step, not the build/signing matrix — watching it is the common mistake.
5. Verify release artifacts on GitHub Releases page

## 8. Post-Release

- [ ] Close milestone — **only when this tag completes a MINOR milestone.** A
      patch release does not close a milestone. Milestones are `v1.0.0`,
      `v1.1.0`, … and a patch tag usually ships *toward* one rather
      than completing it. When it does complete one:
      `gh api repos/{owner}/{repo}/milestones/{id} -X PATCH -f state=closed`
- [ ] Verify auto-updater delivers to existing clients
- [ ] Smoke test: login and core user flows on a fresh install
