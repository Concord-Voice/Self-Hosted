package database_test

import (
	"database/sql"
	"sync"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000208_BoundsNewMessagesWithoutReopeningIDs(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	channelMessageID, dmMessageID, channelID, userID, conversationID := migration000206Fixture(t, db)
	_, err := db.Exec(`UPDATE message_id_reservation_quota SET issued = 0 WHERE issued > 0`)
	migrationRequirePQError(t, err, "23000", "message_id_reservation_quota_monotonic")
	_, err = db.Exec(`DELETE FROM message_id_reservation_quota`)
	migrationRequirePQError(t, err, "23000", "message_id_reservation_quota_monotonic")
	_, err = db.Exec(`TRUNCATE message_id_reservation_quota`)
	migrationRequirePQError(t, err, "23000", "message_id_reservation_quota_monotonic")
	_, err = db.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued + 2 WHERE shard = 0`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE message_id_reservation_policy SET max_per_account = 3 WHERE singleton = TRUE`)
	require.NoError(t, err)

	thirdID := uuid.New()
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'third', 1)`, thirdID, channelID, userID)
	require.NoError(t, err)

	accountRejectedID := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'account cap', 1)`, accountRejectedID, conversationID, userID)
	migrationRequirePQError(t, err, "23514", "message_id_reservation_account_quota")
	migration000208RequireAbsent(t, db, accountRejectedID)
	migration000208RequireUsage(t, db, userID, 3, 3)

	// A same-table retry at the cap does not create another message or charge
	// quota. A deleted ID remains reserved and cannot be used again.
	result, err := db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'retry', 1) ON CONFLICT (id) DO NOTHING`, dmMessageID, conversationID, userID)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	assert.Zero(t, affected)
	migration000208RequireUsage(t, db, userID, 3, 3)
	_, err = db.Exec(`DELETE FROM messages WHERE id = $1`, channelMessageID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'reuse', 1)`, channelMessageID, channelID, userID)
	migrationRequirePQError(t, err, "23505", "message_id_reservations_pkey")
	migration000208RequireUsage(t, db, userID, 3, 3)

	otherUserID := testhelpers.CreateUser(t, db)
	fourthID := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'fourth', 1)`, fourthID, conversationID, otherUserID)
	require.NoError(t, err)
	migration000208RequireUsage(t, db, otherUserID, 4, 1)

	globalRejectedID := uuid.New()
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version)
		VALUES ($1, $2, $3, 'global cap', 1)`, globalRejectedID, channelID, otherUserID)
	migrationRequirePQError(t, err, "23514", "message_id_reservation_global_quota")
	migration000208RequireAbsent(t, db, globalRejectedID)
	migration000208RequireUsage(t, db, otherUserID, 4, 1)

	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, otherUserID)
	require.NoError(t, err)
	var accountRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_id_reservation_account_usage
		WHERE user_id = $1`, otherUserID).Scan(&accountRows))
	assert.Zero(t, accountRows, "account erasure removes its counter")
	var retained int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_id_reservations
		WHERE message_id = $1`, fourthID).Scan(&retained))
	assert.Equal(t, 1, retained, "erasure cannot reopen an identifier")
	migration000208RequireUsage(t, db, userID, 4, 3)
}

