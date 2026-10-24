# coturn TLS certificates

The base `docker-compose.yml` mounts a host directory read-only into the coturn container at
`/etc/coturn/certs`. The host directory is `${COTURN_CERTS_DIR:-./infrastructure/docker/coturn/certs}`,
so this directory is the development default only. The compose `command:` block passes explicit
absolute paths:

```
--cert=/etc/coturn/certs/cert.pem
--pkey=/etc/coturn/certs/key.pem
```

Without these explicit paths, coturn falls back to its built-in relative
lookup (`turn_server_cert.pem` / `turn_server_pkey.pem`). That lookup fails on
the first pass. It then "finds" a nonsensical `//turn_server_cert.pem` path.
The result is startup warnings and flaky TLS/DTLS listener behavior.

## What to put here

Drop two files (or symlinks):

- `cert.pem` — PEM-encoded server certificate (fullchain OK)
- `key.pem` — PEM-encoded private key (unencrypted)

Permissions: `key.pem` should be `600` (owner read-only). The container
mounts the directory `:ro`, so it blocks runtime writes regardless.

## Local development

You do not need real certs in dev. The STUN listener on 3478/udp works
without them, and mediasoup typically routes local traffic peer-to-peer.
To exercise the TLS listener locally, generate a self-signed pair:

```bash
openssl req -x509 -newkey rsa:4096 -nodes \
  -keyout key.pem -out cert.pem -days 365 \
  -subj "/CN=localhost"
```

## Staging / production

Production sets `COTURN_CERTS_DIR=/opt/concord/certs/coturn` in `.env`.
[`infrastructure/deploy/copy-certs.sh`](../../../deploy/copy-certs.sh) is the certbot deploy
hook. It writes `cert.pem` and `key.pem` into that directory after every issuance or renewal,
then restarts the `concordvoice-coturn` container. coturn does not reload certificates on its
own, so the restart is part of the hook.

Do not put production certificates in this repository directory. Do not add a second volume
entry for `/etc/coturn/certs` — the base compose file already mounts one.

## Why this dir is checked in

The `.gitkeep` file keeps the directory tracked in git so the compose
bind mount succeeds on a fresh clone. Never commit the actual `cert.pem` or `key.pem`
files. Add them to your deployment's secret store.
