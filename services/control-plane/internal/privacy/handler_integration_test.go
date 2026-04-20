// services/control-plane/internal/privacy/handler_integration_test.go

package privacy_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// seedUser inserts a bare-minimum user row and returns its generated id.
func seedUser(t *testing.T, db *sql.DB, username string) string {
	t.Helper()
	email := fmt.Sprintf("%s@test.local", username)
	stmt, err := db.Prepare(
		`INSERT INTO users (username, email, password_hash)
		 VALUES ($1, $2, $3) RETURNING id`,
	)
	require.NoError(t, err)
	defer stmt.Close() //nolint:errcheck
	var id string
	require.NoError(t, stmt.QueryRow(username, email, "not-a-real-hash").Scan(&id))
	return id
}

// seedRefreshToken inserts a refresh-token row owned by userID.
func seedRefreshToken(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	tokenHash := fmt.Sprintf("hash-%s", userID)
	stmt, err := db.Prepare(
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		 VALUES ($1, $2, NOW() + INTERVAL '30 days')`,
	)
	require.NoError(t, err)
	defer stmt.Close() //nolint:errcheck
	_, err = stmt.Exec(userID, tokenHash)
	require.NoError(t, err)
}

// seedKeyRevocation seeds a key_revocations row whose revoked_by references
// userID, in a channel owned by a BYSTANDER user. Using a bystander owner is
// intentional: servers.owner_id and channels.server_id both cascade, so if
// the target user owned the server, deleting them would cascade the server
// away, taking the channel and the key_revocations row with it — and the
// SET NULL on revoked_by would never fire (there's no row left to update).
// With the bystander owning the server, only the revoked_by column NULLs
// when the target user is deleted, which is the invariant the migration fix
// guarantees.
func seedKeyRevocation(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	bystanderID := seedUser(t, db, "bystander-"+userID[:8])
	serverName := fmt.Sprintf("test-server-%s", userID[:8])

	srvStmt, err := db.Prepare(
		`INSERT INTO servers (name, owner_id) VALUES ($1, $2) RETURNING id`,
	)
	require.NoError(t, err)
	defer srvStmt.Close() //nolint:errcheck
	var serverID string
	require.NoError(t, srvStmt.QueryRow(serverName, bystanderID).Scan(&serverID))

	chStmt, err := db.Prepare(
		`INSERT INTO channels (server_id, name, type) VALUES ($1, $2, 'text') RETURNING id`,
	)
	require.NoError(t, err)
	defer chStmt.Close() //nolint:errcheck
	var channelID string
	require.NoError(t, chStmt.QueryRow(serverID, "general").Scan(&channelID))

	krStmt, err := db.Prepare(
		`INSERT INTO key_revocations
		 (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 1, 2, 'test', $2)`,
	)
	require.NoError(t, err)
	defer krStmt.Close() //nolint:errcheck
	_, err = krStmt.Exec(channelID, userID)
	require.NoError(t, err)
}

func buildRouter(t *testing.T, db *sql.DB, adminStub privacy.EventsDeleter, log *logger.Logger) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	acct := users.NewAccountService(db, log)
	h := privacy.NewHandler(adminStub, acct, log)
	router.POST(testEraseAccount, wrapWithUserFromCtxHeader(h.EraseAccount))
	return router
}

func wrapWithUserFromCtxHeader(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if uid := c.GetHeader("X-Test-User-ID"); uid != "" {
			c.Set("user_id", uid)
		}
		next(c)
	}
}

func postEraseAccount(t *testing.T, router *gin.Engine, userID string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, testEraseAccount, bytes.NewReader(buf))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	req.Header.Set("X-Test-User-ID", userID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestEraseAccount_FullErasureWithClientId(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-happy")
	seedRefreshToken(t, db, userID)

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{"clientId": testValidClientID})

	require.Equal(t, http.StatusNoContent, rec.Code)

	// User row gone
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "user row must be deleted")

	// Refresh token cascaded away
	err = db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "refresh_tokens must cascade-delete")

	// Audit row present with user_id NULL and sentry_delete_attempted TRUE
	var sentryAttempted bool
	var auditUserID sql.NullString
	err = db.QueryRow(
		`SELECT user_id, sentry_delete_attempted FROM account_deletions
		 ORDER BY deleted_at DESC LIMIT 1`,
	).Scan(&auditUserID, &sentryAttempted)
	require.NoError(t, err)
	assert.False(t, auditUserID.Valid, "audit row user_id must be NULL post-commit")
	assert.True(t, sentryAttempted, "audit row must record Sentry attempt")
	assert.Equal(t, testValidClientID, admin.deleted, "Sentry stub must have received the clientId")
}

func TestEraseAccount_NoClientId_SkipsSentry(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-no-clientid")

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{})

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "", admin.deleted, "Sentry stub must NOT have been called")

	var sentryAttempted bool
	err := db.QueryRow(
		`SELECT sentry_delete_attempted FROM account_deletions
		 ORDER BY deleted_at DESC LIMIT 1`,
	).Scan(&sentryAttempted)
	require.NoError(t, err)
	assert.False(t, sentryAttempted, "audit row must record no Sentry attempt")
}

func TestEraseAccount_InvalidClientIdShape_400(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-bad-clientid")

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{"clientId": "not-hex!"})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "", admin.deleted, "Sentry stub must NOT have been called")

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "user row must still exist after 400")

	err = db.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no audit row on 400")
}

func TestEraseAccount_SentryFails_502_NoDbDelete(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-sentry-fail")

	admin := &stubEventsDeleter{fail: true}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{"clientId": testValidClientID})

	// Watch item: if #672 merges first, change this to StatusInternalServerError.
	require.Equal(t, http.StatusBadGateway, rec.Code)

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "user row must still exist after Sentry failure")

	err = db.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no audit row on Sentry failure")
}

func TestEraseAccount_DbFails_500(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-db-fail")

	// Force a failure by renaming the audit table so the INSERT inside
	// DeleteAccount fails with "relation does not exist" and the
	// transaction rolls back. Rename preserves all attached indexes and
	// constraints, so restoring state in defer needs only the reverse
	// rename — no schema re-creation required.
	//
	// Note: defer is used rather than t.Cleanup so the reverse-rename runs
	// BEFORE the SetupTestDB cleanup (which calls TruncateAllTables and
	// would otherwise fail on the renamed table). The trade-off is that a
	// panic between the rename and the deferred reverse-rename would leave
	// the table renamed; given the tests run sequentially under -p 1 and
	// the only way to panic here is a connection-level failure (in which
	// case the whole suite is compromised anyway), this trade-off is
	// acceptable.
	_, err := db.Exec(`ALTER TABLE account_deletions RENAME TO account_deletions_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		_, _ = db.Exec(`ALTER TABLE account_deletions_hidden_for_test RENAME TO account_deletions`)
	}()

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{})

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "user row must still exist after DB failure (tx rolled back)")
}

