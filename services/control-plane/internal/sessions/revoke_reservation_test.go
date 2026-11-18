package sessions

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const reservationFixtureKey = "session_revokes:reservation-fixture"

func TestFreeRevokeReservation_ConcurrentClaimsAllowExactlyOne(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	const contenders = 8
	start := make(chan struct{})
	type claimResult struct {
		claimed int
		err     error
	}
	results := make(chan claimResult, contenders)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			claimed, err := reserveFreeRevokeScript.Run(context.Background(), client,
				[]string{"session_revokes:concurrent-fixture"}, 0, 1, "free:"+strconv.Itoa(i), 60).Int()
			results <- claimResult{claimed: claimed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	claims := 0
	for result := range results {
		require.NoError(t, result.err)
		claims += result.claimed
	}
	require.Equal(t, 1, claims, "the rolling window must grant only one concurrent free revoke")
	tracked, err := client.ZCard(context.Background(), "session_revokes:concurrent-fixture").Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, tracked)
}

// The script prunes entries at or before the cutoff, so only entries newer
// than the cutoff block a claim; any surviving entry, free or not, blocks it.
func TestFreeRevokeReservation_WindowBoundary(t *testing.T) {
	const cutoff = 1000
	tests := []struct {
		name        string
		seed        []redis.Z
		wantClaimed int
		wantMembers []string
	}{
		{name: "empty window claims", wantClaimed: 1, wantMembers: []string{"free:new"}},
		{name: "entry at cutoff is pruned", seed: []redis.Z{{Score: cutoff, Member: "old"}}, wantClaimed: 1, wantMembers: []string{"free:new"}},
		{name: "non-free entry after cutoff blocks", seed: []redis.Z{{Score: cutoff + 1, Member: "paid"}}, wantClaimed: 0, wantMembers: []string{"paid"}},
		{name: "free entry after cutoff blocks", seed: []redis.Z{{Score: cutoff + 1, Member: "free:earlier"}}, wantClaimed: 0, wantMembers: []string{"free:earlier"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mini := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			ctx := context.Background()
			if len(tt.seed) > 0 {
				require.NoError(t, client.ZAdd(ctx, reservationFixtureKey, tt.seed...).Err())
			}

			claimed, err := reserveFreeRevokeScript.Run(ctx, client, []string{reservationFixtureKey},
				cutoff, cutoff+10, "free:new", 90000).Int()
			require.NoError(t, err)
			require.Equal(t, tt.wantClaimed, claimed)
			members, err := client.ZRange(ctx, reservationFixtureKey, 0, -1).Result()
			require.NoError(t, err)
			require.Equal(t, tt.wantMembers, members)
			if tt.wantClaimed == 1 {
				require.Equal(t, 90000*time.Second, mini.TTL(reservationFixtureKey), "a claim must refresh the tracking TTL")
			}
		})
	}
}

func TestExecuteRevocation_ReleasesOnlyOwnReservationAfterConclusiveFailure(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	target := uuid.NewString()
	reservation := "free:" + target + ":" + uuid.NewString()
	const reservationScore = "123.000001"
	ctx := context.Background()
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user),
		redis.Z{Score: 123.000001, Member: reservation},
		redis.Z{Score: 123.000002, Member: "free:other-request"},
		redis.Z{Score: 123, Member: "paid-revoke"},
		redis.Z{Score: 124, Member: "later-paid-revoke"},
	).Err())
	rig.router.DELETE("/api/v1/sessions/:id", func(c *gin.Context) {
		rig.handler.executeRevocation(c.Request.Context(), c, c.GetString("user_id"), c.Param("id"), reservationScore)
	})

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+target, "")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.JSONEq(t, `{"error":"Session not found"}`, response.Body.String())
	members, err := rig.redis.ZRange(ctx, revokeTrackingKey(rig.user), 0, -1).Result()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"free:other-request", "paid-revoke", "later-paid-revoke"}, members,
		"a conclusive no-effect failure must release only its own reservation")
}

