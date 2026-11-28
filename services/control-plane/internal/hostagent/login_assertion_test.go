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
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
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
	addTestAuthorizer(t, doc, testBackupCredentialID)
	store := loadTestAuthorizers(t, writeAuthorizerDoc(t, root, doc), root)
	adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
	verifier, err := mfa.NewWebAuthnService("admin.example.test", "Concord Voice", []string{"https://admin.example.test"})
	require.NoError(t, err)
	credentials := testCredentials(t, store, adminID)
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

// testBackupCredentialID is the fixture's second (backup) key. It shares the
// primary key's private key only to keep the fixture small; root records it as
// a separate credential with its own counter.
var testBackupCredentialID = []byte("credential-backup")

// addTestAuthorizer appends a copy of the first entry under another ID.
func addTestAuthorizer(t *testing.T, doc map[string]any, id []byte) {
	t.Helper()
	entries, ok := doc["authorizers"].([]map[string]any)
	require.True(t, ok)
	entry := make(map[string]any, len(entries[0]))
	for k, v := range entries[0] {
		entry[k] = v
	}
	entry["credential_id"] = base64.RawURLEncoding.EncodeToString(id)
	doc["authorizers"] = append(entries, entry)
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
	require.Equal(t, uint32(5), testCredentials(t, store, adminID)[0].Authenticator.SignCount)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, assertion), "a stale cloned assertion cannot repeat")
	require.Empty(t, server.authorizations.pending, "login proof cannot allocate action authority")
}

func TestLoginAssertionRevokedKeyCannotSignInOrAuthorizeActions(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	id := testCredentials(t, store, adminID)[0].ID
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, assertion))
	handle, options, err := server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	revoked, err := store.Revoke(adminID)
	require.NoError(t, err)
	require.Equal(t, 2, revoked)
	require.Empty(t, testCredentials(t, store, adminID), "revoked credentials cannot begin a root action")
	actionAssertion := signedAgentAssertion(t, private, testBackupCredentialID, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 6)
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
	// 403, not 404 authorizer_not_imported: the control plane admits a
	// not-imported key, so that answer would let a revoked key sign in.
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, next), "a key root revoked must not sign in")
	list, err = store.List()
	require.NoError(t, err)
	require.True(t, list[0].Revoked)
	require.Equal(t, uint32(5), list[0].SignCount, "a refused login does not move the counter")

	doc, err := store.read()
	require.NoError(t, err)
	reimport := AuthorizerImport{Schema: 1, RPID: doc.RPID, Origins: doc.Origins, AdminID: adminID}
	for _, entry := range doc.Authorizers {
		reimport.Credentials = append(reimport.Credentials, AuthorizerCredentialImport{CredentialID: entry.CredentialID, PublicKey: entry.PublicKey, SignCount: 4, Label: entry.Label})
	}
	_, err = store.ImportMany(reimport, false)
	require.ErrorIs(t, err, ErrAuthorizerRevokedImport, "re-importing an old export must not silently un-revoke")
	list, err = store.List()
	require.NoError(t, err)
	require.True(t, list[0].Revoked)
	_, _, err = server.authorizations.Begin(adminID, "start", actionRequest{})
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	_, err = store.ImportMany(reimport, true)
	require.NoError(t, err)
	require.Equal(t, uint32(5), testCredentials(t, store, adminID)[0].Authenticator.SignCount, "reimport cannot lower the durable counter")
	list, err = store.List()
	require.NoError(t, err)
	require.False(t, list[0].Revoked)
	_, _, err = server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err, "explicit reimport restores root action authority")
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, next), "explicit reimport restores sign-in")
}

// TestOneKeyLogsInAndAuthorizesAnActionOnOneRootCounter proves the same
// credential satisfies a console login and then a root action, and that both
// ceremonies advance the single root-file counter: neither can replay the
// other's signature count.
func TestOneKeyLogsInAndAuthorizesAnActionOnOneRootCounter(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	primary := testCredentials(t, store, adminID)[0].ID
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, assertion))
	require.Equal(t, uint32(5), testCredentials(t, store, adminID)[0].Authenticator.SignCount, "login advances the root counter")

	handle, options, err := server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	allowed := make([][]byte, 0, len(options.Response.AllowedCredentials))
	for _, descriptor := range options.Response.AllowedCredentials {
		allowed = append(allowed, descriptor.CredentialID)
	}
	require.ElementsMatch(t, [][]byte{primary, testBackupCredentialID}, allowed, "every imported key of the admin may authorize an action")
	stale := signedAgentAssertion(t, private, primary, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 5)
	_, err = server.authorizations.Consume(handle, "start", stale)
	require.ErrorIs(t, err, ErrAuthorizationDenied, "an action cannot reuse the counter the login consumed")

	handle, options, err = server.authorizations.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	action := signedAgentAssertion(t, private, primary, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 6)
	_, err = server.authorizations.Consume(handle, "start", action)
	require.NoError(t, err, "the login key authorizes a root action")
	require.Equal(t, uint32(6), testCredentials(t, store, adminID)[0].Authenticator.SignCount, "the action advances the same root counter")

	replay := signedAgentAssertion(t, private, primary, adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 6)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, replay), "a login cannot reuse the counter the action consumed")
	next := signedAgentAssertion(t, private, primary, adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 7)
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, next))
	require.Equal(t, uint32(7), testCredentials(t, store, adminID)[0].Authenticator.SignCount)
}

func TestLoginAssertionMissingKeyAndBadInput(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	missing := signedAgentAssertion(t, private, []byte("db-only"), adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 5) //nolint:gosec // public fixture ID
	require.Equal(t, http.StatusNotFound, postLoginAssertion(t, server, adminID, session, missing), "only never-imported allowed IDs may use DB-only fallback")
	require.Equal(t, uint32(4), testCredentials(t, store, adminID)[0].Authenticator.SignCount)
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

// The selection check, not only the counter write, refuses a revoked key, so
// a revoked key is never verified at all. Each check alone must refuse.
func TestSelectLoginAuthorizerRefusesRevokedKey(t *testing.T) {
	id := []byte("credential-1")
	adminID := uuid.NewString()
	doc := authorizerFile{RPID: "admin.example.test", Authorizers: []authorizerRecord{{
		AdminID: adminID, CredentialID: base64.RawURLEncoding.EncodeToString(id), Revoked: true,
	}}}
	session := webauthn.SessionData{RelyingPartyID: "admin.example.test", AllowedCredentialIDs: [][]byte{id}}
	_, err := selectLoginAuthorizer(doc, session, id, adminID)
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	require.NotErrorIs(t, err, errAuthorizerNotImported, "not-imported would let the control plane sign the key in")

	doc.Authorizers[0].Revoked = false
	selected, err := selectLoginAuthorizer(doc, session, id, adminID)
	require.NoError(t, err, "control: the same key is selected once active")
	require.Equal(t, adminID, selected.AdminID)
}
