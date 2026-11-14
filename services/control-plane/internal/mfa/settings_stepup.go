package mfa

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/gin-gonic/gin"
)

// This file is the step-up gate for every MFA-settings route. Two shapes share
// ONE decision table (verifyStepUpFactors), ONE refusal writer and ONE
// fail-closed attempt budget:
//
//   - In-transaction, for the four writes that must serialize against a
//     destructive reset (EmailSmsDisable, SetBackupEmail, StoreRecoveryKey
//     overwrite, DeleteRecoveryKey): openMFASettingsTx takes the budget and
//     the password's lockout reservation, opens the transaction and runs
//     stepup.LockSubjectTx (first-statement users-row
//     lock, credential-epoch fence, P1 factor read); verifyMFASettingsStepUpTx
//     then checks the factors on that transaction.
//   - Pool-side, for the nine routes that hold no transaction (TOTPSetup,
//     WebAuthnRegisterBegin, SetRecoveryOnly, SetRecoveryHardened,
//     EmailSmsSetup, DesignateTrustedDevice, RemoveTrustedDevice,
//     UpsertRecoveryCircle, DeleteRecoveryCircle): requirePasswordAndMFA takes
//     the budget, reads the subject with stepup.LoadSubject and checks the
//     factors with the pool-scoped verifier.
//
// Both answer with the internal/stepup bodies, so the client parses one
// refusal shape for all thirteen.
//
// The invariant the in-transaction functions serve: between the row lock and
// the commit, nothing reads or writes through h.db. The transaction holds a
// pooled connection AND a lock every destructive reset needs, so a pool read
// there either waits on a second connection while holding the first or, on a
// saturated pool, waits forever on the connection it is itself holding.

// mfaStepUpCredentials is the step-up proof the MFA-settings routes carry. It
// is embedded in each request struct and is never a column field
// ([internal]rules/backend.md, "Disabling a security control"). No binding tags:
// a missing credential is the seam's actionable 403, not a gin 400.
type mfaStepUpCredentials struct {
	Password string `json:"password"` //nolint:gosec // request field, not a secret
	MFACode  string `json:"mfa_code"`
}

// sent reports whether the request carried any credential at all. Only such a
// request is charged against the budget (see stepup.Budget).
func (c mfaStepUpCredentials) sent() bool { return c.Password != "" || c.MFACode != "" }

// stepUpLock is the users-row lock an in-transaction gate takes. Strength
// follows the writes; see stepup.Lock.
type stepUpLock = stepup.Lock

const (
	lockForShare       = stepup.LockForShare
	lockForNoKeyUpdate = stepup.LockForNoKeyUpdate
)

// gateStage names where a step-up stopped. Security events are chosen from
// this enum, never from response-body text.
type gateStage int

const (
	stageNone gateStage = iota
	stageEpochMismatch
	stagePasswordMissing
	stagePasswordInvalid
	stageMFAMissing
	stageMFAInvalid
	stageInternal
	stageSubjectGone
	stageAccountLocked
)

// accountLockedEvent is the step-up mint's lockout event: the account's
// shared /auth/login lockout refused a password before any hash work.
var accountLockedEvent = securityevent.Event{
	EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
	Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAccountLocked,
	AuthMethod: securityevent.AuthPassword,
}

const (
	// mfaSettingsStepUpLimit is the shared budget's bound, re-exported for
	// this package's tests.
	mfaSettingsStepUpLimit = stepup.BudgetLimit
	// mfaSettingsStepUpPrefix keys the one budget every MFA-settings route
	// charges. The MFA-enforcement toggle charges it too (see
	// stepup.MFASettingsBudgetPrefix).
	mfaSettingsStepUpPrefix = stepup.MFASettingsBudgetPrefix
	// postCommitTimeout bounds work that must outlive a cancelled request.
	postCommitTimeout = 5 * time.Second
)

// mfaSettingsStepUpKey is the budget's Redis key for userID.
func mfaSettingsStepUpKey(userID string) string {
	return stepup.NewBudget(nil, mfaSettingsStepUpPrefix).Key(userID)
}

