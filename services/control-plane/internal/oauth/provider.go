// Package oauth provides federated identity provider integration.
// Concrete providers (Google, Apple) implement the Provider interface;
// callers consult the Registry by provider name.
package oauth

import "context"

// Provider abstracts an OAuth/OIDC identity provider.
type Provider interface {
	// Name returns the provider key stored in user_sso_identities.provider
	// (e.g., "google", "apple"). Stable across releases; never renamed.
	Name() string

	// AuthorizationURL builds the URL the client opens in the system browser.
	// state, nonce, codeChallenge are caller-generated CSRF/replay/PKCE values.
	AuthorizationURL(state, nonce, codeChallenge string) string

	// Exchange completes the OAuth code exchange. Implementations MUST:
	// (1) exchange the code at the provider's /token endpoint,
	// (2) validate the id_token signature against the provider's JWKS,
	// (3) verify the iss, aud, exp, and nonce claims (nonce must equal params.Nonce),
	// (4) return normalized user info, or an error if any step fails.
	// The interface does not enforce these steps; full responsibility lies with the implementation.
	Exchange(ctx context.Context, params ExchangeParams) (*UserInfo, error)
}

// ExchangeParams holds inputs for completing an OAuth exchange.
type ExchangeParams struct {
	Code         string // authorization code from callback query
	CodeVerifier string // PKCE verifier matching codeChallenge sent at init
	Nonce        string // expected id_token nonce claim

	// RedirectURI is the per-request loopback URI the client supplied at
	// Initiate time. Per RFC 6749 §4.1.3, the redirect_uri at the /token
	// exchange MUST exactly match the redirect_uri used to build the
	// authorization URL — providers reject any mismatch with
	// "redirect_uri_mismatch". Concord's Initiate handler stores the actual
	// loopback URI (with the renderer-chosen ephemeral port) in the
	// sso_state record; Callback replays it here. Empty falls back to the
	// constructor seed (e.g., GoogleProvider.cfg.RedirectURI).
	RedirectURI string

	// AppleUserData is the raw "user" JSON from Apple's first-auth callback.
	// Empty for Google; documented here so the Google implementation can
	// ignore it cleanly.
	AppleUserData string
}

// UserInfo is the normalized identity returned by all providers.
type UserInfo struct {
	Provider       string // matches Provider.Name()
	ProviderUserID string // OIDC sub claim
	Email          string
	EmailVerified  bool
	Name           string // may be empty (Apple subsequent auths)
	AvatarURL      string // may be empty (Apple, or Google users without picture)
	// IsRelayEmail is true if Email is a provider-issued forwarding alias
	// (e.g., Apple privaterelay.appleid.com). Auto-link flows MUST refuse to
	// link relay addresses to existing accounts. Always false for Google.
	IsRelayEmail bool
}
