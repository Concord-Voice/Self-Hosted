// Package spa serves SPA (Single Page Application) assets from a local directory.
// SPA builds are uploaded by deploy-spa.yml via rsync and organised in versioned
// subdirectories keyed by git SHA: {SPA_DIR}/{sha7}/index.html, assets/, etc.
// Cloudflare caches responses via Cache-Control headers — hashed assets get
// immutable/1-year TTL, index.html gets a 5-minute TTL for fast rollback.
package spa

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	// Hashed assets (JS/CSS bundles with content hashes in filename) are immutable.
	// Cloudflare + browsers can cache them indefinitely.
	hashedAssetMaxAge = 31536000 // 1 year

	// index.html and other non-hashed files get a short TTL so new deploys
	// propagate quickly through Cloudflare's cache.
	entryPointMaxAge = 300 // 5 minutes
)

// Handler serves SPA build assets from a versioned directory structure.
type Handler struct {
	spaDir string
	log    *logger.Logger
}

// NewHandler creates a new SPA asset handler.
// spaDir is the root directory containing versioned SPA builds (e.g. /opt/concord/spa).
func NewHandler(spaDir string, log *logger.Logger) *Handler {
	return &Handler{spaDir: spaDir, log: log}
}

// resolveSafePath validates user-supplied version and file path segments, then
// resolves them against the trusted base directory. Returns the absolute path
// to the file and the cleaned file basename, or an error string if invalid.
// This function breaks the taint chain: the returned path is guaranteed to be
// under baseDir with no traversal, symlinks, or directory escapes.
func resolveSafePath(baseDir, version, filePath string) (absPath string, basename string, errMsg string) {
	if !isValidVersion(version) {
		return "", "", "Invalid version identifier"
	}

	cleanPath := filepath.Clean(filePath)
	if cleanPath == "." || strings.Contains(cleanPath, "..") {
		return "", "", "Invalid file path"
	}

	absSpaDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", "", "Configuration error"
	}

	candidate := filepath.Join(absSpaDir, version, cleanPath)
	if !strings.HasPrefix(candidate, absSpaDir+string(filepath.Separator)) {
		return "", "", "Invalid file path"
	}

	// Verify file exists and is not a symlink or directory
	info, err := os.Lstat(candidate)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", ""
	}

	return candidate, filepath.Base(cleanPath), ""
}

// ServeAsset handles GET /api/v1/spa/*filepath
// Expected URL structure: /api/v1/spa/{sha7}/index.html or /api/v1/spa/{sha7}/assets/chunk-abc123.js
func (h *Handler) ServeAsset(c *gin.Context) {
	if h.spaDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "SPA hosting not configured"})
		return
	}

	rawParam := c.Param("filepath")
	if rawParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing filepath"})
		return
	}
	rawParam = strings.TrimPrefix(rawParam, "/")

	// Expect at minimum: {version}/{file} (e.g. "abc1234/index.html")
	parts := strings.SplitN(rawParam, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SPA path — expected {version}/{file}"})
		return
	}

	safePath, basename, errMsg := resolveSafePath(h.spaDir, parts[0], parts[1])
	if errMsg != "" {
		status := http.StatusBadRequest
		if errMsg == "Configuration error" {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": errMsg})
		return
	}
	if safePath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	// Set caching headers based on file type
	if isHashedAsset(basename) {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", hashedAssetMaxAge))
	} else {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", entryPointMaxAge))
	}
	c.Header("Content-Type", contentTypeForSPA(basename))

	// Override the global default-src 'none' CSP from SecurityHeaders middleware:
	// the SPA's index.html ships its own CSP meta tag that permits the assets it
	// needs (self scripts/styles, wasm-unsafe-eval, the api/ws origins). Browsers
	// intersect multiple CSPs, so leaving the strict header in place blocks every
	// asset and renders an empty window in the desktop client.
	c.Writer.Header().Del("Content-Security-Policy")

	// Serve via http.Dir (Go's built-in sandboxed filesystem) to satisfy
	// static analysis — http.Dir prevents path traversal by design.
	fs := http.Dir(h.spaDir)
	f, err := fs.Open(path.Join(parts[0], filepath.ToSlash(parts[1])))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to stat asset"})
		return
	}
	http.ServeContent(c.Writer, c.Request, basename, info.ModTime(), f.(io.ReadSeeker))
}

// isValidVersion checks that a version string is safe (alphanumeric + hyphens).
func isValidVersion(v string) bool {
	if len(v) == 0 || len(v) > 40 {
		return false
	}
	for _, c := range v {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// isHashedAsset returns true for files that contain a content hash in their name
// (Vite output pattern: chunk-abc123.js, index-def456.css, etc.)
func isHashedAsset(filename string) bool {
	ext := strings.ToLower(path.Ext(filename))
	switch ext {
	case ".js", ".css", ".woff", ".woff2", ".ttf", ".eot":
		return true
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico":
		return true
	default:
		return false
	}
}

// contentTypeForSPA returns the Content-Type for common SPA asset types.
func contentTypeForSPA(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".map":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
