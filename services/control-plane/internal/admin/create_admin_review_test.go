package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

const createPath = "/admin/api/v1/admins"
const createStepUpPath = "/admin/api/v1/admins/stepup"

// createAuditRow is one admin_created row written against an acting admin.
type createAuditRow struct {
	result, reason, createdID, actor string
}

// createAuditRows returns the acting admin's admin_created rows, oldest first.
func createAuditRows(t *testing.T, db *sql.DB, actingID string) []createAuditRow {
	t.Helper()
	rows, err := db.Query(
		`SELECT result, COALESCE(detail->>'reason', ''), COALESCE(detail->>'created_admin_id', ''), COALESCE(actor, '')
		 FROM admin_audit_log WHERE event_type = $1 AND admin_id = $2 ORDER BY id`,
		admin.EventAdminCreated, actingID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []createAuditRow
	for rows.Next() {
		var r createAuditRow
		require.NoError(t, rows.Scan(&r.result, &r.reason, &r.createdID, &r.actor))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// reasonsOf returns the reasons of the rows with this result.
func reasonsOf(rows []createAuditRow, result string) []string {
	var out []string
	for _, r := range rows {
		if r.result == result {
			out = append(out, r.reason)
		}
	}
	return out
}

// failRedisHook fails the named command (any command when cmd is empty) on
// keys with the prefix and passes every other command through.
type failRedisHook struct {
	cmd, prefix string
	// before, when set, runs before the failure is returned.
	before func()
}

func (failRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f failRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); len(args) > 1 && (f.cmd == "" || strings.EqualFold(cmd.Name(), f.cmd)) {
			if key, ok := args[1].(string); ok && strings.HasPrefix(key, f.prefix) {
				if f.before != nil {
					f.before()
				}
				err := errors.New("forced redis failure")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (failRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func hookedRedis(t *testing.T, rdb *redis.Client, hook redis.Hook) *redis.Client {
	t.Helper()
	c := redis.NewClient(rdb.Options())
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	return c
}

// noFault matches no statement.
var noFault = dbFault{statement: "no statement matches this fault"}

// A Commit whose acknowledgement is lost may have written the pending admin
// and its success row. The pending row is removed, the failure row records
// that and names the admin, and a retry is created instead of refused as a
// duplicate.
func TestCreateAdmin_CommitAckLost_RemovesRowAndIsRetryable(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	once := &atomic.Int32{}
	once.Store(1)
	faulty := faultCreateEngine(t, rdb, actingID, dbFault{commitLost: true, commitTimes: once})
	engine := adminCreateEngine(t, db, rdb, actingID)

	username := uniqueAdminUsername("commitlost")
	cleanupAdminNamed(t, db, username)
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	rec := postJSON(faulty, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"audit_unavailable"}`, rec.Body.String())
	assertNoAdmin(t, db, username)

	rows := createAuditRows(t, db, actingID)
	require.Len(t, rows, 2, "the committed success row and the failure row")
	assert.Equal(t, admin.AuditSuccess, rows[0].result)
	assert.NotEmpty(t, rows[0].createdID)
	assert.Equal(t, createAuditRow{admin.AuditFailure, "audit_commit_failed_row_removed", rows[0].createdID, username}, rows[1])

	handle, challenge = stepUpBegin(t, engine, createStepUpPath, nil)
	rec = postJSON(engine, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// A Commit that fails outright wrote nothing; the cleanup finds no row and
// the failure row says so.
func TestCreateAdmin_CommitFailed_AuditsNoRow(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	once := &atomic.Int32{}
	once.Store(1)
	faulty := faultCreateEngine(t, rdb, actingID, dbFault{commit: true, commitTimes: once})
	engine := adminCreateEngine(t, db, rdb, actingID)

	username := uniqueAdminUsername("commitfail")
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	rec := postJSON(faulty, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assertNoAdmin(t, db, username)

	rows := createAuditRows(t, db, actingID)
	require.Len(t, rows, 1)
	assert.Equal(t, admin.AuditFailure, rows[0].result)
	assert.Equal(t, "audit_commit_failed_no_row", rows[0].reason)
	assert.NotEmpty(t, rows[0].createdID)
}

// Only the first audit insert (the one inside the create transaction) fails:
// no admin is left, and the failure row records audit_unavailable.
func TestCreateAdmin_FirstAuditInsertFails_FailureRowRecorded(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	once := &atomic.Int32{}
	once.Store(1)
	faulty := faultCreateEngine(t, rdb, actingID, dbFault{statement: "INSERT INTO admin_audit_log", statementTimes: once})
	engine := adminCreateEngine(t, db, rdb, actingID)

	username := uniqueAdminUsername("firstinsert")
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	rec := postJSON(faulty, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assertNoAdmin(t, db, username)
	assert.Equal(t, []createAuditRow{{admin.AuditFailure, "audit_unavailable", "", username}}, createAuditRows(t, db, actingID))
}

// After a mint failure the cleanup deletes only the row it created. When the
// row was enrolled or reset meanwhile (here: activated), nothing matches and
// the reason is token_mint_failed_row_changed; when the delete itself fails,
// the row is kept and the reason is token_mint_failed_row_kept.
func TestCreateAdmin_TokenMintFailure_RowChangedVersusRowKept(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)

	t.Run("row changed", func(t *testing.T) {
		username := uniqueAdminUsername("mintchanged")
		cleanupAdminNamed(t, db, username)
		activate := func() {
			_, err := db.Exec(`UPDATE admin_users SET status = 'active' WHERE username = $1`, username)
			assert.NoError(t, err)
		}
		failing := hookedRedis(t, rdb, failRedisHook{prefix: "admin_enroll:", before: activate})
		faulty := faultCreateEngine(t, failing, actingID, noFault)
		handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
		rec := postJSON(faulty, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		kept, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
		require.NoError(t, err, "an admin that changed is not removed")
		rows := createAuditRows(t, db, actingID)
		last := rows[len(rows)-1]
		assert.Equal(t, createAuditRow{admin.AuditFailure, "token_mint_failed_row_changed", kept.ID, username}, last)
	})
	t.Run("row kept", func(t *testing.T) {
		username := uniqueAdminUsername("mintkept")
		cleanupAdminNamed(t, db, username)
		failing := hookedRedis(t, rdb, failEnrollMintHook{})
		faulty := faultCreateEngine(t, failing, actingID, dbFault{statement: "DELETE FROM admin_users"})
		handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
		rec := postJSON(faulty, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		kept, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
		require.NoError(t, err, "a failed delete keeps the row")
		assert.Equal(t, admin.StatusPending, kept.Status)
		rows := createAuditRows(t, db, actingID)
		last := rows[len(rows)-1]
		assert.Equal(t, createAuditRow{admin.AuditFailure, "token_mint_failed_row_kept", kept.ID, username}, last)
	})
}

// End to end: a mint failure, its successful retry, and a mint failure whose
// row was kept leave two admins, and the next sign-in of another admin is
// told exactly two were added.
func TestWebAuthnLogin_NoticeCountsAdminsThatExistAfterMintFailures(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	_, viewerName, viewerVA := enrolledAdmin(t, db, rdb)
	actingID, _, va := enrolledAdmin(t, db, rdb)
	authEngine := adminAuthEngine(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	failing := hookedRedis(t, rdb, failEnrollMintHook{})
	mintFails := faultCreateEngine(t, failing, actingID, noFault)
	mintFailsKept := faultCreateEngine(t, failing, actingID, dbFault{statement: "DELETE FROM admin_users"})

	code, body, _ := signInAdmin(t, authEngine, viewerName, viewerVA)
	require.Equal(t, http.StatusOK, code, string(body))

	create := func(e *gin.Engine, username string, want int) {
		t.Helper()
		handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
		rec := postJSON(e, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
		require.Equal(t, want, rec.Code, rec.Body.String())
	}
	retried := uniqueAdminUsername("e2e-retried")
	cleanupAdminNamed(t, db, retried)
	create(mintFails, retried, http.StatusInternalServerError)
	create(engine, retried, http.StatusCreated)
	kept := uniqueAdminUsername("e2e-kept")
	cleanupAdminNamed(t, db, kept)
	create(mintFailsKept, kept, http.StatusInternalServerError)
	assert.Len(t, reasonsOf(createAuditRows(t, db, actingID), admin.AuditSuccess), 3,
		"three success rows were committed for two admins")

	code, body, _ = signInAdmin(t, authEngine, viewerName, viewerVA)
	require.Equal(t, http.StatusOK, code, string(body))
	assert.Equal(t, 2, decodeSignIn(t, body))
}

// Storage faults while redeeming a step-up fail closed with 503 and a
// failure row naming the fault, not a 403 that reads as a bad key.
func TestCreateAdmin_StepUpStorageFaults_Are503AndAudited(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	cases := []struct {
		name   string
		faulty func(t *testing.T) *gin.Engine
		reason string
	}{
		{"redis load", func(t *testing.T) *gin.Engine {
			return faultCreateEngine(t, hookedRedis(t, rdb, failRedisHook{cmd: "getdel", prefix: "admin_create_stepup:"}), actingID, noFault)
		}, "stepup_store_unavailable"},
		{"credential lookup", func(t *testing.T) *gin.Engine {
			return faultCreateEngine(t, rdb, actingID, dbFault{statement: "credential_name, transports"})
		}, "stepup_credential_lookup_failed"},
		{"sign-count persist", func(t *testing.T) *gin.Engine {
			return faultCreateEngine(t, rdb, actingID, dbFault{statement: "UPDATE admin_webauthn_credentials"})
		}, "stepup_sign_count_persist_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			username := uniqueAdminUsername("stepupfault")
			handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
			rec := postJSON(tc.faulty(t), createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.JSONEq(t, `{"error":"service_unavailable"}`, rec.Body.String())
			assertNoAdmin(t, db, username)
			rows := createAuditRows(t, db, actingID)
			assert.Equal(t, createAuditRow{admin.AuditFailure, tc.reason, "", username}, rows[len(rows)-1])
		})
	}
}

// Begin refuses an admin with no key with 403 and a denied row, and answers
// a storage fault with a 5xx and a failure row.
func TestCreateAdminStepUpBegin_FaultsAreAudited(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, _ := enrolledAdmin(t, db, rdb)

	rec := postJSON(faultCreateEngine(t, rdb, actingID, dbFault{statement: "credential_name, transports"}), createStepUpPath, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	failing := hookedRedis(t, rdb, failRedisHook{cmd: "set", prefix: "admin_create_stepup:"})
	rec = postJSON(faultCreateEngine(t, failing, actingID, noFault), createStepUpPath, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"stepup_begin_unavailable", "stepup_begin_failed"},
		reasonsOf(createAuditRows(t, db, actingID), admin.AuditFailure))

	pending, err := admin.NewAdminRepo(db).CreatePending(t.Context(), uniqueAdminUsername("nokey"), "x")
	require.NoError(t, err)
	registerAdminCleanup(t, db, pending.ID)
	rec = postJSON(adminCreateEngine(t, db, rdb, pending.ID), createStepUpPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, []string{"stepup_unavailable"}, reasonsOf(createAuditRows(t, db, pending.ID), admin.AuditDenied))
}

// An expired handle is refused like a forged one, with a denied row.
func TestCreateAdmin_ExpiredHandle_RefusedAndAudited(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	key := "admin_create_stepup:" + actingID + ":" + handle
	require.Equal(t, int64(1), rdb.Del(t.Context(), key).Val(), "the handle's key")

	username := uniqueAdminUsername("expired")
	rec := postJSON(engine, createPath, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"stepup_failed"}`, rec.Body.String())
	assertNoAdmin(t, db, username)
	assert.Equal(t, []createAuditRow{{admin.AuditDenied, "stepup_unknown_handle", "", username}}, createAuditRows(t, db, actingID))
}

// Another admin presenting a handle neither redeems nor spends it: the admin
// who began it can still use it.
func TestCreateAdmin_WrongAdmin_DoesNotSpendTheHandle(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	ownerID, _, ownerVA := enrolledAdmin(t, db, rdb)
	otherID, _, _ := enrolledAdmin(t, db, rdb)
	owner := adminCreateEngine(t, db, rdb, ownerID)
	other := adminCreateEngine(t, db, rdb, otherID)

	handle, challenge := stepUpBegin(t, owner, createStepUpPath, nil)
	body := createBody(uniqueAdminUsername("owned"), handle, signStepUp(t, ownerVA, challenge, flagUP|flagUV))
	rec := postJSON(other, createPath, body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = postJSON(owner, createPath, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	cleanupAdminNamed(t, db, body["username"].(string))
}

// One handle redeemed by many concurrent requests creates exactly one admin.
func TestCreateAdmin_OneHandleConcurrently_CreatesOne(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	assertion := signStepUp(t, va, challenge, flagUP|flagUV)

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		username := uniqueAdminUsername("race-one")
		cleanupAdminNamed(t, db, username)
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = postJSON(engine, createPath, createBody(username, handle, assertion)).Code
		}()
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			created++
		} else {
			assert.Equal(t, http.StatusForbidden, code)
		}
	}
	assert.Equal(t, 1, created, "codes %v", codes)
	assert.Equal(t, 1, adminCreatedRows(t, db, actingID, admin.AuditSuccess))
	assert.Equal(t, n-1, adminCreatedRows(t, db, actingID, admin.AuditDenied))
}

// Two valid step-ups racing for one username create one admin; the other
// gets 409. When both passed the early check, the loser spent its step-up and
// writes a username_taken row; when it was refused by the early check it
// spent nothing and writes none.
func TestCreateAdmin_SameUsernameConcurrently_OneCreatedOneConflict(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	username := uniqueAdminUsername("race-name")
	cleanupAdminNamed(t, db, username)
	type actor struct {
		id     string
		engine *gin.Engine
		body   map[string]any
	}
	actors := make([]actor, 0, 2)
	for range 2 {
		id, _, va := enrolledAdmin(t, db, rdb)
		e := adminCreateEngine(t, db, rdb, id)
		handle, challenge := stepUpBegin(t, e, createStepUpPath, nil)
		actors = append(actors, actor{id, e, createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV))})
	}
	codes := make([]int, len(actors))
	var wg sync.WaitGroup
	for i, a := range actors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = postJSON(a.engine, createPath, a.body).Code
		}()
	}
	wg.Wait()
	assert.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, codes)
	taken := make([]string, 0, 1)
	for _, a := range actors {
		taken = append(taken, reasonsOf(createAuditRows(t, db, a.id), admin.AuditFailure)...)
	}
	assert.Subset(t, []string{"username_taken"}, taken)
	assert.LessOrEqual(t, len(taken), 1, "reasons %v", taken)
}

