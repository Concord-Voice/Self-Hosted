package privacy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// EmailConfirmationTTL bounds a one-use unverified-email erasure intent.
	EmailConfirmationTTL = 15 * time.Minute

	emailConfirmationKeyPrefix = "account_erase_email:"
	emailConfirmationTimeout   = 2 * time.Second
	emailConfirmationTokenSize = 32
	emailConfirmationMaxEpoch  = 128
	emailConfirmationMaxEmail  = 320
	emailConfirmationMaxIssues = 3
)

var (
	// ErrEmailConfirmationStale collapses missing, expired, mismatched, and
	// replayed records so callers never disclose which binding rejected a request.
	ErrEmailConfirmationStale = errors.New("email confirmation stale")
	// ErrEmailConfirmationUnavailable fails closed when an unverified-email
	// intent record cannot be persisted or claimed.
	ErrEmailConfirmationUnavailable = errors.New("email confirmation unavailable")
	// ErrEmailConfirmationInvalid identifies an invalid server-side issue request.
	ErrEmailConfirmationInvalid = errors.New("invalid email confirmation binding")
)

var issueEmailConfirmationScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) ~= 0 then return 0 end
redis.call('HSET', KEYS[1],
  'token_digest', ARGV[1],
  'recipient_digest', ARGV[2],
  'user_id', ARGV[3],
  'family_id', ARGV[4],
  'credential_epoch', ARGV[5],
  'owned_set_digest', ARGV[6],
  'expires_at', ARGV[7],
  'state', 'pending')
if redis.call('PEXPIRE', KEYS[1], ARGV[8]) ~= 1 then
  redis.call('DEL', KEYS[1])
  return -1
end
return 1
`)

var claimEmailConfirmationScript = redis.NewScript(`
local function constant_time_equal(left, right)
  local left_len = string.len(left)
  local right_len = string.len(right)
  local diff = bit.bxor(left_len, right_len)
  local longest = left_len
  if right_len > longest then longest = right_len end
  for i = 1, longest do
    local left_byte = string.byte(left, i) or 0
    local right_byte = string.byte(right, i) or 0
    diff = bit.bor(diff, bit.bxor(left_byte, right_byte))
  end
  return diff == 0
end

local valid = 1
if not constant_time_equal(redis.call('HGET', KEYS[1], 'token_digest') or '', ARGV[1]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'recipient_digest') or '', ARGV[2]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'user_id') or '', ARGV[3]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'family_id') or '', ARGV[4]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'credential_epoch') or '', ARGV[5]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'owned_set_digest') or '', ARGV[6]) then valid = 0 end
if not constant_time_equal(redis.call('HGET', KEYS[1], 'state') or '', 'pending') then valid = 0 end
if valid == 0 then return 0 end
redis.call('HSET', KEYS[1], 'state', 'claimed:' .. ARGV[7])
return 1
`)

var releaseEmailConfirmationScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'claimed:' .. ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'state', 'pending')
return 1
`)

var commitEmailConfirmationScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'claimed:' .. ARGV[1] then return 0 end
return redis.call('DEL', KEYS[1])
`)

// EmailConfirmationBinding is the exact authority snapshot to which an email
// intent is bound. OwnedServerIDs must be canonical, sorted UUIDs.
type EmailConfirmationBinding struct {
	Recipient       string
	UserID          string
	FamilyID        string
	CredentialEpoch string
	OwnedServerIDs  []string
}

// EmailConfirmationIntent is returned only to the preparation handler. Token
// belongs in the email deep link; it must not be serialized in the API response.
type EmailConfirmationIntent struct {
	RequestID string
	Token     string
	ExpiresIn time.Duration
}

// EmailConfirmationStore persists and atomically consumes unverified-email
// erasure intents. The erasure transaction owner controls settlement.
type EmailConfirmationStore struct {
	redis *redis.Client
}

// NewEmailConfirmationStore builds an intent store. A nil client is accepted
// only to make unavailable-dependency handling explicit at the request edge.
func NewEmailConfirmationStore(redisClient *redis.Client) *EmailConfirmationStore {
	return &EmailConfirmationStore{redis: redisClient}
}

// Issue creates a recipient- and authority-bound confirmation record. The
// caller invokes it only after its preparation proof transaction commits.
func (s *EmailConfirmationStore) Issue(ctx context.Context, binding EmailConfirmationBinding) (EmailConfirmationIntent, error) {
	if s == nil || s.redis == nil {
		return EmailConfirmationIntent{}, ErrEmailConfirmationUnavailable
	}
	fields, err := emailConfirmationFields(binding)
	if err != nil {
		return EmailConfirmationIntent{}, err
	}

	for range emailConfirmationMaxIssues {
		requestID, err := newEmailConfirmationRequestID()
		if err != nil {
			return EmailConfirmationIntent{}, fmt.Errorf("generate email confirmation request id: %w", err)
		}
		secret, err := newEmailConfirmationSecret()
		if err != nil {
			return EmailConfirmationIntent{}, fmt.Errorf("generate email confirmation secret: %w", err)
		}
		token := base64.RawURLEncoding.EncodeToString(secret[:])
		tokenDigest := sha256.Sum256([]byte(token))
		expiresAt := time.Now().Add(EmailConfirmationTTL).Unix()

		issueCtx, cancel := emailConfirmationContext(ctx)
		result, runErr := issueEmailConfirmationScript.Run(issueCtx, s.redis, []string{emailConfirmationKey(requestID)},
			hex.EncodeToString(tokenDigest[:]), fields.recipientDigest, binding.UserID, binding.FamilyID,
			binding.CredentialEpoch, fields.ownedSetDigest, expiresAt, EmailConfirmationTTL.Milliseconds()).Int64()
		cancel()
		if runErr != nil {
			return EmailConfirmationIntent{}, fmt.Errorf("persist email confirmation intent: %w", ErrEmailConfirmationUnavailable)
		}
		switch result {
		case 1:
			return EmailConfirmationIntent{RequestID: requestID, Token: token, ExpiresIn: EmailConfirmationTTL}, nil
		case 0:
			// Never overwrite a record, even in the extremely unlikely UUID collision.
			continue
		default:
			return EmailConfirmationIntent{}, ErrEmailConfirmationUnavailable
		}
	}
	return EmailConfirmationIntent{}, ErrEmailConfirmationUnavailable
}

// Claim atomically changes a matching pending intent to a unique claim. The
// returned handle is released only on a known SQL rollback and committed only
// after a definite erasure commit. An unknown commit result does neither.
func (s *EmailConfirmationStore) Claim(ctx context.Context, requestID, token string, binding EmailConfirmationBinding) (*EmailConfirmationClaim, error) {
	if s == nil || s.redis == nil {
		return nil, ErrEmailConfirmationUnavailable
	}
	fields, err := emailConfirmationFields(binding)
	if err != nil || !isCanonicalUUID(requestID) {
		return nil, ErrEmailConfirmationStale
	}
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != emailConfirmationTokenSize || base64.RawURLEncoding.EncodeToString(secret) != token {
		return nil, ErrEmailConfirmationStale
	}
	claimID, err := newEmailConfirmationRequestID()
	if err != nil {
		return nil, fmt.Errorf("generate email confirmation claim id: %w", err)
	}
	tokenDigest := sha256.Sum256([]byte(token))
	claimCtx, cancel := emailConfirmationContext(ctx)
	result, runErr := claimEmailConfirmationScript.Run(claimCtx, s.redis, []string{emailConfirmationKey(requestID)},
		hex.EncodeToString(tokenDigest[:]), fields.recipientDigest, binding.UserID, binding.FamilyID,
		binding.CredentialEpoch, fields.ownedSetDigest, claimID).Int64()
	cancel()
	if runErr != nil {
		return nil, fmt.Errorf("claim email confirmation intent: %w", ErrEmailConfirmationUnavailable)
	}
	if result != 1 {
		return nil, ErrEmailConfirmationStale
	}
	return &EmailConfirmationClaim{store: s, requestID: requestID, claimID: claimID}, nil
}

// EmailConfirmationClaim is a capability for exactly one settlement path.
type EmailConfirmationClaim struct {
	store     *EmailConfirmationStore
	requestID string
	claimID   string
}

// Release returns this exact claim to pending without extending the original
// TTL. It is for a definitely rolled-back SQL transaction only.
func (c *EmailConfirmationClaim) Release(ctx context.Context) error {
	return c.settle(ctx, releaseEmailConfirmationScript, "release")
}

// Commit permanently removes this exact claim after a definitely committed
// account erasure transaction.
func (c *EmailConfirmationClaim) Commit(ctx context.Context) error {
	return c.settle(ctx, commitEmailConfirmationScript, "commit")
}

func (c *EmailConfirmationClaim) settle(ctx context.Context, script *redis.Script, operation string) error {
	if c == nil || c.store == nil || c.store.redis == nil || !isCanonicalUUID(c.requestID) || !isCanonicalUUID(c.claimID) {
		return ErrEmailConfirmationUnavailable
	}
	settleCtx, cancel := emailConfirmationContext(ctx)
	result, err := script.Run(settleCtx, c.store.redis, []string{emailConfirmationKey(c.requestID)}, c.claimID).Int64()
	cancel()
	if err != nil {
		return fmt.Errorf("%s email confirmation intent: %w", operation, ErrEmailConfirmationUnavailable)
	}
	if result != 1 {
		return ErrEmailConfirmationStale
	}
	return nil
}

type emailConfirmationRecordFields struct {
	recipientDigest string
	ownedSetDigest  string
}

func emailConfirmationFields(binding EmailConfirmationBinding) (emailConfirmationRecordFields, error) {
	if !validRecipient(binding.Recipient) || !isCanonicalUUID(binding.UserID) || !isCanonicalUUID(binding.FamilyID) ||
		len(binding.CredentialEpoch) > emailConfirmationMaxEpoch || strings.ContainsAny(binding.CredentialEpoch, "\x00\r\n") ||
		!canonicalOwnedServerIDs(binding.OwnedServerIDs) {
		return emailConfirmationRecordFields{}, ErrEmailConfirmationInvalid
	}
	recipientDigest := sha256.Sum256([]byte(binding.Recipient))
	return emailConfirmationRecordFields{
		recipientDigest: hex.EncodeToString(recipientDigest[:]),
		ownedSetDigest:  digestOwnedServerIDs(binding.OwnedServerIDs),
	}, nil
}

func validRecipient(recipient string) bool {
	return recipient != "" && len(recipient) <= emailConfirmationMaxEmail && recipient == strings.TrimSpace(recipient) &&
		!strings.ContainsAny(recipient, "\x00\r\n")
}

func canonicalOwnedServerIDs(ids []string) bool {
	for index, id := range ids {
		if !isCanonicalUUID(id) || (index > 0 && ids[index-1] >= id) {
			return false
		}
	}
	return true
}

func digestOwnedServerIDs(ids []string) string {
	hash := sha256.New()
	for _, id := range ids {
		// UUIDs are fixed width, but a delimiter keeps this digest unambiguous
		// if the identifier format ever changes.
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(id))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func newEmailConfirmationSecret() ([emailConfirmationTokenSize]byte, error) {
	var secret [emailConfirmationTokenSize]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return [emailConfirmationTokenSize]byte{}, err
	}
	return secret, nil
}

func newEmailConfirmationRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func isCanonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value[index] != '-' {
				return false
			}
			continue
		}
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func emailConfirmationKey(requestID string) string {
	return emailConfirmationKeyPrefix + requestID
}

func emailConfirmationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, emailConfirmationTimeout)
}
