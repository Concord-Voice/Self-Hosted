package auth

import (
	"encoding/base64"
	"errors"
)

const (
	// MaxWrappedPrivateKeyEncoded caps the base64 transport envelope.
	MaxWrappedPrivateKeyEncoded = 16384
	// MaxWrappedPrivateKeyDecoded caps the opaque encrypted private key.
	MaxWrappedPrivateKeyDecoded = 12288
	// MaxKeyDerivationSaltEncoded caps the base64 transport salt.
	MaxKeyDerivationSaltEncoded = 1024
	// MaxKeyDerivationSaltDecoded caps the decoded KDF salt.
	MaxKeyDerivationSaltDecoded = 768
)

var (
	// ErrWrappedPrivateKeyInvalid covers malformed or oversized envelopes.
	ErrWrappedPrivateKeyInvalid = errors.New("invalid wrapped private key")
	// ErrKeyDerivationSaltInvalid covers malformed or oversized salts.
	ErrKeyDerivationSaltInvalid = errors.New("invalid key derivation salt")
)

// DecodeRegistrationPrivateMaterial validates and decodes the opaque private
// envelope and KDF salt shared by password and SSO registration. Bounds are
// checked before and after decoding so oversized material never reaches DB
// writes, hashing, or response/log paths.
func DecodeRegistrationPrivateMaterial(wrappedB64, saltB64 string) (wrapped, salt []byte, err error) {
	if len(wrappedB64) > MaxWrappedPrivateKeyEncoded {
		return nil, nil, ErrWrappedPrivateKeyInvalid
	}
	wrapped, err = base64.StdEncoding.DecodeString(wrappedB64)
	if err != nil || len(wrapped) > MaxWrappedPrivateKeyDecoded {
		return nil, nil, ErrWrappedPrivateKeyInvalid
	}
	if len(saltB64) > MaxKeyDerivationSaltEncoded {
		return nil, nil, ErrKeyDerivationSaltInvalid
	}
	salt, err = base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) > MaxKeyDerivationSaltDecoded {
		return nil, nil, ErrKeyDerivationSaltInvalid
	}
	return wrapped, salt, nil
}
