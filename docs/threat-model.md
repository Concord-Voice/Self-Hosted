# Threat model — Self-Hosted

| Field | Value |
| --- | --- |
| Repository | Concord-Voice/Self-Hosted |
| Visibility | Public |
| Last reviewed | 2026-11-23 at commit `4870ca12` |
| Owner | Concord Voice maintainers |
| Review when | A change to authentication, a trust boundary, or exposed surface; and at each release |

This file is the baseline of security priorities for this repository, written from the attacker's side first. To report a vulnerability, use the disclosure policy in [`.github/SECURITY.md`](../.github/SECURITY.md). This file is not the disclosure scope.

## System summary

Self-Hosted is the server software that an operator runs on their own host. The operator is the trust root of the deployment. Per the published privacy policy, the operator of a self-hosted server is the data controller for its users.

**Production components**

- **Control plane** (Go, `services/control-plane`). HTTP API and WebSocket hub: accounts, sessions, MFA, recovery, RBAC, ciphertext relay and storage, object-storage proxy. Routes are registered in `services/control-plane/internal/api/router.go`.
- **Media plane** (Node.js with mediasoup, `services/media-plane`). Socket.IO signalling and a WebRTC SFU. It forwards media frames that clients encrypt above the SFU.
- **Admin console** (`client/admin`, served by the control plane under `/admin`). Off unless `ADMIN_CONSOLE_ENABLED=true`. It has its own identity store, separate from user accounts.
- **Host agent** (`services/control-plane/cmd/concord-agent`). A root systemd service on the host. It runs fixed `concord-selfhost` commands for the admin console over a unix socket, and serves the first-run setup wizard over HTTPS on TCP 9443 until the server is installed.
- **Account-key authority** (`services/control-plane/cmd/account-authority`, `docker-compose.authority.yml`). A separate service and database that will anchor account keys. It is dormant: nothing in the control plane calls it yet.
- **Data and transport services** in `docker-compose.yml`: PostgreSQL, Redis, NATS, coturn (STUN and TURN), and bundled MinIO or an operator-supplied S3 endpoint (`docker-compose.byo-s3.yml`).
- **Edge**: host nginx, rendered from `infrastructure/deploy/nginx/concordvoice.conf` and `infrastructure/deploy/nginx/admin-console.conf.template`.

**Supported deployment mode.** One host, Docker Compose, files layered in this order: `docker-compose.yml`, `docker-compose.production.yml`, `docker-compose.selfhost.yml`, then optionally `docker-compose.byo-s3.yml`, `docker-compose.host-agent.yml` and `docker-compose.authority.yml`. The CLI is `infrastructure/deploy/concord-selfhost`; the installer is `infrastructure/deploy/install-selfhost.sh`. `docs/GETTING_STARTED.md` states that no published release has a supported install procedure yet.

**Build and release workflows.** `.github/workflows/release.yml` runs on a version tag. It builds a deterministic bundle, a CycloneDX SBOM and build-provenance attestations. It holds no credential and creates no release; a separate release pipeline does that (`docs/runbooks/selfhost-release.md`). `.github/workflows/publish-minio-image.yml` and `.github/workflows/publish-buildtools-base.yml` publish base images. Pull-request gates are `.github/workflows/pr-ci.yml` and `.github/workflows/required-pr-build.yml`, with scanner workflows beside them.

**Tests.** `services/control-plane/tests`, `services/media-plane/tests`, `infrastructure/deploy/tests` and package-level tests. Test stacks use `docker-compose.dev.yml` and the `tools` profile. They are not a production surface, but they share the base compose file with production.

## Assets

Ranked by value to an attacker.

| # | Asset | Where it lives; who holds it |
| --- | --- | --- |
| 1 | Host root | The host agent runs as root, and Docker access is root-equivalent. Root reads every secret and stored byte, and can replace the server code |
| 2 | Account E2EE identity keys | Each user's RSA private key is on the server only in wrapped form. The wrapping key is derived from the account password (`packages/e2ee-client/src/renderer/utils/crypto/crypto.ts`). The same password is sent to the control plane at sign-in and checked there (`services/control-plane/internal/auth/handlers.go`) |
| 3 | `JWT_SECRET` | Root `.env`; read by the control plane and the media plane. Signs user access tokens and MFA challenge tokens, and is the base of the HMAC keys for service-to-service proofs (`services/control-plane/pkg/mediaproof/mediaproof.go`) |
| 4 | Other deployment secrets | Root `.env`, held by host root: `POSTGRES_PASSWORD`, `REDIS_PASSWORD`, `MFA_ENCRYPTION_KEY`, `TURN_SECRET`, `MINIO_ROOT_PASSWORD` or the BYO `STORAGE_SECRET_KEY`, SMTP credentials. Created by `infrastructure/deploy/install-selfhost.sh` |
| 5 | Authority key material | The key with which the authority signs its results (RSK in the code), the authority TLS key, the product-root keys, and the key with which the control plane signs its requests to the authority (CAK in the code), as files under the authority directory (`infrastructure/deploy/selfhost-authority.sh`) |
| 6 | Admin identities and sessions | Password plus WebAuthn; opaque Redis sessions (`services/control-plane/internal/admin/middleware.go`) |
| 7 | The directory of public keys | The control plane stores each user's public key and serves it to peers, who wrap channel and DM keys to it (`services/control-plane/internal/users/handlers.go`) |
| 8 | Backup archives | One archive holds the database dump, the bundled object store and a copy of `.env` (`infrastructure/deploy/selfhost-backup.sh`) |
| 9 | Metadata | Who talks to whom, when, in which server and channel, from which address. Stored and processed in clear |
| 10 | Message, attachment and media content | Stored and relayed as ciphertext. Its exposure depends on assets 2 and 7 |
| 11 | Release integrity | The signer list `infrastructure/deploy/release-signers`, the attested bundle, and the digest-pinned third-party images in `docker-compose.yml` |
| 12 | Availability | One host carries every service. Voice needs the media plane, NATS and coturn together |

