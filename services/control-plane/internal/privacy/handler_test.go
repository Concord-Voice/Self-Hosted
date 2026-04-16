package privacy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// clientIDLeakingDeleter simulates an upstream admin client whose error
// string inadvertently contains the clientId (matches the historical
// shape of a *url.Error wrapping a Sentry bulk-delete URL). Used to
// verify the handler's defense-in-depth scrub removes it from logs.
type clientIDLeakingDeleter struct{ clientID string }

func (d *clientIDLeakingDeleter) DeleteEventsForUser(_ context.Context, _ string) error {
	return errors.New(`Delete "https://sentry.io/api/0/organizations/org/issues/?project=proj&query=user.id%3A` + d.clientID + `": dial tcp: lookup sentry.io: i/o timeout`)
}

const (
	testUserID          = "user-1"
	testEraseTelemetry  = "/erase-telemetry"
	testEraseAccount    = "/erase-account"
	testContentTypeHdr  = "Content-Type"
	testContentTypeJSON = "application/json"
	testValidClientID   = "abc123def4567890123456789012abcd" // pragma: allowlist secret
)

type stubEventsDeleter struct {
	deleted string
	fail    bool
}

func (s *stubEventsDeleter) DeleteEventsForUser(_ context.Context, clientID string) error {
	s.deleted = clientID
	if s.fail {
		return assert.AnError
	}
	return nil
}

func TestEraseTelemetry_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, testValidClientID, admin.deleted)
}

func TestEraseTelemetry_Rejects_InvalidClientID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": "not-hex!"})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, admin.deleted)
}

func TestEraseTelemetry_Returns502_OnUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{fail: true}
	// Pass a real logger so the logUpstreamFailure log.Error path is
	// exercised (separate from the nil-logger path covered below).
	h := privacy.NewHandler(admin, logger.New("test"))
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestEraseTelemetry_UpstreamFailure_NeverLogsClientID pins the
// [internal]rules/observability.md contract on the error-logging path: even
// when the upstream admin client returns an error that textually contains
// the clientId (the historical *url.Error shape where a bulk-delete URL
// carries the clientId in the query string), the handler's defense-in-depth
// scrub must strip it before logging.
func TestEraseTelemetry_UpstreamFailure_NeverLogsClientID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sentinelClientID = "deadbeefcafef00d1122334455667788" // pragma: allowlist secret

	var buf bytes.Buffer
	testLog := &logger.Logger{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	router := gin.New()
	admin := &clientIDLeakingDeleter{clientID: sentinelClientID}
	h := privacy.NewHandler(admin, testLog)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": sentinelClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	out := buf.String()
	require.Contains(t, out, "privacy erasure upstream call failed", "error must be logged")
	require.Contains(t, out, "<redacted-client-id>", "handler must replace the clientId with a redaction marker")
	require.NotContains(t, out, sentinelClientID, "clientId must NEVER appear in the handler's error log")
}

// TestEraseTelemetry_UpstreamFailure_WithoutLogger_DoesNotPanic pins the
// contract documented on NewHandler: passing nil for the logger is allowed
// and must not panic even when the failure path is exercised. This covers
// the nil-check branch in logUpstreamFailure.
func TestEraseTelemetry_UpstreamFailure_WithoutLogger_DoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{fail: true}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()

	require.NotPanics(t, func() { router.ServeHTTP(rec, req) })
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestEraseAccount_WithClientID_CallsSentry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseAccount, withUserID(testUserID), h.EraseAccount)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseAccount, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, testValidClientID, admin.deleted)
}

func TestEraseAccount_WithoutClientID_Succeeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseAccount, withUserID(testUserID), h.EraseAccount)

	req := httptest.NewRequest(http.MethodPost, testEraseAccount, bytes.NewReader([]byte(`{}`)))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, admin.deleted)
}

func TestEraseAccount_AcceptsEmptyBody(t *testing.T) {
	// Empty body (io.EOF from JSON decoder) is a legitimate request —
	// clientId is optional and a user with no Sentry events should still
	// be able to POST for account erasure.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseAccount, withUserID(testUserID), h.EraseAccount)

	req := httptest.NewRequest(http.MethodPost, testEraseAccount, nil)
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, admin.deleted)
}

func TestEraseAccount_RejectsInvalidClientID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseAccount, withUserID(testUserID), h.EraseAccount)

	body, _ := json.Marshal(map[string]string{"clientId": "not-hex!"})
	req := httptest.NewRequest(http.MethodPost, testEraseAccount, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, admin.deleted)
}

func TestEraseAccount_UpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{fail: true}
	// Real logger exercises logUpstreamFailure's log.Error path.
	h := privacy.NewHandler(admin, logger.New("test"))
	router.POST(testEraseAccount, withUserID(testUserID), h.EraseAccount)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseAccount, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestEraseTelemetry_DoesNotLogClientID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer

	router := gin.New()
	router.Use(gin.LoggerWithWriter(&buf))
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin, nil)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	const sentinelClientID = "deadbeefcafef00d1122334455667788" // pragma: allowlist secret
	body, _ := json.Marshal(map[string]string{"clientId": sentinelClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, buf.String(), sentinelClientID,
		"clientId must never appear in any log output")
}

func withUserID(userID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	}
}
