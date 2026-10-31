# Design of Record: Self-Hosted Services and Unified Client Connections

**Record date:** 2026-10-08 (America/New_York)

**Status:** Requirements recorded; option A is the working architecture; implementation pending.

**Tracking:** Epic #1615. Client acceptance informs #2505; public distribution remains #2506.

**Source assessment:** `9f282b854df3e5bc5c4a3e156db6a65320fd71e5`.

This is the design of record for the new multi-service client requirements discussed
on 2026-10-08. It preserves the client assessment and its reproduction evidence,
defines the intended behavior, and identifies implementation and acceptance work.
Requirements below describe the target. They are not claims that the software
already implements these features. The earlier [deployment dossier](README.md)
remains background for installation and the original client slice.

## 1. Terminology and working decision

| Term | Meaning |
| --- | --- |
| **CVNet** | The Concord Voice network, with its own accounts and communities. |
| **Self-Hosted Service** | An independently operated, self-sufficient Concord Voice deployment and identity domain. Replaces “self-hosted server” when referring to the complete deployment. |
| **Server** | A community inside CVNet or a Self-Hosted Service. It owns channels and memberships within that service. |
| **Connection** | A device's approved endpoint and account session for one service. |
| **Service invite** | A one-time admission secret issued by a service owner/admin for joining that service. |
| **Server invite** | An invitation to a community within an already identified service. |
| **Friend code** | A code identifying a person within an already identified service. |

**Working decision: option A.** Each Self-Hosted Service has its own accounts,
servers, channels, DMs, group conversations, permissions and data. Option B
(one community with no DMs) and option C (multiple communities with no DMs) are
excluded from this baseline. This follows the maintainer's stated preference for A.

The desktop presents one unified interface. It does not require an environment
pivot to see another service's communities or conversations. Presentation is
shared; authentication, network operations, encryption and authority are scoped.
CVNet SSO authenticates a local service account; it does not federate messages,
friend lists, memberships or moderation powers.

## 2. Preserved findings from the current client

| ID | Finding at the assessed source | Consequence |
| --- | --- | --- |
| F01 | Self-Hosted at login probes discovery endpoints and reaches the real login/registration forms. | Basic connection is implemented; this is no longer a placeholder. |
| F02 | Add Server and Join Server use invite codes on the selected runtime API base. | There is no service-connection manager or multi-service code routing. |
| F03 | There is no flow to establish a CVNet identity while preserving an authenticated self-hosted connection. | “Join Concord Voice” must establish a separate connection. |
| F04 | Approved origins have separate token, E2EE-key and machine-ID files. The renderer and credential manager still use an active-instance model. | File separation is useful groundwork; simultaneous connection isolation is not established. |
| F05 | Remember Me has a last-active-profile mechanism; there is no saved connection library/picker. | Users cannot select among remembered service/account profiles as required here. |
| F06 | Encrypted preferences currently contain appearance/layout data. | No encrypted connection vault or device restore/start-fresh policy exists. |
| F07 | OAuth providers are Google and Apple; deployment middleware blocks their routes on self-hosted deployments. | CVNet-as-SSO requires a new provider and a distinct service-only admission policy. |
| F08 | Main session restore returns credentials without the restored service origin; the renderer starts with its build-configured default origin. | A cold self-hosted restore can publish its credential while renderer hydration still targets CVNet. |

Relevant source boundaries are AuthFlow,
JoinServerModal,
runtimeServerBase,
selfHostedProfile,
main restore IPC,
renderer restore,
preferencesSync,
and [OAuth deployment policy](../../../services/control-plane/internal/api/oauth_wiring.go).
The assessment concerns these source paths; it did not establish live service behavior.

### 2.1 Reproduction evidence for F08

The retained receipt records a closed
local reproduction using the unchanged main restore handler, renderer restore
function and runtime-base module. Storage, refresh transport, key access and
hydration effects were owned stubs. No network request or real credential-store
access occurred. Source hashes are retained in the receipt.

| Control | Refresh owner | Renderer hydration owner |
| --- | --- | --- |
| Cold remembered self-hosted session | Self-hosted service | CVNet default: defect reproduced |
| Self-hosted origin selected before restore | Self-hosted service | Same self-hosted service |
| Cold CVNet session | CVNet | CVNet |

