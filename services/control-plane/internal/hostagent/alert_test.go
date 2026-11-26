package hostagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeAlertFile(t *testing.T, root, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(root, "agent-alert.json")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestAlertConfigAcceptsOnlyARootOnlyHTTPSFile(t *testing.T) {
	root := t.TempDir()
	path := writeAlertFile(t, root, `{"webhook_url":"https://alerts.example.test/hook/abc"}`, 0o600)
	got, err := loadAlertConfigAt(path, testOwnerUID(), root)
	require.NoError(t, err)
	require.Equal(t, "https://alerts.example.test/hook/abc", got)

	_, err = loadAlertConfigAt(filepath.Join(root, "missing.json"), testOwnerUID(), root)
	require.ErrorIs(t, err, ErrAlertNotConfigured, "no file means alerts are off, not an error")
	_, err = loadAlertConfigAt(filepath.Join(root, "absent-dir", "agent-alert.json"), testOwnerUID(), root)
	require.ErrorIs(t, err, ErrAlertNotConfigured, "no alert directory means alerts are off too")

	for name, content := range map[string]string{
		"http":          `{"webhook_url":"http://alerts.example.test/hook"}`,
		"no host":       `{"webhook_url":"https:///hook"}`,
		"relative":      `{"webhook_url":"/hook"}`,
		"empty":         `{"webhook_url":""}`,
		"unknown field": `{"webhook_url":"https://alerts.example.test/","token":"x"}`,
		"trailing":      `{"webhook_url":"https://alerts.example.test/"}{}`,
		"oversized":     `{"webhook_url":"https://alerts.example.test/` + strings.Repeat("a", maxAlertConfigBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeAlertFile(t, t.TempDir(), content, 0o600)
			_, err := loadAlertConfigAt(path, testOwnerUID(), filepath.Dir(path))
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrAlertNotConfigured)
		})
	}
}

func TestAlertConfigRefusesUnsafeOwnershipModeAndLinks(t *testing.T) {
	const good = `{"webhook_url":"https://alerts.example.test/hook"}`
	for _, mode := range []os.FileMode{0o644, 0o640, 0o620, 0o602} {
		root := t.TempDir()
		path := writeAlertFile(t, root, good, mode)
		_, err := loadAlertConfigAt(path, testOwnerUID(), root)
		require.ErrorContains(t, err, "unsafe alert file", "mode %o", mode)
	}

	root := t.TempDir()
	path := writeAlertFile(t, root, good, 0o600)
	_, err := loadAlertConfigAt(path, testOwnerUID()+1, root)
	require.ErrorContains(t, err, "unsafe alert", "a file owned by another user is refused")

	root = t.TempDir()
	target := writeAlertFile(t, root, good, 0o600)
	link := filepath.Join(root, "link.json")
	require.NoError(t, os.Symlink(target, link))
	_, err = loadAlertConfigAt(link, testOwnerUID(), root)
	require.Error(t, err, "O_NOFOLLOW refuses a symlinked alert file")

	root = t.TempDir()
	dir := filepath.Join(root, "etc")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path = writeAlertFile(t, dir, good, 0o600)
	require.NoError(t, os.Chmod(dir, 0o770)) //nolint:gosec // the unsafe mode under test
	_, err = loadAlertConfigAt(path, testOwnerUID(), root)
	require.ErrorContains(t, err, "unsafe alert directory")
}

type alertReceiver struct {
	server   *httptest.Server
	bodies   chan map[string]any
	failures atomic.Int32 // answer 500 to this many requests first
}

func newAlertReceiver(t *testing.T) *alertReceiver {
	t.Helper()
	r := &alertReceiver{bodies: make(chan map[string]any, 64)}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.failures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		r.bodies <- body
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *alertReceiver) alerter() *Alerter {
	a := newAlerter(r.server.URL+"/hook", "concord-test-host")
	a.client.Transport = r.server.Client().Transport
	a.retry = time.Millisecond
	return a
}

func (r *alertReceiver) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case body := <-r.bodies:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("no alert delivered")
		return nil
	}
}

func TestAlertSendCarriesTheActionAndRetriesOnce(t *testing.T) {
	receiver := newAlertReceiver(t)
	a := receiver.alerter()
	a.now = func() time.Time { return time.Date(2026, time.November, 26, 9, 30, 0, 0, time.UTC) }
	receiver.failures.Store(1)
	event := AlertEvent{Outcome: "admitted", Reason: "admission_approved", Action: "update", Tag: "v1.2.3", AdminID: "6f1d8c1e-2c5b-4c7e-9a7e-0f6d1b2a3c4d", JobID: strings.Repeat("a", 32)}
	require.NoError(t, a.Send(context.Background(), event), "one failure is retried")
	body := receiver.next(t)
	require.Equal(t, "admitted", body["event"])
	require.Equal(t, "update", body["action"])
	require.Equal(t, "v1.2.3", body["tag"])
	require.Equal(t, event.AdminID, body["admin_id"])
	require.Equal(t, event.JobID, body["job_id"])
	require.Equal(t, "concord-test-host", body["host"])
	require.Equal(t, "2026-11-26T09:30:00Z", body["time"])
	require.Contains(t, body["text"], "approved update to v1.2.3 for admin "+event.AdminID)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"text", "source", "event", "reason", "action", "tag", "admin_id", "job_id", "host", "time"}, keys, "no field beyond the fixed set")

	receiver.failures.Store(2)
	err := a.Send(context.Background(), event)
	require.Error(t, err, "two failures are not retried again")
	require.Equal(t, "status", AlertFailureClass(err))
}

