# Self-host machine-readable output

These flags give tools, such as the planned host agent (#182), a stable format to read. Human output is unchanged when a flag is absent.

## `install-selfhost.sh --preflight-only [--json]`

This runs every host check in order: `os`, `docker`, `resources`, `logrotate` and `ports`. It does not stop at the first failure. It writes nothing, needs no answers file, and does not require a fresh checkout.

Exit codes:

- `0` means every check passed.
- `2` means at least one check failed, or an argument was refused.

With `--json`, stdout carries exactly one object:

```json
{"schema":1,"checks":[{"name":"os","ok":true},{"name":"docker","ok":false}],"ok":false}
```

## `concord-selfhost status --json`

This prints one object built from `docker compose ps --all`. Services are sorted by service name.

```json
{"schema":1,"services":[{"service":"control-plane","name":"…","state":"running","health":"healthy","image":"…","exit_code":0}]}
```

`state` is one of Compose's states (`created`, `running`, `paused`, `restarting`, `removing`, `exited`, `dead`). `health` is `healthy`, `unhealthy`, `starting`, or empty when a service defines no healthcheck. `service`, `name` and `image` are plain identifiers (letters, digits and `._/:@+-`, at most 200 characters). Any value outside these forms is reported as `unknown`. `exit_code` is `null` when Compose does not report one. Output that Compose produces but that cannot be parsed exits `1`.

## `concord-selfhost health --json`

This runs the same checks as `health` and prints one object on stdout:

```json
{"schema":1,"checks":[{"name":"storage_model","ok":true},{"name":"control_plane","ok":true}],"ok":true}
```

The check names are `storage_model`, `storage_minio_inactive`, `control_plane`, `port_posture`, `proxy_provenance`, `coturn_tls` and `authority_delegation` (only after `concord-selfhost authority init` or `upgrade`). `proxy_provenance` verifies that the running bridge gateway, both application containers, and their proxy trust match the exact route persisted by `up`. A mismatch sets the check to `false` and makes `health` fail. When there is a warning, the object also has `"warnings":[...]` before `ok`. The only warning is `authority_delegation_expiring`, from 21 days before the delegation expires; `ok` stays true. A check that did not run because an earlier step failed is absent from the list. The exit code matches plain `health`.

## `concord-selfhost backup --json` and `restore --json`

See [selfhost-backup-restore.md](selfhost-backup-restore.md).

## Rules for readers

- `schema` changes only for incompatible changes. Readers must refuse a schema they do not know.
- Check names are fixed tokens. Status fields are fixed tokens or plain identifiers, as listed above. The JSON never contains host paths, addresses or free text.
- Human-readable reasons go to stderr only. Show them as text and never parse them.
- Any other option is refused before Docker is called (`status` and `health` exit `1`, the installer exits `2`).