The reproduction script takes an owned
source snapshot directory as its argument. Prepare it from the assessed commit:

```bash
audit_root=$(mktemp -d /tmp/concord-restore-record.XXXXXX)
git archive 9f282b854df3e5bc5c4a3e156db6a65320fd71e5 client/desktop/src | tar -x -C "$audit_root"
git rev-parse 9f282b854df3e5bc5c4a3e156db6a65320fd71e5 > "$audit_root/audited-main-sha.txt"
node docs/design/self-hosted-deployment/evidence/reproduce-session-restore.mjs "$audit_root"
```

Successful reproduction means the historical defect was observed with its
controls. It is not a passing acceptance test for the intended behavior.

**Required repair:** validate and establish the restored connection context,
including its approved origin, before publishing credentials, unlocking its
content or performing hydration. A failed or superseded restore must not fall
back to another connection's origin, credentials or keys.

### 2.2 Original requirements and subsequent refinements

The original conversation requested a functional pre-login Self-Hosted option,
post-login service joining from CVNet, a Join Concord Voice action from a
self-hosted account, strict trust separation, optional E2EE connection/account
backup, optional operator-controlled CVNet SSO, and a locally remembered
self-hosted profile list with Remember Me. All remain represented in this record.

| Original requirement | Refinement recorded here |
| --- | --- |
| Add Server exposes CVNet / Self-Hosted pills and a URL entry. | Service setup moves to Settings > Connections. Community/friend actions retain an explicit owning-service choice. This supersedes the original placement, not the ability to connect. |
| Self-hosted login shows remembered profiles. | The same Connections dialog is shared with login and shows service/account status without requiring CVNet. |
| Join Concord Voice starts account creation. | Connections offers existing-account sign-in and normal CVNet account creation without ending the self-hosted session. |
| Self-hosted connections are sandboxed and unaware of unrelated connections. | Independent service/account contexts back unified DM/server presentation; SSO and opted-in encrypted backup are explicit scoped bridges. |
| Connections/account details can be encrypted and saved to CVNet. | A dedicated connection vault adds Restore Existing Connections versus Start Fresh, with independent local-only device state and no destructive default writes. |
| CVNet SSO can automatically establish a service account. | Consent and a one-time service invite gate creation; an existing identity in that creation flow signs in and still consumes the invite. Ordinary restore uses existing-account sign-in. |

**Scope clarification, 2026-10-08:** the maintainer requested preservation of the
original findings first, followed by recording/refining the additional design.
No implementation is requested for this documentation turn. Future changes must
retain the original assessment as historical evidence and record any superseding
decision explicitly rather than silently replacing it.

## 3. Service and account ownership

Every operation has an explicit connection owner. Local identifiers are namespaced
by service and account before they enter a shared view, cache, React key, notification,
pending request or event dispatcher. Two services returning the same user, server,
channel or conversation ID must remain distinct.

The service advertises a stable service identity and its protocol/capability
descriptor. The client binds that identity to an approved canonical endpoint;
identity or display-name claims alone do not grant trust. CVNet identity is
recognized through the client's existing trusted CVNet configuration, not a
self-hosted response claiming to be CVNet.

Origins include scheme, host and effective port. Explicit default ports normalize
to the same origin. Different ports are different endpoint approvals. An endpoint
change requires an explicit verified migration and user approval; it cannot
inherit another endpoint's credentials because their names or advertised IDs match.

Each service governs its local accounts, community permissions, invitations,
moderation and registration policy. A CVNet account or SSO assertion does not
give a user operator/admin privileges on a Self-Hosted Service. Existing local
account bans and disabling remain effective when another valid invite is presented.

## 4. Isolation under one client shell

The trusted client shell owns connection management and user consent. Each
connection context owns its API routing, token/session lifecycle, E2EE custody,
cookies/session partition, caches, pending requests, sockets and reconnect state.
The unified shell consumes scoped presentation models rather than a mutable
global bearer token or encryption-key selection.

Privileged network and storage operations enforce the connection owner at the
main/preload boundary. Changing a visible community must not authorize requests
using another service's credentials. Responses and asynchronous continuations
remain bound to their initiating connection, account and credential generation.
Logging out, losing connectivity or receiving a refusal on one connection affects
that connection. It does not reset other services or redirect their traffic.

