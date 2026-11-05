# Self-host backup and restore

`concord-selfhost backup` and `concord-selfhost restore` save and recover one self-hosted instance. Run both from the repository root, as the user who runs `concord-selfhost up`.

## What a backup holds

`concord-selfhost backup [--out DIR] [--json]` writes `concord-backup-<UTC time>.tar.gz` with mode `0600` into `DIR`. The default `DIR` is `./backups`, and the command creates it with mode `0700`. The archive contains:

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

- It contains anything other than regular files and directories, or any absolute or `..` path.
- A part is missing, a part is not in the manifest, or a checksum does not match.
- Its release differs from this install. The release is the content of `VERSION`, or the git commit when there is no `VERSION` file.
- Its storage mode differs from this install.
- Its `env` differs from the current `.env`.

When every check passes, the command:

1. Stops the stack.
2. Starts Postgres alone.
3. Runs `pg_restore --clean --if-exists --single-transaction`.
4. In bundled mode, empties the MinIO volume and replaces it with the archived copy.
5. Runs `concord-selfhost up`, then `concord-selfhost health`.

### Restoring onto a new host

1. Install the **same release** with the installer.
2. Extract `env` from the archive with `tar -xzf ARCHIVE ./env`.
3. Replace `.env` with it, and set its mode to `0600`.
4. Run `restore`.

The `.env` check exists because the database and its keys must come from the same install. Copying the archived `env` into place is therefore a deliberate step.

### When restore fails partway

- **A database step fails:** `--single-transaction` means the database is left unchanged. The stack stays stopped. Run `concord-selfhost up` to resume on the old data.
- **The MinIO step fails:** the volume may be partly emptied. Re-run the same restore.

## Updating to a release tag

`concord-selfhost update --to vX.Y.Z --confirm [--json]` works only in a git checkout of Self-Hosted. It runs these steps:

1. It refuses if `infrastructure/deploy/release-signers` names no key, which is true **today**. Release-Ops adds the release key when it starts signing tags ([Release-Ops#34](https://github.com/Concord-Voice/Release-Ops/issues/34)). Until then, every tag is refused.
2. It refuses if tracked files have local changes.
3. It fetches only that tag, and refuses unless `git verify-tag` passes against `release-signers` (SSH signatures).
4. It runs `backup`. If the backup fails, nothing has changed.
5. It checks out the tag (detached), then runs the **tag's own** `concord-selfhost up` and `health`.

If health fails, the command prints the two commands to roll back: check out the previous commit, then restore the backup it just took. A backup can only be restored onto the release that wrote it, so check out the previous commit first.