## Adversaries

| Adversary | Goal | Starts with | Does not have | A failure grants |
| --- | --- | --- | --- | --- |
| Anonymous internet client | Take over an account, register in bulk, exhaust the host, reach an internal route | Network reach to nginx, TURN and RTP ports | Any account, any secret | Account access, an outage, or a forged service-to-service call |
| Authenticated user | Read or change data in servers and DMs they are not in; escalate roles; disrupt voice | A valid account and tokens; control of their own client | Membership or permissions beyond their own; `JWT_SECRET` | Other communities' metadata and ciphertext; moderation bypass |
| Server owner or privileged member | Exceed the role hierarchy; keep access after demotion | Roles in one community server | Platform admin; host access | Authority over members that their role does not give |
| Platform admin (admin console) | Use the console to reach the host or user data | Admin password and hardware key | A shell on the host; user E2EE keys | Stop, update or restore of the whole server; service logs |
| Careless operator | None; misconfigures | Host root | Security expertise | Published data ports, weak proxy trust, a leaked backup or `.env` |
| Hostile or compromised operator | Read user content; impersonate users | Host root, all secrets, the running code, TLS termination | Users' passwords and unwrapped private keys, until a user signs in | See "Operator against users" below |
| Network attacker | Intercept setup, sign-in or media; redirect clients | A position on the path or on the LAN | A certificate the client trusts | Setup-wizard takeover on first connect; metadata; relay abuse |
| Other servers and remote peers | Use TURN or the media plane as a relay or reflector | UDP reach; TURN credentials if they hold an account | A way to name private relay targets | Reach into the operator's private network; amplification |
| Compromised dependency, base image or CI input | Ship code to every operator | A package, image or workflow input that the build consumes | The release signing keys | Code execution inside containers on every updated host |
| Compromised developer machine or AI agent | Land a change that weakens a boundary | A pull request | Bypass of required checks and review | A release that carries the weakness |
| Holder of a stolen session or lost device | Keep access; recover the account to their own device | A refresh token or a signed-in client | The account password; an MFA factor | Session use until the owner revokes it or a refresh-token theft check fires (`services/control-plane/internal/auth/token_theft_recovery.go`); recovery if a check fails in a recovery ceremony (the multi-step exchange that ends in a credential reset) |

### Operator against users

End-to-end encryption limits a passive operator. It does not make the operator harmless. The limits are:

- **Cannot learn from stored data alone:** message, attachment and media plaintext; unwrapped private keys; passwords. The database holds ciphertext, wrapped keys and Argon2id hashes (`services/control-plane/internal/auth/password.go`).
- **Can always learn:** all metadata, addresses, membership, presence, voice participation, object sizes and timing.
- **Can attempt offline:** password guessing against a stored hash or a wrapped private key.
- **Can do by acting, not only reading:** an operator who alters the running server, or observes its process, receives each user's password at sign-in. That password also unwraps the user's identity key. The same operator controls the public-key directory and can answer a key lookup with a key of their own. Until the account-key authority and its client integration are active, no component in this repository lets a client detect either act.
- **Can deny:** delete data, refuse service, disable accounts.

Treat "the server cannot read content" as true for a database copy, a backup, a storage vendor and an honest operator. Do not extend it to an operator who modifies the server.

## Attack surface

The "Reduce?" column marks a reduction candidate with "Yes": surface that exists for convenience or legacy.

