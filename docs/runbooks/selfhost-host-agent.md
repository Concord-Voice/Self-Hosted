# Self-hosted host agent

The Concord Host Agent lets the Admin Console read the state of a self-hosted
server. It is a small Go program that runs as root under systemd and answers
on a unix socket. It has no network listener.

This first version is read-only. It can do three things, and nothing else:

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

The Admin Console connection is added separately; until then the agent only
answers on its socket.

## Install

The packaged install (#187) will do these steps for you. To install from a
checkout today:

1. Keep the checkout owned by root and not writable by group or others. The
   agent refuses to start, and refuses each request, if the `concord-selfhost`
   wrapper, any file beside it (or the target of a symlink beside it, and the
   directories above that target), or any
   directory above it is owned by another user or is group- or world-writable.
   It runs the wrapper by its resolved path, so a symlink on the configured
   path cannot be swapped after the check.
2. Build the agent: `go -C services/control-plane build -o /usr/local/libexec/concord-agent ./cmd/concord-agent`.
3. Create the group that may use the socket: `groupadd --system concord-agent`.
4. Copy `infrastructure/host-agent/concord-agent.service` to
   `/etc/systemd/system/`, and set `CONCORD_AGENT_CLI` to the wrapper's
   absolute path in your checkout.
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
  `logs_unavailable`.
- An idle connection is closed after 60 seconds.
- A log request without follow returns at most the lines asked for.
- Each log line is capped at 4096 bytes, with terminal escapes, control
  characters and invisible format characters (such as bidi overrides)
  removed. Tab is kept.
- A response that the client stops reading is cut 5 seconds after the
  action's time budget, so a stalled client cannot hold a slot.

## Threat model

- **Socket access means read access to the server.** Anyone in the
  `concord-agent` group can read service logs, which may contain user
  identifiers. Add only the control-plane container's group to it.
- **The agent trusts the checkout.** It runs the wrapper as root, and the
  wrapper reads the Compose files and `.env` in the checkout. The agent checks
  the wrapper's directory and every directory above it. Its own actions only
  read (`compose ps`, `config`, `logs`), but a writable Compose file elsewhere
  in the checkout becomes a path to root the next time an operator runs `up`.
  Keep the whole checkout root-owned.
- **The `docker` group is already root.** Membership of the `docker` group
  gives root on the host with or without this agent.
- **Confused deputy.** The agent allows only fixed actions, whatever the
  caller. A future proxy in the control plane is a convenience, not the gate.
- **Log injection.** Log lines are sanitised so a container cannot write
  escape sequences or fake line breaks into the console.
- **Denial of service.** The request, follow and output limits above bound
  what a socket user can make the agent run.
- **Environment.** The wrapper runs with a fixed `PATH`, `LANG` and `HOME`,
  no stdin, and stderr discarded. A cancelled request kills its whole process
  group.
