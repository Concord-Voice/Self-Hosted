package hostagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func testAudit(t *testing.T) *Audit {
	t.Helper()
	root := t.TempDir()
	audit, err := openAuditAt(filepath.Join(root, "host-agent", "events.jsonl"), testOwnerUID(), root)
	require.NoError(t, err)
	return audit
}

func readAuditLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // test-created audit stream
	require.NoError(t, err)
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		lines = append(lines, record)
	}
	return lines
}

func TestAuditWritesBoundedDurableClosedRecords(t *testing.T) {
	audit := testAudit(t)
	info, err := os.Stat(audit.path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	adminID := uuid.NewString()
	jobID := strings.Repeat("a", 32)
	require.NoError(t, audit.Write(AuditEvent{Action: "update", Outcome: "admitted", Reason: "admission_approved", AdminID: adminID, JobID: jobID}))
	lines := readAuditLines(t, audit.path)
	require.Len(t, lines, 1)
	require.Equal(t, "host-agent-audit.v1", lines[0]["schema_version"])
	require.Equal(t, "host-agent", lines[0]["service"])
	require.Equal(t, "privileged_action", lines[0]["event_type"])
	require.Equal(t, adminID, lines[0]["admin_id"])
	require.Equal(t, jobID, lines[0]["job_id"])
	for _, forbidden := range []string{"assertion", "credential_id", "challenge", "archive", "tag", "password"} {
		require.NotContains(t, string(mustJSON(t, lines)), forbidden)
	}
	for _, invalid := range []AuditEvent{
		{Action: "reboot", Outcome: "denied", Reason: "begin_denied"},
		{Action: "start", Outcome: "denied", Reason: "client_reason"},
		{Action: "start", Outcome: "denied", Reason: "begin_denied", AdminID: "raw-user"},
		{Action: "start", Outcome: "admitted", Reason: "admission_approved", JobID: "raw-job"},
	} {
		require.Error(t, audit.Write(invalid))
	}
	require.Len(t, readAuditLines(t, audit.path), 1)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestAuditRejectsUnsafePathAndFullStream(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "host-agent", "events.jsonl")
	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(root, "outside"), path))
	_, err := openAuditAt(path, testOwnerUID(), root)
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, nil, 0o644)) //nolint:gosec // deliberately unsafe fixture
	_, err = openAuditAt(path, testOwnerUID(), root)
	require.ErrorContains(t, err, "unsafe host-agent audit file")
	require.NoError(t, os.Chmod(path, 0o600)) //nolint:gosec // restore fixture
	audit, err := openAuditAt(path, testOwnerUID(), root)
	require.NoError(t, err)
	_, err = openAuditAt(path, testOwnerUID()+1, root)
	require.Error(t, err, "a different file owner cannot open the audit")
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // unsafe fixture
	require.Error(t, audit.Write(AuditEvent{Outcome: "denied", Reason: "peer_forbidden"}))
	require.NoError(t, os.Chmod(path, 0o600)) //nolint:gosec // restore fixture
	require.NoError(t, os.Truncate(path, maxAuditFileBytes))
	require.Error(t, audit.Write(AuditEvent{Outcome: "denied", Reason: "peer_forbidden"}))
}

func TestAuditRefusesBoundaryChangesAfterOpen(t *testing.T) {
	audit := testAudit(t)
	event := AuditEvent{Outcome: "denied", Reason: "peer_forbidden"}
	parent := filepath.Dir(audit.path)
	require.NoError(t, os.Chmod(parent, 0o777)) //nolint:gosec // deliberately unsafe fixture
	require.ErrorContains(t, audit.Write(event), "unsafe host-agent audit directory")
	require.NoError(t, os.Chmod(parent, 0o700)) //nolint:gosec // restore test directory permissions

	target := filepath.Join(t.TempDir(), "other-events.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("untouched\n"), 0o600))
	require.NoError(t, os.Remove(audit.path))
	require.NoError(t, os.Symlink(target, audit.path))
	require.ErrorContains(t, audit.Write(event), "open host-agent audit")
	content, err := os.ReadFile(target) //nolint:gosec // test-created target
	require.NoError(t, err)
	require.Equal(t, "untouched\n", string(content))
}

func TestAuditPublicSetupAndIdentityValidationFailClosed(t *testing.T) {
	_, err := OpenAudit(filepath.Join(t.TempDir(), "events.jsonl"))
	require.Error(t, err)
	_, err = openAuditAt("relative/events.jsonl", testOwnerUID(), t.TempDir())
	require.ErrorContains(t, err, "invalid host-agent audit path")

	id := uuid.NewString()
	require.Equal(t, id, canonicalAuditAdminID(id))
	require.Empty(t, canonicalAuditAdminID("not-a-uuid"))
	require.False(t, validAuditEvent(AuditEvent{Action: "start/extra", Outcome: "denied", Reason: "peer_forbidden"}))
	require.False(t, validAuditEvent(AuditEvent{Outcome: "unknown", Reason: "peer_forbidden"}))
	require.False(t, validAuditEvent(AuditEvent{Outcome: "denied", Reason: "client_reason"}))
}

func TestAuditUnavailableDisablesWritesBeforeProof(t *testing.T) {
	runner := &fakeRunner{}
	server := NewServer(&fakeRunner{})
	server.EnableWrites(runner, t.TempDir(), filepath.Join(t.TempDir(), "write-in-progress"))
	authorizations, _, _, _, _ := newAuthorizationFixture(t)
	require.NoError(t, server.SetAuthorizationStore(authorizations))
	response := post(t, server, "/v1/actions/start", `{"handle":"anything","assertion":{}}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Empty(t, runner.args)
	require.Empty(t, server.writes.list)
}

func TestAuditFailurePreventsJobMarkerAndRunner(t *testing.T) {
	runner := &fakeRunner{}
	server, _ := writeServer(t, runner)
	server.audit.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	response := postAuthorized(t, server, "/v1/actions/start", "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Empty(t, runner.args)
	_, err := os.Stat(server.writes.marker)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, server.writes.list)
	require.Error(t, server.audit.Write(AuditEvent{Outcome: "denied", Reason: "peer_forbidden"}), "failed fsync leaves audit disabled")
}

func TestAuditRecordsPeerBeginReplayAndJobOutcomes(t *testing.T) {
	runner := &fakeRunner{}
	server, _ := writeServer(t, runner)
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
	response = postBegin(t, server, "reboot", `{}`)
	require.Equal(t, http.StatusForbidden, response.Code)
	list, err := server.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := server.authorizations.Begin(list[0].AdminID, "stop", actionRequest{})
	require.NoError(t, err)
	proof := `{"handle":"` + handle + `","assertion":{"test":true}}`
	job := started(t, post(t, server, "/v1/actions/stop", proof))
	waitJob(t, server, job.ID)
	response = post(t, server, "/v1/actions/stop", proof)
	require.Equal(t, http.StatusForbidden, response.Code)
	lines := readAuditLines(t, server.audit.path)
	reasons := make([]string, 0, len(lines))
	for _, line := range lines {
		reason, ok := line["reason_code"].(string)
		require.True(t, ok)
		reasons = append(reasons, reason)
	}
	for _, want := range []string{"peer_forbidden", "begin_denied", "admission_approved", "job_started", "job_succeeded", "replay_denied"} {
		require.Contains(t, reasons, want)
	}
}
