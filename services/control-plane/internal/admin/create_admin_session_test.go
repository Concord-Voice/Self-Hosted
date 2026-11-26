package admin_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// routedCreateEngine mounts the real admin routes, AdminAuthRequired and the
// create limiter included.
func routedCreateEngine(t *testing.T, db *sql.DB, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log := logger.NewWithWriter(&bytes.Buffer{})
	h, err := admin.NewHandler(db, rdb, log, authHandlerCfg())
	require.NoError(t, err)
	metricsHandler, err := opsmetrics.NewAdminHandler(emptyAdminMetricsReader{}, "cvn_aaaaaaaaaaaaaaaa", 15*time.Second, log)
	require.NoError(t, err)
	engine := gin.New()
	admin.RegisterRoutes(&engine.RouterGroup, h, metricsHandler, rdb, admin.NewUI(adminUITestFS()))
	return engine
}

// mintAdminSession mints a real admin session for the named admin.
func mintAdminSession(t *testing.T, db *sql.DB, rdb *redis.Client, username string) string {
	t.Helper()
	a, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
	require.NoError(t, err)
	sid, err := admin.NewSessionStore(rdb, time.Now).Mint(t.Context(), a.ID, a.SessionGeneration)
	require.NoError(t, err)
	return sid
}

// postAsSession POSTs a JSON body with the session cookie from the given IP.
func postAsSession(engine http.Handler, sid, ip, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.RemoteAddr = ip + ":4242"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// sessionStepUpBegin begins a create_admin step-up in a session and returns
// its handle and challenge.
func sessionStepUpBegin(t *testing.T, engine http.Handler, sid string) (string, string) {
	t.Helper()
	rec := postAsSession(engine, sid, "192.0.2.10", createStepUpPath, map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.PublicKey)
	return resp.Handle, resp.PublicKey.Response.Challenge.String()
}

// A create_admin step-up is bound to the admin session that began it. Another
// session of the same admin is refused (and burns the handle); the session
// that began it can redeem a fresh one; another admin's handle stays out of
// reach and usable by its owner.
func TestCreateAdmin_StepUpBoundToBeginningSession(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, actingName, va := enrolledAdmin(t, db, rdb)
	otherID, otherName, otherVA := enrolledAdmin(t, db, rdb)
	engine := routedCreateEngine(t, db, rdb)
	s1 := mintAdminSession(t, db, rdb, actingName)
	s2 := mintAdminSession(t, db, rdb, actingName)
	sOther := mintAdminSession(t, db, rdb, otherName)
	const ip = "192.0.2.10"

	t.Run("another session of the same admin is refused and burns the handle", func(t *testing.T) {
		handle, challenge := sessionStepUpBegin(t, engine, s1)
		username := uniqueAdminUsername("s2")
		rec := postAsSession(engine, s2, ip, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{"error":"stepup_failed"}`, rec.Body.String())
		assertNoAdmin(t, db, username)
		rows := createAuditRows(t, db, actingID)
		assert.Equal(t, createAuditRow{admin.AuditDenied, "stepup_wrong_session", "", username}, rows[len(rows)-1])

		rec = postAsSession(engine, s1, ip, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusForbidden, rec.Code, "the refused attempt spent the handle: %s", rec.Body.String())
		rows = createAuditRows(t, db, actingID)
		assert.Equal(t, "stepup_unknown_handle", rows[len(rows)-1].reason)
	})
	t.Run("the beginning session redeems", func(t *testing.T) {
		handle, challenge := sessionStepUpBegin(t, engine, s1)
		username := uniqueAdminUsername("s1")
		cleanupAdminNamed(t, db, username)
		rec := postAsSession(engine, s1, ip, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("another admin's handle is untouched", func(t *testing.T) {
		handle, challenge := sessionStepUpBegin(t, engine, sOther)
		username := uniqueAdminUsername("theirs")
		cleanupAdminNamed(t, db, username)
		body := createBody(username, handle, signStepUp(t, otherVA, challenge, flagUP|flagUV))
		rec := postAsSession(engine, s2, ip, createPath, body)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rows := createAuditRows(t, db, actingID)
		assert.Equal(t, "stepup_unknown_handle", rows[len(rows)-1].reason)
		rec = postAsSession(engine, sOther, ip, createPath, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, 1, adminCreatedRows(t, db, otherID, admin.AuditSuccess))
	})
}

// A begin that reaches the handler without a session binding is refused, so
// no handle exists that no session could redeem.
func TestCreateAdminStepUpBegin_WithoutSessionBinding_FailsClosed(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, _ := enrolledAdmin(t, db, rdb)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("admin_id", actingID); c.Next() })
	engine.POST(createStepUpPath, h.CreateAdminStepUpBegin)

	rec := postJSON(engine, createStepUpPath, map[string]any{})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "handle")
	assert.Equal(t, []string{"stepup_begin_no_session"}, reasonsOf(createAuditRows(t, db, actingID), admin.AuditFailure))
	keys, err := rdb.Keys(t.Context(), "admin_create_stepup:"+actingID+":*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// activeKeylessAdmin creates an active admin with no key and returns its
// username.
func activeKeylessAdmin(t *testing.T, db *sql.DB) string {
	t.Helper()
	repo := admin.NewAdminRepo(db)
	created, err := repo.CreatePending(t.Context(), uniqueAdminUsername("limit"), "test-hash")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	require.NoError(t, repo.SetStatus(t.Context(), created.ID, admin.StatusActive))
	return created.Username
}

// The create limit is counted per acting admin and client IP: one admin
// spending five from an address does not throttle another admin behind the
// same address, and the first admin is still limited at the sixth.
func TestAdminCreateRateLimit_IsPerAdminAndIP(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	engine := routedCreateEngine(t, db, rdb)
	sidA := mintAdminSession(t, db, rdb, activeKeylessAdmin(t, db))
	sidB := mintAdminSession(t, db, rdb, activeKeylessAdmin(t, db))
	const ipX, ipY = "192.0.2.10", "198.51.100.7"

	// Bodies that reach a keyless 403: no step-up on create, no key on begin.
	bodies := map[string]map[string]any{
		createPath:       {"username": uniqueAdminUsername("limited"), "password": adminTestPassword}, // pragma: allowlist secret
		createStepUpPath: {},
	}
	for target, body := range bodies {
		t.Run(target, func(t *testing.T) {
			for n := 1; n <= 5; n++ {
				rec := postAsSession(engine, sidA, ipX, target, body)
				require.Equal(t, http.StatusForbidden, rec.Code, "admin A request %d: %s", n, rec.Body.String())
			}
			rec := postAsSession(engine, sidA, ipX, target, body)
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
			assert.JSONEq(t, `{"error":"rate_limited"}`, rec.Body.String())
			assert.NotEmpty(t, rec.Header().Get("Retry-After"))

			rec = postAsSession(engine, sidB, ipX, target, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "admin B behind the same IP is not throttled: %s", rec.Body.String())
			rec = postAsSession(engine, sidA, ipY, target, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "admin A from another IP has its own bucket: %s", rec.Body.String())
		})
	}
}

// The create limiter refuses a request that carries no admin id, instead of
// counting it in a shared bucket.
func TestAdminCreateRateLimit_WithoutAdminID_FailsClosed(t *testing.T) {
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	reached := false
	engine.POST("/limited", admin.RateLimitAdminCreateForTest(rdb, 5, time.Hour), func(c *gin.Context) {
		reached = true
		c.Status(http.StatusNoContent)
	})
	rec := postJSON(engine, "/limited", map[string]any{})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal_error"}`, rec.Body.String())
	assert.False(t, reached)
}
