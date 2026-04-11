// Package klipy provides HTTP handlers that proxy KLIPY API and media
// requests through the control-plane. The proxy is used by the desktop
// client — see PrivacySecuritySection.tsx and the renderer's
// gifProvider/klipyClient.ts.
//
// The proxy is allowed by KLIPY's API ToS (no proxy prohibition exists in
// their terms). It exists so KLIPY never sees per-user IP addresses or
// search terms.
//
// As of this package version, GIF media is also fully proxied: the renderer
// rewrites all KLIPY CDN URLs to /api/v1/klipy/media?url=<encoded> before
// setting them as <img>/<video> src attributes, so the user's IP is never
// exposed to KLIPY's CDN. See the Media handler below and
// klipyClient.ts#rewriteMediaUrl.
//
// Compliance notes:
//   - We never modify API response bodies returned by KLIPY.
//   - We never log slugs or search terms in the proxy routes (privacy promise).
//   - We never cache responses except via standard Cache-Control headers on
//     media binaries — KLIPY does not require revalidation.
//   - We never present KLIPY as a support contact (Section 1).
package klipy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	upstreamTimeout = 10 * time.Second
	// mediaTimeout is intentionally longer than upstreamTimeout: media binaries
	// can be several MB and CDN latency varies by geography.
	mediaTimeout = 30 * time.Second
	maxQueryLen  = 100
	maxPerPage   = 50

	headerContentType  = "Content-Type"
	headerCacheControl = "Cache-Control"
	errUpstreamDown    = "GIF service temporarily unavailable"
	errUpstreamFailed  = "GIF service request failed"
)

// klipyAPIBase is the upstream base URL for KLIPY's API. Defined as a package
// variable rather than a const so tests can substitute an httptest server URL.
// Production code never reassigns it.
var klipyAPIBase = "https://api.klipy.com/api/v1"

// allowedMediaHosts is the host allowlist for the media proxy. Any URL whose
// host does not match one of these patterns is rejected with 400. This is the
// primary SSRF guard — never proxy arbitrary URLs.
//
// Covered hosts:
//   - klipy.com             — bare apex (uncommon but defensive)
//   - *.klipy.com           — API, media, content, and CDN subdomains
//   - klipy.io              — bare apex for the .io TLD
//   - *.klipy.io            — CDN subdomains on the .io TLD
//
// We use a regexp because KLIPY uses numbered CDN sub-hosts (media0, media1,
// etc.). Defined as a package variable rather than a regex literal in the
// function so tests can substitute a more permissive pattern (allowing
// 127.0.0.1) when validating the proxy logic against an httptest server.
var allowedMediaHosts = regexp.MustCompile(`^(klipy\.com|[a-z0-9-]+\.klipy\.com|klipy\.io|[a-z0-9-]+\.klipy\.io)$`)

// Handler holds the dependencies for the KLIPY proxy.
type Handler struct {
	cfg         *config.Config
	log         *logger.Logger
	client      *http.Client // API proxy (JSON endpoints)
	mediaClient *http.Client // media proxy (binary streaming, longer timeout)
}

// NewHandler constructs a KLIPY proxy handler with sane defaults.
// Two http.Clients share the same underlying transport (connection pool) but
// have different timeouts: API calls use a tight 10s deadline while media
// binaries get 30s to accommodate larger payloads and CDN variance.
func NewHandler(cfg *config.Config, log *logger.Logger) *Handler {
	transport := http.DefaultTransport
	return &Handler{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout:   upstreamTimeout,
			Transport: transport,
		},
		mediaClient: &http.Client{
			Timeout:   mediaTimeout,
			Transport: transport,
		},
	}
}

// --- API endpoint proxies ---

// Trending forwards GET /gifs/trending to KLIPY.
func (h *Handler) Trending(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/trending", []string{"page", "per_page", "customer_id", "locale", "format_filter"})
}

// Search forwards GET /gifs/search to KLIPY.
func (h *Handler) Search(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q parameter is required"})
		return
	}
	if len(q) > maxQueryLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q parameter exceeds maximum length"})
		return
	}
	h.proxyAPIGet(c, "/gifs/search", []string{"q", "page", "per_page", "customer_id", "locale", "content_filter", "format_filter"})
}

