package admin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type fakeAudit struct {
	mu     sync.Mutex
	events []AuditEvent
	ctxErr []error
	err    error
}

func (f *fakeAudit) Write(ctx context.Context, ev AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	f.ctxErr = append(f.ctxErr, ctx.Err())
	return f.err
}

// fakeAgent serves handler on a real unix socket, as the host agent does.
func fakeAgent(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func serverEngine(area *ServerArea) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(adminIDContextKey, "admin-1"); c.Next() })
	r.GET("/server/status", area.Status)
	r.GET("/server/health", area.Health)
	r.GET("/server/logs", area.Logs)
	return r
}

func call(r *gin.Engine, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func newArea(sock string, audit *fakeAudit) *ServerArea {
	return &ServerArea{agent: unixClient(sock), audit: audit, log: logger.NewWithWriter(&bytes.Buffer{})}
}

func TestServerAreaStatusAndHealthPassThroughAndAudit(t *testing.T) {
	var paths []string
	sock := fakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		_, _ = w.Write([]byte(`{"schema":1}`))
	})
	audit := &fakeAudit{}
	r := serverEngine(newArea(sock, audit))
	for _, p := range []string{"/server/status", "/server/health"} {
		rec := call(r, p)
		if rec.Code != http.StatusOK || rec.Body.String() != `{"schema":1}` || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %q %q", p, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	if strings.Join(paths, " ") != "/v1/status /v1/health" {
		t.Fatalf("agent paths %v", paths)
	}
	if len(audit.events) != 2 {
		t.Fatalf("audit rows %d", len(audit.events))
	}
	ev := audit.events[0]
	if ev.EventType != EventServerRead || ev.Result != AuditSuccess || ev.AdminID == nil || *ev.AdminID != "admin-1" || ev.Detail["action"] != "status" {
		t.Fatalf("audit row %+v", ev)
	}
}

func TestServerAreaLogs(t *testing.T) {
	var got string
	sock := fakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		_, _ = w.Write([]byte("line one\n"))
	})
	audit := &fakeAudit{}
	r := serverEngine(newArea(sock, audit))
	rec := call(r, "/server/logs?service=postgres&lines=50")
	if rec.Code != http.StatusOK || rec.Body.String() != "line one\n" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if got != "/v1/logs?lines=50&service=postgres" {
		t.Fatalf("agent request %q", got)
	}
	if audit.events[0].Detail["service"] != "postgres" {
		t.Fatalf("audit %+v", audit.events[0])
	}
	if rec := call(r, "/server/logs?service=redis"); rec.Code != http.StatusOK || !strings.Contains(got, "lines=200") {
		t.Fatalf("default lines: %d %q", rec.Code, got)
	}
}

func TestServerAreaLogsRefusesBadInputWithoutCallingAgent(t *testing.T) {
	called := false
	sock := fakeAgent(t, func(http.ResponseWriter, *http.Request) { called = true })
	audit := &fakeAudit{}
	r := serverEngine(newArea(sock, audit))
	for _, q := range []string{"", "service=ops-agent", "service=redis&lines=0", "service=redis&lines=1001", "service=redis&lines=x", "service=redis%26follow%3D1"} {
		if rec := call(r, "/server/logs?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d", q, rec.Code)
		}
	}
	if called {
		t.Fatal("agent called for a refused request")
	}
	if len(audit.events) != 6 || audit.events[0].Result != AuditDenied {
		t.Fatalf("audit %+v", audit.events)
	}
}

func TestServerAreaAgentFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"busy", http.StatusTooManyRequests, "", http.StatusTooManyRequests},
		{"error", http.StatusBadGateway, "", http.StatusBadGateway},
		{"not json", http.StatusOK, "nope", http.StatusBadGateway},
		{"too large", http.StatusOK, strings.Repeat(" ", maxAgentJSON+1), http.StatusBadGateway},
	}
	for _, tc := range cases {
		sock := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		audit := &fakeAudit{}
		rec := call(serverEngine(newArea(sock, audit)), "/server/status")
		if rec.Code != tc.want || audit.events[0].Result != AuditFailure {
			t.Errorf("%s: got %d %+v", tc.name, rec.Code, audit.events)
		}
	}

	audit := &fakeAudit{err: errors.New("db down")}
	rec := call(serverEngine(newArea("/nonexistent/agent.sock", audit)), "/server/health")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "agent_unavailable") {
		t.Fatalf("no agent: %d %s", rec.Code, rec.Body)
	}
}

func TestServerAreaAuditsAReadTheClientAbandoned(t *testing.T) {
	sock := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schema":1}`))
	})
	audit := &fakeAudit{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	serverEngine(newArea(sock, audit)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/server/status", nil).WithContext(ctx))
	if len(audit.events) != 1 || audit.ctxErr[0] != nil {
		t.Fatalf("audit row not written with a live context: %+v %v", audit.events, audit.ctxErr)
	}
}

func TestServerAreaLogsSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		size int
		want int
	}{{maxAgentLogs, http.StatusOK}, {maxAgentLogs + 1, http.StatusBadGateway}} {
		sock := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", tc.size)))
		})
		if rec := call(serverEngine(newArea(sock, &fakeAudit{})), "/server/logs?service=redis"); rec.Code != tc.want {
			t.Errorf("%d bytes: got %d", tc.size, rec.Code)
		}
	}
}

func TestServerAreaOutlastsTheServerWriteTimeout(t *testing.T) {
	sock := fakeAgent(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"schema":1}`))
	})
	// Stands in for the control plane's 15 s WriteTimeout against a slow health run.
	srv := httptest.NewUnstartedServer(serverEngine(newArea(sock, &fakeAudit{})))
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/server/health")
	if err != nil {
		t.Fatalf("answer dropped: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestNewServerAreaUsesTheMountedSocket(t *testing.T) {
	area := NewServerArea(&fakeAudit{}, logger.NewWithWriter(&bytes.Buffer{}))
	if area.agent == nil || area.agent.Timeout == 0 {
		t.Fatal("agent client not configured")
	}
}
