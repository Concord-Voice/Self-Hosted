package hostagent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
)

func loginAssertionFixture(t *testing.T) (*Server, *AuthorizerStore, string, webauthn.SessionData, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	public, err := private.PublicKey.Bytes()
	require.NoError(t, err)
	encoder, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	coseKey, err := encoder.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: public[1:33], -3: public[33:65]})
	require.NoError(t, err)
	root := t.TempDir()
	doc := testAuthorizerDoc(t)
	testAuthorizerEntry(t, doc)["public_key"] = base64.RawURLEncoding.EncodeToString(coseKey)
	store := loadTestAuthorizers(t, writeAuthorizerDoc(t, root, doc), root)
	adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
	verifier, err := mfa.NewWebAuthnService("admin.example.test", "Concord Voice", []string{"https://admin.example.test"})
	require.NoError(t, err)
	credentials := store.Credentials(adminID)
	credentials = append(credentials, webauthn.Credential{ID: []byte("db-only"), PublicKey: coseKey})
	user := &mfa.WebAuthnUser{ID: []byte(adminWebAuthnUserHandle), Name: adminID, Credentials: credentials}
	options, session, err := verifier.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	require.NoError(t, err)
	assertion := signedAgentAssertion(t, private, credentials[0].ID, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 5)
	server := NewServer(&fakeRunner{})
	server.EnableWrites(&fakeRunner{}, root, filepath.Join(t.TempDir(), "marker"))
	require.NoError(t, server.SetAuthorizationStore(NewAuthorizationStore(store, verifier, time.Now)))
	return server, store, adminID, *session, assertion, private
}

func postLoginAssertion(t *testing.T, server *Server, adminID string, session webauthn.SessionData, assertion []byte) int {
	t.Helper()
	body, err := json.Marshal(struct {
		AdminID   string               `json:"admin_id"`
		Session   webauthn.SessionData `json:"session_data"`
		Assertion json.RawMessage      `json:"assertion"`
	}{adminID, session, assertion})
	require.NoError(t, err)
	return post(t, server, "/v1/authorizers/login-assertion", string(body)).Code
}

func TestLoginAssertionChecksRootCounterWithoutActionAuthority(t *testing.T) {
	server, store, adminID, session, assertion, _ := loginAssertionFixture(t)
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, assertion))
	require.Equal(t, uint32(5), store.Credentials(adminID)[0].Authenticator.SignCount)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, assertion), "a stale cloned assertion cannot repeat")
	require.Empty(t, server.authorizations.pending, "login proof cannot allocate action authority")
}

func TestLoginAssertionRevokedKeyRetainsCounterButCannotAuthorizeActions(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	id := store.Credentials(adminID)[0].ID
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, assertion))
	handle, options, err := server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	revoked, err := store.Revoke(adminID)
	require.NoError(t, err)
	require.Equal(t, 1, revoked)
	require.Empty(t, store.Credentials(adminID), "revoked credentials cannot begin a root action")
	actionAssertion := signedAgentAssertion(t, private, id, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 6)
	_, err = server.authorizations.Consume(handle, "start", actionAssertion)
	require.ErrorIs(t, err, ErrAuthorizationDenied, "revocation must also invalidate an action already begun")
	_, _, err = server.authorizations.Begin(adminID, "start", actionRequest{})
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	list, err := store.List()
	require.NoError(t, err)
	require.True(t, list[0].Revoked)
	require.Equal(t, uint32(5), list[0].SignCount)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, assertion), "a revoked stale clone remains known and denied")

	next := signedAgentAssertion(t, private, id, adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 6)
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, next), "a revoked credential may still log in after root counter verification")
	list, err = store.List()
	require.NoError(t, err)
	require.True(t, list[0].Revoked)
	require.Equal(t, uint32(6), list[0].SignCount)

	doc, err := store.read()
	require.NoError(t, err)
	entry := doc.Authorizers[0]
	_, err = store.ImportMany(AuthorizerImport{
		Schema: 1, RPID: doc.RPID, Origins: doc.Origins, AdminID: adminID,
		Credentials: []AuthorizerCredentialImport{{CredentialID: entry.CredentialID, PublicKey: entry.PublicKey, SignCount: 4, Label: entry.Label}},
	})
	require.NoError(t, err)
	require.Equal(t, uint32(6), store.Credentials(adminID)[0].Authenticator.SignCount, "reimport cannot lower the durable counter")
	list, err = store.List()
	require.NoError(t, err)
	require.False(t, list[0].Revoked)
	_, _, err = server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err, "explicit reimport restores root action authority")
}

func TestLoginAssertionMissingKeyAndBadInput(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	missing := signedAgentAssertion(t, private, []byte("db-only"), adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 5) //nolint:gosec // public fixture ID
	require.Equal(t, http.StatusNotFound, postLoginAssertion(t, server, adminID, session, missing), "only never-imported allowed IDs may use DB-only fallback")
	require.Equal(t, uint32(4), store.Credentials(adminID)[0].Authenticator.SignCount)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, []byte(`{}`)))
	require.Equal(t, http.StatusBadRequest, post(t, server, "/v1/authorizers/login-assertion", `{"admin_id":"x","unexpected":true}`).Code)
	require.Equal(t, http.StatusBadRequest, post(t, server, "/v1/authorizers/login-assertion", `{} {}`).Code)
	noPeer := httptest.NewRecorder()
	server.ServeHTTP(noPeer, httptest.NewRequest(http.MethodPost, "/v1/authorizers/login-assertion", nil))
	require.Equal(t, http.StatusForbidden, noPeer.Code)
	server.authorizations = nil
	require.Equal(t, http.StatusServiceUnavailable, postLoginAssertion(t, server, adminID, session, assertion))
}

func TestLoginAssertionRemovedTrustFailsClosed(t *testing.T) {
	server, store, adminID, session, assertion, _ := loginAssertionFixture(t)
	require.NoError(t, os.Remove(store.path))
	require.Equal(t, http.StatusServiceUnavailable, postLoginAssertion(t, server, adminID, session, assertion), "removed trust must never become DB-only fallback")
}
