package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBindSSORequestCapsEntireBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const prefix = `{"state":"valid","ignored":"`
	const suffix = `"}`

	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"at limit", prefix + strings.Repeat("a", maxSSORequestBytes-len(prefix)-len(suffix)) + suffix, false},
		{"over limit", prefix + strings.Repeat("a", maxSSORequestBytes-len(prefix)-len(suffix)+1) + suffix, true},
		{"oversized ignored suffix", `{"state":"valid"}` + strings.Repeat(" ", 131*1024), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/sso/google/session", strings.NewReader(tc.body))
			var req struct {
				State string `json:"state" binding:"required"`
			}
			err := bindSSORequest(c, &req)
			if tc.wantErr {
				require.ErrorIs(t, err, errSSORequestTooLarge)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "valid", req.State)
		})
	}
}

func TestBindSSORequestRejectsTrailingJSONValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/sso/google/session",
		strings.NewReader(`{"state":"valid"}{"extra":true}`))
	var req struct {
		State string `json:"state" binding:"required"`
	}
	require.Error(t, bindSSORequest(c, &req))
}

func TestBindSSORequestAcceptsMaxAppleUserDataWithEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := `{"id_token":"token","state":"valid","apple_user_data":"` + strings.Repeat("a", maxAppleUserDataBytes) + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/sso/apple/session", strings.NewReader(body))
	var req appleSessionRequest
	require.NoError(t, bindSSORequest(c, &req))
	require.Len(t, req.AppleUserData, maxAppleUserDataBytes)
}

func TestSSOPostRoutesRejectOversizedBodyBeforeDependencies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := NewRegistry()
	registry.Register(&AppleProvider{})
	registry.Register(&GoogleProvider{})
	h := NewHandler(HandlerDeps{Registry: registry})
	for _, tc := range []struct {
		route   string
		path    string
		handler gin.HandlerFunc
	}{
		{"/sso/:provider/complete-registration", "/sso/google/complete-registration", h.CompleteRegistration},
		{"/sso/:provider/complete-link", "/sso/google/complete-link", h.CompleteLink},
		{"/sso/:provider/sign-client-secret", "/sso/apple/sign-client-secret", h.SignAppleClientSecret},
		{"/sso/:provider/session", "/sso/apple/session", h.ProviderSession},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := gin.New()
			r.POST(tc.route, tc.handler)
			req := httptest.NewRequest(http.MethodPost, tc.path,
				strings.NewReader(`{}`+strings.Repeat(" ", 131*1024)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			require.Contains(t, w.Body.String(), "request_too_large")
		})
	}
}
