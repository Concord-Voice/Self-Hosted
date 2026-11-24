package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var helperPendingID = uuid.MustParse("550e8400-e29b-41d4-a716-446655440020")

func newHelperHandler(t *testing.T) (*Handler, context.Context) {
	t.Helper()
	return &Handler{redis: newRollbackRedis(t), log: logger.NewWithWriter(io.Discard)}, context.Background()
}

// A handler whose Redis is gone, for the failure branches.
func newBrokenRedisHandler(t *testing.T) (*Handler, context.Context) {
	t.Helper()
	h, ctx := newHelperHandler(t)
	require.NoError(t, h.redis.Close())
	return h, ctx
}

func TestSnapshotVerificationCodeReportsRedisFailure(t *testing.T) {
	h, ctx := newBrokenRedisHandler(t)
	_, err := h.snapshotVerificationCode(ctx, helperPendingID)
	require.Error(t, err)
}

// A record with no expiry has nothing safe to restore, so the snapshot is empty.
func TestSnapshotVerificationCodeIgnoresRecordWithoutLifetime(t *testing.T) {
	h, ctx := newHelperHandler(t)
	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackOldRaw, 0).Err())
	snap, err := h.snapshotVerificationCode(ctx, helperPendingID)
	require.NoError(t, err)
	require.Empty(t, snap.raw)
}

func TestSnapshotAndRestoreCarryTheTestOnlyCode(t *testing.T) {
	t.Setenv("CONCORD_ENV", "test")
	h, ctx := newHelperHandler(t)
	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackOldRaw, 5*time.Minute).Err())
	require.NoError(t, h.redis.Set(ctx, testVerificationCodeKey(helperPendingID), "111111", 5*time.Minute).Err())
	snap, err := h.snapshotVerificationCode(ctx, helperPendingID)
	require.NoError(t, err)
	require.Equal(t, "111111", snap.testCode)

	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackNewRaw, 5*time.Minute).Err())
	require.NoError(t, h.redis.Set(ctx, testVerificationCodeKey(helperPendingID), "222222", 5*time.Minute).Err())
	require.NoError(t, h.restoreVerificationCode(ctx, helperPendingID, rollbackNewRaw, snap))
	require.Equal(t, "111111", h.redis.Get(ctx, testVerificationCodeKey(helperPendingID)).Val())

	// With no previous test-only code the new one is removed.
	snap.testCode = ""
	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackNewRaw, 5*time.Minute).Err())
	require.NoError(t, h.restoreVerificationCode(ctx, helperPendingID, rollbackNewRaw, snap))
	require.Zero(t, h.redis.Exists(ctx, testVerificationCodeKey(helperPendingID)).Val())
}

func TestVerifyRegistrationCodeCurrent(t *testing.T) {
	h, ctx := newHelperHandler(t)
	require.ErrorIs(t, h.verifyRegistrationCodeCurrent(ctx, helperPendingID, rollbackOldRaw), ErrVerificationCodeReplaced)

	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackNewRaw, time.Minute).Err())
	require.ErrorIs(t, h.verifyRegistrationCodeCurrent(ctx, helperPendingID, rollbackOldRaw), ErrVerificationCodeReplaced)
	require.NoError(t, h.verifyRegistrationCodeCurrent(ctx, helperPendingID, rollbackNewRaw))

	broken, bctx := newBrokenRedisHandler(t)
	err := broken.verifyRegistrationCodeCurrent(bctx, helperPendingID, rollbackNewRaw)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrVerificationCodeReplaced), "a Redis fault is not a replaced code")
}

func TestVerificationMaterialHelpersReportRedisFailure(t *testing.T) {
	h, ctx := newBrokenRedisHandler(t)
	require.Error(t, h.deleteVerificationCodeMaterial(ctx, helperPendingID))
	require.Error(t, h.deleteVerificationMaterialIfCurrent(ctx, helperPendingID, rollbackNewRaw))
	// The logging wrappers swallow the error by design; they must not panic.
	h.clearVerificationCode(ctx, helperPendingID)
	h.clearVerificationCodeMaterial(ctx, helperPendingID)
	h.exhaustVerificationCode(ctx, helperPendingID)
}

func TestChargeRegistrationIssuanceFailsClosedOnRedisFailure(t *testing.T) {
	h, ctx := newBrokenRedisHandler(t)
	refund, err := h.chargeRegistrationIssuance(ctx, "a@example.test")
	require.Error(t, err)
	require.Nil(t, refund)
	require.False(t, errors.Is(err, errRegistrationCodesExhausted))
}

func TestWriteRegistrationIssuanceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _ := newHelperHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.writeRegistrationIssuanceError(c, errRegistrationCodesExhausted)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Contains(t, w.Body.String(), "registration_codes_exhausted")

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	h.writeRegistrationIssuanceError(c, errors.New("redis down"))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCarryVerificationAttemptsRejectsMalformedReplacedID(t *testing.T) {
	h, ctx := newHelperHandler(t)
	require.Error(t, h.carryVerificationAttempts(ctx, "not-a-uuid", helperPendingID))
}

func TestReserveVerificationAttemptRejectsCorruptRecord(t *testing.T) {
	h, ctx := newHelperHandler(t)
	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), "{not json", time.Minute).Err())
	_, _, _, err := h.reserveVerificationAttempt(ctx, helperPendingID, time.Minute)
	require.Error(t, err)
}

func postJSON(h gin.HandlerFunc, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h(c)
	return w
}

