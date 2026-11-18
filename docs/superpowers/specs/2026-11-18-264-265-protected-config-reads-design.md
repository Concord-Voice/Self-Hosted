# Protected config reads for base-image-lint and migration-lint — Design (Issues #264, #265)

Issues: #264, #265. Both come from the security review of PR #263.

## Problem

- **#264.** `check-base-images.py` refuses a Dockerfile path tracked as a symbolic link or a
  submodule, but it opens `.github/base-images.json` with symlinks followed and never checks the
  manifest's git mode. A PR that turns the manifest into a symlink would let later edits to the
  target change the manifest without the code-owner review of `/.github/base-images.json`.
- **#265.** The protected `migration-lint` job reads `.squawk.toml` and
  `.github/requirements-squawk.txt` from `HEAD^1`. That is the base only when the checkout is
  GitHub's two-parent merge commit. On a single-parent checkout `HEAD^1` is a PR commit, and the
  PR supplies the config that judges it.

## Design

### #264

- With no FILE list (the mode the protected job uses), the checker already reads
  `git ls-files -s`. It now also finds the manifest's record in that output and exits 2 when the
  manifest is not tracked, or is tracked with a mode other than `100644` or `100755`.
- This check runs before the manifest is read, so a symlinked manifest is never opened.
- The manifest is opened with `O_NOFOLLOW` in every mode, so a FILE-list run (no git) also
  refuses a symlink.
- The current manifest is a regular `100644` file, so `main`'s protected copy of the checker
  keeps passing.

### #265

- New first step after the checkout: assert that `HEAD` has exactly two parents and that
  `HEAD^2` equals `github.event.pull_request.head.sha`. Otherwise fail. The changed-migration
  selection (`git diff HEAD^1 HEAD`) keeps its meaning only under that condition.
- The install step validates `github.event.pull_request.base.sha` as a 40-hex SHA, fetches that
  commit by SHA (`git fetch --depth=1 origin <sha>`), and reads both files with
  `git show <sha>:<path>`. A second `actions/checkout` into a sub-directory was rejected: the
  sub-directory would sit inside the PR's working tree, where the PR controls the paths.
- The advisory copy in `pr-ci.yml` gets the same change ("change both together").

## Acceptance criteria

- The checker exits 2 for a manifest tracked as a symlink (also when the working tree holds a
  regular file), for an untracked manifest, and for a symlinked manifest with a FILE list.
- `migration-lint` in both workflows fails on a single-parent checkout, on a merge of another
  head, and on an empty head SHA. It reads both files from the base SHA, not from `HEAD^1`.
- Each new test fails against the old code.
