// Package updates serves desktop client update assets from a local directory.
// Release assets are downloaded by deploy.sh during the deploy cycle —
// the handler simply serves files from disk with appropriate caching headers.
// Cloudflare caches binary responses via Cache-Control.
package updates

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	// Binary downloads get a 24h Cache-Control so Cloudflare caches them.
	binaryCacheMaxAge = 86400

	// Metadata gets a 5m Cache-Control — balances freshness with caching.
	metadataCacheMaxAge = 300
)

// Handler serves update assets from a local directory.
type Handler struct {
	cfg *config.Config
	log *logger.Logger
}

// NewHandler creates a new update handler.
func NewHandler(cfg *config.Config, log *logger.Logger) *Handler {
	return &Handler{cfg: cfg, log: log}
}

// ServeUpdateAsset handles GET /api/v1/updates/*filename
// Serves release assets (yml metadata + binaries) from the local releases directory.
func (h *Handler) ServeUpdateAsset(c *gin.Context) {
	releasesDir := h.cfg.ReleasesDir
	if releasesDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Update proxy not configured"})
		return
	}

	filename := c.Param("filename")
	if filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing filename"})
		return
	}
	// Gin includes leading slash in wildcard param
	filename = strings.TrimPrefix(filename, "/")

	// Validate filename — only allow safe characters (no path traversal)
	if strings.Contains(filename, "/") || strings.Contains(filename, "..") || filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
		return
	}

	// Resolve and verify the file path is within the releases directory
	absReleasesDir, err := filepath.Abs(releasesDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Configuration error"})
		return
	}
	absPath, err := filepath.Abs(filepath.Join(releasesDir, filename))
	if err != nil || !strings.HasPrefix(absPath, absReleasesDir+string(filepath.Separator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
		return
	}

	// Check file exists — Lstat to reject symlinks
	info, err := os.Lstat(absPath)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	// Set caching headers
	if isMetadataFile(filename) {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", metadataCacheMaxAge))
	} else {
		c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", binaryCacheMaxAge))
	}
	c.Header("Content-Type", contentTypeForFile(filename))

	c.File(absPath)
}

// isMetadataFile returns true for electron-updater yml files.
func isMetadataFile(filename string) bool {
	return filename == "latest-mac.yml" ||
		filename == "latest.yml" ||
		filename == "latest-linux.yml" ||
		filename == "latest-linux-arm64.yml"
}

// contentTypeForFile returns the Content-Type for a release asset.
func contentTypeForFile(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch ext {
	case ".yml", ".yaml":
		return "text/yaml; charset=utf-8"
	case ".zip":
		return "application/zip"
	case ".exe":
		return "application/vnd.microsoft.portable-executable"
	case ".deb":
		return "application/vnd.debian.binary-package"
	case ".rpm":
		return "application/x-rpm"
	case ".appimage":
		return "application/x-executable"
	case ".nupkg":
		return "application/octet-stream"
	default:
		return "application/octet-stream"
	}
}
