package mfa

// VULN-027 regression: a WebAuthn credential must not be deletable on the
// password alone. Ported from the scan exploit test and inverted: the
// password-only request is now refused and nothing is deleted.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const (
	v027Password = "victim-password-fixture" // pragma: allowlist secret
	v027CredID   = "11111111-2222-3333-4444-555555555555"
)

func v027Handler(t *testing.T) (*Handler, *mfaEventDB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hash, err := auth.HashPasswordWithParams(v027Password, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	require.NoError(t, err)
	keyring, err := ParseKeyring(strings.Repeat("01", 32), 1, "")
	require.NoError(t, err)
	st := &mfaEventDB{passwordHash: hash, totpExists: true, totpEnabled: true, totpConfirmed: true}
	db := sql.OpenDB(mfaEventConnector{state: st})
	t.Cleanup(func() { _ = db.Close() })
	return NewHandler(db, rdb, logger.New("test"), keyring, "test", nil, "test"), st
}

func v027Do(st *mfaEventDB, handler func(*gin.Context), method, path, body string, params gin.Params) *httptest.ResponseRecorder {
	st.guard = dbtest.FakeFamilyGuard{} // transaction-local marker, as in Postgres
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = params
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", fixtureUserID)
	attachFamily(c, fixtureUserID)
	handler(c)
	return w
}

func v027Statements(st *mfaEventDB) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.statements...)
}

func TestVuln027Regression_WebAuthnDeleteRefusedOnPasswordOnly(t *testing.T) {
	h, st := v027Handler(t)
	w := v027Do(st, h.WebAuthnDeleteCredential, http.MethodDelete, "/api/v1/auth/mfa/webauthn/credentials/"+v027CredID,
		`{"password":"`+v027Password+`"}`, gin.Params{{Key: "id", Value: v027CredID}})

	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	for _, s := range v027Statements(st) {
		q := strings.Join(strings.Fields(s), " ")
		require.NotContains(t, q, "DELETE FROM user_mfa_webauthn", "credential must not be deleted without MFA proof")
		require.NotContains(t, q, "UPDATE users SET mfa_", "MFA flags must not be rewritten")
	}
}

// Sibling TOTPDisable with the same password and no code stays refused.
func TestVuln027Regression_SiblingTOTPDisableDemandsCode(t *testing.T) {
	h, st := v027Handler(t)
	w := v027Do(st, h.TOTPDisable, http.MethodPost, "/api/v1/auth/mfa/totp/disable",
		`{"password":"`+v027Password+`"}`, nil)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
}

// The password gate stays live: a wrong password is refused.
func TestVuln027Regression_WrongPasswordRefused(t *testing.T) {
	h, st := v027Handler(t)
	w := v027Do(st, h.WebAuthnDeleteCredential, http.MethodDelete, "/api/v1/auth/mfa/webauthn/credentials/"+v027CredID,
		`{"password":"wrong"}`, gin.Params{{Key: "id", Value: v027CredID}})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
