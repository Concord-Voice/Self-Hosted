package users

// reservePurgeFencePassword's refusals on a private Redis: a lock emits the
// lockout security event, a Redis fault is a 500 and never a lock.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func purgeFenceReserveRig(t *testing.T) (*Handler, string, *miniredis.Miniredis, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	uid := dbtest.CreateUser(t, db)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/x", nil)
	return &Handler{db: db, redis: rdb, log: logger.New("purge-fence-lockout")}, uid.String(), mr, c, w
}

func TestReservePurgeFencePassword_LockedEmitsTheLockoutEvent(t *testing.T) {
	h, userID, mr, c, w := purgeFenceReserveRig(t)
	require.NoError(t, mr.Set("login_lockout:"+userID+"@test.local", "1"))

	_, ok := h.reservePurgeFencePassword(context.Background(), c, userID, "any-guess")

	require.False(t, ok)
	require.Equal(t, http.StatusLocked, w.Code)
	verdict, marked := middleware.NightwatchVerdictFromContext(c)
	require.True(t, marked, "a 423 marks the lockout verdict")
	require.Equal(t, securityevent.ReasonAccountLocked, verdict.Reason)
}

func TestReservePurgeFencePassword_RedisFaultIs500(t *testing.T) {
	h, userID, mr, c, w := purgeFenceReserveRig(t)
	mr.Close()

	_, ok := h.reservePurgeFencePassword(context.Background(), c, userID, "any-guess")

	require.False(t, ok)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	_, marked := middleware.NightwatchVerdictFromContext(c)
	require.False(t, marked, "a Redis fault is not a lock")
}
