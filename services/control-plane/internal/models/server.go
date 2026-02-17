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
	ID        string    `json:"id" db:"id"`
	ServerID  string    `json:"server_id" db:"server_id"`
	Name      string    `json:"name" db:"name"`
	Type      string    `json:"type" db:"type"` // voice, text, announcement
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}
