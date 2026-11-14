# Control-plane blue-green: two-process overlap analysis (third activation gate)

**Issue:** #253 · **Date:** 2026-11-14 · **Status:** analysis for a decision · **Read at:** `b3b7f490`, rechecked against `6517eb9a`

This note answers the third activation gate for the control-plane blue-green rail that shipped default-off in PR #259: is it safe for two control-plane processes to run at the same time, and if not, what is the smallest change that makes it safe? It changes no code.

Inputs: [`docs/architecture.md` § "Control-plane blue-green deploys"](../../architecture.md#control-plane-blue-green-deploys), the design spec [`2026-11-11-253-control-plane-blue-green-design.md`](2026-11-11-253-control-plane-blue-green-design.md), and [`.claude/rules/domains/backend.md`](../../../.claude/rules/domains/backend.md). Paths below are under `services/control-plane/` unless they say otherwise. Line numbers are leads only; find each site by symbol.

## 1. Answer

**Do not activate yet.** One finding blocks activation, and its fix is small:

- **V1: every admitted Server Voice join during the overlap is force-disconnected.** Both processes apply the same `voice.joined`. The second one finds the pending admission already consumed and publishes an exact `voice.enforce.disconnect` for the socket that just joined. This was reproduced against a real database and Redis (section 5). It fails closed: nothing is disclosed and nothing is wrongly authorized. But it is a deterministic voice outage for the length of the overlap, and the client's retry is kicked again.
- **Smallest change (PR #281):** make an exact duplicate `voice.joined` a replay. The join still tries to consume its reservation first. When nothing is left to consume and the participant row already carries this event's stamp, the join has committed, so the duplicate is a replay and gets no force-disconnect. A join without that stamp still requires the reservation and still ejects on a mismatch. The rule is keyed on the row's stamp, not on the Redis claim's `duplicate` status, because a claim can outlive a rolled-back first attempt that still owes the consume. #281 adds the two-subscriber test from section 5 as a regression test, with a positive control, a rolled-back-claim case and a stamp-collision case. It also hardens the single-process topology against a republished join.

Three decisions remain for the repo admin:

- **V3, the residual case of V1.** When a producer stamp is ahead of a consumer's receipt time, each colour clamps it to its own receipt time. The two stamps then differ, the replay rule in #281 does not match, and the second applier still ejects the joiner. With the media plane on the same host, its millisecond-granular stamps are ahead of receipt only under a burst of more than about 1000 events in one millisecond, or after the clock steps backwards. The case fails closed. Either accept it as a rare residual, or require an exact replay key: record the consumed admission ID on the participant row. That needs a migration.
- **F1, failed cutovers.** A flip whose rollback cannot prove where nginx routes (`flip-failed`), or an old colour that will not stop (`old-colour-stop-failed`), keeps both colours running until an operator reruns `rebuild control-plane`. The overlap then has no time bound.
- **S3, the revocation backstop can be spent by the wrong colour.** When a logout, revoke-all or credential change cannot close its sockets inline, the durable reconciler is the backstop. Either colour can claim that obligation. A colour that holds none of the target's sockets closes nothing, gets no error, and marks the obligation complete. The revoked socket on the other colour then stays open until that colour stops. This can wrongly authorize, but only after an inline close has already failed.

Everything else is **accept**. The accepted residuals that can disclose or wrongly authorize (P1, S1, S2, S4) are bounded by how long the old colour keeps its WebSockets. After a successful cutover that is at most the 45 s stop grace per leg. After a failed one it lasts until recovery (F1). Accept them only together with the existing Desktop#64 gate (session drain) and a decision on F1, and record both on #253. Two accepted rows are behaviour that today's stop-first deploy already has: P2 (a first connection overwrites Invisible) and B7 (pending DM rings do not survive a restart).

A queue group on `voice.*` is **not** the fix (section 4.1).

## 2. What overlaps, and for how long

Two facts decide most rows below.

1. **Shared stores, private fan-out.** Postgres, Redis and NATS are shared. The WebSocket hub delivers only to its own process's clients. It iterates the in-process client maps (`internal/websocket/hub.go`, `clients` and `userClients`) and has no NATS connection; the voice subscriber says the hub "deliberately has none" (`internal/voice/nats.go`, above `NATSSubscriber.Subscribe`). Any enforcement that acts through the hub (disconnect, presence clear, subscription prune, broadcast) reaches only the colour that ran it.
2. **Shutdown order.** `shutdownControlPlane` (`cmd/server/main.go`) stops the `cleanupCtx` background workers first, then drains HTTP (up to 20 s), then stops the router-owned workers (`stopAttestation`: billing outbox and drift, pinned expiry), then the activity and presence workers, then the hub (which closes sockets), and closes NATS **last**. So a stopping colour keeps applying `voice.*` events until the end of its shutdown sequence, after its hub has closed. Every worker that is still running claims its rows (section 4.4).

The cutover has four overlap windows. Each control-plane cutover runs all four, one leg out to green and one leg back to home:

| Window | Serving colour (nginx) | Other colour | Who has WebSocket clients | Length |
|---|---|---|---|---|
| W1 | home | green booting, waiting for `/readyz` | home only | green's boot time |
| W2 | green (after flip 1) | home stopping | both: home drains its old sockets | ≤ 45 s (stop grace, then SIGKILL) |
| W3 | green | home booting, waiting for `/readyz` | green only | home's boot time |
| W4 | home (after flip 2) | green stopping | both: green drains its old sockets | ≤ 45 s |

Process overlap (W1 to W4) is longer than the 45 s nginx split, as `architecture.md` says. **Client overlap** (both colours holding sockets) is only W2 and W4. In W1 and W3 the booting colour has no clients and receives no HTTP. It can apply shared writes, and it can claim and complete shared background obligations (S3), but it cannot deliver anything to a client.

Green's `MIGRATIONS_MODE=verify` gates only two concerns: the startup migration run and the ops-metrics reader role (`cmd/server/main.go`, `parseMigrationsMode`). `OPS_METRICS_ENABLED=false` turns off the ops-metrics collector. Every other worker and subscriber runs on green too. That is correct, and suppressing tickers in verify mode is not a general mitigation: in W3 green is the only full server, so it must run its workers.

## 3. Recommendation table

Class: **D** = can disclose data, **A** = can wrongly authorize, **F** = fails closed or availability only, **I** = idempotent duplicate work, **none** = no effect.

| # | Assumption or state | What goes wrong in a realistic overlap | Class | Decision | Smallest mitigation |
|---|---|---|---|---|---|
| V1 | `voice.*` has no queue group; both colours apply every `voice.joined` | The second applier re-upserts (equal stamp passes `<=`), finds the admission consumed, logs `Rejected voice.joined pending admission` and publishes an exact force-disconnect. Every admitted join in W1 to W4 is kicked. | F | **Mitigate before activation:** fix in PR #281 | Try the consume; when nothing is left and the row already holds this exact stamp, treat the join as a replay and skip the force-disconnect. Regression tests in #281. |
| V2 | Only the process whose commit applies a voice event broadcasts it | If the other colour wins the commit, the serving colour's clients miss that `voice_state_update`. In W1 to W3 the next flip forces a reconnect, and the reconnect refetch (Desktop#68) repairs the roster. **W4 has no later flip.** A `voice.left` that green wins while it drains is broadcast only to green's sockets, and clients already on home keep the departed user in the roster. A heartbeat does not repair it, because the row is already gone and no removal is left to emit. | F | Accept; follow-up | The roster is wrong until the client's next refetch (channel switch or reconnect). Fix: stop the draining colour's `voice.*` subscription at the start of shutdown. Then in W2 and W4 only the serving colour applies voice events, which also takes W2 and W4 out of V1's and V3's window. W1 and W3 are unaffected, because the next flip repairs them. |
| V3 | Receipt-time clamp in `parseVoiceEventTime` assumes one consumer | Row ordering holds: `LockServerVoiceLifecycleTx` (per sender, in Postgres) and the Redis lifecycle claim serialize the two appliers, and the `lifecycle_event_at <= $3` fence compares in Postgres. **But the clamp itself diverges.** A producer stamp that is ahead of receipt is clamped to each colour's own receipt time (T and T + 2 ms). The second application is then a fresh, later stamp: it advances the row, misses #281's exact-stamp replay rule, finds the reservation consumed and ejects the joiner. The media plane mints stamps as `max(Date.now() × 1000 µs, last + 1 µs)` on the same host, so a stamp is ahead of receipt only after more than about 1000 events in one millisecond or a backwards clock step. | F | **Open: repo admin** | Either accept it as a rare, fail-closed residual (the `clock_skew` class reports clamps above the threshold), or make the replay key exact: store the consumed admission ID on `voice_participants` and treat "reservation gone, row holds this admission" as a replay. That is a nullable column and a migration. |
| V4 | The refresh-only (room-placement) join path has the same admission-mismatch kick (`refreshOnlyJoined`, via `handleRefreshOnlyLifecycleEvent`) | Same shape as V1, but reachable only with `MEDIA_DEPLOY_PROFILE=media-bluegreen`, which turns room placement on. The rail refuses that combination. | F | Accept | Give `refreshOnlyJoined` the same replay rule as V1 if that refusal is ever lifted. #281's rule text says it does not cover this path. |
| F1 | A failed cutover keeps both colours. `concord-ctl.sh` raises `flip-failed` and `old-colour-stop-failed` in `control_plane_bluegreen_cutover` and in `control_plane_bluegreen_flip_home`. The recovery steps (`control_plane_bluegreen_recover`, `_resume`, `_reap_leftover`) raise `old-colour-stop-failed` too, and can leave a green that will not stop beside a serving home. `_abandon` raises `flip-failed` when the flip home succeeded but green cannot be removed. | On `flip-failed` the controller keeps both colours because it cannot prove where nginx routes. On `old-colour-stop-failed` routing is known, but the stop of the old colour failed. Its shutdown state is unknown: it may be fully up or part-way through shutdown. Either way, nothing bounds how long it keeps its sockets and subscriptions, until an operator reruns `rebuild control-plane`. P1, S1 to S4, V2 and V3 last that long. A stop that reaches SIGKILL at the end of the 45 s grace is not F1: the process ends, so that case is W2 or W4. | D, A | **Open: repo admin** | Either accept the longer exposure, with an operator rule to rerun `rebuild control-plane` at once on these named errors (the controller already prints them and keeps the marker), or add containment: on `old-colour-stop-failed`, where routing is known, close the old colour's control sockets. `flip-failed` with unknown routing has no safe automatic containment. |
| P1 | Presence-audience revocation fence (`presenceAuthzState`) and the Server Voice delivery fence (`serverVoiceAuthzEpochs`) are process-local | In W2 and W4, a block, kick, ban, removal or permission change runs on the serving colour. The draining colour's audience worker cannot see the open bracket, its Server Voice deliveries authorized just before the revocation are not discarded, and the revocation's clear and abandon reach only the serving colour's hub. A viewer still connected to the draining colour can receive, or keep, the sender's Rich Presence, Custom Status or Server Voice state until that socket closes. | D | Accept, bounded | Bounded by the draining colour's socket lifetime: ≤ 45 s per leg after a successful cutover (unbounded on F1). Desktop#64 shortens it once clients leave on `session_drain`; that frame is not built on either side yet. Fallback if Desktop#64 slips: close all control sockets at the start of shutdown, not at the hub stage. |
| P2 | First-connection presence is decided per process (`Hub.registerClient` counts the local `userClients` map, then `handleRegister` writes `online` to the shared Redis status key and broadcasts it) | In W2 or W4, an Invisible user who still has a socket on the draining colour opens a socket on the serving colour. That socket is the first one on its process, so it overwrites the shared `invisible` status with `online` and broadcasts it to viewers on the **serving** colour, not only to the draining colour's sockets. | D | Accept for this gate; follow-up | Not new to the overlap. Every reconnect after a user's last socket closes takes the same path, including the reconnect wave of today's stop-first deploy, and the rail causes two such waves. The rail adds only the multi-device case above. Restoring a stored Invisible choice on first connection is a pre-existing privacy fix, to be filed as its own issue, not an activation gate. Whether the client re-sends Invisible after connecting was not verified. |
| P3 | Last-socket offline is decided per process (`transitionUserOffline`) | In W2 or W4, a user who has already moved to the serving colour loses their last socket on the draining colour. That colour writes `offline` to the shared status key and a `presence_offline_fences` row. The serving colour's heartbeat reads `offline`, which is not a user status, so it fails closed and does not restore. The user shows as offline to everyone until the 120 s status TTL lapses. | F | Accept, bounded | Bounded by `presence.StatusTTL` (120 s) after the draining colour closes the socket. The restore path after the TTL lapses was not traced. Fix if it matters: skip the offline transition for sockets closed by a cutover drain. |
| S1 | Session and family revocation closes sockets through the local hub (`loginfamily.CloseCommittedFamilies` → `Hub.DisconnectFamilyContext`) | A logout, revoke-all, password change or theft response handled by the serving colour does not close a socket for that session held by the draining colour. That socket keeps receiving realtime events until the draining colour closes it. Message writes stay fenced: WebSocket persists run `credepoch.GuardFamilyTx` in the database. **Presence status writes are not fenced:** `handleSetStatus` writes the shared Redis status key with no epoch or family check, so the revoked socket can still change the user's status (online, idle, do not disturb, invisible) for everyone. | A | Accept, bounded | Same bound and fallback as P1. A family check in `handleSetStatus` would close the write half; it is pre-existing for any socket between revocation and close, so it is a follow-up, not a gate. |
| S2 | Other hub-side enforcement: subscription pruning and permission re-push | **Server subscriptions** are not checked per delivery. `handleServerBroadcast` fans plain server broadcasts (member events, server updates) out to `serverSubscriptions`, and eviction (`BroadcastToServerAndPrune`) runs only on the colour that handled the kick or ban. A removed member's socket on the draining colour keeps receiving that colour's server broadcasts until it closes. **Channel subscriptions** are checked on every delivery against shared state (`authorizeChannelPermissions`, `checkChannelDeliveryPermissions`), so a stale channel subscription alone does not leak messages; only a sub-second time-of-check race remains. | D | Accept, bounded | Same as P1. Exposure is narrow because there is no cross-colour fan-out: only broadcasts the draining colour itself originates reach it. Verify step 4 on the host should include one kick during W2. |
| S3 | The durable revocation backstop completes on the colour that claims it. `dmblock.Reconciler` claims `login_family_revocations` rows (`FOR UPDATE SKIP LOCKED`, 10 s lease) and delivers through `voiceAuthRuntime.deliverAuth` and `deliverEpoch`, which close families only on the local hub. | `Hub.DisconnectFamilyContext` returns nil when the local hub holds no matching socket, so `deliverAuthRevocation` sets `completed_at`. If the inline close in the HTTP handler failed and the colour that holds none of the target's sockets claims the obligation, nothing is closed and nothing retries. In W1 the booting green can complete an obligation for a socket on home, which then stays open until home stops (W1 + up to 45 s of W2). W3 and W4 are the same the other way round. In W2 and W4 the serving colour can also complete an obligation whose only socket is on the draining colour, and a family with sockets on both colours completes once the claiming colour closes its own. | A | **Open: repo admin** | Either accept it: it needs an inline-close failure first, and it is bounded by boot time + 45 s per leg (unbounded on F1). Or contain it: while the rail marker is set, keep every revocation obligation due until the overlap ends (the marker clears), or complete it only after both colours acknowledge the close. A containment must be proven by a two-hub test with matching sockets on both colours. These do **not** contain it: treating only "no local match" as incomplete (a partial local match still completes), and running the drains only on the serving colour (in W2 and W4 the socket can be only on the draining colour). Not verified: whether the reconciler starts before `/readyz` passes. If it does not, the W1 and W3 cases drop out, and the W2 and W4 cases remain. |
| S4 | The token-theft admission deny can be process-local (`middleware.HoldLocalTokenTheftPending`, a mutex-guarded map consulted first by `CheckTokenTheftBlocked`) | When theft is detected while Postgres and Redis are both unavailable, the in-memory hold is the only deny, and `runTokenTheftRecoveryRetries` retries every second to persist the event. The other colour has no hold. Once the stores recover, the other colour admits the stolen token's user until the detecting colour's retry persists the event. If the cutover stops the detecting colour before a retry succeeds, the event is lost. | A | Accept, bounded | Needs a simultaneous Postgres and Redis outage at detection. Losing the event on a stop is pre-existing: today's stop-first restart loses it the same way. Containment if wanted: the rail refuses to flip or stop a colour while theft retries are pending, or the retry becomes a shutdown-drained worker. |
| H1 | Activity History has a single writer | Cluster mode would run two writers. | D, A | Already mitigated | The rail refuses `ACTIVITY_HISTORY_CLUSTER_ENABLED=true` (`rail-refused-activity-history-cluster`). |
| B1 | Background sweepers and reconcilers (active-presence plans, pending presence ops, DM retirement and clear-reap, expiry purge, billing outbox and drift, subscription expiry, voice-command outbox, feedback uploads, tier-1 erasure) | Each claims its rows in Postgres (`FOR UPDATE`, `SKIP LOCKED`, a lease, or a conditional UPDATE/DELETE), so one colour does the work. Pushes from the winner (entitlement downgrade, purge announcement, ownership completion) reach only the winner's clients. The workers whose effect is enforcement rather than a push (token-theft replay, the dmblock security round, the temp-grant sweep) are S3, not B1. | I, F | Accept | None. The other colour's clients converge at reconnect. Entitlement and permission caches are in Redis, so enforcement is shared. |
| B2 | Object-store reapers with no claim (purge straggler sweep every 15 min, multipart-upload sweeper hourly, orphan reaper daily) | Both colours can delete the same keys. S3 deletes are idempotent. The straggler sweep can count a failed attempt twice, and the multipart sweeper can log an abort failure. | I | Accept | None. |
| B3 | Ops-metrics collector writes samples keyed by `node_id` | Two writers with one `node_id` would interleave the WebSocket gauge. | I | Already mitigated | Green starts with `OPS_METRICS_ENABLED=false`, so only home collects. |
| B4 | Attestation registry cache answers allow/deny in memory | Invalidation is already cross-process: NATS `attestation.registry.*` plus a 5 min poll. The revoked set is in Redis. | none | Accept | None. |
| B5 | `voice.tier_violation` counting | Already in a queue group (`control-plane.tier-violation`), and the wildcard copy is dropped. Counts once. | none | Accept | None. |
| B6 | Voice-command transport bootstrap runs on each boot | `BootstrapTransport` is written for a competing bootstrap: it re-reads the committed receipt and only verifies when one exists. | I | Accept | None. |
| B7 | Pending DM rings are process-local (`pendingDMCalls`, a `sync.Map` in `internal/dm/voicering.go`) | A ring created on the old colour just before a flip cannot be answered on the new one. Decline and cancel get 404 (`resolvePendingDMRing`), and accept gets 409 when no accepted-call lease exists (`resolveVoiceJoinCallID`). `MarkDMPendingVoiceCall` writes only a retirement fence; it does not hand the ring over. | F | Accept | The same rings are lost today: the stop-first deploy restarts the only process. The rail loses them at two flips instead of one restart. The caller rings again. A handoff (the ring in Redis) is a feature, not a gate. |
| B8 | The per-user connection cap is per process (`maxConnectionsPerUser = 8`, `admittedConnectionsByUser` in `internal/websocket/admission.go`) | In W2 and W4 a user can hold up to 16 sockets, 8 on each colour. | F | Accept | None. |

## 4. Findings in detail

### 4.1 Voice lifecycle subscription (V1 to V4)

The `voice.*` wildcard is a plain subscription (`internal/voice/nats.go`, `NATSSubscriber.Subscribe`), so core NATS gives both colours every event. The code was already written for that: the database work runs under `LockServerVoiceLifecycleTx`, and `claimVoiceLifecycleStatus` records the event in Redis and answers `fresh` or `duplicate`. Rows, leases and terminal effects are therefore correct with two appliers.

V1 is the exception, and it comes from the reservation. `moveServerVoiceParticipant` upserts with `WHERE participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at`. An exact duplicate has an equal stamp, so the upsert reports one row, and the function then requires `DELETE FROM voice_pending_admissions … ` to remove exactly one row. The first applier already removed it, so the second gets `errVoicePendingAdmissionMismatch`. `handleServerVoiceJoined` maps that error to `publishForceDisconnect(channel, user, socketID, admissionID)`, which tells the media plane to eject that exact peer. The mutation closure ignores the claim status, so a known duplicate still takes this path.

Why not a queue group on `voice.*`:

- The hub is process-local. With a queue group, NATS would hand roughly half the events to the colour with no clients in W1 and W3, and the serving colour's clients would miss them: V2 for every event instead of only the races it loses.
- It changes a subscription whose per-replica delivery other code relies on. The tier-violation door de-duplicates on its own queue group, and the wildcard drops that copy by subject.
- It does not remove the race in W2 and W4, because each queue member still drives its own local fan-out.

Shortening the overlap does not fix V1 either. Stopping the draining colour's `voice.*` subscription at SIGTERM (the V2 follow-up) removes W2 and W4 from V1's window, but W1 and W3 remain, and they last as long as a boot.

### 4.2 Presence revocation fence (P1)

`Hub.BeginAudienceRevocation` raises a counter in the hub of the process that runs the revoking transaction. An audience worker in the other process reads its own counter, which is zero. backend.md already says this: "the fence is process state, and therefore blind across replicas". `serverVoiceAuthzEpochs`, which `RevalidateChannelSubscriptions` and `RevalidateServerSubscriptions` raise, is process-local in the same way.

What limits it in practice:

- In W1 and W3 the colour that could miss the bracket has no clients, so it has no one to deliver to.
- In W2 and W4, revoking HTTP requests reach only the serving colour, because nginx has flipped. The draining colour can still compute an audience, for example from a voice event it applies or a presence frame from one of its remaining sockets. A committed revocation is visible to it in the database. Only a revocation that is still open while its audience query runs is unproven.
- Separately, the revocation's clear and fail-closed abandon act through the serving colour's hub. A viewer on the draining colour keeps what it already received until its socket closes. Custom Status has no TTL, so it stays visible until the client reconnects and refetches (Desktop#68).

Disclosure: yes, presence-level data only (voice activity, Custom Status, Server Voice state) to a viewer whose authorization was just removed, for at most the draining colour's remaining socket lifetime.

### 4.3 Session and permission enforcement (S1 to S4)

These are the same class as P1, found while tracing it. `loginfamily.CloseCommittedFamilies` closes terminal families through `Hub.DisconnectFamilyContext`, which acts only on the local hub. This repository has no cross-process close path for families. A revoked session's socket on the draining colour keeps receiving realtime events until that colour's hub closes it. It cannot persist messages: those run `credepoch.GuardFamilyTx` inside their transaction. It can still set the user's presence status (S1).

The backstop for a failed inline close (S3) has the same local-only delivery, and a delivery that finds no local socket reports success. That is correct with one process, where "no local socket" means "no socket". With two processes it means "no socket here", and the obligation is spent on the wrong colour.

Server subscriptions are pruned only on the colour that ran the kick or ban (S2). Channel subscriptions are rechecked on every delivery, so they need no prune to stay correct.

### 4.4 Background work (B1 to B8)

The inventory covered every ticker and long-running worker the server binary starts (`cmd/server/main.go`, `internal/api/router.go`). Nothing found double-charges, double-emails, double-publishes or double-disconnects: each one either claims rows in Postgres or is an idempotent delete. Two side effects exist: pushes reach only the winner's clients, and some object-store retries are counted twice. Both are availability only. A draining colour stops its `cleanupCtx` workers at once and its router-owned workers after the HTTP drain; either way they claim rows, so the time they keep running is safe.

## 5. Reproduction of V1

Run against a disposable Postgres and Redis (`DATABASE_URL`, `REDIS_URL`) with two subscribers that share one database and Redis. Each subscriber stands for one process. `newTestSubscriber`, `HandleJoinedAt` and `mustJSON` are test-local helpers in `internal/voice`; #281 uses the NATS-backed variant `newTestSubscriberWithHubAndNATS`.

```go
func TestOverlapProofDuplicateAdmittedJoin(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	home, green := newTestSubscriber(ts), newTestSubscriber(ts)
	user := ts.CreateTestUser(t, "overlap_user")
	serverID := ts.CreateTestServer(t, user.ID, "Overlap Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-overlap")
	admissionID := uuid.New()
	_, err := ts.DB.Exec(`INSERT INTO voice_pending_admissions (channel_id, user_id, admission_id, socket_id, expires_at)
		VALUES ($1, $2, $3, 'socket-overlap', clock_timestamp() + INTERVAL '1 minute')`, channelID, user.ID, admissionID)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	event := mustJSON(t, map[string]interface{}{
		"channelId": channelID, "userId": user.ID, "username": "overlap_user",
		"admissionId": admissionID.String(), "socketId": "socket-overlap",
		"timestamp": now.Format(time.RFC3339Nano),
	})
	home.HandleJoinedAt(event, now)
	green.HandleJoinedAt(event, now.Add(2*time.Millisecond))
}
```

Observed (2026-11-14, `go test ./internal/voice -run TestOverlapProofDuplicateAdmittedJoin -v`): one participant row after each call, and on the second call

```
level=WARN msg="Rejected voice.joined pending admission" failure_class=admission
```

That log line is emitted immediately after `publishForceDisconnect` in `handleServerVoiceJoined`. This client has no NATS, so the publish itself is a no-op here. In production the media plane receives it. #281's regression tests subscribe to the real `voice.enforce.disconnect` subject and assert that no exact force-disconnect is published for the replay, with a sibling positive control that requires one. Removing the replay rule turns them red. The event's stamp here is not ahead of either receipt time, so the clamp does not fire and both colours see one stamp. The skewed-producer case in V3 is not covered by this reproduction.

## 6. Verified and not verified

Verified here: V1 (by test); the voice claim and lock path, the shutdown order, the hub's local-only fan-out, the family close path, the empty-hub success in S3, the `handleSetStatus` write in S1 and the offline write in P3 (by reading the code); and B1 to B8 (by an inventory of every worker the server binary starts, with the claims spot-checked).

Not verified, and to be covered by step 4 of the on-host checklist in `architecture.md`:

- the boot time of each colour, which sets the length of W1 and W3;
- whether the dmblock reconciler starts before `/readyz` passes (S3's W1 and W3 cases);
- how long real clients stay on the draining colour in W2 and W4, before and after Desktop#64;
- one revocation (logout, block or kick) during W2, to confirm the P1, S1 and S2 bound on a real host;
- the presence restore path after the status TTL lapses (P3);
- the managed-Redis acknowledgement path, which the rail does not reach today.

## 7. Proposed gate decision for #253

> Third gate (two-process overlap): **blocked on V1 until #281 merges, and on three admin decisions.** Fix: a duplicate admitted `voice.joined` whose reservation is gone and whose row already carries its stamp is a replay, not a reservation mismatch, with two-subscriber regression tests. Decide V3: accept the rare, fail-closed clamp-divergence residual, or require an exact admission-ID replay key (a migration). Decide F1: accept exposure until operator recovery after `flip-failed` or `old-colour-stop-failed`, with an immediate-rerun rule, or add containment for `old-colour-stop-failed`. Decide S3: accept that the revocation backstop can complete on a colour without the socket (it needs an inline-close failure first), or keep revocation obligations due until the overlap ends, or require a close acknowledgement from both colours. All other overlap effects are accepted. The shared-store writes are already serialized. The process-local residuals (P1, S1, S2, S4) can disclose or wrongly authorize only through sockets still on the draining colour, for at most 45 s per leg after a successful cutover. That bound depends on Desktop#64: if Desktop#64 does not shorten the drain, close control sockets at the start of shutdown before activating. Two further effects are accepted as behaviour today's stop-first deploy already has: a first connection overwrites an Invisible status (P2, which can reach serving-colour viewers and gets its own follow-up), and pending DM rings do not survive a flip (B7). The W4 roster gap (V2) and the offline flash (P3) are availability only and get follow-ups.