func (h *Handler) stepUpBudget() stepup.Budget {
	return stepup.NewBudget(h.redis, mfaSettingsStepUpPrefix)
}

// bindOptionalJSON binds a JSON body that may be absent. An empty body binds
// as the zero value, so an old renderer's body-less request reaches the
// step-up gate and gets the actionable 403 rather than a 400. Malformed JSON
// is still an error.
func bindOptionalJSON(c *gin.Context, dst any) error {
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		return nil
	}
	if err := c.ShouldBindJSON(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// rollbackQuietly is the deferred rollback; sql.ErrTxDone after a commit is
// the expected case, anything else is logged.
func (h *Handler) rollbackQuietly(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		h.log.Error("MFA settings transaction rollback failed", "error", err)
	}
}

// openMFASettingsTx runs the common prefix of every in-transaction gated
// write: the fail-closed budget (only when credentials were sent), the
// password's lockout reservation, BeginTx, and stepup.LockSubjectTx. Both
// Redis steps run BEFORE BeginTx (see stepup.Budget). On refusal it has
// already written the response and returns ok=false with a nil tx.
//
// The caller defers endMFASettingsTx(tx, attempt) and, once its write has
// committed, calls clearTxStepUpAfterCommit.
func (h *Handler) openMFASettingsTx(
	c *gin.Context, userID string, creds mfaStepUpCredentials, lock stepUpLock,
) (tx *sql.Tx, subj stepup.Subject, attempt *stepup.PasswordAttempt, ok bool) {
	if creds.sent() && !h.allowMFASettingsStepUp(c, userID) {
		return nil, stepup.Subject{}, nil, false
	}
	attempt, ok = h.reserveStepUpPassword(c, userID, creds.Password)
	if !ok {
		return nil, stepup.Subject{}, nil, false
	}
	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin MFA settings transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": stepup.ErrMsgVerificationFailed})
		return nil, stepup.Subject{}, nil, false
	}
	subj, e := stepup.LockSubjectTx(ctx, tx, userID, lock, middleware.TokenCredentialEpoch(c))
	if e != nil {
		h.rollbackQuietly(tx)
		h.refuseMFASettingsStepUp(c, e, subjectStage(e))
		return nil, stepup.Subject{}, nil, false
	}
	return tx, subj, attempt, true
}

// reserveStepUpPassword counts a supplied password against the account's
// shared /auth/login lockout. Call it BEFORE BeginTx: it reads the email
// through the pool and makes a Redis round trip. On refusal it has written
// the response (423 while locked, 500 when the guess cannot be counted, 401
// for a deleted account) and returns ok=false. An empty password counts
// nothing and returns an unreserved attempt.
func (h *Handler) reserveStepUpPassword(c *gin.Context, userID, password string) (*stepup.PasswordAttempt, bool) {
	attempt, e := stepup.ReserveUserPasswordAttempt(c.Request.Context(), h.db, h.redis, userID, password)
	if e != nil {
		h.refuseMFASettingsStepUp(c, e, reserveStage(e))
		return nil, false
	}
	return attempt, true
}

// reserveStage classifies a refusal from stepup.ReserveUserPasswordAttempt.
func reserveStage(e *stepup.Error) gateStage {
	switch {
	case e.Status == http.StatusLocked:
		return stageAccountLocked
	case e.Status >= http.StatusInternalServerError:
		return stageInternal
	default:
		return stageSubjectGone
	}
}

// endMFASettingsTx is the deferred end of a gated transaction: the rollback
// (a no-op after a commit), THEN the lockout settle of the password guess, so
// the Redis round trip never runs while the transaction holds its lock.
func (h *Handler) endMFASettingsTx(tx *sql.Tx, attempt *stepup.PasswordAttempt) {
	h.rollbackQuietly(tx)
	h.settleStepUpPassword(attempt)
}

