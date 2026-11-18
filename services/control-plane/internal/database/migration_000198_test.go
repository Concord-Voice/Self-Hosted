package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000198: the per-row media boot stamp on voice_participants and the
// server_voice_room_terminals tombstone (VULN-110, VULN-112, VULN-208,
// VULN-209).
//
// Every behavioural test runs on migration000198Fresh, a transaction that runs
// the down and then the up FILE TEXT and is always rolled back, so the schema
// under test is the file. Call testhelpers.SetupTestServer BEFORE it, so the
// rollback runs before the server's TRUNCATE cleanup.

const migration000198Check = "23514" // check_violation

func migration000198SQL(t *testing.T, direction string) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000198_add_voice_media_boot_binding."+direction+".sql")
}

func migration000198Fresh(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	_, err = tx.Exec(migration000198SQL(t, "down"))
	require.NoError(t, err, "the down must run on the migrated schema")
	_, err = tx.Exec(migration000198SQL(t, "up"))
	require.NoError(t, err, "the up must run after the down")
	return tx
}

// migration000198Seed returns a voice channel and a user that may join it.
func migration000198Seed(t *testing.T, ts *testhelpers.TestServer) (channelID, userID string) {
	t.Helper()
	user := ts.CreateTestUser(t, "m198_user")
	serverID := ts.CreateTestServer(t, user.ID, "M198 Server")
	return ts.CreateVoiceChannel(t, serverID, "voice-m198"), user.ID
}

func migration000198Exists(t *testing.T, tx *sql.Tx, query string, args ...any) bool {
	t.Helper()
	var exists bool
	require.NoError(t, tx.QueryRow(query, args...).Scan(&exists))
	return exists
}

func migration000198ColumnExists(t *testing.T, tx *sql.Tx) bool {
	t.Helper()
	return migration000198Exists(t, tx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'voice_participants' AND column_name = 'media_boot_id')`)
}

func migration000198TableExists(t *testing.T, tx *sql.Tx) bool {
	t.Helper()
	return migration000198Exists(t, tx, `SELECT to_regclass('server_voice_room_terminals') IS NOT NULL`)
}

func migration000198ConstraintValidated(t *testing.T, tx *sql.Tx) (exists, validated bool) {
	t.Helper()
	err := tx.QueryRow(`SELECT convalidated FROM pg_constraint
		WHERE conname = 'voice_participants_media_boot_id_not_nil'`).Scan(&validated)
	if err == sql.ErrNoRows {
		return false, false
	}
	require.NoError(t, err)
	return true, validated
}

func migration000198ExpectCheck(t *testing.T, tx *sql.Tx, query string, args ...any) {
	t.Helper()
	_, err := tx.Exec(`SAVEPOINT m198`)
	require.NoError(t, err)
	_, err = tx.Exec(query, args...)
	migrationRequirePQError(t, err, migration000198Check, "")
	_, err = tx.Exec(`ROLLBACK TO SAVEPOINT m198`)
	require.NoError(t, err)
}

func TestMigration000198_UpDownUpRoundTrip(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	tx := migration000198Fresh(t, ts.DB)

	assert.True(t, migration000198ColumnExists(t, tx))
	assert.True(t, migration000198TableExists(t, tx))
	exists, validated := migration000198ConstraintValidated(t, tx)
	assert.True(t, exists)
	assert.True(t, validated, "the CHECK is a normal validated constraint, never NOT VALID")
	var nullable, hasDefault string
	require.NoError(t, tx.QueryRow(`SELECT is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_name = 'voice_participants' AND column_name = 'media_boot_id'`).Scan(&nullable, &hasDefault))
	assert.Equal(t, "YES", nullable)
	assert.Empty(t, hasDefault, "a default would stamp rows no boot wrote")

	_, err := tx.Exec(migration000198SQL(t, "down"))
	require.NoError(t, err)
	assert.False(t, migration000198ColumnExists(t, tx))
	assert.False(t, migration000198TableExists(t, tx))
	exists, _ = migration000198ConstraintValidated(t, tx)
	assert.False(t, exists)

	_, err = tx.Exec(migration000198SQL(t, "up"))
	require.NoError(t, err, "the up must reapply after its own down")
	assert.True(t, migration000198ColumnExists(t, tx))
	assert.True(t, migration000198TableExists(t, tx))
}

func TestMigration000198_RefusesNilUUIDStamp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	channelID, userID := migration000198Seed(t, ts)
	tx := migration000198Fresh(t, ts.DB)

	migration000198ExpectCheck(t, tx, `INSERT INTO voice_participants
		(channel_id, user_id, lifecycle_event_at, media_boot_id)
		VALUES ($1, $2, clock_timestamp(), '00000000-0000-0000-0000-000000000000')`, channelID, userID)

	_, err := tx.Exec(`INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
		VALUES ($1, $2, clock_timestamp())`, channelID, userID)
	require.NoError(t, err, "an unstamped row stays legal")
	migration000198ExpectCheck(t, tx, `UPDATE voice_participants
		SET media_boot_id = '00000000-0000-0000-0000-000000000000'
		WHERE channel_id = $1 AND user_id = $2`, channelID, userID)
	_, err = tx.Exec(`UPDATE voice_participants SET media_boot_id = $3
		WHERE channel_id = $1 AND user_id = $2`, channelID, userID, uuid.New())
	require.NoError(t, err, "a real boot stamp is accepted")
}

func TestMigration000198_TerminalCascadesWithChannel(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	channelID, _ := migration000198Seed(t, ts)
	tx := migration000198Fresh(t, ts.DB)

	_, err := tx.Exec(`INSERT INTO server_voice_room_terminals
		(channel_id, media_boot_id, recorded_by, removed_count) VALUES ($1, $2, $3, 1)`,
		channelID, uuid.New(), uuid.New())
	require.NoError(t, err)
	var recorded bool
	require.NoError(t, tx.QueryRow(`SELECT recorded_at IS NOT NULL FROM server_voice_room_terminals
		WHERE channel_id = $1`, channelID).Scan(&recorded))
	assert.True(t, recorded)

	_, err = tx.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	require.NoError(t, err)
	assert.False(t, migration000198Exists(t, tx,
		`SELECT EXISTS (SELECT 1 FROM server_voice_room_terminals WHERE channel_id = $1)`, channelID))
}

func TestMigration000198_RemovedCountPositive(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	channelID, _ := migration000198Seed(t, ts)
	tx := migration000198Fresh(t, ts.DB)

	for _, count := range []int{0, -1} {
		migration000198ExpectCheck(t, tx, `INSERT INTO server_voice_room_terminals
			(channel_id, media_boot_id, recorded_by, removed_count) VALUES ($1, $2, $3, $4)`,
			channelID, uuid.New(), uuid.New(), count)
	}
}
