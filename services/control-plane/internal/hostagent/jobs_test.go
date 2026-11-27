package hostagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func post(t *testing.T, s *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	s.ServeHTTP(rec, req.WithContext(withPeer(req.Context(), Peer{UID: 10006})))
	return rec
}

func writeServer(t *testing.T, f *fakeRunner) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewServer(&fakeRunner{out: statusJSON})
	s.EnableWrites(f, dir, filepath.Join(t.TempDir(), "write-in-progress"))
	s.writes.boundary = t.TempDir()
	s.writes.version = writeVersionFile(t, "0.1.0")
	auditRoot := t.TempDir()
	audit, err := openAuditAt(filepath.Join(auditRoot, "host-agent", "events.jsonl"), testOwnerUID(), auditRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAudit(audit); err != nil {
		t.Fatal(err)
	}
	authorizations, _, _, _, _ := newAuthorizationFixture(t)
	if err := s.SetAuthorizationStore(authorizations); err != nil {
		t.Fatal(err)
	}
	s.SetAlerter(testAlerter())
	return s, dir
}

// testAlerter is a configured destination with no worker: alerts queue and
// are dropped when the queue is full, which never blocks a write.
func testAlerter() *Alerter {
	return newAlerter("https://alerts.example.test/hook", "concord-test-host")
}

func postAuthorized(t *testing.T, s *Server, target, arguments string) *httptest.ResponseRecorder {
	t.Helper()
	var req actionRequest
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &req); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.authorizations.authorizers.List()
	if err != nil || len(list) == 0 {
		t.Fatalf("authorizer fixture: %v, %d entries", err, len(list))
	}
	action := strings.TrimPrefix(target, "/v1/actions/")
	handle, _, err := s.authorizations.Begin(list[0].AdminID, action, req)
	if err != nil {
		t.Fatalf("begin %s: %v", action, err)
	}
	return post(t, s, target, fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
}

func postBegin(t *testing.T, s *Server, action, arguments string) *httptest.ResponseRecorder {
	t.Helper()
	list, err := s.authorizations.authorizers.List()
	if err != nil || len(list) == 0 {
		t.Fatalf("authorizer fixture: %v, %d entries", err, len(list))
	}
	return post(t, s, "/v1/authorizations", fmt.Sprintf(`{"admin_id":%q,"action":%q,"arguments":%s}`, list[0].AdminID, action, arguments))
}

