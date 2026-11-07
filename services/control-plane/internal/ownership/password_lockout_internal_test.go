package ownership

// verifyPassword's lockout branches that need a Redis client of their own: a
// fault must refuse as a 500 (never a pass, never a lock), and a lock must
// emit the lockout security event.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func lockoutTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	return c, w
}

func TestVerifyPassword_RedisFaultIs500(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	userID := dbtest.CreateUser(t, db).String()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mini.Close()
	h := &Handler{db: db, log: logger.New("ownership-lockout"), redis: rdb}
	c, w := lockoutTestContext()

	err := h.verifyPassword(c, userID, "any-guess")

	require.Error(t, err, "an uncountable guess must never pass")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), errMsgFailedVerifyPassword)
	_, marked := middleware.NightwatchVerdictFromContext(c)
	require.False(t, marked, "a Redis fault is not a lock")
}

func TestVerifyPassword_LockedEmitsTheLockoutEvent(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	uid := dbtest.CreateUser(t, db)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Set(context.Background(), "login_lockout:"+uid.String()+"@test.local", "1", time.Hour).Err())
	h := &Handler{db: db, log: logger.New("ownership-lockout"), redis: rdb}
	c, w := lockoutTestContext()

	require.Error(t, h.verifyPassword(c, uid.String(), "any-guess"))

	require.Equal(t, http.StatusLocked, w.Code)
	verdict, ok := middleware.NightwatchVerdictFromContext(c)
	require.True(t, ok, "a 423 marks the lockout verdict")
	require.Equal(t, securityevent.ReasonAccountLocked, verdict.Reason)
}
