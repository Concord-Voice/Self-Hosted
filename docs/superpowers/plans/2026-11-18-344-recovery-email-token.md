# Email Recovery Key Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the guessable 6-digit email recovery code with a 256-bit single-use recovery key, keeping the 6-digit path for old clients.

**Architecture:** All work stays in `internal/auth` (recovery section of `handlers.go`, split into a new `recovery_email_token.go` for the key store) plus `internal/email`. Three Lua scripts own the Redis state (issue, cleanup, slot release); `GETDEL` consumes a key. Begin and verify gain one request field each.

**Tech Stack:** Go, gin, go-redis v9, miniredis (unit tests), PostgreSQL + Redis test server (integration tests).

**Spec:** `docs/superpowers/specs/2026-11-18-344-recovery-email-token-design.md`

## Global Constraints

- Token: 32 bytes `crypto/rand`, `base64.RawURLEncoding`, 43 characters.
- Keys: `recovery_code:token:<hex sha256>`, `recovery_code:slot:<user_id>`, `recovery_code:sends:<user_id>`, legacy `recovery_code:<email>`.
- Tiers: count 1-3 → 15 min; count ≥4 → 60 min. Counter expiry 24 h, set once (`EXPIRE NX`).
- Request email shape: contains `@`, no `:`; else `400 {"error":"Valid email is required"}`, before any Redis read.
- Begin `format`: absent/`"code"` → code, `"token"` → token, other → `400`. Response adds `"format"`.
- Begin message text: `If an account exists with that email, a recovery email has been sent`.
- Verify: exactly one of `code`/`token`, else `400`. Token miss: `401 {"error":"Invalid or expired recovery code"}`, no `attempts_remaining`.
- Switch `RECOVERY_LEGACY_CODE` default `true`. When `false`, verify with `code` → `400 {"error":"This app version cannot complete recovery. Update the app."}`, no state touched.
- Never log an address, a code or a token outside the existing dev-mode allowlist. Public repository: no private tracker references.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A token with surrounding whitespace or a trailing newline pasted from email — expected to be accepted (trim before the shape check). Test in Task 4.
2. Begin with `format: "token"` and an email in mixed case — slot and counter shared with the lower-case spelling (keyed by `user_id`). Test in Task 3.
3. Redis down during begin — generic `200`, no partial keys, failure logged with a fixed class. Test in Task 3.
4. A legacy code record live when a token begin arrives — no second email (slot held). Test in Task 3.
5. JSON `token: null` or a number — `400`, not a panic or `500`. Test in Task 4.

---

### Task 1: Email key template and sender

**Files:**
- Create: `services/control-plane/internal/email/templates/recovery_key.html`
- Modify: `services/control-plane/internal/email/service.go` (recovery senders), `services/control-plane/internal/email/templates/recovery.html` (minutes)
- Test: `services/control-plane/internal/email/service_test.go`

**Interfaces:**
- Produces: `func (s *Service) SendRecoveryKey(to, key string, minutes int) error`; `func (s *Service) SendRecoveryCode(to, code string, minutes int) error` (new `minutes` parameter).

- [ ] **Step 1: Tests** — `TestRecoveryKeyTemplateRendersKeyAndMinutes` (output contains the key and `expires in 60 minutes`); `TestRecoveryTemplateRendersMinutes` (`expires in 15 minutes`); `TestSendRecoveryKeyWithoutDeliveryFails` (dev mode with code logging off → `ErrDeliveryNotConfigured`).
- [ ] **Step 2: Run** `go test ./internal/email/ -run 'Recovery' -v` → FAIL (undefined).
- [ ] **Step 3: Implement.** Copy `recovery.html` markup for `recovery_key.html`; key in a monospace block with `word-break:break-all`, letter-spacing 0. Copy per spec §6. Subject `Your Concord account recovery key`. Template data keys `Key`/`Code` and `Minutes`. Update the one caller of `SendRecoveryCode` to pass `10` for now (Task 3 replaces it).
- [ ] **Step 4: Run** the same command → PASS. `go build ./...` → OK.
- [ ] **Step 5: Commit** `feat(email): add the recovery key email`.

### Task 2: Legacy switch configuration