func TestAlertTestEventSaysItIsATest(t *testing.T) {
	receiver := newAlertReceiver(t)
	require.NoError(t, receiver.alerter().Send(context.Background(), AlertEvent{Outcome: "test"}))
	body := receiver.next(t)
	require.Equal(t, "test", body["event"])
	require.Contains(t, body["text"], "test alert")
	require.NotContains(t, body, "action")
}

func TestAlertDeliveryFailureIsLoggedByClassOnly(t *testing.T) {
	logs := captureLog(t)
	receiver := newAlertReceiver(t)
	receiver.failures.Store(100)
	a := receiver.alerter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.worker(ctx)
	a.Enqueue(AlertEvent{Outcome: "denied", Reason: "begin_denied"})
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "alert delivery failed: class=status") }, 5*time.Second, 10*time.Millisecond)
	require.NotContains(t, logs.String(), receiver.server.URL, "the URL never reaches the journal")
}

func TestAlertQueueIsBoundedAndNeverBlocks(t *testing.T) {
	logs := captureLog(t)
	a := newAlerter("https://alerts.example.test/hook", "host") // no worker: nothing drains
	done := make(chan struct{})
	go func() {
		for range alertQueueSize * 4 {
			a.Enqueue(AlertEvent{Outcome: "denied", Reason: "begin_denied"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
	require.Len(t, a.queue, alertQueueSize)
	require.Equal(t, 1, strings.Count(logs.String(), "class=queue_full"), "drops are reported once per window")
}

func serverWithAlerts(t *testing.T, runner *fakeRunner) (*Server, *alertReceiver) {
	t.Helper()
	s, _ := writeServer(t, runner)
	receiver := newAlertReceiver(t)
	a := receiver.alerter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.worker(ctx)
	s.SetAlerter(a)
	return s, receiver
}

func TestAdmittedActionIsAlertedAfterItsAuditRecord(t *testing.T) {
	s, receiver := serverWithAlerts(t, &fakeRunner{})
	job := started(t, postAuthorized(t, s, "/v1/actions/update", `{"tag":"v1.2.3"}`))
	body := receiver.next(t)
	require.Equal(t, "admitted", body["event"])
	require.Equal(t, "update", body["action"])
	require.Equal(t, "v1.2.3", body["tag"])
	require.Equal(t, job.ID, body["job_id"])
	admitted := false
	for _, line := range readAuditLines(t, s.audit.path) {
		if line["outcome"] == "admitted" && line["job_id"] == job.ID {
			admitted = true
		}
	}
	require.True(t, admitted, "the admitted record exists when the alert arrives")
	waitJob(t, s, job.ID)
}

func TestAlertFailureNeverBlocksTheAction(t *testing.T) {
	runner := &fakeRunner{}
	s, receiver := serverWithAlerts(t, runner)
	receiver.failures.Store(1000)
	job := started(t, postAuthorized(t, s, "/v1/actions/stop", ""))
	require.Equal(t, "succeeded", waitJob(t, s, job.ID).State)
	require.Equal(t, [][]string{{"stop"}}, runner.args)
}

func TestAuditFailureSendsNoAdmittedAlert(t *testing.T) {
	runner := &fakeRunner{}
	s, receiver := serverWithAlerts(t, runner)
	require.NoError(t, os.Truncate(s.audit.path, maxAuditFileBytes))
	rec := postAuthorized(t, s, "/v1/actions/stop", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	select {
	case body := <-receiver.bodies:
		require.NotEqual(t, "admitted", body["event"], "no admitted alert without an admitted record")
	case <-time.After(200 * time.Millisecond):
	}
	require.Empty(t, runner.args)
}

func TestDenialAlertsAreCoalesced(t *testing.T) {
	s, receiver := serverWithAlerts(t, &fakeRunner{})
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	s.denials.now = func() time.Time { return now }
	for range 50 {
		rec := post(t, s, "/v1/authorizations", `not json`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	body := receiver.next(t)
	require.Equal(t, "denied", body["event"])
	require.Equal(t, "begin_denied", body["reason"])
	select {
	case extra := <-receiver.bodies:
		t.Fatalf("a second alert inside the window: %v", extra)
	case <-time.After(200 * time.Millisecond):
	}
	now = now.Add(denialWindow)
	post(t, s, "/v1/authorizations", `not json`)
	body = receiver.next(t)
	require.InDelta(t, 49, body["suppressed_count"], 0, "the next alert counts the denials it stood for")
}
