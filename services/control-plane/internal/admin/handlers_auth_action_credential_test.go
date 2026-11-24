package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// actionCredentialLoader stands in for the root-owned Host Agent projection.
type actionCredentialLoader func(adminID string) (map[string]struct{}, error)

func actionCredentialEngine(t *testing.T, db *sql.DB, rdb *redis.Client, load actionCredentialLoader) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
	require.NoError(t, err)
	admin.UseServerAgentForTest(h, fixedAnswerAgent(t, http.StatusNotFound, `{"error":"authorizer_not_imported"}`))
	admin.UseActionCredentialLoaderForTest(h, load)
	r := gin.New()
	r.POST("/admin/api/v1/auth/password", h.PasswordLogin)
	r.POST("/admin/api/v1/auth/webauthn", h.WebAuthnLogin)
	return r
}

// adminWithActionKey enrolls an admin whose first key is a login key and adds a
// second registered key that root will mark as an action credential.
func adminWithActionKey(t *testing.T, db *sql.DB, rdb *redis.Client) (adminID, username string, loginKey, actionKey *virtualAuthenticator) {
	t.Helper()
	adminID, username, loginKey = enrolledAdmin(t, db, rdb)
	actionKey = newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	aaguid, err := actionKey.aaguid.MarshalBinary()
	require.NoError(t, err)
	_, err = admin.NewAdminRepo(db).AddCredential(context.Background(), admin.AdminCredential{
		AdminID: adminID, CredentialID: actionKey.credID, PublicKey: actionKey.coseKey(t),
		AAGUID: aaguid, SignCount: int64(actionKey.signCount), CredentialName: "action",
	})
	require.NoError(t, err)
	return adminID, username, loginKey, actionKey
}

type loginOptions struct {
	Handle    string                        `json:"handle"`
	PublicKey *protocol.CredentialAssertion `json:"publicKey"`
}

func beginAdminLogin(t *testing.T, engine *gin.Engine, username string) (int, loginOptions, string) {
	t.Helper()
	rec := postJSON(engine, "/admin/api/v1/auth/password", map[string]string{"username": username, "password": adminTestPassword})
	var resp loginOptions
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp, rec.Body.String()
}

func allowedIDs(opts loginOptions) []string {
	ids := make([]string, 0, len(opts.PublicKey.Response.AllowedCredentials))
	for _, c := range opts.PublicKey.Response.AllowedCredentials {
		ids = append(ids, base64.RawURLEncoding.EncodeToString(c.CredentialID))
	}
	return ids
}

func idOf(va *virtualAuthenticator) string { return base64.RawURLEncoding.EncodeToString(va.credID) }

func adminSessionKeys(t *testing.T, rdb *redis.Client) []string {
	t.Helper()
	keys, err := rdb.Keys(context.Background(), "admin_session:*").Result()
	require.NoError(t, err)
	return keys
}

func setupActionCredentialStores(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
	return db, rdb
}

func TestPasswordLogin_ExcludesActionCredentialsFromAllowList(t *testing.T) {
	db, rdb := setupActionCredentialStores(t)
	adminID, username, loginKey, actionKey := adminWithActionKey(t, db, rdb)
	var askedFor atomic.Value
	engine := actionCredentialEngine(t, db, rdb, func(id string) (map[string]struct{}, error) {
		askedFor.Store(id)
		return map[string]struct{}{idOf(actionKey): {}}, nil
	})

	code, opts, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{idOf(loginKey)}, allowedIDs(opts), "only the login key may be offered")
	assert.Equal(t, adminID, askedFor.Load(), "the projection is read for the admin signing in")
}

func TestPasswordLogin_ActionProjectionUnreadableFailsClosed(t *testing.T) {
	db, rdb := setupActionCredentialStores(t)
	_, username, _, _ := adminWithActionKey(t, db, rdb)
	engine := actionCredentialEngine(t, db, rdb, func(string) (map[string]struct{}, error) {
		return nil, fs.ErrPermission
	})

	code, _, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusServiceUnavailable, code, body)
	assert.NotContains(t, body, "handle", "no login options without a readable projection")
	assert.NotContains(t, body, "publicKey")
}