| Entry point | Code path | Reach | Untrusted input | Reduce? |
| --- | --- | --- | --- | --- |
| Registration, sign-in, refresh, MFA verify and recovery under `/api/v1/auth` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/auth/handlers.go` | Unauthenticated | JSON bodies, key material, codes, email addresses | |
| SSO routes under `/api/v1/auth/sso` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/api/oauth_wiring.go` | Unauthenticated; registered in every build, and refused before the handler when the instance type is self-hosted | Provider name, registration, link and session bodies | Yes: no function on self-host |
| `/api/v1/client/config` and `/api/v1/server/capabilities` | `services/control-plane/internal/api/router.go` | Unauthenticated, with a per-address limit | None; they return client configuration and capability flags to any caller | |
| Authenticated REST API under `/api/v1` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/middleware/auth.go` | Authenticated | IDs, permission bitfields, ciphertext, wrapped keys | |
| Control WebSocket `/api/v1/ws` | `services/control-plane/internal/websocket/handler.go` | Authenticated by single-use ticket or bearer token | Frames, `Origin`, renewal tokens | Bearer-token fallback, kept for older clients |
| Service-to-service routes under `/api/v1/internal/` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/api/voice_enforcement_sessions.go` | Unauthenticated at the router; each handler checks an HMAC proof | Proof headers, small JSON bodies | Forwarded by the public reverse proxy. Yes: room release keeps a second, older message form |
| Release-attestation publish routes | `services/control-plane/internal/attestation/publish_handler.go` | Unauthenticated at the router; each handler checks an OIDC token | Token, version and hash fields | Yes: serves a release flow that this repository does not carry |
| Public reads: avatars, banners, server icons, invite and friend-code previews, and feedback screenshots when feedback is enabled | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/media/handlers.go` | Unauthenticated, with per-address limits and a global limit on the two previews | User, server and file IDs; invite and friend codes | |
| Media upload and image processing | `services/control-plane/internal/media/handlers.go`, `services/control-plane/internal/media/processing.go`, `services/control-plane/internal/media/upload_session.go` | Authenticated | Image files (PNG, JPEG, GIF, WebP), chunked bodies | |
| GIF provider proxy under `/api/v1/klipy` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/klipy/handlers.go` | Authenticated, with per-user limits. Registered only when `KLIPY_API_KEY` is set; `docker-compose.yml` passes it through and it is empty unless the operator sets it | Search terms and identifiers that the server forwards to the provider; on the media route, a full URL that the server then fetches. The fetch accepts HTTPS and the provider's own domains only, applies the same rule to every redirect, and refuses to connect to an address that is not public | Off by default |
| Redemption-code routes: `/api/v1/redeem` and `/api/v1/admin/redemption/codes` | `services/control-plane/internal/api/router.go`, `services/control-plane/internal/redemption/handler.go` | Authenticated in every build; redeem has per-user and per-address limits. Code generation also needs a static shared token, compared in constant time. It refuses every request while `REDEMPTION_ADMIN_TOKEN` is empty, and no compose file in this repository passes that variable to the control plane | Code strings; generation parameters | Yes: built for paid plans; a self-hosted instance already resolves every user to the top entitlement tier (`services/control-plane/internal/entitlements/cache.go`) |
| Update asset download under `/api/v1/updates/` | `services/control-plane/internal/updates/handler.go` | Unauthenticated | File name in the path | Yes, if no supported client reads it: the desktop client takes its update feed from a fixed release address |
| Billing webhook route | `services/control-plane/internal/api/router.go` | Unauthenticated; registered only when billing is configured | Signed webhook body | Yes: not used by self-host |
| Admin console under `/admin` | `services/control-plane/internal/admin/routes.go` | Unauthenticated for sign-in and enrollment; admin session for the rest | Passwords, WebAuthn assertions, enrollment tokens, action bodies | Off by default |
| Host agent unix socket | `services/control-plane/internal/hostagent/server.go`, `services/control-plane/internal/hostagent/jobs.go` | Privileged: the `concord-agent` group, and the control-plane container when `docker-compose.host-agent.yml` is added | Action name, tag, archive name, log query | |
| Setup wizard on TCP 9443 | `services/control-plane/internal/hostagent/setup.go`, `services/control-plane/cmd/concord-agent/main.go` | Unauthenticated page load on all interfaces; bearer token for every API call | Install answers: domain, address, SMTP, storage | Closes after install or 60 minutes |
| Media-plane Socket.IO | `services/media-plane/src/index.ts`, `services/media-plane/src/middleware/auth.ts`, `services/media-plane/src/lib/originGate.ts` | Authenticated by access token | Signalling events, RTP parameters, `Origin` | |
| Media-plane RTP, UDP 40000 to 41999 | `docker-compose.selfhost.yml` | Unauthenticated packets; ICE and DTLS gate a session | RTP, RTCP, STUN, DTLS | |
| TURN and STUN: TCP and UDP 3478, TLS 5349, relay UDP range | `docker-compose.yml`, `infrastructure/docker/coturn/turnserver.conf`, `services/control-plane/pkg/config/turn.go` | STUN unauthenticated; TURN needs a time-limited credential issued to users | Allocations and peer addresses | |
| NATS bus | `infrastructure/docker/nats/nats-cluster.conf`, `services/media-plane/src/lib/nats.ts` | Any process on the services bridge network | Voice lifecycle events, enforcement commands | |
| PostgreSQL, Redis, NATS host ports in the base file | `docker-compose.yml`, `docker-compose.selfhost.yml` | Published by the base file; removed by the self-host overlay | Native protocols | Yes: the safe posture depends on an overlay |
| `pgadmin` and `redis-commander` (`tools` profile) | `docker-compose.yml` | Developer only | Web UI | Yes: unpinned images in the production base file |
| `ops-agent`, which mounts the Docker socket and the host `/proc` and `/sys` | `docker-compose.yml`, `docker-compose.production.yml` | Not started in the self-host posture: the production overlay moves it to an inactive profile | Host and container metrics | Yes: defined in the base file |
| Installer, update, backup and restore | `infrastructure/deploy/install-selfhost.sh`, `infrastructure/deploy/selfhost-update.sh`, `infrastructure/deploy/selfhost-backup.sh` | Privileged: host root, or an admin through the host agent | Answers file, release tag, backup archive | |
| BYO object storage | `docker-compose.byo-s3.yml`, `services/control-plane/internal/storage/client.go` | Privileged configuration; then the storage vendor | Endpoint, credentials, returned objects | |
| CI and release workflows | `.github/workflows/release.yml`, `.github/workflows/labeler.yml`, `.github/workflows/workflow-validate.yml` | Build-time; fork pull requests reach the two `pull_request_target` workflows | PR content, tags, dependency updates | |
| Synced agent tooling: skills, agents and hooks under `.claude/`, proposed by a scheduled pull request | `.github/workflows/sync-tooling.yml`, `CODEOWNERS` | Build-time. The installer runs in a job that holds no write credential. The draft pull request may change only `.claude/` and `scripts/repo-config.sh`, and `CODEOWNERS` names the security team as owner of both. Otherwise ordinary pull-request review only | Tooling files from the upstream tooling repository | |

