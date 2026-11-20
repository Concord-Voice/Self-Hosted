package mfa

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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
	if !s.done && strings.Contains(query, "SET backup_codes_used") {
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

func backupCodeClaimFixture(t *testing.T) (*sql.DB, *Handler, string, []string) {
	t.Helper()
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := dbtest.CreateUser(t, db).String()
	keyring, err := ParseKeyring(strings.Repeat("00", 32), 1, "")
	require.NoError(t, err)
	secretEnc, secretNonce, keyVersion, err := keyring.Seal([]byte("invalid-totp-seed"))
	require.NoError(t, err)
	codes, hashes, err := GenerateBackupCodes()
	require.NoError(t, err)
	require.NotEqual(t, codes[0], codes[1])
	_, err = db.Exec(`
		INSERT INTO user_mfa_totp
			(user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed, backup_codes_hash, backup_codes_used)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, $5, $6)`,
		userID, secretEnc, secretNonce, keyVersion,
		pq.Array(hashes), pq.Array(make([]bool, len(hashes))))
	require.NoError(t, err)
	return db, NewHandler(db, nil, logger.New("test"), keyring, "test", nil, "test"), userID, codes
}

type backupClaimBarrierStore struct {
	codeVerificationStore
	arrived chan<- struct{}
	release <-chan struct{}
}

func (s *backupClaimBarrierStore) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if strings.Contains(query, "SET backup_codes_used") {
		select {
		case s.arrived <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.codeVerificationStore.ExecContext(ctx, query, args...)
}

func TestVerifyCodeConcurrentBackupClaims(t *testing.T) {
	for _, tc := range []struct {
		name        string
		distinct    bool
		transaction bool
		callers     int
		wantSuccess int
	}{
		{name: "same code on pool", callers: 8, wantSuccess: 1},
		{name: "different codes on pool", distinct: true, callers: 2, wantSuccess: 2},
		{name: "same code in transactions", transaction: true, callers: 8, wantSuccess: 1},
		{name: "different codes in transactions", distinct: true, transaction: true, callers: 2, wantSuccess: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, handler, userID, codes := backupCodeClaimFixture(t)
			db.SetMaxOpenConns(tc.callers + 1)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			arrived := make(chan struct{}, tc.callers)
			release := make(chan struct{})
			type verification struct {
				ok  bool
				err error
			}
			results := make(chan verification, tc.callers)
			for i := 0; i < tc.callers; i++ {
				code := codes[0]
				if tc.distinct {
					code = codes[i]
				}
				go func() {
					var executor codeVerificationStore = db
					var tx *sql.Tx
					if tc.transaction {
						var err error
						tx, err = db.BeginTx(ctx, nil)
						if err != nil {
							results <- verification{err: err}
							return
						}
						executor = tx
					}
					store := &backupClaimBarrierStore{codeVerificationStore: executor, arrived: arrived, release: release}
					ok, err := handler.verifyCode(ctx, store, userID, stepup.PurposeTOTPSetup, code)
					if tx != nil {
						if err == nil && ok {
							err = tx.Commit()
							ok = err == nil
						} else if rollbackErr := tx.Rollback(); err == nil {
							err = rollbackErr
						}
					}
					results <- verification{ok: ok, err: err}
				}()
			}
			for i := 0; i < tc.callers; i++ {
				select {
				case <-arrived:
				case <-ctx.Done():
					close(release)
					t.Fatal("verifiers did not all reach the claim barrier")
				}
			}
			close(release)
			successes := 0
			for i := 0; i < tc.callers; i++ {
				result := <-results
				require.NoError(t, result.err)
				if result.ok {
					successes++
				}
			}
			require.Equal(t, tc.wantSuccess, successes)
			var used []bool
			require.NoError(t, db.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used)))
			require.True(t, used[0])
			require.Equal(t, tc.distinct, used[1])
		})
	}
}

func TestVerifyCodeTxBackupClaimRollsBack(t *testing.T) {
	db, handler, userID, codes := backupCodeClaimFixture(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback backup-code claim: %v", err)
		}
	}()

	ok, err := handler.VerifyCodeTx(ctx, tx, userID, stepup.PurposeTOTPSetup, codes[0])
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = handler.VerifyCodeTx(ctx, tx, userID, stepup.PurposeTOTPSetup, codes[0])
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, tx.Rollback())

	ok, err = handler.VerifyCode(ctx, userID, stepup.PurposeTOTPSetup, codes[0])
	require.NoError(t, err)
	require.True(t, ok, "rolling back the caller transaction must restore the code")
	ok, err = handler.VerifyCode(ctx, userID, stepup.PurposeTOTPSetup, codes[0])
	require.NoError(t, err)
	require.False(t, ok)
}

type deactivatingBackupCodeStore struct {
	*sql.DB
	userID   string
	mutation string
	done     bool
}

func (s *deactivatingBackupCodeStore) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if !s.done && strings.Contains(query, "SET backup_codes_used") {
		s.done = true
		if _, err := s.DB.ExecContext(ctx, s.mutation, s.userID); err != nil {
			return nil, fmt.Errorf("simulate factor deactivation: %w", err)
		}
	}
	return s.DB.ExecContext(ctx, query, args...)
}

func TestVerifyCodeRejectsBackupClaimAfterFactorDeactivation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
	}{
		{name: "disabled", mutation: `UPDATE user_mfa_totp SET enabled = FALSE WHERE user_id = $1`},
		{name: "unconfirmed", mutation: `UPDATE user_mfa_totp SET confirmed = FALSE WHERE user_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, handler, userID, codes := backupCodeClaimFixture(t)
			store := &deactivatingBackupCodeStore{DB: db, userID: userID, mutation: tc.mutation}
			ok, err := handler.verifyCode(context.Background(), store, userID, stepup.PurposeTOTPSetup, codes[0])
			require.NoError(t, err)
			require.False(t, ok)
			require.True(t, store.done)
			var used []bool
			require.NoError(t, db.QueryRow(`SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used)))
			require.False(t, used[0])
		})
	}
}

type failedBackupClaimStore struct {
	*sql.DB
	result sql.Result
	err    error
}

func (s *failedBackupClaimStore) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return s.result, s.err
}

type failedBackupClaimResult struct{ err error }

func (r failedBackupClaimResult) LastInsertId() (int64, error) { return 0, r.err }
func (r failedBackupClaimResult) RowsAffected() (int64, error) { return 0, r.err }

func TestVerifyCodeBackupClaimFailsClosed(t *testing.T) {
	db, handler, userID, codes := backupCodeClaimFixture(t)
	claimErr := errors.New("backup claim failed")
	for _, tc := range []struct {
		name    string
		result  sql.Result
		err     error
		wantErr bool
	}{
		{name: "write error", err: claimErr, wantErr: true},
		{name: "affected rows error", result: failedBackupClaimResult{err: claimErr}, wantErr: true},
		{name: "zero affected rows", result: driver.RowsAffected(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &failedBackupClaimStore{DB: db, result: tc.result, err: tc.err}
			ok, err := handler.verifyCode(context.Background(), store, userID, stepup.PurposeTOTPSetup, codes[0])
			require.False(t, ok)
			if tc.wantErr {
				require.ErrorIs(t, err, claimErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
