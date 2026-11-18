package mfa

import (
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const challengeTTL = 5 * time.Minute

// ChallengePurpose identifies what the MFA challenge is for.
type ChallengePurpose string

// CompletionKind selects the post-MFA login continuation from signed claims.
type CompletionKind string

const (
	// CompletionKindLogin is the legacy ordinary login continuation.
	CompletionKindLogin CompletionKind = "login"
	// CompletionKindSSOLink requires the pending SSO identity and verified MFA.
	CompletionKindSSOLink CompletionKind = "sso_link"
)

// MFA challenge purposes.
const (
	PurposeLogin             ChallengePurpose = "login"
	PurposeSuspiciousRefresh ChallengePurpose = "suspicious_refresh"
	PurposeMFAUpgrade        ChallengePurpose = "mfa_upgrade"
	// Versioned so older recovery tokens cannot authorize anything: v2 added the
	// recovery policy, and v3 marks a token whose mint stamped the credential
	// epoch it was issued against. An empty v3 epoch means "never rotated", which
	// a v2 token could not distinguish from "minted by code that stamps nothing".
	PurposeRecovery          ChallengePurpose = "recovery_v3"
	PurposeRecoveryChallenge ChallengePurpose = "recovery_challenge"
)

// ChallengeTokenIssuer is the `iss` claim on every MFA-family token. Named for the
// same reason middleware.AccessTokenIssuer is: this value is the reciprocal half of
// the #2899 token-class boundary — it is what stops an ACCESS token being redeemed
// as a challenge — and it was previously a bare literal at both the mint and the
// read, one careless edit from letting the two disagree.
const ChallengeTokenIssuer = "concordvoice-mfa"

// ChallengeClaims are the JWT claims for an MFA challenge token.
type ChallengeClaims struct {
	jwt.RegisteredClaims
	UserID  string           `json:"user_id"`
	Purpose ChallengePurpose `json:"purpose"`
	// Empty is the legacy login continuation. A client cannot choose this in MFA verify.
	CompletionKind CompletionKind `json:"completion_kind,omitempty"`
	// RecoveryFactor is the server-verified factor authorizing final recovery.
	RecoveryFactor string `json:"recovery_factor,omitempty"`
	// CredentialEpoch is the durable users.credential_epoch in effect when this
	// challenge was issued (#2418). Redemption compares it against the epoch current
	// at mint time, so a challenge issued before a destructive reset cannot complete
	// into a post-reset session. Empty on challenges that never reach a mint
	// (MFA upgrade) and on pre-#2418 tokens; auth.CompleteLogin rejects an
	// empty claim for a user who has rotated. A recovery_v3 token always carries
	// the epoch it was issued against, empty only when the account never rotated,
	// and every recovery consumer compares it for strict equality.
	//
	// Deliberately NOT validated in a Claims.Validate() method: golang-jwt v5
	// auto-invokes that during ParseWithClaims, where the current durable epoch is
	// not available. Enforcement belongs at the redemption site.
	CredentialEpoch string `json:"cred_epoch,omitempty"`
	// PrimaryAuthMethod records the server-authenticated method that reached the
	// MFA gate. It is signed with the challenge and must never be derived from a
	// client-controlled MFA verification request.
	PrimaryAuthMethod securityevent.AuthMethod `json:"primary_auth_method,omitempty"`
	// RefreshSessionID binds an MFA-upgrade challenge to the exact pre-MFA
	// refresh session that requested it. Other challenge purposes leave it empty.
	RefreshSessionID string `json:"refresh_session_id,omitempty"`
}

// CredEpoch is a user's durable credential epoch as carried into a challenge token.
//
// #2450: this is a distinct type purely to make a parameter transposition a
// COMPILE error. jwtSecret and credEpoch were adjacent plain strings, so swapping
// them compiled cleanly — and would have written the HMAC signing secret into the
// client-visible `cred_epoch` claim while signing with an epoch string. Blast
// radius is total signing-key disclosure, and no test would obviously fail. Do not
// collapse this back to a bare string.
type CredEpoch string

// JWTSecret is the HMAC signing key for challenge tokens.
//
// #2450 follow-up: `CredEpoch` alone left the hazard half-closed. `GenerateRecoveryToken`
// took `(userID, jwtSecret string)` — two adjacent plain strings, two lines below the type
// added to prevent exactly that. Transposing them compiled cleanly and would sign the
// RECOVERY token with the user's UUID as the HMAC key — a value the attacker already knows,
// yielding forgeable recovery tokens and full account takeover via RecoveryResetAccount —
// while writing the real signing secret into the client-visible `sub` claim. Typing the
// secret closes the whole class rather than one instance. Do not collapse either type back
// to a bare string.
type JWTSecret string

// GenerateChallengeToken creates a short-lived, purpose-bound JWT for MFA challenges.
// credEpoch is the issuing user's durable credential epoch, or "" for purposes that
// never mint a session.
func GenerateChallengeToken(userID string, purpose ChallengePurpose, jwtSecret JWTSecret, credEpoch CredEpoch) (string, string, error) {
	if purpose == PurposeLogin {
		return "", "", fmt.Errorf("login challenges require Handler.GenerateLoginChallenge")
	}
	if purpose == PurposeRecovery {
		return "", "", fmt.Errorf("recovery tokens require GenerateRecoveryToken")
	}
	return generateChallengeTokenWithTTL(ChallengeClaims{UserID: userID, Purpose: purpose, CredentialEpoch: string(credEpoch)}, jwtSecret, challengeTTL)
}

// GenerateRecoveryToken signs final recovery authority for a server-verified
// factor, valid for auth.RecoveryTokenTTL. The token names the credential epoch
// it was issued against, and the reset it authorizes rotates that epoch, which
// revokes it and its siblings. credEpoch must be read under the users row lock
// that also evaluated recovery policy (Handler.IssueRecoveryTokenTx).
func GenerateRecoveryToken(userID, verifiedFactor string, jwtSecret JWTSecret, credEpoch CredEpoch) (string, string, error) {
	if !isRecoveryFactor(verifiedFactor) {
		return "", "", fmt.Errorf("invalid recovery proof")
	}
	return generateChallengeTokenWithTTL(ChallengeClaims{
		UserID: userID, Purpose: PurposeRecovery, RecoveryFactor: verifiedFactor, CredentialEpoch: string(credEpoch),
	}, jwtSecret, auth.RecoveryTokenTTL)
}

// isRecoveryFactor reports whether factor is one a recovery verifier proves.
func isRecoveryFactor(factor string) bool {
	return factor == "email" || factor == "totp" || factor == "webauthn"
}

// generateChallengeTokenWithTTL creates a purpose-bound JWT with a configurable TTL.
func generateChallengeTokenWithTTL(claims ChallengeClaims, jwtSecret JWTSecret, ttl time.Duration) (string, string, error) {
	jti := uuid.New().String()
	now := time.Now()

	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        jti,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		NotBefore: jwt.NewNumericDate(now),
		Issuer:    ChallengeTokenIssuer,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		return "", "", fmt.Errorf("sign challenge token: %w", err)
	}

	return signed, jti, nil
}

// ValidateChallengeToken validates an MFA challenge token and checks its purpose.
func ValidateChallengeToken(tokenString, jwtSecret string, expectedPurpose ChallengePurpose) (*ChallengeClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &ChallengeClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(jwtSecret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse challenge token: %w", err)
	}

	claims, ok := token.Claims.(*ChallengeClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid challenge token claims")
	}

	if claims.Issuer != ChallengeTokenIssuer {
		return nil, fmt.Errorf("invalid issuer: %s", claims.Issuer)
	}

	if claims.Purpose != expectedPurpose {
		return nil, fmt.Errorf("purpose mismatch: got %s, want %s", claims.Purpose, expectedPurpose)
	}
	if claims.CompletionKind != "" && (claims.Purpose != PurposeLogin ||
		(claims.CompletionKind != CompletionKindLogin && claims.CompletionKind != CompletionKindSSOLink)) {
		return nil, fmt.Errorf("invalid challenge completion kind")
	}

	return claims, nil
}