## Trust boundaries

| Boundary | Actors | What crosses | Control | Proof |
| --- | --- | --- | --- | --- |
| Internet to edge | Any client, nginx | HTTP, WebSocket upgrades | TLS, host and SNI checks, per-address request limits, body size caps | `infrastructure/deploy/nginx/concordvoice.conf` |
| Edge to application | nginx, control plane, media plane | Client address attribution | Services bound to loopback; trusted proxy pinned to the measured bridge gateway | `docker-compose.selfhost.yml`, `infrastructure/deploy/selfhost-proxy.sh` |
| Anonymous to user | Client, control plane | Session authority | On each request: access-token class and issuer check, token blacklist, live account state, credential-epoch fence, server-side login-family authority. At sign-in: per-route rate limits and an account lockout | `services/control-plane/internal/middleware/auth.go`, `services/control-plane/internal/loginfamily`, `services/control-plane/internal/api/router.go`, `services/control-plane/internal/auth/login_lockout.go` |
| User to community authority | Member, control plane | Permission to read, write, moderate | Membership and permission middleware; a role guard and an override gate inside the write transaction; a channel must belong to the server it is authorized against | `services/control-plane/internal/rbac/middleware.go`, `services/control-plane/internal/rbac/role_guard.go`, `services/control-plane/internal/rbac/override_gates.go`, `services/control-plane/internal/rbac/resolver.go` |
| User to admin | Admin console user | Platform control | Separate cookie session, never the user token; deny-by-default route list; WebAuthn step-up for the destructive actions and for creating an admin | `services/control-plane/internal/admin/routes.go`, `services/control-plane/internal/admin/server_writes.go`, `services/control-plane/internal/admin/create_admin_stepup.go` |
| Control plane to host root | Control-plane container, host agent | Fixed management actions | Socket group permission; closed action list; arguments validated by the agent; ownership check of the checkout before each run. Each write also needs a WebAuthn assertion from an admin's hardware key, verified by the agent against a root-owned key file; the agent refuses every write until a root-owned alert destination is configured, and reports each admitted write there; `update` refuses an older tag | `services/control-plane/internal/hostagent/runner.go`, `services/control-plane/internal/hostagent/jobs.go`, `services/control-plane/internal/hostagent/alert.go`, `services/control-plane/internal/hostagent/version.go`, `docker-compose.host-agent.yml` |
| LAN to uninstalled host | Browser, setup wizard | Install answers; first control of the server | One-time 256-bit token, written to a mode 0600 file and compared in constant time; self-signed TLS whose fingerprint the agent prints to its journal; unknown fields refused | `services/control-plane/internal/hostagent/setup.go`, `services/control-plane/cmd/concord-agent/main.go` |
| Media plane to control plane | Two services | Voice admission, enforcement, release | An HMAC-SHA-256 proof under a per-purpose key derived from `JWT_SECRET`, compared in constant time, with a timestamp inside 30 seconds. Hops that carry a user's bearer token (voice join and authorize, connection register, room register) pass user authentication first, and their proof also binds the token's digest. The release, provisional-close, room-release, health and retire-boot hops carry no bearer; their proof binds the path, the body digest, the media node's boot ID and a nonce. Room release also accepts an older message form, on the same exact path, kept for older media-plane builds | `services/control-plane/pkg/mediaproof/mediaproof.go`, `services/control-plane/internal/voiceauthprotocol/http.go`, `services/control-plane/internal/voiceauthprotocol/proof.go`, `services/control-plane/internal/middleware/service_hop.go`, `services/control-plane/internal/api/voice_enforcement_sessions.go` |
| Service bus | Control plane, media plane | Voice lifecycle events and commands | Network membership; signatures on some commands | `infrastructure/docker/nats/nats-cluster.conf` |
| Client to server, for content | User device, server | Ciphertext, wrapped keys, public keys | Client-side encryption; the server never receives an unwrapped key; the client library refuses a served RSA public key shorter than 4096 bits | `packages/e2ee-client/src/renderer/utils/crypto/crypto.ts`, `docs/adr/0003-e2ee-always.md` |
| Client build to server | Client application, control plane | The platform and version that the client states; an attestation token | Both checks exist and both are off by default. A per-platform minimum version (`CLIENT_MIN_VERSION` and its mobile forms) is applied on authenticated routes and at control WebSocket admission. A client-attestation gate (`REQUIRE_CLIENT_ATTESTATION`) requires an attestation token on authenticated routes; iOS App Attest verification has its own setting. `docker-compose.production.yml` passes the version floors, empty unless the operator sets them; no compose file in this repository sets the attestation variables | `services/control-plane/internal/middleware/client_version.go`, `services/control-plane/internal/middleware/attestation.go`, `services/control-plane/internal/attestation/verify_handler.go`, `services/control-plane/internal/attestation/ios_handler.go` |
| Authority to everything else | Authority service, its database | Account-key records | Own database on unix sockets only, internal network, read-only container, all capabilities dropped, secrets as files owned by their reader | `docker-compose.authority.yml` |
| TURN relay to private networks | TURN client, coturn | Relayed packets | An explicit list of denied peer ranges (loopback, private, link-local and other listed special-purpose ranges); no multicast peers; shared-secret credentials; quota | `infrastructure/docker/coturn/turnserver.conf` |
| Upstream to operator host | Release, operator | New server code | SSH-signed tag checked against the signer list; the signed tag must name the requested release; every tag is refused while the list is empty | `infrastructure/deploy/selfhost-update.sh` |
| Source to release | Contributors, CI | Bundle contents | Tag must match `VERSION` on the default branch; attestation; SBOM check; actions pinned by commit | `.github/workflows/release.yml`, `docs/runbooks/selfhost-release.md` |

