package sessions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// A revocation-mode change ends every login family's Simple-mode window, not
// only the requester's. Family B proves and earns a window; family A switches
// the account to secure and back to simple. B's earlier window must no longer
// waive its password. The control shows B's window was live before the change,
// so the refusal comes from the mode change and nothing else.
func TestSimpleWindowEndsForEveryFamilyOnModeChange(t *testing.T) {
	rig := newSecurityEventFamilyRig(t)
	rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
	rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
	ctx := context.Background()
	_, err := rig.db.Exec(`UPDATE users SET revocation_mode = 'simple' WHERE id = $1`, rig.user)
	require.NoError(t, err)

	const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
	minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
	accessB, _, _, err := minter.IssueAccessAndRefresh(ctx, rig.user)
	require.NoError(t, err)
	targets := make([]string, 3)
	for i := range targets {
		_, _, targets[i], err = minter.IssueAccessAndRefresh(ctx, rig.user)
		require.NoError(t, err)
	}

	// Spend the window's one free revoke, so only a Simple-mode window can
	// waive the password below.
	require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: float64(time.Now().Unix()), Member: "existing-revoke"}).Err())

	send := func(access, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+access)
		response := httptest.NewRecorder()
		rig.router.ServeHTTP(response, req)
		return response
	}
	withPassword := `{"password":"` + privacyPassword + `"}`

	proved := send(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[0], withPassword)
	require.Equal(t, http.StatusOK, proved.Code, proved.Body.String())
	control := send(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[1], "")
	require.Equal(t, http.StatusOK, control.Code, "family B's own window waives its password before any mode change: %s", control.Body.String())

	for _, mode := range []string{"secure", "simple"} {
		changed := send(rig.access, http.MethodPut, "/api/v1/sessions/revocation-mode", `{"mode":"`+mode+`","password":"`+privacyPassword+`"}`)
		require.Equal(t, http.StatusOK, changed.Code, "switch to %s: %s", mode, changed.Body.String())
	}

	refused := send(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[2], "")
	require.Equal(t, http.StatusForbidden, refused.Code, "a window earned before a mode change must not survive it: %s", refused.Body.String())
	require.JSONEq(t, `{"error":"password_required","message":"Password verification required to revoke this session"}`, refused.Body.String())
	require.True(t, sessionLive(t, rig, targets[2]), "a refused revoke must leave its target live")

	reproved := send(accessB, http.MethodDelete, "/api/v1/sessions/"+targets[2], withPassword)
	require.Equal(t, http.StatusOK, reproved.Code, "a fresh proof under the new mode still revokes: %s", reproved.Body.String())
	require.False(t, sessionLive(t, rig, targets[2]))
}

// modeGenerationFaultHook fails every Redis command that names the user's
// revocation-mode generation key, leaving the rest of the client, the password
// lockout included, working.
type modeGenerationFaultHook struct{ key string }

func (modeGenerationFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h modeGenerationFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.names(cmd) {
			err := errors.New("injected generation fault")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (h modeGenerationFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h modeGenerationFaultHook) names(cmd redis.Cmder) bool {
	for _, arg := range cmd.Args() {
		if value, ok := arg.(string); ok && value == h.key {
			return true
		}
	}
	return false
}

// When the generation cannot be replaced, a change to simple is refused and
// the mode is left as it was, since a window earned before it could otherwise
// waive a password again. A change to secure proceeds: no window waives
// anything while the mode is secure.
func TestModeChangeWhenWindowsCannotEnd(t *testing.T) {
	for _, tc := range []struct {
		target, want string
		status       int
	}{
		{target: "simple", want: "secure", status: http.StatusServiceUnavailable},
		{target: "secure", want: "secure", status: http.StatusOK},
	} {
		t.Run("to "+tc.target, func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			rig.router.PUT("/api/v1/sessions/revocation-mode", rig.handler.UpdateRevocationMode)
			if tc.target == "secure" {
				_, err := rig.db.Exec(`UPDATE users SET revocation_mode = 'simple' WHERE id = $1`, rig.user)
				require.NoError(t, err)
			}
			rig.redis.AddHook(modeGenerationFaultHook{key: revocationModeGenerationKey(rig.user)})

			response := rig.request(http.MethodPut, "/api/v1/sessions/revocation-mode", `{"mode":"`+tc.target+`","password":"`+privacyPassword+`"}`)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			if tc.status != http.StatusOK {
				require.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, response.Body.String())
			}
			var mode string
			require.NoError(t, rig.db.QueryRow(`SELECT revocation_mode FROM users WHERE id = $1`, rig.user).Scan(&mode))
			require.Equal(t, tc.want, mode)
		})
	}
}

// A window can waive a password only while its generation can be read: an
// unreadable generation requires the password, even for a family holding a
// window that matches the stored generation.
func TestUnreadableModeGenerationRequiresPassword(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "control"
		if fault {
			name = "generation unreadable"
		}
		t.Run(name, func(t *testing.T) {
			rig := newSecurityEventFamilyRig(t)
			rig.router.DELETE("/api/v1/sessions/:id", rig.handler.RevokeSession)
			ctx := context.Background()
			_, err := rig.db.Exec(`UPDATE users SET revocation_mode = 'simple' WHERE id = $1`, rig.user)
			require.NoError(t, err)
			const secret = "security-event-family-test-signing-secret" // pragma: allowlist secret
			minter := auth.NewHandler(rig.db, rig.redis, logger.New("test"), secret, testDisconnector{})
			_, _, target, err := minter.IssueAccessAndRefresh(ctx, rig.user)
			require.NoError(t, err)
			require.NoError(t, rig.redis.ZAdd(ctx, revokeTrackingKey(rig.user), redis.Z{Score: float64(time.Now().Unix()), Member: "existing-revoke"}).Err())
			const generation = "fixture-generation"
			require.NoError(t, rig.redis.Set(ctx, revocationModeGenerationKey(rig.user), generation, 0).Err())
			require.NoError(t, rig.redis.Set(ctx, simpleAuthWindowKey(rig.user, rig.familyID(t)), generation, 0).Err())
			if fault {
				rig.redis.AddHook(modeGenerationFaultHook{key: revocationModeGenerationKey(rig.user)})
			}

			response := rig.request(http.MethodDelete, "/api/v1/sessions/"+target, "")
			if !fault {
				require.Equal(t, http.StatusOK, response.Code, "the matching window waives the password: %s", response.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.JSONEq(t, `{"error":"password_required","message":"Password verification required to revoke this session"}`, response.Body.String())
			require.True(t, sessionLive(t, rig, target))
		})
	}
}
