// Package messagecreate defines dormant, transport-neutral primitives for a
// future message-v1 create path. It does not perform message persistence or
// reservation.
package messagecreate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/klipy"
)

const (
	// ScopeChannel identifies a channel message create request.
	ScopeChannel Scope = "channel"
	// ScopeDM identifies a direct-message conversation create request.
	ScopeDM Scope = "dm"

	maxAttachments    = 5
	maxMentionedUsers = 50
	maxMentionedRoles = 10
)

const maxSafeInteger uint64 = 9_007_199_254_740_991

var (
	// ErrInvalidInput reports an input that cannot have a single canonical
	// message-v1 create identity.
	ErrInvalidInput = errors.New("invalid message-v1 create fingerprint input")
)

// Scope is the authenticated route family a future writer will use.
type Scope string

// MentionIntent is the requested mention intent after transport decoding but
// before membership, permission, or notification resolution. The codec binds
// this intent so a retry remains the same request when those external facts
// change.
type MentionIntent struct {
	UserIDs  []string `json:"user_ids"`
	RoleIDs  []string `json:"role_ids"`
	Everyone bool     `json:"everyone"`
	Here     bool     `json:"here"`
}

// FingerprintInput is the complete future message-v1 create identity. A future
// writer must derive SenderID from authenticated identity and must not accept a
// caller-provided fingerprint.
type FingerprintInput struct {
	Scope         Scope         `json:"scope"`
	ScopeID       string        `json:"scope_id"`
	MessageID     string        `json:"message_id"`
	SenderID      string        `json:"sender_id"`
	KeyEpoch      uint64        `json:"key_epoch"`
	Envelope      string        `json:"envelope"`
	ReplyToID     string        `json:"reply_to_id"`
	GIFSlug       *string       `json:"gif_slug"`
	AttachmentIDs []string      `json:"attachment_ids"`
	Mentions      MentionIntent `json:"mentions"`
}

// FingerprintV1 returns SHA-256 of the exact canonical UTF-8 positional JSON
// create identity. It is intentionally pure: it neither reserves an ID nor
// writes a message or any side effect.
func FingerprintV1(in FingerprintInput) ([sha256.Size]byte, error) {
	preimage, err := canonicalPreimageV1(in)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(preimage), nil
}

func canonicalPreimageV1(in FingerprintInput) ([]byte, error) {
	if in.Scope != ScopeChannel && in.Scope != ScopeDM {
		return nil, invalid("scope")
	}
	scopeID, err := canonicalUUID(in.ScopeID)
	if err != nil {
		return nil, invalid("scope ID")
	}
	messageID, err := canonicalUUID(in.MessageID)
	if err != nil {
		return nil, invalid("message ID")
	}
	senderID, err := canonicalUUID(in.SenderID)
	if err != nil {
		return nil, invalid("sender ID")
	}
	if in.KeyEpoch == 0 || in.KeyEpoch > maxSafeInteger {
		return nil, invalid("key epoch")
	}
	if err := validateEnvelope(in.Envelope); err != nil {
		return nil, err
	}

	replyToID := ""
	if in.ReplyToID != "" {
		replyToID, err = canonicalUUID(in.ReplyToID)
		if err != nil {
			return nil, invalid("reply ID")
		}
	}

	gifSlug := ""
	if normalized := klipy.NormalizeSlug(in.GIFSlug); normalized != nil {
		if !klipy.ValidateSlug(normalized) {
			return nil, invalid("GIF slug")
		}
		gifSlug = *normalized
	}

	attachments, err := canonicalOrderedUUIDs(in.AttachmentIDs, maxAttachments, "attachment IDs")
	if err != nil {
		return nil, err
	}
	users, err := canonicalSortedUUIDs(in.Mentions.UserIDs, maxMentionedUsers, "mentioned user IDs")
	if err != nil {
		return nil, err
	}
	roles, err := canonicalSortedUUIDs(in.Mentions.RoleIDs, maxMentionedRoles, "mentioned role IDs")
	if err != nil {
		return nil, err
	}
	if in.Scope == ScopeDM && (in.Mentions.Everyone || len(roles) != 0) {
		return nil, invalid("DM mention intent")
	}

	preimage := []any{
		"concord-message-create",
		1,
		"message-v1",
		string(in.Scope),
		scopeID,
		messageID,
		senderID,
		in.KeyEpoch,
		"message-body",
		in.Envelope,
		replyToID,
		gifSlug,
		attachments,
		[]any{users, roles, in.Mentions.Everyone, in.Mentions.Here},
	}
	encoded, err := json.Marshal(preimage)
	if err != nil {
		return nil, fmt.Errorf("marshal message-v1 fingerprint preimage: %w", err)
	}
	return encoded, nil
}

func canonicalOrderedUUIDs(values []string, maximum int, field string) ([]string, error) {
	if len(values) > maximum {
		return nil, invalid(field)
	}
	seen := make(map[string]struct{}, len(values))
	canonical := make([]string, len(values))
	for i, value := range values {
		parsed, err := canonicalUUID(value)
		if err != nil {
			return nil, invalid(field)
		}
		if _, exists := seen[parsed]; exists {
			return nil, invalid(field)
		}
		seen[parsed] = struct{}{}
		canonical[i] = parsed
	}
	return canonical, nil
}

func canonicalSortedUUIDs(values []string, maximum int, field string) ([]string, error) {
	canonical, err := canonicalOrderedUUIDs(values, maximum, field)
	if err != nil {
		return nil, err
	}
	sort.Strings(canonical)
	return canonical, nil
}

func canonicalUUID(value string) (string, error) {
	if len(value) != 36 || strings.ToLower(value) != value {
		return "", ErrInvalidInput
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return "", ErrInvalidInput
			}
			continue
		}
		if !isLowerHex(value[i]) {
			return "", ErrInvalidInput
		}
	}
	if value == "00000000-0000-0000-0000-000000000000" || value[14] < '1' || value[14] > '8' ||
		(value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return "", ErrInvalidInput
	}
	return value, nil
}

func isLowerHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')
}

func validateEnvelope(envelope string) error {
	if !strings.HasPrefix(envelope, "m1:") {
		return invalid("message envelope")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope[3:])
	if err != nil || len(payload) < 28 || base64.StdEncoding.EncodeToString(payload) != envelope[3:] {
		return invalid("message envelope")
	}
	return nil
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, field)
}