## Security objectives

A change must keep each of these. Each is written so that a reviewer can test it.

1. Every `/api/v1` route that `docs/api/openapi.yaml` does not mark as needing no credential rejects a request with no valid access token or service proof. `scripts/api/check-openapi-coverage.sh` keeps that file in step with the router. Every `/admin` route outside the pre-auth list in `services/control-plane/internal/admin/routes.go` rejects a request with no admin session.
2. Every route under `/api/v1/internal/` rejects a request with no valid proof, before any state change, whatever the caller's network position.
3. The control plane never receives, stores or logs an unwrapped private key, a channel or DM key, or message plaintext. Private key material reaches it only as an opaque, size-bounded envelope (`services/control-plane/internal/auth/registration_key_material.go`) that the client wrapped (`packages/e2ee-client/src/renderer/utils/crypto/crypto.ts`).
4. A token that is not of the access class never authenticates as a user: on HTTP, on the control WebSocket, or on the media plane.
5. A password change, key replacement or recovery reset revokes every refresh token of the account inside its own transaction, under the credential-epoch fence, before the request reports success (`services/control-plane/internal/users/handlers.go`, `services/control-plane/internal/auth/handlers.go`, `services/control-plane/internal/loginfamily`).
6. Role and override writes are authorized against the actor's own authority before they are applied. By design the server owner and, for override writes, an Administrator are exempt from parts of this rule (`services/control-plane/internal/rbac/role_guard.go`, `services/control-plane/internal/rbac/override_gates.go`).
7. A resource ID from one community server never yields authority or data in another. The permission resolver refuses a channel that is not in the named server (`services/control-plane/internal/rbac/resolver.go`), a role assignment must name a role of the same server (`services/control-plane/migrations/000144_member_roles_server_composite_fk.up.sql`), and `services/control-plane/internal/rbac/cross_server_row_test.go` pins both.
8. The host agent runs only its fixed actions, with arguments it validated itself, and only from a checkout that is root-owned and not writable by group or others. It runs a write action only after it has verified a WebAuthn assertion bound to that action and its arguments, and only while an alert destination is configured.
9. The setup wizard performs no action without the one-time token, and cannot change an installed server.
10. In the self-host posture, PostgreSQL, Redis and NATS publish no host port, and the control-plane and media-plane signalling ports bind to loopback only (`infrastructure/deploy/selfhost-port-posture.py`).
11. The installer generates every secret from a CSPRNG with 256 bits and writes `.env` with mode 0600. A production start refuses a default or missing secret (`services/control-plane/pkg/config/config.go`, `docker-compose.production.yml`).
12. `concord-selfhost update` changes code only to a tag signed by a key in `infrastructure/deploy/release-signers`.
13. TURN never relays to an address inside a range denied in `infrastructure/docker/coturn/turnserver.conf`, and that list covers every loopback, private and link-local range.
14. Account erasure deletes the account and de-authorizes its tokens before it reports success (`services/control-plane/internal/privacy/handler.go`).
15. No log line carries a token, key, password, password hash or message content. For the auth package a test enforces the token part: `services/control-plane/internal/auth/log_emissions_test.go` fails on a log field that carries a token-derived value.

