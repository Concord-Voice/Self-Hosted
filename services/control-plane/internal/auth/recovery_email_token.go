package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// recoveryFormat is what a recovery email carries: a recovery key (token) or,
// for clients that cannot accept one, the legacy 6-digit code.
type recoveryFormat string

const (
	recoveryFormatCode  recoveryFormat = "code"
	recoveryFormatToken recoveryFormat = "token"
)

const (
	// recoveryEmailTokenBytes is the entropy of a recovery key: 256 bits, so
	// guessing one is infeasible and verification needs no attempt counter.
	recoveryEmailTokenBytes = 32
	// recoveryEmailTokenLen is the base64url (no padding) length of a key.
	recoveryEmailTokenLen = 43

	// The resend cooldown and the lifetime of the secret it guards are one
	// value, so the newest email always holds a usable secret.
	recoveryFastTierTTL   = 15 * time.Minute
	recoverySlowTierTTL   = 60 * time.Minute
	recoveryFastTierSends = 3
	recoverySendsWindow   = 24 * time.Hour

	recoveryTokenKeyPrefix = "recovery_code:token:"
	recoverySlotKeyPrefix  = "recovery_code:slot:"
	recoverySendsKeyPrefix = "recovery_code:sends:"
)

// recoveryMailer is the part of the email service recovery uses.
type recoveryMailer interface {
	SendRecoveryCode(to, code string, minutes int) error
	SendRecoveryKey(to, key string, minutes int) error
}

// generateRecoveryEmailToken returns a new 43-character recovery key.
func generateRecoveryEmailToken() (string, error) {
	var b [recoveryEmailTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// validRecoveryEmailToken reports whether s has the shape of a recovery key.
// It says nothing about whether the key was ever issued.
func validRecoveryEmailToken(s string) bool {
	if len(s) != recoveryEmailTokenLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == recoveryEmailTokenBytes
}

// recoveryEmailTokenKey is the Redis key of a key's record. Only the hash of
// the key is stored.
func recoveryEmailTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return recoveryTokenKeyPrefix + hex.EncodeToString(sum[:])
}

// recoverySlotKey holds the send_id of the account's live recovery email; while
// it exists no new recovery email is sent.
func recoverySlotKey(userID string) string { return recoverySlotKeyPrefix + userID }

// recoverySendsKey counts the account's recovery emails in the current window.
func recoverySendsKey(userID string) string { return recoverySendsKeyPrefix + userID }

// validRecoveryEmail is the request-only shape check that runs before any Redis
// read keyed by a request-supplied address. Requiring "@" and refusing ":"
// keeps a crafted value such as "slot:..." from naming another kind of key.
// Registration already refuses an unquoted ":" in an address.
func validRecoveryEmail(addr string) bool {
	return strings.Contains(addr, "@") && !strings.Contains(addr, ":")
}

// recoveryIssue names the keys one recovery email owns.
type recoveryIssue struct {
	RecordKey string
	SlotKey   string
	SendsKey  string
	SendID    string
	Record    []byte
}

// issueRecoveryRecordLua takes the account's send slot, counts the send, picks
// the tier, and stores the record with the slot's TTL, all in one command.
// KEYS: slot, sends, record. ARGV: send_id, record, fast ms, slow ms, fast
// sends, window ms. Returns 0 when the slot is held, else the TTL in ms.
var issueRecoveryRecordLua = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then return 0 end
local n = redis.call("INCR", KEYS[2])
if redis.call("PTTL", KEYS[2]) < 0 then redis.call("PEXPIRE", KEYS[2], ARGV[6]) end
local ttl = ARGV[3]
if n > tonumber(ARGV[5]) then ttl = ARGV[4] end
redis.call("SET", KEYS[1], ARGV[1], "PX", ttl)
redis.call("SET", KEYS[3], ARGV[2], "PX", ttl)
return tonumber(ttl)
`)

// issueRecoveryRecord stores a recovery record unless the account already has
// a live recovery email. held is true when nothing was stored.
func (h *Handler) issueRecoveryRecord(ctx context.Context, in recoveryIssue) (ttl time.Duration, held bool, err error) {
	ms, err := issueRecoveryRecordLua.Run(ctx, h.redis,
		[]string{in.SlotKey, in.SendsKey, in.RecordKey},
		in.SendID, string(in.Record),
		recoveryFastTierTTL.Milliseconds(), recoverySlowTierTTL.Milliseconds(),
		recoveryFastTierSends, recoverySendsWindow.Milliseconds(),
	).Int64()
	if err != nil {
		return 0, false, fmt.Errorf("issue recovery record: %w", err)
	}
	if ms == 0 {
		return 0, true, nil
	}
	return time.Duration(ms) * time.Millisecond, false, nil
}

// clearUnsentRecoveryLua undoes an issue whose email was never sent: it deletes
// the record and the slot and refunds the send, only while the slot is still
// this send's. KEYS: slot, sends, record. ARGV: send_id.
var clearUnsentRecoveryLua = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
redis.call("DEL", KEYS[1], KEYS[3])
local n = tonumber(redis.call("GET", KEYS[2]) or "0")
if n and n > 0 then redis.call("DECRBY", KEYS[2], 1) end
return 1
`)

func (h *Handler) clearUnsentRecovery(ctx context.Context, in recoveryIssue) error {
	_, err := clearUnsentRecoveryLua.Run(ctx, h.redis, []string{in.SlotKey, in.SendsKey, in.RecordKey}, in.SendID).Result()
	return err
}

// releaseRecoverySlotLua deletes the slot only if it still belongs to the send
// whose secret was just redeemed or spent. KEYS: slot. ARGV: send_id.
var releaseRecoverySlotLua = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0
`)

// releaseRecoverySlot lets the account request a new recovery email at once.
// Best-effort: a failure leaves the slot to expire on its own TTL.
func (h *Handler) releaseRecoverySlot(ctx context.Context, userID, sendID string) {
	if userID == "" || sendID == "" {
		return
	}
	if err := releaseRecoverySlotLua.Run(ctx, h.redis, []string{recoverySlotKey(userID)}, sendID).Err(); err != nil {
		h.log.Warn("Failed to release recovery send slot", "failure_class", "recovery_slot_release")
	}
}

// recoveryEmailTokenRecord is the stored value of a recovery key.
type recoveryEmailTokenRecord struct {
	UserID string `json:"user_id"`
	SendID string `json:"send_id"`
}

// consumeRecoveryEmailToken redeems a recovery key once. A key that was never
// issued, has expired or was already used returns redis.Nil.
func (h *Handler) consumeRecoveryEmailToken(ctx context.Context, token string) (string, error) {
	raw, err := h.redis.GetDel(ctx, recoveryEmailTokenKey(token)).Bytes()
	if err != nil {
		return "", err
	}
	var record recoveryEmailTokenRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.UserID == "" {
		return "", fmt.Errorf("malformed recovery key record")
	}
	h.releaseRecoverySlot(ctx, record.UserID, record.SendID)
	return record.UserID, nil
}
