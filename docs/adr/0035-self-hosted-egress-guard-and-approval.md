# ADR-0035: Self-hosted egress guard + decoupled approval ceremony (CWE-918)

**Status:** Accepted (2026-07-30)
**Deciders:** repo admin
**Issue:** #2354 | **Epic:** #1615 | **Spec:** `docs/superpowers/specs/2026-07-30-2354-selfhosted-probe-ssrf-design.md`

## Context

### The CWE

`selfHosted:probeServer` accepted a renderer-supplied URL and handed it to
`probeSelfHostedServer`, which issued two `net.fetch` GETs **from the main process** —
outside the renderer sandbox, outside Chromium site isolation, with whatever host
reachability the machine has. `normalizeSelfHostedUrl` rejected embedded credentials,
non-`http(s)` schemes, a `null` origin, and `http:` for non-localhost hosts, but performed
**zero address resolution and zero address classification**. The old `isLocalhost` was an
exact string match on `localhost | 127.0.0.1 | [::1] | ::1`, so `127.0.0.2` was not even
recognised as loopback.

That is the textbook SSRF shape (CWE-918). A renderer controlling that one string reached
cloud metadata (`http://169.254.169.254/…` — the address the issue names), any loopback
service on any port, any RFC1918 host on the LAN, and link-local / multicast / unspecified
addresses. With no schema validation, size cap, or timeout, an attacker-shaped JSON body
was returned verbatim across the IPC boundary.

### The amplification

The probe was not a leaf. A **successful probe was the sole minting authority** for
`validatedSelfHostedOrigins` — a process-global, never-expiring, never-revoked
`Set<string>` in `selfHostedProfile.ts`. That set gates admission as a valid API base in
packaged builds (`isValidApiBase`), permission to **persist the refresh credential**
(`auth:storeRefreshToken`), hosting the **Apple/Google SSO exchange and MFA verify POST**
(`approvedApiBase` in `ipc/sso.ts`), and allocation of an on-disk profile directory
`profiles/<sha256(origin)>/`.

**"The probe succeeded" and "this origin may hold my refresh credential and run my SSO
exchange" were the same line of code.** The bar for minting a permanent credential-custody
grant was *reply with any JSON twice*.