// settleStepUpPassword records a wrong step-up password against the lockout
// or resets the count after a correct one (stepup.PasswordAttempt.Settle).
// Best-effort: the guess was already counted when it was reserved.
func (h *Handler) settleStepUpPassword(attempt *stepup.PasswordAttempt) {
	if err := attempt.Settle(context.Background()); err != nil {
		h.log.Warn("Could not settle a step-up password against the lockout", "error", err)
	}
}

// clearTxStepUpAfterCommit is the in-transaction gate's post-commit reset of
// the shared budget. Call it only after the write committed; the deferred
// endMFASettingsTx settles the password's lockout count.
func (h *Handler) clearTxStepUpAfterCommit(c *gin.Context, userID string) {
	h.clearStepUpAfterSuccess(c, userID)
}

// subjectStage classifies a refusal from stepup.LoadSubject/LockSubjectTx.
func subjectStage(e *stepup.Error) gateStage {
	switch {
	case e.EpochMismatch():
		return stageEpochMismatch
	case e.Status >= http.StatusInternalServerError:
		return stageInternal
	default:
		return stageSubjectGone
	}
}

// verifyMFASettingsStepUpTx checks the factors on the caller's transaction.
// The Subject already carries the P1 set LockSubjectTx read under the lock,
// and attempt was reserved by openMFASettingsTx, so nothing here touches the
// pool or Redis.
func (h *Handler) verifyMFASettingsStepUpTx(
	ctx context.Context, tx *sql.Tx, userID string, subj stepup.Subject,
	creds mfaStepUpCredentials, gate settingsStepUp, attempt *stepup.PasswordAttempt,
) (*stepup.Error, gateStage) {
	return verifyStepUpFactors(attempt, subj, creds, gate.wording, func() *stepup.Error {
		// Tx form: backup-code redemption is a write that a rollback must be
		// able to undo. The preloaded set keeps GetEnabledMethods (a pool
		// read) from ever running under the lock.
		return stepup.VerifyMFAFactorTx(ctx, tx, h, userID, gate.purpose, creds.MFACode, subj.MFAMethods)
	})
}

// verifyStepUpFactors is the one decision table both gate shapes share: the
// password factor, then — only for an inline-verifiable factor (policy P1) —
// the MFA factor, each refusal tagged with the stage its event is chosen by.
//
// attempt is the password's lockout reservation, taken before any
// transaction; this makes no Redis call. A locked account was refused at the
// reservation, so no 423 comes from here.
func verifyStepUpFactors(
	attempt *stepup.PasswordAttempt,
	subj stepup.Subject, creds mfaStepUpCredentials, wording stepup.Copy, verifyMFA func() *stepup.Error,
) (*stepup.Error, gateStage) {
	if pErr := stepup.VerifyPasswordFactor(attempt, subj, creds.Password, wording); pErr != nil {
		switch {
		case pErr.Status >= http.StatusInternalServerError:
			return pErr, stageInternal
		case pErr.Status == http.StatusBadRequest || creds.Password == "":
			return pErr, stagePasswordMissing
		default:
			return pErr, stagePasswordInvalid
		}
	}
	if !subj.MFAEnabled {
		return nil, stageNone
	}
	if mErr := verifyMFA(); mErr != nil {
		switch {
		case mErr.Status >= http.StatusInternalServerError:
			return mErr, stageInternal
		case creds.MFACode == "":
			return mErr, stageMFAMissing
		default:
			return mErr, stageMFAInvalid
		}
	}
	return nil, stageNone
}

