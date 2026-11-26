package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// assertNoAdmin fails if an admin row exists for the username.
func assertNoAdmin(t *testing.T, db *sql.DB, username string) {
	t.Helper()
	_, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), username)
	assert.True(t, errors.Is(err, admin.ErrAdminNotFound), "no admin row may be created: %v", err)
}

// VULN-139: a session alone must not create an admin. Without a fresh step-up
// the request is refused and no row or enrollment token exists afterwards.
func TestCreateAdmin_WithoutStepUp_RefusedAndCreatesNoRow(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, _ := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)

	for name, body := range map[string]map[string]any{
		"no step-up": {},
		"forged handle": {"stepup": map[string]any{
			"handle":    "0000000000000000000000000000000000000000000000000000000000000000",
			"assertion": map[string]any{"id": "x"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			username := uniqueAdminUsername("nostepup")
			body["username"] = username
			body["password"] = adminTestPassword // pragma: allowlist secret
			rec := postJSON(engine, "/admin/api/v1/admins", body)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "enrollment_token")
			assertNoAdmin(t, db, username)
		})
	}
}

// stepUpBegin begins a step-up on the engine route and returns its handle and
// challenge.
func stepUpBegin(t *testing.T, engine *gin.Engine, path string, body any) (string, string) {
	t.Helper()
	rec := postJSON(engine, path, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.PublicKey)
	return resp.Handle, resp.PublicKey.Response.Challenge.String()
}

// signStepUp signs a challenge with the next counter value.
func signStepUp(t *testing.T, va *virtualAuthenticator, challenge string, flags byte) json.RawMessage {
	t.Helper()
	va.signCount++
	return va.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flags, false)
}

func createBody(username, handle string, assertion json.RawMessage) map[string]any {
	return map[string]any{
		"username": username, "password": adminTestPassword,
		"stepup": map[string]any{"handle": handle, "assertion": assertion},
	}
}

func adminCreatedRows(t *testing.T, db *sql.DB, actingID, result string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM admin_audit_log WHERE event_type = $1 AND admin_id = $2 AND result = $3`,
		admin.EventAdminCreated, actingID, result,
	).Scan(&n))
	return n
}

// A fresh, user-verified create_admin step-up from the acting admin creates
// the admin, returns a live token and records one admin_created success.
func TestCreateAdmin_WithFreshStepUp_CreatesAdmin(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)

	handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
	username := uniqueAdminUsername("stepped")
	rec := postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		EnrollmentToken string `json:"enrollment_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	created, err := admin.NewAdminRepo(db).GetByUsername(ctx, username)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	bound, _, err := admin.NewEnrollmentStore(rdb).ConsumeEnrollmentToken(ctx, resp.EnrollmentToken)
	require.NoError(t, err)
	assert.Equal(t, created.ID, bound)
	assert.Equal(t, 1, adminCreatedRows(t, db, actingID, admin.AuditSuccess))
}

