package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func issueRevocationTargets(t *testing.T, rig securityEventFamilyRig, count int) []string {
	t.Helper()
	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
	targets := make([]string, 0, count)
	for range count {
		_, _, sid, err := minter.IssueAccessAndRefresh(context.Background(), rig.user)
		require.NoError(t, err)
		targets = append(targets, sid)
	}
	return targets
}

func requireRefreshRevoked(t *testing.T, rig securityEventFamilyRig, sid string, revoked bool) {
	t.Helper()
	var actual bool
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM refresh_tokens WHERE id=$1`, sid).Scan(&actual))
	require.Equal(t, revoked, actual)
	require.NoError(t, rig.db.QueryRow(`SELECT f.terminal_at IS NOT NULL FROM login_families f
 JOIN login_family_sids s USING (family_id,user_id) WHERE s.sid=$1`, sid).Scan(&actual))
	require.Equal(t, revoked, actual)
}

func deadRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { require.NoError(t, dead.Close()) })
	return dead
}

func TestRevokeSession_FreeRevokeDoesNotDependOnRedis(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)

	// Middleware retains its working Redis connection; the handler's allowance
	// path alone is isolated from Redis.
	rig.handler.redis = deadRedisClient(t)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requireRefreshRevoked(t, rig, targets[0], true)
	var last sql.NullTime
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=$1`, rig.user).Scan(&last))
	require.True(t, last.Valid)
}

func TestRevokeAllowance_ConcurrentRequestsSpendOnce(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 6)
	rig.handler.hub = testDisconnector{}
	rig.handler.SetSecurityEvents(nil)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)

	type outcome struct {
		sid, body string
		status    int
	}
	start := make(chan struct{})
	results := make(chan outcome, len(targets))
	for _, sid := range targets {
		go func() {
			<-start
			response := rig.request(http.MethodDelete, "/api/v1/sessions/"+sid, "")
			results <- outcome{sid: sid, status: response.Code, body: response.Body.String()}
		}()
	}
	close(start)
	succeeded, denied := 0, 0
	for range targets {
		result := <-results
		switch result.status {
		case http.StatusOK:
			succeeded++
			requireRefreshRevoked(t, rig, result.sid, true)
		case http.StatusForbidden:
			denied++
			require.Contains(t, result.body, `"error":"password_required"`)
			requireRefreshRevoked(t, rig, result.sid, false)
		default:
			t.Fatalf("unexpected status %d: %s", result.status, result.body)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, len(targets)-1, denied)
}

func TestRevokeAllowance_WindowBoundary(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)

	_, err := rig.db.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp()-INTERVAL '24 hours'+INTERVAL '1 minute' WHERE id=$1`, rig.user)
	require.NoError(t, err)
	denied := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	requireRefreshRevoked(t, rig, targets[0], false)

	_, err = rig.db.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp()-INTERVAL '24 hours' WHERE id=$1`, rig.user)
	require.NoError(t, err)
	free := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusOK, free.Code, free.Body.String())
	requireRefreshRevoked(t, rig, targets[0], true)
}

func TestRevokeAllowance_AuthenticatedRevokeSpendsWindow(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	target := issueRevocationTargets(t, rig, 1)[0]
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	paid := rig.request(http.MethodDelete, "/api/v1/sessions/"+target, `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
	requireRefreshRevoked(t, rig, target, true)
	targets := issueRevocationTargets(t, rig, 2)
	denied := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	requireRefreshRevoked(t, rig, targets[0], false)
}

func TestRevokeAllowance_CommitAmbiguityKeepsDatabaseOutcome(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			targets := issueRevocationTargets(t, rig, 3)
			hook, hookedDB := stmthook.Open(t)
			rig.handler.db = hookedDB
			rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
			hook.Arm([]string{`UPDATE login_family_revocations SET sealed_at=clock_timestamp()`}, nil, nil)
			hook.ArmCommit(committed, stmthook.ErrInjected)

			response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 1, seen, "the family mutation must reach its commit")
			require.NoError(t, hookErr)
			requireRefreshRevoked(t, rig, targets[0], committed)
			var last sql.NullTime
			require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=$1`, rig.user).Scan(&last))
			require.Equal(t, committed, last.Valid)

			if committed {
				response = rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
				requireRefreshRevoked(t, rig, targets[1], false)
			} else {
				response = rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				requireRefreshRevoked(t, rig, targets[0], true)
			}
		})
	}
}