// requirePasswordAndMFA is the pool-side gate for the nine MFA routes that
// hold no transaction. It returns the Subject so a route can consult its P1
// set (EmailSmsSetup does) without a second read. On refusal it has written
// the response and returns ok=false.
//
// It charges the same fail-closed budget as the in-transaction gate. The
// route must call clearStepUpAfterSuccess once its own write has succeeded.
//
// The password counts against the shared /auth/login lockout. No transaction
// is open here, so the guess is settled at once: a wrong one is recorded and
// a correct one resets the count, even when the MFA leg then fails.
func (h *Handler) requirePasswordAndMFA(
	c *gin.Context, userID string, creds mfaStepUpCredentials, gate settingsStepUp,
) (stepup.Subject, bool) {
	if creds.sent() && !h.allowMFASettingsStepUp(c, userID) {
		return stepup.Subject{}, false
	}
	attempt, ok := h.reserveStepUpPassword(c, userID, creds.Password)
	if !ok {
		return stepup.Subject{}, false
	}
	ctx := c.Request.Context()
	subj, e := stepup.LoadSubject(ctx, h.db, userID)
	if e != nil {
		h.refuseMFASettingsStepUp(c, e, subjectStage(e))
		return stepup.Subject{}, false
	}
	e, stage := verifyStepUpFactors(attempt, subj, creds, gate.wording, func() *stepup.Error {
		return stepup.VerifyMFAFactor(ctx, h, userID, gate.purpose, creds.MFACode, subj.MFAMethods)
	})
	h.settleStepUpPassword(attempt)
	if e != nil {
		h.refuseMFASettingsStepUp(c, e, stage)
		return stepup.Subject{}, false
	}
	return subj, true
}

// refuseMFASettingsStepUp emits the stage's event, logs a 500's cause (a 4xx
// carries none — a rejected credential is an outcome, not a fault), and
// writes the seam's body unchanged.
func (h *Handler) refuseMFASettingsStepUp(c *gin.Context, e *stepup.Error, stage gateStage) {
	switch stage {
	case stageAccountLocked:
		h.emitHTTPEvent(c, accountLockedEvent)
	case stagePasswordInvalid:
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials, AuthMethod: securityevent.AuthPassword})
	case stageMFAInvalid:
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid})
	case stageEpochMismatch:
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventCredentialEpoch, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonCredentialEpochMismatch})
	}
	if e.Cause != nil {
		h.log.Error("MFA settings step-up failed", "status", e.Status, "error", e.Cause)
	}
	e.Write(c)
}

// allowMFASettingsStepUp consumes one attempt from the shared fail-closed
// budget, before any transaction opens. On refusal it emits the matching
// event, writes the budget's 429 (exhausted) or 503 (unevaluable) and returns
// false.
func (h *Handler) allowMFASettingsStepUp(c *gin.Context, userID string) bool {
	e := h.stepUpBudget().Consume(c.Request.Context(), userID)
	if e == nil {
		return true
	}
	if e.Status == http.StatusServiceUnavailable {
		h.log.Error("MFA settings step-up budget unavailable", "error", e.Cause)
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonRateLimitBackendUnavailable})
	} else {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonRateLimitExceeded})
	}
	e.Write(c)
	return false
}

// chargeMFASettingsCode is the code-only counterpart for the two legacy MFA
// settings routes that do not use the shared settings gate.
func (h *Handler) chargeMFASettingsCode(c *gin.Context, userID, code string) bool {
	e := stepup.Charge(c.Request.Context(), h.stepUpBudget(), userID, code)
	if e == nil {
		return true
	}
	if e.Status == http.StatusServiceUnavailable {
		h.log.Error("MFA settings step-up budget unavailable", "error", e.Cause)
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonRateLimitBackendUnavailable})
	} else {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonRateLimitExceeded})
	}
	e.Write(c)
	return false
}

// clearMFASettingsStepUp resets the budget after a VERIFIED success, so a
// legitimate user is not locked out by their own changes. Best-effort: a
// failure leaves the counter high, which fails toward more limiting.
func (h *Handler) clearMFASettingsStepUp(ctx context.Context, userID string) {
	if err := h.stepUpBudget().Clear(ctx, userID); err != nil {
		h.log.Warn("Could not reset the MFA settings step-up budget after a successful change", "error", err)
	}
}

// clearStepUpAfterSuccess is clearMFASettingsStepUp on a context detached
// from the request, so a client that hangs up after the commit does not leave
// the budget charged for a change that happened.
func (h *Handler) clearStepUpAfterSuccess(c *gin.Context, userID string) {
	post, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), postCommitTimeout)
	defer cancel()
	h.clearMFASettingsStepUp(post, userID)
}
