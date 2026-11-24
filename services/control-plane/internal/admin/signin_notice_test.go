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
	var detail map[string]any
	if reason != "" {
		detail = map[string]any{"reason": reason}
	}
	insertAuditRowJSON(t, db, adminID, actor, event, result, offset, detail)
}

// insertAuditRowJSON is insertAuditRow with any detail object (nil for none).
func insertAuditRowJSON(t *testing.T, db *sql.DB, adminID *string, actor, event, result, offset string, detail map[string]any) {
	t.Helper()
	var raw any
	if detail != nil {
		b, err := json.Marshal(detail)
		require.NoError(t, err)
		raw = b
	}
	var id int64
	require.NoError(t, db.QueryRow(
		`INSERT INTO admin_audit_log (ts, admin_id, actor, event_type, result, detail)
		 VALUES (now() + $1::interval, $2, $3, $4, $5, $6) RETURNING id`,
		offset, adminID, actor, event, result, raw,
	).Scan(&id))
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE id = $1`, id)
		assert.NoError(t, err)
	})
}

// createdRow writes an admin_created success row as CreateAdmin now does: it
// carries the created admin's id in detail.created_admin_id.
func createdRow(t *testing.T, db *sql.DB, actingID, createdID, username, offset string) {
	t.Helper()
	insertAuditRowJSON(t, db, &actingID, username, admin.EventAdminCreated, admin.AuditSuccess, offset,
		map[string]any{"created_admin_id": createdID})
}

// goneAdminID is a well-formed id that names no admin: the admin a mint
// failure removed.
const goneAdminID = "00000000-0000-4000-8000-000000000394"

// pendingAdminNamed creates a pending admin row with this username, as
// CreateAdmin does, removes it when the test ends, and returns its id.
func pendingAdminNamed(t *testing.T, db *sql.DB, username string) string {
	t.Helper()
	created, err := admin.NewAdminRepo(db).CreatePending(t.Context(), username, "unused-hash")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	return created.ID
}

// existingAdmin creates a pending admin with a unique username and returns it.
func existingAdmin(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	username := uniqueAdminUsername(prefix)
	pendingAdminNamed(t, db, username)
	return username
}

// signInRow writes a login_success row as RecordSignIn does when it reads the
// count: it records the count it showed, so it ends the window exactly.
func signInRow(t *testing.T, db *sql.DB, adminID, offset string) {
	t.Helper()
	insertAuditRowJSON(t, db, &adminID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, offset,
		map[string]any{"admins_added": 0})
}

// The count covers admin_created successes by any admin written after this
// admin's previous sign-in, ignores refusals and other events, and is 0
// before the first sign-in. "After" is row order, not ts: a row written
// before the sign-in was shown then, whatever its ts.
func TestAdminsAddedSinceLastSignIn_CountsCreationsSincePreviousSignIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	adminID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-shown"), admin.EventAdminCreated, admin.AuditSuccess, "2 hours")

	n, err := audit.AdminsAddedSinceLastSignIn(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no previous sign-in: nothing to report")

	signInRow(t, db, adminID, "1 hour")
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-after"), admin.EventAdminCreated, admin.AuditSuccess, "1 hour 1 minute")
	insertAuditRow(t, db, &adminID, existingAdmin(t, db, "notice-own"), admin.EventAdminCreated, admin.AuditSuccess, "1 hour 1 minute") // this admin's own
	insertAuditRow(t, db, nil, existingAdmin(t, db, "notice-denied"), admin.EventAdminCreated, admin.AuditDenied, "1 hour 1 minute")
	insertAuditRow(t, db, nil, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "1 hour 1 minute")

	n, err = audit.AdminsAddedSinceLastSignIn(ctx, adminID)
	require.NoError(t, err)
	// The "2 hours" row was written before the sign-in, so that sign-in
	// already counted it.
	assert.Equal(t, 2, n)
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
// mint fails: the committed admin_created success naming the created admin's
// id, then the failure that records why the creation did not complete.
func mintFailureRows(t *testing.T, db *sql.DB, actingID, createdID, username, offset, reason string) {
	t.Helper()
	createdRow(t, db, actingID, createdID, username, offset)
	insertAuditRowDetail(t, db, &actingID, username, admin.EventAdminCreated, admin.AuditFailure, offset+" 1 second", reason)
}

// A mint failure removes the pending admin, so its committed success row is
// not counted, and the successful retry of the same username is one admin,
// not two.
func TestAdminsAddedSinceLastSignIn_MintFailureThenRetry_CountsOnce(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "5 hours")
	username := uniqueAdminUsername("notice-retry")
	mintFailureRows(t, db, actingID, goneAdminID, username, "5 hours 1 minute", "token_mint_failed")
	// The retry commits the same username again, and this time the row stays.
	createdRow(t, db, actingID, pendingAdminNamed(t, db, username), username, "5 hours 2 minutes")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A mint failure with no retry leaves no admin, so nothing was added.
func TestAdminsAddedSinceLastSignIn_MintFailureNoRetry_CountsNothing(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "6 hours")
	mintFailureRows(t, db, actingID, goneAdminID, uniqueAdminUsername("notice-gone"), "6 hours 1 minute", "token_mint_failed")

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
	username := uniqueAdminUsername("notice-kept")
	mintFailureRows(t, db, actingID, pendingAdminNamed(t, db, username), username, "7 hours 1 minute", "token_mint_failed_row_kept")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// Two success rows naming the same created admin are one admin.
func TestAdminsAddedSinceLastSignIn_SameCreatedAdminCountsOnce(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "8 hours")
	username := uniqueAdminUsername("notice-dup")
	createdID := pendingAdminNamed(t, db, username)
	createdRow(t, db, actingID, createdID, username, "8 hours 1 minute")
	createdRow(t, db, actingID, createdID, username, "8 hours 2 minutes")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A success row written before the created admin's id was recorded, or one
// whose id is not a well-formed admin id, cannot be matched to an admin, so
// each counts as one: an old or malformed row can over-count but never hide
// an admin. Its actor (here a name that matches no admin) is not consulted.
func TestAdminsAddedSinceLastSignIn_RowWithoutCreatedIDCountsOne(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "9 hours")
	insertAuditRow(t, db, &actingID, uniqueAdminUsername("notice-old"), admin.EventAdminCreated, admin.AuditSuccess, "9 hours 1 minute")

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a pre-change row without an id")

	insertAuditRowJSON(t, db, &actingID, "", admin.EventAdminCreated, admin.AuditSuccess, "9 hours 2 minutes",
		map[string]any{"created_admin_id": "not-an-id"})
	insertAuditRowJSON(t, db, &actingID, "", admin.EventAdminCreated, admin.AuditSuccess, "9 hours 3 minutes",
		map[string]any{"created_admin_id": 7})
	n, err = admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "malformed ids count one each")
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
