package dm

// verifyPurgeStepUp's lockout branches on a private Redis: a lock emits the
// lockout security event, a Redis fault refuses as a 500, and the count is
// recorded on a wrong guess and reset only once every factor passed.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const purgeLockoutPlaintext = "purge-lockout-correct" // pragma: allowlist secret

func purgeLockoutUser(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	hash, err := auth.HashPasswordWithParams(purgeLockoutPlaintext,
		&auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	id := uuid.NewString()
	email := id + "@purge-lockout.test"
	_, err = db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, $4, true, true)`, id, email, "pl"+id[:8], hash)
	require.NoError(t, err)
	return id, email
}

func purgeLockoutRig(t *testing.T) (*Handler, *sql.DB, *miniredis.Miniredis) {
	t.Helper()
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Handler{db: db, log: logger.New("dm-purge-lockout"), redis: rdb}, db, mr
}

func purgeLockoutContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/x", nil)
	return c, w
}

func TestVerifyPurgeStepUp_LockedEmitsTheLockoutEvent(t *testing.T) {
	h, db, mr := purgeLockoutRig(t)
	userID, email := purgeLockoutUser(t, db)
	require.NoError(t, mr.Set("login_lockout:"+email, "1"))
	mr.SetTTL("login_lockout:"+email, time.Hour)
	c, w := purgeLockoutContext()

	require.False(t, h.verifyPurgeStepUp(context.Background(), c, userID, purgeLockoutPlaintext, ""))

	require.Equal(t, http.StatusLocked, w.Code)
	verdict, ok := middleware.NightwatchVerdictFromContext(c)
	require.True(t, ok, "a 423 marks the lockout verdict")
	require.Equal(t, securityevent.ReasonAccountLocked, verdict.Reason)
}

func TestVerifyPurgeStepUp_RedisFaultIs500(t *testing.T) {
	h, db, mr := purgeLockoutRig(t)
	userID, _ := purgeLockoutUser(t, db)
	mr.Close()
	c, w := purgeLockoutContext()

	require.False(t, h.verifyPurgeStepUp(context.Background(), c, userID, purgeLockoutPlaintext, ""))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	_, marked := middleware.NightwatchVerdictFromContext(c)
	require.False(t, marked, "a Redis fault is not a lock")
}

func TestVerifyPurgeStepUp_RecordsAWrongGuessAndClearsOnSuccess(t *testing.T) {
	h, db, mr := purgeLockoutRig(t)
	userID, email := purgeLockoutUser(t, db)

	c, w := purgeLockoutContext()
	require.False(t, h.verifyPurgeStepUp(context.Background(), c, userID, "wrong-guess", ""))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.True(t, mr.Exists("login_attempts:"+email))

	c, _ = purgeLockoutContext()
	require.True(t, h.verifyPurgeStepUp(context.Background(), c, userID, purgeLockoutPlaintext, ""))
	require.False(t, mr.Exists("login_attempts:"+email), "a verified step-up resets the count")
}