## Attack paths

Each row is a hypothesis to test, not a finding. Rows are ranked by what a failure grants. Tags are bare identifiers; the legend is in "Standards alignment".

| # | Attacker goal | Path through the surface | Boundary that must hold | Existing control | A failure grants | Tags |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Root on the operator's host | An admin session or code execution in the control plane, then the host agent socket, then a crafted tag, archive name or log query | Control plane to host root | Closed action list, agent-side validation, signed tags only, downgrade refusal, root-only backups directory; a hardware-key assertion per write, verified by the agent; an out-of-band alert for each admitted write | Host root | A01, A05, A08; V8, V15; PR |
| 2 | Own the server before the operator does | Reach TCP 9443 in the setup window; obtain the token or intercept the first TLS connection | LAN to uninstalled host | Token in a root-only file, fingerprint check, 60-minute window, one install | Attacker-chosen domain, SMTP and storage; first control | A02, A07; V6, V12; PR |
| 3 | Act as any user | Obtain `JWT_SECRET` from `.env`, a backup, or either service's environment; mint tokens and proofs | Anonymous to user; media plane to control plane | File modes, generated secrets, token class checks | Session authority for every account, and every service proof | A04, A07; V9, V11, V13; PR |
| 4 | Read user content, as the operator or after server compromise | Capture a password at sign-in, or substitute a public key in a lookup | Client to server, for content | Client-side encryption; the dormant account-key authority | Plaintext of that user's channels and DMs from then on | A04, A06; V11, V14; GV |
| 5 | Forge a service-to-service call from the internet | Send a request to a route under `/api/v1/internal/` through nginx | Media plane to control plane | Handler-level HMAC or OIDC check; body cap | Voice session release, enforcement state change, or a forged release record | A01, A06; V4, V8; PR |
| 6 | Cross a community boundary | Present IDs from server A on a route authorized against server B; race a role write | User to community authority | Channel-in-server check, in-transaction role guard, composite foreign key on role assignments | Another community's metadata, ciphertext and wrapped keys | A01; V8; PR |
| 7 | Take over an account without the password | Guess a recovery code, confuse a recovery ceremony, misuse an MFA challenge, replay a refresh token | Anonymous to user | A per-account recovery failure budget and epoch-bound token issue (`services/control-plane/internal/auth/handlers.go`); refresh-token theft checks that queue a revocation of the account's sessions (`services/control-plane/internal/auth/token_theft_recovery.go`) and are repeated at WebSocket registration (`services/control-plane/internal/websocket/handler.go`) | One account, including its wrapped key | A07; V6, V7; PR, DE |
| 8 | Falsify voice state | Publish forged lifecycle events on NATS from any process on the services network | Service bus | Network membership; payload size bounds on the subscriber (`services/control-plane/internal/voice/ingress_gate.go`) | False voice state, ejected participants, key-epoch churn | A01, A08; V4, V17; PR |
| 9 | Reach the operator's private network | Use a valid TURN credential to ask for a relay toward an internal address | TURN relay to private networks | Denied peer ranges, quota | Reach or scan of internal services | A02, A06; V13, V17; PR |
| 10 | Exhaust the host | Registration and upload floods, large image decodes, WebSocket floods, RTP floods, TURN allocations | Internet to edge | nginx limits, per-route limits, size caps, media policer, TURN quota | Outage of chat and voice for all users | A06, A10; V2, V5, V17; DE, RS |
| 11 | Ship code to every operator | Compromise a dependency, a base image, a workflow input or the default branch | Source to release; upstream to operator host | Pinned digests and action commits, scanners, attestation, signed tags | Code execution in containers, or as root through the update path | A03, A08; V15; ID, PR |
| 12 | Read data at rest off the host | Obtain a backup archive, the database volume or the object store | Host file permissions; the storage vendor | Mode 0600 archive in a 0700 directory; client-side encryption of content | All secrets and metadata; ciphertext; wrapped keys for offline guessing | A02, A04; V13, V14; PR, RC |
| 13 | Hide the attack | Act through paths that write no audit row or security event | Logging | Admin audit log (`services/control-plane/internal/admin/audit.go`); security events (`services/control-plane/internal/securityevent`) | Undetected dwell time | A09; V16; DE |
| 14 | Steer an AI agent that works in this repository | Plant instructions in content the agent reads (a pull request, an issue, a dependency file), or change the agent tooling under `.claude/` through the tooling-sync pull request | Source to release | The sync patch may change only `.claude/` and `scripts/repo-config.sh`; `CODEOWNERS` names the security team as owner of both; otherwise ordinary pull-request review only | A weakening change proposed under a maintainer's or a bot's name | A03, A08; V15; PR; LLM01, LLM06 |

