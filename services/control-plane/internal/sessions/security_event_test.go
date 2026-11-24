package sessions

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

const (
	privacyUserID    = "user-7e4c92"
	privacySessionID = "session-6f4ab1"
	privacyPassword  = "correct-horse-battery-staple" // pragma: allowlist secret -- deterministic test-only password fixture
)

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

func requireSerializedEventPrivacy(t *testing.T, events []securityevent.Event, sensitiveValues ...string) {
	t.Helper()
	require.NotEmpty(t, events)
	serialized, err := json.Marshal(events)
	require.NoError(t, err)
	for _, fixture := range append(sensitiveValues, []string{
		"198.51.100.44", "email-fixture@example.test", "raw-error-fixture",
	}...) {
		require.NotContains(t, string(serialized), fixture)
	}
}

// Both arms use the same live family: the successful retry cannot be an
// idempotent no-op if the failing commit accidentally persisted its writes.
func TestExecuteRevocationEmitsOnlyAfterDurableUpdate(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	localFamily := rig.captureLocalFamily(t)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	body := `{"password":"` + privacyPassword + `"}`
	removeFault := installSecurityEventCommitFault(t, rig.db)
	failed := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, body)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, failed.Body.String())
	rig.requireFailedCommit(t, failed, localFamily)
	tracking, err := rig.redis.Exists(context.Background(), revokeTrackingKey(rig.user), simpleAuthWindowKey(rig.user, rig.familyID(t))).Result()
	require.NoError(t, err)
	require.Zero(t, tracking)
	var last sql.NullTime
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=$1`, rig.user).Scan(&last))
	require.False(t, last.Valid, "a rolled-back revoke must not spend the allowance")
	removeFault()
	succeeded := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, body)
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	require.JSONEq(t, `{"message":"Session revoked successfully","session_id":"`+rig.sid+`"}`, succeeded.Body.String())
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionDelete}}, rig.recorder.events)
	rig.requireCommittedOwner(t)
	requireSerializedEventPrivacy(t, rig.recorder.events, rig.user, rig.sid, rig.access, rig.refresh)
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=$1`, rig.user).Scan(&last))
	require.True(t, last.Valid, "the committed revoke must spend the allowance")
}

func TestUpdateRevocationModeEmitsOnlyAfterDurableUpdate(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	_, err := rig.db.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)
	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
	body := `{"mode":"simple","password":"` + privacyPassword + `"}`
	hook.Arm([]string{"UPDATE users SET revocation_mode=$1"}, nil, stmthook.ErrInjected)
	failed := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode", body)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
	require.Empty(t, rig.recorder.events)
	var mode string
	var last sql.NullTime
	var generation int64
	require.NoError(t, rig.db.QueryRow(`SELECT revocation_mode,last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&mode, &last, &generation))
	require.Equal(t, "secure", mode)
	require.True(t, last.Valid)
	require.Zero(t, generation)

	succeeded := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode", body)
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	require.NoError(t, rig.db.QueryRow(`SELECT revocation_mode,last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&mode, &last, &generation))
	require.Equal(t, "simple", mode)
	require.False(t, last.Valid)
	require.EqualValues(t, 1, generation)
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonRevocationModeChanged, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevocationMode}}, rig.recorder.events)
	requireSerializedEventPrivacy(t, rig.recorder.events, rig.user, privacyPassword)
}

