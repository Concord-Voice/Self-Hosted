package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// enrollTokenTTL is the lifetime of a minted enrollment token. After this window
// the token is unusable and the operator must mint a fresh one (adminctl
// reset-enrollment). One hour balances "enough time to walk a key to the
// console" against minimizing the exposure window of a single-use secret.
const enrollTokenTTL = time.Hour

// enrollTokenBytes is the CSPRNG token length (256 bits). The plaintext is shown
// once to the operator; only its SHA-256 hash is stored.
const enrollTokenBytes = 32

// enrollKeyPrefix namespaces enrollment-token keys in Redis. The stored key is
// admin_enroll:{sha256(plaintext)} so the plaintext token is NEVER at rest —
// a Redis dump cannot be replayed into an enrollment (#1688 §5).
const enrollKeyPrefix = "admin_enroll:"

const enrollChallengeIndexPrefix = "admin_enroll_challenges:"

// ErrEnrollTokenInvalid is returned when a token is unknown, already consumed,
// or expired. A single sentinel keeps the failure uniform (no oracle telling an
// attacker whether a token ever existed vs. expired).
var ErrEnrollTokenInvalid = errors.New("admin: enrollment token is invalid or expired")

// EnrollmentStore mints and consumes single-use, hashed-at-rest enrollment
// tokens in Redis (#1688 §5). It is the bridge between out-of-band provisioning
// (adminctl bootstrap / in-console admin-create) and the browser enrollment
// ceremony.
type EnrollmentStore struct {
	redis *redis.Client
}

type enrollmentTokenBinding struct {
	AdminID              string `json:"admin_id"`
	CredentialGeneration int64  `json:"credential_generation"`
}

// NewEnrollmentStore wires an EnrollmentStore against a Redis client.
func NewEnrollmentStore(rdb *redis.Client) *EnrollmentStore {
	return &EnrollmentStore{redis: rdb}
}

// hashToken returns the hex SHA-256 of a plaintext token. The hash is the Redis
// key suffix; the plaintext is never stored.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func enrollChallengeIndexKey(adminID string, generation int64) string {
	return fmt.Sprintf("%s%s:%d", enrollChallengeIndexPrefix, adminID, generation)
}

// MintEnrollmentToken generates a fresh CSPRNG token bound to adminID, stores
// only its hash in Redis with a 1h TTL, and returns the plaintext to the caller
// (who shows it once to the operator). SetNX guards against the astronomically
// unlikely hash collision: a duplicate key is treated as an error rather than
// silently rebinding an existing admin's token.
func (s *EnrollmentStore) MintEnrollmentToken(ctx context.Context, adminID string, credentialGeneration int64) (string, error) {
	if adminID == "" {
		return "", errors.New("admin: MintEnrollmentToken requires an adminID")
	}
	if credentialGeneration < 1 {
		return "", errors.New("admin: MintEnrollmentToken requires a positive credential generation")
	}

	raw := make([]byte, enrollTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate enrollment token: %w", err)
	}
	plaintext := hex.EncodeToString(raw)

	binding, err := json.Marshal(enrollmentTokenBinding{AdminID: adminID, CredentialGeneration: credentialGeneration})
	if err != nil {
		return "", fmt.Errorf("marshal enrollment token binding: %w", err)
	}
	key := enrollKeyPrefix + hashToken(plaintext)
	ok, err := s.redis.SetNX(ctx, key, binding, enrollTokenTTL).Result()
	if err != nil {
		return "", fmt.Errorf("store enrollment token: %w", err)
	}
	if !ok {
		return "", errors.New("admin: enrollment token key collision")
	}
	return plaintext, nil
}

// ConsumeEnrollmentToken atomically reads-and-deletes the token binding
// (single-use via GETDEL), returning its admin ID and credential generation.
// Unknown, expired, malformed, and legacy unbound tokens fail closed.
func (s *EnrollmentStore) ConsumeEnrollmentToken(ctx context.Context, plaintext string) (string, int64, error) {
	if plaintext == "" {
		return "", 0, ErrEnrollTokenInvalid
	}
	key := enrollKeyPrefix + hashToken(plaintext)
	raw, err := s.redis.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", 0, ErrEnrollTokenInvalid
	}
	if err != nil {
		return "", 0, fmt.Errorf("consume enrollment token: %w", err)
	}
	var binding enrollmentTokenBinding
	if err := json.Unmarshal([]byte(raw), &binding); err != nil || binding.AdminID == "" || binding.CredentialGeneration < 1 {
		return "", 0, ErrEnrollTokenInvalid
	}
	return binding.AdminID, binding.CredentialGeneration, nil
}

// DeleteEnrollmentChallenges removes handles bound to a revoked credential generation.
func (s *EnrollmentStore) DeleteEnrollmentChallenges(ctx context.Context, adminID string, generation int64) error {
	indexKey := enrollChallengeIndexKey(adminID, generation)
	challengeKeys, err := s.redis.SMembers(ctx, indexKey).Result()
	if err != nil {
		return fmt.Errorf("list enrollment challenges: %w", err)
	}
	_, err = s.redis.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		if len(challengeKeys) > 0 {
			pipe.Del(ctx, challengeKeys...)
		}
		pipe.Del(ctx, indexKey)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete enrollment challenges: %w", err)
	}
	return nil
}
