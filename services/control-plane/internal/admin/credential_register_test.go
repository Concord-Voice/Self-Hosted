package admin_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

const (
	credentialsPath         = "/admin/api/v1/credentials"                 //nolint:gosec // G101 false positive: URL path, not a credential
	credentialStepUpPath    = "/admin/api/v1/credentials/stepup"          //nolint:gosec // G101 false positive: URL path, not a credential
	credentialRegBeginPath  = "/admin/api/v1/credentials/register/begin"  //nolint:gosec // G101 false positive: URL path, not a credential
	credentialRegFinishPath = "/admin/api/v1/credentials/register/finish" //nolint:gosec // G101 false positive: URL path, not a credential
	credentialTestIP        = "192.0.2.10"
)

func credentialStores(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)
	return db, rdb
}

func getAsSession(engine http.Handler, sid, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = credentialTestIP + ":4242"
	req.AddCookie(&http.Cookie{Name: "__Host-cv_admin_sid", Value: sid, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// credentialStepUp begins an add_credential step-up in the session and signs
// it with va.
func credentialStepUp(t *testing.T, engine *gin.Engine, sid string, va *virtualAuthenticator) map[string]any {
	t.Helper()
	rec := postAsSession(engine, sid, credentialTestIP, credentialStepUpPath, map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return map[string]any{"handle": resp.Handle, "assertion": signStepUp(t, va, resp.PublicKey.Response.Challenge.String(), flagUP|flagUV)}
}

type registerBegin struct {
	Handle    string                       `json:"handle"`
	PublicKey *protocol.CredentialCreation `json:"publicKey"`
}

func beginCredentialRegister(t *testing.T, engine *gin.Engine, sid string, stepUp map[string]any) registerBegin {
	t.Helper()
	rec := postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": stepUp})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp registerBegin
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.PublicKey)
	return resp
}

func finishCredentialRegister(engine *gin.Engine, sid string, begin registerBegin, va *virtualAuthenticator, t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	attestation := va.attestationResponse(t, testAdminRPID, begin.PublicKey.Response.Challenge.String(), testAdminOrigin, flagUP|flagUV)
	return postAsSession(engine, sid, credentialTestIP, credentialRegFinishPath, map[string]any{
		"handle": begin.Handle, "attestation": json.RawMessage(attestation), "credential_name": "backup",
	})
}

func credentialAddedRows(t *testing.T, db *sql.DB, adminID, result string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM admin_audit_log WHERE event_type = $1 AND admin_id = $2 AND result = $3`,
		admin.EventCredentialAdded, adminID, result,
	).Scan(&n))
	return n
}

func ownCredentialCount(t *testing.T, db *sql.DB, adminID string) int {
	t.Helper()
	n, err := admin.NewAdminRepo(db).CountActiveCredentials(t.Context(), adminID)
	require.NoError(t, err)
	return n
}

// A fresh step-up with the existing key lets the admin add a backup. The
// ceremony excludes the existing key, the backup is stored, audited and then
// itself passes a step-up.
func TestAddCredential_FreshStepUpRegistersBackup(t *testing.T) {
	db, rdb := credentialStores(t)
	adminID, username, primary := enrolledAdmin(t, db, rdb)
	engine := routedCreateEngine(t, db, rdb)
	sid := mintAdminSession(t, db, rdb, username)

	begin := beginCredentialRegister(t, engine, sid, credentialStepUp(t, engine, sid, primary))
	excluded := make([]string, 0, len(begin.PublicKey.Response.CredentialExcludeList))
	for _, d := range begin.PublicKey.Response.CredentialExcludeList {
		excluded = append(excluded, base64.RawURLEncoding.EncodeToString(d.CredentialID))
	}
	assert.Equal(t, []string{idOf(primary)}, excluded, "the browser must refuse to overwrite an existing key")
	assert.Equal(t, protocol.ResidentKeyRequirementRequired, begin.PublicKey.Response.AuthenticatorSelection.ResidentKey)

	backup := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	rec := finishCredentialRegister(engine, sid, begin, backup, t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, ownCredentialCount(t, db, adminID))
	assert.Equal(t, 1, credentialAddedRows(t, db, adminID, admin.AuditSuccess))

	// The backup key is a full key: it can authorize the next step-up.
	begin = beginCredentialRegister(t, engine, sid, credentialStepUp(t, engine, sid, backup))
	assert.Len(t, begin.PublicKey.Response.CredentialExcludeList, 2)
}

// Each way of reaching register/begin without a fresh add_credential step-up
// of this admin and session is refused and registers nothing.
func TestAddCredential_RequiresFreshStepUp(t *testing.T) {
	db, rdb := credentialStores(t)
	adminID, username, primary := enrolledAdmin(t, db, rdb)
	engine := routedCreateEngine(t, db, rdb)
	sid := mintAdminSession(t, db, rdb, username)
	other := mintAdminSession(t, db, rdb, username)

	rec := postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"stepup_required"}`, rec.Body.String())

	// Begun in another session of the same admin.
	foreign := credentialStepUp(t, engine, other, primary)
	rec = postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": foreign})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A create_admin step-up cannot be redeemed for add_credential.
	createHandle, createChallenge := sessionStepUpBegin(t, engine, sid)
	rec = postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": map[string]any{
		"handle": createHandle, "assertion": signStepUp(t, primary, createChallenge, flagUP|flagUV),
	}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A step-up without user verification.
	rec = postAsSession(engine, sid, credentialTestIP, credentialStepUpPath, map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code)
	var noUV struct {
		Handle    string                        `json:"handle"`
		PublicKey *protocol.CredentialAssertion `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &noUV))
	rec = postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": map[string]any{
		"handle": noUV.Handle, "assertion": signStepUp(t, primary, noUV.PublicKey.Response.Challenge.String(), flagUP),
	}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A redeemed step-up cannot be redeemed again.
	fresh := credentialStepUp(t, engine, sid, primary)
	beginCredentialRegister(t, engine, sid, fresh)
	rec = postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": fresh})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	assert.Equal(t, 1, ownCredentialCount(t, db, adminID))
	assert.Equal(t, 0, credentialAddedRows(t, db, adminID, admin.AuditSuccess))
	assert.GreaterOrEqual(t, credentialAddedRows(t, db, adminID, admin.AuditDenied), 5)
}

// The registration handle is redeemed once, only by the session that began
// it, and only while the admin's credential generation is unchanged.
func TestAddCredential_FinishBoundToSessionAndGeneration(t *testing.T) {
	db, rdb := credentialStores(t)
	adminID, username, primary := enrolledAdmin(t, db, rdb)
	engine := routedCreateEngine(t, db, rdb)
	sid := mintAdminSession(t, db, rdb, username)
	other := mintAdminSession(t, db, rdb, username)
	backup := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))

	begin := beginCredentialRegister(t, engine, sid, credentialStepUp(t, engine, sid, primary))
	rec := finishCredentialRegister(engine, other, begin, backup, t)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = finishCredentialRegister(engine, sid, begin, backup, t)
	require.Equal(t, http.StatusForbidden, rec.Code, "another session's attempt spent the handle: %s", rec.Body.String())

	begin = beginCredentialRegister(t, engine, sid, credentialStepUp(t, engine, sid, primary))
	_, err := db.Exec(`UPDATE admin_users SET credential_generation = credential_generation + 1 WHERE id = $1`, adminID)
	require.NoError(t, err)
	rec = finishCredentialRegister(engine, sid, begin, backup, t)
	require.Equal(t, http.StatusForbidden, rec.Code, "a ceremony from before a reset must not add a key: %s", rec.Body.String())

	assert.Equal(t, 1, ownCredentialCount(t, db, adminID))
	assert.Equal(t, 0, credentialAddedRows(t, db, adminID, admin.AuditSuccess))
}

// An admin holding MaxAdminCredentials keys gets 409 before any proof is
// spent; the repository enforces the same cap under the row lock.
func TestAddCredential_CapIsEnforced(t *testing.T) {
	db, rdb := credentialStores(t)
	adminID, username, primary := enrolledAdmin(t, db, rdb)
	repo := admin.NewAdminRepo(db)
	for i := 1; i < admin.MaxAdminCredentials; i++ {
		va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
		_, err := repo.AddCredential(t.Context(), admin.AdminCredential{
			AdminID: adminID, CredentialID: va.credID, PublicKey: va.coseKey(t), AAGUID: make([]byte, 16), CredentialName: "extra",
		})
		require.NoError(t, err)
	}
	engine := routedCreateEngine(t, db, rdb)
	sid := mintAdminSession(t, db, rdb, username)

	stepUp := credentialStepUp(t, engine, sid, primary)
	rec := postAsSession(engine, sid, credentialTestIP, credentialRegBeginPath, map[string]any{"stepup": stepUp})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"credential_limit_reached","max":4}`, rec.Body.String())
	assert.Equal(t, 1, credentialAddedRows(t, db, adminID, admin.AuditDenied))

	a, err := repo.GetByUsername(t.Context(), username)
	require.NoError(t, err)
	va := newVirtualAuthenticator(t, uuid.MustParse(testAllowedAAGID))
	_, err = repo.AddAdditionalCredential(t.Context(), a.CredentialGeneration, admin.AdminCredential{
		AdminID: adminID, CredentialID: va.credID, PublicKey: va.coseKey(t), AAGUID: make([]byte, 16),
	})
	require.ErrorIs(t, err, admin.ErrCredentialLimit)
	_, err = repo.AddAdditionalCredential(t.Context(), a.CredentialGeneration+1, admin.AdminCredential{
		AdminID: adminID, CredentialID: va.credID, PublicKey: va.coseKey(t), AAGUID: make([]byte, 16),
	})
	require.ErrorIs(t, err, admin.ErrEnrollmentStateChanged)
	assert.Equal(t, admin.MaxAdminCredentials, ownCredentialCount(t, db, adminID))
}

// The list route returns only the signed-in admin's keys and never their
// credential IDs or public keys.
func TestListOwnCredentials_ReturnsOnlyOwnKeys(t *testing.T) {
	db, rdb := credentialStores(t)
	adminID, username, _ := enrolledAdmin(t, db, rdb)
	_, otherUsername, _ := enrolledAdmin(t, db, rdb)
	engine := routedCreateEngine(t, db, rdb)

	rec := getAsSession(engine, mintAdminSession(t, db, rdb, username), credentialsPath)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Credentials []map[string]any `json:"credentials"`
		Max         int              `json:"max"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Credentials, 1)
	assert.Equal(t, admin.MaxAdminCredentials, resp.Max)
	stored, err := admin.NewAdminRepo(db).ListCredentials(t.Context(), adminID)
	require.NoError(t, err)
	assert.Equal(t, stored[0].ID, resp.Credentials[0]["id"])
	assert.Equal(t, "primary", resp.Credentials[0]["name"])
	assert.ElementsMatch(t, []string{"id", "name", "created_at"}, keysOf(resp.Credentials[0]))

	rec = getAsSession(engine, mintAdminSession(t, db, rdb, otherUsername), credentialsPath)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), stored[0].ID, "another admin never sees this admin's keys")

	rec = getAsSession(engine, "not-a-session", credentialsPath)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// One key, one counter: with the Host Agent projection enabled, the step-up
// that authorises adding a key is relayed to the agent's root counter like a
// login. A refusal there denies the step-up; a 404 (never imported) and a 200
// let it through.
func TestAddCredential_StepUpAdvancesRootCounter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agent      int
		wantStatus int
	}{
		{"root counter verified", http.StatusOK, http.StatusOK},
		{"never imported", http.StatusNotFound, http.StatusOK},
		{"root counter refused", http.StatusForbidden, http.StatusForbidden},
		{"root unavailable", http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, rdb := setupProjectionStores(t)
			adminID, _, primary := enrolledAdmin(t, db, rdb)
			sock, calls := importedAssertionAgent(t, tc.agent)
			gin.SetMode(gin.TestMode)
			h, err := admin.NewHandler(db, rdb, logger.NewWithWriter(&bytes.Buffer{}), authHandlerCfg())
			require.NoError(t, err)
			admin.UseServerAgentForTest(h, sock)
			engine := gin.New()
			engine.Use(asAdminSession(adminID))
			engine.POST(credentialStepUpPath, h.CredentialStepUpBegin)
			engine.POST(credentialRegBeginPath, h.CredentialRegisterBegin)

			handle, challenge := stepUpBegin(t, engine, credentialStepUpPath, map[string]any{})
			rec := postJSON(engine, credentialRegBeginPath, map[string]any{"stepup": map[string]any{
				"handle": handle, "assertion": signStepUp(t, primary, challenge, flagUP|flagUV),
			}})
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			got := calls()
			require.Len(t, got, 1, "the step-up is relayed to the root counter")
			assert.Equal(t, "/v1/authorizers/login-assertion", got[0].Path)
			assert.Equal(t, adminID, got[0].AdminID)
		})
	}
}
