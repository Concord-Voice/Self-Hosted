package admin_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// adminWithBackupKey enrolls an admin and registers a second (backup) key.
// Both keys log in and, once imported by root, authorize host actions.
func adminWithBackupKey(t *testing.T, db *sql.DB, rdb *redis.Client) (adminID, username string, primary, backup *virtualAuthenticator) {
	t.Helper()
	adminID, username, primary = enrolledAdmin(t, db, rdb)
	backup = newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	aaguid, err := backup.aaguid.MarshalBinary()
	require.NoError(t, err)
	_, err = admin.NewAdminRepo(db).AddCredential(context.Background(), admin.AdminCredential{
		AdminID: adminID, CredentialID: backup.credID, PublicKey: backup.coseKey(t),
		AAGUID: aaguid, SignCount: int64(backup.signCount), CredentialName: "backup",
	})
	require.NoError(t, err)
	return adminID, username, primary, backup
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

func setupProjectionStores(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	t.Setenv("HOST_AGENT_AUTHORIZER_PROJECTION", "1")
	return db, rdb
}

// With the projection enabled every key of the admin is offered at login:
// there is no login-only or action-only credential any more.
func TestPasswordLogin_OffersEveryKeyWithProjectionEnabled(t *testing.T) {
	db, rdb := setupProjectionStores(t)
	sock, _ := importedAssertionAgent(t, http.StatusOK)
	_, username, primary, backup := adminWithBackupKey(t, db, rdb)
	engine := adminAuthEngineWithAgent(t, db, rdb, sock)

	code, opts, body := beginAdminLogin(t, engine, username)
	require.Equal(t, http.StatusOK, code, body)
	assert.ElementsMatch(t, []string{idOf(primary), idOf(backup)}, allowedIDs(opts))
}

// Each key, primary or backup, logs in through the agent's root counter relay:
// the same counter a host action advances.
func TestWebAuthnLogin_EveryKeyLogsInThroughRootCounter(t *testing.T) {
	for _, pick := range []string{"primary", "backup"} {
		t.Run(pick, func(t *testing.T) {
			db, rdb := setupProjectionStores(t)
			sock, calls := importedAssertionAgent(t, http.StatusOK)
			adminID, username, primary, backup := adminWithBackupKey(t, db, rdb)
			key := primary
			if pick == "backup" {
				key = backup
			}
			engine := adminAuthEngineWithAgent(t, db, rdb, sock)

			code, opts, body := beginAdminLogin(t, engine, username)
			require.Equal(t, http.StatusOK, code, body)
			key.signCount++
			assertion := key.assertionResponse(t, testAdminRPID, opts.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV, false)

			rec := postJSON(engine, "/admin/api/v1/auth/webauthn", map[string]any{"handle": opts.Handle, "assertion": json.RawMessage(assertion)})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotEmpty(t, rec.Header().Get("Set-Cookie"))
			got := calls()
			require.Len(t, got, 1, "the root counter is advanced before the session is minted")
			assert.Equal(t, adminID, got[0].AdminID)
			parsed, err := protocol.ParseCredentialRequestResponseBytes(got[0].Assertion)
			require.NoError(t, err)
			assert.Equal(t, key.credID, parsed.RawID, "the agent verifies the key that signed")
		})
	}
}