Per-service isolated execution and Chromium session partitions must be selected
and verified during implementation. A hashed credential directory or a generic
Electron sandbox flag alone is not evidence that this contract is satisfied.
The pinned application userData root remains intact; migration of existing
single-profile storage must be explicit, non-destructive and tested.

Self-hosted content is untrusted data. It cannot supply client executable code,
an update feed, arbitrary privileged IPC calls, or another service's configuration.
Message rendering and external-media handling validate and bound their inputs.
Mic, camera and screen-share activation remain user-controlled. Moderation can
affect that service's session without changing another connection or global
security settings. Logs, telemetry and update requests must not carry service
URLs, profile labels, credentials, invitation secrets or conversation contents.

### 4.1 Ports and shared system resources

CVNet, service A and service B can all expose the same remote port. Independent
outbound sockets use OS-assigned local ports. Client-owned listeners, including
OAuth loopback callbacks, bind an available loopback port and bind state/nonce,
PKCE and callback ownership to the initiating connection. No shared fixed callback
port may overwrite another in-flight ceremony.

WebSockets, HTTP pools, media transports and reconnect budgets are scoped.
The trusted client arbitrates physical microphones, cameras, screen capture,
audio output and notification delivery. A service cannot steal or silently start
another service's capture. Multiple concurrent voice publications require an
explicit supported user action; they are not implied by connecting services.
Bound background work and resource usage per connection and for the whole client.

## 5. Connections UI and connection lifecycle

**Settings > Connections** opens the connection-management dialog. It displays
the CVNet account when present, approved Self-Hosted Services and their accounts,
connection status, local remember settings, account backup/sync choices, and
service-specific privacy/security controls. It owns adding a service; Add Server
continues to mean creating or joining a community inside a service.

The login **Self-Hosted** option opens the same manager in self-hosted login mode.
It lists locally remembered connections without requiring a CVNet account and
offers **New Connection**. An authenticated self-hosted user can explicitly
choose **Join Concord Voice**, then sign in to an existing CVNet account or
create one through the normal CVNet flow. This adds a connection without ending
the self-hosted session. No implicit CVNet account creation occurs.

For a new service connection:

1. Enter an IP address or domain, including a local-only name, and an optional port.
2. Normalize and approve the endpoint; validate TLS and address-class policy.
3. Perform bounded discovery and compatibility checks without sending account credentials.
4. Show sign-in or account creation for that exact service and its declared policy.
5. Complete local auth or explicitly consented CVNet SSO, with service admission when required.
6. Establish the scoped connection and render its communities/conversations.
7. Offer independent local remembering and encrypted account backup choices.

The address field supplies examples such as `voice.example.com`, `voice.home.arpa`,
`192.168.1.20` and `[fd00::20]`. HTTPS is the production default and the port field
defaults to **443**, the public reverse-proxy endpoint, rather than assuming the
internal control-plane port. An explicit URL port populates the port field;
conflicting values are rejected instead of silently selecting a destination.
The existing explicit HTTP-loopback development exception remains scoped.
Non-loopback HTTP, embedded credentials, unsafe schemes and reserved/metadata
addresses remain refused. Private-address approval is endpoint/device scoped;
there is no blanket LAN or certificate-verification bypass.

Selecting a remembered profile first checks reachability after required device
approval. If a valid locally remembered credential and its E2EE custody are
available, authenticate that existing account; otherwise show sign-in/unlock.
Remember Me does not bypass revocation, MFA, TLS checks or E2EE unlock requirements.

## 6. Service admission and registration policy

### 6.1 One-time service invites

New service membership/account creation requires a **service invite**, independently
of server invites and friend codes. The service owner or an explicitly authorized
service administrator issues, expires and revokes these codes. Community-admin
permissions alone do not grant service-invite authority. Owner bootstrap needs
an operator-controlled path; an anonymous first registrant cannot claim ownership.

Service invite secrets are CSPRNG-generated with at least 128 bits of entropy,
stored as digests, rate limited, and bound to the issuing service and invite kind.
They are one-time use. Expiry and revocation are enforced server-side, and secrets
do not appear in URLs, telemetry, referrers or logs. Discovery is not admission.

