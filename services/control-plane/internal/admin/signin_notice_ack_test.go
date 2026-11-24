package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

const noticeAckPath = "/admin/api/v1/signin-notice/ack"

// noticeEngine serves the two sign-in steps and the gated acknowledgement
// route, as RegisterRoutes mounts them.
func noticeEngine(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)
	gated := r.Group("/admin/api/v1", admin.AdminAuthRequired(admin.NewSessionStore(rdb, time.Now), admin.NewAdminRepo(db)))
	gated.POST("/signin-notice/ack", h.AcknowledgeSignInNotice)
	return r
}

// postAck sends an acknowledgement with the session cookie from a sign-in's
// Set-Cookie header.
func postAck(engine *gin.Engine, setCookie, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, noticeAckPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie, _, _ := strings.Cut(setCookie, ";"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func ackBody(cursor string) string { return `{"cursor":"` + cursor + `"}` }

// signedIn signs in and returns the count, the cursor and the cookie.
func signedIn(t *testing.T, engine *gin.Engine, username string, va *virtualAuthenticator) (int, string, string) {
	t.Helper()
	code, body, cookie := signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	n, cursor := decodeSignInNotice(t, body)
	return n, cursor, cookie
}

// addAdmin records one console creation of a new, still existing admin.
func addAdmin(t *testing.T, db *sql.DB, actingID, prefix string) {
	t.Helper()
	username := uniqueAdminUsername(prefix)
	createdRow(t, db, actingID, pendingAdminNamed(t, db, username), username, "0 seconds")
}

// A notice is shown again until the session that displayed it acknowledges
// it. The second sign-in stands in for a client that disconnected after the
// first one committed: it never saw the count, so it is shown again.
func TestSignInNotice_ShownUntilAcknowledged(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	engine := noticeEngine(t, db, rdb)

	n, _, _ := signedIn(t, engine, username, va)
	require.Equal(t, 0, n, "first sign-in")
	addAdmin(t, db, actingID, "ack-one")

	n, lostCursor, _ := signedIn(t, engine, username, va)
	require.Equal(t, 1, n)
	require.NotEmpty(t, lostCursor)
	// That response was lost: the next sign-in shows the creation again.
	n, cursor, cookie := signedIn(t, engine, username, va)
	require.Equal(t, 1, n, "a committed but undelivered notice is shown again")
	assert.Equal(t, lostCursor, cursor)

	rec := postAck(engine, cookie, ackBody(cursor), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = postAck(engine, cookie, ackBody(cursor), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "acknowledging again is a no-op: %s", rec.Body.String())

	n, _, _ = signedIn(t, engine, username, va)
	assert.Equal(t, 0, n, "acknowledged")
}

// A session can acknowledge only up to the cursor it was shown, and never
// below what this admin already acknowledged.
func TestSignInNotice_AckValidation(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	engine := noticeEngine(t, db, rdb)
	n, _, zeroCookie := signedIn(t, engine, username, va)
	require.Equal(t, 0, n)

	addAdmin(t, db, actingID, "ack-first")
	n, cursorA, cookieA := signedIn(t, engine, username, va)
	require.Equal(t, 1, n)
	addAdmin(t, db, actingID, "ack-second")
	var shownA int64
	_, err := fmt.Sscan(cursorA, &shownA)
	require.NoError(t, err)
	above := fmt.Sprint(maxAuditRowID(t, db))

	for name, tc := range map[string]struct {
		cookie, body string
		header       map[string]string
		code         int
	}{
		"above the shown cursor":       {cookieA, ackBody(above), nil, http.StatusBadRequest},
		"one above the shown cursor":   {cookieA, ackBody(fmt.Sprint(shownA + 1)), nil, http.StatusBadRequest},
		"a session that was shown 0":   {zeroCookie, ackBody(cursorA), nil, http.StatusBadRequest},
		"zero":                         {cookieA, ackBody("0"), nil, http.StatusBadRequest},
		"not decimal":                  {cookieA, ackBody("1e3"), nil, http.StatusBadRequest},
		"a number, not a string":       {cookieA, `{"cursor":` + cursorA + `}`, nil, http.StatusBadRequest},
		"unknown field":                {cookieA, `{"cursor":"` + cursorA + `","x":1}`, nil, http.StatusBadRequest},
		"form content type":            {cookieA, ackBody(cursorA), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusForbidden},
		"cross-site fetch":             {cookieA, ackBody(cursorA), map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		"no session":                   {"", ackBody(cursorA), nil, http.StatusUnauthorized},
		"a session that does not hold": {"__Host-cv_admin_sid=" + strings.Repeat("ab", 32), ackBody(cursorA), nil, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postAck(engine, tc.cookie, tc.body, tc.header)
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
		})
	}

	n, cursorB, cookieB := signedIn(t, engine, username, va)
	require.Equal(t, 2, n, "no refused acknowledgement moved the window")
	require.Equal(t, http.StatusNoContent, postAck(engine, cookieB, ackBody(cursorB), nil).Code)
	rec := postAck(engine, cookieA, ackBody(cursorA), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "below the acknowledged cursor: %s", rec.Body.String())
	n, _, _ = signedIn(t, engine, username, va)
	assert.Equal(t, 0, n)
}

// A sign-in whose count could not be read showed nothing, so its session has
// nothing to acknowledge.
func TestSignInNotice_UnreadSessionCannotAck(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	audit := admin.NewAuditLog(db)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	require.Equal(t, 0, requireCounted(t, audit, viewerID))
	addAdmin(t, db, actingID, "unread-ack")
	shown := requireCountedIn(t, audit, viewerID, "shown-session")
	require.Equal(t, 1, shown.n)

	fdb := sql.OpenDB(faultConnector{dsn: testdb.DatabaseURL(), fault: dbFault{statement: admin.AdminsAddedQueryMarkerForTest}})
	t.Cleanup(func() { _ = fdb.Close() })
	unread := recordSignIn(t.Context(), admin.NewAuditLog(fdb), viewerID, "unread-session")
	require.NoError(t, unread.err)
	require.False(t, unread.read)

	assert.ErrorIs(t, audit.AcknowledgeSignInNotice(t.Context(), viewerID, "unread-session", shown.cursor), admin.ErrNoticeCursorInvalid)
	assert.ErrorIs(t, audit.AcknowledgeSignInNotice(t.Context(), viewerID, "never-signed-in", shown.cursor), admin.ErrNoticeCursorInvalid)
	assert.ErrorIs(t, audit.AcknowledgeSignInNotice(t.Context(), viewerID, "shown-session", shown.cursor+1), admin.ErrNoticeCursorInvalid)
	require.NoError(t, audit.AcknowledgeSignInNotice(t.Context(), viewerID, "shown-session", shown.cursor))
	assert.Equal(t, 0, requireCounted(t, audit, viewerID))
}

// adminctl bootstrap adds an admin, so other admins are told about it; it is
// counted while it exists, and the row commits with the admin under the
// shared notice lock.
func TestSignInNotice_CountsAdminctlBootstrap(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	audit := admin.NewAuditLog(db)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	require.Equal(t, 0, requireCounted(t, audit, viewerID), "first sign-in")

	username := uniqueAdminUsername("notice-boot")
	t.Cleanup(func() { removeAdminNamed(t, db, username) })
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, admin.SignInNoticeLockKeyForTest)
	require.NoError(t, err)
	done := make(chan int, 1)
	var out bytes.Buffer
	go func() {
		done <- admin.RunAdminCtlForTest(context.Background(), db, rdb, strings.NewReader("Str0ng-P@ssw0rd-123\n"), &out,
			testEnrollBaseURL, []string{"bootstrap", "--username", username, "--password-stdin"})
	}()
	select {
	case code := <-done:
		assert.Fail(t, "bootstrap committed while a sign-in held the notice lock", "exit %d", code)
		done <- code
	case <-time.After(500 * time.Millisecond):
	}
	assertNoAdmin(t, db, username)
	require.NoError(t, tx.Commit())
	select {
	case code := <-done:
		require.Equal(t, 0, code, out.String())
	case <-time.After(10 * time.Second):
		require.FailNow(t, "bootstrap did not finish after the sign-in committed")
	}

	created, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
	require.NoError(t, err)
	var detailID, rowAdmin string
	require.NoError(t, db.QueryRow(
		`SELECT detail->>'created_admin_id', admin_id::text FROM admin_audit_log
		 WHERE event_type = $1 AND detail->>'verb' = 'bootstrap' AND admin_id = $2`,
		admin.EventBootstrap, created.ID,
	).Scan(&detailID, &rowAdmin))
	assert.Equal(t, created.ID, detailID)

	r := requireCountedIn(t, audit, viewerID, "boot-session")
	require.Equal(t, 1, r.n, "the bootstrapped admin is counted")
	removeAdminNamed(t, db, username)
	assert.Equal(t, 0, requireCounted(t, audit, viewerID), "and not once it is gone")
}

// A bootstrap whose name is refused writes neither the admin nor its row.
func TestAdminCtl_Bootstrap_ConfusableWritesNothing(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	sfx := fmt.Sprintf("-bc%d", time.Now().UnixNano())
	legacyAdminNamed(t, db, "Ops0"+sfx)
	before := maxAuditRowID(t, db)
	var out bytes.Buffer
	code := admin.RunAdminCtlForTest(t.Context(), db, rdb, strings.NewReader("Str0ng-P@ssw0rd-123\n"), &out,
		testEnrollBaseURL, []string{"bootstrap", "--username", "opso" + sfx, "--password-stdin"})
	assert.Equal(t, 1, code)
	assert.Contains(t, out.String(), "looks like an existing admin username")
	assertNoAdmin(t, db, "opso"+sfx)
	var rows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM admin_audit_log WHERE id > $1 AND event_type = $2 AND actor = $3`,
		before, admin.EventBootstrap, "opso"+sfx,
	).Scan(&rows))
	assert.Equal(t, 0, rows)
}

// The enrollment URL query-escapes the username and the token, so a legacy
// name holding a space, & or non-ASCII text survives the round trip.
func TestAdminCtl_ResetEnrollment_EscapesLegacyUsernameInURL(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	legacy := fmt.Sprintf("Ops Team&Zoë=%d", time.Now().UnixNano())
	adminID := legacyAdminNamed(t, db, legacy)
	var out bytes.Buffer
	code := admin.RunAdminCtlForTest(t.Context(), db, rdb, strings.NewReader(""), &out,
		testEnrollBaseURL, []string{"reset-enrollment", "--username", legacy})
	require.Equal(t, 0, code, out.String())

	m := regexp.MustCompile(`Enroll URL: (\S+)`).FindStringSubmatch(out.String())
	require.Len(t, m, 2, out.String())
	u, err := url.Parse(m[1])
	require.NoError(t, err)
	q := u.Query()
	assert.Len(t, q, 2, "no part of the username became its own parameter")
	assert.Equal(t, legacy, q.Get("username"))
	token := extractToken(t, out.String())
	assert.Equal(t, token, q.Get("token"))
	bound, _, err := admin.NewEnrollmentStore(rdb).ConsumeEnrollmentToken(t.Context(), q.Get("token"))
	require.NoError(t, err)
	assert.Equal(t, adminID, bound)
}

// The notice transactions run READ COMMITTED whatever the database default:
// under REPEATABLE READ the snapshot taken before the lock wait would miss the
// row the previous lock holder committed.
func TestNoticeTransactions_ReadCommittedUnderRepeatableReadDefault(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	var name string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))
	ident := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	_, err := db.Exec(`ALTER DATABASE ` + ident + ` SET default_transaction_isolation = 'repeatable read'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`ALTER DATABASE ` + ident + ` RESET default_transaction_isolation`)
		assert.NoError(t, err)
	})
	// Only connections opened after the ALTER take the new default.
	rr, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = rr.Close() })
	var level string
	require.NoError(t, rr.QueryRow(`SHOW default_transaction_isolation`).Scan(&level))
	require.Equal(t, "repeatable read", level)

	t.Run("sign-in sees the creation it waited for", func(t *testing.T) {
		audit := admin.NewAuditLog(rr)
		viewerID, _, _ := enrolledAdmin(t, db, rdb)
		require.Equal(t, 0, requireCounted(t, audit, viewerID))

		createdID := pendingAdminNamed(t, db, uniqueAdminUsername("rr-in-flight"))
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(`SELECT pg_advisory_xact_lock_shared($1)`, admin.SignInNoticeLockKeyForTest)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO admin_audit_log (event_type, result, detail) VALUES ($1, $2, jsonb_build_object('created_admin_id', $3::text))`,
			admin.EventAdminCreated, admin.AuditSuccess, createdID)
		require.NoError(t, err)

		done := make(chan signInResult, 1)
		go func() { done <- recordSignIn(context.Background(), audit, viewerID, "rr-session") }()
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, tx.Commit())
		var r signInResult
		select {
		case r = <-done:
		case <-time.After(10 * time.Second):
			require.FailNow(t, "the sign-in did not finish")
		}
		require.NoError(t, r.err)
		require.True(t, r.read)
		assert.Equal(t, 1, r.n, "the count saw the creation committed during its lock wait")
	})

	t.Run("a lookalike creation sees the one it waited for", func(t *testing.T) {
		repo := admin.NewAdminRepo(rr)
		sfx := fmt.Sprintf("-rr%d", time.Now().UnixNano())
		tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		first, err := admin.NewAdminRepo(db).CreatePendingTx(t.Context(), tx, "opso"+sfx, "unused-hash")
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			created, err := repo.CreatePending(context.Background(), "ops0"+sfx, "unused-hash")
			if err == nil {
				registerAdminCleanup(t, db, created.ID)
			}
			done <- err
		}()
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, tx.Commit())
		registerAdminCleanup(t, db, first.ID)
		select {
		case err := <-done:
			assert.ErrorIs(t, err, admin.ErrConfusableUsername)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "the second creation did not finish")
		}
	})
}

// The sign-in log names the stage that failed: the count, or the insert or
// commit of the sign-in row.
func TestWebAuthnLogin_NoticeFailureLogsItsStage(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	_, username, va := enrolledAdmin(t, db, rdb)

	for name, tc := range map[string]struct {
		fault      dbFault
		stage, not string
	}{
		"count fails":   {dbFault{statement: admin.AdminsAddedQueryMarkerForTest}, "notice_read", "audit_write"},
		"row not saved": {dbFault{statement: "SET LOCAL ROLE"}, "audit_write", "notice_read"},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			fdb := sql.OpenDB(faultConnector{dsn: testdb.DatabaseURL(), fault: tc.fault})
			t.Cleanup(func() { _ = fdb.Close() })
			h, err := admin.NewHandler(fdb, rdb, logger.NewWithWriter(&logs), authHandlerCfg())
			require.NoError(t, err)
			engine := gin.New()
			engine.POST("/admin/api/v1/auth/password", h.PasswordLogin)
			engine.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)

			code, body, _ := signInAdmin(t, engine, username, va)
			require.Equal(t, http.StatusOK, code, string(body))
			assert.NotContains(t, string(body), "admins_added")
			assert.Contains(t, logs.String(), "admin sign-in notice unavailable")
			assert.Contains(t, logs.String(), tc.stage)
			assert.NotContains(t, logs.String(), tc.not)
		})
	}
}

// Before this release the console recorded a creation as a bootstrap row with
// no detail, naming the acting admin, not the new one. Such a row, which a
// replica still on that release can write during a rolling deploy, counts as
// one added admin; the other break-glass verbs' bootstrap rows add no admin.
func TestSignInNotice_CountsLegacyConsoleCreationRow(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	viewerID, _, _ := enrolledAdmin(t, db, rdb)
	actingID, _, _ := enrolledAdmin(t, db, rdb)
	signInRow(t, db, viewerID, "13 hours")
	insertAuditRow(t, db, &actingID, "old-console", admin.EventBootstrap, admin.AuditSuccess, "13 hours 1 minute")
	insertAuditRowJSON(t, db, &actingID, "reset", admin.EventBootstrap, admin.AuditSuccess, "13 hours 2 minutes",
		map[string]any{"verb": "reset-enrollment"})
	insertAuditRowJSON(t, db, &actingID, "unlock", admin.EventBootstrap, admin.AuditSuccess, "13 hours 3 minutes",
		map[string]any{"verb": "unlock"})

	n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
