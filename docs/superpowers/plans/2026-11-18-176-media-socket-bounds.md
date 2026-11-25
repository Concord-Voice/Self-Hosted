# Media Socket Bounds (issue 176) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close VULN-159, VULN-146, VULN-160, VULN-158, VULN-157 and VULN-147 in the media plane: evict displaced and revoked sockets, bound the voice identity ledger per user and per idle socket, require admission for `set-deafen`, rate-limit camera-gate flips per member, and bound every logged client value.

**Architecture:** Every new rule lives in a covered module under `services/media-plane/src/lib` or `src/middleware`; `src/index.ts` (excluded from coverage, not importable) only calls them and is pinned by a text-reading wiring test. Ledger limits are derived from the existing `retained` and `userConnections` counters, so no new counter can drift. The camera flip budget is lazily created state on `Room`, attributed to the member whose change moved the computed gate value.

**Tech Stack:** TypeScript (ESM), Node, Socket.IO 4.8, mediasoup, winston 3.x with `triple-beam` symbols, vitest 5.

**Spec:** docs/superpowers/specs/2026-11-18-176-media-socket-bounds-design.md

## Global Constraints

- Media plane only. No control-plane change, no client release, no env vars: every limit is an exported constant.
- `PER_USER_CONNECTIONS = 4`; `CAPACITY` stays `1000`; `NEW_USER_RESERVE = 200` (general pool `CAPACITY - NEW_USER_RESERVE` = 800). Limits are per node.
- `IDLE_RELEASE_MS = 30_000`, `IDLE_JOIN_GRACE_MS = 30_000`, `IDLE_SWEEP_INTERVAL_MS = 5_000`. Idle identities are released only through `terminate()`; counters are never decremented directly.
- `CAMERA_GATE_MEMBER_FLIP_LIMIT = 2`, `CAMERA_GATE_MEMBER_FLIP_WINDOW_MS = 30_000`. `CAMERA_GATE_OFF_DEBOUNCE_MS` (1500) is unchanged.
- Log limits: `LOG_FIELD_MAX_CHARS = 256`, `LOG_MESSAGE_MAX_CHARS = 2048`, `LOG_STACK_MAX_CHARS = 8192`, `LOG_MAX_DEPTH = 4`, `LOG_MAX_ENTRIES = 32`, `LOG_MAX_KEY_CHARS = 64`, `LOG_MAX_NODES = 256`.
- `WIRE_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/`; `HANDSHAKE_USERNAME_MAX_CHARS = 64`.
- Client-visible strings, verbatim: `Voice authentication unavailable`, `Username too long`, `Voice connection idle`, `Not in a room`, `invalid_payload`.
- Do not touch `src/lib/admissionGate.ts` or the per-IP handshake limit at `src/index.ts:1216` (owned by PR #294). Do not change the `Participant` interface (PR #303).
- `storedDemand(demand, maxSpatialLayer)` keeps its signature.
- `ForceDisconnectIO.sockets.adapter` is required, never optional.
- Commits: Conventional Commits with a bare `type:` (no scope). Never relax a constant to make a fixture pass.
- All commands run from `services/media-plane` unless stated. Unit tests: `npx vitest run <file> [-t "<name>"]`. Full gate: `npm run typecheck && npm run lint && npm test`.

## Review Focus

1. A logged value with a throwing getter, a throwing `toJSON`, or a `Proxy` — the line must still be written (fallback object with `logBoundFailure: true`), never crash the caller. Test added to Task 1.
2. A string cut at `LOG_FIELD_MAX_CHARS` in the middle of a surrogate pair (emoji at index 255) — output must not contain a lone surrogate. Test added to Task 1.
3. A `consume` payload that is `null`, an array, a number, or an object whose `producerId` is a number or 65 characters — ack `{ error: 'invalid_payload' }`, no throw, no log line carrying the raw value. Test added to Task 2.
4. A camera gate defer timer still armed when its room closes and a new room with the same id is created — the stale timer must not touch the new room. Test added to Task 5.
5. The idle sweep after shutdown starts: `terminateAll()` clears `draining` in its `finally`, so `terminateIdle()` would run again once the drain settles. The interval must be cleared before the shutdown drain starts. Wiring pin added to Task 7.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `src/lib/logBounds.ts` (new) | Pure value bounding and the winston `boundedFields()` format | 1 |
| `src/lib/logger.ts` | `createLogFormat(environment)`; logger uses it | 1 |
| `src/lib/wireBounds.ts` (new) | Wire id pattern, `parseConsumeRequest`, handshake username bound | 2 |
| `src/middleware/auth.ts` | `Username too long` branch | 2 |
| `src/lib/setDeafen.ts` | Admission check | 3 |
| `src/lib/cameraLayerGovernor.ts` | `ViewerCameraLayerDemand`, distinct-viewer gate, flip budget helpers | 4, 5 |
| `src/lib/screenLayerGovernor.ts` | `StoredScreenLayerDemand` re-based | 4 |
| `src/lib/roomManager.ts` | Demand type, `cameraGateBudget`, attributed recompute | 4, 5 |
| `src/lib/voiceAuthConnection.ts` | Partition, per-user cap, idle release | 6, 7 |
| `src/lib/roomAdmissionCommit.ts` (new) | Commit functions moved from `index.ts`, `evictDisplacedSocket` | 8 |
| `src/lib/forceDisconnect.ts` | Required `adapter`, user-wide sweep | 9 |
| `src/index.ts` | Imports and call sites only | 8, 2, 7 (serial in that order) |
| `tests/mediaSocketBoundsWiring.test.ts` (new) | Text pins on `src/index.ts` | 8, 2, 7 |

Lanes: `index.ts` is serial T8 → T2 → T7; `voiceAuthConnection.ts` is serial T6 → T7; `roomManager.ts` is serial T4 → T5. T1, T3 and T9 are independent.

---

### Task 1: Log bounds format

Shape: construction

**Files:**
- Create: `src/lib/logBounds.ts`, `tests/logBounds.test.ts`
- Modify: `src/lib/logger.ts:1-24`

**Interfaces:**
- Consumes: none.
- Produces: the seven `LOG_*` constants (Global Constraints); `boundLogValue(value: unknown, budget?: { nodes: number }): unknown`; `boundedFields(): Format` (winston `Format`); `createLogFormat(environment: string): Format`.

- [ ] **Step 1: Write the failing tests in `tests/logBounds.test.ts`**

Import `MESSAGE`, `LEVEL`, `SPLAT` from `triple-beam`. Tests and assertions:

```ts
it('cuts long strings with a dropped-length marker', () => {
  const out = boundLogValue('x'.repeat(300)) as string;
  expect(out.startsWith('x'.repeat(256))).toBe(true);
  expect(out).toContain('44'); // marker states the dropped length
});
it('cuts message at 2048 and stack at 8192', ...)            // via boundedFields().transform
it('cuts keys at 64 chars', ...)
it('replaces values past depth 4 with [Object] / [Array]', ...)
it('keeps 32 entries per object or array and drops the rest', ...)
it('replaces values past 256 nodes with [Truncated]', ...)   // 32 x 32 nested object
it('replaces ancestor cycles with [Circular]', ...)          // a.self = a
it('keeps a repeated non-ancestor reference', ...)            // {x: o, y: o} both bounded copies
it('calls toJSON so Date and Buffer output is unchanged', ...) // equals JSON.parse(JSON.stringify(v))
it('keeps Error meta as enumerable own properties only', ...)   // matches today's JSON output
it('does not mutate the caller object and copies symbol keys', ...) // info[LEVEL], info[SPLAT] identical
it('returns level, bounded message and logBoundFailure on a throwing getter', ...)   // Review Focus 1
it('survives a throwing toJSON and a Proxy whose ownKeys throws', ...)              // Review Focus 1
it('never leaves a lone surrogate when cutting at 256', ...)  // 'a'.repeat(255) + '😀' + 'b'.repeat(10); expect(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])/.test(out)).toBe(false)
it('bounds production format end to end', ...) // createLogFormat('production').transform({level:'info', message:'m', [LEVEL]:'info', f:'x'.repeat(10_000)}); JSON.parse(info[MESSAGE]).f.length < 300
it('bounds dev format end to end', ...)        // createLogFormat('development'); info[MESSAGE].length < 2_000 for the same input
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/logBounds.test.ts`
Expected: FAIL, cannot resolve `../src/lib/logBounds.js`.

- [ ] **Step 3: Implement `src/lib/logBounds.ts` and `createLogFormat` in `src/lib/logger.ts`**

`boundLogValue` is a copy-only traversal with an ancestor stack (for `[Circular]`) and a shared node budget object. `boundedFields()` is `winston.format((info) => ...)()`: build a new object, bound `message` with `LOG_MESSAGE_MAX_CHARS`, `stack` with `LOG_STACK_MAX_CHARS`, every other string key through `boundLogValue` with one budget of `LOG_MAX_NODES`, copy `Object.getOwnPropertySymbols(info)` verbatim, wrap the whole transform in try/catch returning `{ level, message: bounded, logBoundFailure: true, [LEVEL]: info[LEVEL] }`. String cuts back off one code unit when the cut would split a surrogate pair. `createLogFormat(environment)` combines `timestamp()`, `errors({ stack: true })`, `boundedFields()`, then `json()` for `'production'` or `colorize()` + the existing `printf` otherwise. `logger` uses `createLogFormat(config.environment)`.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/logBounds.test.ts && npm run typecheck`
Expected: PASS, no type errors.

- [ ] **Step 5: Commit**

```bash
git add src/lib/logBounds.ts src/lib/logger.ts tests/logBounds.test.ts
git commit -m "fix: bound every logged client value with a winston format"
```

---

### Task 8: Move the commit functions and evict displaced sockets

Shape: defect-repair

(Runs before Task 2 and Task 7: first in the `index.ts` lane.)

**Files:**
- Create: `src/lib/roomAdmissionCommit.ts`, `tests/roomAdmissionCommit.test.ts`, `tests/mediaSocketBoundsWiring.test.ts`
- Modify: `src/index.ts:400-489` (delete the moved code), imports at `src/index.ts:1-50`, call sites at `src/index.ts:841-842` (unchanged names)

**Interfaces:**
- Consumes: `RoomManager.getParticipant`, `promoteChannelParticipant`, `promoteDMParticipant`; `ChannelParticipantPromotion`, `DMParticipantPromotion`, `JoinRoomResult` from `./roomManager.js`; `resolveParticipantIdentity`, `AuthenticatedSocketData` from `../middleware/auth.js`; `logger`.
- Produces:
  - `export type RoomJoinValue` (same fields as today).
  - `export function rollbackSocketMembership(socket: Socket, data: AuthenticatedSocketData, roomId: string): void`
  - `export function commitChannelAdmission(roomManager: RoomManager, socket: Socket, data: AuthenticatedSocketData, roomId: string, value: RoomJoinValue, commitSocketMembership: () => void): RoomJoinValue`
  - `export function commitDMAdmission(...same parameters...): RoomJoinValue`
  - `export function evictDisplacedSocket(sockets: ReadonlyMap<string, { disconnect(close?: boolean): unknown }>, displacedSocketId: string | undefined, currentSocketId: string, context: { roomId: string }): boolean` — returns `true` only when it called `disconnect(true)` and the call did not throw.
  - `tests/mediaSocketBoundsWiring.test.ts` with a shared `const indexSrc = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8')` that Tasks 2 and 7 extend.

- [ ] **Step 1: Write the failing reproduction and regression tests in `tests/roomAdmissionCommit.test.ts`**

Loopback Socket.IO `Server` on an ephemeral port with a real `RoomManager` (mediasoup mocks from `tests/mocks/mediasoup.js`, logger mock `tests/mocks/logger.js`). Use the raw `ws` engine.io client approach from the security tracker's `test_vuln_159_displaced_dm_socket.test.ts`; `ws` is already installed transitively through engine.io, so do not add `socket.io-client`.

```ts
it('a second DM join disconnects the first socket and removes it from adapter.rooms', ...)
  // expect(s1.connected).toBe(false); expect(io.sockets.adapter.rooms.get(ROOM)?.has(s1.id)).toBe(false)
it('a second channel join does the same', ...)
it('a failed promotion evicts nothing', ...)          // promote throws: s1 still connected, error rethrown, s2 rolled back (data.roomId undefined)
it('a same-socket re-commit does not evict itself', ...)
it("the displaced socket's cleanup keeps the successor's participant, session generation and room membership", ...)
it('a throwing disconnect is logged with ids only and the promotion stands', ...)
  // logger.error called with exactly { roomId, displacedSocketId }; getParticipant(roomId, user).socketId === s2.id
```

In `tests/mediaSocketBoundsWiring.test.ts`:

```ts
it('imports the commit functions from lib/roomAdmissionCommit.js and does not define them', () => {
  expect(indexSrc).toMatch(/from '\.\/lib\/roomAdmissionCommit\.js'/);
  expect(indexSrc).not.toMatch(/function commit(Channel|DM)Admission\(/);
});
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/roomAdmissionCommit.test.ts tests/mediaSocketBoundsWiring.test.ts`
Expected: FAIL, module not found; once stubbed, the DM test fails on `s1.connected` being `true`.

- [ ] **Step 3: Implement `src/lib/roomAdmissionCommit.ts` and rewire `src/index.ts`**

Both commit functions: capture `displaced = roomManager.getParticipant(roomId, data.userId)?.socketId` before promotion; promote inside `try` (throw → `rollbackSocketMembership` + rethrow, no eviction); after the `try`, with no `await`, call `evictDisplacedSocket(socket.nsp.sockets, displaced, socket.id, { roomId })`. `evictDisplacedSocket` does not call `leave()` or clear `data.roomId`; it logs a failure with `{ roomId, displacedSocketId }` only and never throws. Delete `index.ts:400-489` and import the four names.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/roomAdmissionCommit.test.ts tests/mediaSocketBoundsWiring.test.ts tests/channelPromotionRollback.test.ts tests/socketJoinLifecycle.test.ts && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/roomAdmissionCommit.ts src/index.ts tests/roomAdmissionCommit.test.ts tests/mediaSocketBoundsWiring.test.ts
git commit -m "fix: disconnect a displaced socket after every room promotion"
```

---

### Task 2: Wire checks for `consume` and the handshake username

Shape: defect-repair

**Files:**
- Create: `src/lib/wireBounds.ts`, `tests/wireBounds.test.ts`
- Modify: `src/middleware/auth.ts:260-267` (new branch after the username-required branch), `tests/auth.test.ts`, `src/index.ts:1768-1772` (connect log), `src/index.ts:1965-1981` (consume handler), `tests/mediaSocketBoundsWiring.test.ts`

**Interfaces:**
- Consumes: `indexSrc` from Task 8's wiring test.
- Produces: `WIRE_ID_PATTERN`; `isBoundedWireId(value: unknown): value is string`; `parseConsumeRequest(payload: unknown): { producerId: string; transportId?: string } | undefined`; `HANDSHAKE_USERNAME_MAX_CHARS = 64`.

- [ ] **Step 1: Write the failing tests**

`tests/wireBounds.test.ts`:

```ts
it('accepts a 64-char id and rejects 65 chars, empty, non-string and control characters', ...)
it('parseConsumeRequest accepts transportId undefined', () =>
  expect(parseConsumeRequest({ producerId: 'p1' })).toEqual({ producerId: 'p1' }));
it('parseConsumeRequest refuses null, arrays, numbers and a null transportId', ...)  // Review Focus 3: each -> undefined
it('parseConsumeRequest refuses a numeric or 65-char producerId', ...)              // Review Focus 3
```

`tests/auth.test.ts` (existing `createMockSocket` helper):

```ts
it('rejects a 65-character username with Username too long before JWT verification', ...)
  // next called with Error('Username too long'); verifySocketToken spy not called; deny() counted; warn meta has no `username`
it('accepts a 64-character username', ...)
```

`tests/mediaSocketBoundsWiring.test.ts`:

```ts
it('the Client connected log has no username key', ...)  // slice from "logger.info('Client connected'" to the next '});'; not.toMatch(/username/)
it('the consume handler calls parseConsumeRequest before its first logger call', ...)
  // slice from "withRateLimit(socket, 'consume'"; indexOf('parseConsumeRequest(') < indexOf('logger.')
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/wireBounds.test.ts tests/auth.test.ts tests/mediaSocketBoundsWiring.test.ts`
Expected: FAIL (module missing; 65-char username admitted; wiring pins fail).

- [ ] **Step 3: Implement**

`wireBounds.ts` per Interfaces. In `auth.ts`, a sibling branch: `if (username.length > HANDSHAKE_USERNAME_MAX_CHARS) { deny(); logger.warn('Socket connection rejected: username too long', { socketId: socket.id }); return next(new Error('Username too long')); }`. In `index.ts`, the consume handler takes `payload: unknown`, calls `parseConsumeRequest(payload)` first and acks `{ error: 'invalid_payload' }` on `undefined`; drop `username` from the `Client connected` meta.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/wireBounds.test.ts tests/auth.test.ts tests/mediaSocketBoundsWiring.test.ts && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/wireBounds.ts src/middleware/auth.ts src/index.ts tests/wireBounds.test.ts tests/auth.test.ts tests/mediaSocketBoundsWiring.test.ts
git commit -m "fix: validate consume ids and bound the handshake username"
```

---

### Task 3: `set-deafen` admission check

Shape: defect-repair

**Files:**
- Modify: `src/lib/setDeafen.ts:4-40`, `tests/setDeafen.test.ts`, `tests/setTestingStatus.test.ts`

**Interfaces:**
- Consumes: `RoomManager['getParticipant']`.
- Produces: `SetDeafenRoomManager.getParticipant: RoomManager['getParticipant']`; `SetDeafenSocket.id: string`. `handleSetDeafen` signature unchanged; the call at `src/index.ts:2248` does not change.

- [ ] **Step 1: Write the failing tests**

`tests/setDeafen.test.ts`:

```ts
it('refuses a socket with roomId but no participant, recording and emitting nothing', ...)
  // result toEqual({ error: 'Not in a room' }); setParticipantDeafen not called; socket.to not called
it('refuses when the participant is on another socket', ...)  // getParticipant -> { socketId: 'other' }; same assertions
it('accepts the admitted participant on this socket', ...)    // { success: true }, emit('participant-deafen-changed', { userId, isDeafened: true })
```

Update the existing fakes to supply `getParticipant` and `id`. `tests/setTestingStatus.test.ts`: add `it('refuses roomId set with no participant', ...)` expecting `{ error: 'Not in a room' }` (regression only; code already checks at `setTestingStatus.ts:42-43`).

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/setDeafen.test.ts tests/setTestingStatus.test.ts`
Expected: FAIL on the first two new `setDeafen` cases (`setParticipantDeafen` called); the testing-status case passes.

- [ ] **Step 3: Implement** the check after the payload check: return `{ error: 'Not in a room' }` unless `roomManager.getParticipant(roomId, userId)?.socketId === socket.id`.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/setDeafen.test.ts tests/setTestingStatus.test.ts && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/setDeafen.ts tests/setDeafen.test.ts tests/setTestingStatus.test.ts
git commit -m "fix: require an admitted participant for set-deafen"
```

---

### Task 4: Camera gate counts distinct viewers

Shape: defect-repair

**Files:**
- Modify: `src/lib/cameraLayerGovernor.ts:18-23,141-159`, `src/lib/screenLayerGovernor.ts:26-29`, `src/lib/roomManager.ts:492` (`cameraLayerDemands` type) and `:4768` (write site), `tests/cameraLayerGovernor.test.ts`, `tests/screenLayerGovernor.test.ts` (type only), any `roomManager.test.ts` literal that builds a camera demand

**Interfaces:**
- Consumes: none.
- Produces: `export interface ViewerCameraLayerDemand extends StoredCameraLayerDemand { userId: string }`; `StoredScreenLayerDemand extends ViewerCameraLayerDemand { sharerUserId: string }`; `computeCameraLayeringGate(input: { codecKind; cameraProducerCount; demands: ViewerCameraLayerDemand[]; previouslyEnabled? }): boolean`; `Room.cameraLayerDemands: Map<string, ViewerCameraLayerDemand>`.

- [ ] **Step 1: Write the failing tests in `tests/cameraLayerGovernor.test.ts`**

```ts
it('one viewer with many visible consumers does not enable the gate', ...)  // 3 demands, same userId, svc, 3 producers -> false
it('two distinct viewers enable the gate', ...)                             // -> true
it('one viewer keeps an enabled gate on', ...)                              // previouslyEnabled: true, one userId -> true
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/cameraLayerGovernor.test.ts`
Expected: FAIL on the first case (returns `true`).

- [ ] **Step 3: Implement** the threshold on `new Set(visible.map((d) => d.userId)).size`; at `roomManager.ts:4768` store `{ ...stored, userId }`; re-base the screen type.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/cameraLayerGovernor.test.ts tests/screenLayerGovernor.test.ts tests/cameraLayerPolicyParity.test.ts tests/roomManager.test.ts && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/cameraLayerGovernor.ts src/lib/screenLayerGovernor.ts src/lib/roomManager.ts tests/
git commit -m "fix: count distinct viewers for the camera layering gate"
```

---

### Task 5: Per-member camera gate flip budget

Shape: defect-repair

**Files:**
- Modify: `src/lib/cameraLayerGovernor.ts` (constants, pure helpers), `src/lib/roomManager.ts` (`Room` beside `cameraGateOffTimer` at `:496`; `recomputeCameraLayeringGate` at `:6254`; the 12 call sites at `:1639, 2490, 2587, 2715, 2933, 3411, 3658, 3966, 4773, 5208, 6295, 6303`; teardown at `:2234` and `:2293`), `tests/roomManager.test.ts`, `tests/cameraLayerGovernor.test.ts`

**Interfaces:**
- Consumes: `ViewerCameraLayerDemand.userId` (Task 4).
- Produces: `CAMERA_GATE_MEMBER_FLIP_LIMIT = 2`; `CAMERA_GATE_MEMBER_FLIP_WINDOW_MS = 30_000`; `flipBudgetDelayMs(times: readonly number[] | undefined, now: number): number` (0 when fewer than 2 times inside the window, else ms until the oldest leaves it); `chargeFlip(flips: Map<string, number[]>, userId: string, now: number): void` (appends, prunes every key to the window, deletes empty keys); `Room.cameraGateBudget?: { flips: Map<string, number[]>; desired: boolean; desiredCause?: string; deferTimer?: ReturnType<typeof setTimeout> }`; `private recomputeCameraLayeringGate(room: Room, causeUserId?: string): void`.

- [ ] **Step 1: Write the failing tests**

`tests/cameraLayerGovernor.test.ts`: `flipBudgetDelayMs(undefined, t) === 0`; `flipBudgetDelayMs([t, t+1], t+2) === 30_000 - 2`; `chargeFlip` prunes an expired key and removes it from the map.

`tests/roomManager.test.ts`, `vi.useFakeTimers()`, gate events captured from `emitEvent`:

```ts
it("defers one viewer's third flip inside 30 s and applies it when the window frees", ...)  // reproduction: today the third flip emits at once
it("another member's recompute that does not move the value neither applies nor is charged", ...)
it("another member's change that moves the value back cancels the pending flip with no charge", ...)
it("another member's flip is immediate when nothing is pending", ...)
it('the budget survives leave and rejoin', ...)
it('charges a demand removal to its owner', ...)
it('charges a codec-kind change via updateRtpCapabilities to its caller', ...)
it('does not charge a change absorbed by the OFF debounce, nor the timers', ...)
it('room close clears the defer timer and the budget', ...)                 // both teardown sites
it('a stale defer timer does not touch a recreated room with the same id', ...) // Review Focus 4
it('keeps the setPreferredLayers ack and applies consumer layers while deferred', ...)
it('a user with no flip inside the window has no map key after the next charge', ...)
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/roomManager.test.ts -t "flip" tests/cameraLayerGovernor.test.ts`
Expected: FAIL; the first case sees three gate events inside 30 s.

- [ ] **Step 3: Implement the attributed recompute**

Follow spec section 4.5 exactly (the `desired`/`desiredCause` settle algorithm; the OFF timer re-runs the live computation and charges `desiredCause`; the defer timer checks `this.rooms.get(room.id) === room` and recomputes with no cause). Call sites pass: the viewer at `:4773`; the demand owner read before delete at `:6295` (`room.cameraLayerDemands.get(consumerId)?.userId`) and the participant at `:6303`; the participant's `userId` at leave/promotion/produce/producer-close/native-retire sites and at `:5208`. Clear `deferTimer` and `cameraGateBudget` beside `cameraGateOffTimer` at `:2234` and `:2293`.

- [ ] **Step 4: Run to verify pass**

Run: `npx vitest run tests/roomManager.test.ts tests/cameraLayerGovernor.test.ts tests/layeringGateBroadcast.test.ts && npm run typecheck`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/cameraLayerGovernor.ts src/lib/roomManager.ts tests/roomManager.test.ts tests/cameraLayerGovernor.test.ts
git commit -m "fix: limit each member to two camera gate flips per 30 seconds"
```

---

### Task 6: Ledger partition and per-user cap

Shape: defect-repair

**Files:**
- Modify: `src/lib/voiceAuthConnection.ts:59-63` (constants), `:195-221` (`reserve`), `:286-296` (`reserveRoom`), `tests/voiceAuthCapacity.test.ts`; fixtures in any `tests/voiceAuth*`, `tests/voiceEnforcement*`, `tests/forceDisconnect*` file that fails

**Interfaces:**
- Consumes: none.
- Produces: `export const CAPACITY = 1000`, `export const PER_USER_CONNECTIONS = 4`, `export const NEW_USER_RESERVE = 200`. Error strings unchanged: `Voice identity capacity exhausted`, `Voice identity capacity exhausted for user`.

- [ ] **Step 1: Write the failing tests in `tests/voiceAuthCapacity.test.ts`** (extend the existing `fixture()`; `filler(i)` gives distinct accounts)

```ts
it("refuses a user's 5th connection with the per-user error", ...)  // reproduction: today the 5th..16th succeed
it('admits three rapid reconnects with a delayed release, refuses a fourth, admits it once one release lands', ...)
  // hold connection_release responses on a deferred; 1 live + 3 reconnects = 4 held; 5th throws 'for user'; resolve one release, settledTurn(), 5th succeeds
it('at 800 retained refuses a user with identities and admits a zero-identity user and that user\'s first room', ...)
it('at 800 refuses reserveRoom for a user holding a connection plus a room, or two connections', ...)
it('a user whose only identity is terminal and awaiting release is not reserve-eligible', ...)
it('at 1000 refuses everyone', ...)
it('leaves legacy room reservations unchanged', ...)
it('199 greedy accounts leave room for a fresh user to connect and join; 200 fill the node', ...)
  // greedy account = 4 connections + 4 rooms in the general pool, then fresh accounts take reserve pairs
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/voiceAuthCapacity.test.ts`
Expected: FAIL on the 5th-connection case.

- [ ] **Step 3: Implement the check order from spec section 4.2** in `reserve()` and `reserveRoom()` (room reserve eligibility: `userConnections.get(userId) === 1 && state.rooms.size === 0`). Leave the overdue check (step 3 of `reserveRoom`) to Task 7.

- [ ] **Step 4: Fixture audit**

Run: `npx vitest run tests/voiceAuth tests/voiceEnforcement tests/forceDisconnect`
Expected: PASS. Fix only fixtures that reserve more than 4 connections for one user or reserve rooms on non-fresh users past 800 (for example the existing "keeps exactly 1000 mixed ..." case), keeping each test's intent.

- [ ] **Step 5: Run full media-plane suite**

Run: `npm run typecheck && npm test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add src/lib/voiceAuthConnection.ts tests/
git commit -m "fix: cap voice identities per user and reserve slots for new users"
```

---

### Task 7: Idle release

Shape: construction

**Files:**
- Modify: `src/lib/voiceAuthConnection.ts` (`ConnectionState` at `:27`, `reserve`, `reserveRoom`, `attachRoom` at `:349`, `releaseRoom` at `:386`, new `terminateIdle`), `src/index.ts` (interval beside `voiceEnforcementLeaseInterval` at `:1658`; clear in the graceful-shutdown block before `shutdownTerminalWork = voiceAuthConnections.terminateAll()` at `:2629`), `tests/mediaSocketBoundsWiring.test.ts`
- Create: `tests/voiceAuthIdleRelease.test.ts`

**Interfaces:**
- Consumes: Task 6 constants and check order; `indexSrc` from Task 8.
- Produces: `export const IDLE_RELEASE_MS = 30_000`, `IDLE_JOIN_GRACE_MS = 30_000`, `IDLE_SWEEP_INTERVAL_MS = 5_000`; `ConnectionState.idleSince?: number`; `VoiceAuthConnections.terminateIdle(): void`; `reserveRoom` throws `Voice connection idle` for an overdue connection.

- [ ] **Step 1: Write the failing tests in `tests/voiceAuthIdleRelease.test.ts`**

Copy the `voiceAuthCapacity` fixture with three changes: a mutable `let now = clock` passed as `now: () => now`; responses signed with `String(Math.floor(now / 1000))` (the protocol rejects skew over 30 s, `voiceAuthProtocol.ts:592`); `tokenExpiresAt: clock + 3_600_000`.

```ts
it('at 30 s terminateIdle goes through terminate() and retainedIdentityCount drops only after the signed release', ...)
it('a room in flight suspends release below 60 s and not at 60 s', ...)
it('a failed join does not reset the clock', ...)
it("an overdue connection's reserveRoom throws Voice connection idle", ...)
it('the clock restarts when the last attached room is released', ...)
it('never idles a connection with an attached room', ...)
it('does nothing while draining', ...)
it('a synchronous re-entrant disconnect joins the existing terminal task', ...)  // disconnect callback calls owners.terminate(handle); same promise
```

`tests/mediaSocketBoundsWiring.test.ts`:

```ts
it('schedules terminateIdle every IDLE_SWEEP_INTERVAL_MS and clears it before the shutdown drain', ...)
  // match /setInterval\([\s\S]{0,120}terminateIdle\(\)[\s\S]{0,40}IDLE_SWEEP_INTERVAL_MS/
  // indexOf('clearInterval(<name>)') < indexOf('shutdownTerminalWork = voiceAuthConnections.terminateAll()')   // Review Focus 5
```

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/voiceAuthIdleRelease.test.ts tests/mediaSocketBoundsWiring.test.ts`
Expected: FAIL, `terminateIdle` is not a function.

- [ ] **Step 3: Implement spec section 4.3** (candidate collection before any `terminate()`; overdue rule and the `IDLE_RELEASE_MS + IDLE_JOIN_GRACE_MS` rule; `releaseRoom` restarts the clock only when it first makes an attached room terminal and no other attached, non-terminal room remains). Add the `reserveRoom` overdue check as step 3 of the Task 6 order. Add the `index.ts` interval and its clear.

- [ ] **Step 4: Fixture audit and suite**

Run: `npx vitest run tests/voiceAuth tests/voiceEnforcement tests/forceDisconnect tests/mediaSocketBoundsWiring.test.ts && npm run typecheck`
Expected: PASS. Fix only fixtures that advance the clock 30 s between `reserve()` and `reserveRoom()`.

- [ ] **Step 5: Commit**

```bash
git add src/lib/voiceAuthConnection.ts src/index.ts tests/
git commit -m "fix: release voice identities of sockets idle outside a room"
```

---

### Task 9: User-wide revocation sweep

Shape: defect-repair

**Files:**
- Modify: `src/lib/forceDisconnect.ts:105-121` (`ForceDisconnectIO`), `:765-883` (`handleForceDisconnect`), `tests/forceDisconnect.test.ts`
- Modify (fakes, add `adapter: { rooms: new Map() }`): `tests/forceDisconnect.registrationTransition.test.ts`, `tests/enforcePermissions.test.ts`, `tests/mediaPolicerIntegration.test.ts`, `tests/mediaPolicerTick.test.ts`, `tests/roomManager.test.ts`, `tests/voiceAuthBootShutdown.test.ts`, `tests/voiceAuthReceiver.test.ts`, `tests/voiceCommandCapturedSession.test.ts`, `tests/voiceCommandDispatcher.test.ts`, `tests/voiceCommandPromotionRace.test.ts`, `tests/helpers/voiceEnforcementRecoveryCallers.ts` (let `npm run typecheck` list any other)

**Interfaces:**
- Consumes: none.
- Produces: `ForceDisconnectIO.sockets.adapter: { rooms: Map<string, Set<string>> }` (required). `handleForceDisconnect` signature unchanged.

- [ ] **Step 1: Write the failing tests in `tests/forceDisconnect.test.ts`**

```ts
it('a user-wide command disconnects a stale same-user socket in the room', ...)  // reproduction: today it stays connected
it("leaves other users' sockets untouched", ...)
it('an exact-socket command does not sweep', ...)
it('stats_unavailable does not sweep', ...)
it('sweeps when the user has no participant left', ...)                          // the "already left" branch, before awaitNativeTermination
it('a callId mismatch on an existing room does not sweep', ...)
it('a successor that commits during the teardown awaits is not swept', ...)      // add socket + make it owner inside the awaitNativeTermination mock
it('after revocation no socket of the account receives io.to(room) events', ...) // loopback Server, as in Task 8
```

Sweep sockets are disconnected with `disconnect(true)` and no `force-disconnect` emit.

- [ ] **Step 2: Run to verify failure**

Run: `npx vitest run tests/forceDisconnect.test.ts`
Expected: FAIL on the first case.

- [ ] **Step 3: Implement spec section 4.1 "Sweep"** (gate; capture from `io.sockets.adapter.rooms.get(channelId)` after `resolveExactSessions` and before the first `await`, minus the exact ids; act after the exact teardown and in the already-left branch; skip a socket that is no longer the same object or is now the participant or provisional owner). Then add `adapter` to every fake.

- [ ] **Step 4: Run to verify pass**

Run: `npm run typecheck && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/lib/forceDisconnect.ts tests/
git commit -m "fix: disconnect stale same-user sockets on user-wide revocation"
```

---

### Task 10: Docs, exploit verification and quality gate

Shape: construction

**Files:**
- Modify: `.claude/rules/domains/media-plane.md` (sections "Logical login connection ownership and protocol 4" at `:17`, "Socket.IO" at `:459` — append at the end of its list, "Camera layer-selection gate" at `:729` including the "Gate-ON is immediate" bullet at `:735`, plus a new "Log bounds" paragraph), `CHANGELOG.md` (`## [Prerelease]` → `### Security`, one entry for all six findings), `docs/architecture.md` (beside the Socket.IO signalling paragraph near `:63`)

**Interfaces:**
- Consumes: every constant and behaviour above.
- Produces: spec section 8 content.

- [ ] **Step 1: Update the three docs** with exactly the items in spec section 8. `docs/api/` does not change. No private repository names or paths.

- [ ] **Step 2: Run the exploit tests and record that both fail**

Copy the security tracker's 2026-11-11 Self-Hosted scan `exploit_tests/` directory (including `vitest.mediaplane.config.mjs`) to `<repo root>/.vulnhunt-176/exploit_tests/`; never commit it. From `services/media-plane`:

```bash
npx vitest run --config ../../.vulnhunt-176/exploit_tests/vitest.mediaplane.config.mjs \
  ../../.vulnhunt-176/exploit_tests/test_vuln_159_displaced_dm_socket.test.ts \
  ../../.vulnhunt-176/exploit_tests/test_vuln_146_voice_ledger_exhaustion.test.ts
```

Expected: 4 failing tests — 159 source pin (`commitDMAdmission` not in `index.ts`), 159 behaviour (`expect(s1.closed).toBe(false)`), 146 behaviour (`attacker 0 socket 4`), 146 static (`const PER_USER_CONNECTIONS = 16;`). Then `rm -rf ../../.vulnhunt-176`.

- [ ] **Step 3: Run the quality gate**

Run: `npm run typecheck && npm run lint && npm run test:coverage`, then `/verify-quality-gate`.
Expected: all pass; new-code coverage ≥ 80%; 0 new blocker/critical issues; 0 new high-confidence hotspots.

- [ ] **Step 4: Commit**

```bash
git add .claude/rules/domains/media-plane.md CHANGELOG.md docs/architecture.md
git commit -m "docs: record media socket bounds, ledger limits and log bounds"
```

---