## Weakest ground

Ranked. Scans and reviews start here.

1. **One shared secret roots user and service identity.** `JWT_SECRET` signs access tokens and MFA challenges, and every service-to-service HMAC key is derived from it. Both application services hold it. A read of either container's environment, of `.env` or of one backup is total compromise of authentication, with no second layer.
2. **Content confidentiality rests on the server it is meant to exclude.** The account password both authenticates to the control plane and unwraps the identity key, and the control plane is the only source of peers' public keys. The second layer, the account-key authority, ships dormant.
3. **Internal routes are authorized inside each handler.** Routes under `/api/v1/internal/` sit on the unauthenticated router group, and the reverse proxy forwards the whole `/api/` prefix. One new handler without the proof check is exposed to the internet. The router file carries a warning to that effect. The routes also carry two proof formats, one kept for older media-plane builds.
4. **Root-level automation is driven from a web console.** The path from an admin browser session to `concord-selfhost` as root is one chain. The agent verifies a hardware-key assertion for each write, but an admin uses the same key to sign in and to approve an action, and the authenticator cannot show what it signs. A compromised control plane can therefore present a sign-in prompt and use the tap as approval for a write. The required out-of-band alert detects this; it does not prevent it. Reads need only the socket group, and the agent trusts the whole checkout.
5. **The escalation rule is restated per write path.** The role guard checks role writes (`services/control-plane/internal/rbac/role_guard.go`) and the override gate checks override writes (`services/control-plane/internal/rbac/override_gates.go`); effective permissions are resolved in a third place (`services/control-plane/internal/rbac/resolver.go`). No shared function or type makes the paths agree, so they can drift.
6. **The service bus trusts network membership.** NATS in this stack has no client authentication. Any process on the services bridge can publish and subscribe, and coturn, which faces the internet, is attached to that bridge (`docker-compose.yml`).
7. **First-run setup is a race on the local network.** A listener on all interfaces, a self-signed certificate and a human fingerprint comparison stand between the LAN and first control of the server.
8. **Secrets at rest are plain files.** `.env` and every backup archive hold all secrets in clear, protected by file mode only. A backup also carries the database beside them.
9. **Parsers of complex untrusted input.** Image decoding and re-encoding in the control plane; RTP, RTCP, DTLS and codec parameters in mediasoup; TURN in coturn; JSON and WebSocket frames on every route.
10. **The safe posture is an overlay.** The base compose file publishes data-service ports on all interfaces, defines an `ops-agent` service that mounts the Docker socket and the host `/proc` and `/sys`, defaults the instance type to the mode that is not self-hosted, and carries defaults that suit another deployment (TURN realm, SMTP host and sender). The production and self-host overlays and a posture gate correct these. A start that skips one of them is exposed.
11. **Surface built for the hosted service ships in every self-host build.** Billing, SSO, release attestation, client attestation, feedback forwarding, redemption codes, the GIF provider proxy and blue-green rails are compiled in and gated by configuration or by the instance type. Each release widens this set.
12. **Long-lived, broad credentials.** The bundled object store is reached with its root credentials. A BYO S3 key is static. This repository automates no rotation of `TURN_SECRET` or `JWT_SECRET`.
13. **Three readers of one token.** The HTTP middleware, the control WebSocket and the media plane each validate the access token, in two languages. Tests and review keep them in step; nothing structural does.

Method: OWASP WSTG for the HTTP and WebSocket API; a CycloneDX SBOM diff per release for item 11 and for dependencies; an external port inventory for item 10.

## Hardening and surface reduction

Ranked. Kinds: remove surface (R), narrow surface (N), add a second layer (L).

