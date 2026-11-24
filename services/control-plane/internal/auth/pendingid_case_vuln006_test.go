package auth

// Regression: a pending id in any letter case names the same Redis records.
// The boundary parses to uuid.UUID, so redisKey is canonical and a clear
// issued with an upper-case spelling removes the lower-case record.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPendingIDSpellingsShareRedisKeys(t *testing.T) {
	lower := uuid.NewString()
	for name, spelling := range map[string]string{
		"lower":   lower,
		"upper":   strings.ToUpper(lower),
		"braced":  "{" + lower + "}",
		"urn":     "urn:uuid:" + lower,
		"no-dash": strings.ReplaceAll(lower, "-", ""),
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := uuid.Parse(spelling)
			require.NoError(t, err)
			require.Equal(t, redisKey(uuid.MustParse(lower)), redisKey(parsed))
			require.Equal(t, verificationAttemptsKey(uuid.MustParse(lower)), verificationAttemptsKey(parsed))
			require.Equal(t, testVerificationCodeKey(uuid.MustParse(lower)), testVerificationCodeKey(parsed))
		})
	}
}

func TestUpperCasePendingIDChangeEmailInvalidatesOldCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := &Handler{redis: rc, log: logger.New("test")}
	ctx := context.Background()

	lower := uuid.New()
	const oldCode = "424242"
	rec, err := json.Marshal(verificationRecord{CodeHash: hashCode(oldCode), Email: "attacker@evil.test"})
	require.NoError(t, err)
	require.NoError(t, rc.Set(ctx, redisKey(lower), rec, 0).Err())

	body := `{"pending_id":"` + strings.ToUpper(lower.String()) + `","new_email":"victim@corp.test"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	var req ChangeRegistrationEmailRequest
	require.NoError(t, c.ShouldBindJSON(&req))

	// The handler parses the id before touching Redis.
	parsed, err := uuid.Parse(req.PendingID)
	require.NoError(t, err)
	require.Equal(t, lower, parsed)

	h.clearVerificationCode(ctx, parsed)

	exists, err := rc.Exists(ctx, redisKey(lower)).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "old lower-case verification record survived an upper-case clear")

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	require.Nil(t, h.attemptsGuard(ctx, c2, lower, oldCode), "old code still accepted")
}