func TestMigration000208_HistoricalBackfillDoesNotSpendProspectiveQuota(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	messageID, _, _, userID, _ := migration000206Fixture(t, db)
	_, err := testdb.ExecReplica(db, `DELETE FROM message_id_reservations WHERE message_id = $1`, messageID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO message_id_reservations (message_id) VALUES ($1)`, messageID)
	require.NoError(t, err)
	migration000208RequireUsage(t, db, userID, 2, 2)
}

func TestMigration000208_ConcurrentGlobalCeiling(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	_, _, channelID, userID, _ := migration000206Fixture(t, db)
	otherUserID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE message_id_reservation_quota SET max_issued = issued + 1 WHERE shard = 0`)
	require.NoError(t, err)

	ids := []uuid.UUID{uuid.New(), uuid.New()}
	users := []uuid.UUID{userID, otherUserID}
	errors := make([]error, len(ids))
	var workers sync.WaitGroup
	for i, id := range ids {
		workers.Add(1)
		go func(i int, id uuid.UUID) {
			defer workers.Done()
			_, errors[i] = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version)
				VALUES ($1, $2, $3, 'concurrent', 1)`, id, channelID, users[i])
		}(i, id)
	}
	workers.Wait()

	var successes int
	for i, err := range errors {
		if err == nil {
			successes++
			continue
		}
		migrationRequirePQError(t, err, "23514", "message_id_reservation_global_quota")
		migration000208RequireAbsent(t, db, ids[i])
	}
	assert.Equal(t, 1, successes)
	var globalIssued, accountIssued int
	require.NoError(t, db.QueryRow(`SELECT sum(issued) FROM message_id_reservation_quota`).Scan(&globalIssued))
	require.NoError(t, db.QueryRow(`SELECT sum(issued) FROM message_id_reservation_account_usage`).Scan(&accountIssued))
	assert.Equal(t, 3, globalIssued)
	assert.Equal(t, 3, accountIssued)
}

func TestMigration000208_DownGuardAndUnusedRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down := migrationReadFile(t, "../../migrations/000208_bound_message_id_reservation_growth.down.sql")
	up := migrationReadFile(t, "../../migrations/000208_bound_message_id_reservation_growth.up.sql")

	_, _, _, _, _ = migration000206Fixture(t, db)
	locker, err := db.Begin()
	require.NoError(t, err)
	_, err = locker.Exec(`LOCK TABLE dm_messages IN ROW EXCLUSIVE MODE`)
	require.NoError(t, err)
	_, err = db.Exec(down)
	migrationRequirePQError(t, err, "55P03", "")
	require.NoError(t, locker.Rollback())

	_, err = db.Exec(down)
	require.ErrorContains(t, err, "refusing to remove message ID issuance quota")

	// An unused installation can round-trip. Keep this in a transaction so
	// the shared scratch schema remains at the newest migration after the test.
	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)
	_, err = tx.Exec(`TRUNCATE messages, dm_messages CASCADE`)
	require.NoError(t, err)
	_, err = tx.Exec(`SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.Exec(`TRUNCATE message_id_reservations`)
	require.NoError(t, err)
	_, err = tx.Exec(`DELETE FROM message_id_reservation_account_usage`)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE message_id_reservation_quota SET issued = 0`)
	require.NoError(t, err)
	_, err = tx.Exec(down)
	require.NoError(t, err)
	var present sql.NullString
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('message_id_reservation_quota')`).Scan(&present))
	assert.False(t, present.Valid)
	_, err = tx.Exec(up)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('message_id_reservation_quota')`).Scan(&present))
	assert.True(t, present.Valid)
}

func migration000208RequireUsage(t *testing.T, db *sql.DB, userID uuid.UUID, global, account int) {
	t.Helper()
	var globalIssued, accountIssued int
	require.NoError(t, db.QueryRow(`SELECT sum(issued) FROM message_id_reservation_quota`).Scan(&globalIssued))
	require.NoError(t, db.QueryRow(`SELECT issued FROM message_id_reservation_account_usage
		WHERE user_id = $1`, userID).Scan(&accountIssued))
	assert.Equal(t, global, globalIssued)
	assert.Equal(t, account, accountIssued)
}

func migration000208RequireAbsent(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	var reservations, messages, dms int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_id_reservations WHERE message_id = $1`, id).Scan(&reservations))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE id = $1`, id).Scan(&messages))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, id).Scan(&dms))
	assert.Zero(t, reservations)
	assert.Zero(t, messages)
	assert.Zero(t, dms)
}
