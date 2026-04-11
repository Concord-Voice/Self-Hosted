package middleware_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/stretchr/testify/assert"
)

const (
	testOrigin                     = "https://concordvoice.chat"
	headerAccessControlAllowOrigin = "Access-Control-Allow-Origin"
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

func TestCORSAllowedOriginSetsHeaders(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", testOrigin)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, testOrigin, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Machine-Id")
}

func TestCORSEmptyOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "GET", "")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORSNullOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{"*"})
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSDisallowedOriginNoCORSHeaders(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "GET", "https://evil.com")

	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	r := corsRouter([]string{testOrigin})
	w := doCORS(r, "OPTIONS", testOrigin)

	assert.Equal(t, 204, w.Code)
	assert.Equal(t, testOrigin, w.Header().Get(headerAccessControlAllowOrigin))
}

func TestCORSPreflightEmptyOrigin(t *testing.T) {
	r := corsRouter([]string{"*"})
	w := doCORS(r, "OPTIONS", "")

	assert.Equal(t, 204, w.Code)
	assert.Empty(t, w.Header().Get(headerAccessControlAllowOrigin))
}
