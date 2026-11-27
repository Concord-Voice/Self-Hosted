# Self-hosted host agent

The Concord Host Agent lets the Admin Console read and manage a self-hosted
server. It is a small Go program that runs as root under systemd and answers
on a unix socket. Its only network listener is the first-run setup wizard
(see [Setup mode](#setup-mode)), which closes for good once the server is
installed.

The socket API has three reads and six write actions, and nothing else
(setup mode, below, can also run the installer's preflight and a fresh
install). The reads:

| Request | Runs | Returns |
| --- | --- | --- |
| `GET /v1/status` | `concord-selfhost status --json` | the status object ([machine output](selfhost-machine-output.md)) |
| `GET /v1/health` | `concord-selfhost health --json` | the health object, with its `warnings`, also when a check fails |
| `GET /v1/logs?service=S&lines=N&follow=0\|1` | `concord-selfhost logs [--no-follow] --tail N S` | plain-text log lines |

`S` must be one of `control-plane`, `media-plane`, `postgres`, `redis`,
`nats`, `minio` or `coturn`. `N` is 1 to 1000 and defaults to 200. The agent
answers any other path with 404, any other method with 405, and a bad
parameter with 400. It never runs a shell and never passes caller text to the
command line.

### Write actions

A write is `POST /v1/actions/<action>`. It answers 202 with a job at once,
and the caller polls the job:

| Action | Body | Runs |
| --- | --- | --- |
| `start` | none | `concord-selfhost up` |
| `stop` | none | `concord-selfhost stop` |
| `restart` | none | `stop`, then `up`; refused with 409 `security_log_boundary_missing` first if `up` would refuse for that |
| `backup` | none | `backup --json` (into the install's `backups/`) |
| `restore` | `{"archive":"concord-backup-<UTC>.tar.gz"}` | `restore <backups>/<archive> --confirm --json` |
| `update` | `{"tag":"vX.Y.Z"}` | `update --to <tag> --confirm --json` (signed tags only) |

- Only one write runs at a time; another gets 409 `write_in_progress`.
  Reads keep working while it runs. The lock covers only the agent: do not
  run `concord-selfhost` by hand while the console runs a write.
- `restore` takes only a file name the CLI generates, and only for a regular
  file (not a link, else 404 `unknown_archive`) directly in the install's
  `backups/` directory, which must be owned by root and closed to group and
  others (else 409 `untrusted_backups`): the archive's `.env` is what the
  root agent runs with afterwards. `update`
  takes only a tag shaped like `vX.Y.Z`; the CLI then refuses any tag that is
  not signed by a release key. Any other field, a body on an action that
  takes none, or a body over 1 KiB gets 400.
- `update` never moves the host to an older release. The agent compares the
  tag with the install's `VERSION` file and refuses an older tag with 409
  `update_downgrade_refused`. If it cannot read `VERSION`, it refuses with
  409 `running_version_unknown`. To roll back on purpose, run
  `concord-selfhost` as root on the host (see [Break-glass](#break-glass)).
- The agent admits at most one `backup` or `update` per hour (`update` takes
  a backup first). A second one within the hour gets 429
  `backup_rate_limited`. The time of the last admission is kept in the
  agent's root-only audit directory, so a restart or reboot does not reset
  it. A backup that fails still uses its hour.
- With no alert destination, every write gets 503 `alerts_not_configured`
  (see [Out-of-band action alert](#out-of-band-action-alert)). Reads keep
  working.
- A missing alert destination is refused before the key prompt. The agent
  checks `write_in_progress`, `backup_rate_limited` and
  `alerts_not_configured` again after the tap but before it spends the
  approval, so these refusals do not use up the admin's key tap.
- `GET /v1/jobs` lists the last 16 jobs, newest first. `GET /v1/jobs/<id>`
  returns one with `state` (`running`, `succeeded`, `failed`), its start and
  finish times, the archive a backup wrote, and the last 64 KiB of the CLI's
  output, sanitised like log lines. A failed job ends with a
  `[concord-agent: <action> failed: …]` line saying why. Jobs live in memory,
  so an agent restart forgets them.
- `GET /v1/backups` lists the archives `restore` accepts, newest first, at
  most 100: name, size and modification time. A `backups/` that does not
  exist yet is an empty list; one that is not root-only gets 409
  `untrusted_backups`, as restore does.
- A job is not tied to the request that started it. Stop gets 15 minutes;
  the others get 2 hours (`up` may rebuild images), after which the run is
  killed.
- Stopping or restarting the agent kills a running job, so while one runs the
  agent keeps `/run/concord-agent/write-in-progress`. A package upgrade then
  leaves the old agent running and says to restart it afterwards, and package
  removal refuses until the job ends.

On an appliance that runs the containers itself (TrueNAS), set
`CONCORD_AGENT_MANAGED_BY=truenas` in the agent's environment. The agent then
refuses every write route, `/v1/jobs` and `/v1/backups` with 409
`{"error":"appliance_managed","appliance":"truenas"}` and keeps serving
reads. Any other value stops the agent at startup.

The socket's group permissions only admit the control-plane container. They do
not authorize a write. The agent requires the expected peer UID and an
agent-local WebAuthn authorization for every write, including start and
backup. The control plane cannot turn its database credentials into that
authorization.

## Admin Console

The Admin Console's Server page reads the agent through signed-in admin
routes. Each call writes one `server_read` row to the admin audit log with the
action, the service for logs, and the result, even when the browser leaves
before the answer. A failed audit write is logged as a warning and does not
block the read, as for every other admin action. Server reads are recorded
only in the audit log; they emit no separate security event. The routes share a limit
of 30 requests per minute per client address; more get 429 `rate_limited`.

| Route | Agent request |
| --- | --- |
| `GET /admin/api/v1/server/status` | `/v1/status` |
| `GET /admin/api/v1/server/health` | `/v1/health` |
| `GET /admin/api/v1/server/logs?service=S&lines=N` | `/v1/logs` (never follow) |
| `GET /admin/api/v1/server/jobs` | `/v1/jobs` |
| `GET /admin/api/v1/server/jobs/<id>` | `/v1/jobs/<id>` |
| `GET /admin/api/v1/server/backups` | `/v1/backups` |
| `POST /admin/api/v1/server/stepup` | none: starts a WebAuthn check |
| `POST /admin/api/v1/server/actions/<action>` | `/v1/actions/<action>` |

### Console write actions

1. Every action starts a fresh, user-verified WebAuthn ceremony in the Host
   Agent. Its root-owned authorizer store supplies the administrator's
   credential; the control plane cannot substitute a credential from its
   database.
2. The console sends the normalized action and, for update or restore, its
   tag or archive to begin the ceremony. The agent returns a one-use handle
   and assertion options. The handle is bound to that administrator, action
   and exact arguments as the agent recorded them at begin time, expires after
   two minutes, and is spent before the assertion is checked. A retry always
   needs a new ceremony. The assertion is bound to the agent's own record of
   the action, not to what the user sees: the authenticator shows the relying
   party, not the action. Login and host actions use the same keys and the
   same root-owned sign counter for each key.
3. The console then submits only the handle and assertion. The agent executes
   the normalized arguments stored at begin time after it verifies the
   assertion, persists the authenticator counter, and durably records the
   admission.
4. Every begin and action attempt writes a `server_write` audit
   row with the action, tag or archive, whether a step-up passed, the job id
   when one started, and the reason for a refusal. Step-up begins carry
   `"phase": "stepup_begin"`. Before calling the agent the control plane
   writes a `"phase": "requested"` row, and if that write fails it refuses
   with 503 `audit_unavailable` and does not call the agent. The agent call
   does not stop if the browser leaves, so the outcome row records what the
   agent did. When the agent may have started a job but its answer was lost,
   the row says `"may_have_started": true`; check `/server/jobs`. Unlike
   reads, these rows also emit a privileged-action security event.
5. Refusals: 403 `stepup_required`, `stepup_failed` or `stepup_unavailable`
   (no enrolled key), 400 `invalid_request`, 404 `unknown_action`, 503
   `audit_unavailable` or `agent_unavailable`, 500 `internal_error`. The
   agent's own refusals (for example 409 `write_in_progress`) pass through
   with its code; anything else from the agent becomes 502 `agent_error`. Job
   polls answer 400 `invalid_request`, 404 `unknown_job`, 502 `agent_error`
   or 503 `agent_unavailable`. The console shows a plain message for each
   refusal code. On an appliance-managed host, jobs, job polls
   and backups answer 409 `appliance_managed` with the appliance's name, and
   writes answer 409 `appliance_managed`.

The Server page's Actions panel uses these routes. It has buttons for start,
stop, restart and back up, a release-tag field for update and a backup picker
for restore. It asks for the security key before the four step-up actions,
follows the running job every 5 seconds, shows its output and lists recent
jobs. While a job runs the buttons are disabled. The panel checks the jobs list
every 30 seconds, so a job started in another tab or by another admin locks
them too. A rate-limited or failed poll is retried; it does not drop the job. On an
appliance-managed host the panel says to use the appliance's own UI and shows
no buttons.

Job polls count as reads and write `server_read` rows. All these routes share
the 30-per-minute limit with the other Server routes, so poll a job no faster
than every 5 seconds.

The agent also writes its own bounded, fsynced JSONL security record for every
denied, admitted, started, succeeded, or failed write at
`/var/log/concord-security/host-agent/events.jsonl`. It records only the
action, outcome, fixed reason code, and opaque administrator or job IDs. A
failed agent audit write refuses a new write before a job starts. This stream
uses `host-agent-audit.v1`; Nightwatch ingestion is not yet verified.

### Agent authorizer administration

The Host Agent stores the credentials it trusts at
`/etc/concord-voice/agent-authorizers/authorizers.json`. The directory and
file must be root-owned; the directory is mode `0755` and the public-key file
is mode `0644`. They are intentionally readable through the control plane's
read-only mount, but only host root can change them. Host root manages the
store with `concord-agent authorizer list`, `import`, and `revoke`. Import
requires the exact SHA-256 printed for an admin's exported authorizer file.

Revoke disables every listed credential for root actions, but retains its
public key and highest authenticator counter as a tombstone. Admin login still
checks that independent counter, so it cannot fall back to stale database-only
trust. The store holds at most 64 active records; revoked records do not count
toward that limit. A revoked record is never removed to make room, because its
counter keeps login clone detection working and stops a later import from
bringing back an old count. The file itself is limited to 1 MiB. A write that
would pass that limit is refused with `authorizer file size limit reached`,
and the file stays as it was. Reaching it takes thousands of records; to clear
it, host root removes revoked records from the file by hand and restarts the
agent. To re-enable a revoked key, import the same administrator's export
again with `--reinstate`. Without that flag, an export that holds a revoked
key is refused. A reinstated key keeps its saved counter. These are host-root
recovery and administration operations; the control plane only receives a
read-only projection.

The agent loads its authorizer configuration at startup. If no usable file
existed then, it checks again every 10 seconds; after the first successful
import it shuts down cleanly and exits with status 75, and systemd
(`Restart=on-failure`) starts it again with the new keys enabled. Its writes
are off until then, so no host action is interrupted. To enable the keys at
once instead of waiting, restart it yourself:

```bash
sudo systemctl restart concord-agent
```

Later imports, revocations and counter updates take effect without a restart,
because the agent reads the file on every check.

Then have the imported administrator begin a host write from the Admin Console.
It must reach the WebAuthn authorization prompt rather than report
`authorizer_unavailable`; only complete the action if it is an intended host
operation. While the Host Agent overlay is active, admin WebAuthn login also
checks the imported credential's root-owned counter. If the Host Agent is
unavailable, login fails closed rather than accepting a database-only result.

#### One key per administrator

Each administrator uses the same hardware key to sign in and to approve host
actions. Register two hardware keys per administrator, a primary and a
backup, and keep them apart. Either one signs in and approves host actions. Each key has one root-owned sign counter that both login and host
actions advance, so a cloned key shows up as a clone warning on whichever use
comes second.

With no active (unrevoked) key imported for an administrator, every host
write is refused (`authorization_denied`, audit reason
`no_action_credential`).

The residual risk is the shared key. The control plane serves the console. If
it is compromised, it can show an admin a sign-in prompt and use that tap to
approve a host action instead. The authenticator shows the relying party, not
the action, so the admin cannot tell the two apart. The out-of-band alert
below is the detection control for this. The update downgrade guard and the
hourly backup limit bound what one harvested tap can do.

#### Out-of-band action alert

The agent reports every admitted action, and refusals, to a webhook that only
host root configures. The control plane never reads or sets it. **The alert is
required:** without a valid alert file, every host write is refused with 503
`alerts_not_configured`. Reads and login keep working.

Create `/etc/concord-voice/agent-alert.json`, owned by root, mode `0600`
(the URL may carry a receiver token):

```json
{"webhook_url": "https://hooks.example.com/<receiver-path>"}
```

The URL must be an absolute `https` URL of at most 2048 bytes with no
fragment. The file is at most 4 KiB and must hold only that field. The agent
reads it at startup, so restart the agent after you change it, then send a
test alert:

```bash
sudo systemctl restart concord-agent
sudo /usr/libexec/concord-agent alert test
```

`alert test` prints `delivered`, or `delivery failed: class=<class>` (one of
`timeout`, `network`, `status`, `request`, `encode`). If the file is missing
or invalid, the agent logs a warning at startup and refuses writes.

Each alert is a JSON `POST` with a readable `text` line and the fields
`source` (`concord-host-agent`), `event` (`admitted`, `denied` or `test`),
`reason`, `action`, `tag`, `archive`, `admin_id`, `job_id`,
`suppressed_count`, `host` and `time`. It never carries assertion data. Point
it at a channel a human reads, such as a chat webhook, on a service that does
not depend on this server. **If an alert says an action was approved and no
admin asked for it, treat the control plane as compromised.**

Delivery is best effort and never blocks an action: a 5-second timeout, one
retry, no redirects and no proxy. The durable record is the agent audit log.
Admitted alerts and refusal alerts use separate queues, so a flood of
refusals cannot crowd out an admitted alert. Repeated refusals are reported
together with a count.

#### Break-glass

If the console or the control plane is unavailable, or you must do something
the agent refuses (for example a rollback to an older release), sign in to the
host as root, through SSH or the provider's console, and run `concord-selfhost`
directly. For example: `sudo ./infrastructure/deploy/concord-selfhost status`.
Do not run it by hand while the agent is running a write. The agent does not
gate root on the host.

#### First import on a new overlay

The Admin Console, including its pre-auth enrollment routes, is mounted only
when `ADMIN_CONSOLE_ENABLED=true`. Enable the console and run
`concord-selfhost up` to apply the Host Agent overlay before first enrollment.
With `HOST_AGENT_AUTHORIZER_PROJECTION=1`, an absent `authorizers.json` leaves
the Host Agent unable to verify a normal Admin login assertion, so that login
fails closed with a service-unavailable response. This does not block the
separate pre-auth enrollment route.

Bootstrap the administrator through the out-of-band control-plane CLI, then
complete the one-time enrollment link with the primary key. After the first
sign-in, register the backup key from the console's Security keys page. Before
the administrator's first normal login, host root must export the now-active
administrator's public-key bundle from the running control-plane and record the
SHA-256 printed to standard error. Import that exact file into the Host Agent:

```bash
sudo docker exec concordvoice-control-plane ./main admin export-agent-authorizer --username <handle> > admin-authorizer.json
sudo /usr/libexec/concord-agent authorizer import --file admin-authorizer.json --confirm-sha256 <printed-sha256>
```

The agent enables the first import by itself within about 15 seconds.

Export and import again after the administrator registers the backup key.
Configure the [out-of-band alert](#out-of-band-action-alert) before the first
host action.

`concordvoice-control-plane` is the fixed control-plane container name. The
import command above is for a package install; a checkout install uses
`/usr/local/libexec/concord-agent` instead. Do not rely on `concord-agent`
being on `PATH`.

After the import and restart succeed, normal Admin login can proceed. This
ordering is verified from the source and test contracts; it is not a
live-install acceptance probe.

### Resetting an administrator with Host Agent trust

When resetting enrollment for an administrator that may have imported Host
Agent trust, use the root-only wrapper rather than invoking the control-plane
reset command directly:

```bash
sudo ./infrastructure/deploy/concord-selfhost admin reset-enrollment --username <handle>
```

The wrapper resolves the administrator ID, revokes that administrator's Host
Agent authorizers, then runs the control-plane reset. Each step must succeed;
on a lookup, revoke, or reset failure it stops and does not print a new
enrollment token.

On a terminal, the wrapper shows the resolved ID and asks you to type the
username to confirm the account. Without a terminal (a script), nobody can
confirm, so you must pass the expected ID:

```bash
sudo ./infrastructure/deploy/concord-selfhost admin reset-enrollment --username <handle> --admin-id <uuid>
```

Take the ID from `sudo /usr/libexec/concord-agent authorizer list`. If the
username resolves to a different ID, the wrapper stops and changes nothing.

The direct `control-plane admin reset-enrollment --username <handle>` path
fails closed while the administrator remains in the imported, root-owned
authorizer projection. This prevents an old hardware key from retaining Host
Agent write authority after its control-plane enrollment is reset. If an
operator must use the direct path, host root must first explicitly revoke the
administrator's imported authorizers, then run the reset; prefer the wrapper
so that ordering is enforced.

### Rollout dependencies

Session-generation reset and indexed handle revocation remain separate work.
Do not treat an agent authorizer revoke as a substitute for that session
boundary. Attestation provenance is also separate work; this release rejects a
WebAuthn clone warning but does not establish provenance. This source
repository is the implementation source. Any managed-deployment mirror follows
a source merge and its own validation gates.

`concord-selfhost` connects the console to the agent only when both of these
hold, and otherwise leaves control-plane exactly as before:

1. `ADMIN_CONSOLE_ENABLED=true` in `.env`.
2. The `concord-agent` group exists on the host.

It then adds `docker-compose.host-agent.yml`, which mounts
`/run/concord-agent` and `/etc/concord-voice/agent-authorizers` read-only into
control-plane, adds the container to that group, and sets
`HOST_AGENT_AUTHORIZER_PROJECTION=1`. The projection check reads the file's
owner as uid 0 inside the container, so rootless Docker and `userns-remap`
installs are unsupported: the mapped uid 0 is not host root, so the check
cannot be trusted there. Run `concord-selfhost up` after installing the agent to apply it.
Until the agent is running the page says "The host agent is not reachable.
Install and start it, then run concord-selfhost up."

## Setup mode

When the agent starts on a host that has no `.env` in the install root (three
directories above `concord-selfhost`), it also serves a setup wizard over
HTTPS, so the operator can install from a browser on another machine.

1. The agent writes the wizard address, with a one-time token, to
   `/run/concord-agent/setup-token` (mode `0600`, root only), and prints the
   SHA-256 fingerprint of its self-signed certificate to its journal. The
   token never goes to the journal, which members of the `adm` and
   `systemd-journal` groups can read.

   ```text
   $ sudo cat /run/concord-agent/setup-token
   https://<this-host>:9443/#token=<64 hex characters>
   $ journalctl -u concord-agent
   concord-agent: setup mode on port 9443; the wizard address with its one-time token is in /run/concord-agent/setup-token (root only)
   concord-agent: setup certificate SHA-256 fingerprint AB:CD:…; setup closes in 1h0m0s
   ```

   If the host has a firewall, allow TCP 9443 from your LAN for the setup
   window (for example `ufw allow from 192.168.1.0/24 to any port 9443
   proto tcp`), and remove the rule afterwards.

2. Open the address from a browser on the LAN. The certificate is
   self-signed, so the browser warns. Compare the fingerprint the browser
   shows with the printed one before you continue; if they differ, stop.
   The token in the address is after `#`, so the browser never sends it in
   the request line, and the page removes it from the address bar.
3. The wizard runs `install-selfhost.sh --preflight-only --json` and lists the
   checks in plain words. The wizard offers the install only when every check
   passes, and the installer runs the same checks again itself.
4. The wizard collects the domain, public IPv4 address, certificate mode,
   SMTP server and storage. The agent writes them to an answers file at mode
   `0600` in a fresh `0700` directory, runs `install-selfhost.sh --env-file`
   on it, and deletes the directory whatever the outcome. The wizard shows
   the installer's output, which ends with the next steps to run on the
   server. The output also goes to the journal, so a browser that left
   before the install finished loses nothing.

The wizard closes after the first install that writes `.env`, after 60
minutes, or when the agent stops, whichever comes first. The token then stops
working and its file is removed; an install already running still finishes. An install that writes `.env` and then fails at a later step
still closes setup, because the installer refuses to run on an existing
installation; the wizard says so, and the output shows which step failed.
After that the token no longer works. To open a new window, run
`systemctl restart concord-agent`, which writes a new token; once `.env`
exists the agent does not start setup mode at all.

| Setting | Default |
| --- | --- |
| `CONCORD_AGENT_SETUP_ADDR` | `:9443` (all interfaces, so the LAN can reach it) |
| `CONCORD_AGENT_WIZARD_DIR` | `/usr/share/concord-agent/wizard` |

Build the wizard pages with `npm --prefix client/admin run build:setup` and
copy `client/admin/dist-setup/` to the wizard directory. The `.deb` ships
them there. The page handles the token, so the agent refuses setup mode
unless the wizard directory, everything in it and every directory above it is
owned by root and not writable by group or others, with no links inside.

Setup-mode rules:

- Every API call needs `Authorization: Bearer <token>`. The token is 32 random
  bytes and is compared in constant time.
- The right token always works. Wrong tokens are counted across all
  addresses; after 10 in 10 minutes, further wrong tokens get 429 and are no
  longer logged. A 256-bit token cannot be guessed, so the limit only keeps a
  flood out of the journal; it never locks out the operator.
- One preflight runs at a time; a second gets 409. At most 16 connections
  are open at once. At most four setup connections from one canonical source
  IP are admitted; an excess connection closes before TLS. The install
  response has an 11-minute write deadline (the 10-minute install budget plus
  time to serialize the result).
- One install runs at a time; a second gets 409. An install may take up to
  10 minutes, and closing the browser does not stop it. Its output is capped
  at 256 KiB, with terminal escapes and control characters removed; output
  past the cap is dropped without stopping the installer.
- The installer refuses an answers file with an unknown field. The agent also
  refuses unknown fields and any value with a line break or NUL, so an answer
  cannot add a line.
- Pages are served with a strict Content Security Policy, `nosniff`,
  `no-referrer` and no framing. API answers are `no-store`.

## Install

### Packaged install (Debian 12, Ubuntu 24.04)

The `concord-selfhost_<version>_<arch>.deb` package (amd64 or arm64) installs:

| Path | What |
| --- | --- |
| `/usr/libexec/concord-agent` | the agent |
| `/usr/lib/systemd/system/concord-agent.service` | its unit |
| `/usr/share/concord-agent/wizard/` | the setup wizard pages |
| `/usr/share/concord-selfhost/concord-selfhost-<version>.tar.gz` | the server scripts and Compose files for that release |

Install Docker Engine with the Compose plugin first, then:

```bash
sudo apt-get install ./concord-selfhost_<version>_<arch>.deb
```

On first install the package:

1. Creates the `concord-agent` system group and the root-owned
   `/etc/concord-voice/agent-authorizers` directory (mode `0755`).
2. Unpacks the release into `/opt/concord`, owned by root, with no file
   writable by group or others and no setuid bits, unless `/opt/concord`
   already exists (then it is left exactly as it is).
3. Enables `concord-agent`, and starts it when systemd is running. With no
   `.env` yet, the agent starts setup mode, and the package prints the
   command that shows the wizard address
   (`sudo cat /run/concord-agent/setup-token`). Installed into an image
   without a running systemd, the agent is enabled and starts on first boot.

`/opt/concord` belongs to you, not to the package. Upgrading the package
replaces only the agent, its unit, the wizard pages and the release tarball
under `/usr/share/concord-selfhost/`, and restarts the agent; it never
touches `/opt/concord` or its `.env`. If you disabled the agent, an upgrade
or a reinstall keeps it disabled. While setup has not finished (no `.env`
and the agent running), an upgrade does not restart the agent, because that
would stop the installer the wizard runs; restart it yourself afterwards.
The same holds while a write action runs, and removal waits for it to end
(see [Write actions](#write-actions)). Removing the package stops and masks the agent and leaves `/opt/concord` in
place; purging it also forgets whether the agent was enabled. Because `/opt/concord` is not a git checkout,
`concord-selfhost update` cannot update it yet; that waits on signed
releases.

To change a setting such as `CONCORD_AGENT_CLI`, use a drop-in
(`sudo systemctl edit concord-agent`); edits to the packaged unit are lost on
upgrade.

**Moving from a checkout install.** A unit in `/etc/systemd/system`
overrides the packaged one, so remove the old files before installing the
package: `sudo systemctl disable --now concord-agent`, then
`sudo rm /etc/systemd/system/concord-agent.service /usr/local/libexec/concord-agent`
and `sudo systemctl daemon-reload`. Unless your checkout is `/opt/concord`,
first point the packaged agent at it with a drop-in, so it keeps managing
your running server: create
`/etc/systemd/system/concord-agent.service.d/override.conf` containing
`[Service]` and
`Environment=CONCORD_AGENT_CLI=<checkout>/infrastructure/deploy/concord-selfhost`.
Without it the agent finds no `.env` beside the fresh `/opt/concord` the
package unpacks, and opens the setup wizard on a host that is already
installed.

**Upgrading a checkout install.** The new unit keeps `/etc` read-only except
for the authorizer directory. If you keep your old unit copy in
`/etc/systemd/system`, `/etc` stays read-only for the agent and the counter
advance after each login fails. If you copy the new unit without first
creating `/etc/concord-voice/agent-authorizers`, the unit fails to start with
status `226/NAMESPACE`. Create the directory first, then copy the unit.

### Upgrading an existing install

Do all of this before the first admin login after the upgrade. With
`docker-compose.host-agent.yml` setting `HOST_AGENT_AUTHORIZER_PROJECTION=1`
and no `authorizers.json`, the agent disables trust and admin WebAuthn login
fails closed with 503. Write actions return `authorizer_unavailable` until the
import is done.

1. Upgrade the files. Package install: `sudo apt-get install
   ./concord-selfhost_<version>_<arch>.deb` (the postinst creates
   `/etc/concord-voice/agent-authorizers`). Checkout install: update the
   checkout, rebuild the agent as in "From a checkout", and run
   `sudo install -d -o root -g root -m 0755 /etc/concord-voice/agent-authorizers`.
2. Checkout install only: re-copy `infrastructure/host-agent/concord-agent.service`
   to `/etc/systemd/system/`, re-apply your `ExecStart` and
   `CONCORD_AGENT_CLI` edits, and run `sudo systemctl daemon-reload`.
3. For each administrator, export the credentials from the running control
   plane and note the SHA-256 printed to standard error:
   `sudo docker exec concordvoice-control-plane ./main admin export-agent-authorizer --username <handle> > admin-authorizer.json`.
   Each administrator should have a primary and a backup key registered; see
   "One key per administrator".
4. Import each export with its exact fingerprint:
   `sudo /usr/libexec/concord-agent authorizer import --file admin-authorizer.json --confirm-sha256 <printed-sha256>`
   (`/usr/local/libexec/concord-agent` on a checkout install).
5. Create the alert file and test it; see "Out-of-band action alert". Without
   it, every host write is refused.
6. Restart the agent: `sudo systemctl restart concord-agent`.
7. Run `concord-selfhost up` to apply the overlay, then check the store with
   `sudo /usr/libexec/concord-agent authorizer list`.

Upgrade the control plane before or together with the agent. A control plane
older than this agent cannot read an authorizers file that holds more than 64
records in total, active and revoked, and admin login then fails closed.

### From a checkout

To install from a checkout instead:

1. Keep the checkout owned by root and not writable by group or others. The
   agent refuses to start, and refuses each request, if the `concord-selfhost`
   wrapper, any file beside it (or the target of a symlink beside it, and the
   directories above that target), or any
   directory above it is owned by another user or is group- or world-writable.
   It runs the wrapper by its resolved path, so a symlink on the configured
   path cannot be swapped after the check.
2. Build the agent: `go -C services/control-plane build -o /usr/local/libexec/concord-agent ./cmd/concord-agent`.
   (`/usr/libexec/concord-agent` belongs to the package.)
3. Create the group that may use the socket: `groupadd --system concord-agent`.
4. Create the root-owned authorizer directory required by the service unit:
   `sudo install -d -o root -g root -m 0755 /etc/concord-voice/agent-authorizers`.
5. Copy `infrastructure/host-agent/concord-agent.service` to
   `/etc/systemd/system/`, and in that copy set
   `ExecStart=/usr/local/libexec/concord-agent` and `CONCORD_AGENT_CLI` to
   the wrapper's absolute path in your checkout.
6. Run `systemctl daemon-reload && systemctl enable --now concord-agent`.

Check it from the host:

```bash
sudo curl --unix-socket /run/concord-agent/agent.sock http://agent/v1/health
```

The socket is `/run/concord-agent/agent.sock`, mode `0660`, owner
`root:concord-agent`, in a `0750` directory. The directory is kept across restarts so
a container that mounts it keeps seeing the live socket. The agent removes a
stale socket on start and refuses to start if something else is at that path,
or if the socket's directory holds anything else (it must be a dedicated
directory, because the agent sets its owner and mode). Each request
is logged to the journal as `action=… outcome=…`, without parameters.

| Setting | Default |
| --- | --- |
| `CONCORD_AGENT_CLI` | required; absolute path of `concord-selfhost` |
| `CONCORD_AGENT_SOCKET` | `/run/concord-agent/agent.sock` |
| `CONCORD_AGENT_GROUP` | `concord-agent` |

## Limits

- At most 8 requests run at once; more get 429.
- At most 2 log follows run at once, each for at most 10 minutes.
- Status times out after 30 seconds and health after 60. Their output is
  capped at 1 MiB. The agent decodes it and passes on only the documented
  fields; fields it does not know are dropped, not refused.
- A log request whose run fails before any line is sent gets 502
  `logs_unavailable`. If it fails after lines were sent, including on a line
  over 1 MiB, the reply ends with `[concord-agent: output incomplete]`.
- An idle connection is closed after 60 seconds.
- A log request without follow returns at most the lines asked for.
- Each log line is capped at 4096 bytes, with terminal escapes, control
  characters and invisible format characters (such as bidi overrides)
  removed. Tab is kept.
- A response that the client stops reading is cut 5 seconds after the
  action's time budget, so a stalled client cannot hold a slot.

## Threat model

- **Socket access means control of the server.** Anyone in the
  `concord-agent` group can read service logs, which may contain user
  identifiers, and can stop, update or restore the server. Add only the
  control-plane container's group to it.
- **The agent trusts the checkout.** It runs the wrapper as root, and the
  wrapper reads the Compose files and `.env` in the checkout. The agent checks
  the wrapper's directory and every directory above it before every run. Its
  reads only read (`compose ps`, `config`, `logs`); its writes run the
  wrapper's own `up`, `stop`, `backup`, `restore` and `update`; setup mode
  also runs the installer, which writes `.env` and the nginx config in the checkout. A
  writable Compose file elsewhere in the checkout becomes a path to root the
  next time an operator runs `up`.
  Keep the whole checkout root-owned.
- **The `docker` group is already root.** Membership of the `docker` group
  gives root on the host with or without this agent.
- **Confused deputy.** The agent allows only fixed actions and validates
  their arguments itself, whatever the caller. The control-plane proxy is a
  convenience, not the gate.
- **Log injection.** Log lines are sanitised so a container cannot write
  escape sequences or fake line breaks into the console.
- **Denial of service.** The request, follow and output limits above bound
  what a socket user can make the agent run.
- **Environment.** The wrapper runs with a fixed `PATH`, `LANG` and `HOME`,
  no stdin, and stderr discarded for reads (writes and the setup wizard keep
  stderr to show the CLI's progress and messages). A cancelled request kills its whole process
  group.
- **Setup wizard on the LAN.** Until the server is installed, anyone on the
  LAN can load the wizard page, but nothing runs without the token, and the
  token is only in a root-only file. Anyone who can read that file on an
  uninstalled host can configure it. Someone on the network
  path could intercept the first connection, which is why the fingerprint
  check matters. The wizard can only run the installer's preflight and a
  fresh install; it cannot change an installed server.
