package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	msgTypeDM = "dm_message"
)

// hubTestMigrationsPath resolves the absolute path to the migrations directory
// using runtime.Caller, matching the pattern in testhelpers/testdb.go.
func hubTestMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	// websocket/ is at internal/websocket/hub_epoch_test.go
	// migrations/ is at migrations/
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// setupHubTestDB creates a real DB connection for hub tests without importing testhelpers (avoids import cycle).
func setupHubTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable" //nolint:gosec
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("hub_epoch_test: failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("hub_epoch_test: failed to ping database: %v", err)
	}

	// Run migrations using absolute path (robust across working directories)
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatalf("hub_epoch_test: could not create migration driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", hubTestMigrationsPath()),
		"postgres", driver,
	)
	if err != nil {
		t.Fatalf("hub_epoch_test: could not create migrate instance: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("hub_epoch_test: could not run migrations: %v", err)
	}

	t.Cleanup(func() {
		if _, err := db.Exec(`TRUNCATE users, dm_conversations, dm_participants, dm_channel_keys, dm_key_revocations, dm_messages CASCADE`); err != nil {
			t.Errorf("hub_epoch_test: failed to truncate tables: %v", err)
		}
		_ = db.Close()
	})
	return db
}

func setupHubTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://:concord_dev_redis@localhost:6379" //nolint:gosec
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("hub_epoch_test: failed to parse redis URL: %v", err)
	}
	// Use DB 1 for test isolation (matches testhelpers/testredis.go)
	opts.DB = 1
	client := redis.NewClient(opts)
	ctx := context.Background()
	require.NoError(t, client.Ping(ctx).Err(), "hub_epoch_test: failed to ping redis")
	require.NoError(t, client.FlushDB(ctx).Err(), "hub_epoch_test: failed to flush redis DB")
	t.Cleanup(func() {
		_ = client.FlushDB(ctx).Err()
		_ = client.Close()
	})
	return client
}

type hubTestSetup struct {
	hub    *Hub
	db     *sql.DB
	client *Client
	convID string
	user1  uuid.UUID
	user2  uuid.UUID
}

func setupEpochTest(t *testing.T, encrypted, seedKey, seedRevocation bool) *hubTestSetup {
	t.Helper()

	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)

	user1ID := uuid.New()
	user2ID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec

	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		user1ID.String(), "hubuser1@test.concord.chat", "hubuser1", hash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		user2ID.String(), "hubuser2@test.concord.chat", "hubuser2", hash)
	require.NoError(t, err)

	convUUID := uuid.New()
	convID := convUUID.String()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, is_encrypted, created_by) VALUES ($1, false, false, $2, $3)`,
		convID, encrypted, user1ID.String())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
		convID, user1ID.String(), user2ID.String())
	require.NoError(t, err)

	if seedKey {
		_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`,
			convID, user1ID.String(), []byte("test-key"))
		require.NoError(t, err)
	}

	if seedRevocation {
		_, err = db.Exec(`INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'test', $2)`,
			convID, user1ID.String())
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 2)`,
			convID, user1ID.String(), []byte("test-key-v2"))
		require.NoError(t, err)
	}

	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   user1ID,
		Username: "hubuser1",
		Send:     make(chan []byte, 10),
		Hub:      hub,
		Channels: make(map[uuid.UUID]bool),
	}

	hub.clients[clientID] = client
	hub.userClients[user1ID] = map[uuid.UUID]bool{clientID: true}
	hub.dmSubscriptions[convUUID] = map[uuid.UUID]bool{clientID: true}

	return &hubTestSetup{
		hub:    hub,
		db:     db,
		client: client,
		convID: convID,
		user1:  user1ID,
		user2:  user2ID,
	}
}

func readClientMsg(t *testing.T, client *Client) map[string]interface{} {
	t.Helper()
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		return msg
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for client message")
		return nil
	}
}

func TestHandleDMMessageCurrentEpochAccepted(t *testing.T) {
	setup := setupEpochTest(t, true, true, false)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:         "hello encrypted",
			keyIsEncrypted:    true,
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.NotEqual(t, "error", resp["type"], "message should be accepted, not an error")
}

func TestHandleDMMessageRevokedEpochRejected(t *testing.T) {
	setup := setupEpochTest(t, true, true, true)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:         "hello with revoked key",
			keyIsEncrypted:    true,
			"key_version":     float64(1),
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Equal(t, "epoch_revoked", data["code"])
	assert.Equal(t, float64(2), data["current_epoch"])
}

func TestHandleDMMessageEncryptedConvPlaintextRejected(t *testing.T) {
	setup := setupEpochTest(t, true, true, false)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:         "plaintext in encrypted conv",
			keyIsEncrypted:    false,
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data["message"], "encrypted")
}

func TestHandleDMMessageNotSubscribed(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	otherConvID := uuid.New().String()
	_, err := setup.db.Exec(
		`INSERT INTO dm_conversations (id, is_group, is_personal, is_encrypted, created_by) VALUES ($1, false, false, false, $2)`,
		otherConvID, setup.user1.String())
	require.NoError(t, err)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: otherConvID,
			keyContent:         "should fail",
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.Equal(t, "error", resp["type"])
	data := resp["data"].(map[string]interface{})
	assert.Contains(t, data["message"], "Not subscribed")
}

func TestHandleDMMessageUnencryptedConvPlaintextAccepted(t *testing.T) {
	setup := setupEpochTest(t, false, false, false)

	msg := IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:         "plaintext in unencrypted conv is fine",
			keyIsEncrypted:    false,
		},
	}

	setup.hub.handleDMMessage(msg)

	resp := readClientMsg(t, setup.client)
	assert.NotEqual(t, "error", resp["type"], "plaintext in unencrypted conversation should be accepted")
}
