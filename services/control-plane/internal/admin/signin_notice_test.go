package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// insertAuditRow writes one audit row at now() + offset and removes it when the
// test ends. Future offsets keep the rows clear of the ones other tests write
// at now(), so the counts below are exact on a shared database. actor is the
// username the row names: for admin_created, the admin that was created.
func insertAuditRow(t *testing.T, db *sql.DB, adminID *string, actor, event, result, offset string) {
	t.Helper()
	insertAuditRowDetail(t, db, adminID, actor, event, result, offset, "")
}

// insertAuditRowDetail is insertAuditRow with a detail reason ("" for none).
func insertAuditRowDetail(t *testing.T, db *sql.DB, adminID *string, actor, event, result, offset, reason string) {
	t.Helper()
	var detail any
	if reason != "" {
		b, err := json.Marshal(map[string]string{"reason": reason})
		require.NoError(t, err)
		detail = b
	}
	var id int64
	require.NoError(t, db.QueryRow(
		`INSERT INTO admin_audit_log (ts, admin_id, actor, event_type, result, detail)
		 VALUES (now() + $1::interval, $2, $3, $4, $5, $6) RETURNING id`,
		offset, adminID, actor, event, result, detail,
	).Scan(&id))
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE id = $1`, id)
		assert.NoError(t, err)
	})
}

// pendingAdminNamed creates a pending admin row with this username, as
// CreateAdmin does, and removes it when the test ends.
func pendingAdminNamed(t *testing.T, db *sql.DB, username string) {
	t.Helper()
	created, err := admin.NewAdminRepo(db).CreatePending(t.Context(), username, "unused-hash")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
}

// existingAdmin creates a pending admin with a unique username and returns it.
func existingAdmin(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	username := uniqueAdminUsername(prefix)
	pendingAdminNamed(t, db, username)
	return username
}

// The count covers admin_created successes by any admin since this admin's
// previous sign-in, opens ten seconds early, ignores refusals and other events,
// and is 0 before the first sign-in.
func TestAdminsAddedSinceLastSignIn_CountsCreationsSincePreviousSignIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	adminID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-later"), admin.EventAdminCreated, admin.AuditSuccess, "2 hours")

	n, err := audit.AdminsAddedSinceLastSignIn(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no previous sign-in: nothing to report")

	insertAuditRow(t, db, &adminID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "1 hour")
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-before"), admin.EventAdminCreated, admin.AuditSuccess, "1 hour -1 minute")   // before the window
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-overlap"), admin.EventAdminCreated, admin.AuditSuccess, "1 hour -5 seconds") // inside the 10 s overlap
	insertAuditRow(t, db, &adminID, existingAdmin(t, db, "notice-own"), admin.EventAdminCreated, admin.AuditSuccess, "1 hour 1 minute")  // this admin's own
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-denied"), admin.EventAdminCreated, admin.AuditDenied, "1 hour 1 minute")
	insertAuditRow(t, db, nil, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "1 hour 1 minute")

	n, err = audit.AdminsAddedSinceLastSignIn(ctx, adminID)
	require.NoError(t, err)
	// The "2 hours" row is after the sign-in too.
	assert.Equal(t, 3, n)
}

func TestAdminsAddedSinceLastSignIn_IsCapped(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &adminID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "3 hours")
	for range admin.MaxAdminsAddedNotice + 1 {
		insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-cap"), admin.EventAdminCreated, admin.AuditSuccess, "4 hours")
	}

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(context.Background(), adminID)
	require.NoError(t, err)
	assert.Equal(t, admin.MaxAdminsAddedNotice, n)
}

// mintFailureRows writes the rows CreateAdmin leaves when the enrollment-token
// mint fails: the committed admin_created success, then the failure that
// records why the creation did not complete.
func mintFailureRows(t *testing.T, db *sql.DB, actingID, username, offset, reason string) {
	t.Helper()
	insertAuditRow(t, db, &actingID, username, admin.EventAdminCreated, admin.AuditSuccess, offset)
	insertAuditRowDetail(t, db, &actingID, username, admin.EventAdminCreated, admin.AuditFailure, offset+" 1 second", reason)
}

// A mint failure removes the pending admin, so its committed success row is
// not counted, and the successful retry of the same username is one admin,
// not two. The previous count reported 2 here.
func TestAdminsAddedSinceLastSignIn_MintFailureThenRetry_CountsOnce(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "5 hours")
	username := uniqueAdminUsername("notice-retry")
	mintFailureRows(t, db, actingID, username, "5 hours 1 minute", "token_mint_failed")
	// The retry commits the same username again, and this time the row stays.
	pendingAdminNamed(t, db, username)
	insertAuditRow(t, db, &actingID, username, admin.EventAdminCreated, admin.AuditSuccess, "5 hours 2 minutes")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A mint failure with no retry leaves no admin, so nothing was added. The
// previous count reported 1 here.
func TestAdminsAddedSinceLastSignIn_MintFailureNoRetry_CountsNothing(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "6 hours")
	mintFailureRows(t, db, actingID, uniqueAdminUsername("notice-gone"), "6 hours 1 minute", "token_mint_failed")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// When the cleanup after a mint failure could not remove the pending admin,
// the admin still exists (reset-enrollment can recover it), so it is counted.
func TestAdminsAddedSinceLastSignIn_MintFailureRowKept_CountsTheAdmin(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "7 hours")
	mintFailureRows(t, db, actingID, existingAdmin(t, db, "notice-kept"), "7 hours 1 minute", "token_mint_failed_row_kept")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestAdminsAddedSinceLastSignIn_DBErrorIsReturned(t *testing.T) {
	ownDB, err := sql.Open("postgres", defaultTestDBURL())
	require.NoError(t, err)
	require.NoError(t, ownDB.Close())

	_, err = admin.NewAuditLog(ownDB).AdminsAddedSinceLastSignIn(context.Background(), "00000000-0000-0000-0000-000000000000")
	require.Error(t, err, "a failed read must not be reported as zero admins added")
}

// signInAdmin runs both sign-in steps and returns the WebAuthn step's recorder.
func signInAdmin(t *testing.T, engine *gin.Engine, username string, va *virtualAuthenticator) (int, []byte, string) {
	t.Helper()
	pwRec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{
		"username": username,
		"password": adminTestPassword,
	})
	require.Equal(t, http.StatusOK, pwRec.Code, pwRec.Body.String())
	var pwResp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(pwRec.Body.Bytes(), &pwResp))

	va.signCount++
	assertion := va.assertionResponse(t, testAdminRPID, pwResp.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)
	rec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{
		"handle":    pwResp.Handle,
		"assertion": json.RawMessage(assertion),
	})
	return rec.Code, rec.Body.Bytes(), rec.Header().Get("Set-Cookie")
}

// decodeSignIn decodes the sign-in success body and requires exactly the two
// fields the portal accepts.
func decodeSignIn(t *testing.T, body []byte) int {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw, 2, string(body))
	var resp struct {
		Status      string `json:"status"`
		AdminsAdded *int   `json:"admins_added"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Equal(t, "authenticated", resp.Status)
	require.NotNil(t, resp.AdminsAdded, "admins_added is required")
	return *resp.AdminsAdded
}

// A sign-in reports the admins created in the console since the previous one.
// Without the notice this test fails: the second sign-in would say nothing.
func TestWebAuthnLogin_ReportsAdminsAddedSincePreviousSignIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	engine := adminAuthEngine(t, db, rdb)

	code, body, cookie := signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotEmpty(t, cookie)
	assert.Equal(t, 0, decodeSignIn(t, body), "first sign-in has no previous one")

	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-signin"), admin.EventAdminCreated, admin.AuditSuccess, "1 minute")

	code, body, cookie = signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotEmpty(t, cookie)
	// At least the row above; a creation another test committed in the ten
	// seconds before the first sign-in may be counted too.
	assert.GreaterOrEqual(t, decodeSignIn(t, body), 1)
}
