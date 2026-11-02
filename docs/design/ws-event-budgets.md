# WebSocket event budgets

Normative contract for `Hosted issue 2270`.
Unit A supplies a pure registry, typed config validation and integer scaling.
Unit B1 adds the Redis script/adapter. Unit B2 adds dedicated client factories
with the source ACL grant. No runtime caller invokes the new backend.
Unit E adds canonical protocol validation for budget advisories. These units
remain **construction groundwork** and do not activate runtime admission.
All proposed thresholds and workload claims are **UNMEASURED**; this document
specifies enforcement and its release prerequisites, not evidence of deployment.

## Policy and identity

One event costs one token. Account budgets use server-authenticated `Client.UserID`
across sockets, session rotation, logical login families and control-plane instances.
Client-supplied identity, socket/session/family IDs and tenants cannot partition the
account budget. Admission never grants authorization or replaces existing guards.

| Incoming application names | Class | Scope | May hold |
|---|---|---|---|
| `message`, `dm_message` | content | existing content | no |
| `typing`, `dm_typing` | typing | account | no |
| `subscribe`, `unsubscribe`, `subscribe_server`, `unsubscribe_server`, `subscribe_dm`, `unsubscribe_dm` | subscription | account | yes |
| `heartbeat` | presence | local maintenance | no |
| `set_status` | presence | account | no |
| `profile_update` | profile | account | no |
| `server_update` | server | account | no |
| `connection_ready_probe` | ready | account | yes |
| `auth_renew` | renewal | account | no |

These are all 16 inputs: 15 hub inputs plus intercepted renewal. Transport control
frames and server-originated events are excluded. Unknown/empty names return zero
policy and false. Enum zero is invalid; policies are returned by value, with no
exposed mutable registry. Class strings are closed; invalid values map to `unknown`.

Preserve pre-JSON ingress at 20/s, burst 10, the 512 KiB frame cap and the bounded
256-entry incoming queue. Preserve the original per-socket shared channel/DM
content budget at 5/s, burst 10, and the renewal 10-second local attempt guard.
The new heartbeat budget is local per socket: one token every 5 seconds, burst 2,
with no distributed call or fallback allocation. Existing guards are not scaled.

| Account class | Base tokens/s | Base interval (microseconds) | Base burst |
|---|---:|---:|---:|
| subscription | 80 | 12500 | 160 |
| typing | 10 | 100000 | 40 |
| profile | 0.2 | 5000000 | 8 |
| server | 0.5 | 2000000 | 16 |
| presence (`set_status`) | 2 | 500000 | 16 |
| ready | 1 | 1000000 | 16 |
| renewal | 0.5 | 2000000 | 16 |

Scale is an integer from 25 through 400. Effective interval is integer
`ceil(baseIntervalMicros * 100 / scale)` microseconds; burst is
`floor(baseBurst * scale / 100)`. Rounding cannot add allowance. Subscription at
scale 33 is 37879 microseconds / burst 52. The smallest allowed account burst is 2.
`accountEventBudget` validates config first, rejects content/unknown classes and
returns configured parameters regardless of `Enabled`. Scope excludes heartbeat.

Future startup settings are `WS_EVENT_BUDGET_ENABLED` (absent default `true`) and
`WS_EVENT_BUDGET_SCALE_PERCENT` (absent default `100`). Explicit empty values fail.
Accept only lowercase `true`/`false` and canonical unsigned ASCII decimal 25..400,
without signs, spaces, leading zeros or fractions. Validate both even if disabled.
No environment parsing, defaults, live reload or per-class override exists in the
groundwork. Zero typed config is invalid, not implicit disable.

## Distributed decisions and bounded fallback

The B1 backend implements atomic Redis GCRA and Lua `TIME`, with key
`ratelimit:user:ws:v1:{<canonical-user-uuid>}:<class>` and exact hash fields `v=1`,
`last_us`, `tat_us`. Values use canonical unsigned decimal integers at most
9007199254740991, never floating/scientific serialization. Invalid type, version,
missing/extra fields or debt `tat_us-last_us > 120000000` refuses without reset.
Absent state starts at current time. Set `now_us=max(wall_us,last_us)` so backward
time does not refill. Admit when `now_us >= tat_us-(burst-1)*interval_us`; on allow
store `tat_us=max(now_us,tat_us)+interval_us`, otherwise retain it and return the
positive ceiling retry in milliseconds. Both valid outcomes store `last_us=now_us`.