// waitJob polls a job until it finishes.
func waitJob(t *testing.T, s *Server, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := get(t, s, http.MethodGet, "/v1/jobs/"+id)
		var j Job
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatalf("job %s: %d %s", id, rec.Code, rec.Body)
		}
		if j.State != "running" {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func started(t *testing.T, rec *httptest.ResponseRecorder) Job {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var j Job
	// The worker may finish before the accepted response is decoded.
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || !jobIDPattern.MatchString(j.ID) ||
		(j.State != "running" && j.State != "succeeded" && j.State != "failed") {
		t.Fatalf("bad job %s", rec.Body)
	}
	return j
}

func TestWritesOffByDefault(t *testing.T) {
	s := NewServer(&fakeRunner{})
	for _, p := range []string{"/v1/actions/start", "/v1/jobs"} {
		if rec := post(t, s, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", p, rec.Code)
		}
	}
}

func TestAllSixWritesRequireAgentProofAndStoredArguments(t *testing.T) {
	f := &fakeRunner{}
	s, _ := writeServer(t, f)
	for _, action := range []string{"start", "stop", "restart", "backup", "update", "restore"} {
		for _, legacy := range []string{"", `{}`, `{"tag":"v1.2.3"}`, `{"archive":"concord-backup-20261106T071500Z.tar.gz"}`} {
			response := post(t, s, "/v1/actions/"+action, legacy)
			if response.Code != http.StatusBadRequest {
				t.Errorf("%s accepted legacy body %q: %d %s", action, legacy, response.Code, response.Body)
			}
		}
	}
	if len(f.args) != 0 {
		t.Fatalf("legacy requests ran %v", f.args)
	}
	list, err := s.authorizations.authorizers.List()
	if err != nil || len(list) == 0 {
		t.Fatalf("authorizer fixture: %v, %d entries", err, len(list))
	}
	handle, _, err := s.authorizations.Begin(list[0].AdminID, "update", actionRequest{Tag: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if response := post(t, s, "/v1/actions/update", fmt.Sprintf(`{"handle":%q,"assertion":{},"tag":"v9.9.9"}`, handle)); response.Code != http.StatusBadRequest {
		t.Fatalf("final request accepted caller tag: %d %s", response.Code, response.Body)
	}
	response := post(t, s, "/v1/actions/update", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
	job := started(t, response)
	waitJob(t, s, job.ID)
	if !reflect.DeepEqual(f.args, [][]string{{"update", "--to", "v1.2.3", "--confirm", "--json"}}) {
		t.Fatalf("ran %v; must use the args bound at begin", f.args)
	}
	// Past the update's backup window, so the replay reaches the handle check.
	s.writes.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if response := post(t, s, "/v1/actions/update", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle)); response.Code != http.StatusForbidden {
		t.Fatalf("replayed handle: %d %s", response.Code, response.Body)
	}
}

func TestWriteActionsRunTheirCommands(t *testing.T) {
	cases := []struct {
		action, body string
		want         [][]string
	}{
		{"start", "", [][]string{{"up"}}},
		{"stop", "", [][]string{{"stop"}}},
		{"restart", "{}", [][]string{{"stop"}, {"up"}}},
		{"backup", "", [][]string{{"backup", "--json"}}},
		{"update", `{"tag":"v1.2.3-rc.1"}`, [][]string{{"update", "--to", "v1.2.3-rc.1", "--confirm", "--json"}}},
	}
	for _, c := range cases {
		f := &fakeRunner{}
		s, _ := writeServer(t, f)
		j := waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/"+c.action, c.body)).ID)
		if j.State != "succeeded" || j.Action != c.action || j.Finished == nil {
			t.Errorf("%s: %+v", c.action, j)
		}
		if !reflect.DeepEqual(f.args, c.want) {
			t.Errorf("%s: ran %v", c.action, f.args)
		}
	}
}

func TestRestoreTakesOnlyAnArchiveInBackups(t *testing.T) {
	f := &fakeRunner{}
	s, dir := writeServer(t, f)
	name := "concord-backup-20261106T071500Z.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, name), filepath.Join(dir, "concord-backup-20261106T071501Z.tar.gz")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"archive":"../concord-backup-20261106T071500Z.tar.gz"}`,
		`{"archive":"/etc/passwd"}`,
		`{}`,
		`{"archive":"` + name + `","tag":"v1.0.0"}`,
	} {
		rec := postBegin(t, s, "restore", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_archive") {
			t.Errorf("%s: got %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, missing := range []string{"concord-backup-20261106T071502Z.tar.gz", "concord-backup-20261106T071501Z.tar.gz"} {
		rec := postBegin(t, s, "restore", `{"archive":"`+missing+`"}`) // absent, then a link
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown_archive") {
			t.Errorf("%s: got %d %s", missing, rec.Code, rec.Body)
		}
	}
	// A backups folder others can write to is refused outright.
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // deliberately unsafe input
		t.Fatal(err)
	}
	if rec := postBegin(t, s, "restore", `{"archive":"`+name+`"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "untrusted_backups") {
		t.Errorf("writable backups: got %d %s", rec.Code, rec.Body)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // root-only directory, as the agent expects
		t.Fatal(err)
	}
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/restore", `{"archive":"`+name+`"}`)).ID)
	want := [][]string{{"restore", filepath.Join(dir, name), "--confirm", "--json"}}
	if !reflect.DeepEqual(f.args, want) {
		t.Errorf("ran %v", f.args)
	}
}

func TestRestoreRechecksArchiveAfterAuthorization(t *testing.T) {
	runner := &fakeRunner{}
	s, dir := writeServer(t, runner)
	name := "concord-backup-20261106T071500Z.tar.gz"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("backup"), 0o600))
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	handle, _, err := s.authorizations.Begin(list[0].AdminID, "restore", actionRequest{Archive: name})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	rec := post(t, s, "/v1/actions/restore", fmt.Sprintf(`{"handle":%q,"assertion":{"test":true}}`, handle))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "unknown_archive")
	require.Empty(t, runner.args)
}

func TestRefusesBadWriteRequests(t *testing.T) {
	f := &fakeRunner{}
	s, _ := writeServer(t, f)
	for _, c := range []struct {
		action, body string
		code         int
		reason       string
	}{
		{"reboot", "", http.StatusNotFound, "unknown_action"},
		{"start/x", "", http.StatusNotFound, "unknown_action"},
		{"start", `{"tag":"v1.0.0"}`, http.StatusBadRequest, "invalid_body"},
		{"start", `{"cmd":"rm"}`, http.StatusBadRequest, "invalid_body"},
		{"start", `{}{}`, http.StatusBadRequest, "invalid_body"},
		{"start", `{"tag":"` + strings.Repeat("a", 2000) + `"}`, http.StatusBadRequest, "invalid_body"},
		{"update", `{"tag":"main"}`, http.StatusBadRequest, "invalid_body"},
		{"update", `{"tag":"v1.0.0;id"}`, http.StatusBadRequest, "invalid_body"},
		{"update", `{"tag":"--help"}`, http.StatusBadRequest, "invalid_body"},
	} {
		rec := post(t, s, "/v1/actions/"+c.action, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.reason) {
			t.Errorf("%s %s: got %d %s", c.action, c.body, rec.Code, rec.Body)
		}
	}
	for _, arguments := range []string{`{"tag":"main"}`, `{"tag":"v1.0.0;id"}`, `{"tag":"--help"}`} {
		if rec := postBegin(t, s, "update", arguments); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_tag") {
			t.Errorf("invalid update begin %s: got %d %s", arguments, rec.Code, rec.Body)
		}
	}
	if rec := get(t, s, http.MethodGet, "/v1/actions/start"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET action: got %d", rec.Code)
	}
	if rec := post(t, s, "/v1/jobs", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST jobs: got %d", rec.Code)
	}
	if len(f.args) != 0 {
		t.Errorf("refused requests ran %v", f.args)
	}
}

func TestRestartChecksTheLogBoundaryFirst(t *testing.T) {
	f := &fakeRunner{}
	s, _ := writeServer(t, f)
	s.writes.boundary = filepath.Join(t.TempDir(), "missing")
	if rec := postBegin(t, s, "restart", `{}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "security_log_boundary_missing") {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if len(f.args) != 0 {
		t.Errorf("ran %v", f.args)
	}
}

func TestMarkerExistsOnlyWhileAWriteRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "write-in-progress")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{wait: make(chan struct{})}
	s := NewServer(&fakeRunner{})
	s.EnableWrites(f, t.TempDir(), marker)
	s.SetAlerter(testAlerter())
	auditRoot := t.TempDir()
	audit, err := openAuditAt(filepath.Join(auditRoot, "host-agent", "events.jsonl"), testOwnerUID(), auditRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAudit(audit); err != nil {
		t.Fatal(err)
	}
	authorizations, _, _, _, _ := newAuthorizationFixture(t)
	if err := s.SetAuthorizationStore(authorizations); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale marker kept: %v", err)
	}
	j := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	b, err := os.ReadFile(marker) //nolint:gosec // temp path from the test
	if err != nil || strings.TrimSpace(string(b)) != j.ID {
		t.Errorf("marker during the run: %q %v", b, err)
	}
	close(f.wait)
	waitJob(t, s, j.ID)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker after the run: %v", err)
	}
	// No marker, no job.
	s.writes.marker = filepath.Join(t.TempDir(), "gone", "write-in-progress")
	if rec := postAuthorized(t, s, "/v1/actions/stop", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwritable marker: got %d", rec.Code)
	}
}