// A rate-limited create never reaches the handler, so it does not spend the
// step-up it carries.
func TestCreateAdmin_Throttled_DoesNotSpendStepUp(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	ctx := t.Context()

	actingID, actingName, va := enrolledAdmin(t, db, rdb)
	acting, err := admin.NewAdminRepo(db).GetByUsername(ctx, actingName)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	log := logger.NewWithWriter(&bytes.Buffer{})
	h, err := admin.NewHandler(db, rdb, log, authHandlerCfg())
	require.NoError(t, err)
	metricsHandler, err := opsmetrics.NewAdminHandler(emptyAdminMetricsReader{}, "cvn_aaaaaaaaaaaaaaaa", 15*time.Second, log)
	require.NoError(t, err)
	routed := gin.New()
	admin.RegisterRoutes(&routed.RouterGroup, h, metricsHandler, rdb, admin.NewUI(adminUITestFS()))
	sid, err := admin.NewSessionStore(rdb, time.Now).Mint(ctx, actingID, acting.SessionGeneration)
	require.NoError(t, err)

	engine := adminCreateEngine(t, db, rdb, actingID)
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	body := createBody(uniqueAdminUsername("throttled"), handle, signStepUp(t, va, challenge, flagUP|flagUV))
	cleanupAdminNamed(t, db, body["username"].(string))

	for range 5 {
		serveAdminMetricsRoute(routed, sid, http.MethodPost, createPath)
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, createPath, bytes.NewReader(raw))
	req.RemoteAddr = "192.0.2.10:4242"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	rec := httptest.NewRecorder()
	routed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	assert.Equal(t, int64(1), rdb.Exists(ctx, "admin_create_stepup:"+actingID+":"+handle).Val(), "the handle is unspent")
	rec = postJSON(engine, createPath, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// The create body is decoded strictly and bounded, and a username over the
// limit is refused before the step-up, so neither spends the proof nor
// writes an audit row.
func TestCreateAdmin_RequestHygiene_RefusedBeforeStepUp(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	handle, challenge := stepUpBegin(t, engine, createStepUpPath, nil)
	assertion := signStepUp(t, va, challenge, flagUP|flagUV)

	unknown := createBody(uniqueAdminUsername("unknown"), handle, assertion)
	unknown["role"] = "owner"
	long := createBody(strings.Repeat("a", admin.MaxAdminUsernameLength+1), handle, assertion)
	huge := createBody(strings.Repeat("a", 512<<10), handle, assertion)
	for name, body := range map[string]map[string]any{"unknown field": unknown, "long username": long, "oversized body": huge} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(engine, createPath, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	assert.Empty(t, createAuditRows(t, db, actingID), "refused input writes no audit row")

	username := uniqueAdminUsername("hygiene")
	rec := postJSON(engine, createPath, createBody(username, handle, assertion))
	require.Equal(t, http.StatusCreated, rec.Code, "the handle is unspent: %s", rec.Body.String())
	cleanupAdminNamed(t, db, username)
}