func TestRevokeAllSessionsEmitsOnlyAfterCommittedBulkRevoke(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	localFamily := rig.captureLocalFamily(t)
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	ctx := context.Background()
	_, err := rig.db.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: 1, Member: "existing-revoke"}).Err())
	require.NoError(t, rig.redis.Set(ctx, simpleAuthWindowKey(rig.user, rig.familyID(t)), "1", 0).Err())
	removeFault := installSecurityEventCommitFault(t, rig.db)
	body := `{"password":"` + privacyPassword + `","include_current":true}`
	failed := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all", body)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, failed.Body.String())
	rig.requireFailedCommit(t, failed, localFamily)
	members, err := rig.redis.ZRange(ctx, revokeTrackingKey(rig.user), 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{"existing-revoke"}, members)
	window, err := rig.redis.Get(ctx, simpleAuthWindowKey(rig.user, rig.familyID(t))).Result()
	require.NoError(t, err)
	require.Equal(t, "1", window)
	var last sql.NullTime
	var generation int64
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&last, &generation))
	require.True(t, last.Valid)
	require.Zero(t, generation)
	removeFault()
	succeeded := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all", body)
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	require.JSONEq(t, `{"message":"All sessions revoked successfully","count":1,"include_current":true}`, succeeded.Body.String())
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonSessionsRevoked, AuthMethod: securityevent.AuthSession, RouteTemplate: securityevent.RouteSessionsRevokeAll}}, rig.recorder.events)
	rig.requireCommittedOwner(t)
	requireSerializedEventPrivacy(t, rig.recorder.events, rig.user, rig.sid, privacyPassword, rig.access, rig.refresh)
	remaining, err := rig.redis.Exists(ctx, revokeTrackingKey(rig.user), simpleAuthWindowKey(rig.user, rig.familyID(t))).Result()
	require.NoError(t, err)
	require.Zero(t, remaining)
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&last, &generation))
	require.False(t, last.Valid)
	require.EqualValues(t, 1, generation)
	cookies := succeeded.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "refresh_token", cookies[0].Name)
	require.Equal(t, -1, cookies[0].MaxAge)
}

type securityEventDisconnector struct {
	users         []uuid.UUID
	sessions      []string
	families      []uuid.UUID
	localFamilies []uuid.UUID
	snapshotErr   error
}

func (d *securityEventDisconnector) DisconnectFamilyContext(_ context.Context, _, family uuid.UUID) error {
	d.families = append(d.families, family)
	return nil
}

func (d *securityEventDisconnector) SnapshotUserFamiliesContext(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return append([]uuid.UUID(nil), d.localFamilies...), d.snapshotErr
}

func (d *securityEventDisconnector) DisconnectFamiliesContext(_ context.Context, _ uuid.UUID, families []uuid.UUID) error {
	d.families = append(d.families, families...)
	return nil
}

func (d *securityEventDisconnector) DisconnectUser(user uuid.UUID) { d.users = append(d.users, user) }
func (d *securityEventDisconnector) DisconnectSession(sid string) {
	d.sessions = append(d.sessions, sid)
}

type securityEventFamilyRig struct {
	db                         *sql.DB
	redis                      *redis.Client
	router                     *gin.Engine
	handler                    *Handler
	recorder                   *securityEventRecorder
	disconnector               *securityEventDisconnector
	user, sid, access, refresh string
}

func newSecurityEventFamilyRig(t *testing.T) securityEventFamilyRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	user := dbtest.CreateUser(t, db).String()
	hash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, user, hash)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO user_keys(user_id,wrapped_private_key,key_derivation_salt,key_version,key_derivation_alg) VALUES($1,$2,$3,1,'argon2id')`, user, []byte{1}, []byte{2})
	require.NoError(t, err)
	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(db, rdb, logger.New("test"), secret, testDisconnector{})
	access, refresh, sid, err := minter.IssueAccessAndRefresh(context.Background(), user)
	require.NoError(t, err)
	disconnector := &securityEventDisconnector{}
	handler := NewHandler(db, rdb, logger.New("test"), disconnector, nil)
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	router := gin.New()
	router.Use(middleware.AuthRequired(secret, rdb, db, nil))
	return securityEventFamilyRig{db: db, redis: rdb, router: router, handler: handler, recorder: recorder, disconnector: disconnector, user: user, sid: sid, access: access, refresh: refresh}
}

func (r securityEventFamilyRig) captureLocalFamily(t *testing.T) uuid.UUID {
	t.Helper()
	var family uuid.UUID
	require.NoError(t, r.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, r.sid).Scan(&family))
	r.disconnector.localFamilies = []uuid.UUID{family}
	return family
}

// familyID returns the login family that owns the rig's session.
func (r securityEventFamilyRig) familyID(t *testing.T) string {
	t.Helper()
	return familyOf(t, r, r.sid)
}

func familyOf(t *testing.T, rig securityEventFamilyRig, sid string) string {
	t.Helper()
	var family string
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid = $1`, sid).Scan(&family))
	return family
}

func sessionLive(t *testing.T, rig securityEventFamilyRig, sid string) bool {
	t.Helper()
	var live bool
	require.NoError(t, rig.db.QueryRow(
		`SELECT f.terminal_at IS NULL FROM login_family_sids s JOIN login_families f ON f.family_id = s.family_id WHERE s.sid = $1`, sid,
	).Scan(&live))
	return live
}