**Files:**
- Modify: `services/control-plane/pkg/config/config.go`, `services/control-plane/internal/auth/handlers.go` (field + setter), `services/control-plane/internal/api/router.go` (wire after `SetEmailService`)
- Test: `services/control-plane/pkg/config/config_test.go`

**Interfaces:**
- Produces: `Config.RecoveryLegacyCode bool`; `func (h *Handler) SetRecoveryLegacyCode(enabled bool)`; handler field `recoveryLegacyCode bool`, default `true` in `NewHandlerForInstance`.

- [ ] **Step 1: Test** `TestRecoveryLegacyCodeDefaultsOn` (unset → `true`; `"false"` → `false`). Follow the file's existing bool-env pattern; invalid value behaves as that pattern does elsewhere.
- [ ] **Step 2: Run** `go test ./pkg/config/ -run RecoveryLegacy -v` → FAIL.
- [ ] **Step 3: Implement** with the existing bool env helper.
- [ ] **Step 4: Run** → PASS; `go build ./...`.
- [ ] **Step 5: Commit** `feat(config): add RECOVERY_LEGACY_CODE`.

### Task 3: Recovery key store and begin

**Files:**
- Create: `services/control-plane/internal/auth/recovery_email_token.go`
- Modify: `services/control-plane/internal/auth/handlers.go` (`RecoveryBegin`, queue, `processRecoveryBegin`, `recoveryDelivery`, `deliverRecoveryCode`, cleanup helpers, constants)
- Test: `services/control-plane/internal/auth/recovery_email_token_test.go` (miniredis), extend `recovery_begin_async_test.go`

**Interfaces:**
- Consumes: Task 1 senders; Task 2 `h.recoveryLegacyCode`.
- Produces:
  - `type recoveryFormat string` with `recoveryFormatCode = "code"`, `recoveryFormatToken = "token"`.
  - `func generateRecoveryEmailToken() (string, error)`; `func recoveryEmailTokenKey(token string) string`; `func recoverySlotKey(userID string) string`; `func recoverySendsKey(userID string) string`.
  - `func validRecoveryEmail(addr string) bool` (contains `@`, no `:`).
  - `type recoveryIssue struct{ RecordKey, SlotKey, SendsKey, SendID string; Record []byte }`
  - `func (h *Handler) issueRecoveryRecord(ctx context.Context, in recoveryIssue) (ttl time.Duration, held bool, err error)` — issue script: slot exists → held; `INCR` sends, `EXPIRE NX` 24 h; tier; `SET` slot and record `PX` tier.
  - `func (h *Handler) clearUnsentRecovery(ctx context.Context, in recoveryIssue) error` — cleanup script: if slot == SendID: `DEL` record, `DEL` slot, `DECR` sends (floor 0, keep TTL).
  - `recoveryDelivery` becomes `{issue recoveryIssue; addr, secret string; format recoveryFormat; ttl time.Duration}`; queue items become `recoveryJob{addr string; format recoveryFormat}`.
  - Legacy record JSON adds nothing; its key stays `recoveryRedisKey(addr)`; its `SETNX` is replaced by the issue script.
- [ ] **Step 1: Unit tests (miniredis)**
  - `TestRecoveryEmailTokenShape`: 43 chars, decodes to 32 bytes, 100 tokens all distinct.
  - `TestIssueRecoveryRecordTiers`: calls 1-3 (slot deleted between calls) give 15 min; 4th gives 60 min; record TTL == slot TTL each time; sends TTL ≤ 24 h and not reset by later calls.
  - `TestIssueRecoveryRecordHeldSendsNothing`: second issue while slot held returns `held=true`, counter unchanged, record not written.
  - `TestIssueRecoveryRecordIsAtomicOnFailure`: miniredis `SetError` during the script → no slot, record or counter.
  - `TestClearUnsentRecoveryRefundsOnlyOwnSend`: matching SendID clears record and slot and decrements counter; non-matching SendID changes nothing.
  - `TestValidRecoveryEmail`: `a@b.c` true; `slot:a@b.c`, `ab.c`, `token:x` false.
