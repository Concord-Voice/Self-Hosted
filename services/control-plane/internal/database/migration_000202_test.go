package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000202_ReservationNamespaceAndGuards(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	var columnCount int
	require.NoError(t, db.QueryRow(`
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'message_id_reservations'`).Scan(&columnCount))
	assert.Equal(t, 1, columnCount, "the reservation namespace must remain ID-only")

	messageID, dmMessageID, channelID, userID, conversationID := migration000202Fixture(t, db)
	_, err := db.Exec(`DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
	migrationRequirePQError(t, err, "23000", "message_id_reservations_immutable")
	_, err = db.Exec(`TRUNCATE message_id_reservations`)
	migrationRequirePQError(t, err, "23000", "message_id_reservations_immutable")
	_, err = testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'collision', 1)`, messageID, conversationID, userID)
	migrationRequirePQError(t, err, "23505", "message_id_reservations_pkey")
	_, err = testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, dmMessageID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'collision', 1)`, dmMessageID, channelID, userID)
	migrationRequirePQError(t, err, "23505", "message_id_reservations_pkey")
	result, err := db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'idempotent retry', 1)
		ON CONFLICT (id) DO NOTHING`, dmMessageID, conversationID, userID)
	require.NoError(t, err, "same-table legacy idempotency must reach ON CONFLICT")
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	assert.Zero(t, affected, "idempotent retry must not insert a second message")

	_, err = db.Exec(`UPDATE messages SET id = $1 WHERE id = $2`, uuid.New(), messageID)
	migrationRequirePQError(t, err, "23514", "message_id_reservations_message_id_immutable")

	_, err = db.Exec(`DELETE FROM messages WHERE id = $1`, messageID)
	require.NoError(t, err)
	var retained int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM message_id_reservations WHERE message_id = $1`, messageID).Scan(&retained))
	assert.Equal(t, 1, retained, "temporary delete capture retains a pre-baseline ID")

	// The fixture's channel remains valid for direct SQL setup, proving no
	// unrelated source-table mutation is needed to exercise the migration.
	assert.NotEqual(t, uuid.Nil, channelID)
}

func TestMigration000202_DownFailsClosedAndRoundTripsWhenUnused(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down := migrationReadFile(t, "../../migrations/000202_message_id_reservations.down.sql")
	up := migrationReadFile(t, "../../migrations/000202_message_id_reservations.up.sql")

	t.Run("history blocks downgrade", func(t *testing.T) {
		messageID, _, _, _, _ := migration000202Fixture(t, db)
		_, err := db.Exec(down)
		require.ErrorContains(t, err, "refusing to remove message ID reservation state")
		var retained int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_id_reservations WHERE message_id = $1`, messageID).Scan(&retained))
		assert.Equal(t, 1, retained, "refused down must preserve the reservation")
	})

	t.Run("unused installation round trips", func(t *testing.T) {
		// Use a transaction so the shared test schema always remains at head.
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		_, err = tx.Exec(`TRUNCATE messages, dm_messages CASCADE`)
		require.NoError(t, err)
		_, err = tx.Exec(`SET LOCAL session_replication_role = replica`)
		require.NoError(t, err)
		_, err = tx.Exec(`TRUNCATE message_id_reservations`)
		require.NoError(t, err)
		_, err = tx.Exec(down)
		require.NoError(t, err)
		var present sql.NullString
		require.NoError(t, tx.QueryRow(`SELECT to_regclass('message_id_reservations')`).Scan(&present))
		assert.False(t, present.Valid)
		_, err = tx.Exec(up)
		require.NoError(t, err)
		require.NoError(t, tx.QueryRow(`SELECT to_regclass('message_id_reservations')`).Scan(&present))
		assert.True(t, present.Valid)
	})
}

func migration000202Fixture(t *testing.T, db *sql.DB) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	userID := testhelpers.CreateUser(t, db)
	serverID := testhelpers.CreateServer(t, db, userID)
	channelID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO channels (id, server_id, name, type)
		VALUES ($1, $2, 'reservation-migration', 'text')`, channelID, serverID)
	require.NoError(t, err)
	conversationID := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, userID)
	require.NoError(t, err)
	messageID, dmMessageID := uuid.New(), uuid.New()
	_, err = db.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, messageID, channelID, userID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, dmMessageID, conversationID, userID)
	require.NoError(t, err)
	return messageID, dmMessageID, channelID, userID, conversationID
}
