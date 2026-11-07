package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const testJobID = "0123456789abcdef0123456789abcdef"

type agentCall struct{ path, body string }

// recordingAgent answers every request with status and body and keeps what
// it was asked.
func recordingAgent(t *testing.T, status int, body string) (string, func() []agentCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []agentCall
	sock := fakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, agentCall{r.Method + " " + r.URL.Path, string(b)})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return sock, func() []agentCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]agentCall(nil), calls...)
	}
}

func writeEngine(sock string, audit *fakeAudit) *gin.Engine {
	gin.SetMode(gin.TestMode)
	log := logger.NewWithWriter(&bytes.Buffer{})
	h := &Handler{server: newArea(sock, audit), log: log}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(adminIDContextKey, "admin-1"); c.Next() })
	r.POST("/server/actions/:action", h.ServerAction)
	r.POST("/server/stepup", h.ServerStepUpBegin)
	r.GET("/server/jobs", h.server.ServerJobs)
	r.GET("/server/jobs/:id", h.server.ServerJob)
	return r
}

func send(r *gin.Engine, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)))
	return rec
}

func TestServerActionStartsJobsWithoutStepUp(t *testing.T) {
	job := `{"id":"` + testJobID + `","action":"start","state":"running"}`
	for _, action := range []string{"start", "backup"} {
		sock, calls := recordingAgent(t, http.StatusAccepted, job)
		audit := &fakeAudit{}
		rec := send(writeEngine(sock, audit), "/server/actions/"+action, "")
		if rec.Code != http.StatusAccepted || rec.Body.String() != job {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body)
		}
		if got := calls(); len(got) != 1 || got[0].path != "POST /v1/actions/"+action || got[0].body != "{}" {
			t.Fatalf("%s: agent got %+v", action, got)
		}
		if len(audit.events) != 2 || audit.events[0].Detail["phase"] != "requested" {
			t.Fatalf("%s: want requested row then outcome, got %+v", action, audit.events)
		}
		ev := audit.events[1]
		if ev.EventType != EventServerWrite || ev.Result != AuditSuccess || ev.Detail["job_id"] != testJobID || ev.Detail["action"] != action {
			t.Fatalf("%s: audit %+v", action, ev)
		}
	}
}

