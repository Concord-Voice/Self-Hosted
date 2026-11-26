package mfa

import (
	"context"
	"database/sql"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
)

var factorRemovalCopy = stepup.Copy{
	NoFactors:          "No authentication factor is available",
	CredentialRequired: errMsgPasswordRequired,
}

// requireFactorRemovalStepUpTx verifies the fresh proof for a factor removal
// against the caller's already-locked subject. It makes no Redis or lock call
// while the transaction is open; a busy backup-code check ends the transaction
// first and then refunds the charge.
func (h *Handler) requireFactorRemovalStepUpTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	subject stepup.Subject,
	charge stepUpCharge,
	creds mfaStepUpCredentials,
	purpose stepup.Purpose,
) *stepup.Error {
	e, _ := verifyStepUpFactors(charge.attempt, subject, creds, factorRemovalCopy, func() *stepup.Error {
		return stepup.VerifyMFAFactorTx(ctx, tx, h, userID, purpose, creds.MFACode, subject.MFAMethods)
	})
	if stepup.IsBusy(e) {
		h.rollbackQuietly(tx)
		h.refundBusyStepUp(ctx, charge.charged, e)
	}
	return e
}

func factorRemovalStage(e *stepup.Error, password, mfaCode string) gateStage {
	if e == nil {
		return stageNone
	}
	if e.Status >= 500 {
		return stageInternal
	}
	if password == "" {
		return stagePasswordMissing
	}
	if e.Body["error"] == stepup.ErrMsgInvalidPassword {
		return stagePasswordInvalid
	}
	if mfaCode == "" {
		return stageMFAMissing
	}
	return stageMFAInvalid
}
