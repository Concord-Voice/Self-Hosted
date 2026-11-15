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

const (
	jwksTTL             = time.Hour
	jwksRefreshInterval = time.Second
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

	mu           sync.RWMutex
	keys         map[string]*rsa.PublicKey
	fetchedAt    time.Time
	lastAttempt  time.Time
	negativeKIDs map[string]time.Time
	refreshGroup singleflight.Group
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

	now := nowFunc()
	v.mu.RLock()
	k, ok := v.keys[kid]
	fresh := now.Sub(v.fetchedAt) < jwksTTL
	negativeUntil, negative := v.negativeKIDs[kid]
	v.mu.RUnlock()
	if ok && fresh {
		return k, nil
	}
	if negative && now.Before(negativeUntil) {
		return nil, errUnknownKID
	}

	if err := v.refresh(); err != nil {
		return nil, fmt.Errorf("%w: jwks unavailable", errUnknownKID)
	}

	v.mu.RLock()
	k, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		v.cacheNegativeKID(kid, nowFunc())
		return nil, errUnknownKID
	}
	return k, nil
}

func (v *accessVerifier) refresh() error {
	_, err, _ := v.refreshGroup.Do("jwks", func() (any, error) {
		return nil, v.refreshOnce()
	})
	return err
}

func (v *accessVerifier) refreshOnce() error {
	now := nowFunc()
	v.mu.Lock()
	if now.Sub(v.lastAttempt) < jwksRefreshInterval {
		v.mu.Unlock()
		return nil
	}
	v.lastAttempt = now
	v.mu.Unlock()

	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody+1))
	if err != nil {
		return fmt.Errorf("read jwks: %w", err)
	}
	if len(body) > maxJWKSBody {
		return errors.New("jwks response too large")
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
		return fmt.Errorf("decode jwks: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("jwks has trailing data")
	}

	next := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, jwk := range doc.Keys {
		pub, ok := rsaPublicKeyFromJWK(jwk.Kty, jwk.Alg, jwk.N, jwk.E)
		if ok {
			next[jwk.Kid] = pub
		}
	}
	if len(next) == 0 {
		return errors.New("jwks empty")
	}
	v.mu.Lock()
	v.keys = next
	v.fetchedAt = nowFunc()
	v.mu.Unlock()
	return nil
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
