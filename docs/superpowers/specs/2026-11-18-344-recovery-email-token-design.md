# Email recovery key: design

**Issue:** #344
**Status:** approved 2026-11-18
**Date:** 2026-11-18

## Intent

Email account recovery proves control of the mailbox with a 6-digit code. A
6-digit code can be guessed, so the server counts wrong submissions for each
address and refuses more after a limit. That limit follows the address, not the
mailbox holder, so the same limit that protects the account can stop its owner
from completing email recovery until it resets.

This design replaces the code with a secret that cannot be guessed. With no
guessing to bound, the verification step needs no per-address limit, and wrong
submissions cannot affect the owner.

Naming: the emailed secret is the **recovery key** in user text and
`recoveryEmailToken` in code. It is not the recovery JWT that the server
returns after verification (`recovery_token` in the API). That JWT is out of
scope and does not change.

## Goals

1. Guessing the emailed secret is infeasible by construction.
2. No third party can stop the mailbox owner from completing email recovery on
   the token path.
3. Responses and timing do not depend on whether an account exists.
   `RecoveryBegin` keeps its queued, timing-uniform delivery.
4. A bounded number of recovery emails for each account, on both paths
   together.
5. The newest recovery email always holds a usable secret.
6. Clients that submit a 6-digit code keep working during a compatibility
   window.

## Non-goals

- The recovery JWT, `CompleteRecovery`, the recovery policy check, MFA-backed
  recovery, device recovery, and social recovery.
- The desktop client change. It is a separate change in the Desktop repository.
- The registration verification code.
- The per-client-address auth ban (`RecordAuthFailure`, `AuthBanCheck`). It is
  keyed by client address, not by target account, and is unchanged.

## Anti-goals

- A link in the email. This server has no web page to receive one, and mail
  scanners that open links would consume a single-use token.
- A second limit that can block the owner under a new name.

## Constraints

- Public repository: no private tracker references.
- Release compatibility policy (`.claude/rules/domains/backend.md`, "Client
  compatibility"): fields used by current Stable, the previous minor line and
  the active Pilot line must keep working. New server behaviour for new clients
  needs an explicit capability sent by the client.
- The desktop client today sends `{email}` to begin and `{email, code}` to
  verify, limits the code field to 6 characters, and sends no version header on
  these calls. The mobile app does not call the recovery endpoints.
- Managed Redis is a single instance (no cluster client), its ACL allows
  `EVAL`, `GETDEL`, `INCR` and the key pattern `recovery_code:*`, and the
  self-host restore purges the `recovery_code:` prefix. Multi-key Lua scripts
  are already used in this package.

## Approaches

| | Approach | Verdict |
|---|---|---|
| A | Opaque random token; Redis record keyed by the token's hash; consumed with one atomic `GETDEL` | **Chosen** |
| B | Signed token (JWT style) | Rejected. Single use still needs server state, and it adds a signing-key dependency. |
| C | Longer code typed by hand | Rejected. Enough entropy to make guessing infeasible is too long to type. |

Two smaller changes to the existing limit were examined earlier and rejected:
keying it by address plus client address removes the only bound on total
guesses, and charging it only while a code is pending changes what responses
reveal about an address and still allows the block.

## Design

### 1. Token and keys

- Token: 32 bytes from `crypto/rand`, encoded with `base64.RawURLEncoding`:
  43 characters, 256 bits. The issue requires at least 128.
- Keys, all under the existing `recovery_code:` prefix so the ACL manifest and
  the restore purge list need no change:

| Key | Value | TTL |
|---|---|---|
| `recovery_code:token:<hex SHA-256 of token>` | `{user_id, send_id}` | tier (section 4) |
| `recovery_code:slot:<user_id>` | `send_id` | tier |
| `recovery_code:sends:<user_id>` | integer | 24 h from first send |
| `recovery_code:<email>` (legacy, unchanged shape) | `{code_hash, user_id, send_id, attempts}` | tier |

- Slot and counter are keyed by account, not by address, so differently
  spelled addresses that resolve to one account share one limit, and an email
  change mid-window does not orphan the slot.
- The token is never stored or logged. Only its hash is a key.
- **Request-supplied email is checked before any Redis read** on every path
  that uses it: it must contain `@` and must not contain `:`. Otherwise `400`
  (`Valid email is required`). This depends only on the request. It closes a
  reviewer-found oracle in which a crafted "email" such as `slot:...` made the
  legacy verify read a different kind of key and answer `500` instead of `401`.
  Registration already refuses an unquoted `:` in an address, so no real
  account is refused.

### 2. Begin: `POST /auth/recovery/begin`

Request gains one optional field:

```json
{ "email": "user@example.com", "format": "token" }
```

- `format` absent or `"code"`: legacy code (section 5).
- `format: "token"`: recovery key.
- Any other value: `400`, decided before any account work.

Response gains one field naming what the server will email:

```json
{ "message": "If an account exists with that email, a recovery email has been sent", "format": "token" }
```

`format` in the response depends only on the request and on the server switch
(section 5), never on the account. A client that sent `format: "token"` and
gets no `format` back is talking to an older server and must show the 6-digit
entry. That is how a new client works with an old self-hosted server.

The handler still only validates, queues `{addr, format}`, and returns. All
account, Redis and mail work stays on the worker.

Worker, both formats:

1. Look up the user. No user: stop.
2. Generate the secret (token, or 6-digit code for legacy) and a `send_id`.
3. Run the **issue script** (one Lua call, keys: slot, counter, record):
   - If the slot exists: return "held". Send nothing.
   - `INCR` the counter; `EXPIRE NX` 24 h.
   - Tier = 15 min if the count is 1 to 3, else 60 min.
   - `SET` the slot (`send_id`) and the record, both with `PX` = tier.
   - Return the tier.
   One script means no partial state: either all three keys change or none.
   The slot and the record always share one TTL, so the record never expires
   before the slot.
4. Deliver on the existing detached delivery goroutine, tracked for shutdown.
   The email names the tier in minutes.
5. On delivery failure, or when shutdown abandons the delivery, run the
   **cleanup script**: delete the record and the slot, and `DECR` the counter,
   each only if the slot still holds this `send_id`. A failing mail server
   therefore does not push the owner into the slow tier.

`recoveryDelivery` is extended with the record key, slot key, counter key and
`send_id`, so `clearInflightRecoveryCodes` can run the cleanup script.

Legacy and token sends share the slot and counter, so alternating `format`
values gets no extra emails.

### 3. Verify: `POST /auth/recovery/verify-code`

Request carries exactly one of `code` or `token`:

```json
{ "token": "<43 characters>" }
```

- Both or neither: `400`, in every switch state.
- `token` path: `email` is optional and ignored. The token alone finds the
  record, so a mistyped address cannot burn the token.
- Shape check first (43 base64url characters): otherwise `400`. Request only.
- `GETDEL` the record. Miss (wrong, expired, used, never issued): `401`,
  existing body `Invalid or expired recovery code`, existing security event,
  existing per-client-address `RecordAuthFailure`. No `attempts_remaining`.
- Hit: compare-and-delete the slot if it holds the record's `send_id` (so the
  owner can ask again at once if a later step fails). Then run
  `recoveryPolicyAllowsEmailCompletion` and `CompleteRecovery(c, userID,
  "email")` exactly as the code path does today. An account with reserved
  recovery factors still gets the MFA recovery challenge.
