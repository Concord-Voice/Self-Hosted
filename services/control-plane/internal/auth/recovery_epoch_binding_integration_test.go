package auth_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Regression suite for VULN-018 and VULN-203: recovery authority is bound to
// the credential epoch it was issued against, on every consumer.

const deviceUnauthorizedBody = `{"error":"Invalid or expired recovery token"}`

func recoveryResetPasswordBody(token string) map[string]interface{} {
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	return map[string]interface{}{
		"recovery_token": token, "new_password": testNewPassword,
		"wrapped_private_key": wrappedKey, "key_derivation_salt": salt, "key_derivation_alg": "argon2id",
	}
}

func recoveryResetAccountBody(token string) map[string]interface{} {
	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	return map[string]interface{}{
		"recovery_token": token, "new_password": testNewPassword,
		"wrapped_private_key": wrappedKey, "key_derivation_salt": salt, "key_derivation_alg": "argon2id",
		"public_key": publicKey, "acknowledge_data_loss": true,
	}
}

func recoveryTokenJTI(t *testing.T, token string) string {
	t.Helper()
	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeRecovery)
	require.NoError(t, err)
	return claims.ID
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

// createDeviceRequest opens a pending device-recovery request with token.
func createDeviceRequest(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, token string) string {
	t.Helper()
	w := ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created struct {
		RequestID string `json:"request_id"`
	}
	testhelpers.ParseJSON(t, w, &created)
	require.NotEmpty(t, created.RequestID)
	return created.RequestID
}

// resetDeviceCreateLimit clears the create route's 3-per-15-minute IP budget so
// a refusal under test is the handler's, never the limiter's 429.
func resetDeviceCreateLimit(t *testing.T, ts *testhelpers.TestServer) {
	t.Helper()
	require.NoError(t, ts.Redis.Del(context.Background(), "ratelimit:ip:192.0.2.1:POST:"+pathDeviceReqCreate).Err())
}

func designateRecoveryDevice(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'device',$2)`, userID, uuid.NewString())
	require.NoError(t, err)
}

func storedPasswordHash(t *testing.T, ts *testhelpers.TestServer, userID string) string {
	t.Helper()
	var hash string
	require.NoError(t, ts.DB.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash))
	return hash
}

func storedCredentialEpoch(t *testing.T, ts *testhelpers.TestServer, userID string) sql.NullString {
	t.Helper()
	var epoch sql.NullString
	require.NoError(t, ts.DB.QueryRow(`SELECT credential_epoch FROM users WHERE id = $1`, userID).Scan(&epoch))
	return epoch
}

// requireRecoveryTokenRefusedEverywhere drives the five recovery-token routes
// with token and asserts each refuses it as unauthorized without spending it
// or changing the account's password. Those outcomes hold under either layer:
// the reset path also releases its marker when the locked validator refuses.
// The unlocked pre-check that refuses before any side effect is pinned by
// TestValidateRecoveryTokenPrechecksEpoch and the VULN-018 test in internal/mfa.
func requireRecoveryTokenRefusedEverywhere(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, token, requestID string) {
	t.Helper()
	jti := recoveryTokenJTI(t, token)
	hash := storedPasswordHash(t, ts, user.ID)
	for name, body := range map[string]map[string]interface{}{
		pathRecoveryResetPwd:  recoveryResetPasswordBody(token),
		pathRecoveryResetAcct: recoveryResetAccountBody(token),
	} {
		w := ts.DoRequest(http.MethodPost, name, body, nil)
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s: %s", name, w.Body.String())
		used, err := ts.Redis.Exists(context.Background(), "recovery_token_used:"+jti).Result()
		require.NoError(t, err)
		require.Zero(t, used, "%s: a refused stale token must not reserve the spent marker", name)
		require.Equal(t, hash, storedPasswordHash(t, ts, user.ID), "%s: a refused token must not change the password", name)
	}
	var before int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM recovery_requests WHERE user_id = $1`, user.ID).Scan(&before))
	resetDeviceCreateLimit(t, ts)
	w := ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil)
	require.Equal(t, http.StatusUnauthorized, w.Code, "device create: %s", w.Body.String())
	require.JSONEq(t, deviceUnauthorizedBody, w.Body.String())
	var after int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM recovery_requests WHERE user_id = $1`, user.ID).Scan(&after))
	require.Equal(t, before, after, "device create must not insert a request for a stale token")
	w = ts.DoRequest(http.MethodGet, pathDeviceReqPoll+requestID, nil, bearer(token))
	require.Equal(t, http.StatusUnauthorized, w.Code, "device poll: %s", w.Body.String())
	require.JSONEq(t, deviceUnauthorizedBody, w.Body.String())
	w = ts.DoRequest(http.MethodPost, pathDeviceReqPoll+requestID+"/complete", devicerecovery.CompleteBody{ProtocolVersion: 2, TranscriptHash: devicerecovery.Encode(make([]byte, 32))}, bearer(token))
	require.Equal(t, http.StatusUnauthorized, w.Code, "device complete: %s", w.Body.String())
	require.JSONEq(t, deviceUnauthorizedBody, w.Body.String())
}

func TestRecoveryTokenRejectedAfterCredentialRotation(t *testing.T) {
	rotations := map[string]func(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser){
		"password change": func(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
			_, wrappedKey, salt := testhelpers.E2EETestKeys()
			w := ts.DoRequest(http.MethodPost, pathMePassword, map[string]interface{}{
				"current_password": user.Password, "new_password": credEpochNewPass,
				"wrapped_private_key": wrappedKey, "key_derivation_salt": salt, "key_derivation_alg": "argon2id",
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		},
		"completed recovery with a sibling token": func(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
			sibling := recoveryTokenFor(t, ts, user)
			w := ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, recoveryResetPasswordBody(sibling), nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		},
		"credential fence rotation": func(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
			ts.RotateCredentialEpoch(t, user.ID)
		},
	}
	for name, rotate := range rotations {
		t.Run(name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "epochbound")
			designateRecoveryDevice(t, ts, user.ID)
			ceremony := recoveryTokenFor(t, ts, user)
			requestID := createDeviceRequest(t, ts, user, ceremony)
			stale := recoveryTokenFor(t, ts, user)
			before := storedCredentialEpoch(t, ts, user.ID)
			rotate(t, ts, user)
			require.NotEqual(t, before, storedCredentialEpoch(t, ts, user.ID), "the rotation must change the durable epoch")
			requireRecoveryTokenRefusedEverywhere(t, ts, user, stale, requestID)
			requireRecoveryTokenRefusedEverywhere(t, ts, user, ceremony, requestID)
		})
	}
}

func TestSiblingRecoveryTokenDiesWithCompletedRecovery(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "siblingtoken")
	designateRecoveryDevice(t, ts, user.ID)
	a := recoveryTokenFor(t, ts, user)
	b := recoveryTokenFor(t, ts, user)
	requestID := createDeviceRequest(t, ts, user, b)
	w := ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, recoveryResetPasswordBody(a), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	requireRecoveryTokenRefusedEverywhere(t, ts, user, b, requestID)
}

func TestNeverRotatedAccountRecoveryStillCompletes(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "neverrotated")
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = NULL WHERE id = $1`, user.ID)
	require.NoError(t, err)
	token := recoveryTokenFor(t, ts, user)
	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeRecovery)
	require.NoError(t, err)
	require.Empty(t, claims.CredentialEpoch, "a never-rotated account mints an empty epoch")
	w := ts.DoRequest(http.MethodPost, pathRecoveryResetPwd, recoveryResetPasswordBody(token), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	epoch := storedCredentialEpoch(t, ts, user.ID)
	require.True(t, epoch.Valid && epoch.String != "", "the completed reset rotates the epoch")
}