// Every way of presenting a step-up that is not a fresh create_admin assertion
// from this admin is refused with 403 and leaves no admin row behind.
func TestCreateAdmin_StepUpMisuse_RefusedAndCreatesNoRow(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	otherID, _, otherVA := enrolledAdmin(t, db, rdb)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(io.Discard), authHandlerCfg())
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	as := actingID
	session := testAdminSessionBinding
	engine.Use(func(c *gin.Context) {
		c.Set("admin_id", as)
		c.Set("admin_session_binding", session)
		c.Next()
	})
	engine.POST("/admin/api/v1/admins", h.CreateAdmin)
	engine.POST("/admin/api/v1/admins/stepup", h.CreateAdminStepUpBegin)
	engine.POST("/server/stepup", h.ServerStepUpBegin)

	refused := func(t *testing.T, body map[string]any, want string) {
		t.Helper()
		username := uniqueAdminUsername("misuse")
		body["username"] = username
		body["password"] = adminTestPassword // pragma: allowlist secret
		rec := postJSON(engine, "/admin/api/v1/admins", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{"error":"`+want+`"}`, rec.Body.String())
		assertNoAdmin(t, db, username)
	}
	proof := func(handle string, assertion json.RawMessage) map[string]any {
		return map[string]any{"stepup": map[string]any{"handle": handle, "assertion": assertion}}
	}

	t.Run("missing step-up", func(t *testing.T) {
		refused(t, map[string]any{}, "stepup_required")
	})
	t.Run("handle is single-use", func(t *testing.T) {
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		rec := postJSON(engine, "/admin/api/v1/admins", createBody(uniqueAdminUsername("first"), handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp struct {
			Username string `json:"username"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		first, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), resp.Username)
		require.NoError(t, err)
		registerAdminCleanup(t, db, first.ID)
		refused(t, proof(handle, signStepUp(t, va, challenge, flagUP|flagUV)), "stepup_failed")
	})
	t.Run("user verification required", func(t *testing.T) {
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		refused(t, proof(handle, signStepUp(t, va, challenge, flagUP)), "stepup_failed")
	})
	t.Run("tampered signature", func(t *testing.T) {
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		va.signCount++
		refused(t, proof(handle, va.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flagUP|flagUV, true)), "stepup_failed")
	})
	t.Run("counter did not advance", func(t *testing.T) {
		// Replay the stored counter: the library raises a clone warning.
		var stored uint32
		require.NoError(t, db.QueryRow(`SELECT sign_count FROM admin_webauthn_credentials WHERE admin_id = $1`, actingID).Scan(&stored))
		require.Positive(t, stored)
		va.signCount = stored
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		refused(t, proof(handle, va.assertionResponse(t, testAdminRPID, challenge, testAdminOrigin, flagUP|flagUV, false)), "stepup_failed")
	})
	t.Run("handle begun by another admin", func(t *testing.T) {
		as = otherID
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		assertion := signStepUp(t, otherVA, challenge, flagUP|flagUV)
		as = actingID
		refused(t, proof(handle, assertion), "stepup_failed")
	})
	t.Run("server action handle", func(t *testing.T) {
		handle, challenge := stepUpBegin(t, engine, "/server/stepup", map[string]string{"action": "stop"})
		refused(t, proof(handle, signStepUp(t, va, challenge, flagUP|flagUV)), "stepup_failed")
	})
	t.Run("handle begun in another session of this admin", func(t *testing.T) {
		session = "other-session-binding"
		handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
		session = testAdminSessionBinding
		refused(t, proof(handle, signStepUp(t, va, challenge, flagUP|flagUV)), "stepup_failed")
	})

	assert.Equal(t, 1, adminCreatedRows(t, db, actingID, admin.AuditSuccess))
	assert.Equal(t, 8, adminCreatedRows(t, db, actingID, admin.AuditDenied), "one denied row per refusal above")
	// Each refusal records why, in the order of the subtests above; the
	// response stays a uniform stepup_failed.
	assert.Equal(t, []string{
		"stepup_required",
		"stepup_unknown_handle",     // single-use
		"stepup_assertion_rejected", // no user verification
		"stepup_assertion_rejected", // tampered signature
		"stepup_clone_warning",      // counter did not advance
		"stepup_unknown_handle",     // another admin's handle is not reachable
		"stepup_unknown_handle",     // server-action handle, other prefix
		"stepup_wrong_session",      // another session of this admin
	}, reasonsOf(createAuditRows(t, db, actingID), admin.AuditDenied))
}

// Input that is refused before the step-up does not spend the handle: a weak
// password, and a taken username, which is a 409 before the step-up as a
// lookalike is a 400.
func TestCreateAdmin_StepUpOrdering(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := context.Background()

	actingID, actingName, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)

	handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
	assertion := signStepUp(t, va, challenge, flagUP|flagUV)
	weak := createBody(uniqueAdminUsername("weak"), handle, assertion)
	weak["password"] = "short" // pragma: allowlist secret
	assert.Equal(t, http.StatusBadRequest, postJSON(engine, "/admin/api/v1/admins", weak).Code)

	// A taken username is a 409 with or without a step-up.
	rec := postJSON(engine, "/admin/api/v1/admins", map[string]any{"username": actingName, "password": adminTestPassword})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = postJSON(engine, "/admin/api/v1/admins", createBody(actingName, handle, assertion))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "username already exists")
	assert.Equal(t, 0, adminCreatedRows(t, db, actingID, admin.AuditFailure), "no step-up was spent")

	// The same handle still works after the 400 and the 409.
	username := uniqueAdminUsername("after")
	rec = postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, assertion))
	require.Equal(t, http.StatusCreated, rec.Code, "the proof was not spent: %s", rec.Body.String())
	created, err := admin.NewAdminRepo(db).GetByUsername(ctx, username)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
}

