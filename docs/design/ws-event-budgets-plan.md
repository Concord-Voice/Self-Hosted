# WebSocket event budget implementation plan

Plan for `Hosted issue 2270`, governed
by the [normative contract](ws-event-budgets.md). All thresholds remain
**UNMEASURED**. Units A and B1 are construction groundwork only; no runtime caller
uses the new Redis backend. Socket behavior, wire schema, metrics and environment
parsing are unchanged.

## Bounded work units

| Unit | Canonical owner and scope | Required evidence |
|---|---|---|
| A: pure policy | Self-Hosted: `event_admission.go`, focused independent tests, WebSocket README and these design documents | exhaustive registry, strict typed validation, integer vectors and source census |
| B: account engine | Self-Hosted: Redis Lua/adapter, bounded local fallback, dedicated database client and control-plane ACL | frozen Redis byte/clock vectors, uncertainty, map/slot bounds, lifecycle and managed credentials |
| C: integration | Self-Hosted: read/dispatch, renewal, handler/router/bootstrap, typed env parsing | before-effect refusal, FIFO, cancellation/authority races, disable and startup validation |
| D: metrics pair | Self-Hosted: producer, closed backend catalog and bundled Admin reader/labels | all 27 served points through actual reader definitions/parser; rollback compatibility |
| E: schema | Self-Hosted: canonical `packages/protocol` error contract | valid/invalid payload vectors and compatibility |
| F: client | Desktop: actual WebSocket service and currently required consumer schema copy | strict 1013 grammar, generation timers, all automatic dial paths and no mutation replay |
| G: self-host config | Self-Hosted: actual production Compose, fresh installer, env example and operator docs | required-value wiring, installer/parity fixtures and existing-install preparation |
| H: managed delivery | Hosted: canonical config writer, Compose, ACL/operator docs and verified delivery lane | every admitted writer branch, reader/client prerequisites and canonical source provenance |

No SQL migration, media-plane change or live commissioning belongs to these units.
Self-Hosted owns control-plane policy and the protocol origin; Desktop owns its
consumer; Hosted owns managed deployment. The protocol package is unpublished:
identify the exact schema source commit and required consumer copy instead of
assuming a package version propagates it. Census other supported clients before
activation. Hosted source builds must prove a delivery lane from canonical changes.

## Unit A frozen interface and verification

All symbols are in package `websocket`:

```go
type EventClass uint8
type BudgetScope uint8
type EventPolicy struct { Class EventClass; Scope BudgetScope; MayHold bool }
type EventBudgetConfig struct { Enabled bool; ScalePercent int }
type eventBudgetParameters struct { Interval time.Duration; Burst int }
func ValidateEventBudgetConfig(EventBudgetConfig) error
func classifyInboundEvent(string) (EventPolicy, bool)
func accountEventBudget(EventBudgetConfig, EventClass) (eventBudgetParameters, error)
```

Class constants are `EventClassContent`, `EventClassTyping`,
`EventClassSubscription`, `EventClassPresence`, `EventClassProfile`,
`EventClassServer`, `EventClassReady`, `EventClassRenewal`. Scope constants are
`BudgetScopeExistingContent`, `BudgetScopeLocalMaintenance`, `BudgetScopeAccount`.
Zero is invalid for both. Return policy copies; expose no mutable map. No
placeholder admitter or runtime call site belongs in A. Future admission must keep
explicit `abandon` distinct from refusal/overload and `DefinitiveRateDeny` distinct
from uncertain execution; A does not implement that interface.

Independent tests must cover all 16 policies, empty/unknown inputs, mutation of a
returned value, closed enum strings, content/invalid account-class refusal and
config validation before class lookup. Disabled config still validates scale;
valid disabled config still returns parameters. Reject at least 0, 24 and 401.

| Class | Scale 25 interval μs / burst | Scale 100 | Scale 400 |
|---|---|---|---|
| subscription | 50000 / 40 | 12500 / 160 | 3125 / 640 |
| typing | 400000 / 10 | 100000 / 40 | 25000 / 160 |
| profile | 20000000 / 2 | 5000000 / 8 | 1250000 / 32 |
| server | 8000000 / 4 | 2000000 / 16 | 500000 / 64 |
| presence | 2000000 / 4 | 500000 / 16 | 125000 / 64 |
| ready | 4000000 / 4 | 1000000 / 16 | 250000 / 64 |
| renewal | 8000000 / 4 | 2000000 / 16 | 500000 / 64 |

Assert subscription scale 33 gives 37879 μs / burst 52, and rounding never
increases allowance. Parse the actual `Hub.handleIncoming` switch, expanding
multi-name cases, and independently resolve renewal interception in `dispatchFrame`
through its constant in `token_renewal.go`. Compare that union to the registry.
Adding a real dispatcher case must fail without changing a second manual name
list. Keep a separately asserted policy table to test meaning.

Run only focused pure Go tests for A; no service bootstrap/database is required.
Apply independent correctness/security review before integration. Report commands,
results and unavailable checks honestly; pure policy evidence is not runtime proof.

## Later evidence and acceptance gates

### Unit B1 construction and verification

