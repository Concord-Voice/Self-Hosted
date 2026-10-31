# Self-hosted TLS / certificate provisioning

> Server-side TLS for a self-hosted Concord Voice instance. The
> `install-selfhost.sh` wizard drives it. You can also run it directly.
> Client trust of a self-signed cert is a **separate, unshipped** concern — see the
> threat-model note under `local` below.

`infrastructure/deploy/provision-cert.sh` provisions the TLS origin (nginx reverse
proxy + coturn TURNS) in one of three modes. All three converge on the **same
canonical cert directory**. The installer prints the selected `sudo` command and
reports `Install configured. Certificate delivery and coturn TLS checks are pending.`
Provisioning reports `Certificate source and coturn delivery complete.` only after
source validation and required fan-out work finish. The self-host lifecycle reports
`coturn TLS listener loaded the selected certificate (5349).` only after its bounded,
hostname-verified handshake confirms the selected leaf.

The basic self-host release requires a parsed IPv4 `PUBLIC_IP`; IPv6-only TURN
deployment is unsupported. The standalone certificate helper can still handle
IPv6 SAN entries. That certificate capability does not establish IPv6 relay support.

## Choosing a mode

| Mode | Use when | Trust | Renewal |
|---|---|---|---|
| **`letsencrypt`** (recommended) | Public domain + routable IP, internet-reachable | Publicly trusted (ACME CA) | Automatic (certbot systemd timer) |
| **`local`** | LAN / homelab, no public domain | **Self-signed, NOT publicly trusted** | Manual re-run before expiry |
| **`import`** | You have a cert already (corporate CA or external ACME client) | As trusted as your cert | Manual re-import |

**Let's Encrypt is the recommended default for any internet-reachable instance.**
The installer steers domain holders to `letsencrypt`.

## Canonical cert layout

All three modes write:

```
/etc/letsencrypt/live/<domain>/
  fullchain.pem   (0644)
  privkey.pem     (0600)
```

Each mode then runs the existing `copy-certs.sh` deploy hook. The source private key
is root-owned with mode `0600`. The hook copies the pair to coturn
(`/opt/concord/certs/coturn/{cert,key}.pem`): the directories are root-owned mode
`0711`, `cert.pem` is root-owned mode `0444`, and `key.pem` is owned by UID `65534`,
GID `65533`, mode `0400`. These delivered-file permissions let the pinned coturn
identity read the key while preventing unrelated identities from reading it. The
hook also reloads nginx when it is active.
The rendered nginx vhost (`concordvoice.rendered.conf`) and the coturn
`COTURN_CERTS_DIR` already reference these paths. Nothing downstream needs
mode-specific configuration.

All modes and the copy hook require Python 3 with native SSL decoding. The leaf
certificate must have started its validity window and remain unexpired before
activation or delivery; copied candidates are checked again.

`local` and `import` stage a complete validated pair in a root-only directory on
the live filesystem before activating it with Linux atomic directory rename.
Existing regular local/import generations require `renameat2(RENAME_EXCHANGE)`;
unsupported exchange refuses without overwriting the live pair. Certbot-managed
lineages and symlinked live directories refuse these replacement modes. Renew
those lineages with Certbot instead.

The previous complete generation stays in a root-only retained directory for
recovery. An expired prior pair can be replaced. If delivery of a replacement
fails or is interrupted, the provisioner restores the previous source and checks
delivery again. An unavailable or failed recovery, including expiry of that
previous source, remains an error and does not claim completion. With no previous
generation, failed delivery retains the new complete source and reports that
recovery is unavailable. Retained directories contain private keys; preserve
their permissions and reconcile retention through a reviewed procedure.

The canonical directory always switches as one complete generation. Independent
certificate and key opens spanning that switch are not a snapshot of one
generation; delivery and verification remain required.

## Modes

### `letsencrypt`: ACME for domain holders

```bash
sudo infrastructure/deploy/provision-cert.sh letsencrypt \
  --domain example.com --email ops@example.com
```

This mode runs `certbot certonly --webroot -w /var/www/html` for `example.com`
and the `api.`, `media.`, and `turn.` subdomains. It passes `--reuse-key`, which
keeps the SPKI stable across renewals. The default self-host path installs and
runs only the required `copy-certs.sh` delivery hook. If the fixed managed hook
`/etc/letsencrypt/renewal-hooks/deploy/upload-cert-to-cloudflare.sh` already exists,
the helper refuses before issuance or mutation. Stop and investigate the host's
managed/self-host boundary. Cloudflare credential-file absence is not a safety
check. The explicit `--managed-cloudflare-upload` flag is reserved for the managed
provisioner; public installer commands do not select it.

