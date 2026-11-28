package hostagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// One key logs in and authorizes host actions, so the out-of-band alert is
// the only detection control for a harvested login tap: no alert, no write.
func TestWritesRequireAnAlertDestination(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := writeServer(t, runner)
	s.SetAlerter(nil)

	rec := postBegin(t, s, "start", `{}`)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "no key tap is asked for")
	require.JSONEq(t, `{"error":"alerts_not_configured"}`, rec.Body.String())
	rec = postAuthorized(t, s, "/v1/actions/start", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.JSONEq(t, `{"error":"alerts_not_configured"}`, rec.Body.String())
	require.Empty(t, runner.args, "nothing ran")
	require.Equal(t, http.StatusOK, get(t, s, http.MethodGet, "/v1/status").Code, "reads keep working")

	s.SetAlerter(testAlerter())
	job := started(t, postAuthorized(t, s, "/v1/actions/start", ""))
	require.Equal(t, "succeeded", waitJob(t, s, job.ID).State, "writes run once alerts are configured")
}

// The route gate is not the only guard: start itself refuses without an
// alerter, before anything is admitted.
func TestStartRefusesWithoutAnAlertDestination(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	s.writes.alerts = nil
	job, err := s.writes.start("stop", list[0].AdminID, actionRequest{}, [][]string{{"stop"}}, shortJobBudget)
	require.ErrorIs(t, err, ErrAlertNotConfigured)
	require.Nil(t, job)
	require.Empty(t, readAuditLines(t, s.audit.path), "nothing is admitted")
}

func beginHandle(t *testing.T, s *Server, action string) string {
	t.Helper()
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := s.authorizations.Begin(list[0].AdminID, action, actionRequest{})
	require.NoError(t, err)
	return handle
}

func postHandle(t *testing.T, s *Server, action, handle string) int {
	t.Helper()
	return post(t, s, "/v1/actions/"+action, fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle)).Code
}

// A write refused as busy keeps the admin's one-use approval.
func TestBusyRefusalDoesNotSpendTheHandle(t *testing.T) {
	f := &fakeRunner{wait: make(chan struct{})}
	s, _ := writeServer(t, f)
	first := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	handle := beginHandle(t, s, "start")
	require.Equal(t, http.StatusConflict, postHandle(t, s, "start", handle))
	close(f.wait)
	waitJob(t, s, first.ID)
	job := started(t, post(t, s, "/v1/actions/start", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle)))
	waitJob(t, s, job.ID)
	require.Equal(t, http.StatusForbidden, postHandle(t, s, "start", handle), "the handle is still single-use")
}

// A backup refused inside the window keeps the admin's approval too.
func TestBackupWindowRefusalDoesNotSpendTheHandle(t *testing.T) {
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	s, _ := writeServer(t, &fakeRunner{})
	s.writes.now = func() time.Time { return now }
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/backup", "")).ID)
	handle := beginHandle(t, s, "backup")
	require.Equal(t, http.StatusTooManyRequests, postHandle(t, s, "backup", handle))
	s.writes.now = func() time.Time { return now.Add(time.Hour) }
	waitJob(t, s, started(t, post(t, s, "/v1/actions/backup", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))).ID)
}

// The early check is advisory: a write admitted while the proof is checked
// is still refused at start, and the spent handle never works again.
func TestBusyStateIsRecheckedAfterTheProof(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	verifier, ok := s.authorizations.webauthn.(*fakeAgentWebAuthn)
	require.True(t, ok)
	verifier.onFinish = func() {
		s.writes.mu.Lock()
		s.writes.running = true
		s.writes.mu.Unlock()
	}
	handle := beginHandle(t, s, "start")
	require.Equal(t, http.StatusConflict, postHandle(t, s, "start", handle))
	verifier.onFinish = nil
	s.writes.mu.Lock()
	s.writes.running = false
	s.writes.mu.Unlock()
	require.Equal(t, http.StatusForbidden, postHandle(t, s, "start", handle), "a handle spent at start is not reusable")
}

