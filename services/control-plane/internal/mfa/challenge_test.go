package mfa

import (
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test-secret-for-mfa-challenges" // #nosec G101

func TestChallengeTokenPreservesAuthorityClaims(t *testing.T) {
	want := ChallengeClaims{
		UserID: "user-123", Purpose: PurposeLogin, CredentialEpoch: "epoch-abc",
		PrimaryAuthMethod: securityevent.AuthMethod("password"), RefreshSessionID: "session-123", RecoveryFactor: "totp",
	}
	token, jti, err := generateChallengeTokenWithTTL(want, testJWTSecret, challengeTTL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ValidateChallengeToken(token, testJWTSecret, want.Purpose)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != want.UserID || got.Purpose != want.Purpose || got.CredentialEpoch != want.CredentialEpoch ||
		got.PrimaryAuthMethod != want.PrimaryAuthMethod || got.RefreshSessionID != want.RefreshSessionID || got.RecoveryFactor != want.RecoveryFactor {
		t.Fatal("token lost authority claims")
	}
	if got.ID != jti || got.Issuer != ChallengeTokenIssuer || time.Until(got.ExpiresAt.Time) > challengeTTL || time.Until(got.ExpiresAt.Time) < challengeTTL-time.Minute {
		t.Fatal("token has invalid registered claims")
	}
}

func TestGenerateChallengeToken(t *testing.T) {
	token, jti, err := GenerateChallengeToken("user-123", PurposeMFAUpgrade, testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	if token == "" {
		t.Error("token is empty")
	}
	if jti == "" {
		t.Error("jti is empty")
	}
}

func TestGenerateChallengeTokenRejectsLogin(t *testing.T) {
	token, jti, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret, "epoch-abc")
	if err == nil {
		t.Fatal("expected login challenge generation to fail")
	}
	if token != "" || jti != "" {
		t.Fatalf("rejected login challenge returned token material: token=%q jti=%q", token, jti)
	}
}

func TestValidateChallengeToken(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeMFAUpgrade, testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeMFAUpgrade)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed: %v", err)
	}

	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
	}
	if claims.Purpose != PurposeMFAUpgrade {
		t.Errorf("Purpose = %q, want %q", claims.Purpose, PurposeMFAUpgrade)
	}
	if claims.Issuer != "concordvoice-mfa" {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, "concordvoice-mfa")
	}
}

func TestValidateChallengeTokenWrongPurpose(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeMFAUpgrade, testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeSuspiciousRefresh)
	if err == nil {
		t.Error("expected error for wrong purpose")
	}
}

func TestValidateChallengeTokenWrongSecret(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeMFAUpgrade, testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, "wrong-secret", PurposeMFAUpgrade)
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

func TestGenerateRecoveryToken(t *testing.T) {
	token, jti, err := GenerateRecoveryToken("user-456", "email", testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateRecoveryToken failed: %v", err)
	}
	if token == "" {
		t.Error("token is empty")
	}
	if jti == "" {
		t.Error("jti is empty")
	}

	// Validate with correct purpose
	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeRecovery)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed for recovery token: %v", err)
	}
	if claims.UserID != "user-456" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-456")
	}
	if claims.Purpose != PurposeRecovery {
		t.Errorf("Purpose = %q, want %q", claims.Purpose, PurposeRecovery)
	}

	// Verify ~15 minute TTL (ADR-0046 decision 7).
	expiresIn := time.Until(claims.ExpiresAt.Time)
	if expiresIn < 14*time.Minute+50*time.Second || expiresIn > 15*time.Minute+10*time.Second {
		t.Errorf("recovery token TTL = %v, want ~15 minutes", expiresIn)
	}
}

func TestRecoveryTokenRejectsRotatedSigningSecret(t *testing.T) {
	token, _, err := GenerateRecoveryToken("user-456", "email", testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateRecoveryToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, "rotated-test-secret", PurposeRecovery)
	if err == nil {
		t.Error("expected a recovery token signed before restore to be rejected after signing-key rotation")
	}
}