// sendAs serves one request authenticated by access, a token of any family of
// the rig's user.
func (r securityEventFamilyRig) sendAs(access, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+access)
	response := httptest.NewRecorder()
	r.router.ServeHTTP(response, req)
	return response
}

// issueFamilyAccess mints a second login family of the rig's user and returns
// its access token and session ID.
func issueFamilyAccess(t *testing.T, rig securityEventFamilyRig) (access, sid string) {
	t.Helper()
	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
	access, _, sid, err := minter.IssueAccessAndRefresh(context.Background(), rig.user)
	require.NoError(t, err)
	return access, sid
}

const passwordRequiredBody = `{"error":"password_required","message":"Password verification required to revoke this session"}` // #nosec G101 -- refusal response body, not a credential

// VULN-014 regression: a Simple-mode revoke window belongs to the login family
// that proved its password. Family B proves and earns a window; family A, a
// second session of the same account, must still prove before it can revoke.
// The control (family B revoking the same target without a password) shows the
// window exists, so A's refusal comes from the family scope and nothing else.
func TestSimpleWindowIsPerLoginFamily(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	rig.handler.hub = testDisconnector{}
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	ctx := context.Background()
	// The day's free revoke is spent, so only a window can waive the password.
	_, err := rig.db.Exec(`UPDATE users SET revocation_mode='simple', last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)
	accessB, sidB := issueFamilyAccess(t, rig)
	targets := issueRevocationTargets(t, rig, 2)

	proved := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[0], `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, proved.Code, proved.Body.String())

	refused := rig.sendAs(rig.access, http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
	require.Equal(t, http.StatusForbidden, refused.Code, "family A must not spend family B's window: %s", refused.Body.String())
	require.JSONEq(t, passwordRequiredBody, refused.Body.String())
	require.True(t, sessionLive(t, rig, targets[1]), "a refused revoke must leave its target live")

	var generation int64
	require.NoError(t, rig.db.QueryRow(`SELECT session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&generation))
	windowB, err := rig.redis.Get(ctx, simpleAuthWindowKey(rig.user, familyOf(t, rig, sidB))).Result()
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%s%d", simpleAuthWindowVersion, generation), windowB, "the window holds the generation it was earned under")
	for _, key := range []string{simpleAuthWindowKey(rig.user, rig.familyID(t)), legacySimpleAuthWindowKey(rig.user)} {
		exists, err := rig.redis.Exists(ctx, key).Result()
		require.NoError(t, err)
		require.Zero(t, exists, "only family B earned a window: %s", key)
	}

	control := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
	require.Equal(t, http.StatusOK, control.Code, "family B's own window waives its password: %s", control.Body.String())
	require.False(t, sessionLive(t, rig, targets[1]))
}

// A revocation-mode change ends every login family's Simple-mode window, not
// only the requester's. Family B proves and earns a window; family A switches
// the account to secure and back to simple. B's earlier window must no longer
// waive its password. The control shows B's window was live before the change,
// so the refusal comes from the mode change and nothing else.
func TestSimpleWindowEndsForEveryFamilyOnModeChange(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	rig.handler.hub = testDisconnector{}
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
	_, err := rig.db.Exec(`UPDATE users SET revocation_mode='simple', last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)
	accessB, _ := issueFamilyAccess(t, rig)
	targets := issueRevocationTargets(t, rig, 3)
	withPassword := `{"password":"` + privacyPassword + `"}`

	proved := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[0], withPassword)
	require.Equal(t, http.StatusOK, proved.Code, proved.Body.String())
	control := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
	require.Equal(t, http.StatusOK, control.Code, "family B's own window waives its password before any mode change: %s", control.Body.String())

	for _, mode := range []string{"secure", "simple"} {
		changed := rig.sendAs(rig.access, http.MethodPut, "/api/v1/sessions/revocation-mode", `{"mode":"`+mode+`","password":"`+privacyPassword+`"}`)
		require.Equal(t, http.StatusOK, changed.Code, "switch to %s: %s", mode, changed.Body.String())
	}
	// A mode change also restores the day's free revoke. Spend it again
	// without touching the generation, so only a window could waive the
	// password below.
	_, err = rig.db.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)

	refused := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[2], "")
	require.Equal(t, http.StatusForbidden, refused.Code, "a window earned before a mode change must not survive it: %s", refused.Body.String())
	require.JSONEq(t, passwordRequiredBody, refused.Body.String())
	require.True(t, sessionLive(t, rig, targets[2]), "a refused revoke must leave its target live")

	reproved := rig.sendAs(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[2], withPassword)
	require.Equal(t, http.StatusOK, reproved.Code, "a fresh proof under the new mode still revokes: %s", reproved.Body.String())
}