- [ ] **Step 2: Begin tests** (extend async test file)
  - `TestRecoveryBeginFormatEcho`: `{}`→ 400 (unchanged); `{email}` → `format:"code"`; `{email, format:"token"}` → `format:"token"`; `format:"x"` → 400 without queueing; `{email:"slot:a@b"}` → 400 without queueing; switch off + `{email}` → `format:"token"`.
  - `TestRecoveryBeginTokenSendsKeyEmail`: worker with fake sender records `SendRecoveryKey` with a 43-char key and 15 minutes; Redis has `recovery_code:token:<hash of that key>` with `user_id`; no key contains the raw token.
  - `TestRecoveryBeginAlternatingFormatsShareSlot`: code begin then token begin for one account → one email.
  - `TestRecoveryBeginMixedCaseSharesSlot` (Review Focus 2).
  - `TestRecoveryBeginRedisDownLeavesNoState` (Review Focus 3).
  - Update `TestAbandonedRecoveryDrainClearsUnsentCodes` for token deliveries: record and slot cleared, counter refunded.
  Introduce a narrow sender seam if tests need it: `type recoveryMailer interface { SendRecoveryCode(to, code string, minutes int) error; SendRecoveryKey(to, key string, minutes int) error }`, held by the handler and set from `SetEmailService`. Only add it if the existing tests' SMTP fake cannot observe the call.
- [ ] **Step 3: Run** `go test ./internal/auth/ -run 'RecoveryEmailToken|IssueRecovery|ClearUnsent|ValidRecoveryEmail|RecoveryBegin|AbandonedRecovery' -v` → FAIL.
- [ ] **Step 4: Implement.** Begin handler: bind `{email, format}`, trim/lower email, check shape, map format, apply switch, queue, respond with message + format. Worker: lookup user; build secret (token or 6-digit); record value (`{user_id, send_id}` for token, existing `recoveryRecord` for code); issue script; on `held` log the existing info line and stop; track delivery; send with `int(ttl/time.Minute)`; on failure or abandon `clearUnsentRecovery`. Delete `deleteRecoveryRecordIfSameSendLua` and its tests once nothing calls it. Rename `recoveryMaxAttempts`/`recoveryFailureLimit`/`recoveryFailureWindow` to `legacyRecovery*`.
- [ ] **Step 5: Run** the Step 3 command and `go test ./internal/auth/ -run Recovery` (unit only, no DB) → PASS.
- [ ] **Step 6: Commit** `feat(auth): issue recovery keys and share the email limit`.

### Task 4: Verify with a recovery key

**Files:**
- Modify: `services/control-plane/internal/auth/handlers.go` (`parseRecoveryVerifyCodeRequest`, `RecoveryVerifyCode`), `recovery_email_token.go`
- Test: `services/control-plane/internal/auth/recovery_email_token_test.go`

**Interfaces:**
- Consumes: Task 3 keys and `validRecoveryEmail`.
- Produces:
  - `type recoveryVerifyRequest struct{ Email string; Code, Token *string }` and `func parseRecoveryVerifyRequest(c *gin.Context, legacyEnabled bool) (recoveryVerifyRequest, bool)`.
  - `func (h *Handler) consumeRecoveryEmailToken(ctx context.Context, token string) (userID string, err error)` — `GETDEL`; miss → `redis.Nil`; then slot release script (`DEL` slot if value == record `send_id`), errors on release logged and ignored.
- [ ] **Step 1: Tests**
  - `TestParseRecoveryVerifyRequest` table: both fields → 400; neither → 400; `token: null` / `token: 7` → 400 (Review Focus 5); token wrong length → 400; token with spaces/newline around → accepted, trimmed (Review Focus 1); code path with `email:"slot:a@b"` → 400; switch off + code → 400 with the update message.
  - `TestConsumeRecoveryEmailTokenSingleUse`: first consume returns user id; second returns `redis.Nil`; slot released when send_id matches, kept when it does not.
  - `TestConsumeRecoveryEmailTokenConcurrent`: 20 goroutines, exactly one success.
  - `TestTokenFailuresWriteNoBudget`: after 50 wrong-token handler calls, miniredis keys contain no `recovery_fail:` and no new `recovery_code:` key.