// /run is a tmpfs: backup admission state kept there would hand out a fresh
// window after every reboot, so it lives beside the durable audit.
func TestBackupAdmissionSurvivesALostRuntimeDirectory(t *testing.T) {
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	s, _ := writeServer(t, &fakeRunner{})
	s.writes.now = func() time.Time { return now }
	require.Equal(t, filepath.Join(filepath.Dir(s.audit.path), backupAdmissionFile), s.writes.admission)
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/backup", "")).ID)

	// After a reboot: a new runtime directory, the same audit directory.
	rebooted := NewServer(&fakeRunner{out: statusJSON})
	rebooted.EnableWrites(&fakeRunner{}, t.TempDir(), filepath.Join(t.TempDir(), "write-in-progress"))
	rebooted.writes.boundary = s.writes.boundary
	rebooted.writes.version = s.writes.version
	rebooted.writes.now = s.writes.now
	audit, err := openAuditAt(s.audit.path, testOwnerUID(), s.audit.trustRoot)
	require.NoError(t, err)
	require.NoError(t, rebooted.SetAudit(audit))
	authorizations, _, _, _, _ := newAuthorizationFixture(t)
	require.NoError(t, rebooted.SetAuthorizationStore(authorizations))
	rebooted.SetAlerter(testAlerter())
	rec := postAuthorized(t, rebooted, "/v1/actions/backup", "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	// No audit, no admission state: a backup is refused, never unlimited.
	bare := NewServer(&fakeRunner{})
	bare.EnableWrites(&fakeRunner{}, t.TempDir(), filepath.Join(t.TempDir(), "write-in-progress"))
	require.Empty(t, bare.writes.admission)
	require.Error(t, bare.writes.checkBackupAdmission(now))
}

// Denials refused past the reserve are counted in one summary record, written
// ahead of the next record that fits.
func TestDenialsPastTheReserveAreSummarized(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	require.NoError(t, os.Truncate(s.audit.path, maxAuditDenialBytes))
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}), errAuditDenialReserve)
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied", Suppressed: 4}), errAuditDenialReserve)
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "peer_forbidden"}), errAuditDenialReserve)

	require.NoError(t, s.audit.Write(AuditEvent{Action: "stop", Outcome: "admitted", Reason: "admission_approved"}))
	require.NoError(t, s.audit.Write(AuditEvent{Action: "stop", Outcome: "started", Reason: "job_started"}))
	tail := readAuditTail(t, s.audit.path, 3)
	require.Equal(t, "denials_suppressed", tail[0]["reason_code"])
	require.Equal(t, "denied", tail[0]["outcome"])
	require.EqualValues(t, 7, tail[0]["suppressed_count"], "three refused records, one of which stood for four more")
	require.Equal(t, "admission_approved", tail[1]["reason_code"])
	require.Equal(t, "job_started", tail[2]["reason_code"], "the summary is written once")

	// After a rotation, the next denial carries a new summary first.
	require.ErrorIs(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}), errAuditDenialReserve)
	require.NoError(t, os.Truncate(s.audit.path, 0))
	require.NoError(t, s.audit.Write(AuditEvent{Outcome: "denied", Reason: "begin_denied"}))
	lines := readAuditLines(t, s.audit.path)
	require.Len(t, lines, 2)
	require.Equal(t, "denials_suppressed", lines[0]["reason_code"])
	require.EqualValues(t, 1, lines[0]["suppressed_count"])
	require.Equal(t, "begin_denied", lines[1]["reason_code"])
}

// readAuditTail decodes the last n records of a stream padded with NULs.
func readAuditTail(t *testing.T, path string, n int) []map[string]any {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // test-created audit stream
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(content, "\n"), []byte{'\n'})
	require.GreaterOrEqual(t, len(lines), n)
	records := make([]map[string]any, 0, n)
	for _, line := range lines[len(lines)-n:] {
		var record map[string]any
		require.NoError(t, json.Unmarshal(bytes.TrimLeft(line, "\x00"), &record))
		records = append(records, record)
	}
	return records
}
