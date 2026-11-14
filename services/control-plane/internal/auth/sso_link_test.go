package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompleteSSOLinkRejectsMissingMalformedOrMismatchedPending(t *testing.T) {
	for _, test := range []struct {
		name    string
		pending string
		claim   bool
	}{
		{name: "missing", claim: true},
		{name: "malformed", pending: "not-json", claim: true},
		{name: "wrong user", pending: `{"user_id":"other","credential_epoch":"epoch-1","identity":{"Provider":"google","ProviderUserID":"subject"}}`, claim: true},
		{name: "wrong epoch", pending: `{"user_id":"user-1","credential_epoch":"epoch-2","identity":{"Provider":"google","ProviderUserID":"subject"}}`, claim: true},
		{name: "unclaimed challenge", pending: `{"user_id":"user-1","credential_epoch":"epoch-1","identity":{"Provider":"google","ProviderUserID":"subject"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			rig := newAdapterRig(t)
			ctx := context.Background()
			if test.pending != "" {
				require.NoError(t, rig.Redis.Set(ctx, "mfa_sso_link:jti-1", test.pending, time.Minute).Err())
			}
			if test.claim {
				require.NoError(t, rig.Redis.Set(ctx, "mfa_challenge_used:jti-1", "1", time.Minute).Err())
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
			require.False(t, rig.Handler.CompleteSSOLink(c, "user-1", "jti-1", "epoch-1", "totp"))
			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.Empty(t, response.Header().Values("Set-Cookie"))
			require.Empty(t, response.Header().Get("X-Session-ID"))
		})
	}
}

func TestCompleteSSOLinkCommitsIdentityAndSessionOnce(t *testing.T) {
	rig := newAdapterRig(t)
	userID := insertAdapterUser(t, rig, "complete-link@example.test", "completelink", "Password!1234") // pragma: allowlist secret
	_, err := rig.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{totp}' WHERE id = $1`, userID)
	require.NoError(t, err)
	ctx := context.Background()
	payload, err := json.Marshal(map[string]any{
		"user_id": userID, "credential_epoch": "",
		"identity": auth.SSOIdentityLink{Provider: "google", ProviderUserID: "completed-subject", ProviderEmail: "complete-link@example.test"},
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(ctx, "mfa_sso_link:jti-complete", payload, time.Minute).Err())
	require.NoError(t, rig.Redis.Set(ctx, "mfa_challenge_used:jti-complete", "1", time.Minute).Err())
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
	require.True(t, rig.Handler.CompleteSSOLink(c, userID, "jti-complete", "", "totp"))
	require.Equal(t, http.StatusOK, response.Code)
	require.NotEmpty(t, response.Header().Get(middleware.SessionIDHeader))
	require.NotEmpty(t, response.Header().Values("Set-Cookie"))
	var identities, families, refreshes int
	for _, entry := range []struct {
		table string
		count *int
	}{{"user_sso_identities", &identities}, {"login_families", &families}, {"refresh_tokens", &refreshes}} {
		require.NoError(t, rig.DB.QueryRow(`SELECT COUNT(*) FROM `+entry.table+` WHERE user_id = $1`, userID).Scan(entry.count))
		require.Equal(t, 1, *entry.count, entry.table)
	}

	replay := httptest.NewRecorder()
	replayContext, _ := gin.CreateTestContext(replay)
	replayContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
	require.False(t, rig.Handler.CompleteSSOLink(replayContext, userID, "jti-complete", "", "totp"))
	require.Equal(t, http.StatusUnauthorized, replay.Code)
	for _, table := range []string{"user_sso_identities", "login_families", "refresh_tokens"} {
		var count int
		require.NoError(t, rig.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&count))
		require.Equal(t, 1, count, table)
	}
}

func TestCompleteSSOLinkRejectsChangedFactorBeforeWriting(t *testing.T) {
	rig := newAdapterRig(t)
	userID := insertAdapterUser(t, rig, "changed-link@example.test", "changedlink", "Password!1234") // pragma: allowlist secret
	// The TOTP factor that answered the challenge was removed before final mint.
	_, err := rig.DB.Exec(`UPDATE users SET mfa_enabled = FALSE, mfa_methods = '{}' WHERE id = $1`, userID)
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{
		"user_id": userID, "credential_epoch": "",
		"identity": auth.SSOIdentityLink{Provider: "google", ProviderUserID: "changed-subject", ProviderEmail: "changed-link@example.test"},
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, rig.Redis.Set(ctx, "mfa_sso_link:jti-changed", payload, time.Minute).Err())
	require.NoError(t, rig.Redis.Set(ctx, "mfa_challenge_used:jti-changed", "1", time.Minute).Err())
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
	require.False(t, rig.Handler.CompleteSSOLink(c, userID, "jti-changed", "", "totp"))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Empty(t, response.Header().Values("Set-Cookie"))
	for _, table := range []string{"user_sso_identities", "login_families", "refresh_tokens"} {
		var count int
		require.NoError(t, rig.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&count))
		require.Zero(t, count, table)
	}
}

func TestCompleteSSOLinkConcurrentPendingHasOneWinner(t *testing.T) {
	rig := newAdapterRig(t)
	userID := insertAdapterUser(t, rig, "concurrent-link@example.test", "concurrentlink", "Password!1234") // pragma: allowlist secret
	_, err := rig.DB.Exec(`UPDATE users SET mfa_enabled = TRUE, mfa_methods = '{totp}' WHERE id = $1`, userID)
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{
		"user_id": userID, "credential_epoch": "",
		"identity": auth.SSOIdentityLink{Provider: "google", ProviderUserID: "concurrent-subject", ProviderEmail: "concurrent-link@example.test"},
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, rig.Redis.Set(ctx, "mfa_sso_link:jti-concurrent", payload, time.Minute).Err())
	require.NoError(t, rig.Redis.Set(ctx, "mfa_challenge_used:jti-concurrent", "1", time.Minute).Err())

	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", nil)
			rig.Handler.CompleteSSOLink(c, userID, "jti-concurrent", "", "totp")
			statuses <- response.Code
		}()
	}
	wg.Wait()
	close(statuses)
	var ok, denied int
	for status := range statuses {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
			denied++
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, denied)
	for _, table := range []string{"user_sso_identities", "login_families", "refresh_tokens"} {
		var count int
		require.NoError(t, rig.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&count))
		require.Equal(t, 1, count, table)
	}
}