func TestBackupAdmissionPersistsAcrossFailureAndRestart(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	s, _ := writeServer(t, &fakeRunner{err: errors.New("backup failed")})
	s.writes.now = func() time.Time { return now }
	first := started(t, postAuthorized(t, s, "/v1/actions/backup", ""))
	if job := waitJob(t, s, first.ID); job.State != "failed" {
		t.Fatalf("failed backup state = %q", job.State)
	}
	if rec := postAuthorized(t, s, "/v1/actions/backup", ""); rec.Code != http.StatusTooManyRequests ||
		!strings.Contains(rec.Body.String(), "backup_rate_limited") {
		t.Fatalf("second backup: %d %s", rec.Code, rec.Body)
	}
	if lines := readAuditLines(t, s.audit.path); len(lines) != 4 || lines[len(lines)-1]["reason_code"] != "backup_rate_limited" || lines[len(lines)-1]["outcome"] != "denied" {
		t.Fatalf("backup limit audit: %#v", lines)
	}

	s2, _ := writeServer(t, &fakeRunner{})
	s2.writes.admission = s.writes.admission
	s2.writes.now = func() time.Time { return now }
	if rec := postAuthorized(t, s2, "/v1/actions/backup", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("restart backup: %d %s", rec.Code, rec.Body)
	}
	s2.writes.now = func() time.Time { return now.Add(time.Hour - time.Nanosecond) }
	if rec := postAuthorized(t, s2, "/v1/actions/backup", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("backup 1ns before the hour: %d %s", rec.Code, rec.Body)
	}
	// A literal hour, so a change to backupWindow cannot move both sides.
	s2.writes.now = func() time.Time { return now.Add(time.Hour) }
	if job := started(t, postAuthorized(t, s2, "/v1/actions/backup", "")); waitJob(t, s2, job.ID).State != "succeeded" {
		t.Fatalf("backup at window boundary did not succeed")
	}
}

