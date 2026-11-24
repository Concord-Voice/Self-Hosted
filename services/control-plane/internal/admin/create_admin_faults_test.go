package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// failEnrollMintHook fails any command on an enrollment-token key (the SETNX
// that mints one) and passes every other Redis command through, so step-up
// challenges and sessions still work.
type failEnrollMintHook struct{}

func (failEnrollMintHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failEnrollMintHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); len(args) > 1 {
			if key, ok := args[1].(string); ok && strings.HasPrefix(key, "admin_enroll:") {
				err := errors.New("forced enrollment token store failure")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (failEnrollMintHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A failed enrollment-token mint after the pending row committed removes that
// row and audits the failure, so a retry of the same username with a fresh
// step-up is created (201) instead of refused as a duplicate (409).
func TestCreateAdmin_TokenMintFailure_IsRetryable(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	failing := redis.NewClient(rdb.Options())
	t.Cleanup(func() { _ = failing.Close() })
	failing.AddHook(failEnrollMintHook{})
	faulty := faultCreateEngine(t, failing, actingID, dbFault{statement: "no statement matches this fault"})
	engine := adminCreateEngine(t, db, rdb, actingID)

	username := uniqueAdminUsername("mintfail")
	handle, challenge := stepUpBegin(t, faulty, "/admin/api/v1/admins/stepup", map[string]any{})
	rec := postJSON(faulty, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "enrollment_token")
	assertNoAdmin(t, db, username)

	var reason string
	require.NoError(t, db.QueryRow(
		`SELECT detail->>'reason' FROM admin_audit_log
		 WHERE event_type = $1 AND admin_id = $2 AND result = $3 AND actor = $4`,
		admin.EventAdminCreated, actingID, admin.AuditFailure, username,
	).Scan(&reason))
	assert.Equal(t, "token_mint_failed", reason)

	handle, challenge = stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
	rec = postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "enrollment_token")
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