Retention is `ttl_ms=600000+ceil(now_us/1000)-floor(wall_us/1000)`, so expiry is no
earlier than logical `now_us+600s`. Before mutation, check every integer/arithmetic
bound and require `ceil(wall_us/1000)+ttl_ms <= floor(9007199254740991/1000)`.
A one-hour rollback can retain state for about 70 minutes; long retention can
increase memory pressure. No eviction of unexpired debt is allowed. The exact
three-integer reply is `{1, allowed, retry_after_ms}`: allow means retry 0; denial
means retry 1..120000. Malformed replies/errors are unavailable, never permission.
Process/data loss can reset Redis state; this contract makes no durability claim.

### B1 request, result and ownership

The public construction seam in package `websocket` is:

```go
type RedisBudgetDecision struct {
    Allowed bool
    RetryAfter time.Duration
}
type AccountBudgetBackend interface {
    Decide(context.Context, uuid.UUID, EventClass, int64, int64) (RedisBudgetDecision, error)
    Close() error
}
func NewRedisAccountBudgetBackend(EventBudgetConfig, *redis.Client) (AccountBudgetBackend, error)
```

The last two `Decide` arguments are interval microseconds and burst. Construction
validates scale even when disabled, rejects disabled config and nil clients, and
performs no command, script load or dial. Config is copied. Only successful
construction transfers ownership of the dedicated client; on failure the caller
retains it and checks cleanup errors. `Close` closes that client exactly once,
retains its checked result, and refuses future decisions. Cancellation/joining of
active application decisions belongs to the later admitter, not this backend.

`Decide` rejects nil UUIDs, content/unknown classes and a supplied pair that differs
from the approved configured pair before issuing a command. The authenticated
UUID's canonical string and the closed class name are the sole key components.
For profile at scale 100, the logical bulk-string command is:

```text
EVALSHA <sha> 1 ratelimit:user:ws:v1:{11111111-1111-4111-8111-111111111111}:profile 1 5000000 8
```

The UUID above is a test identity. Arguments are version, interval microseconds,
burst; no caller clock or payload is sent. `redis.NewScript` identifies the
embedded `event_admission.lua` bytes by SHA-1
`aca3e0db97c777545e5ee970f170f6d1c6406fc2` (a cache identifier, not an integrity
primitive). `Script.Run` falls back once from NOSCRIPT to EVAL with the identical
source/key/arguments and context. All other errors end the attempt. The dedicated
client must disable library command retries; B2 supplies its production factory.

Lua requires one key and three arguments, version bytes `1`, canonical unsigned
ASCII decimal interval 3125..20000000 and burst 2..640, and product at most
120000000. It validates TIME's two decimal strings, microseconds 0..999999 and
the combined safe-integer bound. TYPE must be absent or a hash with exactly the
three specified fields. Stored times are exactly `0` or nonzero-leading ASCII
digits; signs, leading zeros, spaces, decimal/exponent forms and values above
9007199254740991 are rejected lexically before numeric conversion. Negative idle
debt is valid. Stored writes use `string.format('%.0f', value)`.

Every validation and arithmetic check precedes HSET. Rejection returns exactly
`ERR WS_EVENT_BUDGET_INVALID_STATE` without changing fields or absolute expiry.
A later runtime failure, including PEXPIRE after HSET, is uncertain execution;
Redis does not roll back the preceding write. No failure permits replay or reset.

The Go adapter accepts only an array of exactly three RESP integers (`int64`),
version 1 and either `{1,1,0}` or `{1,0,retry}` with retry 1..120000 milliseconds.
Numeric strings, floats, booleans, nulls, extra fields and contradictory delays
are unavailable. The exact script rejection maps to
`ErrAccountBudgetInvalidState`; other command/protocol failures map to
`ErrAccountBudgetUnavailable`, with a zero decision. Neither exposes raw Redis
errors or key values, and neither is a definitive rate denial. Canceled/expired
contexts and closed backends cannot return a distributed allowance.

For exact arithmetic examples, let T=1800000000000000 microseconds. At default
profile settings, eight requests at T leave tat=T+40000000; the ninth denies for
5000ms. At T+4999999 it denies for 1ms and retains TTL600001; at T+5000000 it
allows and stores tat=T+45000000 with TTL600000. Wall T−3600000000 with logical
last T retains TTL4200000; nonaligned last T+1 and wall T−3600000000+1 retain
TTL4200001. Repeated rollback by two hours retains TTL7800000. Changing scale
retains debt: that burst state denies for 20000ms at scale25 and 1250ms at scale400.