func TestBackupAdmissionDoesNotSpendWindowWithoutAJob(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	for _, failure := range []string{"audit", "marker"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := writeServer(t, &fakeRunner{})
			s.writes.now = func() time.Time { return now }
			list, err := s.authorizations.authorizers.List()
			require.NoError(t, err)
			require.Len(t, list, 1)
			marker := s.writes.marker
			if failure == "audit" {
				s.audit.syncFile = func(*os.File) error { return errors.New("injected audit failure") }
			} else {
				s.writes.marker = filepath.Join(t.TempDir(), "missing", "write-in-progress")
			}
			job, err := s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
			require.Error(t, err)
			require.Nil(t, job)
			_, err = os.Stat(s.writes.admission)
			require.ErrorIs(t, err, os.ErrNotExist, "a rejected backup must not consume the hourly slot")
			require.Empty(t, s.writes.list)
			if failure == "marker" {
				lines := readAuditLines(t, s.audit.path)
				require.Len(t, lines, 2)
				require.Equal(t, "admission_failed", lines[1]["reason_code"], "a rolled-back admission is closed in the audit")
				require.Equal(t, lines[0]["job_id"], lines[1]["job_id"])
			}

			if failure == "marker" {
				s.writes.marker = marker
			} else {
				replacement, _ := writeServer(t, &fakeRunner{})
				replacement.writes.admission = s.writes.admission
				replacement.writes.now = s.writes.now
				s = replacement
			}
			job, err = s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
			require.NoError(t, err)
			require.NotNil(t, job)
			waitJob(t, s, job.ID)
		})
	}
}

