package hostagent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
)

var testLoginCredentialID = []byte("credential-1")

// purposeFixture trusts one login key and one action key for the same admin,
// signed by the same private key, behind the real go-webauthn verifier. Only
// the purpose root recorded tells the two apart.
func purposeFixture(t *testing.T) (*AuthorizationStore, *AuthorizerStore, string, *ecdsa.PrivateKey) {
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
	testAuthorizerEntry(t, doc)["purpose"] = AuthorizerPurposeLogin
	addTestAuthorizer(t, doc, testActionCredentialID, AuthorizerPurposeAction)
	authorizers := loadTestAuthorizers(t, writeAuthorizerDoc(t, root, doc), root)
	verifier, err := mfa.NewWebAuthnService("admin.example.test", "Concord Voice", []string{"https://admin.example.test"})
	require.NoError(t, err)
	store := NewAuthorizationStore(authorizers, verifier, time.Now)
	store.jobs = &jobs{backups: root, boundary: root}
	return store, authorizers, testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id"), private
}

func signCounts(t *testing.T, authorizers *AuthorizerStore) map[string]uint32 {
	t.Helper()
	doc, err := authorizers.read()
	require.NoError(t, err)
	counts := map[string]uint32{}
	for _, entry := range doc.Authorizers {
		counts[entry.CredentialID] = entry.SignCount
	}
	return counts
}

func TestAuthorizerPurposeIsRequiredAndClosed(t *testing.T) {
	for purpose, valid := range map[any]bool{"login": true, "action": true, "": false, "root": false, "Action": false, nil: false} {
		root := t.TempDir()
		doc := testAuthorizerDoc(t)
		if purpose == nil {
			delete(testAuthorizerEntry(t, doc), "purpose")
		} else {
			testAuthorizerEntry(t, doc)["purpose"] = purpose
		}
		_, err := loadAuthorizersAt(writeAuthorizerDoc(t, root, doc), testOwnerUID(), root)
		if valid {
			require.NoError(t, err, "purpose %v", purpose)
		} else {
			require.Error(t, err, "purpose %v must make the trust file invalid", purpose)
		}
	}
}

func TestActionOptionsContainOnlyActionCredentials(t *testing.T) {
	store, _, adminID, _ := purposeFixture(t)
	_, options, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	require.Len(t, options.Response.AllowedCredentials, 1)
	require.Equal(t, testActionCredentialID, []byte(options.Response.AllowedCredentials[0].CredentialID))
}

func TestActionAssertionByLoginKeyIsRefused(t *testing.T) {
	store, authorizers, adminID, private := purposeFixture(t)
	before := signCounts(t, authorizers)
	handle, options, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	assertion := signedAgentAssertion(t, private, testLoginCredentialID, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 9)
	_, err = store.Consume(handle, "start", assertion)
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	require.Equal(t, before, signCounts(t, authorizers), "a refused login-key assertion moves no counter")

	handle, options, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	assertion = signedAgentAssertion(t, private, testActionCredentialID, adminWebAuthnUserHandle, options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, 9)
	_, err = store.Consume(handle, "start", assertion)
	require.NoError(t, err, "positive control: the action key authorizes")
}

// The verifier reports which credential actually signed. Even if it returned
// a login credential, the counter write refuses it under the file lock.
func TestConsumeChecksPurposeOfTheSigningCredential(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	doc, err := authorizers.read()
	require.NoError(t, err)
	actionID := doc.Authorizers[0].CredentialID
	reimport := testAuthorizerImportFor(t, authorizers, adminID)
	login := reimport.Credentials[0]
	login.CredentialID = base64.RawURLEncoding.EncodeToString([]byte("credential-login"))
	reimport.Credentials = append(reimport.Credentials, login)
	_, err = authorizers.ImportWithPurposes(reimport, false, []string{actionID})
	require.NoError(t, err)
	before := signCounts(t, authorizers)
	handle, _, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	signer := *verifier.credential
	signer.ID = []byte("credential-login")
	verifier.credential = &signer
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.ErrorIs(t, err, errNoActionCredential)
	require.Equal(t, before, signCounts(t, authorizers))
}

func TestNoActionCredentialIsADistinctAuditedRefusal(t *testing.T) {
	server, _ := writeServer(t, &fakeRunner{})
	authorizers := server.authorizations.authorizers
	list, err := authorizers.List()
	require.NoError(t, err)
	adminID := list[0].AdminID
	handle, _, err := server.authorizations.Begin(adminID, "stop", actionRequest{})
	require.NoError(t, err)

	// Root re-imports the key as a login key between Begin and Consume.
	_, err = authorizers.ImportMany(testAuthorizerImportFor(t, authorizers, adminID), false)
	require.NoError(t, err)
	response := post(t, server, "/v1/actions/stop", `{"handle":"`+handle+`","assertion":{"test":true}}`)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, response.Body.String(), "authorization_denied")

	_, _, err = server.authorizations.Begin(adminID, "stop", actionRequest{})
	require.ErrorIs(t, err, errNoActionCredential)
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	response = postBegin(t, server, "stop", `{}`)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, response.Body.String(), "authorization_denied")

	var reasons []string
	for _, line := range readAuditLines(t, server.audit.path) {
		reason, ok := line["reason_code"].(string)
		require.True(t, ok)
		reasons = append(reasons, reason)
	}
	require.Equal(t, []string{"no_action_credential", "no_action_credential"}, reasons, "consume and begin each record the distinct reason")
}

