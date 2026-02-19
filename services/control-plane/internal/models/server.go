// Package models contains data models for the Concord platform.
package models

import "time"

// Server represents a Concord server (community-style community)
type Server struct {
	ID        string    `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	IconURL   *string   `json:"icon_url,omitempty" db:"icon_url"`
	OwnerID   string    `json:"owner_id" db:"owner_id"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// ServerMember represents a user's membership in a server
type ServerMember struct {
	ServerID string    `json:"server_id" db:"server_id"`
	UserID   string    `json:"user_id" db:"user_id"`
	Role     string    `json:"role" db:"role"` // owner, admin, member
	JoinedAt time.Time `json:"joined_at" db:"joined_at"`
}

// ServerWithRole combines server info with user's role
type ServerWithRole struct {
	Server
	Role string `json:"role" db:"role"`
}

// Channel represents a voice or text channel within a server
type Channel struct {
	ID          string    `json:"id" db:"id"`
	ServerID    string    `json:"server_id" db:"server_id"`
	Name        string    `json:"name" db:"name"`
	Description *string   `json:"description,omitempty" db:"description"`
	Type        string    `json:"type" db:"type"` // voice, text, bulletin
	Emoji       *string   `json:"emoji,omitempty" db:"emoji"` // Optional custom emoji
	Position    int       `json:"position" db:"position"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// Message represents a chat message in a channel
type Message struct {
	ID          string     `json:"id" db:"id"`
	ChannelID   string     `json:"channel_id" db:"channel_id"`
	UserID      string     `json:"user_id" db:"user_id"`
	Content     string     `json:"content" db:"content"`
	IsEncrypted bool       `json:"is_encrypted" db:"is_encrypted"`
	EditedAt    *time.Time `json:"edited_at,omitempty" db:"edited_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

// MessageWithUser combines message with user details
type MessageWithUser struct {
	Message
	Username    string  `json:"username" db:"username"`
	DisplayName *string `json:"display_name,omitempty" db:"display_name"`
	AvatarURL   *string `json:"avatar_url,omitempty" db:"avatar_url"`
}

// ServerInvite represents an invite code for joining a server
type ServerInvite struct {
	ID        string     `json:"id" db:"id"`
	ServerID  string     `json:"server_id" db:"server_id"`
	Code      string     `json:"code" db:"code"`
	CreatedBy string     `json:"created_by" db:"created_by"`
	MaxUses   *int       `json:"max_uses" db:"max_uses"`
	UseCount  int        `json:"use_count" db:"use_count"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	IsRevoked bool       `json:"is_revoked" db:"is_revoked"`
	CreatedAt time.Time  `json:"created_at" db:"created_at"`
}

// ServerInviteWithCreator combines invite info with creator's username
type ServerInviteWithCreator struct {
	ServerInvite
	CreatorUsername string `json:"creator_username" db:"creator_username"`
}