**Preconditions:** DNS A records for the apex and the `api.`, `media.`, and
`turn.` subdomains must resolve to this host. The nginx server must serve
`/var/www/html` on port 80 for the ACME webroot challenge. Run
`sudo ufw allow 80,443/tcp` first. It matters here, because host nginx sits on
the `INPUT` path, which UFW controls. See the quickstart's
[Firewall (UFW)](selfhost-quickstart.md#firewall-ufw) section.

Provisioning prepares and verifies the complete root-owned renewal copy hook in
a private directory on the hook filesystem, then atomically renames it into place
before certificate issuance. Preparation or rename failure preserves the working
hook. Failed issuance, an incomplete certificate source, or failed delivery
restores the previous hook, including its lineage binding and executable mode;
a failed first issuance leaves no new hook. The installed executable binds the complete selected lineage path and
ignores other Certbot lineages before reading their certificates or operating any
service. Domain ordering does not change that binding. If that installation fails,
fix the reported hook problem and rerun the
same `letsencrypt` command; issuance did not complete. After delivery, install and
validate the rendered nginx vhost, then reload nginx before starting the stack.

**Renewal:** automatic. certbot installs a systemd timer. The required copy hook
delivers this selected lineage on renewal; unrelated lineages leave Concord
certificates and services untouched.

### `local`: self-signed for LAN / homelab

```bash
sudo infrastructure/deploy/provision-cert.sh local \
  --host homelab.lan --ip 10.0.0.5
```

This mode generates a self-signed RSA-4096 leaf. Its SAN covers `homelab.lan`,
the `api.`, `media.`, `turn.`, and `spa.homelab.lan` names, and the supplied IP.
It needs no public domain, no DNS, and no certbot. Omit the optional `--ip` flag
on a DNS-only LAN. The `--days` flag defaults to 825.

> ⚠ **Threat model.** A self-signed origin offers **no MITM protection on
> untrusted networks**. Anyone on the path can present their own cert. Use
> `local` only on a trusted LAN or homelab. For anything reachable from the
> internet, use `letsencrypt`.
>
> A LAN is also exactly where a same-segment neighbour sits. The stack's
> internal-service posture does not depend on trusting the LAN: Postgres, Redis
> and NATS publish no host socket, and the control-plane and media-plane HTTP ports bind
> `127.0.0.1`. The supported floor is Docker Engine 29.8.2 or newer: it retains
> the loopback/firewalld fix (CVE-2025-54388) and blocks registry credential
> disclosure through malicious mixed-address DNS responses (CVE-2026-92543).
> Compose v2 2.24.5 or newer remains required for the `!override` port policy.
> See the quickstart's [Requirements](selfhost-quickstart.md#requirements).
>
> The desktop client has **no** self-signed-cert trust path today. Every host is
> validated by Chromium's defaults — there is no per-origin consent prompt and no
> custom-CA import. (This is unchanged by the removal of the SaaS API cert pin:
> non-SaaS hosts always deferred to Chromium. See
> `docs/policies/api-tls-trust-model.md`.)
> #1618 closed
> **COMPLETED** without shipping that half of its scope, and **nothing currently
> tracks the gap** — do not read that issue's Completed state as the feature
> existing. The parent epic is
> #1615.

**Renewal:** none. Self-signed certs expire after `--days`. Re-run
`provision-cert.sh local` before expiry.

### `import`: bring your own certificate

```bash
sudo infrastructure/deploy/provision-cert.sh import \
  --cert /path/cert.pem --key /path/key.pem --host example.com
```

This mode validates the submitted pair before staging or installing a source
generation. Input validation must pass before any certificate source files are
written. The private staged pair is checked again before activation.
Import requires Python 3 with the native SSL certificate decoder supplied by
the supported Debian/Ubuntu packages. An unavailable decoder refuses import.
Typed DNS SANs are checked; URI text and the certificate's common name do not
provide SAN coverage.

1. The private key matches the certificate (SPKI comparison).
2. The certificate is currently within its `notBefore`/`notAfter` validity window.
3. The certificate's SAN covers the deployment base supplied as `--host` and
   `api.<host>`, `media.<host>` and `turn.<host>`. Exact names or a matching
   one-label wildcard are accepted. The base plus `*.<host>` covers all four;
   a wildcard alone does not cover the base.

These input-validation failures exit non-zero and install no certificate source
files. Later activation, delivery and recovery failures also exit non-zero;
complete generations may remain as described in [Canonical cert layout](#canonical-cert-layout).

**Renewal:** none. Re-run `provision-cert.sh import` with a fresh cert before expiry.

## Switching modes

To switch a domain from `local` to `letsencrypt` later, remove the stale
self-signed directory first. The certbot client refuses to issue into a
pre-existing, non-managed `live/<domain>` directory:

```bash
sudo rm -rf /etc/letsencrypt/live/<domain> /etc/letsencrypt/archive/<domain>
sudo infrastructure/deploy/provision-cert.sh letsencrypt --domain <domain> --email <email>
```

## Installer integration

`install-selfhost.sh` prompts for the TLS mode, and defaults to `letsencrypt`. It
never runs `provision-cert.sh` itself: every mode writes root-owned paths, and the
installer runs unprivileged. After it renders the nginx vhost, it prints the exact
`sudo` command for the chosen mode. Run it before installing the vhost, because
`nginx -t` needs the cert files. `local` can run straight away, and `import` once you
have your PEM pair. `letsencrypt` needs DNS pointing at this host and port 80 open;
nginx's stock default site serves `/var/www/html` for the challenge. The
mode is a wizard answer or CLI argument. Nothing writes it to `.env`, because it is
a deploy-time choice, not runtime config.

## Completion and recovery

The installer only configures the selected source path and prints the pending status.
Run its exact root command before installing the rendered nginx vhost. Successful
provisioning means the certificate source and coturn fan-out completed; it does not
mean coturn has loaded the certificate yet. Then install the rendered vhost, run
`sudo nginx -t`, reload nginx, prepare the security-event log boundary, and start the
stack with `./infrastructure/deploy/concord-selfhost up`. Finish with
`./infrastructure/deploy/concord-selfhost health`. That command reports the selected
certificate as loaded only after a bounded TLS handshake on the local published
5349/TCP listener returns the expected leaf.

If delivery fails but the source pair is still valid, preserve it and retry only the
existing copy hook with the quoted command shown by the failure guidance. Replace
both placeholders with the actual domain and clone path before running it:

```bash
sudo env RENEWED_LINEAGE='/etc/letsencrypt/live/<domain>' bash '/path/to/clone/infrastructure/deploy/copy-certs.sh'
```

This retries fan-out from the existing source; it does not rerun the installer or
regenerate the private key or application secrets. For Let's Encrypt, if the required
installed renewal hook is missing or invalid, rerun the shipped provisioner with
the original domain and email to reconstruct its lineage binding. Certbot may
contact ACME and retains `--reuse-key`; this also retries delivery. Do not install
the unbound source helper directly as a global renewal hook.

```bash
sudo bash '/path/to/clone/infrastructure/deploy/provision-cert.sh' letsencrypt --domain '<domain>' --email '<email>'
```

If issuance or import did not produce a valid complete source pair, correct that
mode's input and rerun the installer-provided provisioning command. A missing pair
is a bootstrap skip for the standalone hook; it never means that delivery completed.
After the stack starts, use `concord-selfhost health` for readiness and
`concord-selfhost logs coturn` for coturn logs. The listener proof establishes local
selected-certificate delivery only. It does not establish desktop client trust,
public ingress, NAT traversal, relay reachability or TURN allocation. Local
self-signed certificates remain untrusted by desktop clients; imported certificates
must be trusted and cover the service names clients use.

## Security notes

- No custom crypto: all issuance and validation uses `openssl` and `certbot` only.
- The scripts never log key material, key fingerprints, PEM bytes, raw environment
  values or raw TLS diagnostics. Certificate paths and public-certificate
  fingerprints may be shown.
- The source private key is mode `0600`; the delivered coturn key is mode `0400`
  with owner `65534:65533`, under root-owned `0711` directories.
- coturn `turns:<host>:5349` works in all three modes. `copy-certs.sh` mounts the
  cert from the canonical dir. Port 5349 is Docker-published, so UFW does not
  govern it on IPv4. See the quickstart's
  [Firewall (UFW)](selfhost-quickstart.md#firewall-ufw) section.

See `.claude/rules/deployment.md` (in Concord-Voice-Alpha) (cert-writer surface) and the design spec
`docs/superpowers/specs/2026-06-27-1617-selfhost-tls-cert-provisioning-design.md`.