func (r securityEventFamilyRig) request(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.access)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: r.refresh, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	response := httptest.NewRecorder()
	r.router.ServeHTTP(response, req)
	return response
}

// A deferred trigger runs at COMMIT after the refresh and family mutations.
// Sequence advancement survives rollback and proves this exact fault was reached.
func installSecurityEventCommitFault(t *testing.T, db *sql.DB) func() {
	t.Helper()
	_, err := db.Exec(`CREATE SEQUENCE security_event_commit_fault_seq;
 CREATE FUNCTION security_event_commit_fault() RETURNS trigger AS $$
 BEGIN
  IF NOT EXISTS (SELECT 1 FROM login_families f JOIN login_family_revocations o ON o.operation_id=f.auth_operation_id
    JOIN refresh_tokens r ON r.user_id=f.user_id JOIN login_family_sids s ON s.sid=r.id AND s.family_id=f.family_id
    WHERE f.family_id=NEW.family_id AND f.terminal_at IS NOT NULL AND o.sealed_at IS NOT NULL AND r.revoked_at IS NOT NULL) THEN
   RAISE EXCEPTION 'commit fault reached before complete family mutation';
  END IF;
  PERFORM nextval('security_event_commit_fault_seq');
  RAISE EXCEPTION 'raw-error-fixture';
 END; $$ LANGUAGE plpgsql;
 CREATE CONSTRAINT TRIGGER security_event_commit_fault AFTER INSERT ON login_family_revocation_targets
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION security_event_commit_fault()`)
	require.NoError(t, err)
	removed := false
	remove := func() {
		if removed {
			return
		}
		_, err := db.Exec(`DROP TRIGGER security_event_commit_fault ON login_family_revocation_targets; DROP FUNCTION security_event_commit_fault(); DROP SEQUENCE security_event_commit_fault_seq`)
		require.NoError(t, err)
		removed = true
	}
	t.Cleanup(remove)
	return remove
}

func (r securityEventFamilyRig) requireFailedCommit(t *testing.T, response *httptest.ResponseRecorder, localFamily uuid.UUID) {
	t.Helper()
	var called bool
	require.NoError(t, r.db.QueryRow(`SELECT is_called FROM security_event_commit_fault_seq`).Scan(&called))
	require.True(t, called, "must reach deferred commit fault after family and refresh mutations")
	require.Empty(t, r.recorder.events)
	require.Empty(t, r.disconnector.users)
	require.Empty(t, r.disconnector.sessions)
	require.Equal(t, []uuid.UUID{localFamily}, r.disconnector.families, "an ambiguous commit must fence the captured local family")
	require.Empty(t, response.Header().Values("Set-Cookie"))
	var count int
	for _, query := range []string{
		`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NOT NULL`,
		`SELECT count(*) FROM login_families WHERE user_id=$1 AND (terminal_at IS NOT NULL OR auth_operation_id IS NOT NULL OR epoch_generation IS NOT NULL)`,
		`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`,
		`SELECT count(*) FROM login_family_revocation_targets WHERE user_id=$1`,
	} {
		require.NoError(t, r.db.QueryRow(query, r.user).Scan(&count))
		require.Zero(t, count, query)
	}
}