// A username taken between the early check and the create lock is still a
// 409, after the step-up is spent, and that spent step-up is audited.
func TestCreateAdmin_UsernameTakenAfterEarlyCheck_ConflictAfterStepUp(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	username := uniqueAdminUsername("raced")

	// This transaction holds the create lock with the name inserted but not
	// committed, so the early check, which reads without the lock, passes.
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	first, err := admin.NewAdminRepo(db).CreatePendingTx(t.Context(), tx, username, "unused-hash")
	require.NoError(t, err)

	handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", nil)
	assertion := signStepUp(t, va, challenge, flagUP|flagUV)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, assertion)) }()
	select {
	case rec := <-done:
		assert.Fail(t, "the creation finished while another held the create lock", "code %d", rec.Code)
		done <- rec
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	registerAdminCleanup(t, db, first.ID)

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the creation did not finish after the lock holder committed")
	}
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"username_taken"}, reasonsOf(createAuditRows(t, db, actingID), admin.AuditFailure),
		"the spent step-up is audited")
	rec = postJSON(engine, "/admin/api/v1/admins", createBody(uniqueAdminUsername("reuse"), handle, assertion))
	assert.Equal(t, http.StatusForbidden, rec.Code, "the step-up was spent: %s", rec.Body.String())
}

// An admin with no enrolled key cannot begin a create_admin step-up.
func TestCreateAdminStepUpBegin_NoKey_Refused(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	pending, err := admin.NewAdminRepo(db).CreatePending(context.Background(), uniqueAdminUsername("nokey"), "x")
	require.NoError(t, err)
	registerAdminCleanup(t, db, pending.ID)
	engine := adminCreateEngine(t, db, rdb, pending.ID)

	rec := postJSON(engine, "/admin/api/v1/admins/stepup", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":"stepup_unavailable"}`, rec.Body.String())

	req := bytes.NewBufferString(`{"action":"stop"}`)
	rec = postRaw(engine, "/admin/api/v1/admins/stepup", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the begin body takes no fields")
}

// Both admin-create routes sit behind a fail-closed limiter of five per hour.
func TestAdminCreateRoutesAreRateLimited(t *testing.T) {
	engine, sessions, activeAdmin := registerAdminEngineAndSessions(t)
	sid, err := sessions.Mint(context.Background(), activeAdmin.ID, activeAdmin.SessionGeneration)
	require.NoError(t, err)
	for _, target := range []string{"/admin/api/v1/admins", "/admin/api/v1/admins/stepup"} {
		for requestNumber := 1; requestNumber <= 5; requestNumber++ {
			response := serveAdminMetricsRoute(engine, sid, http.MethodPost, target)
			assert.NotEqual(t, http.StatusTooManyRequests, response.Code, "%s request %d", target, requestNumber)
		}
		response := serveAdminMetricsRoute(engine, sid, http.MethodPost, target)
		assert.Equal(t, http.StatusTooManyRequests, response.Code, target)
		assert.JSONEq(t, `{"error":"rate_limited"}`, response.Body.String())
	}
}

func TestAdminCreateRouteRateLimitFailsClosed(t *testing.T) {
	limiterRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = limiterRedis.Close() })
	engine, sessions, activeAdmin := registerAdminEngineAndSessionsWithLimiter(t, limiterRedis)
	sid, err := sessions.Mint(context.Background(), activeAdmin.ID, activeAdmin.SessionGeneration)
	require.NoError(t, err)
	for _, target := range []string{"/admin/api/v1/admins", "/admin/api/v1/admins/stepup"} {
		response := serveAdminMetricsRoute(engine, sid, http.MethodPost, target)
		assert.Equal(t, http.StatusInternalServerError, response.Code, target)
		assert.JSONEq(t, `{"error":"internal_error"}`, response.Body.String())
	}
}

// postRaw issues an in-process POST with a raw JSON body.
func postRaw(engine *gin.Engine, path string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}