// signRecoveryClaims signs claims with the test secret, bypassing the mint so a
// test can present shapes the server no longer issues.
func signRecoveryClaims(t *testing.T, claims mfa.ChallengeClaims, issuedAt time.Time, ttl time.Duration) string {
	t.Helper()
	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID: uuid.NewString(), Issuer: mfa.ChallengeTokenIssuer,
		IssuedAt: jwt.NewNumericDate(issuedAt), NotBefore: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(ttl)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testhelpers.TestJWTSecret))
	require.NoError(t, err)
	return signed
}

// requireDeviceAndResetRoutesRefuse asserts token reaches no recovery consumer.
func requireDeviceAndResetRoutesRefuse(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, token, requestID string) {
	t.Helper()
	resetDeviceCreateLimit(t, ts)
	for _, c := range []struct {
		method, path string
		body         interface{}
		headers      http.Header
	}{
		{http.MethodPost, pathRecoveryResetPwd, recoveryResetPasswordBody(token), nil},
		{http.MethodPost, pathRecoveryResetAcct, recoveryResetAccountBody(token), nil},
		{http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil},
		{http.MethodGet, pathDeviceReqPoll + requestID, nil, bearer(token)},
		{http.MethodPost, pathDeviceReqPoll + requestID + "/complete", devicerecovery.CompleteBody{ProtocolVersion: 2, TranscriptHash: devicerecovery.Encode(make([]byte, 32))}, bearer(token)},
	} {
		w := ts.DoRequest(c.method, c.path, c.body, c.headers)
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s %s: %s", c.method, c.path, w.Body.String())
	}
}

func TestLegacyAndExpiredRecoveryTokensReachNoConsumer(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "legacyrecovery")
	designateRecoveryDevice(t, ts, user.ID)
	requestID := createDeviceRequest(t, ts, user, recoveryTokenFor(t, ts, user))
	epoch := storedCredentialEpoch(t, ts, user.ID).String
	t.Run("recovery_v2", func(t *testing.T) {
		legacy := signRecoveryClaims(t, mfa.ChallengeClaims{UserID: user.ID, Purpose: "recovery_v2", RecoveryFactor: "email", CredentialEpoch: epoch}, time.Now(), time.Hour)
		requireDeviceAndResetRoutesRefuse(t, ts, user, legacy, requestID)
	})
	t.Run("expired recovery_v3", func(t *testing.T) {
		expired := signRecoveryClaims(t, mfa.ChallengeClaims{UserID: user.ID, Purpose: mfa.PurposeRecovery, RecoveryFactor: "email", CredentialEpoch: epoch}, time.Now().Add(-16*time.Minute), 15*time.Minute)
		requireDeviceAndResetRoutesRefuse(t, ts, user, expired, requestID)
	})
	t.Run("control: current recovery_v3 is accepted", func(t *testing.T) {
		current := signRecoveryClaims(t, mfa.ChallengeClaims{UserID: user.ID, Purpose: mfa.PurposeRecovery, RecoveryFactor: "email", CredentialEpoch: epoch}, time.Now(), 15*time.Minute)
		resetDeviceCreateLimit(t, ts)
		w := ts.DoRequest(http.MethodPost, pathDeviceReqCreate, deviceCreateV2(t, user.ID, current), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
}
