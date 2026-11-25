# Media socket bounds: displaced-socket eviction, voice identity ledger limits, bounded logging (issue 176)

- **Status:** Final spec (architect Mode 3, post-panel reconciliation). Supersedes the Mode-2 design philosophy and the system engineer's technical approach where they differ.
- **Date:** 2026-11-18
- **Base:** `main` @ `b23fe7d7`, `services/media-plane`
- **Findings:** VULN-159, VULN-146 (Medium); VULN-160, VULN-158, VULN-157, VULN-147 (Low)
- **Frontend:** out of scope. No client release is needed.
- **Dependency contracts read for this spec (Socket.IO 4.8, winston 3.x):**
  - `socket.disconnect(true)` closes the underlying connection. The socket leaves every room and `connected` becomes false before the `disconnect` event fires, and the listeners run synchronously inside the call. A second `disconnect()` is a no-op.
  - `adapter.rooms` is a synchronous `Map<room, Set<socketId>>`.
  - A winston format may return a new info object. Internal state lives in the `triple-beam` symbol keys (`LEVEL`, `MESSAGE`, `SPLAT`).

## 1. Problem

The media plane keeps per-socket and per-process state that it never re-validates, and it trusts several client-supplied values:

- A socket displaced by a second DM join stays in the Socket.IO room and keeps receiving room events after access is revoked.
- 63 accounts can fill the 1000-slot voice identity ledger with idle sockets, so every new voice connection on the node is refused.
- `set-deafen` broadcasts for a caller that is not an admitted participant.
- One viewer can flap the room-wide camera layering gate. Each flip makes every camera publisher re-create its producer.
- `consume` fields and the handshake `username` reach the log with no type or length bound.

## 2. Decided inputs (not open)