- If the slot release fails, the slot expires on its own TTL. The token is
  already consumed by then, so only a retry after a failed later step waits.
  Accepted: it needs a Redis fault between two commands, and it blocks only
  the person who held the token.
- A failed token submission reads or writes nothing but the `GETDEL` miss and
  the client-address failure record. It never touches `recovery_fail:`.
- On a completed recovery the address budget is cleared as today
  (`RecoveryVerifyCode` for email-only, `CompleteRecovery` for MFA-backed).
  That keeps the legacy path's behaviour for the owner and needs no change to
  `CompleteRecovery`.
- Redis error: `500`, existing body.

The route's own limiter (5 per 15 minutes per client address) stays. It is
keyed by client address, so a third party elsewhere cannot spend it against the
owner.

### 4. Email limit for each account

- Tiers: sends 1 to 3 in a 24-hour window: 15 minutes; later sends: 60 minutes.
- The tier sets the slot TTL, which is the resend cooldown, and the record TTL.
  They are equal by construction (one script), so the newest email's secret is
  valid for exactly as long as no new email can be sent. Goal 5.
- Bound: 3 + 21 = 24 emails in 24 hours for one account across both formats
  (45 min for three fast sends, then 23 h 15 min at 60 min).
- No hard daily cap. A cap would let a third party use it up and leave the
  owner with no way to get a key, which is the same defect in a new place.
- The legacy record TTL becomes the tier (was 10 min). Its guessing bound is
  per record (5 wrong codes delete it), so a longer life does not add guesses.

### 5. Compatibility

- **Server first.** This PR ships both paths. Old clients keep sending
  `{email}` and `{email, code}` and see the same behaviour, except that the
  code lives as long as the cooldown and shares the account's email limit.
- **Legacy path keeps its limits.** `recoveryMaxAttempts` (5 per record) and
  `recoveryFailureLimit` (10 in 24 h per address) apply only to 6-digit
  submissions, are renamed with a `legacy` prefix, and are deleted with the
  legacy path.
- **What the window fixes.** An owner on a new client cannot be blocked: the
  token path ignores the legacy budget. An owner on an old client keeps
  today's behaviour until they update.
- **Switch:** `RECOVERY_LEGACY_CODE` (default `true`). When `false`:
  - begin always issues a token and answers `format: "token"`;
  - verify with `code` answers `400` (`This app version cannot complete
    recovery. Update the app.`) and touches no state; the answer depends only
    on the request and the config.
  Turning it off locks out every owner whose client cannot accept a key. The
  configuration docs say so, and it is for operators who know all their users
  are on a token-capable client.
- **Removal.** The legacy path and the switch are deleted when no desktop
  release in the supported window (current Stable, previous minor, active
  Pilot) lacks token support. A follow-up issue filed with this PR tracks it,
  blocked on the Desktop change.