func TestServerActionRefusesBadInputWithoutCallingAgent(t *testing.T) {
	sock, calls := recordingAgent(t, http.StatusAccepted, "{}")
	audit := &fakeAudit{}
	r := writeEngine(sock, audit)
	cases := []struct {
		target, body string
		want         int
		reason       string
	}{
		{"/server/actions/delete", "", http.StatusNotFound, "unknown_action"},
		{"/server/actions/start", `{"tag":"v1.0.0"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/backup", `{"archive":"concord-backup-20260101T000000Z.tar.gz"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/start", `{"force":true}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/backup", `{"stepup":{"handle":"x","assertion":{}}}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/start", `{}{}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/start", `{"tag":"` + strings.Repeat("x", maxServerActionBody) + `"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/update", `{"tag":"main"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/update", `{"tag":"v1.0.0; rm -rf /"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/restore", `{"archive":"../etc/shadow"}`, http.StatusBadRequest, "invalid_request"},
		{"/server/actions/stop", "", http.StatusForbidden, "stepup_required"},
		{"/server/actions/update", `{"tag":"v1.2.3"}`, http.StatusForbidden, "stepup_required"},
		{"/server/actions/restart", `{"stepup":{"handle":"nothex","assertion":{}}}`, http.StatusForbidden, "stepup_failed"},
	}
	for _, tc := range cases {
		rec := send(r, tc.target, tc.body)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.reason) {
			t.Errorf("%s %.40s: got %d %s", tc.target, tc.body, rec.Code, rec.Body)
		}
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("agent called: %+v", got)
	}
	if len(audit.events) != len(cases) {
		t.Fatalf("audit rows %d, want %d", len(audit.events), len(cases))
	}
	for _, ev := range audit.events {
		if ev.EventType != EventServerWrite || ev.Result != AuditDenied {
			t.Fatalf("audit %+v", ev)
		}
	}
	if audit.events[0].Detail["action"] != "unknown" {
		t.Fatalf("unknown action logged as %v", audit.events[0].Detail["action"])
	}
}

func TestServerActionRelaysAgentRefusals(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		reason string
	}{
		{http.StatusConflict, `{"error":"write_in_progress"}`, http.StatusConflict, "write_in_progress"},
		{http.StatusConflict, `{"error":"security_log_boundary_missing"}`, http.StatusConflict, "security_log_boundary_missing"},
		{http.StatusConflict, `{"appliance":"truenas","error":"appliance_managed"}`, http.StatusConflict, "appliance_managed"},
		{http.StatusConflict, `{"appliance":"<script>","error":"appliance_managed"}`, http.StatusConflict, "appliance_managed"},
		{http.StatusConflict, `{"error":"<script>"}`, http.StatusBadGateway, "agent_error"},
		{http.StatusInternalServerError, `nope`, http.StatusBadGateway, "agent_error"},
		{http.StatusAccepted, `{"id":"short"}`, http.StatusBadGateway, "agent_error"},
	}
	for _, tc := range cases {
		sock, _ := recordingAgent(t, tc.status, tc.body)
		audit := &fakeAudit{}
		rec := send(writeEngine(sock, audit), "/server/actions/backup", "")
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.reason) {
			t.Errorf("%s: got %d %s", tc.body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "<script>") {
			t.Errorf("%s: relayed %s", tc.body, rec.Body)
		}
		// Only a known appliance name is passed on to the panel.
		if named := strings.Contains(rec.Body.String(), `"appliance":"truenas"`); named != strings.Contains(tc.body, "truenas") {
			t.Errorf("%s: appliance relay %s", tc.body, rec.Body)
		}
		ev := audit.events[len(audit.events)-1]
		if ev.Result != AuditFailure || ev.Detail["reason"] != tc.reason {
			t.Errorf("%s: audit %+v", tc.body, ev)
		}
		if started := tc.status == http.StatusAccepted; (ev.Detail["may_have_started"] != nil) != started {
			t.Errorf("%s: may_have_started %v", tc.body, ev.Detail["may_have_started"])
		}
	}
	rec := send(writeEngine("/nonexistent/agent.sock", &fakeAudit{}), "/server/actions/start", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "agent_unavailable") {
		t.Fatalf("no agent: %d %s", rec.Code, rec.Body)
	}
}

func TestServerJobsProxy(t *testing.T) {
	job := `{"id":"` + testJobID + `","state":"succeeded"}`
	sock, calls := recordingAgent(t, http.StatusOK, job)
	audit := &fakeAudit{}
	r := writeEngine(sock, audit)
	if rec := call(r, "/server/jobs/"+testJobID); rec.Code != http.StatusOK || rec.Body.String() != job {
		t.Fatalf("job: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r, "/server/jobs"); rec.Code != http.StatusOK {
		t.Fatalf("jobs: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r, "/server/jobs/NOTHEX"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
	got := calls()
	if len(got) != 2 || got[0].path != "GET /v1/jobs/"+testJobID || got[1].path != "GET /v1/jobs" {
		t.Fatalf("agent got %+v", got)
	}
	if len(audit.events) != 3 || audit.events[0].EventType != EventServerRead || audit.events[2].Result != AuditDenied {
		t.Fatalf("audit %+v", audit.events)
	}

	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusNotFound, `{"error":"unknown_job"}`, http.StatusNotFound},
		{http.StatusInternalServerError, `{}`, http.StatusBadGateway},
		{http.StatusOK, `nope`, http.StatusBadGateway},
	} {
		sock, _ := recordingAgent(t, tc.status, tc.body)
		if rec := call(writeEngine(sock, &fakeAudit{}), "/server/jobs/"+testJobID); rec.Code != tc.want {
			t.Errorf("%d %s: got %d", tc.status, tc.body, rec.Code)
		}
	}
	if rec := call(writeEngine("/nonexistent/agent.sock", &fakeAudit{}), "/server/jobs/"+testJobID); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no agent: %d", rec.Code)
	}
}

func TestServerStepUpBeginRefusesActionsWithoutStepUp(t *testing.T) {
	audit := &fakeAudit{}
	r := writeEngine("/nonexistent/agent.sock", audit)
	bodies := []string{`{"action":"start"}`, `{"action":"delete"}`, `{}`, `nope`}
	for _, body := range bodies {
		if rec := send(r, "/server/stepup", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", body, rec.Code)
		}
	}
	if len(audit.events) != len(bodies) {
		t.Fatalf("audit rows %d", len(audit.events))
	}
	for _, ev := range audit.events {
		if ev.EventType != EventServerWrite || ev.Result != AuditDenied || ev.Detail["phase"] != "stepup_begin" {
			t.Fatalf("audit %+v", ev)
		}
	}
}

func TestServerActionFailsClosedWhenAuditIsDown(t *testing.T) {
	sock, calls := recordingAgent(t, http.StatusAccepted, `{"id":"`+testJobID+`"}`)
	rec := send(writeEngine(sock, &fakeAudit{err: errors.New("db down")}), "/server/actions/backup", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "audit_unavailable") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("agent called: %+v", got)
	}
}

func TestServerActionOutlivesTheClient(t *testing.T) {
	sock, calls := recordingAgent(t, http.StatusAccepted, `{"id":"`+testJobID+`"}`)
	audit := &fakeAudit{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/server/actions/backup", nil).WithContext(ctx)
	writeEngine(sock, audit).ServeHTTP(httptest.NewRecorder(), req)
	if len(calls()) != 1 {
		t.Fatal("agent not called")
	}
	if ev := audit.events[len(audit.events)-1]; ev.Result != AuditSuccess || ev.Detail["job_id"] != testJobID {
		t.Fatalf("audit %+v", ev)
	}
}
