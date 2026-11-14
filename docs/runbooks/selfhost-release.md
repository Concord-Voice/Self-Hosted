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
4. In a separate job with no `id-token`, scans the same exported tree with
   syft (pinned by version and SHA-256) and writes a CycloneDX JSON SBOM,
   `concord-selfhost-v<version>.cdx.json`. Dev dependencies are included,
   because the bundle ships every lockfile whole.
   `scripts/release/check-selfhost-sbom.py` then fails the run unless the
   SBOM lists every npm package locked in a `package-lock.json` and every Go
   module a `go.mod` requires, at the locked version. The job uploads it as
   the intermediate artifact `selfhost-sbom`.
5. Uploads the bundle files and the SBOM as artifact
   `concord-selfhost-v<version>` and attests the build provenance of each
   file. `SHA256SUMS` lists only the archive and manifest. Release-Ops requires
   the SBOM at the top of this artifact and attaches it to the release as an
   asset.

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
# The SBOM: same attestation check, then the completeness check.
gh attestation verify concord-selfhost-v<version>.cdx.json --repo Concord-Voice/Self-Hosted \
  --signer-workflow Concord-Voice/Self-Hosted/.github/workflows/release.yml \
  --source-ref refs/tags/v<version>
python3 scripts/release/check-selfhost-sbom.py --sbom concord-selfhost-v<version>.cdx.json --tree /tmp/tree
```

## Failures

| Symptom | Cause | Action |
|---|---|---|
| `tag vX does not match VERSION Y` | The tag and `VERSION` differ | Delete the tag and tag the commit whose `VERSION` matches. |
| `<sha> is not on main` | Tagged a branch commit | Tag the merged commit on `main`. |
| `VERSION must be one X.Y.Z line` | Prefix, suffix or extra text in `VERSION` | Fix `VERSION` through a PR. |
| `<file>.cdx.json: missing npm …` or `missing golang …` | syft did not catalog a locked package (a syft upgrade or a config change) | Fix the syft version or options in `release.yml` through a PR, then re-tag. |
| `sha256sum: WARNING: 1 computed checksum did NOT match` in the SBOM job | The syft download does not match the pinned checksum | Do not change the checksum to the new value. Check the syft release, then pin a version and checksum through a PR. |
| `selfhost bundle refused: unsafe payload type/link` | A tracked symlink or special file | Remove it from the tree through a PR. |