func TestBackupAdmissionCommitFailureRemovesMarker(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	s.writes.now = func() time.Time { return now }
	list, err := s.authorizations.authorizers.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	changed := false
	s.audit.syncFile = func(f *os.File) error {
		// The state changes after the eligibility check, at the admitted audit.
		if !changed {
			changed = true
			if err := os.Mkdir(s.writes.admission, 0o700); err != nil {
				return err
			}
		}
		return f.Sync()
	}
	job, err := s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
	require.ErrorContains(t, err, "publish backup admission state")
	require.Nil(t, job)
	lines := readAuditLines(t, s.audit.path)
	require.Len(t, lines, 2, "the rolled-back admission gets a terminal record")
	require.Equal(t, "admission_approved", lines[0]["reason_code"])
	require.Equal(t, "failed", lines[1]["outcome"])
	require.Equal(t, "admission_failed", lines[1]["reason_code"])
	require.Equal(t, lines[0]["job_id"], lines[1]["job_id"])
	require.Empty(t, s.writes.list)
	_, err = os.Stat(s.writes.marker)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.Remove(s.writes.admission))
	s.audit.syncFile = func(f *os.File) error { return f.Sync() }
	job, err = s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
	require.NoError(t, err)
	require.NotNil(t, job)
	waitJob(t, s, job.ID)
}

func TestBackupAdmissionAuditFailurePreventsLimitResponse(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{})
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	s.writes.now = func() time.Time { return now }
	first := started(t, postAuthorized(t, s, "/v1/actions/backup", ""))
	waitJob(t, s, first.ID)
	s.audit.syncFile = func(*os.File) error { return errors.New("injected audit failure") }
	if rec := postAuthorized(t, s, "/v1/actions/backup", ""); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "job_unavailable") {
		t.Fatalf("limited backup with audit failure: %d %s", rec.Code, rec.Body)
	}
	if len(s.writes.list) != 1 {
		t.Fatalf("audit failure started another job: %#v", s.writes.list)
	}
}

func TestBackupAdmissionRejectsInvalidAndUntrustedState(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	s, _ := writeServer(t, &fakeRunner{})
	require.NoError(t, os.WriteFile(s.writes.admission, []byte("-1\n"), 0o600))
	require.Error(t, s.writes.checkBackupAdmission(now))

	s, _ = writeServer(t, &fakeRunner{})
	target := filepath.Join(t.TempDir(), "admission-target")
	require.NoError(t, os.WriteFile(target, []byte("0\n"), 0o600))
	require.NoError(t, os.Symlink(target, s.writes.admission))
	require.Error(t, s.writes.checkBackupAdmission(now))
}

func TestBackupAdmissionRefusesUnsafeDirectoryAndStateType(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	s, _ := writeServer(t, &fakeRunner{})
	dir := filepath.Dir(s.writes.admission)
	require.NoError(t, os.Chmod(dir, 0o777)) //nolint:gosec // deliberately unsafe fixture
	require.ErrorContains(t, s.writes.checkBackupAdmission(now), "backup admission directory")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // restore test directory permissions
	require.NoError(t, os.Mkdir(s.writes.admission, 0o700))
	require.ErrorContains(t, s.writes.checkBackupAdmission(now), "backup admission state is untrusted")
}

