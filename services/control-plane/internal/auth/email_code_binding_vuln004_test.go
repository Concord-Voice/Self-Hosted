package auth

// Regression for the registration code binding: an accepted code carries the
// recipient it was mailed to, so a later change-email cannot lend that proof
// to a different address. In-process (miniredis).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRegistrationCodeProofCarriesRecordRecipient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := &Handler{redis: rc, log: logger.New("test")}
	ctx := context.Background()

	id := uuid.New()
	const code = "135790"
	rec, err := json.Marshal(verificationRecord{CodeHash: hashCode(code), Email: "Attacker@Evil.Test"})
	require.NoError(t, err)
	require.NoError(t, rc.Set(ctx, redisKey(id), rec, 0).Err())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	proof := h.attemptsGuard(ctx, c, id, code)
	require.NotNil(t, proof, "guard rejected: %d %s", w.Code, w.Body.String())
	// The proof names the address the code was mailed to, lower-cased; the
	// confirm path compares it to the locked pending row before promoting.
	require.Equal(t, "attacker@evil.test", proof.email)
	require.NotEqual(t, "victim@corp.test", proof.email)
}

func TestRegistrationCodeProofRejectsWrongCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := &Handler{redis: rc, log: logger.New("test")}
	ctx := context.Background()

	id := uuid.New()
	rec, err := json.Marshal(verificationRecord{CodeHash: hashCode("111111"), Email: "a@b.test"})
	require.NoError(t, err)
	require.NoError(t, rc.Set(ctx, redisKey(id), rec, 0).Err())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.Nil(t, h.attemptsGuard(ctx, c, id, "222222"))
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