Successful account creation/admission and invite consumption are atomic.
Concurrent redemption permits one winner. For SSO, validate and reserve the
invite before redirecting; a bounded reservation belongs to that ceremony.
Final successful admission/login consumes it. Invalid identity assertions,
cancelled/expired ceremonies and failed transactions cannot create an account or
issue a session; reservations cannot indefinitely lock codes.

An already admitted account uses normal sign-in and does not require a new invite
on every device or reconnect. Entering the **creation** flow is different: it
requires an invite even if the authenticated SSO identity eventually matches an
existing account, as specified below. This distinction reconciles one-time
admission with restoring an existing account on a new device.

### 6.2 Local account creation

The form is bound to the selected service. It collects username/password, a required
service-invite field, and email/age acknowledgement according to that service's
validated policy descriptor. Submission remains disabled until required fields
are supplied, and the backend independently enforces all requirements.

| Operator policy | Client and service behavior |
| --- | --- |
| Email required | Collect email and enforce the service's declared verification policy. |
| Email optional | Allow omission; enforce the declared verification/recovery behavior when supplied. |
| Email disabled | Do not collect or transmit an email address or invent a placeholder address. |
| Age acknowledgement enabled | Require acknowledgement under the operator's disclosed policy. |
| Age acknowledgement disabled | Do not collect an acknowledgement or birth date through this flow. |

Required acknowledgement is recommended for publicly exposed services. The operator
chooses policy for private services. Email-disabled/optional services require a
defined local account/E2EE recovery path; the client must not route their recovery
through CVNet. Existing authentication and cryptographic standards remain binding.

Independent service operators govern admissions and moderation. CVNet receives
no local contents or administrative authority by default. This describes product
authority and data flow; applicable legal terms remain authoritative.

## 7. CVNet SSO into Self-Hosted Services

CVNet acts as an identity provider; the Self-Hosted Service creates or authenticates
its own account. Use standard OIDC authorization-code flow with PKCE, state/nonce,
strict issuer/audience verification, bounded token lifetime, registered redirect
handling and rotation/revocation rules. Do not introduce custom cryptography.

The service administrator can disable CVNet SSO. The backend advertises and enforces
this setting. CVNet production rejects use of CVNet as its own SSO provider. Add
this service-only provider without lifting the existing Google/Apple deployment
policy as a side effect. Local sign-in remains available when SSO is unavailable.

The creation journey is:

1. Display a trusted-client acknowledgement/privacy statement identifying the exact service,
   what identity attributes it receives, and CVNet's participation in the authentication.
2. Obtain consent and a valid service invite; reserve it for this ceremony.
3. Authenticate with CVNet, using the existing account or its normal account-creation flow.
4. Verify the assertion and resolve the local account by issuer and service-specific subject.
5. Create/admit a new account, or sign in to the exact existing account.
6. Consume the invite on either successful outcome and establish a device-local session.

For the existing-account outcome show a brief message such as
**“You already have an account on this service. Signing in…”**. Do not expose
account existence before identity verification or link accounts merely by email.
A disabled/banned account does not become admitted through this fallback.

Use pairwise identifiers stable across devices for the same service, with minimal
consented claims. The subject is service-scoped, not derived from a device ID or
changed when another device registers its client session. A service configured
with email disabled receives no email claim.
Never pass CVNet passwords, ordinary CVNet access/refresh tokens or CVNet private
E2EE keys to the service. Local service keys are independent. Account creation and
key setup must not publish an authenticated content session before E2EE custody
is ready or its explicit unlock gate is established.

Restored connections use **sign-in**, not the creation journey. Their stable SSO
subject resolves the same local account, mints a fresh device session, and neither
creates a duplicate account nor requires another service invite.

SSO is a consented identity bridge. It does not provide absolute unlinkability:
ordinary OIDC involves CVNet in authentication and identifies the relying service.
Consent must explain that boundary without implying unrelated profiles are shared.

## 8. Service-owned community and friend codes

New service, server and friend codes carry an unambiguous versioned reference to
their issuing service and their kind. An opaque secret remains scoped to that
issuer; shared parsing does not make the three kinds interchangeable. Resolve
and approve any endpoint before redeeming a code or sending credentials.

Join Server and Add Friend require a visible service selector when self-hosted
connections exist. Create Server also requires the service that will own the new
community. Eligible authenticated services appear in the selector, with their
identity and status. The same rule applies with only one currently online candidate.
Use the established accessible pill style where appropriate; use the requested
mandatory dropdown for creation/ownership selection.

