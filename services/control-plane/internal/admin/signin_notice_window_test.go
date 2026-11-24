package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// The rows these tests write through RecordSignIn and WriteWith stay in the
// append-only log. They are inert for later tests: each names an admin that
// the test removes (a sign-in row's admin_id becomes NULL, and a success row's
// created_admin_id then matches no admin).

// signInResult is one RecordSignIn outcome.
type signInResult struct {
	n      int
	cursor int64
	read   bool
	err    error
}

func recordSignIn(ctx context.Context, audit *admin.AuditLog, adminID, sessionID string) signInResult {
	rec, err := audit.RecordSignIn(ctx, adminID, "viewer", sessionID)
	return signInResult{n: rec.Notice.AdminsAdded, cursor: rec.Notice.Cursor, read: rec.Shown, err: err}
}

// requireCountedIn records a sign-in of this session and requires a read
// count.
func requireCountedIn(t *testing.T, audit *admin.AuditLog, adminID, sessionID string) signInResult {
	t.Helper()
	r := recordSignIn(t.Context(), audit, adminID, sessionID)
	require.NoError(t, r.err)
	require.True(t, r.read, "the count must be read")
	return r
}

// requireCounted is requireCountedIn for a fresh session.
func requireCounted(t *testing.T, audit *admin.AuditLog, adminID string) int {
	t.Helper()
	return requireCountedIn(t, audit, adminID, uniqueAdminUsername("session")).n
}

// A creation transaction's ts is when it began, which can be long before it
// commits. A success row written after a sign-in is in the next window however
// old its ts is. With the window opened by time (ten seconds before the
// sign-in row) this row, an hour older than the sign-in, was never counted.
func TestAdminsAddedSinceLastSignIn_LateCommitWithOldTSIsCounted(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	signInRow(t, db, viewerID, "10 hours")
	username := uniqueAdminUsername("notice-late")
	createdRow(t, db, actingID, pendingAdminNamed(t, db, username), username, "9 hours")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A sign-in row written before this rule carries no count. Its window keeps
// the earlier ten-second overlap and adds every row written after it, so it
// can over-count but never miss.
func TestAdminsAddedSinceLastSignIn_LegacySignInRowOnlyWidens(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, nil, existingAdmin(t, db, "legacy-before"), admin.EventAdminCreated, admin.AuditSuccess, "11 hours -1 minute")
	insertAuditRow(t, db, nil, existingAdmin(t, db, "legacy-overlap"), admin.EventAdminCreated, admin.AuditSuccess, "11 hours -5 seconds")
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "11 hours")
	insertAuditRow(t, db, nil, existingAdmin(t, db, "legacy-late"), admin.EventAdminCreated, admin.AuditSuccess, "10 hours")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the overlap row and the late row, not the one a minute before")
}

// A sign-in whose count was not read, or whose nonzero count was never
// acknowledged, moves no window: the next count runs from the last
// acknowledged cursor.
func TestAdminsAddedSinceLastSignIn_UnacknowledgedSignInEndsNoWindow(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	unread := map[string]any{"notice_session": "unread"}
	// Only unread sign-ins: nothing was ever shown, so every creation counts.
	insertAuditRow(t, db, nil, existingAdmin(t, db, "unread-first"), admin.EventAdminCreated, admin.AuditSuccess, "12 hours")
	insertAuditRowJSON(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "12 hours 1 minute", unread)
	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	signInRow(t, db, viewerID, "12 hours 2 minutes")
	insertAuditRow(t, db, nil, existingAdmin(t, db, "unread-between"), admin.EventAdminCreated, admin.AuditSuccess, "12 hours 3 minutes")
	insertAuditRowJSON(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "12 hours 4 minutes", unread)
	insertAuditRowJSON(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "12 hours 5 minutes",
		map[string]any{"notice_session": "shown-never-acked", "admins_added": 1, "notice_cursor": maxAuditRowID(t, db)})
	n, err = admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the creation after the last zero count, though unread and unacknowledged sign-ins followed it")
}