- [ ] **Step 2: Run** `go test ./internal/auth/ -run 'ParseRecoveryVerify|ConsumeRecoveryEmailToken|TokenFailures' -v` → FAIL.
- [ ] **Step 3: Implement.** In `RecoveryVerifyCode`: token path → consume; miss → existing event + `RecordAuthFailure` + `401 {"error": errInvalidExpiredRecoveryCode}`; error → 500; hit → `recoveryPolicyAllowsEmailCompletion` then `CompleteRecovery`; after an email-only completion, read `users.email` by `user_id` and clear `recovery_fail:<lower(email)>` with the existing `clearRecoveryFailureBudget` (a read failure is logged and skipped; the MFA branch is already cleared inside `CompleteRecovery`). Code path unchanged behind the legacy budget.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(auth): verify recovery with a single-use key`.

### Task 5: Integration tests

**Files:**
- Create: `services/control-plane/internal/auth/recovery_email_token_integration_test.go`
- Modify: `services/control-plane/internal/auth/recovery_integration_test.go` (begin message text, legacy TTL expectations)

**Interfaces:**
- Consumes: the test server helper in `recovery_integration_test.go` (accepted SMTP payloads give the emailed key).

- [ ] **Step 1: Tests**
  - `TestRecoveryKeyCompletesRecovery`: begin token → read key from captured email → verify `{token}` → 200 with `recovery_token`.
  - `TestRecoveryKeySurvivesWrongSubmissionsFromManyAddresses` (regression): key outstanding; 1,000 wrong tokens and 11 wrong codes for the address, rotating client addresses via `X-Forwarded-For` with the test server's trusted proxy (or one fixed address); then owner from an unused client address completes. Run with the IP limiter in place by giving each request a distinct address; the single-address variant uses one attacker address and a different owner address.
  - `TestRecoveryKeyUniformFailures`: wrong, expired (miniredis-free: set PX small and sleep, or `Redis.PExpire`), no-account → identical status and body.
  - `TestRecoveryKeyMFAAccountGetsChallenge`: account with a reserved factor → recovery challenge response, not `recovery_token`.
  - `TestRecoveryLegacyCodeStillCompletes`: `{email}` begin, `{email, code}` verify → 200.
- [ ] **Step 2: Run** with disposable services: `DATABASE_URL=... REDIS_URL=... go test ./internal/auth/ -run 'RecoveryKey|RecoveryLegacy|RecoveryBegin|RecoveryVerify' -p 1 -v` → PASS (after Tasks 3-4; failures here are bugs to fix in those tasks' code).
- [ ] **Step 3: Commit** `test(auth): cover recovery key flows end to end`.

### Task 6: Documentation and follow-up

**Files:**
- Modify: `docs/api/openapi.yaml` (`/auth/recovery/begin`, `/auth/recovery/verify-code`), `CHANGELOG.md`, `.claude/rules/domains/backend.md` (recovery budget paragraph), `docs/adr/0046-account-authentication-and-recovery.md` (recovery attempt policy note), the configuration document that lists control-plane env vars (find with `grep -rn CLIENT_MIN_VERSION docs`).

- [ ] **Step 1:** Update each document per spec §8. Changelog `[Prerelease]`: Security — `**Auth:** Email account recovery now sends a single-use recovery key that cannot be guessed, so wrong attempts by others can no longer block the account owner's email recovery.`; Deprecated — `**Auth:** The 6-digit email recovery code is deprecated. Clients should request a recovery key with \`format: "token"\`.`
- [ ] **Step 2:** Validate OpenAPI with the repo's existing check (find it in `.github/workflows` or `package.json`); run `pre-commit run --files <changed docs>` if configured.
- [ ] **Step 3: Commit** `docs: describe the recovery key`.
- [ ] **Step 4:** File the follow-up issue via `/issue-creation`: remove the legacy 6-digit recovery path and `RECOVERY_LEGACY_CODE` once no supported desktop release lacks key support; blocked on the Desktop change.

### Task 7: Validate

- [ ] `cd services/control-plane && go vet ./... && golangci-lint run ./internal/auth/... ./internal/email/... ./pkg/config/...` → clean.
- [ ] Full `internal/auth`, `internal/email`, `pkg/config`, `internal/api` tests with disposable DB/Redis → PASS; coverage of `recovery_email_token.go` ≥ 80%.