func (r securityEventFamilyRig) requireCommittedOwner(t *testing.T) {
	t.Helper()
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM refresh_tokens r
 JOIN login_family_sids s ON s.sid=r.id AND s.user_id=r.user_id
 JOIN login_families f ON f.family_id=s.family_id AND f.user_id=s.user_id
 JOIN login_family_revocation_targets t ON t.family_id=f.family_id AND t.user_id=f.user_id AND t.operation_id=f.auth_operation_id
 JOIN login_family_revocations o ON o.operation_id=t.operation_id AND o.user_id=t.user_id
 WHERE r.id=$1 AND r.user_id=$2 AND r.revoked_at IS NOT NULL AND f.terminal_at IS NOT NULL
 AND f.epoch_generation IS NULL AND o.sealed_at IS NOT NULL`, r.sid, r.user).Scan(&count))
	require.Equal(t, 1, count, "committed refresh revocation must have its exact sealed durable family owner")
}

type revokeMFAVerifier struct {
	enabled   bool
	valid     bool
	verifyErr error
}

func (v revokeMFAVerifier) IsEnabled(context.Context, string) bool { return v.enabled }
func (v revokeMFAVerifier) VerifyCode(context.Context, string, stepup.Purpose, string) (bool, error) {
	return v.valid, v.verifyErr
}
func (revokeMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return []string{"totp"}, nil
}

type methodsFaultMFAVerifier struct{ revokeMFAVerifier }

func (methodsFaultMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, errors.New("methods read failed")
}

// A failed method read is a 500, never the empty list that would tell the
// client the account has no factor to offer.
func TestAuthenticateForRevoke_MethodsReadFailureIs500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	db := sql.OpenDB(execConnector{passwordHash: passwordHash})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	h := NewHandler(db, rdb, logger.New("test"), testDisconnector{}, methodsFaultMFAVerifier{revokeMFAVerifier{enabled: true}})
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/sessions/revocation-mode", nil)

	require.True(t, h.authenticateForRevoke(context.Background(), c, privacyUserID, privacyPassword, "", revocationModeSetAction))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Contains(t, response.Body.String(), errMsgMFAVerificationFailed)
	require.NotContains(t, response.Body.String(), "auth_required")
}

func TestAuthenticateForRevokeEmitsCredentialSpecificRefusalEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)

	for _, test := range []struct {
		name   string
		action revokeAction
	}{
		{name: "delete", action: revokeSessionAction},
		{name: "revoke all", action: revokeAllSessionsAction},
		{name: "revocation mode", action: revocationModeSetAction},
	} {
		t.Run(test.name, func(t *testing.T) {
			newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
				return c, response
			}

			db := sql.OpenDB(execConnector{passwordHash: passwordHash})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			lockoutRedis := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			t.Cleanup(func() { require.NoError(t, lockoutRedis.Close()) })
			passwordHandler := NewHandler(db, lockoutRedis, logger.New("test"), testDisconnector{}, revokeMFAVerifier{})
			passwordRecorder := &securityEventRecorder{}
			passwordHandler.SetSecurityEvents(passwordRecorder)
			passwordContext, passwordResponse := newContext()
			require.True(t, passwordHandler.authenticateForRevoke(context.Background(), passwordContext, privacyUserID, "wrong", "", test.action))
			require.Equal(t, http.StatusForbidden, passwordResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials,
				AuthMethod: securityevent.AuthPassword, RouteTemplate: test.action.route,
			}}, passwordRecorder.events)
			require.True(t, middleware.NightwatchHandled(passwordContext))

			mfaHandler := NewHandler(db, lockoutRedis, logger.New("test"), testDisconnector{}, revokeMFAVerifier{enabled: true})
			mfaRecorder := &securityEventRecorder{}
			mfaHandler.SetSecurityEvents(mfaRecorder)
			mfaContext, mfaResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), mfaContext, privacyUserID, privacyPassword, "bad-code", test.action))
			require.Equal(t, http.StatusForbidden, mfaResponse.Code)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied,
				Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid,
				RouteTemplate: test.action.route,
			}}, mfaRecorder.events)
			require.True(t, middleware.NightwatchHandled(mfaContext))

			noCredentialContext, noCredentialResponse := newContext()
			require.True(t, mfaHandler.authenticateForRevoke(context.Background(), noCredentialContext, privacyUserID, "", "", test.action))
			require.Equal(t, http.StatusForbidden, noCredentialResponse.Code)
			require.False(t, middleware.NightwatchHandled(noCredentialContext))
		})
	}
}

func TestAuthenticateForRevoke_MFARequiresPasswordAndUsesSharedBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	db := sql.OpenDB(execConnector{passwordHash: passwordHash})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	h := NewHandler(db, rdb, logger.New("test"), testDisconnector{}, revokeMFAVerifier{enabled: true})

	request := func(password, code string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
		require.True(t, h.authenticateForRevoke(context.Background(), c, privacyUserID, password, code, revokeAllSessionsAction))
		return response
	}

	// The three revocation endpoints share the account-wide budget. Exercise
	// distinct action descriptors here so a future route-local key cannot hide
	// behind the common helper.
	actions := []revokeAction{revokeSessionAction, revokeAllSessionsAction, revocationModeSetAction}
	for i := 0; i < stepup.BudgetLimit; i++ {
		action := actions[i%len(actions)]
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
		require.True(t, h.authenticateForRevoke(context.Background(), c, privacyUserID, privacyPassword, "bad-code", action))
		require.Equal(t, http.StatusForbidden, response.Code)
	}
	denied := request(privacyPassword, "bad-code") // sixth request, different from the final loop action
	require.Equal(t, http.StatusTooManyRequests, denied.Code)
	require.NotEmpty(t, denied.Header().Get("Retry-After"))
	require.Contains(t, denied.Body.String(), "step_up_budget_exhausted")

	passwordOnly := request(privacyPassword, "")
	require.Equal(t, http.StatusForbidden, passwordOnly.Code)
	require.NotContains(t, passwordOnly.Body.String(), "step_up_budget")
	count, err := rdb.Get(context.Background(), stepup.DangerousActionBudget(rdb).Key(privacyUserID)).Int()
	require.NoError(t, err)
	require.Equal(t, stepup.BudgetLimit+1, count, "a correct password without MFA must not clear or charge the MFA budget")
}

func TestAuthenticateForRevoke_MFABudgetUnavailableIs503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	db := sql.OpenDB(execConnector{passwordHash: passwordHash})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	rdb.AddHook(revokeBudgetIncrFaultHook{key: stepup.DangerousActionBudget(rdb).Key(privacyUserID)})
	h := NewHandler(db, rdb, logger.New("test"), testDisconnector{}, revokeMFAVerifier{enabled: true})
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)

	require.True(t, h.authenticateForRevoke(context.Background(), c, privacyUserID, privacyPassword, "bad-code", revokeAllSessionsAction))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "step_up_budget_unavailable")
}

func TestAuthenticateForRevoke_VerifierResultControlsRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	passwordHash, err := auth.HashPasswordWithParams(privacyPassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		verifier   revokeMFAVerifier
		wantStatus int
		blocked    bool
		verified   bool
	}{
		{name: "valid factor", verifier: revokeMFAVerifier{enabled: true, valid: true}, wantStatus: http.StatusOK, verified: true},
		{name: "verifier fault", verifier: revokeMFAVerifier{enabled: true, verifyErr: errors.New("verification unavailable")}, wantStatus: http.StatusInternalServerError, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sql.OpenDB(execConnector{passwordHash: passwordHash})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			mini := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			h := NewHandler(db, rdb, logger.New("test"), testDisconnector{}, tc.verifier)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
			blocked, verified := h.authenticateForRevokeWithMFA(context.Background(), c, privacyUserID, privacyPassword, "123456", revokeAllSessionsAction)
			require.Equal(t, tc.blocked, blocked)
			require.Equal(t, tc.verified, verified)
			require.Equal(t, tc.wantStatus, response.Code)
		})
	}
}

func TestUpdateRevocationMode_ValidMFAClearsSharedBudget(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	key := stepup.DangerousActionBudget(rig.redis).Key(rig.user)
	require.NoError(t, rig.redis.Set(context.Background(), key, "4", stepup.BudgetWindow).Err())
	rig.handler.mfaVerifier = revokeMFAVerifier{enabled: true, valid: true}
	rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
	response := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode",
		`{"mode":"simple","password":"`+privacyPassword+`","mfa_code":"123456"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	remaining, err := rig.redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Zero(t, remaining, "successful password-and-MFA update clears the shared budget")
}

type revokeBudgetIncrFaultHook struct{ key string }

func (revokeBudgetIncrFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h revokeBudgetIncrFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "incr" {
			if args := cmd.Args(); len(args) >= 2 {
				if key, ok := args[1].(string); ok && key == h.key {
					err := errors.New("forced session MFA budget failure")
					cmd.SetErr(err)
					return err
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (revokeBudgetIncrFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type testDisconnector struct{}

func (testDisconnector) DisconnectUser(uuid.UUID) {}
func (testDisconnector) DisconnectSession(string) {}
func (testDisconnector) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (testDisconnector) SnapshotUserFamiliesContext(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func (testDisconnector) DisconnectFamiliesContext(context.Context, uuid.UUID, []uuid.UUID) error {
	return nil
}

type execConnector struct {
	err          error
	commitErr    error
	rows         int64
	passwordHash string
}

func (c execConnector) Connect(context.Context) (driver.Conn, error) {
	return execConn{connector: c}, nil
}
func (c execConnector) Driver() driver.Driver { return execDriver{connector: c} }

type execDriver struct{ connector execConnector }

func (d execDriver) Open(string) (driver.Conn, error) { return execConn(d), nil }

type execConn struct{ connector execConnector }

func (c execConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c execConn) Close() error                        { return nil }
func (c execConn) Begin() (driver.Tx, error)           { return execTx(c), nil }
func (c execConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if c.connector.err != nil {
		return nil, c.connector.err
	}
	return driver.RowsAffected(c.connector.rows), nil
}

func (c execConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "credential_epoch") {
		return &oneSessionValueRow{value: nil}, nil
	}
	if c.connector.passwordHash == "" {
		return noSessionRows{}, nil
	}
	if strings.Contains(query, "SELECT email, password_hash") {
		return &emailHashRow{hash: c.connector.passwordHash}, nil
	}
	return &oneSessionStringRow{value: c.connector.passwordHash}, nil
}

// emailHashRow answers verifyUserPassword's lockout-keyed read.
type emailHashRow struct {
	hash string
	sent bool
}

func (emailHashRow) Columns() []string { return []string{"email", "password_hash"} }
func (emailHashRow) Close() error      { return nil }
func (r *emailHashRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	dest[0], dest[1] = "revoke-events@example.test", r.hash
	r.sent = true
	return nil
}

type execTx struct{ connector execConnector }

func (tx execTx) Commit() error { return tx.connector.commitErr }
func (execTx) Rollback() error  { return nil }

type noSessionRows struct{}

func (noSessionRows) Columns() []string         { return []string{"password_hash"} }
func (noSessionRows) Close() error              { return nil }
func (noSessionRows) Next([]driver.Value) error { return io.EOF }

type oneSessionStringRow struct {
	value string
	sent  bool
}

type oneSessionValueRow struct {
	value any
	sent  bool
}

func (r oneSessionValueRow) Columns() []string { return []string{"credential_epoch"} }
func (oneSessionValueRow) Close() error        { return nil }
func (r *oneSessionValueRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	dest[0] = r.value
	r.sent = true
	return nil
}

func (r oneSessionStringRow) Columns() []string { return []string{"password_hash"} }
func (r oneSessionStringRow) Close() error      { return nil }
func (r *oneSessionStringRow) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	dest[0] = r.value
	r.sent = true
	return nil
}

// A Simple-mode window waives a password only against a generation read under
// the revocation's user lock. When that read fails, even a family holding a
// window that matches the stored generation gets no revoke, free or waived.
// The control shows the same window waives the password when the read works.
func TestUnreadableRevokeGenerationGrantsNoWaivedRevoke(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "control"
		if fault {
			name = "generation unreadable"
		}
		t.Run(name, func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			target := issueRevocationTargets(t, rig, 1)[0]
			_, err := rig.db.Exec(`UPDATE users SET revocation_mode='simple', last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
			require.NoError(t, err)
			var generation int64
			require.NoError(t, rig.db.QueryRow(`SELECT session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&generation))
			require.NoError(t, rig.redis.Set(context.Background(), simpleAuthWindowKey(rig.user, rig.familyID(t)), fmt.Sprintf("%s%d", simpleAuthWindowVersion, generation), 0).Err())
			hook, hookedDB := stmthook.Open(t)
			rig.handler.db = hookedDB
			rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
			if fault {
				hook.Arm([]string{`SELECT revocation_mode, session_revoke_auth_gen`}, nil, stmthook.ErrInjected)
			}

			response := rig.request(http.MethodDelete, "/api/v1/sessions/"+target, "")
			if !fault {
				require.Equal(t, http.StatusOK, response.Code, "the matching window waives the password: %s", response.Body.String())
				requireRefreshRevoked(t, rig, target, true)
				return
			}
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, hookErr)
			require.True(t, sessionLive(t, rig, target), "a refused revoke must leave its target live")
		})
	}
}
