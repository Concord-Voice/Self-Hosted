package hostagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
)

const maxLoginAssertionBody = 128 << 10

var errAuthorizerNotImported = errors.New("authorizer not imported")

// verifyLoginAssertion checks a CP-validated login assertion against root's
// current credential and durable counter. It creates no action authorization.
func (s *AuthorizationStore) verifyLoginAssertion(adminID string, session webauthn.SessionData, assertion []byte) error {
	if s == nil || s.authorizers == nil || s.webauthn == nil {
		return ErrAuthorizerUnavailable
	}
	parsedAdmin, err := uuid.Parse(adminID)
	if err != nil || parsedAdmin.String() != adminID || len(assertion) == 0 || len(assertion) > maxAssertionBytes {
		return ErrAuthorizationDenied
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil || !bytes.Equal(session.UserID, []byte(adminWebAuthnUserHandle)) || session.UserVerification != protocol.VerificationRequired {
		return ErrAuthorizationDenied
	}
	doc, err := s.authorizers.read()
	if err != nil {
		log.Printf("concord-agent: authorizer trust unreadable at login: %v", err)
		return ErrAuthorizerUnavailable
	}
	selected, err := selectLoginAuthorizer(doc, session, parsed.RawID, adminID)
	if err != nil {
		return err
	}
	// The verifier's origins were fixed at startup and BeginLogin leaves
	// session.Origin empty, so hold the asserted origin to root's current file.
	if !slices.Contains(doc.Origins, parsed.Response.CollectedClientData.Origin) {
		return ErrAuthorizationDenied
	}
	key, _ := decodeAuthorizerField(selected.PublicKey, maxAuthorizerKeyBytes)
	user := &mfa.WebAuthnUser{
		ID: []byte(adminWebAuthnUserHandle), Name: adminID, DisplayName: adminID,
		Credentials: []webauthn.Credential{{ID: parsed.RawID, PublicKey: key, Authenticator: webauthn.Authenticator{SignCount: selected.SignCount}}},
	}
	// CP may have more DB credentials than root has imported. Narrow the already
	// validated allow-list to the asserted ID; never accept an ID outside it.
	session.AllowedCredentialIDs = [][]byte{parsed.RawID}
	credential, err := s.webauthn.FinishLoginWithBytes(user, session, assertion)
	if err != nil || credential == nil || credential.Authenticator.CloneWarning {
		return ErrAuthorizationDenied
	}
	if err := s.authorizers.advanceLoginCounter(credential.ID, credential.Authenticator.SignCount); err != nil {
		if errors.Is(err, errAuthorizerCounterStale) {
			return ErrAuthorizationDenied
		}
		log.Printf("concord-agent: authorizer counter update failed at login: %v", err)
		return ErrAuthorizerUnavailable
	}
	return nil
}

func selectLoginAuthorizer(doc authorizerFile, session webauthn.SessionData, credentialID []byte, adminID string) (*authorizerRecord, error) {
	if session.RelyingPartyID != doc.RPID {
		return nil, ErrAuthorizationDenied
	}
	if session.Origin != "" {
		trusted := false
		for _, origin := range doc.Origins {
			trusted = trusted || session.Origin == origin
		}
		if !trusted {
			return nil, ErrAuthorizationDenied
		}
	}
	allowed := false
	for _, id := range session.AllowedCredentialIDs {
		allowed = allowed || bytes.Equal(id, credentialID)
	}
	if !allowed {
		return nil, ErrAuthorizationDenied
	}
	var selected *authorizerRecord
	for i := range doc.Authorizers {
		id, _ := decodeAuthorizerField(doc.Authorizers[i].CredentialID, maxAuthorizerIDBytes)
		if bytes.Equal(id, credentialID) {
			selected = &doc.Authorizers[i]
			break
		}
	}
	if selected == nil {
		return nil, errAuthorizerNotImported
	}
	if selected.AdminID != adminID {
		return nil, ErrAuthorizationDenied
	}
	return selected, nil
}

func (s *Server) handleLoginAssertion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if s.authorizations == nil {
		refuse(w, http.StatusServiceUnavailable, "authorizer_unavailable")
		return
	}
	var body struct {
		AdminID   string               `json:"admin_id"`
		Assertion json.RawMessage      `json:"assertion"`
		Session   webauthn.SessionData `json:"session_data"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginAssertionBody))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
		refuse(w, http.StatusBadRequest, "invalid_authorization")
		return
	}
	switch err := s.authorizations.verifyLoginAssertion(body.AdminID, body.Session, body.Assertion); {
	case err == nil:
		writeJSON(w, map[string]string{"status": "verified"})
	case errors.Is(err, errAuthorizerNotImported):
		refuse(w, http.StatusNotFound, "authorizer_not_imported")
	case errors.Is(err, ErrAuthorizerUnavailable):
		refuse(w, http.StatusServiceUnavailable, "authorizer_unavailable")
	default:
		refuse(w, http.StatusForbidden, "authorization_denied")
	}
}