A structured code may prefill its owner, but the client shows that owner for
confirmation. A mismatch between the selected owner and code issuer refuses before
redemption. Legacy bare codes require explicit service selection when self-hosted
connections are present; the CVNet-only legacy path remains scoped to CVNet.

A message's source can suggest a choice but cannot authorize it. Pasting a code
must not silently adopt its containing DM's service, fan it out to multiple
services, or ask CVNet to identify private-service codes. Community/friend code
handling is distinct from Connections > New Connection service admission.

## 9. Unified DMs, server bar and status

DMs and group conversations from all connected services appear in the normal
combined list and retain their usual sorting. Each row visibly identifies its
owning service; replies, attachments, calls, reactions, crypto operations and
context-menu actions use that row's owner. A combined list does not create
cross-service conversations or move memberships/keys between services.

Servers from Self-Hosted Services appear in the ordinary server bar with a
self-hosted ownership badge. The owning service's connection state adds an
offline/unreachable indication. Identical names or local IDs remain separate.
One service's failure does not remove CVNet or another service's communities.

Connections and the self-hosted login list display **not checked**, **checking**,
**reachable**, **connected**, **offline/unreachable**, **authentication needed**,
and **incompatible** states as appropriate. Reachable discovery is not proof of
authenticated access or complete application/media health.

Use one bounded, credential-free public reachability check per service, with
backoff/jitter and shared connection state for its communities. Do not probe once
per community, scan ports, or contact an unapproved restored endpoint. Periodic
checks and reconnects respect device privacy settings and resource limits.
Temporary connections remain session-local; chosen local remembering persists
on that device; chosen encrypted backup permits later opt-in device restore.

## 10. Compatibility floors and update authority

Each service advertises its protocol/capability schema and client compatibility
requirements, including minimum desktop, IPC-contract and SPA versions. These are
validated compatibility claims for that connection. They cannot redefine the
client's main/preload IPC contract or authorize code from that service.

The client either satisfies the minimum floor, offers the existing trusted update
path, or refuses that service connection with a clear incompatibility state.
A service requesting an unavailable future version cannot redirect the updater
or block unrelated connections with a global update overlay.

Desktop and SPA updates continue through the existing trusted CVNet distribution
and verification path, without requiring a CVNet account. Service responses
cannot change the update feed, signing trust, channel or executable SPA origin.
They cannot install an older desktop/SPA or force rollback. If the latest trusted
client cannot interoperate with an older service, that service needs an upgrade
or a supported protocol adaptation; the client does not downgrade itself.

## 11. Local remembering and encrypted CVNet backup

Local remembering is available to self-host-only users. Store credentials and
private keys through the existing OS-protected storage policy, separately per
service/account/device. A temporary connection does not silently become persistent.

CVNet connection backup is separately opt-in, managed in Connections. Use a
dedicated, versioned E2EE connection vault scoped to the CVNet account and its
vault key. Do not add service credentials to the active service's ordinary
appearance/layout preferences. URLs, ports, account references, labels and
selected metadata are inside authenticated ciphertext, not plaintext indexes.
Follow the existing AES-256-GCM/KDF/key-wrapping standards and bind owner/version
context. Ciphertext hydration validates a bounded schema and cannot supply code,
new trust grants or executable configuration.

Backup connection descriptors and selected account metadata. Secret backup needs
its own explicit policy/consent; rotating refresh tokens and cookies remain
device-local. Restoring a descriptor authenticates the existing account on the
new device, subject to MFA/E2EE unlock, rather than copying another device's live
session. Connection state must not be pushed into a Self-Hosted Service's prefs.

CVNet can store opaque vault ciphertext without learning its plaintext contents.
It still observes the vault's CVNet-account ownership, request timing and padded
size. Use bounded padding/minimal metadata; do not promise zero observable metadata.
The SSO disclosures in section 7 remain separate from storage confidentiality.

### 11.1 New-device restore versus Start Fresh

| Device connection-vault mode | Read account vault | Write/delete account vault | Behavior |
| --- | --- | --- | --- |
| Awaiting choice | No | No | Show Restore Existing Connections and Start Fresh. |
| Restore Existing Connections | Explicitly permitted | Only under selected backup/sync policy | Restore descriptors; obtain device-local endpoint approvals and authenticate existing accounts. |
| Start Fresh / unique device | No | No | Maintain an independent local connection library. |