func TestRevokeAllowance_DatabaseFailureDoesNotSpend(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)
	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	hook.Arm([]string{`SELECT revocation_mode, session_revoke_auth_gen`}, nil, stmthook.ErrInjected)

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
	requireRefreshRevoked(t, rig, targets[0], false)
	require.Empty(t, rig.disconnector.families, "pre-commit failures must not fence live sessions")
	var last sql.NullTime
	require.NoError(t, rig.db.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=$1`, rig.user).Scan(&last))
	require.False(t, last.Valid)
}

func TestRevokeSession_AmbiguousCommitFencesExactTargetWhenOwnerLookupFails(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			targets := issueRevocationTargets(t, rig, 2)
			var targetFamily, currentFamily, otherFamily uuid.UUID
			require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, targets[0]).Scan(&targetFamily))
			require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, targets[1]).Scan(&otherFamily))
			require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, rig.sid).Scan(&currentFamily))
			hook, hookedDB := stmthook.Open(t)
			rig.handler.db = hookedDB
			rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
			hook.Arm([]string{`UPDATE login_family_revocations SET sealed_at=clock_timestamp()`,
				"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)
			hook.ArmCommit(committed, stmthook.ErrInjected)

			response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 2, seen)
			require.NoError(t, hookErr)
			require.Equal(t, []uuid.UUID{targetFamily}, rig.disconnector.families)
			require.NotContains(t, rig.disconnector.families, currentFamily)
			require.NotContains(t, rig.disconnector.families, otherFamily)
			requireRefreshRevoked(t, rig, targets[0], committed)
			requireRefreshRevoked(t, rig, rig.sid, false)
			requireRefreshRevoked(t, rig, targets[1], false)
		})
	}
}

func TestHandleRevokeSessionErrorReportsUnconfirmedAmbiguousFence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	handler := &Handler{log: logger.New("test")}
	target := loginfamily.Binding{UserID: uuid.New(), FamilyID: uuid.New()}

	handler.handleRevokeSessionError(context.Background(), ctx, target.UserID.String(),
		singleRevocationResult{target: target},
		loginfamily.NewError(loginfamily.ReasonCommitUnknown, fmt.Errorf("commit result unknown")))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestHandleRevokeSessionErrorReportsUnexpectedFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	handler := &Handler{log: logger.New("test")}

	handler.handleRevokeSessionError(context.Background(), ctx, uuid.NewString(), singleRevocationResult{}, fmt.Errorf("unexpected failure"))

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
}

func TestRevokeSession_PostCommitLookupFailureFencesTarget(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)
	var targetFamily, currentFamily uuid.UUID
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, targets[0]).Scan(&targetFamily))
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, rig.sid).Scan(&currentFamily))
	require.NotEqual(t, currentFamily, targetFamily)

	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen, "the owner lookup must fail after the revocation commits")
	require.NoError(t, hookErr)
	require.Equal(t, []uuid.UUID{targetFamily}, rig.disconnector.families)
	requireRefreshRevoked(t, rig, targets[0], true)
	requireRefreshRevoked(t, rig, rig.sid, false)
	requireRefreshRevoked(t, rig, targets[1], false)
}

func TestRevokeAll_PostCommitLookupFailureReportsPendingFence(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)
	targetFamilies := make([]uuid.UUID, 0, len(targets))
	for _, target := range targets {
		var family uuid.UUID
		require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, target).Scan(&family))
		targetFamilies = append(targetFamilies, family)
	}
	var currentFamily uuid.UUID
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, rig.sid).Scan(&currentFamily))
	unknownFamily := uuid.New()
	rig.disconnector.localFamilies = append(append([]uuid.UUID(nil), targetFamilies...), currentFamily, unknownFamily)
	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	var freshSID string
	var freshFamily uuid.UUID
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, func() error {
		freshSID = issueRevocationTargets(t, rig, 1)[0]
		if err := rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, freshSID).Scan(&freshFamily); err != nil {
			return err
		}
		rig.disconnector.localFamilies = append(rig.disconnector.localFamilies, freshFamily)
		return nil
	}, stmthook.ErrInjected)

	response := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
		`{"password":"`+privacyPassword+`","include_current":false}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "revoked")
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen, "the owner lookup must fail after the revocation commits")
	require.NoError(t, hookErr)
	require.ElementsMatch(t, targetFamilies, rig.disconnector.families)
	require.NotContains(t, rig.disconnector.families, currentFamily)
	require.NotContains(t, rig.disconnector.families, unknownFamily)
	require.NotContains(t, rig.disconnector.families, freshFamily)
	for _, target := range targets {
		requireRefreshRevoked(t, rig, target, true)
	}
	requireRefreshRevoked(t, rig, rig.sid, false)
	requireRefreshRevoked(t, rig, freshSID, false)
}

