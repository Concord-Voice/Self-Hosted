package testhelpers

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/api"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// TestServer wraps a fully-wired Gin router with a real DB and Redis for integration testing.
type TestServer struct {
	Router *gin.Engine
	Hub    *websocket.Hub
	DB     *sql.DB
	Redis  *redis.Client
}

// SetupTestServer creates a test server backed by a real database and Redis.
func SetupTestServer(t *testing.T) *TestServer {
	t.Helper()

	db, dbCleanup := SetupTestDB(t)
	redisClient, redisCleanup := SetupTestRedis(t)

	cfg := &config.Config{
		Environment:       "test",
		Port:              "0",
		JWTSecret:         TestJWTSecret,
		AllowedOrigins:    []string{"*"},
		MFAEncryptionKey:  "0000000000000000000000000000000000000000000000000000000000000000",
		WebAuthnRPID:      "localhost",
		WebAuthnRPOrigins: []string{"http://localhost:3001"},
	}

	log := logger.New("test")

	router, hub, natsClient := api.NewRouter(db, redisClient, cfg, log)
	if natsClient != nil {
		t.Cleanup(func() { natsClient.Close() })
	}

	t.Cleanup(func() {
		redisCleanup()
		dbCleanup()
	})

	return &TestServer{
		Router: router,
		Hub:    hub,
		DB:     db,
		Redis:  redisClient,
	}
}

// AuthHeaders returns an Authorization header map for the given user.
func AuthHeaders(accessToken string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	return h
}

// CreateTestUser inserts a user directly into the database and returns a TestUser
// with a valid JWT access token. Uses a pre-computed password hash for speed.
func (ts *TestServer) CreateTestUser(t *testing.T, username string) TestUser {
	t.Helper()

	userID := uuid.New().String()
	email := username + "@test.concord.chat"
	normalizedUsername := auth.NormalizeUsername(username)

	// Insert user with pre-computed hash
	_, err := ts.DB.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, e2ee_preference)
		 VALUES ($1, $2, $3, $4, true, false)`,
		userID, email, normalizedUsername, TestPasswordHash,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test user: %v", err)
	}

	// Insert E2EE keys
	pubKey, wrappedKey, saltB64 := E2EETestKeys()
	pubKeyBytes, _ := base64.StdEncoding.DecodeString(pubKey)
	wrappedKeyBytes, _ := base64.StdEncoding.DecodeString(wrappedKey)
	saltBytes, _ := base64.StdEncoding.DecodeString(saltB64)

	_, err = ts.DB.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt) VALUES ($1, $2, $3)`,
		userID, wrappedKeyBytes, saltBytes,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create user keys: %v", err)
	}

	keyID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO public_keys (id, user_id, public_key, key_version) VALUES ($1, $2, $3, 1)`,
		keyID, userID, pubKeyBytes,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create public key: %v", err)
	}

	// Generate access token
	accessToken, err := auth.GenerateAccessToken(userID, TestJWTSecret)
	if err != nil {
		t.Fatalf("testhelpers: failed to generate access token: %v", err)
	}

	return TestUser{
		ID:          userID,
		Email:       email,
		Username:    normalizedUsername,
		Password:    TestPassword,
		AccessToken: accessToken,
	}
}

// CreateTestServer creates a server owned by the given user and returns its ID.
func (ts *TestServer) CreateTestServer(t *testing.T, ownerID, name string) string {
	t.Helper()

	serverID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO servers (id, name, owner_id, e2ee_default) VALUES ($1, $2, $3, false)`,
		serverID, name, ownerID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test server: %v", err)
	}

	_, err = ts.DB.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		serverID, ownerID,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to add owner to server: %v", err)
	}

	return serverID
}

// CreateTestChannel creates a channel in the given server and returns its ID.
func (ts *TestServer) CreateTestChannel(t *testing.T, serverID, name string, isEncrypted bool) string {
	t.Helper()

	channelID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channels (id, server_id, name, type, is_encrypted) VALUES ($1, $2, $3, 'text', $4)`,
		channelID, serverID, name, isEncrypted,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to create test channel: %v", err)
	}

	return channelID
}

// AddMemberToServer adds a user to a server with the given role.
func (ts *TestServer) AddMemberToServer(t *testing.T, serverID, userID, role string) {
	t.Helper()

	_, err := ts.DB.Exec(
		`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, $3)`,
		serverID, userID, role,
	)
	if err != nil {
		t.Fatalf("testhelpers: failed to add member to server: %v", err)
	}
}

// DoRequest performs an HTTP request against the test router and returns the response.
func (ts *TestServer) DoRequest(method, path string, body interface{}, headers http.Header) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	if headers != nil {
		req.Header = headers
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

// ParseJSON parses the response body into the given target.
func ParseJSON(t *testing.T, w *httptest.ResponseRecorder, target interface{}) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(target); err != nil {
		t.Fatalf("testhelpers: failed to parse JSON response: %v", err)
	}
}
