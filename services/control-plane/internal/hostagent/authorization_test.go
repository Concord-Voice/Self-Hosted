package hostagent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
)

type fakeAgentWebAuthn struct {
	beginErr, finishErr error
	credential          *webauthn.Credential
	session             webauthn.SessionData
	begins, finishes    int
	gotSession          webauthn.SessionData
	gotOptions          protocol.PublicKeyCredentialRequestOptions
}

func (f *fakeAgentWebAuthn) BeginLogin(_ *mfa.WebAuthnUser, opts ...webauthn.LoginOption) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	f.begins++
	if f.beginErr != nil {
		return nil, nil, f.beginErr
	}
	assertion := &protocol.CredentialAssertion{}
	for _, opt := range opts {
		if err := opt(&assertion.Response); err != nil {
			return nil, nil, err
		}
	}
	f.gotOptions = assertion.Response
	return assertion, &f.session, nil
}

func (f *fakeAgentWebAuthn) FinishLoginWithBytes(user *mfa.WebAuthnUser, session webauthn.SessionData, _ []byte) (*webauthn.Credential, error) {
	f.finishes++
	f.gotSession = session
	if f.credential == nil {
		return nil, f.finishErr
	}
	credential := *f.credential
	if len(user.Credentials) > 0 {
		credential.Authenticator.SignCount = user.Credentials[0].Authenticator.SignCount + 1
	}
	return &credential, f.finishErr
}

func newAuthorizationFixture(t *testing.T) (*AuthorizationStore, *fakeAgentWebAuthn, string, *AuthorizerStore, *time.Time) {
	t.Helper()
	root := t.TempDir()
	doc := testAuthorizerDoc(t)
	path := writeAuthorizerDoc(t, root, doc)
	authorizers := loadTestAuthorizers(t, path, root)
	adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
	credential := authorizers.Credentials(adminID)[0]
	credential.Authenticator.SignCount = 5
	verifier := &fakeAgentWebAuthn{
		credential: &credential,
		session: webauthn.SessionData{
			Challenge: "full-session", RelyingPartyID: "admin.example.test", Origin: "https://admin.example.test",
			UserID: []byte(adminID), AllowedCredentialIDs: [][]byte{credential.ID}, UserVerification: protocol.VerificationRequired,
			Expires: time.Now().Add(5 * time.Minute),
		},
	}
	now := time.Now()
	store := NewAuthorizationStore(authorizers, verifier, func() time.Time { return now })
	store.jobs = &jobs{backups: root, boundary: root}
	return store, verifier, adminID, authorizers, &now
}

func TestAuthorizationBeginIsBoundedAndRequiresTrustedAdminAndArgs(t *testing.T) {
	store, verifier, adminID, _, now := newAuthorizationFixture(t)
	_, _, err := store.Begin("unknown", "start", actionRequest{})
	require.Error(t, err)
	_, _, err = store.Begin(adminID, "unknown", actionRequest{})
	require.Error(t, err)
	_, _, err = store.Begin(adminID, "start", actionRequest{Tag: "v1.2.3"})
	require.Error(t, err)
	require.Zero(t, verifier.begins)

	for range 64 {
		handle, options, err := store.Begin(adminID, "start", actionRequest{})
		require.NoError(t, err)
		require.Len(t, handle, 43)
		require.NotNil(t, options)
	}
	require.Equal(t, protocol.VerificationRequired, verifier.gotOptions.UserVerification)
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.Error(t, err)
	require.Equal(t, 64, verifier.begins)
	*now = now.Add(authorizationTTL)
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err, "expired handles must release capacity")
}

func TestAuthorizationConsumeUsesStoredActionAndCompleteSessionOnce(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	handle, _, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	_, err = store.Consume(handle, "stop", []byte(`{"assertion":true}`))
	require.Error(t, err)
	require.Zero(t, verifier.finishes)
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.Error(t, err, "a mismatched action spends its handle")

	handle, _, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	req, err := store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.NoError(t, err)
	require.Equal(t, actionRequest{}, req)
	require.True(t, reflect.DeepEqual(verifier.session, verifier.gotSession), "SessionData must round-trip unchanged")
	require.Equal(t, uint32(5), authorizers.Credentials(adminID)[0].Authenticator.SignCount)
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.Error(t, err, "successful handle is spent")
}

func TestAuthorizationExpiryCloneAndCounterFaultFailClosed(t *testing.T) {
	for _, scenario := range []string{"expired", "verifier error", "clone warning", "counter fault"} {
		t.Run(scenario, func(t *testing.T) {
			store, verifier, adminID, authorizers, now := newAuthorizationFixture(t)
			handle, _, err := store.Begin(adminID, "backup", actionRequest{})
			require.NoError(t, err)
			switch scenario {
			case "expired":
				*now = now.Add(3 * time.Minute)
			case "verifier error":
				verifier.finishErr = errors.New("wrong origin or missing UV")
			case "clone warning":
				verifier.credential.Authenticator.CloneWarning = true
			case "counter fault":
				authorizers.syncFile = func(*os.File) error { return errors.New("forced fsync fault") }
			}
			_, err = store.Consume(handle, "backup", []byte(`{"assertion":true}`))
			require.Error(t, err)
			_, err = store.Consume(handle, "backup", []byte(`{"assertion":true}`))
			require.Error(t, err, "failed ceremony cannot be replayed")
			require.Equal(t, uint32(4), authorizers.Credentials(adminID)[0].Authenticator.SignCount)
		})
	}
}

