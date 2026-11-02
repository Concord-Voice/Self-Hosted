// Package clientconfig serves dynamic configuration to desktop/web clients.
// The endpoint is public (pre-auth) so clients can discover infrastructure
// details and feature flags before login.
package clientconfig

import (
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// Handler handles client config requests.
type Handler struct {
	cfg     *config.Config
	liveSpa *config.LiveSpaConfig
	log     *logger.Logger
}

// NewHandler creates a new client config handler.
func NewHandler(cfg *config.Config, liveSpa *config.LiveSpaConfig, log *logger.Logger) *Handler {
	return &Handler{cfg: cfg, liveSpa: liveSpa, log: log}
}

// FeatureFlags represents toggleable client features.
type FeatureFlags struct {
	// GifsEnabled signals whether the KLIPY GIF integration is available
	// (i.e. the server is configured with a KLIPY app key). The key itself
	// is NEVER sent to clients — all KLIPY traffic must go through the
	// control-plane proxy at /api/v1/klipy/* so the app key stays server-side.
	GifsEnabled bool `json:"gifsEnabled"`
}

// TURNConfig holds non-secret TURN server info for WebRTC ICE.
type TURNConfig struct {
	Host  string `json:"host,omitempty"`
	Realm string `json:"realm,omitempty"`
}

// Response is the payload for GET /api/v1/client/config.
type Response struct {
	// MinVersion is the minimum client version allowed to connect.
	// Clients below this version must update before continuing.
	MinVersion string `json:"minVersion"`

	// FeatureFlags toggles client capabilities server-side.
	FeatureFlags FeatureFlags `json:"featureFlags"`

	// MediaPlaneURL lets the server override the media SFU URL
	// without rebuilding the client (hotfix for Issue #155 Tier 2).
	MediaPlaneURL string `json:"mediaPlaneUrl"`

	// TURN provides non-secret TURN server details for ICE configuration.
	TURN TURNConfig `json:"turn"`

	// SpaURL is the remote SPA URL for Tier 3 hot updates.
	// Empty means the client should use its bundled renderer.
	SpaURL string `json:"spaUrl,omitempty"`

	// SpaIpcContract is the minimum IPC contract version required by the remote SPA.
	// Shells with a lower contract version fall back to their bundled SPA.
	// Zero means no remote SPA is available.
	SpaIpcContract int `json:"spaIpcContract,omitempty"`
}

// GetConfig returns the current client configuration.
// This endpoint is public — no auth required.
func (h *Handler) GetConfig(c *gin.Context) {
	channel := config.SpaChannel(c.Query("channel"))
	var spa config.SpaSnapshot
	if h.liveSpa != nil {
		spa = h.liveSpa.Snapshot(channel)
	} else {
		spa = h.cfg.Snapshot(channel)
	}

	resp := Response{
		MinVersion: h.cfg.ClientMinVersion,
		FeatureFlags: FeatureFlags{
			GifsEnabled: h.cfg.KlipyAPIKey != "",
		},
		MediaPlaneURL: h.cfg.MediaPlaneURL,
		TURN: TURNConfig{
			Host:  h.cfg.TURNServerHost,
			Realm: h.cfg.TURNRealm,
		},
		SpaURL:         spa.URL,
		SpaIpcContract: spa.IPCContract,
	}

	// Never cache /client/config — clients poll this to discover new SPA builds.
	// A stale CDN/browser cache means a new SPA hot-update sits unseen until the
	// max-age expires, defeating the entire Tier 3 update mechanism.
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, resp)
}
