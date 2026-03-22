package middleware_test

import (
	"net/http/httptest"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func corsRouter(origins []string) *gin.Engine {
	r := gin.New()
	r.Use(middleware.CORS(origins))
	r.GET("/test", func(c *gin.Context) { c.String(200, "ok") })
	return r
}

func doCORS(r *gin.Engine, method, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/test", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCORS_AllowedOrigin_SetsHeaders(t *testing.T) {
	r := corsRouter([]string{"https://concordvoice.chat"})
	w := doCORS(r, "GET", "https://concordvoice.chat")

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "https://concordvoice.chat", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Machine-Id")
}

func TestCORS_EmptyOrigin_NoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "GET", "")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORS_NullOrigin_NoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_DisallowedOrigin_NoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"https://concordvoice.chat"})
	w := doCORS(r, "GET", "https://evil.com")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Preflight_AllowedOrigin(t *testing.T) {
	r := corsRouter([]string{"https://concordvoice.chat"})
	w := doCORS(r, "OPTIONS", "https://concordvoice.chat")

	assert.Equal(t, 204, w.Code)
	assert.Equal(t, "https://concordvoice.chat", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Preflight_EmptyOrigin(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "OPTIONS", "")

	assert.Equal(t, 204, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}
