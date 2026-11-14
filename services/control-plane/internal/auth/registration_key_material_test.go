package auth_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestRegistrationKeyMaterialLimit(t *testing.T) {
	wrappedExact := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("w", auth.MaxWrappedPrivateKeyDecoded)))
	saltExact := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", auth.MaxKeyDerivationSaltDecoded)))
	require.Len(t, wrappedExact, auth.MaxWrappedPrivateKeyEncoded)
	require.Len(t, saltExact, auth.MaxKeyDerivationSaltEncoded)
	wrapped, salt, err := auth.DecodeRegistrationPrivateMaterial(wrappedExact, saltExact)
	require.NoError(t, err)
	require.Len(t, wrapped, auth.MaxWrappedPrivateKeyDecoded)
	require.Len(t, salt, auth.MaxKeyDerivationSaltDecoded)

	for _, test := range []struct {
		name    string
		wrapped string
		salt    string
		want    error
	}{
		{"wrapped encoded max plus one", wrappedExact + "A", saltExact, auth.ErrWrappedPrivateKeyInvalid},
		{"wrapped decoded max plus one", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("w", auth.MaxWrappedPrivateKeyDecoded+1))), saltExact, auth.ErrWrappedPrivateKeyInvalid},
		{"salt encoded max plus one", wrappedExact, saltExact + "A", auth.ErrKeyDerivationSaltInvalid},
		{"salt decoded max plus one", wrappedExact, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", auth.MaxKeyDerivationSaltDecoded+1))), auth.ErrKeyDerivationSaltInvalid},
		{"malformed wrapped", "not-base64", saltExact, auth.ErrWrappedPrivateKeyInvalid},
		{"malformed salt", wrappedExact, "not-base64", auth.ErrKeyDerivationSaltInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotWrapped, gotSalt, err := auth.DecodeRegistrationPrivateMaterial(test.wrapped, test.salt)
			require.ErrorIs(t, err, test.want)
			require.Empty(t, gotWrapped)
			require.Empty(t, gotSalt)
			require.NotContains(t, err.Error(), strings.Repeat("w", 64))
		})
	}
}