func TestBackupAdmissionConcurrent(t *testing.T) {
	f := &fakeRunner{wait: make(chan struct{})}
	s, _ := writeServer(t, f)
	s.writes.now = func() time.Time { return time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC) }
	list, err := s.authorizations.authorizers.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("authorizer fixture: %v, %d entries", err, len(list))
	}
	start := make(chan struct{})
	type result struct {
		job *Job
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			job, err := s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
			results <- result{job: job, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if (first.job == nil) == (second.job == nil) || first.err != nil || second.err != nil {
		t.Fatalf("concurrent admissions: job=%v err=%v, job=%v err=%v", first.job, first.err, second.job, second.err)
	}
	close(f.wait)
	if first.job != nil {
		waitJob(t, s, first.job.ID)
	} else {
		waitJob(t, s, second.job.ID)
	}

	// With nothing running, the one-write guard is out of the way: concurrent
	// admissions inside the window are refused by the rate limit alone.
	start = make(chan struct{})
	results = make(chan result, 4)
	for range 4 {
		go func() {
			<-start
			job, err := s.writes.start("backup", list[0].AdminID, actionRequest{}, [][]string{{"backup", jsonFlag}}, longJobBudget)
			results <- result{job: job, err: err}
		}()
	}
	close(start)
	for range 4 {
		r := <-results
		if r.job != nil || !errors.Is(r.err, errBackupLimited) {
			t.Fatalf("admission inside the window: job=%v err=%v", r.job, r.err)
		}
	}
}

func TestOneWriteAtATime(t *testing.T) {
	f := &fakeRunner{wait: make(chan struct{})}
	s, _ := writeServer(t, f)
	first := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	if rec := postAuthorized(t, s, "/v1/actions/start", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second write: got %d", rec.Code)
	}
	// Reads still work while a write runs.
	if rec := get(t, s, http.MethodGet, "/v1/status"); rec.Code != http.StatusOK {
		t.Errorf("status during a write: got %d", rec.Code)
	}
	close(f.wait)
	waitJob(t, s, first.ID)
	waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/start", "")).ID)
}

func TestFailedStepStopsTheJob(t *testing.T) {
	f := &fakeRunner{out: "\x1b[31mstopping\x1b[0m\n", err: errors.New("exit 1")}
	s, _ := writeServer(t, f)
	j := waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/restart", "")).ID)
	if j.State != "failed" || len(f.args) != 1 {
		t.Errorf("state %s, ran %v", j.State, f.args)
	}
	if j.Output != "stopping\n[concord-agent: restart failed: the command could not run or was stopped]\n" {
		t.Errorf("output %q", j.Output)
	}
	s, _ = writeServer(t, &fakeRunner{err: exitErr(3)})
	if j := waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/stop", "")).ID); j.Output != "[concord-agent: stop failed: exit status 3]\n" {
		t.Errorf("exit output %q", j.Output)
	}
	s, _ = writeServer(t, &fakeRunner{})
	if j := waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/stop", "")).ID); j.Output != "" {
		t.Errorf("no output: %q", j.Output)
	}
}

func TestBackupJobReportsItsArchive(t *testing.T) {
	for out, want := range map[string]string{
		`{"schema":1,"ok":true,"archive":"concord-backup-20261106T071500Z.tar.gz"}` + "\nwarning: late stderr\n": "concord-backup-20261106T071500Z.tar.gz",
		`{"schema":1,"ok":false}` + "\n": "",
		"dumping\n" + `{"schema":1,"ok":true,"archive":"concord-backup-20261106T071500Z.tar.gz"}` + "\n": "concord-backup-20261106T071500Z.tar.gz",
		`{"schema":1,"ok":true,"archive":"../x.tar.gz"}` + "\n":                                          "",
		"no json\n": "",
	} {
		s, _ := writeServer(t, &fakeRunner{out: out})
		if j := waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/backup", "")).ID); j.Archive != want {
			t.Errorf("%q: archive %q", out, j.Archive)
		}
	}
}

func TestJobsList(t *testing.T) {
	s, _ := writeServer(t, &fakeRunner{out: "x\n"})
	var ids []string
	for i := 0; i < maxJobs+2; i++ {
		ids = append(ids, waitJob(t, s, started(t, postAuthorized(t, s, "/v1/actions/stop", "")).ID).ID)
	}
	var doc struct{ Jobs []Job }
	rec := get(t, s, http.MethodGet, "/v1/jobs")
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Jobs) != maxJobs || doc.Jobs[0].ID != ids[len(ids)-1] || doc.Jobs[0].Output != "" {
		t.Errorf("list: %d jobs, first %+v", len(doc.Jobs), doc.Jobs[0])
	}
	for _, id := range []string{ids[0], "nope", strings.Repeat("A", 32), ""} {
		if rec := get(t, s, http.MethodGet, "/v1/jobs/"+id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", id, rec.Code)
		}
	}
}

func TestTailBufferKeepsWholeLines(t *testing.T) {
	tb := tailBuffer{max: 10}
	_, _ = tb.Write([]byte("aaaaaa\nbbb\ncc\n"))
	if got := tb.String(); got != "bbb\ncc\n" {
		t.Errorf("got %q", got)
	}
}