B1 adds `internal/websocket/event_admission.lua` and
`event_admission_redis.go`, with independent unit and isolated Redis integration
tests. Its exact API, byte grammar, ownership and boundary examples are recorded
in the [B1 contract](ws-event-budgets.md#b1-request-result-and-ownership).
The backend has no runtime call site, warm map, application semaphore or new
dependency. B2 owns dedicated client factories and ACL; B3 owns admission,
deadlines and fallback; B4 combines failure/lifetime evidence. No permissive
placeholder for these later units is introduced.

Required B1 evidence covers all approved configured pairs, command bytes and
one-key count, strict three-integer reply types, copied config, zero-command
invalid inputs, ownership on constructor error, and concurrent/idempotent Close.
Run production Lua on isolated Redis for exact stored decimal bytes, burst/refill,
scale changes, idle/maximum debt, canonical maximum/max+1, corrupt type/field/
grammar rejection, rollback retention and the conservative expiry horizon.
Two clients contending for one account must share its burst; another account or
class remains independent. Corrupt records retain their bytes and absolute expiry.

Only test-local substitutions of the unique TIME acquisition and result plumbing
may supply deterministic clock/TTL vectors; the production algorithm stays the
same. Execute the unchanged script separately with real TIME. Cold script cache
permits exactly EVALSHA then EVAL under one context; warm cache needs only EVALSHA.
A real debit followed by a lost reply must return unavailable with no replay.
Use only disposable Redis and administrative test credentials for fixture
inspection; never broaden production ACL to accommodate tests.

Root captures the absent-feature compile barrier before source implementation,
then runs focused unit/integration/race/vet/lint checks and independent review.
The initial missing-symbol barrier is not behavioral red evidence. Record actual
Redis version, commands, results and missing checks; source construction and test
fixtures alone do not establish passing tests or production capacity.

### Remaining account-engine and runtime gates

Pin corrupt-state refusal, safe-integer overflow, exact reply grammar and
backward-clock retention. Profile debt at logical T with wall T−3600s must retain
4200000ms for millisecond-aligned T, or 4200001ms with a microsecond remainder;
after 600 wall seconds it must still deny, without a new burst. Use a deterministic
script clock seam; never change host time. Cover upward/repeated rollback and
scale changes without deleting debt.

For runtime work, measure the actual read/dispatch and client reconnect boundaries:

| Workload | Shape and required observation |
|---|---|
| Ordinary | 1/2/8 sockets, 16 servers + channel + DM + ready/socket; retain duplicate cold-hook emissions; at scale 100, no new denial/1013 and actual FIFO subscription state before ack |
| Large replay | 100/500 servers, 1/2/8 sockets; bounded queue/hold, eventual processing or explicit 1013; report possible 5s ready timeout |
| Flap | reconnect at 0/3/6/9s then quiet 30s, aligned/staggered devices; surviving account debt and bounded recovery, no content replay |
| Typing/metadata | eight devices: typing start/stop every 3s for 60s; profile once/device then every 60s; two status edges/device then every 10s; two server edges/device then every 60s |
| Renewal | eight aligned rotations and existing 11s retries, depleted account bucket, revocation during decision/validation or an earlier hold; preserve authority and no renewal hold |
| Outage/capacity | 10001 users, full map, cold/idle users, failure before request and lost reply after debit; healthy full-map allow, conservative warm fallback, no refund/retry |
| Slots/deadlines | 32 blocked operations plus one, heartbeat on another socket, close/revocation; immediate fallback, bounded application settlement, independently observed library recovery |
| Config/credentials | scales 25/100/400, disabled/re-enabled with retained debt; legacy and managed rotation, before/after TIME ACL; unchanged shared-client behavior |

Record class decisions, downstream DB/Redis/fanout calls, CPU, queue depth,
application inflight, peak warm users, event/ready p50/p95/p99 and allocations.
Report scheduler tolerance and pool-dial quiescence separately. Hardware-dependent
latency/CPU has no invented fixed SLO. Default ordinary zero-denial, exact integer
arithmetic, identity isolation, memory/concurrency bounds and authority/FIFO are
acceptance requirements. Flap/large-replay cases observe bounded recovery rather
than promise zero denial. Re-review thresholds if ordinary workloads fail.

Mutation/negative tests must show refusal prevents expensive effects, stale results
cannot revive authority, and intermediate/abandoned attempts do not count final
admission. Renewal's local guard claims once; account allowance counts admission
even when token validation later fails. Assert actual outbound schema, reader
parsing and reconnection behavior rather than only helper inputs or key sets.

## Delivery and activation order

1. Land A after independent policy evidence and review.
2. Deliver E and F with exact schema provenance. Other supported consumers need
   compatibility evidence before activation.
3. Implement B/C/D/G in bounded reviewed units. Pair the Admin reader with or
   ahead of its producer, including when disabled. Preserve current authority owners.
4. Deliver H and prove the actual managed build consumes canonical code and Admin.
   Populate required keys through existing writers before required-value Compose;
   provision the reviewed TIME ACL before the enabled managed binary.
5. Activate only after measured availability, security/compatibility review and
   current-head tests, lint, CI and quality gates. Require no new blocker/critical
   findings or high-confidence security hotspots and at least 80% new-code coverage.

Missing compatible readers or delivery evidence blocks activation, not A. A
temporary operator-selected disable does not change the default. Disable preserves
original guards and Redis debt; retain the compatible reader during producer
rollback. Stop the new producer before restoring an old reader. No Redis flush,
credential replacement, deployment or production configuration mutation is part
of this plan's source implementation authorization.