- **Desktop change (separate repository).** Send `format: "token"`; when the
  response echoes it, accept a pasted 43-character key and submit `{token}`;
  otherwise keep the 6-digit entry.

### 6. Email

- New template `recovery_key.html` and `Service.SendRecoveryKey(to, token,
  minutes)`. Subject: `Your Concord account recovery key`.
- Body: the key on its own line; "Paste this key into the account recovery
  screen in the app."; "It works once and expires in N minutes."; "If you did
  not ask for it, ignore this email."
- Development mode logs the key under the same `ENVIRONMENT` allowlist as the
  code. Elsewhere no secret is logged.
- The legacy template gets the minutes from the tier instead of a fixed 10.

### 7. Uniformity and timing

- Begin: one handler path for every input. Validation and the response
  `format` depend only on the request and the config.
- Verify, token path: every failure is one `GETDEL` miss and one response. No
  budget, so no `429` or `503` that depends on the account.
- Every Redis read keyed by request-supplied email is preceded by the shape
  check in section 1.
- Nothing logs the address or the secret. Log lines carry a fixed
  `failure_class`. Request bodies on these routes are not logged by any
  middleware (to be confirmed during implementation).

### 8. Documentation in the PR

- `docs/api/openapi.yaml`: `format` on begin request and response; `token` on
  verify-code; `code` deprecated; token-path responses.
- `CHANGELOG.md` `[Prerelease]`: Security and Deprecated entries.
- `.claude/rules/domains/backend.md`: the recovery budget paragraph.
- `docs/adr/0046-account-authentication-and-recovery.md`: the recovery attempt
  policy note.
- `RECOVERY_LEGACY_CODE` in the configuration docs, with the lock-out warning.

## Tests

Handler and Redis level, against the test Redis and database; the route's
client-address limiter is outside the handler under test.

- Token: 32 bytes from `crypto/rand`, 43 characters; two tokens differ; Redis
  holds only the hash key; the raw token appears in no stored value.
- Single use: second submission gets the generic `401`. Concurrent double
  submission: exactly one success.
- Expiry: a token past its TTL gets the generic `401`.
- Uniform failures: wrong token, expired token, a token for no account, and a
  begin for no account give the same status and body as their counterparts.
- **Regression (goal 2):** with a valid token outstanding, send 1,000 wrong
  token submissions and enough wrong 6-digit submissions to exhaust the legacy
  address budget, first from one client address and then spread over many.
  The owner, from a client address not used by the attacker, then completes
  recovery with the valid token. (The per-client-address ban means an owner
  who shares the attacker's address is refused by that ban, as today.)
- **Infeasible by construction:** assert token length and source, and assert
  that after failed token submissions the key space has no new or changed key
  besides the client-address failure record.
- Email limit: repeated begins send one email per cooldown; tier change after
  the third send; record TTL equals slot TTL; alternating `format` values do
  not add emails; differently spelled addresses for one account share the
  limit; redeeming releases the slot.
- Issue script atomicity: a failure inside the script leaves no slot, record
  or counter change.
- Delivery failure and shutdown abandon: record and slot cleared, counter
  refunded.
- Oracle fix: verify with `email: "slot:x@y"` or `email` without `@` answers
  `400` before any Redis read, for existing and missing accounts alike.
- Legacy: `{email, code}` still completes recovery; legacy limits still apply
  to codes; both `code` and `token`, or neither, is `400`;
  `RECOVERY_LEGACY_CODE=false` behaviour on begin and verify.
- MFA: an account with reserved recovery factors gets the recovery challenge
  after a valid token.

## Review record

A security review and a builder check read the first draft. Changes made:

| Finding | Change |
|---|---|
| Crafted legacy-verify email read other key kinds (existence oracle) | Email shape check before any Redis read (section 1) |
| Slot and token TTL order unspecified; partial state after a failed step | One issue script; record TTL equals slot TTL (section 2) |
| Legacy sends bypassed the email limit | Both formats share slot and counter (sections 2, 4) |
| Limit keyed by address, not account | Slot and counter keyed by `user_id` (section 1) |
| Counter not refunded on delivery failure | Cleanup script refunds it (section 2) |
| Shutdown cleanup carried only address and value | `recoveryDelivery` extended (section 2) |
| Client could not tell which format the server sends | `format` echoed in begin response (section 2) |
| Switch-off behaviour on verify unclear | `400`, no state touched, lock-out warning (section 5) |
| Regression test conflicted with the client-address limiter | Test level and owner address stated (Tests) |
| Slot release after redeem not atomic | Accepted residual, stated (section 3) |

## Decisions for the approver

1. `format` on begin, echoed in the response, as the capability handshake.
2. `token` as a new field on the existing verify endpoint, `email` not needed
   with it.
3. Two-tier email limit per account (15 then 60 minutes, 24 a day), shared by
   both formats, no hard cap.
4. Legacy code lifetime follows the tier (up to 60 minutes) instead of 10.
5. `RECOVERY_LEGACY_CODE` switch, default on.

## Left to the builder

Function names, test file layout, and the exact template markup.
