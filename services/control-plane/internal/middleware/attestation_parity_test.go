package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func TestValidateAttestationMatchesHTTPMiddleware(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token bool
		store bool
		down  bool
		want  int
	}{
		{name: "missing", want: http.StatusForbidden},
		{name: "valid", token: true, store: true, want: http.StatusOK},
		{name: "registry unavailable", token: true, down: true, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rdb, cleanup := testhelpers.SetupTestRedis(t)
			defer cleanup()
			if tc.store {
				writeToken(t, rdb, "attestation:s-parity:web", "proof", "desktop-v1")
			}
			if tc.down {
				require.NoError(t, rdb.Close())
			}
			log := logger.NewWithWriter(io.Discard)
			request := func(router *gin.Engine) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.Header.Set("X-Session-ID", "s-parity")
				if tc.token {
					req.Header.Set("X-Attestation-Token", "proof")
				}
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				return out
			}
			wrapped := gin.New()
			wrapped.GET("/test", setCaller, middleware.RequireAttestation(true, rdb, log), func(c *gin.Context) { c.Status(http.StatusOK) })
			direct := gin.New()
			direct.GET("/test", setCaller, func(c *gin.Context) {
				if middleware.ValidateAttestation(c, rdb, log) {
					c.Status(http.StatusOK)
				}
			})
			want := request(wrapped)
			got := request(direct)
			require.Equal(t, tc.want, got.Code)
			require.Equal(t, want.Code, got.Code)
			require.Equal(t, want.Body.String(), got.Body.String())
		})
	}
}