func TestRevokeAll_SnapshotFailureRollsBackBeforeFencing(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	target := issueRevocationTargets(t, rig, 1)[0]
	rig.disconnector.snapshotErr = stmthook.ErrInjected
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)

	response := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
		`{"password":"`+privacyPassword+`","include_current":false}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Empty(t, rig.disconnector.families)
	requireRefreshRevoked(t, rig, target, false)
	requireRefreshRevoked(t, rig, rig.sid, false)
}

func TestRevokeAll_AmbiguousCommitFencesCapturedFamiliesWhenLookupFails(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			target := issueRevocationTargets(t, rig, 1)[0]
			var targetFamily, currentFamily uuid.UUID
			require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, target).Scan(&targetFamily))
			require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, rig.sid).Scan(&currentFamily))
			rig.disconnector.localFamilies = []uuid.UUID{targetFamily, currentFamily}
			hook, hookedDB := stmthook.Open(t)
			rig.handler.db = hookedDB
			rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
			hook.Arm([]string{`UPDATE login_family_revocations SET sealed_at=clock_timestamp()`,
				"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)
			hook.ArmCommit(committed, stmthook.ErrInjected)

			response := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
				`{"password":"`+privacyPassword+`","include_current":false}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 2, seen)
			require.NoError(t, hookErr)
			require.Equal(t, []uuid.UUID{targetFamily}, rig.disconnector.families)
			require.NotContains(t, rig.disconnector.families, currentFamily)
			requireRefreshRevoked(t, rig, target, committed)
			requireRefreshRevoked(t, rig, rig.sid, false)
		})
	}
}

func TestRevokeAll_FenceFailureStillClearsRevokedCurrentCookie(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	hook.Arm([]string{"SELECT family_id FROM login_families\n WHERE auth_operation_id=$1"}, nil, stmthook.ErrInjected)

	response := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
		`{"password":"`+privacyPassword+`","include_current":true}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
	requireRefreshRevoked(t, rig, rig.sid, true)
	cookies := response.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "refresh_token", cookies[0].Name)
	require.Equal(t, -1, cookies[0].MaxAge)
}

func TestRevokeSession_PreviouslyTerminalTargetAlsoFencesOriginalOwner(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 2)
	var firstFamily, secondFamily, currentFamily uuid.UUID
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, targets[0]).Scan(&firstFamily))
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, targets[1]).Scan(&secondFamily))
	require.NoError(t, rig.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid=$1`, rig.sid).Scan(&currentFamily))
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)

	all := rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
		`{"password":"`+privacyPassword+`","include_current":false}`)
	require.Equal(t, http.StatusOK, all.Code, all.Body.String())
	rig.disconnector.families = nil
	single := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, single.Code, single.Body.String())
	require.ElementsMatch(t, []uuid.UUID{firstFamily, secondFamily}, rig.disconnector.families)
	require.NotContains(t, rig.disconnector.families, currentFamily)
}

