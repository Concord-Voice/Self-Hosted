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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/mfa"
)

type fakeAgentWebAuthn struct {
	beginErr, finishErr error
	credential          *webauthn.Credential
	session             webauthn.SessionData
	begins, finishes    int
	gotSession          webauthn.SessionData
	gotOptions          protocol.PublicKeyCredentialRequestOptions
	// repeatCount reports the stored counter unchanged, as a clone would.
	repeatCount bool
	onFinish    func()
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
	if f.onFinish != nil {
		f.onFinish()
	}
	if f.credential == nil {
		return nil, f.finishErr
	}
	credential := *f.credential
	if len(user.Credentials) > 0 {
		credential.Authenticator.SignCount = user.Credentials[0].Authenticator.SignCount + 1
		if f.repeatCount {
			credential.Authenticator.SignCount--
		}
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
	credential := testCredentials(t, authorizers, adminID)[0]
	credential.Authenticator.SignCount = 5
	verifier := &fakeAgentWebAuthn{
		credential: &credential,
		session: webauthn.SessionData{
			Challenge: "full-session", RelyingPartyID: "admin.example.test", Origin: "https://admin.example.test",
			UserID: []byte(adminWebAuthnUserHandle), AllowedCredentialIDs: [][]byte{credential.ID}, UserVerification: protocol.VerificationRequired,
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
	var no *refusal
	_, _, err := store.Begin("unknown", "start", actionRequest{})
	require.ErrorAs(t, err, &no)
	require.Equal(t, refusal{http.StatusBadRequest, "invalid_authorization"}, *no)
	_, _, err = store.Begin(adminID, "unknown", actionRequest{})
	require.ErrorAs(t, err, &no)
	require.Equal(t, "invalid_authorization", no.reason)
	_, _, err = store.Begin(adminID, "start", actionRequest{Tag: "v1.2.3"})
	require.ErrorAs(t, err, &no)
	require.Equal(t, refusal{http.StatusBadRequest, "unexpected_argument"}, *no)
	_, _, err = store.Begin(adminID, "update", actionRequest{Tag: "v1.2.3-" + strings.Repeat("a", 122)})
	require.ErrorAs(t, err, &no, "a 129-byte tag is refused before the parser")
	require.Equal(t, "invalid_authorization", no.reason)
	require.Zero(t, verifier.begins)

	for range authorizationsPerAdmin {
		handle, options, err := store.Begin(adminID, "start", actionRequest{})
		require.NoError(t, err)
		require.Len(t, handle, 43)
		require.NotNil(t, options)
	}
	require.Equal(t, protocol.VerificationRequired, verifier.gotOptions.UserVerification)
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.ErrorIs(t, err, ErrAuthorizationBusy, "one admin cannot hold more than its share of ceremonies")
	require.Equal(t, authorizationsPerAdmin, verifier.begins)

	// This admin's handles expire while other admins' ceremonies leave one
	// free slot in the shared store.
	*now = now.Add(authorizationTTL / 2)
	for handle, pending := range store.pending {
		if pending.adminID == adminID {
			pending.expires = *now
			store.pending[handle] = pending
		}
	}
	for i := range authorizationCapacity - 1 {
		store.pending[fmt.Sprintf("other-%d", i)] = pendingAuthorization{adminID: uuid.NewString(), action: "start", expires: now.Add(authorizationTTL)}
	}
	// Begin sweeps the expired handles, takes the last slot, then is full.
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err, "expired handles must release capacity")
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.ErrorIs(t, err, ErrAuthorizationBusy, "the global bound still applies")
	*now = now.Add(authorizationTTL)
	_, _, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err, "expired handles must release global capacity")
}

func TestAuthorizationBeginArgumentsRoundTripAndAssertionBoundary(t *testing.T) {
	store, verifier, adminID, _, _ := newAuthorizationFixture(t)
	tag := "v1.2.3-" + strings.Repeat("a", 121) // exactly 128 bytes
	require.Len(t, tag, 128)
	handle, _, err := store.Begin(adminID, "update", actionRequest{Tag: tag})
	require.NoError(t, err)
	req, err := store.Consume(handle, "update", make([]byte, maxAssertionBytes))
	require.NoError(t, err, "an assertion of exactly maxAssertionBytes is accepted")
	require.Equal(t, actionRequest{Tag: tag}, req)

	name := "concord-backup-20261106T071500Z.tar.gz"
	require.NoError(t, os.WriteFile(filepath.Join(store.jobs.backups, name), []byte("x"), 0o600))
	handle, _, err = store.Begin(adminID, "restore", actionRequest{Archive: name})
	require.NoError(t, err)
	req, err = store.Consume(handle, "restore", []byte(`{"assertion":true}`))
	require.NoError(t, err)
	require.Equal(t, actionRequest{Archive: name}, req)

	finishes := verifier.finishes
	handle, _, err = store.Begin(adminID, "update", actionRequest{Tag: tag})
	require.NoError(t, err)
	_, err = store.Consume(handle, "update", make([]byte, maxAssertionBytes+1))
	require.ErrorIs(t, err, ErrAuthorizationDenied, "one byte over the bound is refused")
	require.Equal(t, finishes, verifier.finishes, "an oversized assertion never reaches the verifier")
}

func TestAuthorizationCloneAndStaleCounterAreDistinctDenials(t *testing.T) {
	store, verifier, adminID, _, _ := newAuthorizationFixture(t)
	handle, _, err := store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	verifier.credential.Authenticator.CloneWarning = true
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.ErrorIs(t, err, errCloneDetected)
	require.ErrorIs(t, err, ErrAuthorizationDenied)
	verifier.credential.Authenticator.CloneWarning = false

	// The verifier reports the counter the root store already holds.
	verifier.repeatCount = true
	handle, _, err = store.Begin(adminID, "start", actionRequest{})
	require.NoError(t, err)
	_, err = store.Consume(handle, "start", []byte(`{"assertion":true}`))
	require.ErrorIs(t, err, errCounterStale)
	require.ErrorIs(t, err, ErrAuthorizationDenied, "a stale counter is an authorization denial, not an unavailable authorizer")
	require.NotErrorIs(t, err, ErrAuthorizerUnavailable)
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
	require.Equal(t, uint32(5), testCredentials(t, authorizers, adminID)[0].Authenticator.SignCount)
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
			require.Equal(t, uint32(4), testCredentials(t, authorizers, adminID)[0].Authenticator.SignCount)
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
	for _, scenario := range []string{"wrong origin", "wrong RP", "wrong credential", "wrong user handle", "missing UV", "valid"} {
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

			rpID, origin, credID, userHandle, flags := "admin.example.test", "https://admin.example.test", []byte("credential-1"), adminWebAuthnUserHandle, byte(0x05) //nolint:gosec // public test credential ID, not a secret
			switch scenario {
			case "wrong origin":
				origin = "https://attacker.example.test"
			case "wrong RP":
				rpID = "attacker.example.test"
			case "wrong credential":
				credID = []byte("other-credential")
			case "wrong user handle":
				userHandle = adminID
			case "missing UV":
				flags = 0x01
			}
			assertion := signedAgentAssertion(t, private, credID, userHandle, options.Response.Challenge.String(), rpID, origin, flags, 5)
			_, boundAdminID, err := store.consume(handle, "start", assertion)
			if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, adminID, boundAdminID)
				require.Equal(t, uint32(5), testCredentials(t, authorizers, adminID)[0].Authenticator.SignCount)
			} else {
				require.Error(t, err)
				require.Equal(t, uint32(4), testCredentials(t, authorizers, adminID)[0].Authenticator.SignCount)
			}
		})
	}
}