func TestPasswordLogin_ActionProjectionAbsentOffersEveryKey(t *testing.T) {
	db, rdb := setupActionCredentialStores(t)
	_, username, loginKey, actionKey := adminWithActionKey(t, db, rdb)
	engine := actionCredentialEngine(t, db, rdb, func(string) (map[string]struct{}, error) {
		return nil, os.ErrNotExist
	})

	code, opts, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusOK, code, body)
	assert.ElementsMatch(t, []string{idOf(loginKey), idOf(actionKey)}, allowedIDs(opts))
}

func TestWebAuthnLogin_ActionCredentialAssertionIsForbidden(t *testing.T) {
	for _, tc := range []struct {
		name string
		// atBegin and atFinish are the action IDs the projection reports at
		// each step; the second case models root changing a key's purpose
		// while the ceremony is open.
		atBegin, atFinish func(action *virtualAuthenticator) map[string]struct{}
	}{
		{
			name:     "browser ignores allowCredentials",
			atBegin:  func(a *virtualAuthenticator) map[string]struct{} { return map[string]struct{}{idOf(a): {}} },
			atFinish: func(a *virtualAuthenticator) map[string]struct{} { return map[string]struct{}{idOf(a): {}} },
		},
		{
			name:     "purpose changed after begin",
			atBegin:  func(*virtualAuthenticator) map[string]struct{} { return map[string]struct{}{} },
			atFinish: func(a *virtualAuthenticator) map[string]struct{} { return map[string]struct{}{idOf(a): {}} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, rdb := setupActionCredentialStores(t)
			_, username, _, actionKey := adminWithActionKey(t, db, rdb)
			var finishing atomic.Bool
			engine := actionCredentialEngine(t, db, rdb, func(string) (map[string]struct{}, error) {
				if finishing.Load() {
					return tc.atFinish(actionKey), nil
				}
				return tc.atBegin(actionKey), nil
			})

			code, opts, body := beginAdminLogin(t, engine, username)
			require.Equal(t, http.StatusOK, code, body)
			finishing.Store(true)
			actionKey.signCount++
			assertion := actionKey.assertionResponse(t, testAdminRPID, opts.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

			rec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": opts.Handle, "assertion": json.RawMessage(assertion)})
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.JSONEq(t, `{"error":"action_credential_not_for_login"}`, rec.Body.String())
			assert.Empty(t, rec.Header().Get("Set-Cookie"), "no session cookie")
			assert.Empty(t, adminSessionKeys(t, rdb), "no session key")
		})
	}
}

func TestWebAuthnLogin_LoginKeyStillSignsIn(t *testing.T) {
	db, rdb := setupActionCredentialStores(t)
	_, username, loginKey, actionKey := adminWithActionKey(t, db, rdb)
	engine := actionCredentialEngine(t, db, rdb, func(string) (map[string]struct{}, error) {
		return map[string]struct{}{idOf(actionKey): {}}, nil
	})

	code, opts, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusOK, code, body)
	loginKey.signCount++
	assertion := loginKey.assertionResponse(t, testAdminRPID, opts.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	rec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": opts.Handle, "assertion": json.RawMessage(assertion)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Set-Cookie"))
	assert.Len(t, adminSessionKeys(t, rdb), 1)
}

func TestWebAuthnLogin_ActionProjectionUnreadableAtFinishFailsClosed(t *testing.T) {
	db, rdb := setupActionCredentialStores(t)
	_, username, loginKey, _ := adminWithActionKey(t, db, rdb)
	var finishing atomic.Bool
	engine := actionCredentialEngine(t, db, rdb, func(string) (map[string]struct{}, error) {
		if finishing.Load() {
			return nil, fs.ErrPermission
		}
		return map[string]struct{}{}, nil
	})

	code, opts, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusOK, code, body)
	finishing.Store(true)
	loginKey.signCount++
	assertion := loginKey.assertionResponse(t, testAdminRPID, opts.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

	rec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": opts.Handle, "assertion": json.RawMessage(assertion)})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Set-Cookie"))
	assert.Empty(t, adminSessionKeys(t, rdb))
}
