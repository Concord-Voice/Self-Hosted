package mfa_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/lib/pq"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
)

// VULN-028 end to end: a repeated method is stored and coded once.

func currentTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

func TestEmailSmsSetupStoresOneCodeForARepeatedMethod(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "smsrepeated")
	secret, _ := enrollTOTP(t, ts, user)

	w := ts.DoRequest("POST", urlEmailSmsSetup, map[string]interface{}{
		"password": testPassword,
		"mfa_code": currentTOTPCode(t, secret),
		"methods":  []string{"sms", "sms"},
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Methods []string `json:"methods"`
	}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, []string{"sms"}, body.Methods)
	keys, err := ts.Redis.Keys(context.Background(), fmt.Sprintf("mfa_emailsms_setup:%s:*", user.ID)).Result()
	require.NoError(t, err)
	require.Len(t, keys, 1, "one method gets one stored code")
}

func TestSetRecoveryOnlyStoresARepeatedMethodOnceInTheRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "recovrepeated")
	secret, _ := enrollTOTP(t, ts, user)
	_, err := ts.DB.Exec(`UPDATE users SET mfa_methods = '{totp,email}' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("PUT", urlRecoveryOnly, map[string]interface{}{
		"password": testPassword,
		"mfa_code": currentTOTPCode(t, secret),
		"methods":  []string{"email", "email", "email"},
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var stored []string
	require.NoError(t, ts.DB.QueryRow(`SELECT recovery_only_methods FROM users WHERE id = $1`, user.ID).Scan(pq.Array(&stored)))
	require.Equal(t, []string{"email"}, stored)
}
