package mfa

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testJWTSecret = "test-secret-for-mfa-challenges" // #nosec G101

func TestGenerateChallengeToken(t *testing.T) {
	token, jti, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
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

func TestValidateChallengeToken(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	claims, err := ValidateChallengeToken(token, testJWTSecret, PurposeLogin)
	if err != nil {
		t.Fatalf("ValidateChallengeToken failed: %v", err)
	}

	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
	}
	if claims.Purpose != PurposeLogin {
		t.Errorf("Purpose = %q, want %q", claims.Purpose, PurposeLogin)
	}
	if claims.Issuer != "concordvoice-mfa" {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, "concordvoice-mfa")
	}
}

func TestValidateChallengeTokenWrongPurpose(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, testJWTSecret, PurposeSuspiciousRefresh)
	if err == nil {
		t.Error("expected error for wrong purpose")
	}
}

func TestValidateChallengeTokenWrongSecret(t *testing.T) {
	token, _, err := GenerateChallengeToken("user-123", PurposeLogin, testJWTSecret)
	if err != nil {
		t.Fatalf("GenerateChallengeToken failed: %v", err)
	}

	_, err = ValidateChallengeToken(token, "wrong-secret", PurposeLogin)
	if err == nil {
		t.Error("expected error for wrong secret")
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
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, err = ValidateChallengeToken(signed, testJWTSecret, PurposeLogin)
	if err == nil {
		t.Error("expected error for expired token")
	}
}
