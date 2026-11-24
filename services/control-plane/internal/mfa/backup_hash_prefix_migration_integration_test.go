//go:build integration

package mfa_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
)

// The migration under test. Renumber here if it moves at merge time.
const backupHashPrefixMigration uint = 202

// TestBackupHashPrefixMigrationRoundTrip pins 000202: up prefixes only bare
// SHA-256 elements and keeps order and used flags, the deployed reader still
// verifies the result, a clean down restores the bare row exactly, and the
// down refuses while any element has no bare form, such as a v2 code.
func TestBackupHashPrefixMigrationRoundTrip(t *testing.T) {
	db, m := newDisposableBackupHashMigrationDB(t)
	require.NoError(t, m.Migrate(priorBackupHashMigration(t)), "migrate to the pre-prefix schema")

	codes := []string{"BAREAAA1", "BAREAAA2", "BAREAAA3"}
	bare := make([]string, len(codes))
	for i, code := range codes {
		bare[i] = sha256Hex(code) // the verifier upper-cases a code before hashing
	}
	used := []bool{false, true, false}
	legacyUser := insertTOTPRow(t, db, bare, used)
	emptyUser := insertTOTPRow(t, db, []string{}, []bool{})

	require.NoError(t, m.Migrate(backupHashPrefixMigration), "000202 up")
	hashes, gotUsed := readBackupRow(t, db, legacyUser)
	require.Equal(t, []string{"v1$" + bare[0], "v1$" + bare[1], "v1$" + bare[2]}, hashes, "order is kept and each digest is unchanged")
	require.Equal(t, used, gotUsed, "used flags are untouched")
	for i, code := range codes {
		idx, matched, err := mfa.VerifyBackupCode(strings.ToLower(code), hashes, gotUsed)
		require.NoError(t, err, "the deployed reader accepts every migrated element")
		require.Equal(t, !used[i], matched, "code %d", i)
		if matched {
			require.Equal(t, i, idx)
		}
	}
	emptyHashes, _ := readBackupRow(t, db, emptyUser)
	require.Empty(t, emptyHashes)

	require.NoError(t, m.Steps(-1), "000202 down succeeds while every element is bare or v1")
	hashes, gotUsed = readBackupRow(t, db, legacyUser)
	require.Equal(t, bare, hashes, "down restores the bare row exactly")
	require.Equal(t, used, gotUsed)

	// A row written during the rollout: a bare code, a v2 code and a value no
	// reader accepts. Up prefixes only the bare element.
	_, v2Hashes, err := mfa.GenerateBackupCodes()
	require.NoError(t, err)
	unreadable := strings.ToUpper(sha256Hex("upper"))
	mixedBare := sha256Hex("mixedaa1")
	mixedUser := insertTOTPRow(t, db, []string{mixedBare, v2Hashes[0], unreadable}, []bool{false, false, false})

	require.NoError(t, m.Migrate(backupHashPrefixMigration), "000202 up again")
	hashes, _ = readBackupRow(t, db, mixedUser)
	require.Equal(t, []string{"v1$" + mixedBare, v2Hashes[0], unreadable}, hashes, "only bare elements are prefixed")

	refuseDown := func(reason string) {
		t.Helper()
		before, beforeUsed := readBackupRow(t, db, legacyUser)
		mixedBefore, _ := readBackupRow(t, db, mixedUser)
		err := m.Steps(-1)
		require.ErrorContains(t, err, "cannot roll back 000202", reason)
		after, afterUsed := readBackupRow(t, db, legacyUser)
		require.Equal(t, before, after, "a refused down leaves every row intact")
		require.Equal(t, beforeUsed, afterUsed)
		mixedAfter, _ := readBackupRow(t, db, mixedUser)
		require.Equal(t, mixedBefore, mixedAfter)
		// Reset the dirty marker only after asserting preservation, as the
		// 000155 round trip does.
		_, err = db.Exec(`UPDATE schema_migrations SET version = $1, dirty = FALSE`, backupHashPrefixMigration)
		require.NoError(t, err)
	}
	refuseDown("a v2 code has no bare form")

	newHash, err := backuphash.NewV2("onlyv2aa")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_mfa_totp SET backup_codes_hash = $1 WHERE user_id = $2`, pq.Array([]string{newHash}), mixedUser)
	require.NoError(t, err)
	refuseDown("a regenerated v2-only row refuses")

	_, err = db.Exec(`UPDATE user_mfa_totp SET backup_codes_hash = $1 WHERE user_id = $2`, pq.Array([]string{"v1$" + mixedBare, unreadable}), mixedUser)
	require.NoError(t, err)
	refuseDown("an element no reader accepts refuses too")

	_, err = db.Exec(`UPDATE user_mfa_totp SET backup_codes_hash = $1 WHERE user_id = $2`, pq.Array([]string{"v1$" + mixedBare}), mixedUser)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "down succeeds once only bare and v1 elements remain")
	hashes, _ = readBackupRow(t, db, mixedUser)
	require.Equal(t, []string{mixedBare}, hashes)
}

// The down migration must hold its write-blocking lock before the guard reads
// the table, or a v2 write could commit between the check and the rewrite.
func TestBackupHashPrefixDownLocksBeforeChecking(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "000202_*.down.sql"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	body, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	sqlText := string(body)
	lock := strings.Index(sqlText, "LOCK TABLE user_mfa_totp IN SHARE ROW EXCLUSIVE MODE;")
	guard := strings.Index(sqlText, "RAISE EXCEPTION")
	rewrite := strings.Index(sqlText, "UPDATE user_mfa_totp")
	require.Positive(t, lock)
	require.Greater(t, guard, lock, "the guard runs under the lock")
	require.Greater(t, rewrite, guard, "the rewrite runs after the guard")
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func insertTOTPRow(t *testing.T, db *sql.DB, hashes []string, used []bool) string {
	t.Helper()
	id := uuid.NewString()
	name := "bh" + strings.ReplaceAll(id, "-", "")[:12]
	_, err := db.Exec(`INSERT INTO users (id, username, email, password_hash) VALUES ($1, $2, $3, 'unused')`,
		id, name, name+"@example.test")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, backup_codes_hash, backup_codes_used)
		VALUES ($1, '\x00', '\x00', TRUE, $2, $3)`, id, pq.Array(hashes), pq.Array(used))
	require.NoError(t, err)
	return id
}

