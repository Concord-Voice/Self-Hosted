# Self-Host Release Build

This runbook covers building and attesting a self-host bundle. Pushing a tag
does not publish anything. A published release is not a supported install
until it passes the fresh-host acceptance in
[selfhost-public-acceptance.md](selfhost-public-acceptance.md): v0.3.0 was
published before that acceptance, so its docs say install is unsupported.

## What the tag builds

`.github/workflows/release.yml` runs on a `v*` tag push. It:

1. Runs the IOC supply-chain scan.
2. Runs `scripts/release/check-release-tag.sh`. The tag must be `v` plus the
   root `VERSION` file (one `X.Y.Z` line, surrounding whitespace ignored), and
   the tagged commit must be on `main`.
3. Exports the tracked files at the tagged commit with `git archive`, then runs
   `scripts/release/selfhost-bundle.py build` and `verify`. The outputs are
   `concord-selfhost-<version>.tar.gz`, `selfhost-manifest.json` and
   `SHA256SUMS`. The archive is deterministic: sorted paths, owner 0:0, modes
   0644/0755, and the commit time as every member's mtime. The manifest
   (`selfhost-bundle/v2`) records the version, tag, source SHA and the SHA-256
   of every file.
4. Uploads the three files as artifact `concord-selfhost-v<version>` and
   attests the build provenance of each file.

The workflow holds no credential and never creates a release.

## Cutting a release build

1. Merge the `VERSION` change and its `CHANGELOG.md` section to `main`.
2. Tag that `main` commit, for example
   `git tag -s "v$(tr -d '[:space:]' < VERSION)" <sha>`, and push the tag.
3. The Release Trigger App reports the successful run to the release
   pipeline, which checks each file's attestation against this workflow at
   the tag and SHA, then opens an **unsigned draft** release.
4. The pipeline's signing step fails closed until its signing steps exist.
   A release published before fresh-host acceptance must say in its docs and
   notes that installing it is not supported.

## Checking a bundle by hand

```bash
git archive --format=tar <sha> | tar -x -C /tmp/tree
python3 scripts/release/selfhost-bundle.py verify --public-tree /tmp/tree \
  --archive concord-selfhost-<version>.tar.gz \
  --manifest selfhost-manifest.json --sums SHA256SUMS
gh attestation verify concord-selfhost-<version>.tar.gz --repo Concord-Voice/Self-Hosted \
  --signer-workflow Concord-Voice/Self-Hosted/.github/workflows/release.yml \
  --source-ref refs/tags/v<version>
```

## Failures

| Symptom | Cause | Action |
|---|---|---|
| `tag vX does not match VERSION Y` | The tag and `VERSION` differ | Delete the tag and tag the commit whose `VERSION` matches. |
| `<sha> is not on main` | Tagged a branch commit | Tag the merged commit on `main`. |
| `VERSION must be one X.Y.Z line` | Prefix, suffix or extra text in `VERSION` | Fix `VERSION` through a PR. |
| `selfhost bundle refused: unsafe payload type/link` | A tracked symlink or special file | Remove it from the tree through a PR. |
