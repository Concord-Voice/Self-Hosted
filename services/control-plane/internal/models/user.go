package models

import (
	"encoding/json"
	"time"
)

// User represents a Concord user
type User struct {
	ID             string          `json:"id" db:"id"`
	Email          string          `json:"email" db:"email"`
	Username       string          `json:"username" db:"username"`
	PasswordHash   string          `json:"-" db:"password_hash"` // Never send in JSON
	DisplayName    *string         `json:"display_name,omitempty" db:"display_name"`
	Bio            *string         `json:"bio,omitempty" db:"bio"`
	AvatarURL      *string         `json:"avatar_url,omitempty" db:"avatar_url"`
	Links          json.RawMessage `json:"links" db:"links"`
	EmailVerified  bool            `json:"email_verified" db:"email_verified"`
	AgeVerified    bool            `json:"age_verified" db:"age_verified"`
	E2EEPreference bool            `json:"e2ee_preference" db:"e2ee_preference"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at" db:"updated_at"`
}

// PublicUser returns user data safe for the authenticated user
func (u *User) PublicUser() map[string]interface{} {
	result := map[string]interface{}{
		"id":              u.ID,
		"email":           u.Email,
		"username":        u.Username,
		"e2ee_preference": u.E2EEPreference,
		"created_at":      u.CreatedAt,
		"links":           json.RawMessage("[]"),
	}
	if u.DisplayName != nil {
		result["display_name"] = *u.DisplayName
	}
	if u.Bio != nil {
		result["bio"] = *u.Bio
	}
	if u.AvatarURL != nil {
		result["avatar_url"] = *u.AvatarURL
	}
	if len(u.Links) > 0 {
		result["links"] = u.Links
	}
	return result
}

// UserKeys represents E2EE keys for a user
type UserKeys struct {
	UserID            string    `json:"user_id" db:"user_id"`
	WrappedPrivateKey []byte    `json:"-" db:"wrapped_private_key"` // Never send directly
	KeyDerivationSalt []byte    `json:"-" db:"key_derivation_salt"` // Never send directly
	KeyVersion        int       `json:"key_version" db:"key_version"`
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" db:"updated_at"`
}

// RefreshToken represents a session refresh token
type RefreshToken struct {
	ID         string     `json:"id" db:"id"`
	UserID     string     `json:"user_id" db:"user_id"`
	TokenHash  string     `json:"-" db:"token_hash"` // Never send
	DeviceName string     `json:"device_name" db:"device_name"`
	IPAddress  string     `json:"ip_address" db:"ip_address"`
	UserAgent  string     `json:"user_agent" db:"user_agent"`
	ExpiresAt  time.Time  `json:"expires_at" db:"expires_at"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	LastUsedAt time.Time  `json:"last_used_at" db:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty" db:"revoked_at"`
	RememberMe bool       `json:"remember_me" db:"remember_me"`
}

// Session returns a public-safe session representation
func (rt *RefreshToken) Session() map[string]interface{} {
	return map[string]interface{}{
		"id":          rt.ID,
		"device_name": rt.DeviceName,
		"ip_address":  rt.IPAddress,
		"created_at":  rt.CreatedAt,
		"last_used":   rt.LastUsedAt,
		"expires_at":  rt.ExpiresAt,
	}
}