// Categories forwards GET /gifs/categories to KLIPY.
func (h *Handler) Categories(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/categories", []string{"locale"})
}

// Recent forwards GET /gifs/recent/{customer_id} to KLIPY.
func (h *Handler) Recent(c *gin.Context) {
	customerID := c.Param("customerID")
	if !ValidateSlug(&customerID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer_id"})
		return
	}
	h.proxyAPIGet(c, "/gifs/recent/"+customerID, []string{"page", "per_page"})
}

// HideRecent forwards DELETE /gifs/recent/{customer_id} to KLIPY.
func (h *Handler) HideRecent(c *gin.Context) {
	customerID := c.Param("customerID")
	if !ValidateSlug(&customerID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer_id"})
		return
	}
	h.proxyAPIRequest(c, http.MethodDelete, "/gifs/recent/"+customerID, nil)
}

// Items forwards GET /gifs/items to KLIPY (used to resolve saved-tab slugs).
func (h *Handler) Items(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/items", []string{"slugs", "customer_id", "format_filter"})
}

// Share forwards POST /gifs/share/{slug} to KLIPY (optional analytics).
func (h *Handler) Share(c *gin.Context) {
	slug := c.Param("slug")
	if !ValidateSlug(&slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": SlugValidationError(&slug)})
		return
	}
	h.proxyAPIRequest(c, http.MethodPost, "/gifs/share/"+slug, nil)
}

// Report forwards POST /gifs/report/{slug} to KLIPY.
func (h *Handler) Report(c *gin.Context) {
	slug := c.Param("slug")
	if !ValidateSlug(&slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": SlugValidationError(&slug)})
		return
	}
	h.proxyAPIRequest(c, http.MethodPost, "/gifs/report/"+slug, nil)
}

// RandomID forwards GET /randomid to KLIPY (per-user opaque session ID).
//
// Deprecated: kept for compatibility with any in-flight clients. New clients
// should call CustomerID below, which generates the opaque ID server-side
// without depending on KLIPY's /randomid endpoint (which is undocumented and
// occasionally returns 404).
func (h *Handler) RandomID(c *gin.Context) {
	h.proxyAPIGet(c, "/randomid", nil)
}

// CustomerID returns a fresh opaque per-user identifier for KLIPY personalization.
//
// KLIPY's customer_id is an opaque, server-defined string — KLIPY does not
// validate any particular format. We generate a UUID v4 server-side rather
// than calling KLIPY's undocumented /randomid endpoint (which has been
// observed returning 404 in production, see QA bug #571 item #15). This
// preserves the personalization feature without leaving a hard dependency
// on a third-party endpoint that may disappear, and it removes the need for
// the client to ever speak directly to api.klipy.com.
//
// Auth requirements match the rest of the klipy proxy routes (JWT-protected,
// rate-limited via the same apiLimiter middleware in router.go).
func (h *Handler) CustomerID(c *gin.Context) {
	id := uuid.NewString()
	c.JSON(http.StatusOK, gin.H{"customer_id": id})
}

// --- Media proxy ---