func readBackupRow(t *testing.T, db *sql.DB, userID string) ([]string, []bool) {
	t.Helper()
	var hashes []string
	var used []bool
	require.NoError(t, db.QueryRow(`SELECT backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).
		Scan(pq.Array(&hashes), pq.Array(&used)))
	return hashes, used
}

// priorBackupHashMigration returns the highest migration version below 000202,
// read from the migrations directory so no neighbour's number is hard-coded.
func priorBackupHashMigration(t *testing.T) uint {
	t.Helper()
	ups, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	require.NoError(t, err)
	var prior uint
	for _, path := range ups {
		n, convErr := strconv.ParseUint(strings.SplitN(filepath.Base(path), "_", 2)[0], 10, 64)
		require.NoError(t, convErr)
		if uint(n) < backupHashPrefixMigration && uint(n) > prior {
			prior = uint(n)
		}
	}
	require.NotZero(t, prior)
	return prior
}

// newDisposableBackupHashMigrationDB creates a throwaway database so the round
// trip never moves the shared test database's schema version. Copied from
// newDisposableClearReasonMigrationDB in internal/purge.
func newDisposableBackupHashMigrationDB(t *testing.T) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	baseURL := testdb.DatabaseURL()
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	databaseName := "concord_backup_hash_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pq.QuoteIdentifier(databaseName)
	adminDB, err := sql.Open("postgres", baseURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
		if _, cleanupErr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quotedName + ` WITH (FORCE)`); cleanupErr != nil {
			t.Errorf("drop disposable database: %v", cleanupErr)
		}
		if cleanupErr := adminDB.Close(); cleanupErr != nil {
			t.Errorf("close disposable database admin connection: %v", cleanupErr)
		}
	})
	require.NoError(t, adminDB.Ping())
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
	_, err = adminDB.Exec(`CREATE DATABASE ` + quotedName)
	require.NoError(t, err)

	parsed.Path = "/" + databaseName
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := db.Close(); cleanupErr != nil {
			t.Errorf("close disposable migration database: %v", cleanupErr)
		}
	})
	require.NoError(t, db.Ping())

	migrationDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	require.NoError(t, err)
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationDir, "postgres", driver)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil {
			t.Errorf("close disposable migration source: %v", sourceErr)
		}
		if databaseErr != nil {
			t.Errorf("close disposable migration database driver: %v", databaseErr)
		}
	})
	return db, m
}