type fenceContextProbe struct {
	err          error
	hasDeadline  bool
	user, family uuid.UUID
}

func (p *fenceContextProbe) DisconnectFamilyContext(ctx context.Context, user, family uuid.UUID) error {
	p.err = ctx.Err()
	_, p.hasDeadline = ctx.Deadline()
	p.user, p.family = user, family
	return p.err
}

func (p *fenceContextProbe) SnapshotUserFamiliesContext(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func (p *fenceContextProbe) DisconnectFamiliesContext(ctx context.Context, user uuid.UUID, families []uuid.UUID) error {
	for _, family := range families {
		if err := p.DisconnectFamilyContext(ctx, user, family); err != nil {
			return err
		}
	}
	return nil
}

func TestCloseCommittedFamilyDetachesCanceledRequest(t *testing.T) {
	probe := &fenceContextProbe{}
	handler := &Handler{hub: probe}
	target := loginfamily.Binding{UserID: uuid.New(), FamilyID: uuid.New()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, handler.closeCommittedFamily(ctx, target))
	require.NoError(t, probe.err)
	require.True(t, probe.hasDeadline)
	require.Equal(t, target.UserID, probe.user)
	require.Equal(t, target.FamilyID, probe.family)
}

func TestSimpleMode_LegacyAndResetWindowsCannotBypassAllowance(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	targets := issueRevocationTargets(t, rig, 3)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	rig.router.POST("/api/v1/sessions/revoke-all", rig.handler.RevokeAllSessions)
	_, err := rig.db.Exec(`UPDATE users SET revocation_mode='simple',last_session_revoke_at=clock_timestamp() WHERE id=$1`, rig.user)
	require.NoError(t, err)
	require.NoError(t, rig.redis.Set(context.Background(), simpleAuthWindowKey(rig.user), "1", time.Minute).Err())

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], "")
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[0], `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	oldWindow, err := rig.redis.Get(context.Background(), simpleAuthWindowKey(rig.user)).Result()
	require.NoError(t, err)
	require.Equal(t, "v2:0", oldWindow)
	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	response = rig.request(http.MethodPost, "/api/v1/sessions/revoke-all",
		`{"password":"`+privacyPassword+`","include_current":false}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	// Model an old grant arriving after revoke-all removed its Redis key.
	require.NoError(t, rig.redis.Set(context.Background(), simpleAuthWindowKey(rig.user), oldWindow, time.Minute).Err())
	fresh := issueRevocationTargets(t, rig, 3)
	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+fresh[0], "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+fresh[1], "")
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	requireRefreshRevoked(t, rig, fresh[1], false)

	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+fresh[1], `{"password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = rig.request(http.MethodDelete, "/api/v1/sessions/"+fresh[2], "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestUpdateRevocationMode_RevokedActorCannotResetAllowance(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	var before time.Time
	require.NoError(t, rig.db.QueryRow(`UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1 RETURNING last_session_revoke_at`, rig.user).Scan(&before))
	hook, hookedDB := stmthook.Open(t)
	rig.handler.db = hookedDB
	rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
	hook.Arm([]string{`SELECT email, password_hash FROM users WHERE id = $1`}, func() error {
		_, err := auth.RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, auth.RevokeAllRefreshTokensOptions{})
		return err
	}, nil)

	response := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode",
		`{"mode":"simple","password":"`+privacyPassword+`"}`)
	require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	seen, hookErr := hook.Report()
	require.Equal(t, 1, seen)
	require.NoError(t, hookErr)
	var mode string
	var last time.Time
	var generation int64
	require.NoError(t, rig.db.QueryRow(`SELECT revocation_mode,last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&mode, &last, &generation))
	require.Equal(t, "secure", mode)
	require.True(t, last.Equal(before))
	require.Zero(t, generation)
}