func TestEraseAccount_UserNotFound_404(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-idempotent")

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))

	// First call succeeds with 204.
	rec1 := postEraseAccount(t, router, userID, map[string]string{})
	require.Equal(t, http.StatusNoContent, rec1.Code)

	// Second call targets the same (now-gone) user — expect 404.
	rec2 := postEraseAccount(t, router, userID, map[string]string{})
	require.Equal(t, http.StatusNotFound, rec2.Code)

	// Only one audit row should exist (the first successful deletion).
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "second call must not leave an audit row")
}

func TestEraseAccount_KeyRevocationsCascadeUnblock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-keyrev")
	seedKeyRevocation(t, db, userID)

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, logger.New("test"))
	rec := postEraseAccount(t, router, userID, map[string]string{})

	require.Equal(t, http.StatusNoContent, rec.Code, "must succeed — migration 000059 made this cascade unblock")

	var userCount int
	err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = $1`, userID).Scan(&userCount)
	require.NoError(t, err)
	assert.Equal(t, 0, userCount)

	// key_revocations row persists with revoked_by NULL (ON DELETE SET NULL)
	var revokedBy sql.NullString
	err = db.QueryRow(
		`SELECT revoked_by FROM key_revocations
		 WHERE reason = 'test' ORDER BY revoked_at DESC LIMIT 1`,
	).Scan(&revokedBy)
	require.NoError(t, err)
	assert.False(t, revokedBy.Valid, "revoked_by must be NULL after user deleted")
}

// TestEraseAccount_NoClientIdLeak is a forward-trip-wire: the happy path is
// currently silent (no log line is emitted on success), so the
// Contains-false assertion passes trivially TODAY. The test exists to fail
// the instant someone adds a log line on the happy path that accidentally
// includes the clientId value — matching the #642 discipline invariant.
// The negative-logging path (handler's logUpstreamFailure) is separately
// covered by TestEraseAccount_SentryFailure_ClientIdScrubbed.
func TestEraseAccount_NoClientIdLeak(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-no-leak")

	// Capture the logger output into a buffer.
	var logBuf bytes.Buffer
	testLog := logger.NewWithWriter(&logBuf)

	admin := &stubEventsDeleter{}
	router := buildRouter(t, db, admin, testLog)
	rec := postEraseAccount(t, router, userID, map[string]string{"clientId": testValidClientID})

	require.Equal(t, http.StatusNoContent, rec.Code)

	// Regression-lock: the raw 32-hex clientId must NEVER appear in log output.
	logged := logBuf.String()
	assert.False(t, strings.Contains(logged, testValidClientID),
		"clientId must not appear in logs — regression-lock for #642 discipline. Logs: %q", logged)
}

func TestEraseAccount_SentryFailure_ClientIdScrubbed(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := seedUser(t, db, "erasure-scrub")

	// Use the existing clientIDLeakingDeleter from handler_test.go to simulate
	// an upstream that accidentally returns the clientId in an error string.
	admin := &clientIDLeakingDeleter{clientID: testValidClientID}
	var logBuf bytes.Buffer
	testLog := logger.NewWithWriter(&logBuf)

	router := buildRouter(t, db, admin, testLog)
	rec := postEraseAccount(t, router, userID, map[string]string{"clientId": testValidClientID})

	require.Equal(t, http.StatusBadGateway, rec.Code)

	// Even on failure, the scrub in logUpstreamFailure must have removed the clientId.
	assert.False(t, strings.Contains(logBuf.String(), testValidClientID),
		"clientId must be scrubbed from upstream-failure log lines")
	assert.Contains(t, logBuf.String(), "<redacted-client-id>")
}
