package admin_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// faultHandler builds a Handler on the shared test database whose connections
// fail at fault.
func faultHandler(t *testing.T, rdb *redis.Client, fault dbFault) *admin.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := sql.OpenDB(faultConnector{dsn: testdb.DatabaseURL(), fault: fault})
	t.Cleanup(func() { _ = db.Close() })
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	return h
}

func faultCreateEngine(t *testing.T, rdb *redis.Client, actingID string, fault dbFault) *gin.Engine {
	t.Helper()
	h := faultHandler(t, rdb, fault)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("admin_id", actingID); c.Next() })
	r.POST("/admin/api/v1/admins", h.CreateAdmin)
	r.POST("/admin/api/v1/admins/stepup", h.CreateAdminStepUpBegin)
	return r
}

// A failed admin_created audit write leaves no pending row behind, so the
// operator can retry with a fresh step-up instead of hitting a 409.
func TestCreateAdmin_AuditWriteFailure_IsRetryable(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	faulty := faultCreateEngine(t, rdb, actingID, dbFault{statement: "INSERT INTO admin_audit_log"})
	engine := adminCreateEngine(t, db, rdb, actingID)

	username := uniqueAdminUsername("auditfail")
	handle, challenge := stepUpBegin(t, faulty, "/admin/api/v1/admins/stepup", map[string]any{})
	rec := postJSON(faulty, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "enrollment_token")
	assertNoAdmin(t, db, username)

	handle, challenge = stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
	rec = postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
}

// A storage fault after the step-up was spent is audited, so every spent
// create_admin proof has a record.
func TestCreateAdmin_InsertFailureAfterStepUp_IsAudited(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	faulty := faultCreateEngine(t, rdb, actingID, dbFault{statement: "INSERT INTO admin_users"})

	username := uniqueAdminUsername("insertfail")
	handle, challenge := stepUpBegin(t, faulty, "/admin/api/v1/admins/stepup", map[string]any{})
	rec := postJSON(faulty, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assertNoAdmin(t, db, username)

	var reason string
	require.NoError(t, db.QueryRow(
		`SELECT detail->>'reason' FROM admin_audit_log WHERE event_type = $1 AND admin_id = $2 AND result = $3`,
		admin.EventAdminCreated, actingID, admin.AuditFailure,
	).Scan(&reason))
	assert.Equal(t, "create_failed", reason)
}

// The sign-in notice is informational: when it cannot be read, sign-in still
// succeeds and the response carries no count.
func TestWebAuthnLogin_NoticeReadFailure_DoesNotBlockSignIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, username, va := enrolledAdmin(t, db, rdb)
	h := faultHandler(t, rdb, dbFault{statement: "LEAST(COUNT(*)"})
	engine := gin.New()
	engine.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	engine.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)

	code, body, cookie := signInAdmin(t, engine, username, va)
	require.Equal(t, http.StatusOK, code, string(body))
	assert.NotEmpty(t, cookie)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.JSONEq(t, `"authenticated"`, string(raw["status"]))
	assert.NotContains(t, raw, "admins_added", "an unread count is not reported as zero")
}
