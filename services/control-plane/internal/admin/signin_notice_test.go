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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// insertAuditRow writes one audit row at now() + offset and removes it when the
// test ends. Future offsets keep the rows clear of the ones other tests write
// at now(), so the counts below are exact on a shared database.
func insertAuditRow(t *testing.T, db *sql.DB, adminID *string, event, result, offset string) {
	t.Helper()
	var id int64
	require.NoError(t, db.QueryRow(
		`INSERT INTO admin_audit_log (ts, admin_id, actor, event_type, result)
		 VALUES (now() + $1::interval, $2, 'notice-test', $3, $4) RETURNING id`,
		offset, adminID, event, result,
	).Scan(&id))
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE id = $1`, id)
		assert.NoError(t, err)
	})
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
	insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditSuccess, "2 hours")

	n, err := audit.AdminsAddedSinceLastSignIn(ctx, adminID)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no previous sign-in: nothing to report")

	insertAuditRow(t, db, &adminID, admin.EventLoginSuccess, admin.AuditSuccess, "1 hour")
	insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditSuccess, "1 hour -1 minute")     // before the window
	insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditSuccess, "1 hour -5 seconds")    // inside the 10 s overlap
	insertAuditRow(t, db, &adminID, admin.EventAdminCreated, admin.AuditSuccess, "1 hour 1 minute") // this admin's own
	insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditDenied, "1 hour 1 minute")
	insertAuditRow(t, db, nil, admin.EventLoginSuccess, admin.AuditSuccess, "1 hour 1 minute")

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
	insertAuditRow(t, db, &adminID, admin.EventLoginSuccess, admin.AuditSuccess, "3 hours")
	for range admin.MaxAdminsAddedNotice + 1 {
		insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditSuccess, "4 hours")
	}

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(context.Background(), adminID)
	require.NoError(t, err)
	assert.Equal(t, admin.MaxAdminsAddedNotice, n)
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

	insertAuditRow(t, db, nil, admin.EventAdminCreated, admin.AuditSuccess, "1 minute")

	code, body, cookie = signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotEmpty(t, cookie)
	// At least the row above; a creation another test committed in the ten
	// seconds before the first sign-in may be counted too.
	assert.GreaterOrEqual(t, decodeSignIn(t, body), 1)
}
