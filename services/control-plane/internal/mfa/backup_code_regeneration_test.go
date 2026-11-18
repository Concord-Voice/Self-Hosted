package mfa

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// regeneratingBackupCodeStore simulates the account owner regenerating their
// backup codes after this consumer reads the stored set but before its
// conditional update runs. Regeneration replaces every hash and resets every
// used flag, so the used flags alone look unchanged.
type regeneratingBackupCodeStore struct {
	*sql.DB
	userID    string
	newHashes []string
	done      bool
}

func (s *regeneratingBackupCodeStore) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	// Race only the consume update, so an earlier exec cannot move the race point.
	if !s.done && strings.Contains(query, "SET backup_codes_used = $1") {
		s.done = true
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2 WHERE user_id = $3`,
			pq.Array(s.newHashes), pq.Array(make([]bool, len(s.newHashes))), s.userID); err != nil {
			return nil, fmt.Errorf("simulate backup-code regeneration: %w", err)
		}
	}
	return s.DB.ExecContext(ctx, query, args...)
}

func TestConsumeBackupCodeRejectsCodeRevokedByConcurrentRegeneration(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db)
	keyring, err := ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte("invalid-totp-seed"))
	require.NoError(t, err)

	oldCodes, oldHashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	_, newHashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp
			(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`,
		userID, secretEnc, secretNonce, keyVersion,
		pq.Array(oldHashes), pq.Array(make([]bool, len(oldHashes))))
	require.NoError(t, err)

	store := &regeneratingBackupCodeStore{DB: db, userID: userID.String(), newHashes: newHashes}
	ok, err := consumeBackupCode(context.Background(), store, userID.String(), oldCodes[0])

	require.NoError(t, err)
	require.False(t, ok, "a code from the set that regeneration revoked must not be accepted")
	var used []bool
	require.NoError(t, db.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used)))
	require.Equal(t, make([]bool, len(newHashes)), used, "no code of the new set may be marked used")
	var stored []string
	require.NoError(t, db.QueryRow(`SELECT backup_codes_hash FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&stored)))
	require.Equal(t, newHashes, stored, "the regenerated set must stay in place")
	require.True(t, store.done, "the simulated regeneration must have run")
}