func TestNewFreeRevokeReservationScore_IsNonIntegerInRequestSecond(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	seen := map[string]bool{}
	for range 64 {
		score, err := newFreeRevokeReservationScore(now)
		require.NoError(t, err)
		require.Regexp(t, `^1800000000\.[0-9]{6}$`, score)
		require.NotEqual(t, "1800000000.000000", score, "an integer score could release a non-free revoke")
		seen[score] = true
	}
	require.Greater(t, len(seen), 1, "reservation scores must vary within one second")
}

func TestExecuteRevocation_RetainsReservationAfterUnknownCommit(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	reservation := "free:" + rig.sid + ":" + uuid.NewString()
	const reservationScore = "123.000001"
	ctx := context.Background()
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{
		Score: 123.000001, Member: reservation,
	}).Err())
	rig.router.DELETE("/api/v1/sessions/:id", func(c *gin.Context) {
		rig.handler.executeRevocation(c.Request.Context(), c, c.GetString("user_id"), c.Param("id"), reservationScore)
	})
	installSecurityEventCommitFault(t, rig.db)

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, response.Body.String())
	rig.requireFailedCommit(t, response)
	members, err := rig.redis.ZRange(ctx, revokeTrackingKey(rig.user), 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{reservation}, members, "an ambiguous commit must keep the allowance consumed")
}

// With enough sessions for a free revoke, a Redis outage must not skip the
// reservation: the request has to fall back to step-up.
func TestDetermineAuthRequired_RedisDownRequiresPassword(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
	for i := 1; i < freeRevokeSessionThreshold; i++ {
		_, _, _, err := minter.IssueAccessAndRefresh(context.Background(), rig.user)
		require.NoError(t, err)
	}
	active, err := rig.handler.countActiveSessions(context.Background(), rig.user)
	require.NoError(t, err)
	require.GreaterOrEqual(t, active, freeRevokeSessionThreshold, "fixture must qualify for a free revoke")

	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { require.NoError(t, dead.Close()) })
	handler := NewHandler(rig.db, dead, logger.New("test"), &securityEventDisconnector{}, nil)

	needsPassword, _, reservation := handler.determineAuthRequired(context.Background(), rig.user, rig.familyID(t), rig.sid)
	require.True(t, needsPassword, "a failed reservation must require step-up")
	require.Empty(t, reservation)
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

// A Redis outage after the database effect must not change the response:
// a failed release keeps the reservation, which fails closed, and a failed
// record of a non-free revoke is reported, not hidden.
func TestExecuteRevocation_RedisDownKeepsDatabaseOutcome(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	handler := NewHandler(rig.db, deadRedisClient(t), logger.New("test"), &securityEventDisconnector{}, nil)
	rig.router.DELETE("/api/v1/sessions/:id", func(c *gin.Context) {
		handler.executeRevocation(c.Request.Context(), c, c.GetString("user_id"), c.Param("id"), c.Query("reservation"))
	})

	missing := rig.request(http.MethodDelete, "/api/v1/sessions/"+uuid.NewString()+"?reservation=123.000001", "")
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	require.JSONEq(t, `{"error":"Session not found"}`, missing.Body.String())

	revoked := rig.request(http.MethodDelete, "/api/v1/sessions/"+rig.sid, "")
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())

	err := handler.recordRevoke(context.Background(), rig.user, rig.sid)
	require.ErrorContains(t, err, "record session revoke")
}

func TestDetermineAuthRequired_SessionCountFailureRequiresPassword(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	db, err := sql.Open("postgres", "postgres://closed.invalid/unused")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	handler := NewHandler(db, client, logger.New("test"), &securityEventDisconnector{}, nil)

	needsPassword, mode, reservation := handler.determineAuthRequired(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString())
	require.True(t, needsPassword)
	require.Equal(t, "secure", mode)
	require.Empty(t, reservation)
	keys, err := client.Keys(context.Background(), "*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "no reservation may be made when sessions cannot be counted")
}
