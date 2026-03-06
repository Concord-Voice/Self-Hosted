package mfa

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const challengeTTL = 5 * time.Minute

// ChallengePurpose identifies what the MFA challenge is for.
type ChallengePurpose string

const (
	PurposeLogin             ChallengePurpose = "login"
	PurposeSuspiciousRefresh ChallengePurpose = "suspicious_refresh"
	PurposeMFAUpgrade        ChallengePurpose = "mfa_upgrade"
)

// MFAChallengeClaims are the JWT claims for an MFA challenge token.
type MFAChallengeClaims struct {
	jwt.RegisteredClaims
	UserID  string           `json:"user_id"`
	Purpose ChallengePurpose `json:"purpose"`
}

// GenerateChallengeToken creates a short-lived, purpose-bound JWT for MFA challenges.
func GenerateChallengeToken(userID string, purpose ChallengePurpose, jwtSecret string) (string, string, error) {
	jti := uuid.New().String()
	now := time.Now()

	claims := MFAChallengeClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(challengeTTL)),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    "concordvoice-mfa",
		},
		UserID:  userID,
		Purpose: purpose,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		return "", "", fmt.Errorf("sign challenge token: %w", err)
	}

	return signed, jti, nil
}

// ValidateChallengeToken validates an MFA challenge token and checks its purpose.
func ValidateChallengeToken(tokenString, jwtSecret string, expectedPurpose ChallengePurpose) (*MFAChallengeClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &MFAChallengeClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(jwtSecret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse challenge token: %w", err)
	}

	claims, ok := token.Claims.(*MFAChallengeClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid challenge token claims")
	}

	if claims.Issuer != "concordvoice-mfa" {
		return nil, fmt.Errorf("invalid issuer: %s", claims.Issuer)
	}

	if claims.Purpose != expectedPurpose {
		return nil, fmt.Errorf("purpose mismatch: got %s, want %s", claims.Purpose, expectedPurpose)
	}

	return claims, nil
}