// TestAuthorizationRealVerifierRejectsRepeatedOrLowerCounters carries the
// control plane's former step-up counter rule to the agent, which now owns the
// host-write ceremony: a zero-counter key may repeat zero, but a repeated or
// lower advanced counter is refused and never rolls the root counter back.
func TestAuthorizationRealVerifierRejectsRepeatedOrLowerCounters(t *testing.T) {
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
	entry := testAuthorizerEntry(t, doc)
	entry["public_key"] = base64.RawURLEncoding.EncodeToString(coseKey)
	entry["sign_count"] = 0
	path := writeAuthorizerDoc(t, root, doc)
	authorizers := loadTestAuthorizers(t, path, root)
	adminID := testAuthorizerString(t, entry, "admin_id")
	verifier, err := mfa.NewWebAuthnService("admin.example.test", "Concord Voice", []string{"https://admin.example.test"})
	require.NoError(t, err)
	store := NewAuthorizationStore(authorizers, verifier, time.Now)
	store.jobs = &jobs{backups: root, boundary: root}

	authorize := func(signCount uint32) error {
		handle, options, beginErr := store.Begin(adminID, "update", actionRequest{Tag: "v1.2.3"})
		require.NoError(t, beginErr)
		assertion := signedAgentAssertion(t, private, []byte("credential-1"), adminWebAuthnUserHandle,
			options.Response.Challenge.String(), "admin.example.test", "https://admin.example.test", 0x05, signCount)
		_, consumeErr := store.Consume(handle, "update", assertion)
		return consumeErr
	}
	stored := func() uint32 { return testCredentials(t, authorizers, adminID)[0].Authenticator.SignCount }

	require.NoError(t, authorize(0), "a zero-counter key remains usable")
	require.NoError(t, authorize(0), "a zero-counter key may repeat zero")
	require.NoError(t, authorize(1))
	require.ErrorIs(t, authorize(1), ErrAuthorizationDenied, "a repeated advanced counter cannot authorize an action")
	require.NoError(t, authorize(2))
	require.ErrorIs(t, authorize(1), ErrAuthorizationDenied, "a lower advanced counter cannot authorize an action")
	require.Equal(t, uint32(2), stored(), "rejected assertions must not roll the root counter back")
}

func signedAgentAssertion(t *testing.T, private *ecdsa.PrivateKey, credentialID []byte, userHandle, challenge, rpID, origin string, flags byte, signCount uint32) []byte {
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
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userHandle)),
		},
	})
	require.NoError(t, err)
	return assertion
}
