package sessions

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

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
		i := i
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

func TestExecuteRevocation_ReleasesReservationAfterConclusiveFailure(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	target := uuid.NewString()
	reservation := "free:" + target
	const reservationScore = "123.5"
	ctx := context.Background()
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{
		Score: 123.5, Member: reservation,
	}).Err())
	rig.router.DELETE("/api/v1/sessions/:id", func(c *gin.Context) {
		rig.handler.executeRevocation(c.Request.Context(), c, c.GetString("user_id"), c.Param("id"), reservationScore)
	})

	response := rig.request(http.MethodDelete, "/api/v1/sessions/"+target, "")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.JSONEq(t, `{"error":"Session not found"}`, response.Body.String())
	tracked, err := rig.redis.ZCard(ctx, revokeTrackingKey(rig.user)).Result()
	require.NoError(t, err)
	require.Zero(t, tracked, "a conclusive no-effect failure must restore the free allowance")
}

func TestExecuteRevocation_RetainsReservationAfterUnknownCommit(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	reservation := "free:" + rig.sid
	const reservationScore = "123.5"
	ctx := context.Background()
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{
		Score: 123.5, Member: reservation,
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
