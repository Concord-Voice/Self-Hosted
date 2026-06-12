package oauth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// appleSessionRequest is the POST body for AppleSession (#974). The desktop
// main process completes the Apple code exchange itself and posts the
// resulting id_token for server-side re-verification — the server NEVER
// trusts client-side verification (CWE-345 defense in depth).
//
// AppleUserData carries Apple's first-auth "user" JSON under the same wire
// name Callback used: same 64 KiB cap, same treat-as-opaque posture (a UX
// display-name suggestion, never an authoritative identity signal). Never
// logged in any error path.
type appleSessionRequest struct {
	IDToken       string `json:"id_token" binding:"required"`
	State         string `json:"state" binding:"required"`
	AppleUserData string `json:"apple_user_data,omitempty"`
}

// AppleSession implements POST /api/v1/auth/sso/:provider/session (#974) —
// the terminal server step of the client-driven Apple exchange.
//
// Flow:
//  1. Provider gate: apple-only until #975 migrates Google (404 otherwise,
//     broker parity).
//  2. State validation — error class `invalid_state` (401): record lookup,
//     GET-then-DEL one-shot (Callback parity; GETDEL is the tracked
//     hardening), constant-time compare against the record-embedded copy
//     (#972 / CWE-385; pre-#972 records decode State as "" and fail closed),
//     provider binding. Runs AFTER the broker consumed its sibling one-shot
//     key earlier in the flow — independent keys, compatible orderings.
//  3. id_token verification — error class `invalid_id_token` (401): the
//     existing #271 verifier (signature via JWKS with RS256-only keyfunc,
//     iss/aud/exp/iat, nonce binding against rec.Nonce, nonce_supported).
//     The two 401 classes exist because the client remediation differs
//     (restart the flow vs never-retry-this-token); WHICH check failed
//     within a class stays opaque.
//  4. Email-verified gate (403, Callback parity).
//  5. parseAppleUserData + isAppleRelayEmail → UserInfo (the helpers the
//     retired Exchange consumed), then classifyCallback + the existing
//     respond* terminal step — byte-identical response shapes, so the
//     renderer's register/link/MFA paths are untouched.
func (h *Handler) AppleSession(c *gin.Context) {
	if c.Param("provider") != "apple" {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	provider, err := h.deps.Registry.Get("apple")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}
	apple, ok := provider.(*AppleProvider)
	if !ok {
		// A registered "apple" that is not the concrete AppleProvider has no
		// validateIDToken — treat as unconfigured rather than 500.
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}

	var req appleSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.State) > maxStateLength {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_request"})
		return
	}

	ctx := c.Request.Context()

	stateKey := stateKeyPrefix + req.State
	raw, err := h.deps.Redis.Get(ctx, stateKey).Bytes()
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Best-effort delete: the record is already read, so a delete failure
	// cannot enable replay within this request (Callback parity).
	_, _ = h.deps.Redis.Del(ctx, stateKey).Result()

	var rec ssoStateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.State), []byte(rec.State)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}
	// Provider names are not secret material — plain equality is correct.
	if rec.Provider != "apple" {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_state"})
		return
	}

	claims, err := apple.validateIDToken(ctx, req.IDToken, rec.Nonce)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_id_token"})
		return
	}

	if !bool(claims.EmailVerified) {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "oauth_email_unverified"})
		return
	}

	appleUserData := req.AppleUserData
	if len(appleUserData) > maxAppleUserDataBytes {
		appleUserData = ""
	}
	info := &UserInfo{
		Provider:       "apple",
		ProviderUserID: claims.Subject,
		Email:          claims.Email,
		EmailVerified:  bool(claims.EmailVerified),
		Name:           parseAppleUserData(appleUserData),
		AvatarURL:      "", // Apple does not provide avatars.
		IsRelayEmail:   isAppleRelayEmail(claims.Email, bool(claims.IsPrivateEmail)),
	}

	switch h.classifyCallback(ctx, info) {
	case branchExistingSSO:
		h.respondExistingSSO(c, info)
	case branchAccountLink:
		h.respondAccountLink(c, info)
	default: // branchNewUser
		h.respondNewUser(c, info)
	}
}