All additions are checked before evaluation, and ceiling conversion uses quotient
and remainder. Positive stored debt120000000 is valid (default profile retry85000),
while debt120000001 is rejected unchanged. An absent record at wall
9007198654740000 allows, stores tat9007198659740000 and TTL600000 exactly at the
retained horizon; the next microsecond rejects without creating a key. Canonical
9007199254740991 parses exactly but cannot pass that horizon at ordinary wall
time; 9007199254740992 fails the lexical check. Isolated tests substitute only
the TIME acquisition and expose the PEXPIRE argument; production has no clock
override. Absolute-expiry snapshots establish no-write rejection, not decreasing
PTTL comparisons. Real TIME execution and uncertainty tests remain required.

### B2 dedicated clients and source ACL

Package `database` supplies these constructors in `redis_event_budget.go`:

```go
func NewEventBudgetRedisClient(redisURL string) (*redis.Client, error)
func NewManagedEventBudgetRedisClient(endpoint string, tlsConfig *tls.Config,
    runtime *managedredis.Runtime) (*redis.Client, error)
```

Both construct fresh options and independent client pools, without a PING, script
load or credential probe. The legacy constructor uses `redis.ParseURL` and keeps
URL authentication, database selection, TLS and protocol. Invalid URL errors are
fixed strings, without echoing credentials. The managed constructor rejects a nil
runtime, reuses existing endpoint validation and TLS cloning in `managedOptions`,
then attaches that same runtime as `StreamingCredentialsProvider` and installs
`NewFreshnessHook(runtime)`. It does not create a runtime, fetch a credential or
take an immutable credential snapshot. A cold or expired runtime does not prevent
construction; the existing hook/provider rejects actual use until credentials
are valid. Existing startup and candidate-authentication checks remain authoritative.

The dedicated settings are fixed: `MaxRetries=-1` at construction (normalized to
zero by go-redis), `DialerRetries=1`, `ContextTimeoutEnabled=true`, and
`PoolSize=MaxActiveConns=MaxConcurrentDials=32`. `MinIdleConns=0`; dial, read, write
and pool timeouts are each 100ms. Optional maintenance notifications are explicitly
disabled with `maintnotifications.ModeDisabled`. The constructor preserves the
configured/default RESP protocol and leaves all shared-client settings unchanged.
Only the account backend receives this client; application pipelines, autopipeline,
PubSub, cache and full-duplex wrappers are outside this contract.

The caller owns the returned client until successful backend construction; failed
backend construction leaves checked cleanup with that caller. Closing the dedicated
client does not close the credential runtime or shared client. The streaming
provider retains per-connection unsubscribe behavior. Subscription cleanup,
shared-client survival and library recovery/dial quiescence need separate evidence;
the constructors do not promise a public join of all library goroutines.

The checked `internal/managedredis/acl-manifest.json` adds only `TIME` to the
control-plane service command list and removes it from the absent-command list.
Media-plane and operator selectors receive no new grant. Existing account key
patterns and TYPE/HLEN/HGET/HSET/PEXPIRE/EVAL/EVALSHA permissions already cover
the script; no production SCRIPT LOAD, PEXPIRETIME or administrative grant is
added for test fixtures. This is a source ACL change, not live provisioning;
managed delivery must install it before enabled runtime admission.

### Later bounded fallback

The following B3 fallback/lifecycle behavior remains future work.
Acquire one of 32 application slots nonblocking. Each decision has
an absolute 100ms bound, with at most 90ms for remote work, shortened by authority
or hold deadlines. Saturation uses immediate fallback. Only a definitive NOSCRIPT
may fall back to EVAL; uncertain execution is never retried or refunded.

Fallback has at most 10000 user records, each with seven fixed class states.
Allocate only after a valid Redis result, initially empty; no LRU or eviction.
Healthy Redis allowance remains authoritative even with a full/empty local map.
Fallback interval is four times the effective interval; burst is
`max(1,floor(effectiveBurst/4))`. Reserve local credit before a remote attempt and
never refund. Definitive Redis denial refuses; unavailable execution may allow only
that attempt's already reserved credit. Cold/untracked users are denied in outage.
Under one lock, look up, touch and reserve; never hold it across Redis or shutdown.
Sweep every 60s, removing only records idle at least 600s using monotonic time.
Shutdown rejects new work, cancels/joins application work and sweep, then closes
the dedicated client before credential teardown. Library dial quiescence needs
separate evidence; the application slot bound does not prove library shutdown.

## FIFO, authority and wire contract

Admission belongs after parsing/known-type validation and before enqueue or renewal
authority work. Subscription/ready may hold only after definitive Redis rate denial,
for at most 2s per event and 21 attempts. Next start is
`max(previousStart+100ms, previousCompletion+retryAfter)` and must precede the
original deadline. Other classes never hold. Unavailable denial or exhaustion
closes with 1013 before this frame or any later ready probe is dispatched.
Preserve FIFO and the existing best-effort 5s ready wait; large replays have no
universal 5s completion guarantee. No payload replay queue is introduced.