func TestAuthorizationRevocationBetweenBeginAndConsumeFails(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	handle, _, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	_, err = authorizers.Revoke(adminID)
	require.NoError(t, err)
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.Error(t, err)
	require.Zero(t, verifier.finishes)
}

func TestAuthorizationBeginRejectsUntrustedStore(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	require.NoError(t, os.Chmod(authorizers.path, 0o666)) //nolint:gosec // corrupt root-store mode fixture
	_, _, err := store.Begin(adminID, "start", actionRequest{})
	require.Error(t, err)
	require.Zero(t, verifier.begins)
}

func TestAuthorizationBeginRouteIsPeerGatedAndReturnsNoActionAuthority(t *testing.T) {
	store, _, adminID, _, _ := newAuthorizationFixture(t)
	runner := &fakeRunner{}
	server := NewServer(runner)
	marker := filepath.Join(t.TempDir(), "write-in-progress")
	server.EnableWrites(runner, t.TempDir(), marker)
	require.NoError(t, server.SetAuthorizationStore(store))
	response := post(t, server, "/v1/authorizations", `{"admin_id":"`+adminID+`","action":"start","arguments":{}}`)
	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Handle  string                        `json:"handle"`
		Options *protocol.CredentialAssertion `json:"options"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Handle, 43)
	require.NotNil(t, body.Options)
	require.NotContains(t, response.Body.String(), "authorization_token")
	require.Empty(t, runner.args, "beginning a ceremony must not run an action")
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "beginning a ceremony must not create a job marker")

	for _, invalid := range []string{
		`{"admin_id":"` + adminID + `","action":"start","arguments":{},"unexpected":true}`,
		`{"admin_id":"` + adminID + `","action":"start","arguments":{"unexpected":true}}`,
		`{"admin_id":"` + adminID + `","action":"start","arguments":{}} {}`,
	} {
		response = post(t, server, "/v1/authorizations", invalid)
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
}

func TestAuthorizationRealWebAuthnVerifierRejectsWrongTrustClaims(t *testing.T) {
	for _, scenario := range []string{"wrong origin", "wrong RP", "wrong credential", "missing UV", "valid"} {
		t.Run(scenario, func(t *testing.T) {
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
			path := writeAuthorizerDoc(t, root, doc)
			authorizers := loadTestAuthorizers(t, path, root)
			adminID := testAuthorizerString(t, testAuthorizerEntry(t, doc), "admin_id")
			verifier, err := mfa.NewWebAuthnService("admin.example.test", "Concord Voice", []string{"https://admin.example.test"})
			require.NoError(t, err)
			store := NewAuthorizationStore(authorizers, verifier, time.Now)
			store.jobs = &jobs{backups: root, boundary: root}
			handle, options, err := store.Begin(adminID, "start", actionRequest{})
			require.NoError(t, err)

			rpID, origin, credID, flags := "admin.example.test", "https://admin.example.test", []byte("credential-1"), byte(0x05) //nolint:gosec // public test credential ID, not a secret
			switch scenario {
			case "wrong origin":
				origin = "https://attacker.example.test"
			case "wrong RP":
				rpID = "attacker.example.test"
			case "wrong credential":
				credID = []byte("other-credential")
			case "missing UV":
				flags = 0x01
			}
			assertion := signedAgentAssertion(t, private, credID, adminID, options.Response.Challenge.String(), rpID, origin, flags, 5)
			_, err = store.Consume(handle, "start", assertion)
			if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, uint32(5), authorizers.Credentials(adminID)[0].Authenticator.SignCount)
			} else {
				require.Error(t, err)
				require.Equal(t, uint32(4), authorizers.Credentials(adminID)[0].Authenticator.SignCount)
			}
		})
	}
}

func signedAgentAssertion(t *testing.T, private *ecdsa.PrivateKey, credentialID []byte, adminID, challenge, rpID, origin string, flags byte, signCount uint32) []byte {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, flags)
	var counter [4]byte
	binary.BigEndian.PutUint32(counter[:], signCount)
	authData = append(authData, counter[:]...)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin, "crossOrigin": false})
	require.NoError(t, err)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	require.NoError(t, err)
	id := base64.RawURLEncoding.EncodeToString(credentialID)
	assertion, err := json.Marshal(map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(adminID)),
		},
	})
	require.NoError(t, err)
	return assertion
}
