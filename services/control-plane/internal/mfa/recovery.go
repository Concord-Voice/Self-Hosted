package mfa

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"fmt"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// RecoveryCompleter issues final recovery authority after the challenge is claimed.
type RecoveryCompleter interface {
	CompleteRecovery(c *gin.Context, userID, verifiedFactor string) bool
}

// SetRecoveryCompleter wires final recovery issuance after factor verification.
func (h *Handler) SetRecoveryCompleter(completer RecoveryCompleter) {
	h.recoveryCompleter = completer
}

// GetRecoveryMethods reads the actual factors rather than the denormalized MFA
// flags. Sign-in's effectiveRecoveryOnly lapse must not weaken recovery policy.
// The caller has already proved email ownership; one reserved inline factor is
// additionally required when configured. Backup codes belong to TOTP.
func (h *Handler) GetRecoveryMethods(ctx context.Context, userID string) ([]string, error) {
	methods, _, err := h.readRecoveryPolicy(ctx, userID)
	return methods, err
}

// readRecoveryPolicy is the unlocked policy read. It also returns the
// credential epoch (empty when never rotated) from the same statement, so the
// recovery token pre-check costs no extra round trip.
func (h *Handler) readRecoveryPolicy(ctx context.Context, userID string) ([]string, string, error) {
	var reserved, flags []string
	var hardened, totpEnabled, webauthnEnabled bool
	var credEpoch string
	if err := h.db.QueryRowContext(ctx, `
		SELECT recovery_only_methods, recovery_hardened, mfa_methods,
		 EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = users.id AND enabled AND confirmed),
		 EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = users.id),
		 COALESCE(credential_epoch, '')
		FROM users WHERE id = $1`, userID,
	).Scan(pq.Array(&reserved), &hardened, pq.Array(&flags), &totpEnabled, &webauthnEnabled, &credEpoch); err != nil {
		return nil, "", fmt.Errorf("read recovery policy: %w", err)
	}
	_, smsEnabled, err := h.readEmailSmsEnabled(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	methods, err := recoveryMethods(reserved, flags, hardened, totpEnabled, webauthnEnabled, smsEnabled)
	if err != nil {
		return nil, "", err
	}
	return methods, credEpoch, nil
}

func recoveryMethods(reserved, flags []string, hardened, totpEnabled, webauthnEnabled, smsEnabled bool) ([]string, error) {
	// SMS has no recovery verifier yet. Never downgrade an enrolled dual-channel
	// policy; the default hardened flag remains dormant without enrolled SMS.
	if hardened && (smsEnabled || containsStr(flags, "sms")) {
		return nil, fmt.Errorf("SMS recovery verification is unavailable")
	}
	methods := make([]string, 0, 2)
	for _, method := range reserved {
		switch method {
		case "totp":
			if totpEnabled && !containsStr(methods, method) {
				methods = append(methods, method)
			}
		case "webauthn":
			if webauthnEnabled && !containsStr(methods, method) {
				methods = append(methods, method)
			}
		case "email": // Proved by the recovery email code.
		case "sms":
			return nil, fmt.Errorf("SMS recovery verification is unavailable")
		default:
			return nil, fmt.Errorf("unknown recovery factor")
		}
	}
	return methods, nil
}

// lockRecoveryPolicyTx takes the users row lock every recovery consumer and
// every credential writer shares, then reads current policy and the credential
// epoch in one fresh statement snapshot, after waiting for any factor writer.
func lockRecoveryPolicyTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, string, error) {
	if err := lockUserForMFAWriteTx(ctx, tx, userID); err != nil {
		return nil, "", fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	var reserved, flags []string
	var hardened, totpEnabled, webauthnEnabled bool
	var credEpoch string
	if err := tx.QueryRowContext(ctx, `
  SELECT recovery_only_methods, recovery_hardened, mfa_methods,
   EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = users.id AND enabled AND confirmed),
   EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = users.id),
   COALESCE(credential_epoch, '')
  FROM users WHERE id=$1`, userID).Scan(pq.Array(&reserved), &hardened, pq.Array(&flags), &totpEnabled, &webauthnEnabled, &credEpoch); err != nil {
		return nil, "", fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	methods, err := recoveryMethods(reserved, flags, hardened, totpEnabled, webauthnEnabled, false)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", auth.ErrRecoveryPolicyUnavailable, err)
	}
	return methods, credEpoch, nil
}

// IssueRecoveryTokenTx mints final recovery authority under the users row lock
// that also evaluated current policy, stamping the credential epoch read in the
// same statement. The caller never handles the epoch, so it cannot be omitted
// or confused with another string. Implements auth.MFAChecker.
func (h *Handler) IssueRecoveryTokenTx(ctx context.Context, tx *sql.Tx, userID, verifiedFactor string) (string, string, error) {
	if tx == nil || !isRecoveryFactor(verifiedFactor) {
		return "", "", auth.ErrRecoveryProofInvalid
	}
	methods, credEpoch, err := lockRecoveryPolicyTx(ctx, tx, userID)
	if err != nil {
		return "", "", err
	}
	if len(methods) > 0 && !containsStr(methods, verifiedFactor) {
		return "", "", auth.ErrRecoveryProofInvalid
	}
	return GenerateRecoveryToken(userID, verifiedFactor, JWTSecret(h.jwtSecret), CredEpoch(credEpoch))
}

// ValidateRecoveryClaimsTx linearizes recovery authority with policy, factor
// and credential writers. It is the authority every consumer reaches: the
// token must have been verified as epoch-bound and must name the epoch stored
// now, under the lock every epoch rotation takes. Strict equality, not
// credepoch.MatchEpoch, which admits any claim while the stored epoch is NULL.
func (h *Handler) ValidateRecoveryClaimsTx(ctx context.Context, tx *sql.Tx, claims *auth.RecoveryClaims) error {
	if tx == nil || claims == nil || !claims.EpochBound || !isRecoveryFactor(claims.RecoveryFactor) {
		return auth.ErrRecoveryProofInvalid
	}
	methods, credEpoch, err := lockRecoveryPolicyTx(ctx, tx, claims.UserID)
	if err != nil {
		return err
	}
	if !sameRecoveryEpoch(claims.CredentialEpoch, credEpoch) {
		return auth.ErrRecoveryProofInvalid
	}
	if len(methods) > 0 && !containsStr(methods, claims.RecoveryFactor) {
		return auth.ErrRecoveryProofInvalid
	}
	return nil
}

// sameRecoveryEpoch compares a token's epoch with the stored one. Both empty
// means a never-rotated account; any other difference is a stale token.
func sameRecoveryEpoch(claimed, stored string) bool {
	return subtle.ConstantTimeCompare([]byte(claimed), []byte(stored)) == 1
}

// GenerateRecoveryChallenge is called only after the single-use email proof.
// Its distinct purpose cannot authorize any reset, device or social operation.
func (h *Handler) GenerateRecoveryChallenge(_ context.Context, userID string) (string, string, error) {
	return GenerateChallengeToken(userID, PurposeRecoveryChallenge, JWTSecret(h.jwtSecret), "")
}

func recoveryFactor(method string) string {
	if method == "backup_code" {
		return "totp"
	}
	return method
}