Authority expiry, parent cancellation and security-terminal work abandon the event,
with no overload close or admission counters. Recheck before productive work;
late success never revives authority. Limiter/hold deadlines while authority is
live are final refusals, not abandonment. Family security closure must never wait
on limiter locks, metrics, Redis cleanup or courtesy frames between siblings.

| Refused event | Future behavior |
|---|---|
| subscription / ready | bounded hold only for definitive rate denial, then 1013 |
| typing / heartbeat | silent drop; no fanout, ack or presence effect |
| profile / server / set_status | bounded advisory, at most one/socket/second across these classes |
| renewal | existing `auth_renew_result` with `ok:false`; original guard/deadline retained |
| content | original content refusal behavior |

Advisory shape: `{"type":"error","data":{"code":"ws_rate_limited","event_class":"profile","retry_after_ms":5000}}`.
Only the eight closed class names above and finite integer retries 100..30000 are
accepted, without coercion. Require both new fields for this code and reject
partial pairs for every code. Complete valid pairs remain accepted with another
or absent code. Existing errors with neither field retain their parsed fields and
acceptance, including empty data and `epoch_revoked`; existing unknown-key
stripping is unchanged. Do not echo payloads,
targets or messages. Full outbound queues drop advisories without blocking.
For positive known delay, ceil milliseconds then clamp to 100..30000; unavailable
or nonpositive/nonfinite delay uses 1000. Internal holds use the original delay.

1013 reason is exactly `retry_after_ms=<decimal>`. Clients accept only whole ASCII
`^retry_after_ms=([1-9][0-9]{2,4})$` and range 100..30000; all else yields 1000.
Use bounded control writes and transport teardown. Client reconnect delay is
`max(existingJitterDelay, parsedRetryDelay)`, enforced on every automatic dial path
by a generation-bound timer, including online/recovery paths. Explicit disconnect
or account change cancels obsolete timers. Preserve terminal security behavior.
Restore desired subscriptions only; never automatically replay content/mutations.
Renewal retains its existing 5s acknowledgment wait and single 11s retry.

## Observability, compatibility and activation

The canonical schema is
`packages/protocol/src/renderer/types/ws-events.ts`: nested error data validation
retains `event_class` and `retry_after_ms` through both `ErrorSchema` and the
existing `WebSocketEventSchema` union. The `error` event remains one union member,
and `ErrorPayload` is still derived from that schema.

The private `@concord-voice/protocol` package has no publication/import delivery
mechanism established here. Desktop currently imports its own
`desktop/src/renderer/types/ws-events.ts`; unit F must port the reviewed canonical
schema and tests with the exact source commit, then prove its actual parser and
service behavior. Canonical schema validation alone proves neither Desktop
adoption nor a compatible released client. No package version, dependency or
activation change follows from E.

Emit exactly 24 scalar counters: eight classes times `allowed`, `throttled`,
`degraded`, named `websocket_event_<class>_<outcome>_total`. Count final admission
once, not handler success or intermediate hold denials. OR degradation across
attempts; pre-finalization abandonment counts nothing. Original content counts at
its existing gate; heartbeat at its local gate; renewal claims its local guard
exactly once before account admission. Add inflight/warm-user gauges and actual
warm-capacity-miss counter: 27 keys total, all `SourceControl`/`UnitCount`, counters
`KindCounter/RollupLast`, gauges `KindGauge/RollupAverage`, existing count bounds.
Use a closed catalog. Emit no payloads, identifiers or raw close/Redis errors.
Reuse rate-limit-denied/backend-unavailable security reasons, coalesced per process
per reason per 60s, without new identity/class fields.

Ship the compatible Admin reader before or with the 27-key producer, including
disabled zero emission. Ship canonical protocol schema and compatible Desktop/
other supported consumers before relying on the 1013 floor. Verify the actual
managed deployment consumes the canonical Self-Hosted implementation; any temporary
source port must carry the matching Admin changes and explicit provenance.
Install the control-plane `TIME` ACL and required env keys before enabling the
binary. Existing installations populate keys through their current config owner
before required-value Compose; fresh installation remains a separate fresh-only
writer. No live provisioning is authorized by this design.

Activation requires independent correctness/security review, supported-client and
delivery evidence, and controlled ordinary/replay/flap/renewal/outage/clock/scale
measurements. Default ordinary 1/2/8-device workloads must have no new denial.
Disable bypasses only new policies/responses/metrics and fallback allocation;
original ingress/content/renewal/authority guards remain. Retain compatible readers
and Redis debt on rollback; never flush/delete budget keys. See the
[implementation and evidence plan](ws-event-budgets-plan.md).