> Update-feed steering is **not** part of this stake. `setUpdateFeedUrl(_apiBase)` ignores
> its argument and activates the static app-controlled feed (#1981). A reviewer citing it
> as motivation will be falsified by the code.

### The guard asymmetry

`ipc/sso.ts` made five further main-process `net.fetch` calls to those same origins, none
address-checked. Guarding the probe alone would leave an origin *checked at admission and
unchecked at use* — the pattern that produced this bug.

This is the desktop port of the Go control-plane's dial-time egress guard
(`.claude/rules/domains/backend.md` § "Outbound-request SSRF egress guard", reference implementation
`internal/klipy/handlers.go`, issue #1361). Same principle, different runtime.

## Decision

**One guarded transport, and one durable native approval ceremony that is the only mint.**

1. **A pure two-tier address classifier** — `client/desktop/src/main/egressPolicy.ts`.
   `classifyAddress(address)` is total, throw-free, and takes **exactly one parameter**:
   there is no approval flag, so "tier 1 is absolute" is a type-level property rather than
   a convention. `isDialPermitted(decision, originApproved)` returns `false` for `tier1`
   with no branch on `originApproved`, `false` for `invalid` (fail closed), `originApproved`
   for `tier2`, `true` for `public`.
   - **Tier 1 — never approvable, no dialog ever.** IPv4 `0.0.0.0/8`, `169.254.0.0/16`,
     `192.0.0.0/24`, `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4` (covering
     `255.255.255.255`). IPv6 `::/128`, `fe80::/10`, `fec0::/10`, `ff00::/8`, `100::/64`,
     `2001:db8::/32`, `3fff::/20`, `2002::/16`, `64:ff9b::/96`, `64:ff9b:1::/48`. NAT64 and
     6to4 are tier 1 because `net.BlockList` does not unwrap their embedded IPv4 — an IPv4
     laundering channel.
   - **Tier 2 — approvable with proof, default deny.** IPv4 `127.0.0.0/8`, `10.0.0.0/8`,
     `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`. IPv6 `::1/128`, `fc00::/7`.
   - `::ffff:0:0/96` is deliberately **not listed**: it is the mapping container, and
     `net.BlockList` judges mapped addresses by the embedded IPv4 automatically. Do not add
     a manual unmap.

2. **The address checked is the address dialled** — `client/desktop/src/main/guardedRequest.ts`.
   `assertLiteralAdmissible` classifies an IP-literal target textually before the request is
   issued, and `makeGuardedLookup` is installed as the `lookup` option on `http(s).request`
   for hostnames, so a denied address class is unreachable by construction rather than by
   policy. Transport controls: redirects are **followed only within the pinned origin**
   (manual, **max 3 hops**, full admission re-run per hop — so a self-hosted server that
   301s a discovery path is probed successfully, which the retired `redirect: 'manual'`
   init option wrongly implied it would not be); a **mandatory origin pin** checked *before*
   the next hop is dialled; a **64 KiB body cap** enforced in `res.on('data')` before
   `JSON.parse`; a **per-call non-pooling agent** (`keepAlive: false`, so no socket is ever
   reused past its admission check); `req.setTimeout(8000)` plus an overall
   `AbortSignal.timeout(10000)`. `credentials: 'omit'` is structural — Node has no cookie
   jar, auth cache, or HSTS store.

3. **A durable approval store** — `client/desktop/src/main/selfHostedApprovals.ts`.
   `<pinned userData>/self-hosted-approvals.json` (ADR-0020's pinned root, at root level
   rather than inside `profiles/` because this file is the index of which profiles may
   exist), mode `0o600`, `version: 1`. Write is `writeFileSync(tmp, {mode:0o600})` →
   `renameSync` (same-directory atomic) → re-read-verify. Read is parsed once and cached;
   **any** failure — missing, unreadable, invalid JSON, wrong version, non-array, malformed
   record, or `origin !== new URL(origin).origin` — yields the **empty set, never a partial
   one**. Partial recovery of a trust file is a trust bug. A missing file is normal.

4. **Minting moves out of the probe entirely.** `commitSelfHostedApproval(apiBase, address)`
   in `selfHostedProfile.ts` is the single writer of both the durable file and the in-memory
   set. A failed write returns `false`; the in-memory set is **not** optimistically updated
   and the probe does not proceed (`approval_not_saved`). `loadApprovedSelfHostedOrigins()`
   repopulates the hot-path set once at startup. A successful probe mints nothing.

5. **The ceremony is a native, window-modal `dialog.showMessageBox`** in the
   `selfHosted:probeServer` handler — async (never `showMessageBoxSync`), with
   `showMessageBox` an injected dependency (the `applicationsFolderGate.ts` pattern),
   `buttons: ['Cancel', 'Trust This Server']`, `defaultId: 0`, `cancelId: 0`, `noLink: true`,
   `type: 'warning'`. One copy template for every tier; only the address-class suffix on the
   `Resolves to:` line varies. Template invariance is itself a security property: a user who
   learns the dialog's shape on their LAN server must not meet a structurally different one
   for a public origin. Punycode from `new URL()` is never decoded, prettified, or truncated.

6. **The `sso.ts` split.** Only `:264` (`revokeExplicitSession`, already
   `credentials: 'omit'`) moves to `guardedRequest`. The four `credentials: 'include'` sites
   stay on `net.fetch` — they need a cookie jar Node does not have, and stranding them would
   expire SSO sessions at 15 minutes. `approvedApiBase` gains a **synchronous, IP-literal-only**
   tier-1 pre-flight reusing `classifyAddress`, documented in-comment as defence in depth
   against a tampered approvals file — *not* as TOCTOU or rebinding protection.

`IPC_CONTRACT_VERSION` is unchanged at **21**; no new channel; no new dependency; the lockfile
is unchanged. (This change alters what `selfHosted:probeServer` *does* — it no longer mints —
but not its signature, so no bump is owed. The pre-#2424 value **20** appeared in an earlier
draft of this ADR; it was stale, not a decision to hold the version.)

### Measurements that constrain the design

These are the reason the code looks the way it does. A future reader who does not see the
numbers will "helpfully" undo them.

**1. Do not set the TLS `ca` option — Node's `ca` *replaces* the trust store.**

| config | `api.concordvoice.chat` | `github.com` |
|---|---|---|
| `ca` unset | OK (`301`, `authorized: true`) | — |
| `ca: getCACertificates('bundled')` (120 certs) | OK | — |
| `ca: getCACertificates('default')` (161 certs) | OK | — |
| **`ca: getCACertificates('system')` (41 certs)** | **`UNABLE_TO_GET_ISSUER_CERT_LOCALLY`** | **`UNABLE_TO_GET_ISSUER_CERT_LOCALLY`** |

macOS `system` is 41 certificates, **41/41 disjoint from `bundled`** — a supplement, not a
superset. Passing it as `ca` discards all 120 public roots, so **every public-CA self-hosted
server breaks**. Measured `default == bundled ∪ system` (161 = 120 + 41), so the runtime
already consults the OS store. **Correct instruction: do not set `ca` at all.** The
implementation carries an inline comment citing this measurement so a future reviewer does
not add `system`.

**2. A `lookup`-only guard is 100% bypassable; the literal case needs its own check.** An
IP-literal host never invokes `lookup` (`net.Socket.connect` skips it when `net.isIP(host)`
is truthy). Measured: `host:'127.0.0.1'` and `host:'::1'` both served `200` with the guard
**uncalled**.

The two cases are therefore guarded separately, and both inside `guardedRequest`:
`assertLiteralAdmissible` classifies an IP-literal target textually before the request is
issued — a literal cannot rebind, so that *is* connection time for it — and
`makeGuardedLookup` is installed as the `lookup` option on `http(s).request`, which fires at
dial time and validates every resolved address. Both run at the top of **every** redirect
hop, so a redirect target re-enters full admission rather than inheriting the first hop's
verdict.

*An earlier draft named a single `guardedCreateConnection` wrapper as the enforcement point.
That function existed, was tested, and was called by nothing; it was deleted in this PR. The
property it described is real and is what the two mechanisms above provide — but naming a
function with no call sites is the same defect this ADR corrects in #14b, so the names here
are the ones that actually run.*

**3. Every address in a multi-address `lookup` result must be validated.** Measured: Happy
Eyeballs completed the connection against `addresses[1]` when `addresses[0]` was a
blackhole. Validating only `addresses[0]` is not a guard. The rule is **wholesale deny** —
if any entry is denied the whole lookup fails; never filter-and-pass.

**4. `net.isIP()` gates `BlockList` — `BlockList` fails *open*.** Measured:
`new net.BlockList().check('not-an-ip', 'ipv4') === false` — indistinguishable from
"allowed". `classifyAddress` therefore reaches `BlockList` only after a non-zero
`net.isIP`, and strips brackets first (`new URL().hostname` yields bracketed IPv6;
`net.isIP('[::1]') === 0`).

### The pre-resolution is not the forbidden pattern

The IPC handler calls `resolveForDisplay(host)` before the ceremony to render the dialog's
`Resolves to:` line and to fast-fail tier-1 addresses without ever showing a dialog. A
reviewer will read that as the forbidden "pre-resolution plus `net.fetch`". It is not.
Verbatim, per spec §5.3:

> **`resolveForDisplay` is a pre-resolution, but it is not the security check. No dial is
> ever authorized by its result; the authoritative check re-runs on every dial and every
> redirect hop — `assertLiteralAdmissible` for a literal target, `makeGuardedLookup` at
> dial time for a hostname.**

Mechanically, `resolveForDisplay` feeds two things and nothing else — the dialog's display
line, and a tier-1 fast-fail that can only ever *deny*. It cannot admit. Every byte that
leaves the process does so through `guardedRequest`, whose per-hop `assertLiteralAdmissible`
and dial-time `makeGuardedLookup` re-classify the address they are about to dial with no
reference to the earlier resolution. The forbidden pattern is one where the pre-resolution
result *authorizes* a subsequent unguarded fetch; here the subsequent fetch is itself
guarded. `resolveForDisplay` classifies **wholesale** — if any resolved address is tier 2 it
presents and records tier 2, the strictest member of the set, and the dialog lists every
address rather than the first, so consent is never taken against a partial view. A
`resolveForDisplay` DNS failure **blocks** the ceremony and surfaces the retryable
`unreachable` state: approving a name you cannot inspect defeats the "show the user what it
resolves to" invariant, and showing a ceremony for an origin that will then fail to connect
would burn the once-per-origin dialog.

**Trust is minted only after the probe proves the server.** The ceremony's grant is
request-scoped until `probeSelfHostedServer` returns `status: 'ok'`; it authorizes the dial
the probe itself needs and is cleared in a `finally`. `commitSelfHostedApproval` — the sole
durable minter — runs after that proof. Otherwise every post-consent failure (a non-Concord
server, a TLS error, `ECONNREFUSED`, an over-cap body) would leave a permanent,
un-revocable credential-custody grant for an origin that was never reached, and a compromised
renderer would only need the user to approve *any* origin once, never to make it behave like
a Concord server.

### Non-negotiable #14a — main introduces no trigger of its own

Stated verbatim so a reviewer checks it rather than infers it:

> **Main never initiates the ceremony. No startup path, no timer, no revalidation
> sweep, no discovery scan, no update or migration hook may show the approval
> dialog — ever, without a full re-review of this design. The only path to the
> dialog is a `selfHosted:probeServer` invocation.**

This is a property of the main process and is main-verifiable. It is what keeps
the ceremony **rare** for a legitimate user: a future "auto-detect servers on
your network" or background-revalidation feature would fire the identical dialog
on a schedule the user did not initiate — the habituation failure that made the
retired security-label mode floor cosmetic.

**Four mechanical review assertions (all lock #14a, and only #14a):**

1. `grep -rn "requestSelfHostedApproval" client/desktop/src/main/` returns
   **exactly two lines** — definition plus one call site.
2. That call site is **lexically inside** the
   `ipcMain.handle('selfHosted:probeServer', …)` callback, in the same `await`
   chain — no `setTimeout`, `setInterval`, `app.on`, `session.on`,
   `EventEmitter`, `queueMicrotask`, or detached `Promise` between handler entry
   and the call.
3. **Test:** module import + `loadApprovedSelfHostedOrigins()` + full main-init
   calls `showMessageBox` **zero times**. Locks "no startup and no revalidation
   trigger."
4. **Test:** the `showMessageBox` return value flows into the resolution of
   *that same* `probeServer` invocation. Locks "the dialog answers a specific
   invocation" — an invocation, not a user action; see #14b.

The companion assertion for minting is unchanged: `grep -rn
"commitSelfHostedApproval" client/desktop/src/main/` returns the definition plus
exactly one call site.

### Non-property #14b — renderer-side anchoring does not exist

The original #14 additionally claimed the ceremony fires only as "the direct,
synchronous result of a user activating Connect … with that exact URL in the
field." **That is not a property this design has, and none of the four
assertions above measures it.** Falsified by red-team PoC (2026-07-30):
`IpcMainInvokeEvent` on Electron 43 carries no user-activation signal
(exhaustive against the shipped typings — `{frameId, processId, sender,
senderFrame, type}`), and `isTrustedAuthSender` answers "is this the real
Concord frame?", which a compromised renderer satisfies by construction. Such a
renderer chooses **when** the invocation fires — it receives every real click
and owns every screen, so it can make the dialog appear causally connected to
any user action, and the shipped `ServerInput.tsx` pending-state copy already
tells the user to expect a dialog — and **which URL** it carries, which may not
be the one the user typed. The four assertions were written against this claim
and passed while measuring the trusted side of a boundary whose untrusted side
the claim depended on; they are retained above under the property they actually
lock.

What the ceremony is, stated honestly: **a consent prompt whose moment and
displayed origin are attacker-controlled under renderer compromise. It bounds
damage by requiring one human "Trust This Server" per origin — and by nothing
else.** The controls that carry the rest are: the tier-1 arm of
`isDialPermitted`, which no approval can override; the ceremony token bucket and
rejecting single-flight (this PR), which remove the unbounded-retry channel; and
in-app revocation (PR-2, the GA blocker), which bounds the grant's permanence.
Restoring real anchoring would require relocating both the trigger and the URL
entry into a main-owned surface — the only construction that also closes URL
substitution; a gesture proof alone would not. Its cost is recorded in PR-2's
issue; it is an option there, not a commitment.

**Re-measurement obligation:** the absence of a user-activation query on
`WebContents`, `WebFrameMain`, or any IPC event is a *current* absence, verified
on Electron 43.1.1. `DisplayMediaRequestHandlerHandlerRequest.userGesture`
proves Electron plumbs Chromium's transient-activation bit where a surface
warrants it. Re-check on every Electron major bump, alongside the three stdlib
behaviours and the inferred libuv threadpool size already listed.

A confirmation checkbox (`checkboxLabel`) was evaluated and declined:
`MessageBoxOptions` cannot gate the affirmative on checkbox state, so an
unchecked accept becomes a silent no-op that cannot be explained or validated
inline within a native dialog; recovering it requires a second dialog
(reflex-training) or a Concord-drawn dialog (the relocation cost). The
`checkboxLabel === undefined` assertion is retained as a decision, not an
omission.

### The four abuse-channel controls (this PR)

The ceremony bounds damage per origin; these bound the *channel* a compromised
renderer has into it. None is consent integrity — see #14b.

- **A rejecting single-flight** keyed on `event.sender.id`, set synchronously
  before the handler's first `await`, gating the **whole handler** rather than
  just the ceremony. It is **required, not hardening**: `resolveForDisplay` uses
  `dns.lookup` in callback form, a `uv_getaddrinfo` job on the libuv threadpool
  with no cancellation API, so concurrent lookups on unresolvable names
  head-of-line-block every async `fs` operation in main — `tokenManager`
  credential IO and the approvals-file read included — **with zero dialogs
  shown**. It is deliberately *not* the promise-sharing single-flight used by
  `restoreSessionPromise` / `spaSelfHeal`: two concurrent probes of different
  URLs must never share one result. `ServerInput` already disables Connect while
  busy, so the refused concurrency has no legitimate caller.
- **A ceremony token bucket** — `selfHostedCeremonyBudget.ts`. It rations
  **ceremonies shown**, never probes attempted: an already-approved origin never
  reaches the branch, so re-probing it stays unthrottled. The token is taken
  before the modal opens, because a declined ceremony cost the user the same
  interruption an approved one did. The numbers are the two exported constants
  `CEREMONY_BUDGET` / `CEREMONY_WINDOW_MS`; they are in-memory only, which bounds
  a burst **within one app session** and no further. A compromised renderer can
  reset the bucket by invoking `app:relaunch` (`main.ts` exposes it via
  `ipcMain.handle`, `preload.ts` as `relaunchApp()`), gated only by
  `requireTrustedSender` — which that renderer passes by definition. Accepted: a
  relaunch is user-visible and costs the attacker its own execution context, and
  a state file would add an IO failure mode to a path whose purpose is to stay
  available. *An earlier draft claimed "the renderer cannot restart the app
  (`app.relaunch` is main-only)". That reasoned from where the API lives rather
  than who can reach it — every IPC handler is main-only; that is what makes it
  an IPC handler. `requireTrustedSender` is never a mitigation against an
  adversary who **is** the trusted sender.*
- **A `resolveForDisplay` race-timer** (`RESOLVE_DISPLAY_TIMEOUT_MS`) bounding
  how long **one caller** waits. It does **not** cancel the underlying
  `getaddrinfo`, which keeps its threadpool slot until the OS resolver gives up;
  losing the race abandons the result, not the query. It is therefore
  defence-in-depth and **not** a resource bound — the single-flight is the
  structural fix. Failing to `unreachable` is the correct direction: identical
  to a genuine resolution failure, so no dial is authorized and no dialog is
  shown for a lookup whose answer was never seen.
- **A 253-character hostname reject** in `normalizeSelfHostedUrl` (the RFC 1035
  presentation-form limit). A longer host can never resolve, so rejecting loses
  no legitimate case and keeps an unbounded attacker-chosen string out of the
  dialog. Display truncation was **rejected** as the alternative: it would
  violate the never-truncate invariant above, which exists so `evil.com` cannot
  be middle-elided into a plausible `bank…com`.

Two new wire codes arrive with the first two: **`busy`** and
**`too_many_prompts`**, both carrying an **empty `message`**. Each therefore
*requires* a `FAIL_COPY` entry in `ServerInput.tsx` or the refusal renders as a
silent no-op. Note this as a standing consequence of carrying the refusal reason
inside `message` rather than in a dedicated field: **every future code needs a
renderer entry**, and omitting one fails silently rather than loudly.

## Consequences

### Positive

- The metadata address is unreachable **even for an approved origin** — `isDialPermitted`'s
  `tier1` arm cannot see the approval flag.
- A renderer that names `http://127.0.0.1:9000/` no longer obtains credential custody by
  answering two GETs. It obtains a dialog the user did not ask for.
- Loopback and RFC1918 self-hosting still work, through the production path, once approved
  — the test suite drives that same path rather than a bypass seam.
- The probe's JSON is now bounded (64 KiB, ≤ 200 keys, timeout) instead of returned raw.
- Tier-1 and tier-2 denials carry **different wire codes**. `address_not_allowed` is the
  renderer's terminal "can never connect" copy and is only ever true of tier 1; a tier-2
  denial reports `origin_not_approved` with an empty message, so a self-hoster is never told
  their own server is permanently unreachable and a tier-2 reason token
  (`loopback`/`private`/`ula`/`cgnat` — all forbidden renderer vocabulary) cannot ride the
  tier-1 code into a user-visible string.

### Negative and accepted residuals

- **Approval binds the origin, not the address.** Post-approval rebinding *within tier 2* is
  inside the class the user accepted; rebinding to tier 1 is hard-blocked at every dial.
  That hard block is the safety net the origin-only binding leans on. The residual for the
  four un-movable `sso.ts` sites is filed for PR-3.
- **PR-2 is a blocker on promoting self-hosting past beta — and with #14b it is the
  compensating control, not a nicety.** Renderer-side anchoring does not exist, so the
  defence reduces to *one un-undoable human decision per origin*. In-app revocation is what
  bounds that decision's permanence; without it the grant outlives the compromise that
  obtained it, survives SPA rollback, self-heal, and restart, and has no in-app remedy.
  The grant is also **disproportionate to a single dialog**: an approved origin satisfies
  `isValidApiBase` (so `auth:storeRefreshToken` persists a refresh credential under
  safeStorage) and `approvedApiBase` in `ipc/sso.ts`, which hosts the Apple/Google SSO
  exchange and MFA-verify POSTs **from main** — four of them still `credentials: 'include'`.
  That amplification is the reason revocation, not more prompt friction, is the right next
  control. PR-2's issue is filed at PR-1 merge as a sub-issue of epic #1615 carrying
  `risk: security` + `release: beta` + milestone `v1.0.0`, with a line added to #1615's body
  naming it a blocker. Concretely: **self-hosting may not be announced, documented as
  generally available, or promoted beyond the current beta audience while a user has no
  in-app way to withdraw credential custody.** PR-1's grant is acceptable for a small,
  technically sophisticated beta audience; it is not acceptable at GA. "Deferrable" ≠
  "indefinite." Trigger relocation (the only construction that would restore anchoring) is
  recorded in that issue as an **option**, not a commitment, and must not be folded into the
  GA blocker.
- **Costs of the native-dialog constraint, carried honestly:** zero theming at a power-user
  surface (none of the 32 scheme blocks, none of the selected application font); no custom
  iconography, illustration, or motion at the highest-stakes moment in onboarding; platform
  inconsistency not fully controllable; and no cancel-the-wait affordance — once the dialog
  is up the renderer cannot un-show it.

### Accepted limitations

- **No system-proxy support in PR-1.** Node has none; `ses.resolveProxy` is a PR-3 bridge
  needing CONNECT-tunnel plumbing. Nothing that works today regresses (Concord has zero
  `resolveProxy` hits), but the Node transport forecloses it working *by accident*.
- **No in-app revocation in PR-1.** The dialog does not promise otherwise — it says
  *"Concord Voice will remember this choice on this device."* A coarse "Forget all
  self-hosted servers" was rejected as a second dishonesty about the same fact: it would not
  delete `profiles/<sha256(origin)>/secure-token.dat` and would not sign the user out.
  PR-2 owns revoke-clears-profile semantics and blocks GA promotion (above).
- **For the loopback adversarial case the ceremony is a damage bound, not a
  detection.** The prompt's timing and displayed origin are attacker-controlled
  under renderer compromise (#14b): "I didn't do this" does not discriminate,
  and a person with no mental model of what runs on their own machine cannot
  evaluate `127.0.0.1:9000` — including when it was substituted for the URL they
  actually typed. What the user's decision buys is one bounded, revocable
  (PR-2) grant per origin. This is why the `.security-info` reassurance panel
  above the ceremony trigger was a required deletion, and why the pending-state
  copy is hedged rather than assertive.
- **Node cannot honour operator *distrust* of a system root** — it "does not currently
  support distrusting certificates based on system settings", so a root explicitly
  distrusted in the OS keychain still validates here. A strictly *more permissive*
  divergence from Chromium; not load-bearing for #1615, which needs public-CA validation to
  work, not to be revocable.
- **Private-CA end-to-end verification is inferred, not measured.** System roots are
  confirmed present in Node's default set; a freshly keychain-added private root verifying
  end-to-end is not confirmed. Private-CA trust is out of scope regardless.
- **Test/ship runtime divergence.** Vitest runs on system Node **v26.5.0**; the app ships
  Electron 43.1.1's embedded Node **24.18.0**. This guard's correctness depends on stdlib
  behaviour — `net.isIP` short-circuiting `lookup`, `BlockList.check` failing open,
  `autoSelectFamily` defaults. All three were measured on **both** runtimes, so there is no
  live hole; but nothing structurally prevents a future Electron bump from changing one
  while the suite stays green. **Re-measure those three on any Electron major bump**, plus
  two more of the same class:
  - **The user-activation API absence** (#14b). Verified absent on Electron 43.1.1 across
    `WebContents`, `WebFrameMain`, and every IPC event; it is a *current* absence, not a
    permanent one, and `DisplayMediaRequestHandlerHandlerRequest.userGesture` proves
    Electron plumbs the bit where a surface warrants it. If it ever appears on
    `IpcMainInvokeEvent`, #14b is due for re-decision.
  - **`UV_THREADPOOL_SIZE = 4`** — the sizing behind the single-flight's justification.
    This is **inferred from the libuv default and was not measured** on Electron 43.1.1 /
    Node 24.18.0. Measure it before relying on the exact number; the single-flight's
    correctness does not depend on the value, only its stated magnitude does.

## Out of scope (tracked separately)

- Renderer-side discovery via Local Network Access — Electron 43 has not plumbed LNA
  through (`electron/electron#48655` closed "not planned"); it is not a security boundary
  today.
- Private-CA trust, system-proxy support, and in-app revocation (PR-2 / PR-3).
- The `.server-header` branding and the inert `help-link` footer — adjacent defects in a
  region this change does not otherwise touch. The footer is **retained**, with an in-file
  comment recording why.
- **Bringing `src/main/**` under the coverage gate — #2672.**
  Recorded here because it bears directly on how much assurance this ADR's green
  Quality Gate actually carries. `vite.config.ts`'s Istanbul `include` omits `src/main/**`
  and `sonar-project.properties` excludes it, so **seven of the eight files this change
  touches are unmeasured** — `egressPolicy.ts`, `guardedRequest.ts`,
  `selfHostedApprovals.ts`, `selfHostedCeremonyBudget.ts`, `main.ts`, `ipc/sso.ts`, and
  `ipcContract.ts`. Only `ServerInput.tsx` is in the measured set, and the ≥80%-on-new-code
  gate passed on that one file. The security-critical surface is well covered *in fact* —
  five new main-process suites, and the C1 predicate is mutation-verified in both
  directions — but that is a property of the tests written, not of any gate that would
  have caught their absence. **Do not read this PR's green gate as evidence the egress
  guard is covered.** The exclusion long predates this change and is deliberately not
  fixed here: switching it on repo-wide would surface a large pre-existing debt in one
  unrelated PR.

## References

- Spec: `docs/superpowers/specs/2026-07-30-2354-selfhosted-probe-ssrf-design.md`
- Plan: `docs/superpowers/plans/2026-07-30-2354-selfhosted-probe-ssrf.md`
- `.claude/rules/domains/backend.md` § "Outbound-request SSRF egress guard" — the Go original this
  ports (#1361)
- `.claude/rules/electron.md` (in Desktop) § "Token Storage" — the corrected minting sentence
- ADR-0020 — the pinned `userData` root the
  approvals file lives under
