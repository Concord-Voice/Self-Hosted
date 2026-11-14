package mfa

import (
	"context"
	"database/sql"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

var factorRemovalCopy = stepup.Copy{
	NoFactors:          "No authentication factor is available",
	CredentialRequired: errMsgPasswordRequired,
}

// requireFactorRemovalStepUpTx verifies the fresh proof for a factor removal
// against the caller's already-locked subject. It makes no Redis or lock call.
func (h *Handler) requireFactorRemovalStepUpTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	subject stepup.Subject,
	attempt *stepup.PasswordAttempt,
	password, mfaCode string,
	purpose stepup.Purpose,
) *stepup.Error {
	e, _ := verifyStepUpFactors(attempt, subject, mfaStepUpCredentials{Password: password, MFACode: mfaCode}, factorRemovalCopy, func() *stepup.Error {
		return stepup.VerifyMFAFactorTx(ctx, tx, h, userID, purpose, mfaCode, subject.MFAMethods)
	})
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