// RecordSignIn counts under the exclusive notice lock, so it waits for a
// creation that holds the lock shared, and that creation is counted at this
// sign-in. Without the lock the sign-in counted 0 while the creation was in
// flight, and the creation, whose row id is below the sign-in's, was not in
// the next window either.
func TestRecordSignIn_WaitsForInFlightCreation(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	audit := admin.NewAuditLog(db)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	require.Equal(t, 0, requireCounted(t, audit, viewerID), "first sign-in")

	createdID := pendingAdminNamed(t, db, uniqueAdminUsername("in-flight"))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock_shared($1)`, admin.SignInNoticeLockKeyForTest)
	require.NoError(t, err)
	detail, err := json.Marshal(map[string]any{"created_admin_id": createdID})
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO admin_audit_log (event_type, result, detail) VALUES ($1, $2, $3)`,
		admin.EventAdminCreated, admin.AuditSuccess, detail)
	require.NoError(t, err)

	done := make(chan signInResult, 1)
	go func() { done <- recordSignIn(context.Background(), audit, viewerID, "in-flight-session") }()
	select {
	case r := <-done:
		assert.Fail(t, "the sign-in counted while a creation held the lock", "count %d", r.n)
		done <- r
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())

	var during signInResult
	select {
	case during = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the sign-in did not finish after the creation committed")
	}
	require.NoError(t, during.err)
	require.True(t, during.read)
	require.NoError(t, audit.AcknowledgeSignInNotice(t.Context(), viewerID, "in-flight-session", during.cursor))
	after := requireCounted(t, audit, viewerID)
	assert.Equal(t, 1, during.n, "counted at the sign-in that waited for it")
	assert.Equal(t, 0, after, "and only there")
}

// WriteWith takes the notice lock shared for an admin_created success, so it
// waits while a sign-in holds it exclusive; other rows do not wait.
func TestWriteWith_AdminCreatedSuccessWaitsForSignIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	audit := admin.NewAuditLog(db)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, admin.SignInNoticeLockKeyForTest)
	require.NoError(t, err)

	detail := map[string]any{"created_admin_id": goneAdminID}
	require.NoError(t, audit.Write(t.Context(), admin.AuditEvent{
		EventType: admin.EventAdminCreated, Result: admin.AuditDenied, Detail: detail,
	}), "a refusal does not wait")

	done := make(chan error, 1)
	go func() {
		done <- audit.Write(context.Background(), admin.AuditEvent{
			EventType: admin.EventAdminCreated, Result: admin.AuditSuccess, Detail: detail,
		})
	}()
	select {
	case err := <-done:
		assert.Fail(t, "the success row was written while a sign-in held the lock", "err %v", err)
		done <- err
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the success row was not written after the sign-in committed")
	}
}

// A sign-in whose count could not be read still signs in, and the creations
// it could not show are shown at the next sign-in. Before, that sign-in still
// ended the window, so they were never shown.
func TestWebAuthnLogin_NoticeReadFailure_NextSignInStillReports(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)
	h := faultHandler(t, rdb, dbFault{statement: admin.AdminsAddedQueryMarkerForTest})
	faulty := gin.New()
	faulty.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	faulty.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)

	code, body, _ := signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, 0, decodeSignIn(t, body))

	// ts a minute in the past, so no ten-second overlap could pick it up.
	insertAuditRow(t, db, nil, existingAdmin(t, db, "unread-signin"), admin.EventAdminCreated, admin.AuditSuccess, "-1 minute")

	code, body, cookie := signInAdmin(t, faulty, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotEmpty(t, cookie)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotContains(t, raw, "admins_added")

	var unread int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM admin_audit_log
		 WHERE admin_id = $1 AND event_type = $2 AND result = $3 AND detail ? 'notice_session' AND NOT detail ? 'notice_cursor'`,
		viewerID, admin.EventLoginSuccess, admin.AuditSuccess,
	).Scan(&unread))
	assert.Equal(t, 1, unread, "the sign-in is still audited")

	code, body, _ = signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	assert.Equal(t, 1, decodeSignIn(t, body), "the creation the failed read could not show")
}
