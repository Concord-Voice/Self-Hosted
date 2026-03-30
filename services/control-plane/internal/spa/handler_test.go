package spa

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	testVersion     = "abc1234"
	testIndexFile   = "index.html"
	spaBasePath     = "/api/v1/spa/"
	headerCacheCtrl = "Cache-Control"
	headerContType  = "Content-Type"
	jsContentType   = "application/javascript; charset=utf-8"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// setupTestSPA creates a temporary SPA directory with versioned assets.
func setupTestSPA(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create versioned SPA build: abc1234/
	version := testVersion
	versionDir := filepath.Join(dir, version)
	assetsDir := filepath.Join(versionDir, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// index.html (entry point — short cache)
	if err := os.WriteFile(filepath.Join(versionDir, testIndexFile), []byte("<html>SPA</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hashed JS bundle (long cache)
	if err := os.WriteFile(filepath.Join(assetsDir, "chunk-abc123.js"), []byte("console.log('spa')"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hashed CSS (long cache)
	if err := os.WriteFile(filepath.Join(assetsDir, "index-def456.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func newTestRouter(spaDir string) *gin.Engine {
	log := logger.New("test")
	h := NewHandler(spaDir, log)
	r := gin.New()
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.GET("/api/v1/spa/*filepath", h.ServeAsset)
	return r
}

func TestServeAssetIndexHTML(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/index.html", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (Location: %s): %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if ct := w.Header().Get(headerContType); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %s", ct)
	}
	// index.html should get short cache TTL
	cc := w.Header().Get(headerCacheCtrl)
	if cc != "public, max-age=300" {
		t.Errorf("expected short cache for index.html, got %s", cc)
	}
	if w.Body.String() != "<html>SPA</html>" {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestServeAssetHashedJS(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/assets/chunk-abc123.js", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get(headerContType); ct != jsContentType {
		t.Errorf("expected application/javascript, got %s", ct)
	}
	// Hashed assets should get immutable long cache
	cc := w.Header().Get(headerCacheCtrl)
	if cc != "public, max-age=31536000, immutable" {
		t.Errorf("expected immutable cache for hashed JS, got %s", cc)
	}
}

func TestServeAssetHashedCSS(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/assets/index-def456.css", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get(headerContType); ct != "text/css; charset=utf-8" {
		t.Errorf("expected text/css, got %s", ct)
	}
	cc := w.Header().Get(headerCacheCtrl)
	if cc != "public, max-age=31536000, immutable" {
		t.Errorf("expected immutable cache for CSS, got %s", cc)
	}
}

func TestServeAssetNotFound(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/nonexistent.js", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestServeAssetInvalidVersion(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spa/../etc/passwd", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal, got %d", w.Code)
	}
}

func TestServeAssetPathTraversal(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/../../etc/passwd", nil)
	r.ServeHTTP(w, req)

	// Should be rejected by path validation
	if w.Code == http.StatusOK {
		t.Error("path traversal should not return 200")
	}
}

func TestServeAssetEmptySpaDir(t *testing.T) {
	log := logger.New("test")
	h := NewHandler("", log)
	r := gin.New()
	r.GET("/api/v1/spa/*filepath", h.ServeAsset)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/index.html", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when SPA dir is empty, got %d", w.Code)
	}
}

func TestServeAssetMissingFile(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/", nil)
	r.ServeHTTP(w, req)

	// Version only, no file — should be rejected
	if w.Code == http.StatusOK {
		t.Error("should not serve directory listing")
	}
}

func TestServeAssetVersionOnly(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/spa/abc1234", nil)
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Error("version-only path should not return 200")
	}
}

func TestServeAssetSymlinkRejected(t *testing.T) {
	dir := setupTestSPA(t)

	// Create a symlink inside the version dir
	versionDir := filepath.Join(dir, testVersion)
	symlinkPath := filepath.Join(versionDir, "evil.html")
	target := filepath.Join(versionDir, testIndexFile)
	if err := os.Symlink(target, symlinkPath); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	r := newTestRouter(dir)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", spaBasePath+testVersion+"/evil.html", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("symlinks should be rejected, got %d", w.Code)
	}
}

func TestServeAssetInvalidVersionFormats(t *testing.T) {
	dir := setupTestSPA(t)
	r := newTestRouter(dir)

	tests := []struct {
		path   string
		wantOK bool
	}{
		{"/api/v1/spa/ABC-123_test/index.html", false}, // Valid format but no dir → 404
		{"/api/v1/spa/abc!def/index.html", false},      // Special chars → 400
	}

	for _, tt := range tests {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", tt.path, nil)
		r.ServeHTTP(w, req)

		if tt.wantOK && w.Code != http.StatusOK {
			t.Errorf("path=%q: expected 200, got %d", tt.path, w.Code)
		}
		if !tt.wantOK && w.Code == http.StatusOK {
			t.Errorf("path=%q: should not return 200, got %d", tt.path, w.Code)
		}
	}
}

func TestIsValidVersion(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{testVersion, true},
		{"ABC-123_test", true},
		{"a", true},
		{"0123456789abcdef0123456789abcdef01234567", true},   // 40 chars (full SHA)
		{"0123456789abcdef0123456789abcdef012345678", false}, // 41 chars (too long)
		{"", false},
		{"../etc", false},
		{"abc def", false},
		{"abc!def", false},
		{"abc/def", false},
	}

	for _, tt := range tests {
		got := isValidVersion(tt.input)
		if got != tt.valid {
			t.Errorf("isValidVersion(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

func TestIsHashedAsset(t *testing.T) {
	tests := []struct {
		filename string
		hashed   bool
	}{
		{"chunk-abc123.js", true},
		{"index-def456.css", true},
		{"logo.png", true},
		{"font.woff2", true},
		{testIndexFile, false},
		{"manifest.json", false},
		{"robots.txt", false},
	}

	for _, tt := range tests {
		got := isHashedAsset(tt.filename)
		if got != tt.hashed {
			t.Errorf("isHashedAsset(%q) = %v, want %v", tt.filename, got, tt.hashed)
		}
	}
}

func TestContentTypeForSPA(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{testIndexFile, "text/html; charset=utf-8"},
		{"app.js", jsContentType},
		{"style.css", "text/css; charset=utf-8"},
		{"data.json", "application/json; charset=utf-8"},
		{"logo.svg", "image/svg+xml"},
		{"photo.png", "image/png"},
		{"photo.jpg", "image/jpeg"},
		{"font.woff2", "font/woff2"},
		{"font.woff", "font/woff"},
		{"font.ttf", "font/ttf"},
		{"app.mjs", jsContentType},
		{"photo.gif", "image/gif"},
		{"photo.webp", "image/webp"},
		{"favicon.ico", "image/x-icon"},
		{"chunk.js.map", "application/json"},
		{"unknown.xyz", "application/octet-stream"},
	}

	for _, tt := range tests {
		got := contentTypeForSPA(tt.filename)
		if got != tt.expected {
			t.Errorf("contentTypeForSPA(%q) = %q, want %q", tt.filename, got, tt.expected)
		}
	}
}