- Media plane only. No control-plane per-user cap.
- `PER_USER_CONNECTIONS = 4`. `CAPACITY` stays 1000.
- 200 slots are reserved for users with zero identities, and the reserve also covers `reserveRoom`.
- A connection that is not in a room for 30 s is released through `terminate()`, including after `leave-room`. Counters are never decremented directly.
- DM displacement calls `disconnect(true)`, as the channel path does.
- `set-deafen` requires an admitted participant on the calling socket.
- The camera gate counts distinct viewers and limits each member to 2 flips per 30 s. Flip state lives in a `Map<userId, ...>` on `Room`, not on `Participant`.
- Logged client values are bounded by a winston format.
- Limits are constants, not env vars.
- `lib/admissionGate.ts` and `index.ts:1216` are not touched (PR #294 owns them). The per-IP handshake limit is out of scope.

## 3. Reconciliation rulings

Rows 9 and 10 are architect amendments found during reconciliation. Neither earlier document had them.

| # | Topic | Positions | Ruling | Reason |
|---|---|---|---|---|
| 1 | Home of the commit functions | Mode 2: add only a helper file. Engineer: move `commitChannelAdmission` and `commitDMAdmission` into `lib`. | Engineer | `src/index.ts` is excluded from coverage and cannot be imported. The defect was a forgotten call site, so the regression test must drive production code, not a copy. |
| 2 | Idle re-check inside `releaseRoom` | Mode 2: yes. Engineer: no, the sweep is the only caller of `terminate()`. | Engineer | `terminate()` runs Socket.IO listeners synchronously, which would re-enter the ledger mid-mutation from the join `finally` block. |
| 3 | `storedDemand()` signature | Mode 2: add a viewer parameter. Engineer: keep it and add `userId` at the one camera write site. | Engineer | 21 test callers pass `maxSpatialLayer` second. A reordered parameter would silently fall back to the default layer. |
| 4 | Log limits | Mode 2: five limits. Engineer: add a per-line node budget and a key bound. | Engineer | 32 entries at depth 4 allows about one million leaves in one line. |
| 5 | Who is charged for a demand removal | Mode 2: the caller. Engineer: the demand's owner. | Engineer | A server-observed consumer close has no caller in scope. |
| 6 | Sweep enumeration | Issue text: `io.in(room).fetchSockets()`. Engineer: `adapter.rooms`, captured before the first await. | Engineer | `fetchSockets` is async and opens a window in which a successor can commit. The adapter is also the ground truth for who receives broadcasts. |
| 7 | Per-node limits | Mode 2: private note. Engineer: public fact. | Engineer | The decision record says the limit is per node. |
| 8 | `docs/api` | Mode 2: update if the socket contract is there. Engineer: it is not. | Engineer | `docs/api` holds only the control-plane contract. |
| 9 | Flip attribution | Mode 2 and engineer: charge the member whose recompute applies the transition. | Neither. Charge the member whose change moved the computed gate value (section 4.5). | Under the earlier rule, an over-budget member's deferred transition is applied by the next recompute from any other member, who is then charged. One member could flap the gate at the room's recompute rate and spend other members' budgets. |
| 10 | Join in flight and the idle release | Mode 2 and engineer: a join in flight suspends the release for as long as it runs. | Suspended for at most `IDLE_JOIN_GRACE_MS` (30 s) more. | `withKeyedJoinFence` awaits `fence.acquire(roomId)` with no timeout, so the join duration has no bound. `terminate()` during a join is already a supported path. |
| 11 | Reserve eligibility for rooms | Open in both documents. | A room may use the reserve when the user holds exactly one connection and no room. | The decision says the reserve "also covers `reserveRoom`". At `reserveRoom` time a user always holds a connection, so no other reading gives the clause any effect. |
| 12 | `Error` values in log meta | Engineer asked: keep `{}` or surface `name`, `message`, `stack`? | Keep today's output (enumerable own properties only). | Surfacing them adds data to every error line, some of it client text. That is a separate observability change. |
| 13 | Handshake `displayName` and `avatarUrl` | Engineer asked whether to bound them. | Not bounded here. | They are not logged, the format bounds them if a later line logs them, and the control-plane identity is authoritative. Their product limits are unknown. |
| 14 | Publisher-caused gate changes | Engineer asked whether to charge them. | Every recompute site passes the member whose state it changed. | `update-rtp-capabilities` can change the codec kind and so the gate. Under ruling 9 passing the member costs nothing. |
| 15 | Sweep payload | Engineer asked: silent, or send `force-disconnect`? | Silent `disconnect(true)`. | It matches displacement eviction. The stale socket is not a participant. |
| 16 | `setTestingStatus` | Scout expected the `set-deafen` defect. | No code change, one regression test. | `setTestingStatus.ts:42-43` already requires `participant?.socketId === socket.id`. |
| 17 | Revocation sweep in scope | Open in Mode 2. | In scope. | The revocation criterion and the behavioural half of the 159 exploit test depend on it. |

## 4. Design

Every rule has one owner, and every new rule lives in `src/lib` or `src/middleware` so that it is covered.

### 4.1 Displaced-socket eviction and revocation sweep (VULN-159)

New `src/lib/roomAdmissionCommit.ts` becomes the only home of `RoomJoinValue`, `rollbackSocketMembership`, `commitChannelAdmission`, `commitDMAdmission` and `evictDisplacedSocket`. They move out of `index.ts:400-489`.

Both commit functions follow one order:

1. Capture `displaced = roomManager.getParticipant(roomId, data.userId)?.socketId`, synchronously, before promotion.
2. Promote. A throw rolls back socket membership and rethrows, as today. Nothing is evicted.
3. After the `try` block, with no `await`, call `evictDisplacedSocket(socket.nsp.sockets, displaced, socket.id, { roomId })`.

`evictDisplacedSocket` does nothing when `displaced` is absent or equals the current socket. Otherwise it calls `disconnect(true)` inside try/catch, logs a failure with `roomId` and `displacedSocketId` only, and never throws. Today's channel path evicts inside the `try`, so a throwing listener rolls back the successor. That latent defect is fixed.

The helper only disconnects. It does not call `leave()` or clear `data.roomId` first, which the issue text suggested. `handleLeaveRoom` returns early without `data.roomId`, and that would skip the displaced session's release. The displaced socket's own `disconnect` handler already takes the ownership-guard branch (`index.ts:2350-2366`) and its middleware listener calls `terminate()`, so its identities are released.

**Sweep.** `handleForceDisconnect` gains a backstop. `ForceDisconnectIO.sockets` gains a required `adapter: { rooms: Map<string, Set<string>> }`. An optional field would let the sweep fail open.

- Gate: `options.socketId === undefined`, the reason is not `stats_unavailable`, and when `options.callId` is set the room is missing or `room.callId === options.callId`.
- Capture, after `resolveExactSessions` and before the first `await`: the socket ids in `adapter.rooms.get(channelId)` whose `data.userId` equals the target user, minus the exact participant and exact provisional socket ids.
- Act, after the exact teardown, and in the "already left" branch before `awaitNativeTermination`: disconnect each captured socket if it is still the same `Socket` object and is not the current owner of the participant or provisional entry.

Exact-socket commands never sweep.

### 4.2 Ledger partition and per-user cap (VULN-146)

Exported constants in `voiceAuthConnection.ts`: `PER_USER_CONNECTIONS = 4`, `NEW_USER_RESERVE = 200`. The general pool is `CAPACITY - NEW_USER_RESERVE` = 800. No new counter is added, so nothing can drift.

| Method | Checks, in order |
|---|---|
| `reserve()` | 1. `retained >= 1000` refuses. 2. `held >= 4` refuses with the per-user error. 3. `retained >= 800 && held !== 0` refuses. 4. Otherwise reserve and set `idleSince = now`. |
| `reserveRoom()` | 1. `assertProductive`, as today. 2. `retained >= 1000` refuses. 3. An overdue connection refuses with `Voice connection idle` (section 4.3). 4. `retained >= 800` refuses unless `userConnections.get(userId) === 1 && state.rooms.size === 0`. 5. Owner checks, as today. |

- `held` still drops only after the signed release, so a terminal identity awaiting release counts per user and makes the user reserve-ineligible.
- Legacy room reservations are unchanged. Their only caller is the shutdown drain, so a client cannot use them to take reserve slots.
- The limits are per node. A user can hold 4 connections on each node.

**Resulting bound.** Filling the node needs at least 200 accounts: 100 accounts holding 4 connections and 4 rooms fill the general pool, and each reserve slot pair needs an account that held nothing. Today it needs 63. Room access is not a barrier, because any two accounts can open a DM call. A test pins the figure.

### 4.3 Idle release (VULN-146)

Exported constants: `IDLE_RELEASE_MS = 30_000`, `IDLE_JOIN_GRACE_MS = 30_000`, `IDLE_SWEEP_INTERVAL_MS = 5_000`. `ConnectionState` gains `idleSince?: number` on the injected clock.

- `reserve()` sets `idleSince`. `attachRoom()` clears it. "Attached" means the control-plane room registration succeeded, which happens after authorization.
- `releaseRoom()` sets `idleSince = now` when it first makes an attached room terminal and no other attached, non-terminal room remains. A room that never attached leaves the clock alone, so a failed join does not reset it.
- A connection is overdue when `now - idleSince >= IDLE_RELEASE_MS`. `reserveRoom()` refuses an overdue connection.
- `terminateIdle()` collects candidates first and then calls `terminate(handle)` on each. A candidate is `provisional` or `registered`, has no `terminal`, and either:
  - is overdue and every child room is terminal; or
  - has `now - idleSince >= IDLE_RELEASE_MS + IDLE_JOIN_GRACE_MS`, whatever its unattached rooms are doing.
- `terminateIdle()` does nothing while draining. It is the only new caller of `terminate()`.
- `index.ts` gets one interval that calls `terminateIdle()` every 5 s. It is cleared in `completeShutdown` beside `voiceEnforcementLeaseInterval`.

Bounds: a socket that never joins is released within 30 to 35 s. A socket with a join held in flight is released within 65 s.

### 4.4 `set-deafen` admission check (VULN-160)

`SetDeafenRoomManager` gains `getParticipant`, and `SetDeafenSocket` gains `id`. After the payload check, the handler returns `{ error: 'Not in a room' }` unless `getParticipant(roomId, userId)?.socketId === socket.id`. It then mutates nothing and emits nothing. The call site (`index.ts:2247`) already passes the real objects and does not change.

### 4.5 Camera gate (VULN-158)

**Distinct viewers.** New `ViewerCameraLayerDemand extends StoredCameraLayerDemand { userId: string }` in `cameraLayerGovernor.ts`. `StoredScreenLayerDemand` re-bases onto it. `room.cameraLayerDemands` stores it, and the one write site (`roomManager.ts:4768`) stores `{ ...stored, userId }`. `computeCameraLayeringGate` applies its threshold (2 to enable, 1 to stay enabled) to distinct `userId`s among visible demands. `storedDemand()` keeps its signature.

**Flip budget.** `set-preferred-layers` allows a burst of 200 and 20 per second, so the event budget does not bound flips. The `Room` gains one optional, lazily created field beside `cameraGateOffTimer`:

```ts
cameraGateBudget?: {
  flips: Map<string, number[]>;   // userId -> times of applied transitions inside the window
  desired: boolean;               // last computed gate value
  desiredCause?: string;          // member whose change last moved `desired`
  deferTimer?: ReturnType<typeof setTimeout>;
};
```

`recomputeCameraLayeringGate(room, causeUserId?)` stays synchronous:

```
next = compute()                         // inputs unchanged, hysteresis still reads the applied flag
if next !== desired: desired = next; desiredCause = causeUserId
settle:
  if desired === applied: clear the OFF timer and the defer timer; return
  wait = desiredCause ? flipBudgetDelayMs(flips.get(desiredCause), now) : 0
  if wait > 0: clear the OFF timer; arm one defer timer for `wait`; return
  if desired: apply ON now; charge desiredCause; emit
  else if no OFF timer: arm the existing 1500 ms OFF timer
        at fire: re-run the live computation as today; if still OFF, apply, charge desiredCause, emit
defer timer fire: check the room is still current, then recompute with no cause
```

- A charge records an applied transition. A change that the OFF debounce absorbs costs nothing.
- A recompute that does not move `desired` never changes the attribution, so another member cannot apply, or be charged for, a pending transition.
- Every call site passes the member whose state it just changed: the viewer for `set-preferred-layers`, the demand's owner for a demand removal (read before the delete), the participant for leave, promotion, produce, producer close, native retire and `update-rtp-capabilities`. Only the two timers pass no cause.
- `chargeFlip` prunes every user's timestamps to the window and deletes empty keys, so the map does not grow with room lifetime.
- The budget and its timer are cleared at both room-teardown sites (`roomManager.ts:2234`, `:2293`).
- The demand is still stored and applied to the consumer, and the ack is unchanged. A refusal would leave Desktop's layer stuck, because Desktop does not resend an unchanged payload.

Two consequences are accepted. An over-budget member's ON transition is deferred, which is an exception to "gate-ON is immediate". While a transition is pending, a later change by another member in the same direction does not release it early. In both cases the gate lags by at most 30 s (31.5 s for OFF), and the gate is a cost optimization, not an access control.

### 4.6 Bounded logging and wire checks (VULN-157, VULN-147)

New `src/lib/logBounds.ts` exports `boundedFields()` and `boundLogValue()`. `logger.ts` exports `createLogFormat(environment)` and builds the chain `timestamp()`, `errors({ stack: true })`, `boundedFields()`, then `json()` or `colorize()` plus `printf()`.

| Constant | Value |
|---|---|
| `LOG_FIELD_MAX_CHARS` | 256 |
| `LOG_MESSAGE_MAX_CHARS` | 2048 |
| `LOG_STACK_MAX_CHARS` | 8192 |
| `LOG_MAX_DEPTH` | 4 |
| `LOG_MAX_ENTRIES` | 32 per object or array |
| `LOG_MAX_KEY_CHARS` | 64 |
| `LOG_MAX_NODES` | 256 per info object |

- The format returns a new info object and never mutates a caller's object. Symbol keys are copied verbatim.
- Strings are cut with a marker that states the dropped length. Values past the depth become `[Object]` or `[Array]`, ancestor cycles become `[Circular]`, and values past the node budget become `[Truncated]`.
- A value with `toJSON` has it called inside try/catch and the result is bounded, so `Date` and `Buffer` output is unchanged.
- If the transform throws, the format returns the level, a bounded message and `logBoundFailure: true`.
- Worst case for one line is about 100,000 characters before JSON escaping.

New `src/lib/wireBounds.ts`:

- `WIRE_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/` and `isBoundedWireId(value)`.
- `parseConsumeRequest(payload)` returns `{ producerId, transportId? }` or `undefined`. `producerId` must match the pattern. `transportId` must be `undefined` or match it, because the resolved transport is authoritative. A non-object payload is refused.
- The `consume` handler calls it before any log line and acks `{ error: 'invalid_payload' }` on refusal.
- `HANDSHAKE_USERNAME_MAX_CHARS = 64`. `middleware/auth.ts` rejects a longer `username` before JWT verification with `Username too long`, in its own branch that calls `deny()` like its siblings. The product maximum is 32 and registration accepts up to 50.
- The `Client connected` log drops `username`.

## 5. Client-visible behaviour

Every change is a refusal or a disconnect that existing clients already handle.

| Surface | Change | What the client sees |
|---|---|---|
| Handshake | A user's 5th concurrent connection is refused. At 800 or more retained identities, only a user with none is admitted. | `connect_error`, `Voice authentication unavailable` (same string as today) |
| Handshake | `username` longer than 64 characters | `connect_error`, `Username too long` |
| Idle socket | Not in a room for 30 s | Server disconnect, reason `io server disconnect` |
| `join-room` | Refused for an overdue connection, or under fill unless it is the user's first room on a single connection | Existing join error ack |
| Second same-account join | The displaced socket is disconnected, for DMs as for channels | Displaced client: `io server disconnect` |
| User-wide revocation | Stale same-user sockets in the room are disconnected | `io server disconnect`, no payload |
| `set-deafen` | Requires the admitted participant on this socket | `{ error: 'Not in a room' }` |
| `consume` | Ids are validated before logging | `{ error: 'invalid_payload' }` |
| `set-preferred-layers` | Ack unchanged | The room-wide gate event may arrive up to 30 s late |

Desktop was checked for this spec. It creates the media socket immediately before `join-room`, disconnects it on leave, never sends a null `transportId`, and starts a rebuild as soon as a reconnect lands.

## 6. Tests

**Exploit tests (security tracker; run with its media-plane vitest config; both files must fail against the fix).**

- `test_vuln_159_displaced_dm_socket.test.ts`
  - The source pin fails because `commitDMAdmission` is no longer defined in `index.ts`.
  - The behavioural test uses a copy of the old commit function, so only the sweep makes it fail. It fails on `expect(s1.closed).toBe(false)`.
- `test_vuln_146_voice_ledger_exhaustion.test.ts`
  - The behavioural test fails at `attacker 0 socket 4`.
  - The static test fails on `const PER_USER_CONNECTIONS = 16;`.

**Regression tests in `services/media-plane/tests`.**

- VULN-159, `roomAdmissionCommit.test.ts` (loopback Socket.IO server, real commit functions):
  - a second DM join disconnects the first socket and removes it from `adapter.rooms`;
  - the same for a channel;
  - a failed promotion evicts nothing;
  - a same-socket re-commit does not evict itself;
  - the displaced socket's cleanup leaves the successor's participant, session generation and room membership intact;
  - a throwing `disconnect` is logged with ids only and the promotion stands.
- Sweep, `forceDisconnect.test.ts`:
  - a user-wide command disconnects a stale same-user socket;
  - other users' sockets are untouched;
  - an exact-socket command and `stats_unavailable` do not sweep;
  - the sweep runs when the user has no participant left;
  - a `callId` mismatch on an existing room does not sweep;
  - a successor that commits during the teardown awaits is not swept;
  - after revocation no socket of the account receives `io.to(room)` events.
- VULN-146 cap, `voiceAuthCapacity.test.ts`:
  - a user's 5th connection is refused with the per-user error;
  - **three rapid reconnects with a delayed control-plane release are all admitted, a fourth is refused, and it is admitted once one release lands.**
- VULN-146 reserve:
  - at 800 retained, a user with identities is refused, and a zero-identity user is admitted, as is that user's first room;
  - a user holding a connection plus a room, or two connections, cannot reserve a room at 800;
  - a user whose only identity is terminal and awaiting release is not reserve-eligible;
  - at 1000 everyone is refused;
  - legacy reservations are unchanged;
  - fill count: 199 greedy accounts leave room for a fresh user to connect and join, and 200 fill the node.
- VULN-146 idle, new `voiceAuthIdleRelease.test.ts`:
  - at 30 s `terminateIdle` goes through `terminate()`, and `retainedIdentityCount()` drops only after the signed release;
  - a room in flight suspends release below 60 s and not at 60 s;
  - a failed join does not reset the clock;
  - an overdue connection's `reserveRoom` throws `Voice connection idle`;
  - the clock restarts when the last attached room is released;
  - an attached room is never idled;
  - nothing happens while draining;
  - a synchronous re-entrant `disconnect` joins the existing terminal task.
- VULN-160, `setDeafen.test.ts`: no participant, a participant on another socket, and an admitted participant. The first two record nothing and emit nothing. `setTestingStatus.test.ts` adds a "roomId set, no participant" case.
- VULN-158 counting: one viewer with many visible consumers does not enable the gate, two distinct viewers do, and one viewer keeps an enabled gate on.
- VULN-158 flip budget, `roomManager.test.ts` with fake timers:
  - a third flip inside 30 s is deferred and applied when the window frees;
  - another member's recompute that does not move the computed value neither applies the pending transition nor is charged;
  - another member's change that moves the value back cancels the pending transition with no flip and no charge;
  - with no transition pending, another member's flip is immediate;
  - the budget survives leave and rejoin;
  - a demand removal is charged to its owner;
  - a codec-kind change through `update-rtp-capabilities` is charged to its caller;
  - a change absorbed by the OFF debounce is not charged, and the timers are not charged;
  - room close clears the defer timer and the budget;
  - the ack is unchanged and the consumer's layers are applied;
  - a user with no flip inside the window has no map key after the next charge.
- VULN-157, `logBounds.test.ts` and `wireBounds.test.ts`:
  - oversized, non-string and control-character ids are refused, and `transportId: undefined` is accepted;
  - the format bounds strings, keys, depth, entries and the node budget, and handles cycles, `toJSON` and `Error`;
  - it does not mutate the caller's object and preserves symbol keys;
  - the production and dev formats from `createLogFormat` are bounded end to end, read from `info[MESSAGE]`.
- VULN-147, `auth.test.ts`: a 65-character `username` is rejected with `Username too long`, 64 is accepted, and the warn log has no `username`.
- Wiring pins, new `mediaSocketBoundsWiring.test.ts` (reads `src/index.ts` as text, because it cannot be imported):
  - the `Client connected` log has no `username` key;
  - the `consume` handler calls `parseConsumeRequest` before its first `logger.` call;
  - `terminateIdle()` is scheduled with `IDLE_SWEEP_INTERVAL_MS` and cleared at shutdown;
  - the commit functions are imported from `lib/roomAdmissionCommit.js` and not defined in `index.ts`.

**Fixture audit (part of T6, T7 and T9).** Run every `voiceAuth*`, `voiceEnforcement*` and `forceDisconnect*` suite. Fix fixtures that reserve more than 4 connections for one user, reserve rooms on non-fresh users past 800, or advance the clock 30 s between `reserve()` and `reserveRoom()`. Keep each test's intent and never relax a constant.

## 7. Acceptance criteria

| Criterion | Required evidence |
|---|---|
| Displaced DM socket leaves the room | Loopback test: first socket disconnected and absent from `adapter.rooms` after the second join |
| Eviction never rolls back a committed promotion | Throwing-disconnect test |
| No socket of a revoked account receives room events | Sweep tests, plus the 159 exploit test failing on both of its tests |
| Per-user cap 4 with reconnect headroom | Three-rapid-reconnect test with delayed release, and the fourth refused |
| Fresh user connects and joins under fill | Reserve tests at 800 and 1000 |
| Accounts needed to fill rise well above 63 | Fill-count test at 199 and 200, plus the 146 exploit test failing on both of its tests |
| Idle identities are released only through `terminate()` | Idle tests, including the 60 s limit and the counter dropping only after the signed release |
| `set-deafen` from a non-participant emits nothing | Three-case handler test |
| One viewer cannot move the camera gate more than 2 times per 30 s, alone or through other members' activity | Distinct-viewer tests and flip-budget tests, including the two "another member" cases |
| No unbounded client value in logs | Format tests, `consume` validation tests, handshake `username` tests, wiring pins |
| No client release needed | Section 5: every change is a refusal or a disconnect |
| Docs to update | Every item in section 8 ships in the same PR |
| New blocker or critical issues | **0** |
| New high-confidence security hotspots | **0** |
| New-code coverage | **≥ 80%** |

## 8. Docs to update

- `.claude/rules/domains/media-plane.md`
  - "Logical login connection ownership": `PER_USER_CONNECTIONS = 4`; the 200-slot reserve and both eligibility rules; terminal identities count; the idle release (30 s, `terminate()` only, 5 s sweep, clock stops on attach, overdue connections cannot reserve a room, 60 s limit for a join in flight); the limits are per node.
  - "Socket.IO", appended at the end of the list (PR #294 edits an early bullet): eviction for every room kind after promotion and never thrown; the user-wide sweep over `adapter.rooms` captured before the first await; the `set-deafen` admission check; `consume` id validation; the handshake `username` bound.
  - "Camera layer-selection gate": distinct viewers; the per-member flip budget on `Room`; attribution to the member whose change moved the computed value; deferral instead of refusal; the exception to "gate-ON is immediate".
  - New "Log bounds" paragraph: the format is the single bound for every log line, with the limits, and `username` and raw payloads are never logged.
- `CHANGELOG.md`: one entry under `[Prerelease]`, `### Security`, covering all six findings.
- `docs/architecture.md`: beside the Socket.IO events paragraph, state that a replaced same-user session's socket is disconnected for channels and DMs, and that a media socket outside a room for 30 s is disconnected.
- `docs/api/`: no change.

## 9. Task breakdown (input for `/superpowers:writing-plans`)

Each task is sized for about 35 tool calls. Three lanes are serial: `index.ts` (T8, then T2, then the T7 wiring step), `voiceAuthConnection.ts` (T6, then T7) and `roomManager.ts` (T4, then T5). T1, T3 and T9 run in parallel with everything.

| Task | Files | Produces | Depends on |
|---|---|---|---|
| T1 Log bounds | create `src/lib/logBounds.ts`, `tests/logBounds.test.ts`; modify `src/lib/logger.ts` | `boundLogValue(value: unknown, budget?: { nodes: number }): unknown`, `boundedFields(): Format`, `createLogFormat(environment: string): Format`, the seven limit constants | none |
| T2 Wire checks | create `src/lib/wireBounds.ts`, `tests/wireBounds.test.ts`; modify `src/middleware/auth.ts:261`, `tests/auth.test.ts`, `src/index.ts` (consume handler `:1965`, connect log `:1767`), wiring pins | `WIRE_ID_PATTERN`, `isBoundedWireId(v: unknown): v is string`, `parseConsumeRequest(payload: unknown): { producerId: string; transportId?: string } \| undefined`, `HANDSHAKE_USERNAME_MAX_CHARS = 64` | T8 (index lane) |
| T3 Deafen check | modify `src/lib/setDeafen.ts`, `tests/setDeafen.test.ts`, `tests/setTestingStatus.test.ts` | `SetDeafenRoomManager.getParticipant`, `SetDeafenSocket.id` | none |
| T4 Distinct viewers | modify `src/lib/cameraLayerGovernor.ts`, `src/lib/screenLayerGovernor.ts`, `src/lib/roomManager.ts` (`cameraLayerDemands` type, `:4768`), governor tests | `ViewerCameraLayerDemand`; `computeCameraLayeringGate` takes `demands: ViewerCameraLayerDemand[]`; `storedDemand` unchanged | none (rebase over PR #303 if it merged) |
| T5 Flip budget | modify `cameraLayerGovernor.ts` (pure helpers, constants), `roomManager.ts` (`cameraGateBudget`, recompute, 12 call sites, teardown at `:2234` and `:2293`), `tests/roomManager.test.ts` | `CAMERA_GATE_MEMBER_FLIP_LIMIT = 2`, `CAMERA_GATE_MEMBER_FLIP_WINDOW_MS = 30_000`, `flipBudgetDelayMs(times: readonly number[] \| undefined, now: number): number`, `chargeFlip(flips: Map<string, number[]>, userId: string, now: number): void`, `recomputeCameraLayeringGate(room: Room, causeUserId?: string): void` | T4 |
| T6 Partition and cap | modify `src/lib/voiceAuthConnection.ts` (`reserve`, `reserveRoom`, exported constants), `tests/voiceAuthCapacity.test.ts`; fixture audit | `PER_USER_CONNECTIONS = 4`, `NEW_USER_RESERVE = 200` | none |
| T7 Idle release | modify `voiceAuthConnection.ts` (`idleSince`, `attachRoom`, `releaseRoom`, overdue check, `terminateIdle`); create `tests/voiceAuthIdleRelease.test.ts`; modify `src/index.ts` (interval, shutdown clear), wiring pin | `terminateIdle(): void`, `IDLE_RELEASE_MS`, `IDLE_JOIN_GRACE_MS`, `IDLE_SWEEP_INTERVAL_MS` | T6, T2 (index lane) |
| T8 Commit move and eviction | create `src/lib/roomAdmissionCommit.ts`, `tests/roomAdmissionCommit.test.ts`, `tests/mediaSocketBoundsWiring.test.ts`; modify `src/index.ts:400-489` and imports | `evictDisplacedSocket(sockets: ReadonlyMap<string, { disconnect(close?: boolean): unknown }>, displacedSocketId: string \| undefined, currentSocketId: string, context: { roomId: string }): boolean` | none (index lane, first) |
| T9 Revocation sweep | modify `src/lib/forceDisconnect.ts`, `tests/forceDisconnect.test.ts`; add `adapter: { rooms: new Map() }` to the `io` fakes (12 test files match). Split the fake update into its own task if the budget runs out. | required `ForceDisconnectIO.sockets.adapter`, sweep inside `handleForceDisconnect` | none |
| T10 Docs and verification | rule file, `CHANGELOG.md`, `docs/architecture.md`; run both exploit tests and record that they fail; run `/verify-quality-gate` | Section 8 | all |

## 10. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| A sybil attacker with 200 or more accounts still fills the node | Accepted | Out of scope: the control-plane cap was dropped and the per-IP handshake limit belongs to PR #294. |
| A user who reconnects during a fill is refused until the old identity's release lands | Low | Socket.IO retries the handshake. Counting only live identities would let 50 accounts drain the reserve. |
| A Desktop reconnect uses two new identities (the reconnected socket and the rebuilt one), so two devices flapping together can reach the cap of 4 while releases are delayed | Low | The refusal clears when a release lands, and the client retries. Pinned by the reconnect test. |
| A join in flight for more than 60 s is terminated | Low | Such a join has already failed for the client. |
| A client holds a registered socket outside a room for more than 30 s | Low | Desktop does not. No other media-socket client was found. |
| The gate lags up to 30 s behind demand while an over-budget member's transition is pending | Medium, low impact | One room timer applies it when the budget frees. |
| Distinct-viewer counting changes when the gate turns on for a lone viewer of several cameras | Low | Matches the screen gate rule. Documented. |
| The format mutates a live object or throws on an unusual value | Medium without care | Copy-only traversal, a fallback object, and tests for cyclic, deep and non-plain values. |
| Making `adapter` required breaks existing fakes | Certain, mechanical | T9 updates them. The type error is the signal. |
| Conflict with PR #303 in `roomManager.ts` | Medium | The `Participant` interface is untouched. Rebase after #303 if it merges first, and audit any recompute call it adds. |

## 11. Calls made in reconciliation (approved by the project lead 2026-11-18), and what was not verified

These three specifics go beyond the decision record. They need the owner's confirmation, and each can be reverted without changing the rest of the design:

1. The room-reserve eligibility rule (ruling 11).
2. `IDLE_JOIN_GRACE_MS = 30_000`, the 60 s limit for a join in flight (ruling 10).
3. The flip attribution rule and the deferred-ON exception (ruling 9).

Not verified:

- Neither exploit test was run. Their failure points are read from the test source.
- The Mobile client was searched for the media-socket event names only. Nothing matched.
- Desktop's `resumeAfterReconnect` body was not read, so the two-identities-per-reconnect figure is inferred from its call site.
- The hunks of PR #303 and PR #294 are as the system engineer reported them. They were not re-read.

