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
The move path (`ServerMove` in `internal/voice/handlers.go`) calls the authorization with `requireHierarchy=false`, plus a self-move short-circuit (actor == target ⇒ no permission required). This is an explicit, documented break of the hierarchy invariant, scoped to the move action only, with an inline comment at the call site citing #487 + this ADR.

### Options Considered
| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **A: no hierarchy for move (issue-specified)** | Matches the organizer use case; move is low-harm (can't kick/ban, only relocate within server) | Breaks the "hierarchy protects higher roles" mental model; could be used to annoy the owner | **Chosen** (issue-mandated) |
| B: hierarchy applies to move too | Consistent invariant | Defeats the feature's purpose (organizer can't move admins) | Rejected |
| C: new `MOVE_ANYONE` super-perm distinct from `MOVE_MEMBERS` | Granular | Scope creep; not requested | Deferred |

### Consequences
- **Threat model:** worst case is a `MOVE_MEMBERS` holder relocating the owner between voice channels in the same server — annoyance, not privilege escalation (no data access, no kick/ban, server-scoped). Cannot move across servers, cannot move users not already in voice.
- **Mitigations:** the deviation carries an inline comment at the `requireHierarchy=false` call site citing #487 + this ADR; a hierarchy-crossing move writes a `voice_member_moved` audit-log row.
- **Reviewer note:** `rbac-reviewer` — `ServerMove` is the single sanctioned place where hierarchy is skipped; treat any *other* `requireHierarchy=false` as a finding. `security-reviewer` — the move endpoint cannot grant durable access (the temp grant is `VIEW|CONNECT|SPEAK` only and presence-bound, cleaned up with CSK rotation on departure).

---

## Implementation references
- Migration: `services/control-plane/migrations/000067_temp_sbac_overrides.{up,down}.sql`
- Grant/revoke convergence: `services/control-plane/internal/voice/temp_grants.go`
- Per-channel CSK rotation (shared with member-removal): `services/control-plane/internal/keyrotation/rotator.go`
- Move endpoint: `services/control-plane/internal/voice/handlers.go` (`ServerMove`)
- Force-disconnect: NATS `voice.enforce.disconnect` → `services/media-plane/src/lib/forceDisconnect.ts`
- Client purge signal: WS `channel_access_revoked` → `client/desktop/src/renderer/hooks/useWebSocketMessages.ts`
- Orphan sweep: `services/control-plane/internal/voice/temp_grant_sweep.go`
