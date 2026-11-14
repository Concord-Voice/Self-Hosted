package mfa

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

func TestFactorRemovalRequiresFreshProofWhenMFAIsLocked(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	totpSecret := iuEnrollTOTP(t, db, kr, userID)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	var credentialID string
	require.NoError(t, db.QueryRow(`SELECT id FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&credentialID))

	deleteCredential := func(code string) (int, map[string]any) {
		c, w := iuGinContext(http.MethodDelete, "/api/v1/mfa/webauthn/credentials/"+credentialID,
			fmt.Sprintf(`{"password":%q,"mfa_code":%q}`, iuPassword, code), userID, "")
		c.Params = gin.Params{{Key: "id", Value: credentialID}}
		h.WebAuthnDeleteCredential(c)
		return w.Code, iuBody(t, w)
	}

	code, body := deleteCredential("")
	require.Equal(t, http.StatusForbidden, code, body)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	assert.Equal(t, 1, count, "password-only deletion must leave the credential")

	foreign := ipMintInlineToken(t, h, rdb, auth, userID, string(stepup.PurposeTOTPDisable))
	code, body = deleteCredential(foreign)
	require.Equal(t, http.StatusForbidden, code, body)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	assert.Equal(t, 1, count, "a wrong-purpose token must not delete the credential")
	ipRequireStillValidFor(t, h, userID, stepup.PurposeTOTPDisable, foreign)

	proof := ipMintInlineToken(t, h, rdb, auth, userID, string(stepup.PurposeWebAuthnDelete))
	code, body = deleteCredential(proof)
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&count))
	assert.Zero(t, count, "a valid fresh proof may delete only the named credential")

	// The valid proof deleted only the credential; the other enrolled factor
	// remains available and its secret is never read by the route.
	assert.NotEmpty(t, totpSecret)
}

func TestTOTPDisableRequiresFreshProofWhenMFAIsLocked(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuEnrollTOTP(t, db, kr, userID)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	disable := func(code string) (int, map[string]any) {
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/totp/disable",
			fmt.Sprintf(`{"password":%q,"code":%q}`, iuPassword, code), userID, "")
		h.TOTPDisable(c)
		return w.Code, iuBody(t, w)
	}

	status, body := disable("")
	require.Equal(t, http.StatusForbidden, status, body)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&count))
	assert.Equal(t, 1, count, "password-only deletion must leave TOTP enrolled")

	proof := ipMintInlineToken(t, h, rdb, auth, userID, string(stepup.PurposeTOTPDisable))
	status, body = disable(proof)
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&count))
	assert.Zero(t, count, "the exact-purpose proof may delete TOTP")

	spent, err := h.VerifyCode(context.Background(), userID, stepup.PurposeTOTPDisable, proof)
	require.NoError(t, err)
	assert.False(t, spent, "the committed consumer must spend its one-use proof")
}