Start Fresh persists a device/account-specific **unique/local-only** decision.
An empty local library must not overwrite, clear or merge into the account backup.
Logging in, changing ordinary settings or restarting the app cannot silently
change this choice. A later change to restore/sync requires an explicit action
and conflict/merge handling before any account-vault write.

Use authenticated revision/conflict handling for vault updates. A stale device
cannot replace a newer snapshot or resurrect a forgotten connection through a
blind last-write-wins upload. Distinguish local forget, account-backup removal,
device-session revocation and service-account deletion in the UI.

Restore does not copy another device's private-address consent, certificate
exception, machine identity or refresh session. It does not automatically contact
every restored endpoint. CVNet SSO resolves the existing service account using its
stable pairwise subject; a unique-device choice does not create a new account.

## 12. Implementation sequence and acceptance

| Order | Deliverable | Required result |
| --- | --- | --- |
| 1 | Restore repair | Bind origin and credential/key owner before hydration; retain CVNet and explicit self-hosted login behavior. |
| 2 | Scoped connection foundation | CVNet + services A/B coexist with separate transport, auth, crypto, stores and IPC authority. |
| 3 | Connections dialog, preference ownership and unified views | Shared login/settings manager, scoped settings with client-wide defaults, owned server bar/DM views, local profile picker and safe status checks. |
| 4 | Service admission and registration policy | Operator/admin invites, atomic one-time redemption, email/age controls and local recovery behavior. |
| 5 | CVNet SSO | Consented service-only provider; correct new/existing account behavior; stable new-device identity. |
| 6 | Encrypted connection vault | Explicit backup and device restore/start-fresh semantics with no destructive default writes. |
| 7 | Compatibility/resource integration | Service-scoped floors and status; trusted accountless updates; listener/media ownership. |
| 8 | End-to-end acceptance | Real client journeys and negative isolation controls before expanded acceptance/release claims. |

At minimum, acceptance must cover:

- Fresh local login and registration against a self-hosted service, with all email/age policy modes.
- CVNet + A + B with identical local entity IDs and identical remote ports, without collisions.
- Cold remembered self-hosted restore, pending E2EE unlock, expired credentials and superseded restores.
- Scoped cookies, tokens, keys, machine IDs, caches, queued work, sockets and async responses.
- Unified DM/group ordering and server-bar actions always reaching their displayed service owner.
- No service-controlled script, update feed, rollback, privileged IPC or automatic capture activation.
- Community/friend/service code type confusion, issuer mismatch and code-from-another-service refusal.
- Missing/invalid/revoked/expired service invites, double redemption, SSO cancellation and transaction failure.
- CVNet SSO disabled by operator; CVNet production refusing itself as a relying service.
- New SSO account, existing-account creation fallback consuming an invite, banned account refusal,
  and new-device sign-in resolving the same account without registration or another invite.
- Self-host-only saved profiles; temporary versus remembered connections; per-service logout/revocation.
- Every settings field classified as client/device-wide, service/account-owned or authoritative account data; connection selectors, independent defaults, one-time preference copy and shared-setting conflict handling.
- Restore, Start Fresh, restart and stale-device writes without corrupting the account's encrypted backup.
- Restored private endpoints requiring device approval before checks or authentication.
- IPv4/IPv6, domains, local-only names, custom/default ports, unsafe URLs, DNS rebinding and TLS failure.
- Concurrent loopback SSO callbacks, media/device arbitration, reconnect storms and bounded background work.
- Old compatible services, incompatible floors and unavailable future versions while other connections remain usable.
- Trusted desktop/SPA updates without a CVNet account and refusal of service-directed code/downgrades.
- Privacy checks on ciphertext shape, claims, logs, referrers and network destination/credential ownership.

These cases are pending implementation/verification. Existing installer or single-
service test results do not establish the new multi-service contract. Full release
and fresh-host acceptance suites remain **on demand**, as directed by the maintainer.
The ingress contract #2269
remains an independent release prerequisite. This record does not close it or
claim public-host acceptance, publication or offline distribution complete.

## 13. Scope boundaries and remaining design parameters