// Media streams a KLIPY-hosted GIF/MP4/WEBP binary back to the client.
// Accepts the full upstream URL via ?url=<urlencoded>. The host must match
// the allowedMediaHosts whitelist; query params on the upstream URL are
// preserved verbatim (defensive against future signed-URL behavior).
func (h *Handler) Media(c *gin.Context) {
	rawURL := c.Query("url")
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url parameter is required"})
		return
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}
	if !allowedMediaHosts.MatchString(parsed.Host) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url host not allowed"})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}

	// Use the dedicated media client (30s timeout) instead of the API client.
	resp, err := h.mediaClient.Do(req) //nolint:gosec // URL validated: scheme=https + host allowlist
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	if resp.StatusCode >= 400 {
		c.Status(resp.StatusCode)
		return
	}

	// Forward content headers from upstream
	if ct := resp.Header.Get(headerContentType); ct != "" {
		c.Header(headerContentType, ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	// Use the upstream Cache-Control if present; otherwise apply a conservative
	// public cache with a 1-hour TTL so CDN bursts are absorbed without risk of
	// stale content. KLIPY ToS does not prohibit client-side caching.
	if cc := resp.Header.Get(headerCacheControl); cc != "" {
		c.Header(headerCacheControl, cc)
	} else {
		c.Header(headerCacheControl, "public, max-age=3600")
	}
	c.Status(http.StatusOK)
	if _, copyErr := io.Copy(c.Writer, resp.Body); copyErr != nil && !errors.Is(copyErr, io.EOF) {
		// Best-effort: client likely disconnected mid-stream. Don't log slugs.
		return
	}
}

// --- Internal helpers ---

// proxyAPIGet builds a KLIPY API URL from the path + whitelisted query params,
// fetches the upstream JSON response, and forwards it to the client.
func (h *Handler) proxyAPIGet(c *gin.Context, path string, allowedParams []string) {
	upstreamURL := h.buildUpstreamURL(path, c, allowedParams)
	h.forwardJSON(c, http.MethodGet, upstreamURL, nil)
}

// proxyAPIRequest is the same as proxyAPIGet but for non-GET methods.
// No request body forwarding is supported in v1 — KLIPY's POST/DELETE endpoints
// in our scope take all parameters in the URL.
func (h *Handler) proxyAPIRequest(c *gin.Context, method, path string, _ io.Reader) {
	upstreamURL := h.buildUpstreamURL(path, c, nil)
	h.forwardJSON(c, method, upstreamURL, nil)
}

// sanitizeParam normalizes a query param value for forwarding to KLIPY.
// Numeric pagination params are validated; per_page is clamped to maxPerPage
// so callers cannot request unbounded results. Returns the value to forward,
// or "" to skip the param entirely.
func sanitizeParam(name, raw string) string {
	switch name {
	case "per_page":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return ""
		}
		if n > maxPerPage {
			n = maxPerPage
		}
		return strconv.Itoa(n)
	case "page":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return ""
		}
		return strconv.Itoa(n)
	default:
		return raw
	}
}

// buildUpstreamURL constructs the upstream KLIPY URL with the server-side
// app_key injected into the path and only the whitelisted query params copied
// from the incoming request. Any other query params (including a client-supplied
// app_key, defense-in-depth) are dropped. Numeric pagination params are
// validated and clamped via sanitizeParam.
func (h *Handler) buildUpstreamURL(path string, c *gin.Context, allowedParams []string) string {
	base := fmt.Sprintf("%s/%s%s", klipyAPIBase, h.cfg.KlipyAPIKey, path)
	if len(allowedParams) == 0 {
		return base
	}
	values := url.Values{}
	for _, name := range allowedParams {
		raw := c.Query(name)
		if raw == "" {
			continue
		}
		if v := sanitizeParam(name, raw); v != "" {
			values.Set(name, v)
		}
	}
	if len(values) == 0 {
		return base
	}
	if strings.Contains(base, "?") {
		return base + "&" + values.Encode()
	}
	return base + "?" + values.Encode()
}

// forwardJSON performs the upstream HTTP request and copies the response to the
// client. JSON content type is preserved; rate limit + auth headers are stripped
// to avoid leaking the app_key in error envelopes.
func (h *Handler) forwardJSON(c *gin.Context, method, upstreamURL string, body io.Reader) {
	req, err := http.NewRequestWithContext(c.Request.Context(), method, upstreamURL, body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req) //nolint:gosec // upstreamURL constructed from config base URL, not user input
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	if resp.StatusCode >= 400 {
		// Mirror the status but never forward the upstream error body — KLIPY's
		// error envelopes can echo the URL (and therefore the app_key) and we
		// must not leak that to the client. Surface a generic message instead.
		c.JSON(resp.StatusCode, gin.H{"error": errUpstreamFailed})
		return
	}

	if ct := resp.Header.Get(headerContentType); ct != "" {
		c.Header(headerContentType, ct)
	}
	c.Status(resp.StatusCode)
	if _, copyErr := io.Copy(c.Writer, resp.Body); copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return
	}
}