func TestRecoveryTokenRejectsWrongPurpose(t *testing.T) {
	token, _, err := GenerateRecoveryToken("user-456", "email", testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateRecoveryToken failed: %v", err)
	}

	// Login purpose should reject a recovery token
	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeLogin)
	if err == nil {
		t.Error("expected error when validating recovery token with login purpose")
	}

	// MFA upgrade purpose should also reject
	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeMFAUpgrade)
	if err == nil {
		t.Error("expected error when validating recovery token with mfa_upgrade purpose")
	}
}

func TestValidateChallengeTokenExpired(t *testing.T) {
	// Create a token that's already expired
	claims := ChallengeClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "test-jti",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-10 * time.Minute)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-5 * time.Minute)),
			NotBefore: jwt.NewNumericDate(time.Now().Add(-10 * time.Minute)),
			Issuer:    "concordvoice-mfa",
		},
		UserID:  "user-123",
		Purpose: PurposeLogin,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, err = ValidateChallengeToken(signed, testJWTSecret, PurposeLogin)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestGenerateChallengeTokenStampsCredentialEpoch(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeSuspiciousRefresh, testJWTSecret, "epoch-abc")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeSuspiciousRefresh)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed: %v", err)
	}
	if claims.CredentialEpoch != "epoch-abc" {
		t.Errorf("CredentialEpoch = %q, want %q", claims.CredentialEpoch, "epoch-abc")
	}
}

func TestGenerateChallengeTokenEmptyEpochOmitsClaim(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeMFAUpgrade, testJWTSecret, "")
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeMFAUpgrade)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed: %v", err)
	}
	if claims.CredentialEpoch != "" {
		t.Errorf("CredentialEpoch = %q, want empty: an unstamped challenge must carry no epoch", claims.CredentialEpoch)
	}
}

func TestGenerateRecoveryTokenStampsEpochPurposeAndTTL(t *testing.T) {
	token, jti, err := GenerateRecoveryToken("user-456", "email", testJWTSecret, CredEpoch("e1"))
	require.NoError(t, err)
	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeRecovery)
	require.NoError(t, err)
	require.Equal(t, ChallengePurpose("recovery_v3"), claims.Purpose)
	require.Equal(t, "e1", claims.CredentialEpoch)
	require.Equal(t, "email", claims.RecoveryFactor)
	require.Equal(t, jti, claims.ID)
	require.Equal(t, auth.RecoveryTokenTTL, claims.ExpiresAt.Sub(claims.IssuedAt.Time))
	require.Equal(t, 15*time.Minute, auth.RecoveryTokenTTL)

	// A never-rotated account mints with an empty epoch; the purpose version,
	// not the claim, marks the token as epoch-bound.
	token, _, err = GenerateRecoveryToken("user-456", "totp", testJWTSecret, "")
	require.NoError(t, err)
	claims, err = ValidateChallengeToken(token, testJWTSecret, PurposeRecovery)
	require.NoError(t, err)
	require.Empty(t, claims.CredentialEpoch)
	require.Equal(t, "totp", claims.RecoveryFactor)
}

func TestGenerateRecoveryTokenRejectsUnknownFactor(t *testing.T) {
	for _, factor := range []string{"unverified", "", "sms", "backup_code"} {
		token, jti, err := GenerateRecoveryToken("user-456", factor, testJWTSecret, "e1")
		require.Error(t, err, factor)
		require.Empty(t, token)
		require.Empty(t, jti)
	}
}

func TestGenerateChallengeTokenRefusesRecoveryPurpose(t *testing.T) {
	token, jti, err := GenerateChallengeToken("user-456", PurposeRecovery, testJWTSecret, "")
	require.Error(t, err)
	require.Empty(t, token)
	require.Empty(t, jti)
}

// TestLegacyRecoveryPurposeIsRejected proves a token minted before the epoch
// binding (purpose recovery_v2, no epoch) fails purpose validation everywhere.
func TestLegacyRecoveryPurposeIsRejected(t *testing.T) {
	legacy, _, err := generateChallengeTokenWithTTL(ChallengeClaims{
		UserID: "user-456", Purpose: "recovery_v2", RecoveryFactor: "email",
	}, testJWTSecret, time.Hour)
	require.NoError(t, err)
	_, err = ValidateChallengeToken(legacy, testJWTSecret, PurposeRecovery)
	require.ErrorContains(t, err, "purpose mismatch")
}
