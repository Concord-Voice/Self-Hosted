package middleware

import (
	"bytes"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

// Stale-key policy: a cached key verifies only while its key set is younger
// than jwksTTL. An expired key is never served without a successful refresh,
// whether that refresh failed or was throttled behind a failure, because the
// issuer may have withdrawn it. Availability comes from refreshing ahead of
// expiry instead: inside the last jwksRefreshAhead of the TTL a still-valid
// key keeps verifying while one background refresh runs, so a transient
// outage near the hour gets retries without extending any key's maximum age.
const (
	jwksTTL             = time.Hour
	jwksRefreshAhead    = 10 * time.Minute
	jwksRefreshInterval = time.Second
	jwksRefreshKey      = "jwks"
	negativeKIDTTL      = time.Minute
	maxNegativeKIDs     = 256
	maxJWKSBody         = 1 << 20
	clockSkew           = 60 * time.Second
)

var (
	errMalformed    = errors.New("malformed")
	errBadSignature = errors.New("bad_signature")
	errUnknownKID   = errors.New("unknown_kid")
	errBadAlg       = errors.New("bad_alg")
	errAudMismatch  = errors.New("aud_mismatch")
	errExpired      = errors.New("expired")
	errBadIssuer    = errors.New("bad_issuer")

	nowFunc = time.Now
)

type accessClaims struct {
	Aud   audience `json:"aud"`
	Exp   int64    `json:"exp"`
	Iat   int64    `json:"iat"`
	Nbf   int64    `json:"nbf"`
	Iss   string   `json:"iss"`
	Email string   `json:"email"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

type accessVerifier struct {
	jwksURL string
	issuer  string
	aud     string
	client  *http.Client

	mu             sync.RWMutex
	keys           map[string]*rsa.PublicKey
	fetchedAt      time.Time
	lastAttempt    time.Time
	lastRefreshErr error
	negativeKIDs   map[string]time.Time
	refreshGroup   singleflight.Group
}

// cachedKey is one consistent read of the cache for a KID.
type cachedKey struct {
	key       *rsa.PublicKey
	known     bool
	age       time.Duration
	negative  bool
	throttled bool
}

func newAccessVerifier(teamDomain, aud string) *accessVerifier {
	td := strings.TrimRight(teamDomain, "/")
	return &accessVerifier{
		jwksURL:      td + "/cdn-cgi/access/certs",
		issuer:       td,
		aud:          aud,
		client:       &http.Client{Timeout: 5 * time.Second},
		keys:         map[string]*rsa.PublicKey{},
		negativeKIDs: map[string]time.Time{},
	}
}

func (v *accessVerifier) Verify(token string) (*accessClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errMalformed
	}

	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil {
		return nil, errMalformed
	}
	if hdr.Alg != "RS256" {
		return nil, errBadAlg
	}

	pub, err := v.lookupKey(hdr.Kid)
	if err != nil {
		return nil, err
	}

	if err := verifyAccessSignature(token, pub); err != nil {
		return nil, err
	}

	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errMalformed
	}
	var claims accessClaims
	if json.Unmarshal(pb, &claims) != nil {
		return nil, errMalformed
	}
	if err := v.validateClaims(&claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func verifyAccessSignature(token string, pub *rsa.PublicKey) error {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithoutClaimsValidation(),
	)
	if _, err := parser.ParseWithClaims(token, jwt.MapClaims{}, func(t *jwt.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, errBadAlg
		}
		return pub, nil
	}); err != nil {
		if errors.Is(err, errBadAlg) {
			return errBadAlg
		}
		if errors.Is(err, jwt.ErrTokenMalformed) {
			return errMalformed
		}
		return errBadSignature
	}
	return nil
}

func (v *accessVerifier) lookupKey(kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, errUnknownKID
	}

	c := v.cached(kid, nowFunc())
	if c.known && c.age < jwksTTL {
		if c.age >= jwksTTL-jwksRefreshAhead && !c.throttled {
			// Refresh ahead without blocking this still-valid key. DoChan joins an
			// in-flight refresh, and its buffered result needs no reader.
			v.refreshGroup.DoChan(jwksRefreshKey, v.refreshResult)
		}
		return c.key, nil
	}
	if !c.known && c.negative {
		return nil, errUnknownKID
	}

	fetched, err := v.refresh()
	c = v.cached(kid, nowFunc())
	switch {
	case c.known && c.age < jwksTTL:
		return c.key, nil
	case c.known || err != nil:
		// The key set is still expired, or this KID's refresh failed: fail closed.
		return nil, fmt.Errorf("%w: jwks unavailable", errUnknownKID)
	case fetched:
		// A completed fetch confirmed the issuer does not publish this KID.
		v.cacheNegativeKID(kid, nowFunc())
		return nil, errUnknownKID
	default:
		// The refresh was throttled and fetched nothing for this KID, so it is
		// not negatively cached; the next permitted refresh may find it.
		return nil, errUnknownKID
	}
}

func (v *accessVerifier) cached(kid string, now time.Time) cachedKey {
	v.mu.RLock()
	defer v.mu.RUnlock()
	key, known := v.keys[kid]
	negativeUntil, negative := v.negativeKIDs[kid]
	return cachedKey{
		key:       key,
		known:     known,
		age:       now.Sub(v.fetchedAt),
		negative:  negative && now.Before(negativeUntil),
		throttled: now.Sub(v.lastAttempt) < jwksRefreshInterval,
	}
}

// refresh reports whether a fetch completed successfully for this call or the
// singleflight call it joined. A throttled call fetches nothing and returns the
// previous attempt's error, so a failure is not masked during the interval.
func (v *accessVerifier) refresh() (bool, error) {
	result, err, _ := v.refreshGroup.Do(jwksRefreshKey, v.refreshResult)
	fetched, _ := result.(bool)
	return fetched, err
}

func (v *accessVerifier) refreshResult() (any, error) {
	return v.refreshOnce()
}

func (v *accessVerifier) refreshOnce() (bool, error) {
	now := nowFunc()
	v.mu.Lock()
	if now.Sub(v.lastAttempt) < jwksRefreshInterval {
		err := v.lastRefreshErr
		v.mu.Unlock()
		return false, err
	}
	v.lastAttempt = now
	v.mu.Unlock()

	next, err := v.fetchKeys()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastRefreshErr = err
	if err != nil {
		return false, err
	}
	v.keys = next
	v.fetchedAt = nowFunc()
	return true, nil
}

func (v *accessVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody+1))
	if err != nil {
		return nil, fmt.Errorf("read jwks: %w", err)
	}
	if len(body) > maxJWKSBody {
		return nil, errors.New("jwks response too large")
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("jwks has trailing data")
	}

	next := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, jwk := range doc.Keys {
		pub, ok := rsaPublicKeyFromJWK(jwk.Kty, jwk.Alg, jwk.N, jwk.E)
		if ok {
			next[jwk.Kid] = pub
		}
	}
	if len(next) == 0 {
		return nil, errors.New("jwks empty")
	}
	return next, nil
}

func (v *accessVerifier) cacheNegativeKID(kid string, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for cachedKID, until := range v.negativeKIDs {
		if !now.Before(until) {
			delete(v.negativeKIDs, cachedKID)
		}
	}
	if len(v.negativeKIDs) >= maxNegativeKIDs {
		for cachedKID := range v.negativeKIDs {
			delete(v.negativeKIDs, cachedKID)
			break
		}
	}
	v.negativeKIDs[kid] = now.Add(negativeKIDTTL)
}

func rsaPublicKeyFromJWK(kty, alg, n, e string) (*rsa.PublicKey, bool) {
	if kty != "RSA" || (alg != "" && alg != "RS256") {
		return nil, false
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, false
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, false
	}
	exponent := int(new(big.Int).SetBytes(eBytes).Int64())
	modulus := new(big.Int).SetBytes(nBytes)
	if exponent <= 1 || modulus.Sign() <= 0 {
		return nil, false
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, true
}

func (v *accessVerifier) validateClaims(claims *accessClaims) error {
	now := nowFunc()
	if claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(clockSkew)) {
		return errExpired
	}
	if claims.Nbf != 0 && now.Before(time.Unix(claims.Nbf, 0).Add(-clockSkew)) {
		return errExpired
	}
	if claims.Iss != v.issuer {
		return errBadIssuer
	}
	for _, aud := range claims.Aud {
		if subtle.ConstantTimeCompare([]byte(aud), []byte(v.aud)) == 1 {
			return nil
		}
	}
	return errAudMismatch
}