func TestUpdateRevocationMode_CommitAmbiguityKeepsResetAtomic(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			var before time.Time
			require.NoError(t, rig.db.QueryRow(`UPDATE users SET last_session_revoke_at=clock_timestamp() WHERE id=$1 RETURNING last_session_revoke_at`, rig.user).Scan(&before))
			hook, hookedDB := stmthook.Open(t)
			rig.handler.db = hookedDB
			rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
			hook.Arm([]string{`UPDATE users SET revocation_mode=$1`}, nil, nil)
			hook.ArmCommit(committed, stmthook.ErrInjected)
			response := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode",
				`{"mode":"simple","password":"`+privacyPassword+`"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			seen, hookErr := hook.Report()
			require.Equal(t, 1, seen)
			require.NoError(t, hookErr)
			require.Empty(t, rig.recorder.events)
			var mode string
			var last sql.NullTime
			var generation int64
			require.NoError(t, rig.db.QueryRow(`SELECT revocation_mode,last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, rig.user).Scan(&mode, &last, &generation))
			if committed {
				require.Equal(t, "simple", mode)
				require.False(t, last.Valid)
				require.EqualValues(t, 1, generation)
			} else {
				require.Equal(t, "secure", mode)
				require.True(t, last.Valid)
				require.True(t, last.Time.Equal(before))
				require.Zero(t, generation)
			}
		})
	}
}

func TestMigration000198_ConservativeCutoverAndGuardedDown(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	up, err := os.ReadFile("../../migrations/000198_add_session_revoke_allowance.up.sql")
	require.NoError(t, err)
	down, err := os.ReadFile("../../migrations/000198_add_session_revoke_allowance.down.sql")
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	_, err = tx.Exec(`CREATE TEMP TABLE users (id integer PRIMARY KEY) ON COMMIT DROP;
 INSERT INTO users (id) VALUES (1),(2)`)
	require.NoError(t, err)
	var migrationTime time.Time
	require.NoError(t, tx.QueryRow(`SELECT CURRENT_TIMESTAMP`).Scan(&migrationTime))
	_, err = tx.Exec(string(up))
	require.NoError(t, err)
	for _, id := range []int{1, 2} {
		var last sql.NullTime
		var generation int64
		require.NoError(t, tx.QueryRow(`SELECT last_session_revoke_at,session_revoke_auth_gen FROM users WHERE id=$1`, id).Scan(&last, &generation))
		require.True(t, last.Valid)
		require.True(t, last.Time.Equal(migrationTime))
		require.Zero(t, generation)
	}
	_, err = tx.Exec(`INSERT INTO users (id) VALUES (3)`)
	require.NoError(t, err)
	var fresh sql.NullTime
	require.NoError(t, tx.QueryRow(`SELECT last_session_revoke_at FROM users WHERE id=3`).Scan(&fresh))
	require.False(t, fresh.Valid, "new accounts must start with a fresh allowance")

	_, err = tx.Exec(`SAVEPOINT before_down`)
	require.NoError(t, err)
	_, err = tx.Exec(string(down))
	require.ErrorContains(t, err, "cannot roll back 000198")
	_, err = tx.Exec(`ROLLBACK TO SAVEPOINT before_down`)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE users SET last_session_revoke_at=clock_timestamp()-INTERVAL '24 hours 1 second' WHERE last_session_revoke_at IS NOT NULL`)
	require.NoError(t, err)
	_, err = tx.Exec(string(down))
	require.NoError(t, err)
	var columns int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM pg_attribute WHERE attrelid='users'::regclass
 AND attname IN ('last_session_revoke_at','session_revoke_auth_gen') AND NOT attisdropped`).Scan(&columns))
	require.Zero(t, columns)
}
