package middleware_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestIPBucket(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "ipv4", raw: "198.51.100.19", want: "198.51.100.19"},
		{name: "mapped IPv4", raw: "::ffff:198.51.100.19", want: "198.51.100.19"},
		{name: "native IPv6", raw: "2001:db8:1:2:abcd:ef01:2345:6789", want: "2001:db8:1:2::"},
		{name: "compressed and uppercase IPv6", raw: "2001:DB8:1:2::BEEF", want: "2001:db8:1:2::"},
		{name: "IPv6 zone", raw: "fe80::1234%en0", want: "fe80::"},
		{name: "empty", raw: "", want: "unattributed"},
		{name: "malformed", raw: "not an address", want: "unattributed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, middleware.IPBucket(tc.raw))
		})
	}

	first := middleware.IPBucket("2001:db8:1:2::1")
	for lastByte := 0; lastByte < 256; lastByte++ {
		require.Equalf(t, first, middleware.IPBucket(fmt.Sprintf("2001:db8:1:2::%x", lastByte)), "address %d", lastByte)
	}
	require.NotEqual(t, first, middleware.IPBucket("2001:db8:1:3::1"))
}

func TestClientIPBucketUsesGinClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	var bucket string
	router.GET("/", func(c *gin.Context) {
		bucket = middleware.ClientIPBucket(c)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.9:1234"
	request.Header.Set("X-Forwarded-For", "2001:db8:1:2::99")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, "2001:db8:1:2::", bucket)
}

func TestRateLimitByIPAggregatesIPv6Prefix(t *testing.T) {
	ts := setupTS(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", middleware.RateLimitByIP(ts.Redis, 2, time.Minute), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	for suffix := 1; suffix <= 3; suffix++ {
		writer := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = fmt.Sprintf("[2001:db8:1:2::%x]:1234", suffix)
		router.ServeHTTP(writer, request)
		if suffix <= 2 {
			require.Equal(t, http.StatusNoContent, writer.Code)
		} else {
			require.Equal(t, http.StatusTooManyRequests, writer.Code)
		}
	}

	keys, err := ts.Redis.Keys(t.Context(), "ratelimit:ip:*").Result()
	require.NoError(t, err)
	require.Equal(t, []string{"ratelimit:ip:2001:db8:1:2:::GET:/"}, keys)
}
