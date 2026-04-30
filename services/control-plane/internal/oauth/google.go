package oauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GoogleConfig is the constructor input for GoogleProvider.
// Mirrored on pkg/config/GoogleSSOConfig but kept package-local so callers
// pass exactly what GoogleProvider needs and nothing more.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string // #nosec G117 G101 -- False positive: config field name, actual secret loaded from env
	RedirectURI  string

	// Endpoint overrides (default to Google's production URLs when empty).
	// Tests point these at httptest.Server.
	TokenEndpoint string
	JWKSEndpoint  string
	Issuer        string

	// HTTPClient is optional; defaults to a 10s-timeout client whose Transport
	// is the standard http.DefaultTransport (TLS verification enabled).
	// Tests inject a fake httptest.Server-backed client.
	// Production callers MUST leave this nil or supply a client whose Transport
	// preserves TLS verification — never set InsecureSkipVerify: true.
	HTTPClient *http.Client
}

const (
	googleAuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	// #nosec G101 -- False positive: public OAuth endpoint URL, not a credential.
	defaultTokenEndpoint = "https://oauth2.googleapis.com/token"
	defaultJWKSEndpoint  = "https://www.googleapis.com/oauth2/v3/certs"
	defaultIssuer        = "https://accounts.google.com"
)

// Compile-time check that *GoogleProvider implements Provider.
var _ Provider = (*GoogleProvider)(nil)

// GoogleProvider implements Provider for Google's OIDC code flow with PKCE.
type GoogleProvider struct {
	cfg        GoogleConfig
	httpClient *http.Client

	jwksMu    sync.RWMutex
	jwksCache map[string]cachedKey
}

// NewGoogleProvider validates configuration and returns a provider ready to use.
func NewGoogleProvider(cfg GoogleConfig) (*GoogleProvider, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oauth/google: ClientID is required")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("oauth/google: ClientSecret is required")
	}
	if cfg.RedirectURI == "" {
		return nil, fmt.Errorf("oauth/google: RedirectURI is required")
	}
	if cfg.TokenEndpoint == "" {
		cfg.TokenEndpoint = defaultTokenEndpoint
	}
	if cfg.JWKSEndpoint == "" {
		cfg.JWKSEndpoint = defaultJWKSEndpoint
	}
	if cfg.Issuer == "" {
		cfg.Issuer = defaultIssuer
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &GoogleProvider{cfg: cfg, httpClient: httpClient}, nil
}

// Name returns "google".
func (g *GoogleProvider) Name() string { return "google" }

// AuthorizationURL builds the URL the client opens in the system browser.
func (g *GoogleProvider) AuthorizationURL(state, nonce, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", g.cfg.ClientID)
	q.Set("redirect_uri", g.cfg.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	return googleAuthEndpoint + "?" + q.Encode()
}

// Exchange performs the code-exchange + id_token validation flow.
func (g *GoogleProvider) Exchange(ctx context.Context, params ExchangeParams) (*UserInfo, error) {
	// Per RFC 6749 §4.1.3, the redirect_uri sent here MUST equal the
	// redirect_uri used to build the authorization URL. The handler builds
	// the auth URL with the per-request loopback URI (chosen ephemeral port
	// in the renderer), stores it in the sso_state record, and replays it
	// via params.RedirectURI. Fall back to the constructor seed only when
	// callers omit the param — production paths always set it.
	redirectURI := g.cfg.RedirectURI
	if params.RedirectURI != "" {
		redirectURI = params.RedirectURI
	}
	tokens, err := g.exchangeCode(ctx, params.Code, params.CodeVerifier, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("oauth/google: code exchange: %w", err)
	}
	claims, err := g.validateIDToken(ctx, tokens.IDToken, params.Nonce)
	if err != nil {
		return nil, fmt.Errorf("oauth/google: id_token validation: %w", err)
	}
	return &UserInfo{
		Provider:       "google",
		ProviderUserID: claims.Sub,
		Email:          claims.Email,
		EmailVerified:  claims.EmailVerified,
		Name:           claims.Name,
		AvatarURL:      claims.Picture,
		IsRelayEmail:   false,
	}, nil
}

type googleTokenResponse struct {
	IDToken string `json:"id_token"`
	// Note: Google also returns access_token, expires_in, and token_type.
	// We do not retain them — Concord uses the id_token only for OIDC identity.
}

func (g *GoogleProvider) exchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*googleTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", g.cfg.ClientID)
	form.Set("client_secret", g.cfg.ClientSecret)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.httpClient.Do(req) //nolint:gosec // TokenEndpoint set from trusted config (env or hardcoded default)
	if err != nil {
		return nil, fmt.Errorf("HTTP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	var out googleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.IDToken == "" {
		return nil, errors.New("token response missing id_token")
	}
	return &out, nil
}

type googleClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Nonce         string `json:"nonce"`
	jwt.RegisteredClaims
}

