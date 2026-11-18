# ADR-0023: Temporary move-granted SBAC permissions & the MOVE_MEMBERS hierarchy exception

**Status:** Accepted
**Date:** 2026-06-15
**Deciders:** @markdrogersjr (security owner)
**Issue:** #487
**Reviewers who should read this:** `rbac-reviewer`, `e2ee-reviewer`, `security-reviewer`

> Number note: originally drafted as ADR-0019; renumbered to 0023 after the #1282 + #1561 ADR-collision renumbers landed on `main` (0019→0022 churn; 0019-0022 became occupied).

---

## Decision 1 — Model temporary grants as columns on `channel_permission_overrides`

### Context
A `MOVE_MEMBERS` holder can drag a user into a voice channel the target cannot see (#487 Scope B). The target needs *just enough* permission to participate (`PermViewVoiceChannels | PermJoinVoice | PermSpeak`) and nothing more, with the grant auto-cleaned on departure including E2EE key rotation. The resolver already applies **user-specific** channel overrides with final authority (`applyChannelOverrides` in `internal/rbac/resolver.go`), so the grant itself needs no resolver change — only a way to mark a row as ephemeral.

### Decision
Add 3 columns to the existing `channel_permission_overrides` table (migration `000067`): `is_temporary BOOLEAN NOT NULL DEFAULT FALSE`, `temporary_reason VARCHAR(32)`, `granted_at TIMESTAMPTZ`. Grant lifetime is **bound to voice presence**, not a wall-clock timer; a nightly sweep over orphaned `is_temporary` rows (`internal/voice/temp_grant_sweep.go`) is the backstop.

### Options Considered
| Option | Complexity | Resolver change | Cleanup | Verdict |
|---|---|---|---|---|
| **A: columns on existing table** | Low | None (user override already final-authority) | reuse cache invalidation; `WHERE is_temporary` partial index | **Chosen** |
| B: new `temporary_grants` table | Med | New join in resolver + visibility CTE | separate invalidation path | Rejected — duplicates a working mechanism |
| C: in-memory grant registry | Med | New runtime layer | lost on restart → ghost access | Rejected — not server-authoritative across restarts |
| D: `expires_at` timer | Low-Med | none | timer + sweep | Partially adopted — presence-bound primary, sweep backstop; no per-grant timer |

### Consequences
- **Easier:** the temp grant is a normal user override → the resolver, cache, and visibility query already handle it; the only new logic is cleanup triggers.
- **Harder:** graceful leave, heartbeat-timeout, moderator revoke, and orphan sweep MUST converge on the shared cleanup helpers in `internal/voice/temp_grants.go`. For Server Voice terminal cleanup, `PrepareCapture` precedes one visibility→lifecycle→channel transaction that deletes the participant and only an eligible `is_temporary=true` override together, computes the successor epoch before purging key material, records the durable `key_revocations` row, and purges the departing user's wrapped channel key and pending key request. A permanent user override serializes on the same lifecycle lock and reuses the Tx-only keyrotation helper: loss of effective `VIEW` commits the permanent row, revocation, and key purges atomically; retained `VIEW` clears temporary metadata without rotation. Ambiguous grant compensation is fenced to the returned tuple `xmin`, not `granted_at`, so a newer refresh or permanent override wins. Failed immediate compensation force-disconnects and the existing five-second reconciliation callback retries past-grace orphan grants. Generic deletion/category-sync operations that would remove a `move_granted` row fail atomically under the visibility lock; category parent/sync-flag and child writes remain one transaction. Only the dedicated revoke or permanent-upsert supersession paths remove those rows. A rollback or other proven no-commit outcome runs neither effect class. A confirmed commit may trigger commit-derived effects; the completion and roster-left call chain shares one detached bounded 10-second context, while presence execution is queued to its independently bounded executor. An ambiguous commit may run only fail-closed invalidation, revalidation, disconnect, and presence-abandon effects, never an unconfirmed epoch or leave announcement. These effects are not durable authorization state.
- **Revisit when:** a second temporary-grant use case appears (e.g. timed mod overrides) — then `expires_at` may be promoted to first-class.
- **Reviewer note:** `rbac-reviewer` — the new override is `allow`-only (`VIEW | CONNECT | SPEAK` per the developer course-correction granting full voice participation), never `deny`, never `SEND_MESSAGES` or management bits; the `is_temporary` guard on DELETE is the integrity-critical line. `e2ee-reviewer` — departure MUST record the per-channel CSK rotation inside the same transaction as participant/override cleanup so the departed user can't decrypt post-visit traffic. Voice join/key access/participant reads and WebSocket subscribe/prune authorization bypass stale permission-cache reads; `key_revocations` is the durable authorization fence and missed push delivery is recovered by a pull. Future at-least-once JetStream delivery belongs to #2635, not a second rail here.

---

## Decision 2 — `POST /move` intentionally bypasses role hierarchy

### Context
All existing voice-mod actions (`ServerMute/Deafen/Kick`) call `authorizeVoiceMod(..., requireHierarchy=true)` → `CheckHierarchy`, blocking a lower-ranked actor from acting on an equal/higher-ranked target or the owner. Issue #487 states the move permission must let a designated organizer (e.g. an events host with `MOVE_MEMBERS` but a low role) move *anyone*, including higher roles and the owner.

### Decision
The move path (`ServerMove` in `internal/voice/handlers.go`) calls the authorization with `requireHierarchy=false`, plus a self-move short-circuit (actor == target ⇒ no Move Members bit and no hierarchy step). This is an explicit, documented break of the hierarchy invariant, scoped to the move action only, with an inline comment at the call site citing #487 + this ADR.

**Amended (Concord-Voice/Security issue 168):** the self-move short-circuit no longer skips authorization entirely. A self-move still requires server membership (pool preamble and again in the decision transaction) and the actor's own `VIEW_VOICE_CHANNELS` and `JOIN_VOICE` on the destination (`authorizeMoveDestinationTx` with `selfMove=true`). It never writes a temporary grant and never writes an audit row. See Decision 3.

### Options Considered
| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **A: no hierarchy for move (issue-specified)** | Matches the organizer use case; move is low-harm (can't kick/ban, only relocate within server) | Breaks the "hierarchy protects higher roles" mental model; could be used to annoy the owner | **Chosen** (issue-mandated) |
| B: hierarchy applies to move too | Consistent invariant | Defeats the feature's purpose (organizer can't move admins) | Rejected |
| C: new `MOVE_ANYONE` super-perm distinct from `MOVE_MEMBERS` | Granular | Scope creep; not requested | Deferred |

### Consequences
- **Threat model:** worst case is a `MOVE_MEMBERS` holder relocating the owner between voice channels in the same server — annoyance, not privilege escalation (no data access, no kick/ban, server-scoped). Cannot move across servers, cannot move users not already in voice.
- **Mitigations:** the deviation carries an inline comment at the `requireHierarchy=false` call site citing #487 + this ADR; a non-self move writes one `voice_member_moved` audit-log row when it crosses hierarchy **or** writes a temporary grant (amended by Concord-Voice/Security issue 168; see Decision 3). The row's metadata always carries both `hierarchy_crossed` and `temporary_grant` booleans.
- **Reviewer note:** `rbac-reviewer` — `ServerMove` is the single sanctioned place where hierarchy is skipped; treat any *other* `requireHierarchy=false` as a finding. `security-reviewer` — the move endpoint cannot grant durable access (the temp grant is `VIEW|CONNECT|SPEAK` only and presence-bound, cleaned up with CSK rotation on departure).

---

## Decision 3 — The mover holds what the move confers, at channel scope

**Date:** 2026-11-18 · **Issue:** Concord-Voice/Security issue 168 (folds in the channel-scope and route-deletion parts of Concord-Voice/Security issue 144)

### Context
Decisions 1 and 2 decided move authority at server scope only, after the handler had already read voice state. A Move Members holder could therefore write a temporary `VIEW | CONNECT | SPEAK` override on a channel they could not access themselves, the write left no audit row when the target was lower-ranked, and a channel DENY of Move Members (or of Mute/Deafen Members on the other session actions) had no effect.

### Decision
Voice moderation is a narrow-only gate: the server-scope bit stays necessary, and channel overrides can only refuse. Code lives in `services/control-plane/internal/voice/mod_authority.go`.

- **Authorize before reveal.** `authorizeVoiceModWith` runs on the pool before the body is parsed, before any voice-state read, and before any transaction or lock: path IDs (400) → membership (403 `Not a member of this server`) → server-scope bit (403 `Insufficient permissions`) → hierarchy (403, all routes except move).
- **Move (`ServerMove`)** is decided again inside the decision transaction, under the server visibility lock, with `ResolveChannelPermissionsTx`:
  - destination: a voice channel of this server on which the actor holds `VIEW_VOICE_CHANNELS`, `JOIN_VOICE` and, unless self, Move Members. A missing, non-voice, foreign or inaccessible destination answers the same 403 (`authorizeMoveDestinationTx`);
  - source: every voice channel the target occupies, sorted (`findVoiceChannelsTx`). Channels the actor cannot see are dropped; none left answers the existing 409 not-in-voice. Every visible source requires Move Members, else 403 (`authorizeMoveSourcesTx`);
  - grant: when the target needs a temporary grant, the actor's destination permissions must hold all of `tempGrantAllow` (`VIEW | CONNECT | SPEAK`), so a mover cannot confer a bit they lack.
- **In-transaction re-check.** `revalidateVoiceMoveAuthorityTx` recomposes membership, server-scope Move (no hierarchy), the destination stage, `tempGrantAllow`, and the source stage, and is the only authority closure a moderated move passes to the grant transaction. A source no longer visible answers 409; a destination deleted since the decision answers 403. `grantTemporaryChannelAccessWithCredential` refuses a nil closure with `errTemporaryGrantAuthorityRequired` before `BeginTx` and runs the closure whether or not the credential guard is on.
- **Session actions** (user-mute, user-deafen, disconnect) filter their captured command specs per channel (`filterAuthorizedSessionSpecsTx`): a channel the actor cannot see is dropped, a visible channel without the bit refuses the whole request with 403, and no remaining spec answers the existing not-in-voice response.
- **Temp-access revoke** requires the named channel to be a voice channel of this server on which the actor holds `VIEW_VOICE_CHANNELS` and Move Members (`requireActorVoiceChannelPermTx`, inside the revoke transaction). The old pool-side channel check, which answered 400, is gone. A foreign or non-voice channel now answers 403. A refuse-only pool preflight (`preflightRevokeChannelScope`) gives the same 403 before the presence capture, so a refused revoke does no work in a channel the actor cannot see. It never grants: a lookup error falls through to the transaction, which decides.
- **Persistent server mute/deafen** (`ServerMute`, `ServerUnmute`, `ServerDeafen`, `ServerUndeafen`) stay at server scope by decision: a channel DENY of the bit does not affect them, and a test pins that.
- **Audit.** `auditVoiceMove` writes one `voice_member_moved` row for every non-self move that crosses hierarchy or writes a temporary grant (best-effort, after commit).
- **Route removal.** The unused channel-scoped voice authorization-preview route is deleted; no client or service called it.

### Consequences
- **Upgrade behaviour:** latent channel DENY rows for Mute, Deafen or Move Members start to take effect on session actions and moves. Organizer roles that moved people into channels they cannot enter need access to those channels. Several 400 answers on move and temp-access revoke become 403.
- **Accepted (R12):** a channel override carrying the Administrator bit satisfies the new channel checks, because they use the resolver's own semantics (the same ones `AuthorizeJoin` uses). No new authority arises, because the server-scope bit stays necessary. The class closes at the resolver under Concord-Voice/Security issue 142, which also carries the "channel ALLOW confers the bit" half of Concord-Voice/Security issue 144.
- **Reviewer note:** `rbac-reviewer` — multi-bit checks use `hasAllPerms`, never `Permission.Has(A|B)` (any-of); channel resolves test `rbac.ErrChannelNotInServer` before `rbac.ErrNotMember`; move remains the only `requireHierarchy=false` call.

---

## Implementation references
- Migration: `services/control-plane/migrations/000067_temp_sbac_overrides.{up,down}.sql`
- Grant/revoke convergence: `services/control-plane/internal/voice/temp_grants.go`
- Per-channel CSK rotation (shared with member-removal): `services/control-plane/internal/keyrotation/rotator.go`
- Move endpoint: `services/control-plane/internal/voice/handlers.go` (`ServerMove`)
- Channel-scoped moderation authority (Decision 3): `services/control-plane/internal/voice/mod_authority.go`
- Force-disconnect: NATS `voice.enforce.disconnect` → `services/media-plane/src/lib/forceDisconnect.ts`
- Client purge signal: WS `channel_access_revoked` → `client/desktop/src/renderer/hooks/useWebSocketMessages.ts`
- Orphan sweep: `services/control-plane/internal/voice/temp_grant_sweep.go`
