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
| `GET /v1/health` | `concord-selfhost health --json` | the health object, also when a check fails |
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
| `restart` | none | `stop`, then `up` |
| `backup` | none | `backup --json` (into the install's `backups/`) |
| `restore` | `{"archive":"concord-backup-<UTC>.tar.gz"}` | `restore <backups>/<archive> --confirm --json` |
| `update` | `{"tag":"vX.Y.Z"}` | `update --to <tag> --confirm --json` (signed tags only) |

- Only one write runs at a time; another gets 409 `write_in_progress`.
  Reads keep working while it runs.
- `restore` takes only a file name the CLI generates, and only for a regular
  file (not a link) directly in the install's `backups/` directory. `update`
  takes only a tag shaped like `vX.Y.Z`; the CLI then refuses any tag that is
  not signed by a release key. Any other field, a body on an action that
  takes none, or a body over 1 KiB gets 400.
- `GET /v1/jobs` lists the last 16 jobs, newest first. `GET /v1/jobs/<id>`
  returns one with `state` (`running`, `succeeded`, `failed`), its start and
  finish times, the archive a backup wrote, and the last 64 KiB of the CLI's
  output, sanitised like log lines. Jobs live in memory, so an agent restart
  forgets them.
- A job is not tied to the request that started it. Start, stop and restart
  get 15 minutes; backup, restore and update get 2 hours, after which the run
  is killed. A restart of the agent itself also kills a running job.

The agent does not check who is asking beyond the socket's permissions. The
Admin Console requires a signed-in admin, a fresh WebAuthn check for stop,
update and restore, and an audit row for every write before it calls the
agent.

## Admin Console

The Admin Console's Server page reads the agent through three signed-in admin
routes. Each call writes one `server_read` row to the admin audit log with the
action, the service for logs, and the result, even when the browser leaves
before the answer. A failed audit write is logged as a warning and does not
block the read, as for every other admin action. Server reads are recorded
only in the audit log; they emit no separate security event. The three routes share a limit
of 30 requests per minute per client address; more get 429 `rate_limited`.

| Route | Agent request |
| --- | --- |
| `GET /admin/api/v1/server/status` | `/v1/status` |
| `GET /admin/api/v1/server/health` | `/v1/health` |
| `GET /admin/api/v1/server/logs?service=S&lines=N` | `/v1/logs` (never follow) |

`concord-selfhost` connects the console to the agent only when both of these
hold, and otherwise leaves control-plane exactly as before:

1. `ADMIN_CONSOLE_ENABLED=true` in `.env`.
2. The `concord-agent` group exists on the host.

It then adds `docker-compose.host-agent.yml`, which mounts
`/run/concord-agent` read-only into control-plane and adds the container to
that group. Run `concord-selfhost up` after installing the agent to apply it.
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
  are open at once.
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

1. Creates the `concord-agent` system group.
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
Removing the package stops and masks the agent and leaves `/opt/concord` in
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
4. Copy `infrastructure/host-agent/concord-agent.service` to
   `/etc/systemd/system/`, and in that copy set
   `ExecStart=/usr/local/libexec/concord-agent` and `CONCORD_AGENT_CLI` to
   the wrapper's absolute path in your checkout.
5. Run `systemctl daemon-reload && systemctl enable --now concord-agent`.

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
