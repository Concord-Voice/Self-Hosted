package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// recoveryCodeSendPrefix starts the send_id of a 6-digit code email, so the
	// issue script can tell a code-held slot from a key-held one.
	recoveryCodeSendPrefix = "c-"
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
	// CodeKey is set only for a key: the account's 6-digit code record that
	// the key replaces when a code holds the slot.
	CodeKey string
}

// issueRecoveryRecordLua takes the account's send slot, counts the send, picks
// the tier, and stores the record with the slot's TTL, all in one command.
// Redis does not roll back a script that fails part way, so the record is
// written before the slot: a partial run leaves a usable record and a free
// slot, never a held slot with nothing behind it.
// A held slot refuses the send, except that a key (ARGV[7] == "1") replaces a
// slot held by a 6-digit code (its send_id starts with ARGV[8]) and deletes
// that code (KEYS[4]): an attacker must not hold the slot with a code its owner
// cannot use. The code is deleted last, after both writes, because Redis
// refuses INCR and SET (not DEL) when it is out of memory.
// A code record written before send slots existed has no slot. A code request
// treats it as held, so its attempt count is never reset, and a key request
// deletes it like a code that holds the slot.
// KEYS: slot, sends, record, code. ARGV: send_id, record, fast ms, slow ms,
// fast sends, window ms, replace, code send prefix. Returns 0 when held, else
// the TTL in ms.
var issueRecoveryRecordLua = redis.NewScript(`
local slot = redis.call("GET", KEYS[1])
local replaced = ARGV[7] == "1"
if slot then
  if not replaced or string.sub(slot, 1, string.len(ARGV[8])) ~= ARGV[8] then return 0 end
elseif not replaced and redis.call("EXISTS", KEYS[4]) == 1 then
  return 0
end
local n = redis.call("INCR", KEYS[2])
if redis.call("PTTL", KEYS[2]) < 0 then redis.call("PEXPIRE", KEYS[2], ARGV[6]) end
local ttl = ARGV[3]
if n > tonumber(ARGV[5]) then ttl = ARGV[4] end
redis.call("SET", KEYS[3], ARGV[2], "PX", ttl)
redis.call("SET", KEYS[1], ARGV[1], "PX", ttl)
if replaced then redis.call("DEL", KEYS[4]) end
return tonumber(ttl)
`)

// issueRecoveryRecord stores a recovery record unless the account already has
// a live recovery email. held is true when nothing was stored.
func (h *Handler) issueRecoveryRecord(ctx context.Context, in recoveryIssue) (ttl time.Duration, held bool, err error) {
	codeKey, replace := in.RecordKey, "0"
	if in.CodeKey != "" {
		codeKey, replace = in.CodeKey, "1"
	}
	ms, err := issueRecoveryRecordLua.Run(ctx, h.redis,
		[]string{in.SlotKey, in.SendsKey, in.RecordKey, codeKey},
		in.SendID, string(in.Record),
		recoveryFastTierTTL.Milliseconds(), recoverySlowTierTTL.Milliseconds(),
		recoveryFastTierSends, recoverySendsWindow.Milliseconds(),
		replace, recoveryCodeSendPrefix,
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
// It runs on a context detached from the request: a client that hangs up after
// its key was spent must not leave the owner waiting out the cooldown.
// Best-effort: a failure leaves the slot to expire on its own TTL.
func (h *Handler) releaseRecoverySlot(ctx context.Context, userID, sendID string) {
	if userID == "" || sendID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := releaseRecoverySlotLua.Run(ctx, h.redis, []string{recoverySlotKey(userID)}, sendID).Err(); err != nil {
		h.log.Warn("Failed to release recovery send slot", "failure_class", recoveryRedisFailureClass("recovery_slot_release", err))
	}
}

// recoverySendStillCurrent reports whether a code delivery still owns its
// account's send slot. Only a code can be replaced (by a key), so a key always
// is. A Redis error answers true: this check only narrows a race, and failing
// toward no email would cost the owner a send they asked for.
// ponytail: a code whose SMTP send is already under way when the key replaces
// it can still arrive after the key email; closing that needs a per-account
// delivery lock across both formats.
func (h *Handler) recoverySendStillCurrent(d *recoveryDelivery) bool {
	if d.format == recoveryFormatToken {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	slot, err := h.redis.Get(ctx, d.issue.SlotKey).Result()
	if errors.Is(err, redis.Nil) {
		return false
	}
	return err != nil || slot == d.issue.SendID
}

// recoveryRedisFailureClass names a Redis failure by cause, so a timeout and a
// Redis error are told apart in logs that carry no error text.
func recoveryRedisFailureClass(base string, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return base + "_timeout"
	case errors.Is(err, context.Canceled):
		return base + "_cancelled"
	default:
		return base + "_redis"
	}
}

// recoveryEmailTokenRecord is the stored value of a recovery key. Addr is the
// address the key was emailed to; a key is refused once the account no longer
// uses that address.
type recoveryEmailTokenRecord struct {
	UserID string `json:"user_id"`
	SendID string `json:"send_id"`
	Addr   string `json:"addr"`
}

// errMalformedRecoveryKeyRecord marks a stored key record that cannot be read.
var errMalformedRecoveryKeyRecord = errors.New("malformed recovery key record")

func decodeRecoveryEmailTokenRecord(raw []byte) (recoveryEmailTokenRecord, error) {
	var record recoveryEmailTokenRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, fmt.Errorf("%w: %w", errMalformedRecoveryKeyRecord, err)
	}
	if record.UserID == "" || record.Addr == "" {
		return record, errMalformedRecoveryKeyRecord
	}
	return record, nil
}

// peekRecoveryEmailToken reads a key's record without spending the key, so
// checks that can fail for server reasons run before the key is used up. A key
// that was never issued, has expired or was already used returns redis.Nil.
func (h *Handler) peekRecoveryEmailToken(ctx context.Context, token string) (recoveryEmailTokenRecord, error) {
	raw, err := h.redis.Get(ctx, recoveryEmailTokenKey(token)).Bytes()
	if err != nil {
		return recoveryEmailTokenRecord{}, err
	}
	return decodeRecoveryEmailTokenRecord(raw)
}

// consumeRecoveryEmailToken redeems a recovery key once and frees the account's
// send slot. A key that was never issued, has expired or was already used
// returns redis.Nil.
func (h *Handler) consumeRecoveryEmailToken(ctx context.Context, token string) (recoveryEmailTokenRecord, error) {
	raw, err := h.redis.GetDel(ctx, recoveryEmailTokenKey(token)).Bytes()
	if err != nil {
		return recoveryEmailTokenRecord{}, err
	}
	record, err := decodeRecoveryEmailTokenRecord(raw)
	if err != nil {
		return record, err
	}
	h.releaseRecoverySlot(ctx, record.UserID, record.SendID)
	return record, nil
}
