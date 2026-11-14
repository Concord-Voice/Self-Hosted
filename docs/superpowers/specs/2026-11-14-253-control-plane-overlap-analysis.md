# Control-plane blue-green: two-process overlap analysis (third activation gate)

**Issue:** #253 · **Date:** 2026-11-14 · **Status:** analysis for a decision · **Read at:** `b3b7f490`

This note answers the third activation gate for the control-plane blue-green rail that shipped default-off in PR #259: is it safe for two control-plane processes to run at the same time, and if not, what is the smallest change that makes it safe? It changes no code.

Inputs: `docs/architecture.md` § "Control-plane blue-green deploys", the design spec `2026-11-11-253-control-plane-blue-green-design.md`, and `.claude/rules/domains/backend.md`. Paths below are under `services/control-plane/` unless they say otherwise. Line numbers are leads at `b3b7f490`; find each site by symbol.

## 1. Answer

**Do not activate yet.** One finding blocks activation, and its fix is small:

- **V1: every admitted Server Voice join during the overlap is force-disconnected.** Both processes apply the same `voice.joined`. The second one finds the pending admission already consumed and publishes an exact `voice.enforce.disconnect` for the socket that just joined. This was reproduced against a real database and Redis (section 5). It fails closed: nothing is disclosed and nothing is wrongly authorized. But it is a deterministic voice outage for the length of the overlap, and the client's retry is kicked again.
- **Smallest change (draft PR #281):** make an exact duplicate `voice.joined` a replay. When the participant row already carries this event's stamp, the join has committed and consumed its reservation, so the duplicate skips the consume and the force-disconnect. A join without that stamp still requires the reservation and still ejects on a mismatch. The rule is keyed on the row's stamp, not on the Redis claim's `duplicate` status, because a claim can outlive a rolled-back first attempt that still owes the consume. #281 adds the two-subscriber test from section 5 as a regression test, plus a positive control and a rolled-back-claim case. This also hardens the single-process topology against a republished join.

Everything else is **accept**, with one condition: the residuals that can disclose or wrongly authorize (P1 and S1) are bounded by how long the old colour keeps its WebSockets. Today that is at most the 45 s stop grace per leg. Accept them only together with the existing Desktop#64 gate (session drain), and record that dependency on #253.

A queue group on `voice.*` is **not** the fix (section 4.1).

## 2. What overlaps, and for how long

Two facts decide most rows below.

1. **Shared stores, private fan-out.** Postgres, Redis and NATS are shared. The WebSocket hub delivers only to its own process's clients. It iterates the in-process client map (`internal/websocket/hub.go`, `clients`), and the hub itself says "the fan-out is process-local (there is no NATS leg here)". Any enforcement that acts through the hub (disconnect, presence clear, subscription prune, broadcast) reaches only the colour that ran it.
2. **Shutdown order.** `shutdownControlPlane` (`cmd/server/main.go`) stops background workers first, then drains HTTP, then the presence workers, then the hub (which closes sockets), and closes NATS **last**. So a stopping colour keeps applying `voice.*` events until the very end of its stop grace, but its sweepers stop at once.

The cutover has four overlap windows:

| Window | Serving colour (nginx) | Other colour | Who has WebSocket clients | Length |
|---|---|---|---|---|
| W1 | home | green booting, waiting for `/readyz` | home only | green's boot time |
| W2 | green (after flip 1) | home stopping | both: home drains its old sockets | ≤ 45 s (stop grace, then SIGKILL) |
| W3 | green | home booting, waiting for `/readyz` | green only | home's boot time |
| W4 | home (after flip 2) | green stopping | both: green drains its old sockets | ≤ 45 s |

Process overlap (W1 to W4) is longer than the 45 s nginx split, as `architecture.md` says. **Client overlap** (both colours holding sockets) is only W2 and W4. In W1 and W3 the booting colour has no clients and receives no HTTP, so it can apply shared writes but cannot deliver anything to anyone.

Green's `MIGRATIONS_MODE=verify` gates only the startup migration run and the ops-metrics reader role (`cmd/server/main.go`, `parseMigrationsMode` and its two consumers). `OPS_METRICS_ENABLED=false` turns off the ops-metrics collector. Every other worker and subscriber runs on green too. That is correct, and suppressing tickers in verify mode is not a general mitigation: in W3 green is the only full server, so it must run its workers.

## 3. Recommendation table

Class: **D** = can disclose data, **A** = can wrongly authorize, **F** = fails closed or availability only, **I** = idempotent duplicate work.

| # | Assumption or state | What goes wrong in a realistic overlap | Class | Decision | Smallest mitigation |
|---|---|---|---|---|---|
| V1 | `voice.*` has no queue group; both colours apply every `voice.joined` | The second applier re-upserts (equal stamp passes `<=`), finds the admission consumed, logs `Rejected voice.joined pending admission` and publishes an exact force-disconnect. Every admitted join in W1 to W4 is kicked. | F | **Mitigate before activation:** fix in draft PR #281 | A join whose row already holds its exact stamp is a replay: skip the reservation consume and the force-disconnect. Regression tests in #281. |
| V2 | Only the process whose commit applies a voice event broadcasts it | If the other colour wins the commit, the serving colour's clients miss that `voice_state_update`. In W1 and W3 the winner can be the colour with no clients. | F | Accept | None. Rosters converge at the reconnect each leg forces (Desktop#68 refetch). |
| V3 | Receipt-time clamp in `parseVoiceEventTime` assumes one consumer | Holds. Both colours run on one host, so they share a clock. `LockServerVoiceLifecycleTx` (per-sender, in Postgres) and the Redis lifecycle claim serialize the two appliers. The `lifecycle_event_at <= $3` fence compares in Postgres. A second applier can only repeat or lose, never advance a stamp past a newer row. | I | Accept | Update the backend.md sentence that calls fan-out a future topology. The rail makes it a deploy-time topology. Docs only. |
| V4 | The refresh-only (room-placement) join path has the same admission-mismatch kick (`handleRefreshOnlyLifecycleEvent`) | Same shape as V1, but reachable only with `MEDIA_DEPLOY_PROFILE=media-bluegreen`. The rail refuses that combination. | F | Accept | Give it the same replay rule as V1 if that refusal is ever lifted. |
| P1 | Presence-audience revocation fence (`presenceAuthzState`) is process-local | In W2 and W4, a block, kick, ban or removal runs on the serving colour. The draining colour's audience worker cannot see the open bracket, and the revocation's clear and abandon reach only the serving colour's hub. A viewer still connected to the draining colour can receive, or keep, the sender's Rich Presence or Custom Status until that socket closes. | D | Accept, bounded | Bounded by the draining colour's socket lifetime: ≤ 45 s per leg, and much shorter once Desktop#64 makes clients leave on `session_drain`. Fallback if Desktop#64 slips: close all control sockets at the start of shutdown, not at the hub stage. |
| S1 | Session and family revocation closes sockets through the local hub (`loginfamily.CloseCommittedFamilies` → `Hub.DisconnectFamilyContext`) | A logout, revoke-all, password change or theft response handled by the serving colour does not close a socket for that session held by the draining colour. That socket keeps receiving realtime events until the draining colour closes it. Writes stay fenced: WebSocket persists run `credepoch.GuardFamilyTx` in the database. | A (read only) | Accept, bounded | Same bound and fallback as P1. |
| S2 | Other hub-side enforcement (RBAC subscriber pruning, permission re-push) | Same class as S1. It runs on the colour that handled the change. The draining colour's subscriptions are not pruned until its sockets close. This was not traced per call site. | A (read only) | Accept, bounded | Same as P1. Verify step 4 on the host should include one revocation during W2. |
| H1 | Activity History has a single writer | Cluster mode would run two writers. | D/A | Already mitigated | The rail refuses `ACTIVITY_HISTORY_CLUSTER_ENABLED=true` (`rail-refused-activity-history-cluster`). |
| B1 | Background sweepers and reconcilers (active-presence plans, pending presence ops, token-theft replay, DM retirement and clear-reap, expiry purge, billing outbox and drift, subscription expiry, temp-grant sweep, voice-command outbox, feedback uploads, tier-1 erasure) | Each claims its rows in Postgres (`FOR UPDATE`, `SKIP LOCKED`, a lease, or a conditional UPDATE/DELETE), so one colour does the work. Pushes from the winner (entitlement downgrade, purge announcement, ownership completion) reach only the winner's clients. | I, F | Accept | None. The other colour's clients converge at reconnect. Entitlement and permission caches are in Redis, so enforcement is shared. |
| B2 | Object-store reapers with no claim (purge straggler sweep, orphan reaper, multipart-upload sweeper) | Both colours can delete the same keys. S3 deletes are idempotent. The straggler sweep can count a failed attempt twice, and the multipart sweeper can log an abort failure. | I | Accept | None. In practice most of these run hourly or daily and stop at SIGTERM. |
| B3 | Ops-metrics collector writes samples keyed by `node_id` | Two writers with one `node_id` would interleave the WebSocket gauge. | I | Already mitigated | Green starts with `OPS_METRICS_ENABLED=false`, so only home collects. |
| B4 | Attestation registry cache answers allow/deny in memory | Invalidation is already cross-process: NATS `attestation.registry.*` plus a 5 min poll. The revoked set is in Redis. | none | Accept | None. |
| B5 | `voice.tier_violation` counting | Already in a queue group (`control-plane.tier-violation`), and the wildcard copy is dropped. Counts once. | none | Accept | None. |
| B6 | Voice-command transport bootstrap runs on each boot | `BootstrapTransport` is written for a competing bootstrap: it re-reads the committed receipt and only verifies when one exists. | I | Accept | None. |

## 4. Findings in detail

### 4.1 Voice lifecycle subscription (V1 to V4)

The `voice.*` wildcard is a plain subscription (`internal/voice/nats.go`, `NATSSubscriber.Subscribe`), so core NATS gives both colours every event. The code was already written for that: the database work runs under `LockServerVoiceLifecycleTx`, and `claimVoiceLifecycleStatus` records the event in Redis and answers `fresh` or `duplicate`. Rows, leases and terminal effects are therefore correct with two appliers.

V1 is the exception, and it comes from the reservation. `moveServerVoiceParticipant` upserts with `WHERE participant.lifecycle_event_at <= EXCLUDED.lifecycle_event_at`. An exact duplicate has an equal stamp, so the upsert reports one row, and the function then requires `DELETE FROM voice_pending_admissions … ` to remove exactly one row. The first applier already removed it, so the second gets `errVoicePendingAdmissionMismatch`. `handleServerVoiceJoined` maps that error to `publishForceDisconnect(channel, user, socketID, admissionID)`, which tells the media plane to eject that exact peer. The mutation closure ignores the claim status, so a known duplicate still takes this path.

Why not a queue group on `voice.*`:

- The hub is process-local. With a queue group, NATS would hand roughly half the events to the colour with no clients in W1 and W3, and the serving colour's clients would miss them: V2 for every event instead of only the races it loses.
- It changes a subscription whose per-replica delivery other code relies on. The tier-violation door de-duplicates on its own queue group, and the wildcard drops that copy by subject.
- It does not remove the race in W2 and W4, because each queue member still drives its own local fan-out.

Shortening the overlap does not fix V1 either. Closing the draining colour's NATS connection at SIGTERM would remove W2 and W4 from V1's window, but W1 and W3 remain, and they last as long as a boot.

### 4.2 Presence revocation fence (P1)

`Hub.BeginAudienceRevocation` raises a counter in the hub of the process that runs the revoking transaction. An audience worker in the other process reads its own counter, which is zero. backend.md already says this: "the fence is process state, and therefore blind across replicas".

What limits it in practice:

- In W1 and W3 the colour that could miss the bracket has no clients, so it has no one to deliver to.
- In W2 and W4, revoking HTTP requests reach only the serving colour, because nginx has flipped. The draining colour can still compute an audience, for example from a voice event it applies or a presence frame from one of its remaining sockets. A committed revocation is visible to it in the database. Only a revocation that is still open while its audience query runs is unproven.
- Separately, the revocation's clear and fail-closed abandon act through the serving colour's hub. A viewer on the draining colour keeps what it already received until its socket closes. Custom Status has no TTL, so it stays visible until the client reconnects and refetches (Desktop#68).

Disclosure: yes, presence-level data only (voice activity, Custom Status) to a viewer whose authorization was just removed, for at most the draining colour's remaining socket lifetime.

### 4.3 Session and permission enforcement (S1, S2)

Not on the review's list, but the same class. `loginfamily.CloseCommittedFamilies` closes terminal families through `Hub.DisconnectFamilyContext`, which acts only on the local hub. This repository has no cross-process close path for families. A revoked session's socket on the draining colour keeps receiving realtime events until that colour's hub closes it. It cannot write: message persists run `credepoch.GuardFamilyTx` inside their transaction. RBAC subscriber pruning is hub-local in the same way; it was not traced call by call.

### 4.4 Background work (B1 to B6)

The inventory covered every ticker and long-running worker the server binary starts (`cmd/server/main.go`, `internal/api/router.go`). Nothing found double-charges, double-emails, double-publishes or double-disconnects: each one either claims rows in Postgres or is an idempotent delete. Two side effects exist: pushes reach only the winner's clients, and some object-store retries are counted twice. Both are availability only. Workers stop first at shutdown, so a draining colour runs none of them in W2 or W4.

## 5. Reproduction of V1

Run against a disposable Postgres and Redis (`DATABASE_URL`, `REDIS_URL`) with two subscribers that share one database and Redis. Each subscriber stands for one process:

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

That log line is emitted immediately after `publishForceDisconnect` in `handleServerVoiceJoined`. The test client has no NATS, so the publish itself is a no-op here. In production the media plane receives it. For the regression test, assert that no exact force-disconnect is requested on the second call; it needs a NATS stub or a hook. With the fix in place, the test must go red when the replay rule is removed.

## 6. Verified and not verified

Verified here: V1 (by test); the voice claim and lock path, the shutdown order, the hub's local-only fan-out and the family close path (by reading the code); and B1 to B6 (by an inventory of every worker the server binary starts).

Not verified, and to be covered by step 4 of the on-host checklist in `architecture.md`:

- the boot time of each colour, which sets the length of W1 and W3;
- how long real clients stay on the draining colour in W2 and W4, before and after Desktop#64;
- one revocation (logout, block or kick) during W2, to confirm the P1/S1 bound on a real host;
- the managed-Redis acknowledgement path, which the rail does not reach today.

## 7. Proposed gate decision for #253

> Third gate (two-process overlap): **blocked on V1 until #281 merges.** Fix: a duplicate admitted `voice.joined` whose row already carries its stamp is a replay, not a reservation mismatch, with two-subscriber regression tests. All other overlap effects are accepted: the shared-store writes are already serialized, and the process-local residuals (presence fence, session close, subscriber pruning) can disclose only to sockets still on the draining colour, for at most 45 s per leg. That bound depends on Desktop#64. If Desktop#64 does not shorten the drain, close control sockets at the start of shutdown before activating.