Ground Control federation is later work. Its future posture gate requires email
and age acknowledgement enabled and enforced; CVNet SSO alone does not enable
that federation. Air-gapped distribution remains #210.
Offline services retain local accounts; CVNet SSO requires CVNet reachability.

Implementation must specify the concrete service identity/handshake schema,
execution/session isolation mechanism, typed code format and legacy migration,
invite reservation/expiry limits, registered SSO client lifecycle, account/key
recovery for each email mode, vault conflict/removal protocol, supported resource
limits and existing-profile migration. They must satisfy this record; none may
silently reduce services to single communities, remove DMs, share credential
authority, weaken TLS, redirect updates, or overwrite a unique device's backup.

This document is the durable requirements and assessment record. It does not
create follow-up issues, mutate existing issue status, or certify implementation.

## 14. Connection-aware preferences

**Additional requirement, 2026-10-08:** settings ownership follows what the setting
controls. The settings audit must classify every existing field and persistence
consumer, including mixed modals, rather than assigning one scope to an entire
modal by its title.

| Scope | Examples | Ownership and persistence |
| --- | --- | --- |
| Client/device-wide | Client Appearance; Audio and Video configuration; client accessibility, local device and capture controls where applicable | One shared client/device authority across connections. Quietly replicated copies may exist, but they converge on the latest accepted client edit rather than developing independent per-service behavior. Hardware-specific values retain their device context. |
| Service/account connection | Service-specific privacy/security preferences, account presentation and connection behavior, membership/presence preferences where service-owned | Separate preferences per service/account connection. Reads, drafts, validation, saves, queued writes and encrypted sync remain bound to that owner. |
| Authoritative account/service state | Account identity and credential changes; CVNet subscriptions/entitlements; bans, permissions and admission policy | Read or mutate through the selected account/service's authorized API. These are not transferable preference-file values. Subscription management is available only for CVNet. |

When multiple connections exist, each settings modal with connection-owned fields
shows a required **Connection** dropdown containing the applicable connected
profiles, including CVNet. Changing it changes the account/service whose settings
are displayed and edited. The client retains owner-bound drafts and prevents a
late response or queued Apply from reading or overwriting the successor profile.
Mixed modals visibly distinguish their client/device-wide fields from their
connection-owned fields. Pure client/device modals continue to show shared values.

Each new service/account connection initializes an independent preferences file
with validated defaults. It does not inherit another connection's preferences,
account or security approvals implicitly. Separate files and encryption/sync
contexts prevent ordinary reads or writes from crossing service ownership. The
connection vault in section 11 backs up connection descriptors; it is not an
ambient preferences store for whichever service is currently selected.

Offer an explicit convenience action to initialize a new profile from another
named connection's settings. The requested **“Sync settings to [X] Profile”**
behavior is a **one-time copy**: show which profile is the source and which is the
destination, take a read-only snapshot of the chosen source's transferable
preferences, and replicate those validated values exactly into the destination's
new file. A label such as **“Copy settings from [X]”** can make that direction clear.
The source remains unchanged and no continuing synchronization is enabled.
Incompatible policy/schema values require an explicit resolution; do not silently
coerce or discard them while claiming an exact copy.

Credentials, E2EE keys, account/recovery/SSO identities, subscriptions/entitlement
claims, machine IDs, endpoint/private-address approvals and TLS exceptions are not
transferable preferences. Copying a profile cannot grant another account's
identity, trust or authority. This locally requested one-time preference transfer
is an explicit client action, not communication between the two services.

Client/device-wide settings stay consistent for CVNet and all self-hosted
connections. If their replicated copies conflict, the **most recent accepted
update wins**. Use bounded schemas and trusted-client revision/order handling;
an arbitrary service-supplied future timestamp cannot take control of global
preferences. Replicate only approved client-setting fields, never service/account
preferences or a directory of other connections. Ordinary settings activity also
cannot change a device's Restore/Start Fresh connection-vault choice.

Acceptance must cover each actual settings field and persistence path, independent
CVNet/A/B edits, default initialization, profile switching during Apply and async
responses, one-time read-only source copying, incompatible values, latest-update
convergence of shared Appearance/Audio/Video, CVNet-only subscription controls,
and absence of credential/key/trust or service-preference transfer. The field
classification and any changed settings documentation ship with implementation.
