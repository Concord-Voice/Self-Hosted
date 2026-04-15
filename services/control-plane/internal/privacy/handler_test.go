package privacy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
)

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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
	router.POST(testEraseTelemetry, withUserID(testUserID), h.EraseTelemetry)

	body, _ := json.Marshal(map[string]string{"clientId": testValidClientID})
	req := httptest.NewRequest(http.MethodPost, testEraseTelemetry, bytes.NewReader(body))
	req.Header.Set(testContentTypeHdr, testContentTypeJSON)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestEraseAccount_WithClientID_CallsSentry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := &stubEventsDeleter{}
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
	h := privacy.NewHandler(admin)
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
