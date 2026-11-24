# Rename the control-plane Go module path — Plan

Spec: `docs/superpowers/specs/2026-11-18-rename-go-module-path-design.md`

## Tasks

1. **Branch** `chore/rename-control-plane-module-path` from `origin/main`.
2. **Substitute the full path** in every tracked file:
   `git grep -lz "$OLD" | xargs -0 perl -pi -e 's#\Q$OLD\E#$NEW#g'`, where `$OLD` is the
   previous module path and `$NEW` is `github.com/Concord-Voice/Self-Hosted/services/control-plane`.
   Expected: 1,412 files, equal insertions and deletions.
3. **Fix the bare-prefix references** in `license-check.yml`, `prepare-protected-pr-analysis.py`
   and `test-update-notice-md.sh` (spec step 2).
4. **Verify locally:** `gofmt -l`, `go build ./...`, both `go vet` runs, `go mod tidy -diff`,
   `govulncheck ./...`, `python3 -m unittest discover -s scripts/sonar/tests`,
   `scripts/test-update-notice-md.sh` and `scripts/test-check-compose-manifest-coverage.sh`.
   DB-backed Go tests run in CI.
5. **PR:** push with the keyring `gh` login (the diff touches `.github/workflows/`), open a
   draft, wait for CI, mark ready, reconcile the reviews.