func TestLoginAssertionByActionKeyIsRefusedAndAudited(t *testing.T) {
	server, store, adminID, session, assertion, private := loginAssertionFixture(t)
	require.NoError(t, server.SetAudit(testAudit(t)))
	before := signCounts(t, store)
	actionAssertion := signedAgentAssertion(t, private, testActionCredentialID, adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 9)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, actionAssertion))
	require.Equal(t, before, signCounts(t, store), "an action key's login attempt moves no counter")
	lines := readAuditLines(t, server.audit.path)
	require.Len(t, lines, 1)
	require.Equal(t, "action_credential_login", lines[0]["reason_code"])
	require.Equal(t, adminID, lines[0]["admin_id"])
	require.Equal(t, http.StatusOK, postLoginAssertion(t, server, adminID, session, assertion), "positive control: the login key logs in")

	// The purpose is checked before any verification: an action key named at
	// login is refused and audited even when its signature would not verify.
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	forged := signedAgentAssertion(t, other, testActionCredentialID, adminWebAuthnUserHandle, session.Challenge, "admin.example.test", "https://admin.example.test", 0x05, 10)
	require.Equal(t, http.StatusForbidden, postLoginAssertion(t, server, adminID, session, forged))
	lines = readAuditLines(t, server.audit.path)
	require.Len(t, lines, 2)
	require.Equal(t, "action_credential_login", lines[1]["reason_code"])
}

func TestImportPurposeDesignationAndReimport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authorizers.json")
	store, err := loadOrCreateAuthorizersAt(path, testOwnerUID(), root, "admin.example.test", []string{"https://admin.example.test"})
	require.NoError(t, err)
	doc := testAuthorizerImport(t)
	second := doc.Credentials[0]
	second.CredentialID = base64.RawURLEncoding.EncodeToString(testActionCredentialID)
	doc.Credentials = append(doc.Credentials, second)
	loginID, actionID := doc.Credentials[0].CredentialID, second.CredentialID

	_, err = store.ImportWithPurposes(doc, false, []string{actionID, base64.RawURLEncoding.EncodeToString([]byte("absent"))})
	require.ErrorIs(t, err, ErrAuthorizerActionCredentialUnknown)
	empty, err := store.read()
	require.NoError(t, err)
	require.Empty(t, empty.Authorizers, "a rejected designation imports nothing")

	result, err := store.ImportWithPurposes(doc, false, []string{actionID})
	require.NoError(t, err)
	require.Equal(t, 2, result.Added)
	require.Empty(t, result.PurposeChanges)
	ids, err := store.actionCredentialIDs(doc.AdminID)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{actionID: {}}, ids)
	list, err := store.List()
	require.NoError(t, err)
	require.Equal(t, AuthorizerPurposeLogin, list[0].Purpose)
	require.Equal(t, AuthorizerPurposeAction, list[1].Purpose)

	raw, err := base64.RawURLEncoding.DecodeString(actionID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceCounter(raw, 7))
	// Swap the purposes: allowed for active keys, reported, counters kept.
	result, err = store.ImportWithPurposes(doc, false, []string{loginID})
	require.NoError(t, err)
	require.Zero(t, result.Added)
	require.Len(t, result.PurposeChanges, 2)
	require.Equal(t, AuthorizerPurposeLogin, result.PurposeChanges[0].From)
	require.Equal(t, AuthorizerPurposeAction, result.PurposeChanges[0].To)
	require.Equal(t, AuthorizerPurposeAction, result.PurposeChanges[1].From)
	require.Equal(t, AuthorizerPurposeLogin, result.PurposeChanges[1].To)
	require.Len(t, result.PurposeChanges[0].CredentialSHA256, 64)
	require.Equal(t, uint32(7), signCounts(t, store)[actionID], "a re-import never lowers a counter")
	ids, err = store.actionCredentialIDs(doc.AdminID)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{loginID: {}}, ids)

	// The same designation again changes nothing.
	result, err = store.ImportWithPurposes(doc, false, []string{loginID})
	require.NoError(t, err)
	require.Zero(t, result.Added)
	require.Empty(t, result.PurposeChanges)

	// A revoked key stays revoked without --reinstate, and its purpose cannot
	// change by re-import even with it.
	_, err = store.Revoke(doc.AdminID)
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // test-created trust file
	require.NoError(t, err)
	_, err = store.ImportWithPurposes(doc, false, []string{loginID})
	require.ErrorIs(t, err, ErrAuthorizerRevokedImport)
	_, err = store.ImportWithPurposes(doc, true, []string{actionID})
	require.ErrorIs(t, err, ErrAuthorizerRevokedPurposeChange)
	after, err := os.ReadFile(path) //nolint:gosec // test-created trust file
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after), "a refused re-import leaves the trust file untouched")
	result, err = store.ImportWithPurposes(doc, true, []string{loginID})
	require.NoError(t, err)
	require.Equal(t, 2, result.Added)
	ids, err = store.actionCredentialIDs(doc.AdminID)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{loginID: {}}, ids)
}

func TestLoadActionCredentialIDsAbsentProjection(t *testing.T) {
	_, err := LoadActionCredentialIDs(filepath.Join(string(filepath.Separator), "concord-test-absent-authorizers.json"), "3e7138e5-6eb9-4a79-8133-5eab67b7f2b6")
	require.Equal(t, os.ErrNotExist, err) //nolint:errorlint // the contract is os.ErrNotExist itself
}
