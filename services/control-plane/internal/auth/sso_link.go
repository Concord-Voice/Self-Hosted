package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const ssoLinkChallengeTTL = 5 * time.Minute

// SSOLinkChallenge contains only the public MFA offer. Provider identity stays
// server-side in a short-lived Redis record until the challenge is verified.
type SSOLinkChallenge struct {
	Token               string
	Methods             []string
	RecoveryOnlyMethods []string
	WebAuthnOptions     interface{}
}

type ssoLinkPending struct {
	UserID          string          `json:"user_id"`
	CredentialEpoch string          `json:"credential_epoch"`
	Identity        SSOIdentityLink `json:"identity"`
}

func ssoLinkPendingKey(jti string) string { return "mfa_sso_link:" + jti }

// PrepareSSOLink checks the password-authorized account's current policy and
// issues a signed MFA continuation only when login-eligible factors exist.
// A nil challenge means the caller may attempt the no-proof bound mint, whose
// locked policy read independently refuses an enrollment race.
func (h *Handler) PrepareSSOLink(ctx context.Context, userID, expectedEpoch string, identity SSOIdentityLink) (*SSOLinkChallenge, error) {
	if h.mfaChecker == nil {
		return nil, errors.New("mfa checker not wired")
	}
	var disabled, passwordDisabled bool
	var epoch sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT disabled, password_login_disabled, credential_epoch FROM users WHERE id = $1`, userID,
	).Scan(&disabled, &passwordDisabled, &epoch); err != nil {
		return nil, fmt.Errorf("read SSO link policy: %w", err)
	}
	if disabled {
		return nil, ErrAccountDisabled
	}
	if EffectivePasswordLoginDisabled(h.instanceType, passwordDisabled) {
		return nil, ErrSSOPasswordDisabled
	}
	if err := credepoch.MatchEpoch(epoch, expectedEpoch); err != nil {
		return nil, fmt.Errorf("SSO link credential epoch: %w", err)
	}
	methods, err := h.mfaChecker.GetLoginMethods(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read SSO link MFA methods: %w", err)
	}
	if len(methods) == 0 {
		return nil, nil
	}
	allMethods, err := h.mfaChecker.GetEnabledMethods(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read SSO link enabled methods: %w", err)
	}
	token, jti, err := h.mfaChecker.GenerateSSOLinkChallenge(ctx, userID, expectedEpoch)
	if err != nil {
		return nil, fmt.Errorf("generate SSO link challenge: %w", err)
	}
	clean := func() { h.discardLoginChallenge(ctx, userID, jti) }
	offered, options, err := h.webAuthnLoginOffer(ctx, userID, jti, methods)
	if err != nil {
		clean()
		return nil, fmt.Errorf("offer SSO link MFA: %w", err)
	}
	payload, err := json.Marshal(ssoLinkPending{UserID: userID, CredentialEpoch: expectedEpoch, Identity: identity})
	if err != nil {
		clean()
		return nil, fmt.Errorf("encode SSO link pending state: %w", err)
	}
	stored, err := h.redis.SetNX(ctx, ssoLinkPendingKey(jti), payload, ssoLinkChallengeTTL).Result()
	if err != nil {
		clean()
		return nil, fmt.Errorf("store SSO link pending state: %w", err)
	}
	if !stored {
		clean()
		return nil, errors.New("SSO link challenge JTI collision")
	}
	return &SSOLinkChallenge{Token: token, Methods: offered, RecoveryOnlyMethods: computeRecoveryOnlyMethods(allMethods, methods), WebAuthnOptions: options}, nil
}

// CompleteSSOLink is called only by MFA.Verify after factor proof and the
// one-use challenge claim. It spends the pending identity before policy reads;
// any failure makes the account-link flow restart from password proof.
func (h *Handler) CompleteSSOLink(c *gin.Context, userID, jti, expectedEpoch, verifiedMethod string) bool {
	if userID == "" || jti == "" || verifiedMethod == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_link_invalid"})
		return false
	}
	ctx := c.Request.Context()
	claimed, err := h.redis.Exists(ctx, "mfa_challenge_used:"+jti).Result()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error_code": "sso_link_unavailable"})
		return false
	}
	if claimed != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_link_invalid"})
		return false
	}
	data, err := h.redis.GetDel(ctx, ssoLinkPendingKey(jti)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_link_invalid"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error_code": "sso_link_unavailable"})
		}
		return false
	}
	var pending ssoLinkPending
	if err := json.Unmarshal(data, &pending); err != nil || pending.UserID == "" || pending.Identity.Provider == "" || pending.Identity.ProviderUserID == "" ||
		subtle.ConstantTimeCompare([]byte(pending.UserID), []byte(userID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pending.CredentialEpoch), []byte(expectedEpoch)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_link_invalid"})
		return false
	}
	access, refresh, sessionID, err := h.issueAccessAndRefresh(ctx, userID, &expectedEpoch, &pending.Identity, &ssoLinkProof{verifiedMethod: verifiedMethod})
	if err != nil {
		switch {
		case errors.Is(err, ErrAccountDisabled):
			c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		case errors.Is(err, ErrSSOPasswordDisabled):
			c.JSON(http.StatusForbidden, gin.H{"error_code": "account_uses_sso"})
		case errors.Is(err, ErrSSOLinkProofInvalid), errors.Is(err, credepoch.ErrEpochMismatch), errors.Is(err, ErrMFARequired):
			c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_link_invalid"})
		case errors.Is(err, loginfamily.ErrUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error_code": "sso_link_unavailable"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "token_issuance_failed"})
		}
		return false
	}
	SetRefreshCookie(c, refresh, 60*60*24*30)
	c.Header(middleware.SessionIDHeader, sessionID)
	c.JSON(http.StatusOK, gin.H{"access_token": access, "refresh_token": refresh, "session_id": sessionID, "remember_me": true})
	return true
}
