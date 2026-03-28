package websocket

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	originMain = "https://concordvoice.chat"
	originApp  = "https://app.concordvoice.chat"
	originFile = "file://"
)

// --- NewHandler tests ---

func TestNewHandler_SetsFields(t *testing.T) {
	hub := NewHub(nil, nil)
	origins := []string{originMain, originApp}

	h := NewHandler(hub, nil, nil, "test-secret", origins)

	require.NotNil(t, h)
	assert.Equal(t, hub, h.hub)
	assert.Equal(t, "test-secret", h.jwtSecret)
	assert.Equal(t, origins, h.allowedOrigins)
	assert.NotNil(t, h.upgrader)
}

func TestNewHandler_NilDepsDoNotPanic(t *testing.T) {
	// NewHandler with nil deps is safe for CheckOrigin testing
	h := NewHandler(nil, nil, nil, "", nil)
	require.NotNil(t, h)
}

// --- CheckOrigin comprehensive tests ---

func TestCheckOrigin_EmptyAllowList(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{})

	tests := []struct {
		name   string
		origin string
		set    bool
		want   bool
	}{
		{"empty origin allowed (native client)", "", false, true},
		{"null origin rejected", "null", true, false},
		{"file:// allowed (Electron)", originFile, true, true},
		{"any origin rejected with empty list", "https://example.com", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			if tt.set {
				r.Header.Set("Origin", tt.origin)
			}
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOrigin_MultipleAllowedOrigins(t *testing.T) {
	origins := []string{
		originMain,
		originApp,
		"http://localhost:3001",
	}
	h := NewHandler(nil, nil, nil, "", origins)

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{"first allowed", originMain, true},
		{"second allowed", originApp, true},
		{"third allowed (localhost dev)", "http://localhost:3001", true},
		{"not in list", "https://evil.com", false},
		{"partial match not allowed", originMain + ".evil.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", tt.origin)
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOrigin_FileProtocolVariants(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{originMain})

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{originFile, originFile, true},
		{"file:///", "file:///", true},
		{"file:///C:/Users/app", "file:///C:/Users/app/index.html", true},
		{"file:///home/user", "file:///home/user/app/index.html", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", tt.origin)
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOrigin_NullOriginAlwaysRejected(t *testing.T) {
	// Even with wildcard, null origin should be rejected
	tests := []struct {
		name    string
		origins []string
	}{
		{"empty list", []string{}},
		{"specific origin", []string{originMain}},
		{"wildcard", []string{"*"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil, "", tt.origins)
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", "null")
			assert.False(t, h.upgrader.CheckOrigin(r), "null origin should always be rejected")
		})
	}
}
