package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// captureLog redirects the package's standard logger for one test.
// lockedBuffer lets a test read logs while a background worker writes them.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

func forbiddenRequest(s *Server, uid int, withPeer bool) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/start", nil)
	if withPeer {
		req = req.WithContext(withPeerContext(req.Context(), uid))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

func withPeerContext(ctx context.Context, uid int) context.Context {
	return withPeer(ctx, Peer{UID: uid})
}

func TestPeerForbiddenAuditIsRateLimitedPerUIDAndCounted(t *testing.T) {
	server, _ := writeServer(t, &fakeRunner{})
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	server.denials.now = func() time.Time { return now }
	for range 100 {
		require.Equal(t, http.StatusForbidden, forbiddenRequest(server, 10007, true))
	}
	for range 10 {
		require.Equal(t, http.StatusForbidden, forbiddenRequest(server, 0, false))
	}
	lines := readAuditLines(t, server.audit.path)
	require.Len(t, lines, 2, "one record per UID per window, however many denials")
	now = now.Add(denialWindow)
	require.Equal(t, http.StatusForbidden, forbiddenRequest(server, 10007, true))
	lines = readAuditLines(t, server.audit.path)
	require.Len(t, lines, 3)
	require.Equal(t, "peer_forbidden", lines[2]["reason_code"])
	require.InDelta(t, 99, lines[2]["suppressed_count"], 0, "the next record counts the denials dropped before it")
}

func TestPeerDenialLimiterIsBoundedAcrossManyUIDs(t *testing.T) {
	var limiter denialLimiter
	now := time.Now()
	limiter.now = func() time.Time { return now }
	recorded := 0
	for uid := range maxDenialKeys * 4 {
		if ok, _ := limiter.admit(denialKey{reason: "peer_forbidden", uid: uid}); ok {
			recorded++
		}
	}
	require.LessOrEqual(t, len(limiter.entries), maxDenialKeys+1)
	require.Equal(t, maxDenialKeys+1, recorded, "UIDs past the bound share one overflow record")
}

func TestAuditFullLogsOnceAndRefusesWrites(t *testing.T) {
	logs := captureLog(t)
	audit := testAudit(t)
	require.NoError(t, os.Truncate(audit.path, maxAuditFileBytes))
	event := AuditEvent{Action: "stop", Outcome: "admitted", Reason: "admission_approved"}
	require.ErrorIs(t, audit.Write(event), errAuditFull)
	require.ErrorIs(t, audit.Write(event), errAuditFull)
	require.Equal(t, 1, strings.Count(logs.String(), "host-agent audit full"), "the full stream is reported, once")
	// Rotation by rename + create 0600, as logrotate does, re-enables writes.
	require.NoError(t, os.Rename(audit.path, audit.path+".1"))
	require.NoError(t, os.WriteFile(audit.path, nil, 0o600))
	require.NoError(t, audit.Write(event))
	require.Len(t, readAuditLines(t, audit.path), 1, "the next write lands in the replacement file")
	// So does an in-place truncation.
	require.NoError(t, os.Truncate(audit.path, 0))
	require.NoError(t, audit.Write(event))
	require.Len(t, readAuditLines(t, audit.path), 1)
}

func TestAuditBrokenLatchIsLogged(t *testing.T) {
	logs := captureLog(t)
	audit := testAudit(t)
	audit.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	require.Error(t, audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}))
	require.Contains(t, logs.String(), "host-agent audit sync failed; audit disabled until repaired")
}

func TestServerLogsAuditWriteFailures(t *testing.T) {
	logs := captureLog(t)
	server, _ := writeServer(t, &fakeRunner{})
	require.NoError(t, os.Truncate(server.audit.path, maxAuditFileBytes))
	server.auditDenial(AuditEvent{Outcome: "denied", Reason: "begin_denied"})
	require.Contains(t, logs.String(), "host-agent audit write failed: reason=begin_denied")
}

func TestJobAuditFailuresAreLoggedWithJobID(t *testing.T) {
	logs := captureLog(t)
	server, _ := writeServer(t, &fakeRunner{})
	server.audit.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	list, err := server.authorizations.authorizers.List()
	require.NoError(t, err)
	_, err = server.writes.start("stop", list[0].AdminID, actionRequest{}, [][]string{{"stop"}}, shortJobBudget)
	require.Error(t, err)
	require.Regexp(t, `host-agent audit admission event failed job=[0-9a-f]{32}`, logs.String())
}

func TestPeerCredentialFailureIsLogged(t *testing.T) {
	logs := captureLog(t)
	client, conn := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = conn.Close() })
	_ = peerConnContextWith(func(net.Conn) (Peer, error) { return Peer{}, errors.New("getsockopt failed") })(context.Background(), conn)
	require.Contains(t, logs.String(), "peer credentials unavailable: getsockopt failed")
}

