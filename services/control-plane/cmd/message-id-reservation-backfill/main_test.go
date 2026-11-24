package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReservationBackfillCompletesVerifiedBaseline(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	messageID, dmMessageID := insertReservationFixture(t, db, uuid.New(), uuid.New())
	// SetupTestDB is already at migration 000202, so remove these fixture
	// reservations to model rows that existed before the schema migration.
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id IN ($1, $2)`, messageID, dmMessageID)
	require.NoError(t, err)

	require.NoError(t, (&reservationBackfill{db: db, batchSize: 1}).run(context.Background()))

	var complete bool
	require.NoError(t, db.QueryRow(`
		SELECT backfill_complete FROM message_id_reservation_rollout WHERE singleton = TRUE`).Scan(&complete))
	assert.True(t, complete)

	var reserved int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM message_id_reservations WHERE message_id IN ($1, $2)`, messageID, dmMessageID).Scan(&reserved))
	assert.Equal(t, 2, reserved)

	var deleteCaptureTriggers int
	require.NoError(t, db.QueryRow(`
		SELECT count(*)
		FROM pg_trigger
		WHERE tgname = 'message_id_reservations_before_delete'
			AND tgrelid IN ('messages'::regclass, 'dm_messages'::regclass)
			AND NOT tgisinternal`).Scan(&deleteCaptureTriggers))
	assert.Zero(t, deleteCaptureTriggers, "verified completion removes temporary delete capture")
}

func TestReservationBackfillRejectsPreexistingCrossTableCollision(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	collidingID := uuid.New()
	insertReservationFixture(t, db, collidingID, collidingID)

	err := (&reservationBackfill{db: db, batchSize: 1}).run(context.Background())
	require.ErrorContains(t, err, "cross-table collision")

	var complete bool
	require.NoError(t, db.QueryRow(`
		SELECT backfill_complete FROM message_id_reservation_rollout WHERE singleton = TRUE`).Scan(&complete))
	assert.False(t, complete, "collision leaves the activation gate closed")
}

func insertReservationFixture(t *testing.T, db *sql.DB, messageID, dmMessageID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	userID := testhelpers.CreateUser(t, db)
	serverID := testhelpers.CreateServer(t, db, userID)
	channelID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO channels (id, server_id, name, type)
		VALUES ($1, $2, 'message-id-reservation', 'text')`, channelID, serverID)
	require.NoError(t, err)

	conversationID := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, userID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, messageID, channelID, userID)
	require.NoError(t, err)
	if messageID == dmMessageID {
		// A historical collision can predate the reservation migration. Remove
		// the fixture's automatic insert reservation so the second legacy row
		// models that state and the backfill command can detect it.
		_, err = testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
		require.NoError(t, err)
	}
	insertDM := func() error {
		_, insertErr := db.Exec(`
		INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fixture', 1)`, dmMessageID, conversationID, userID)
		return insertErr
	}
	if messageID == dmMessageID {
		// A scoped trigger bypass models a historical collision that predates
		// 000202; it is immediately restored for the rest of this test DB.
		tx, beginErr := db.Begin()
		require.NoError(t, beginErr)
		defer func() {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback historical collision fixture: %v", rollbackErr)
			}
		}()
		_, err = tx.Exec(`SET LOCAL session_replication_role = replica`)
		require.NoError(t, err)
		_, err = tx.Exec(`ALTER TABLE dm_messages DISABLE TRIGGER message_id_reservations_before_insert`)
		require.NoError(t, err)
		_, err = tx.Exec(`
			INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
			VALUES ($1, $2, $3, 'fixture', 1)`, dmMessageID, conversationID, userID)
		require.NoError(t, err)
		_, err = tx.Exec(`ALTER TABLE dm_messages ENABLE ALWAYS TRIGGER message_id_reservations_before_insert`)
		require.NoError(t, err)
		err = tx.Commit()
	} else {
		err = insertDM()
	}
	require.NoError(t, err)
	return messageID, dmMessageID
}