// validateIDToken verifies signature, iss, aud, exp, and nonce.
func (g *GoogleProvider) validateIDToken(ctx context.Context, idToken, expectedNonce string) (*googleClaims, error) {
	parser := jwt.NewParser(
		jwt.WithIssuer(g.cfg.Issuer),
		jwt.WithAudience(g.cfg.ClientID),
		jwt.WithExpirationRequired(),
	)
	claims := &googleClaims{}
	tok, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("id_token has no kid")
		}
		return g.fetchKey(ctx, kid)
	})
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, fmt.Errorf("id_token exp: %w", err)
		case errors.Is(err, jwt.ErrTokenInvalidAudience):
			return nil, fmt.Errorf("id_token aud: %w", err)
		case errors.Is(err, jwt.ErrTokenInvalidIssuer):
			return nil, fmt.Errorf("id_token iss: %w", err)
		}
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("id_token invalid")
	}
	if claims.Nonce != expectedNonce {
		return nil, fmt.Errorf("id_token nonce mismatch: got %q expected %q", claims.Nonce, expectedNonce)
	}
	return claims, nil
}

// JWKS cache (1h TTL).
type cachedKey struct {
	key       *rsa.PublicKey
	expiresAt time.Time
}

// jwksKey is one entry in the Google JWKS response. RSA-only fields; we
// ignore non-RSA entries during parse.
type jwksKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwksResponse is the top-level JWKS payload Google returns at /oauth2/v3/certs.
type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}

// parseJWKResponse converts a decoded JWKS payload into a kid→public-key map.
// Per-key parse failures (non-RSA Kty, malformed base64 N or E) are skipped
// silently — Google occasionally publishes keys we don't understand and the
// goal is to extract whatever signing keys we CAN use, not to fail the whole
// fetch on one stray entry.
func parseJWKResponse(resp *jwksResponse) map[string]*rsa.PublicKey {
	out := make(map[string]*rsa.PublicKey, len(resp.Keys))
	for _, k := range resp.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		eInt := 0
		for _, b := range eBytes {
			eInt = eInt<<8 | int(b)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: eInt}
	}
	return out
}

func (g *GoogleProvider) fetchKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.jwksMu.RLock()
	if ck, ok := g.jwksCache[kid]; ok && time.Now().Before(ck.expiresAt) {
		g.jwksMu.RUnlock()
		return ck.key, nil
	}
	g.jwksMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.JWKSEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpClient.Do(req) //nolint:gosec // JWKSEndpoint set from trusted config (env or hardcoded default)
	if err != nil {
		return nil, fmt.Errorf("JWKS HTTP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var jwksResp jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwksResp); err != nil {
		return nil, fmt.Errorf("JWKS decode: %w", err)
	}

	parsed := parseJWKResponse(&jwksResp)

	// Concurrent cache misses on the same kid will all issue a JWKS fetch and
	// each acquire this lock in turn to write the same keys (last-write-wins).
	// Acceptable at human-paced auth volume; if telemetry shows fetch storms,
	// coalesce concurrent fetches via golang.org/x/sync/singleflight.Group.
	g.jwksMu.Lock()
	defer g.jwksMu.Unlock()
	if g.jwksCache == nil {
		g.jwksCache = make(map[string]cachedKey)
	}
	expiresAt := time.Now().Add(time.Hour)
	for k, pub := range parsed {
		g.jwksCache[k] = cachedKey{key: pub, expiresAt: expiresAt}
	}

	if ck, ok := g.jwksCache[kid]; ok {
		return ck.key, nil
	}
	return nil, fmt.Errorf("JWKS: kid %q not found", kid)
}