func TestMarkerRemovalFailureIsLogged(t *testing.T) {
	logs := captureLog(t)
	dir := filepath.Join(t.TempDir(), "marker-dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "child"), nil, 0o600))
	removeMarker(dir, strings.Repeat("a", 32))
	require.Contains(t, logs.String(), "write marker removal failed job="+strings.Repeat("a", 32))
	logs.Reset()
	removeMarker(filepath.Join(t.TempDir(), "absent"), "")
	require.Empty(t, logs.String(), "an absent marker is not a failure")
}

func TestBeginRefusalsKeepTheirOwnCodes(t *testing.T) {
	server, dir := writeServer(t, &fakeRunner{})
	server.writes.boundary = filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		action, arguments string
		code              int
		reason            string
	}{
		{"update", `{"tag":"main"}`, http.StatusBadRequest, "invalid_tag"},
		{"start", `{"tag":"v1.0.0"}`, http.StatusBadRequest, "unexpected_argument"},
		{"restore", `{"archive":"../x"}`, http.StatusBadRequest, "invalid_archive"},
		{"restore", `{"archive":"concord-backup-20261106T071500Z.tar.gz"}`, http.StatusNotFound, "unknown_archive"},
		{"restart", `{}`, http.StatusConflict, "security_log_boundary_missing"},
		{"reboot", `{}`, http.StatusBadRequest, "invalid_authorization"},
	} {
		rec := postBegin(t, server, tc.action, tc.arguments)
		require.Equal(t, tc.code, rec.Code, "%s %s: %s", tc.action, tc.arguments, rec.Body)
		require.Contains(t, rec.Body.String(), `"`+tc.reason+`"`)
	}
	require.NoError(t, os.Chmod(dir, 0o777)) //nolint:gosec // deliberately unsafe input
	rec := postBegin(t, server, "restore", `{"archive":"concord-backup-20261106T071500Z.tar.gz"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "untrusted_backups")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // restore fixture

	// An admin with no trusted credential is a real authorization failure.
	rec = post(t, server, "/v1/authorizations", `{"admin_id":"`+uuid.NewString()+`","action":"stop","arguments":{}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "authorization_denied")

	server.SetManagedBy("truenas")
	rec = postBegin(t, server, "stop", `{}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "appliance_managed")
}

func TestWritesDisabledRefusesWriteRoutesDistinctly(t *testing.T) {
	server := NewServer(&fakeRunner{out: statusJSON})
	server.DisableWrites()
	for _, path := range []string{"/v1/actions/start", "/v1/authorizations", "/v1/jobs", "/v1/backups"} {
		rec := post(t, server, path, `{}`)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		require.Contains(t, rec.Body.String(), "writes_disabled", path)
	}
	require.Equal(t, http.StatusOK, get(t, server, http.MethodGet, "/v1/status").Code, "reads stay available")
	require.Equal(t, http.StatusNotFound, post(t, server, "/v1/unknown", "").Code)
}

func TestCredentialsReadErrorIsAuthorizerUnavailable(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	handle, _, err := store.Begin(adminID, "stop", actionRequest{})
	require.NoError(t, err)
	require.NoError(t, os.Chmod(authorizers.path, 0o666)) //nolint:gosec // corrupt root-store mode fixture
	_, _, err = store.Begin(adminID, "stop", actionRequest{})
	require.ErrorIs(t, err, ErrAuthorizerUnavailable, "an unreadable trust file is not 'no credentials'")
	_, err = store.Consume(handle, "stop", []byte(`{"assertion":true}`))
	require.ErrorIs(t, err, ErrAuthorizerUnavailable)
	require.Zero(t, verifier.finishes)
}

func TestWriteAuditNamesCloneAndStaleCounter(t *testing.T) {
	server, _ := writeServer(t, &fakeRunner{})
	verifier, ok := server.authorizations.webauthn.(*fakeAgentWebAuthn)
	require.True(t, ok)
	verifier.credential.Authenticator.CloneWarning = true
	rec := postAuthorized(t, server, "/v1/actions/stop", "")
	require.Equal(t, http.StatusForbidden, rec.Code)
	verifier.credential.Authenticator.CloneWarning = false
	verifier.repeatCount = true
	rec = postAuthorized(t, server, "/v1/actions/stop", "")
	require.Equal(t, http.StatusForbidden, rec.Code, "a stale counter is 403, not 503")
	require.Contains(t, rec.Body.String(), "authorization_denied")
	lines := readAuditLines(t, server.audit.path)
	require.Len(t, lines, 2)
	require.Equal(t, "clone_detected", lines[0]["reason_code"])
	require.Equal(t, "counter_stale", lines[1]["reason_code"])
}

func TestAuditRecordsTheValidatedArgumentsThatRan(t *testing.T) {
	server, dir := writeServer(t, &fakeRunner{})
	waitJob(t, server, started(t, postAuthorized(t, server, "/v1/actions/update", `{"tag":"v1.2.3"}`)).ID)
	name := "concord-backup-20261106T071500Z.tar.gz"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	waitJob(t, server, started(t, postAuthorized(t, server, "/v1/actions/restore", `{"archive":"`+name+`"}`)).ID)
	got := map[string][]string{}
	for _, line := range readAuditLines(t, server.audit.path) {
		reason, _ := line["reason_code"].(string)
		if tag, ok := line["tag"].(string); ok {
			got[reason] = append(got[reason], "tag="+tag)
		}
		if archive, ok := line["archive"].(string); ok {
			got[reason] = append(got[reason], "archive="+archive)
		}
	}
	for _, reason := range []string{"admission_approved", "job_started"} {
		require.Equal(t, []string{"tag=v1.2.3", "archive=" + name}, got[reason], reason)
	}
	require.False(t, validAuditEvent(AuditEvent{Outcome: "admitted", Reason: "admission_approved", Tag: "main"}))
	require.False(t, validAuditEvent(AuditEvent{Outcome: "admitted", Reason: "admission_approved", Tag: "v1.2.3-" + strings.Repeat("a", 122)}))
	require.False(t, validAuditEvent(AuditEvent{Outcome: "admitted", Reason: "admission_approved", Archive: "../etc/passwd"}))
	require.False(t, validAuditEvent(AuditEvent{Outcome: "denied", Reason: "peer_forbidden", Suppressed: -1}))
}

func TestAdvanceCounterRefusesARevokedCredential(t *testing.T) {
	store, verifier, adminID, authorizers, _ := newAuthorizationFixture(t)
	credential := testCredentials(t, authorizers, adminID)[0]
	_, err := authorizers.Revoke(adminID)
	require.NoError(t, err)
	require.ErrorIs(t, authorizers.AdvanceCounter(credential.ID, 9), errAuthorizerRevoked)
	list, err := authorizers.List()
	require.NoError(t, err)
	require.Equal(t, uint32(4), list[0].SignCount, "a revoked key's counter is not advanced")

	// Revocation racing the ceremony: Credentials() still saw the key, but the
	// revoke lands before the counter write.
	reimport := testAuthorizerImportFor(t, authorizers, adminID)
	_, err = authorizers.ImportMany(reimport, true)
	require.NoError(t, err)
	handle, _, err := store.Begin(adminID, "stop", actionRequest{})
	require.NoError(t, err)
	verifier.onFinish = func() {
		_, revokeErr := authorizers.Revoke(adminID)
		require.NoError(t, revokeErr)
	}
	_, err = store.Consume(handle, "stop", []byte(`{"assertion":true}`))
	require.ErrorIs(t, err, ErrAuthorizationDenied, "a revoke racing the ceremony denies it")
	require.NotErrorIs(t, err, ErrAuthorizerUnavailable)
}

func testAuthorizerImportFor(t *testing.T, store *AuthorizerStore, adminID string) AuthorizerImport {
	t.Helper()
	doc, err := store.read()
	require.NoError(t, err)
	entry := doc.Authorizers[0]
	return AuthorizerImport{
		Schema: 1, RPID: doc.RPID, Origins: doc.Origins, AdminID: adminID,
		Credentials: []AuthorizerCredentialImport{{CredentialID: entry.CredentialID, PublicKey: entry.PublicKey, SignCount: entry.SignCount, Label: entry.Label}},
	}
}

func rewriteTrustRP(t *testing.T, store *AuthorizerStore, rpID string, origins []string) {
	t.Helper()
	doc, err := store.read()
	require.NoError(t, err)
	doc.RPID, doc.Origins = rpID, origins
	require.NoError(t, validateAuthorizerFile(doc))
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, store.writeAtomic(data))
}

func TestLoginAssertionGuardsDenyWithoutAdvancingCounter(t *testing.T) {
	for _, scenario := range []string{"control", "wrong RP ID", "origin not trusted", "another admin", "UV not required", "wrong user handle"} {
		t.Run(scenario, func(t *testing.T) {
			server, store, adminID, session, assertion, _ := loginAssertionFixture(t)
			want := http.StatusForbidden
			switch scenario {
			case "control":
				want = http.StatusOK
			// These two change root's trust document, not the session, so the
			// WebAuthn library still accepts the assertion and only the agent's
			// own comparison against the root file can refuse it.
			case "wrong RP ID":
				rewriteTrustRP(t, store, "example.test", []string{"https://admin.example.test"})
			case "origin not trusted":
				rewriteTrustRP(t, store, "admin.example.test", []string{"https://login.admin.example.test"})
			case "another admin":
				adminID = uuid.NewString()
			case "UV not required":
				session.UserVerification = protocol.VerificationPreferred
			case "wrong user handle":
				session.UserID = []byte("someone-else")
			}
			require.Equal(t, want, postLoginAssertion(t, server, adminID, session, assertion))
			list, err := store.List()
			require.NoError(t, err)
			wantCount := uint32(4)
			if scenario == "control" {
				wantCount = 5
			}
			require.Equal(t, wantCount, list[0].SignCount)
		})
	}
}