| # | Action | Kind | Answers |
| --- | --- | --- | --- |
| 1 | Give service-to-service proofs their own secret, separate from the token-signing secret; move token signing to an asymmetric key so the media plane can verify but not mint | L | Weakest ground 1 |
| 2 | Activate the account-key authority and its client integration, so a client can detect a substituted key; separate the sign-in credential from the key-wrapping key, as `docs/adr/0051-nonmedia-e2ee-context-and-credential-domains.md` proposes (status Proposed, not shipped) | L | Weakest ground 2 |
| 3 | Put internal routes behind one shared middleware, on a listener or path that nginx does not forward; deny `/api/v1/internal/` at the edge; retire the older proof format when no supported media-plane build sends it | R, N, L | Weakest ground 3 |
| 4 | Express the escalation subset rule once: one shared function that every conferring role or override write must call, plus a test that enumerates those writes | L | Weakest ground 5 |
| 5 | Require client authentication and per-subject permissions on NATS; sign lifecycle events | L | Weakest ground 6 |
| 6 | Leave hosted-only features out of the self-host build, or do not register their routes when `INSTANCE_TYPE` is `self-hosted` | R | Weakest ground 11 |
| 7 | Move the `tools` profile services, `ops-agent`, the published data ports and the deployment-specific defaults out of the base compose file; default the instance type to self-hosted in the base file too | R | Weakest ground 10 |
| 8 | Default the setup listener to loopback or a named address (`CONCORD_AGENT_SETUP_ADDR` already selects it), with all interfaces as an explicit choice; offer an install path that opens no listener | N | Weakest ground 7 |
| 9 | Encrypt backup archives to an operator key; deliver secrets as per-service files, as the authority overlay already does | L | Weakest ground 8 |
| 10 | Done for writes (#165): the agent verifies a WebAuthn assertion bound to each write and requires an out-of-band alert. Remaining: a confirmation outside the browser (for example, approving the action in the alert channel) so a harvested sign-in tap cannot become an approval | L | Weakest ground 4 |
| 11 | Run image processing in a separate, resource-capped process that holds no secret | L | Weakest ground 9 |
| 12 | Use a bucket-scoped object-store credential in place of the root account | N | Weakest ground 12 |
| 13 | Document and script rotation for `JWT_SECRET`, `TURN_SECRET` and storage keys | L | Weakest ground 12 |
| 14 | Test all three token readers against one shared vector file | L | Weakest ground 13 |
| 15 | Remove the bearer-token fallback on the control WebSocket when no supported client needs it | R | Weakest ground 13 |

## Assumptions and unknowns

Deployment prerequisites. This model assumes:

- The operator controls the host, its firewall, DNS and TLS certificates, and keeps the checkout root-owned.
- The host runs the Docker Engine and Compose versions that `docker-compose.selfhost.yml` names as its floor. Older versions merge the port lists instead of replacing them.
- A host firewall does not filter Docker-published ports by default. The operator verifies exposure from outside the host.
- Clients are the official desktop and mobile applications. Client-side threats belong to those repositories' models.
- Email delivery is a third party that the operator chooses. Whoever controls a user's mailbox can start account recovery.
- With BYO object storage, the storage vendor holds the ciphertext and sees object sizes, counts and timing.
- An operator behind a CDN or proxy configures origin authentication before trusting a forwarded client address.

Exclusions: the hosted service's deployment and operations; physical security of the host; third-party identity providers, because the SSO routes refuse every request on an instance that the self-host overlay marks self-hosted (`docker-compose.selfhost.yml`).

## Standards alignment

All frameworks below are alignment targets. None is a claim of compliance or certification.

Tag legend: `A01` to `A10` are OWASP Top 10:2025 categories; `V1` to `V17` are OWASP ASVS 5.0 chapters; `GV`, `ID`, `PR`, `DE`, `RS` and `RC` are NIST CSF 2.0 Functions; `LLM01` to `LLM10` are entries of the OWASP Top 10 for LLM Applications 2025.

| Framework | What it prioritises in this repository |
| --- | --- |
| OWASP Top 10:2025 | A01 for community and admin authorization; A02 for compose and proxy defaults; A03 and A08 for release and update integrity; A04 for the key hierarchy |
| OWASP ASVS 5.0 | V6, V7 and V9 for accounts, sessions and tokens; V8 for RBAC; V11 and V14 for keys and data; V13 for configuration; V17 for the media plane and TURN |
| OWASP Top 10 for LLM Applications 2025 | LLM01 and LLM06 for AI agents and synced agent tooling that work in this repository; build-time surface only |
| OWASP Cheat Sheet Series | Reference for session, password storage, file upload and container hardening choices |
| NIST CSF 2.0 | PR for the controls above; DE and RS for audit and security events; RC for backup and restore; GV for the operator's role as controller |
| NIST SP 800-63-4 | Authenticator and recovery strength for user and admin accounts |
| CIS Critical Security Controls | Secure configuration of the host and containers; inventory of published ports |
| GDPR, and the regimes that follow its model (UK GDPR and DUA Act, CCPA and CPRA, VCDPA, TDPSA, CTDPA, RIDTPPA, PIPEDA and Quebec Law 25, Australia Privacy Act 1988, Mexico federal law, COPPA) | The operator is the controller. Drives a lawful basis, minimisation of metadata, erasure, breach notice by the operator, and children's data: per the published privacy policy users must be at least 16, and registration refuses a request without the age confirmation (`services/control-plane/internal/auth/handlers.go`) |
| HIPAA | A hardening target only. No BAA is offered and no compliance is claimed |