// Request-shape refusals happen before any database or Redis work.
func TestResendAndChangeEmailRefuseMalformedRequests(t *testing.T) {
	h, _ := newHelperHandler(t)
	for name, body := range map[string]string{
		"not json":     `{`,
		"bad pending":  `{"pending_id":"nope","new_email":"a@example.test"}`,
		"empty object": `{}`,
	} {
		require.Equal(t, http.StatusBadRequest, postJSON(h.ResendRegistrationCode, body).Code, "resend: "+name)
		require.Equal(t, http.StatusBadRequest, postJSON(h.ChangeRegistrationEmail, body).Code, "change-email: "+name)
	}
}

// Change-email charges the address before it touches the pending row, so an
// address out of codes is refused with nothing else read or written.
func TestChangeRegistrationEmailRefusesAnAddressOutOfCodes(t *testing.T) {
	h, ctx := newHelperHandler(t)
	email := "full@example.test"
	require.NoError(t, h.redis.Set(ctx, registrationIssuanceKey(email), registrationIssuanceLimit, time.Minute).Err())
	body := `{"pending_id":"` + helperPendingID.String() + `","new_email":"` + email + `"}`

	w := postJSON(h.ChangeRegistrationEmail, body)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Contains(t, w.Body.String(), "registration_codes_exhausted")
}

func TestChangeRegistrationEmailFailsClosedWhenIssuanceLimitUnavailable(t *testing.T) {
	h, _ := newBrokenRedisHandler(t)
	body := `{"pending_id":"` + helperPendingID.String() + `","new_email":"a@example.test"}`
	require.Equal(t, http.StatusServiceUnavailable, postJSON(h.ChangeRegistrationEmail, body).Code)
}

func TestPrepareInitialCodeRefusesWithoutAnEmailService(t *testing.T) {
	h, ctx := newHelperHandler(t)
	_, _, raw, err := h.prepareInitialCode(ctx, nil, helperPendingID, "a@example.test")
	require.Error(t, err)
	require.Empty(t, raw)
}

func answerOf(t *testing.T, write func(c *gin.Context)) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	write(c)
	return w.Code, w.Body.String()
}

func TestWriteResendFailureMapsEachOutcome(t *testing.T) {
	h, _ := newHelperHandler(t)
	boom := errors.New("boom")
	cases := []struct {
		name                      string
		err, issuanceErr, prepErr error
		status                    int
		body                      string
	}{
		{"issuance exhausted", boom, errRegistrationCodesExhausted, nil, http.StatusTooManyRequests, "registration_codes_exhausted"},
		{"preparation failed", boom, nil, boom, http.StatusServiceUnavailable, errMsgVerificationSendFailed},
		{"preparation hit an expiring row", ErrPendingExpired, nil, ErrPendingExpired, http.StatusGone, "pending_expired"},
		{"not found", ErrPendingNotFound, nil, nil, http.StatusNotFound, "pending_not_found"},
		{"resends exhausted", ErrResendsExhausted, nil, nil, http.StatusTooManyRequests, "resends_exhausted"},
		{"cooldown", ErrResendCooldown, nil, nil, http.StatusTooManyRequests, "cooldown_active"},
		{"expired", ErrPendingExpired, nil, nil, http.StatusGone, "pending_expired"},
		{"unexpected", boom, nil, nil, http.StatusInternalServerError, errMsgInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := answerOf(t, func(c *gin.Context) { h.writeResendFailure(c, tc.err, tc.issuanceErr, tc.prepErr) })
			require.Equal(t, tc.status, status)
			require.Contains(t, body, tc.body)
		})
	}
}

func TestWriteChangeEmailFailureMapsEachOutcome(t *testing.T) {
	h, _ := newHelperHandler(t)
	boom := errors.New("boom")
	cases := []struct {
		name         string
		err, prepErr error
		status       int
		body         string
	}{
		{"preparation failed", boom, boom, http.StatusServiceUnavailable, "Failed to send verification code"},
		{"not found", ErrPendingNotFound, nil, http.StatusNotFound, "pending_not_found"},
		{"expired", ErrPendingExpired, nil, http.StatusGone, "pending_expired"},
		{"email registered", ErrEmailAlreadyRegister, nil, http.StatusConflict, "email_already_registered"},
		{"email pending", ErrEmailPending, nil, http.StatusConflict, "registration_pending"},
		{"resends exhausted", ErrResendsExhausted, nil, http.StatusTooManyRequests, "resends_exhausted"},
		{"cooldown", ErrResendCooldown, nil, http.StatusTooManyRequests, "cooldown_active"},
		{"unexpected", boom, nil, http.StatusInternalServerError, errMsgInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := answerOf(t, func(c *gin.Context) { h.writeChangeEmailFailure(c, tc.err, tc.prepErr) })
			require.Equal(t, tc.status, status)
			require.Contains(t, body, tc.body)
		})
	}
}

func TestWriteStaleCodeRefusal(t *testing.T) {
	h, ctx := newHelperHandler(t)
	proof := &registrationCodeProof{email: "a@example.test", raw: rollbackOldRaw, attempts: 1}
	require.NoError(t, h.redis.Set(ctx, redisKey(helperPendingID), rollbackOldRaw, time.Minute).Err())

	status, body := answerOf(t, func(c *gin.Context) { h.writeStaleCodeRefusal(ctx, c, helperPendingID, proof) })
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, body, "invalid_code")
	require.Contains(t, body, `"attempts_remaining":`+strconv.Itoa(MaxCodeAttempts-1))
	require.Zero(t, h.redis.Exists(ctx, redisKey(helperPendingID)).Val(), "the stale record is removed")

	broken, bctx := newBrokenRedisHandler(t)
	status, _ = answerOf(t, func(c *gin.Context) { broken.writeStaleCodeRefusal(bctx, c, helperPendingID, proof) })
	require.Equal(t, http.StatusInternalServerError, status)
}
