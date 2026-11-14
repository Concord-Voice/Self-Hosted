# Self-host backup and restore

`concord-selfhost backup` and `concord-selfhost restore` save and recover one self-hosted instance. Run both from the repository root, as the user who runs `concord-selfhost up`.

## What a backup holds

`concord-selfhost backup [--out DIR] [--json]` writes `concord-backup-<UTC time>.tar.gz` with mode `0600` into `DIR`. The default `DIR` is `./backups`. A `DIR` that does not exist is created with mode `0700`. An existing `DIR` must already be owned by the same user with mode `0700`; the command refuses any other directory and never changes its mode. The archive contains:

| Part | Content |
|---|---|
| `postgres.dump` | `pg_dump --format=custom` of the `concord` database, taken while the stack runs |
| `minio.tar` | The bundled MinIO volume. Present only in bundled storage mode. With BYO S3, back up your bucket using your provider's own tools. |
| `env` | The root `.env`, **including every secret** |
| `manifest.json` | Schema, release, storage mode, creation time, and a SHA-256 for each part |

Treat the archive like the `.env` it contains: copy it off the host to encrypted storage, and never commit it. `/backups/` is git-ignored.

MinIO data is copied while MinIO runs. The copy is crash-consistent, but the stack is not paused. Stop the stack first if you need a frozen copy.

With `--json`, stdout carries `{"schema":1,"ok":true,"archive":"<file name>"}` on success and `{"schema":1,"ok":false}` on failure. Progress goes to stderr.

## Restore

`concord-selfhost restore ARCHIVE --confirm [--json]` **replaces the database** and, in bundled mode, **all stored files**. Before it stops anything, it refuses an archive when any of these is true:

- It contains anything other than the top-level directory and the four known parts (`manifest.json`, `postgres.dump`, `env`, `minio.tar`) as regular files. The archive is read once, and each part is written by its name, never by a path from the archive.
- A part is missing, a part is not in the manifest, or a checksum does not match.
- Its release differs from this install. The release is the git commit in a git checkout, or the content of `VERSION` in an install without git.
- Its storage mode differs from this install.
- Its `env` differs from the current `.env`.

When every check passes, the command:

1. Stops the stack.
2. Starts Postgres alone.
3. Runs `pg_restore --clean --if-exists --single-transaction`.
4. In bundled mode, unpacks the archived MinIO copy into the volume, then replaces the old objects with it.
5. Runs `concord-selfhost up`, then `concord-selfhost health`.

### Restoring onto a new host

1. Install the **same release** with the installer.
2. Extract `env` from the archive with `tar -xzf ARCHIVE ./env`.
3. Replace `.env` with it, and set its mode to `0600`.
4. Run `restore`.

The `.env` check exists because the database and its keys must come from the same install. Copying the archived `env` into place is therefore a deliberate step.

### When restore fails partway

- **A database step fails:** `--single-transaction` means the database is left unchanged. The stack stays stopped. Run `concord-selfhost up` to resume on the old data.
- **The MinIO step fails:** the archive is unpacked beside the live objects and swapped in only after it unpacks fully, so a bad stream leaves the old objects in place. The database has already been restored by then. Re-run the same restore.

## Updating to a release tag

`concord-selfhost update --to vX.Y.Z --confirm [--json]` works only in a git checkout of Self-Hosted. It runs these steps:

1. It refuses if `infrastructure/deploy/release-signers` names no key, which is true **today**. The release key is added when the release-signing work starts signing tags. Until then, every tag is refused.
2. It refuses if tracked files have local changes.
3. It fetches only that tag into a private ref (`refs/concord-update/candidate`), so local tags are never replaced. It refuses unless `git verify-tag` passes against `release-signers` (SSH signatures) and the signed tag object names the requested tag and points at a commit. The name check stops an older signed release from being served under a newer tag name.
4. It runs `backup`. If the backup fails, nothing has changed.
5. It checks out the tag (detached), then runs the **tag's own** `concord-selfhost up` and `health`.

If health fails, the command prints the two commands to roll back: check out the previous commit, then restore the backup it just took. A backup can only be restored onto the release that wrote it, so check out the previous commit first.

Once `concord-selfhost authority init` or `upgrade` has run, `up` also migrates the account-key authority's database. Backups do not include that database, so a rollback leaves the authority's schema where the newer release put it. If the older release's authority then refuses that schema, `up` warns and the rest of the stack still runs; nothing calls the authority until the client integration ships.
