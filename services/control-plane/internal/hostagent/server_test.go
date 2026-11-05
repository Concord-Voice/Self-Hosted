package hostagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

type fakeRunner struct {
	mu   sync.Mutex
	args [][]string
	out  string
	err  error
	wait chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, args []string, w io.Writer) error {
	f.mu.Lock()
	f.args = append(f.args, args)
	f.mu.Unlock()
	if _, err := io.WriteString(w, f.out); err != nil {
		return err
	}
	if f.wait != nil {
		select {
		case <-f.wait:
		case <-ctx.Done():
		}
	}
	return f.err
}

func get(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

const statusJSON = `{"schema":1,"services":[{"service":"postgres","name":"concord-postgres-1","state":"running","health":"healthy","image":"postgres:16","exit_code":null}]}`

func TestRefusesUnknownActionsAndMethods(t *testing.T) {
	f := &fakeRunner{out: statusJSON}
	s := NewServer(f)
	for _, p := range []string{"/", "/v1/up", "/v1/status/", "/v1/../v1/status", "/v2/status"} {
		if rec := get(t, s, http.MethodGet, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", p, rec.Code)
		}
	}
	if rec := get(t, s, http.MethodPost, "/v1/status"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d", rec.Code)
	}
	if len(f.args) != 0 {
		t.Fatalf("runner called for a refused request: %v", f.args)
	}
}

func TestStatus(t *testing.T) {
	f := &fakeRunner{out: statusJSON}
	rec := get(t, NewServer(f), http.MethodGet, "/v1/status")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"service":"postgres"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.args[0], " "); got != "status --json" {
		t.Fatalf("argv %q", got)
	}
}

func TestStatusRejectsBadOutput(t *testing.T) {
	cases := map[string]*fakeRunner{
		"wrong schema": {out: `{"schema":2,"services":[]}`},
		"not json":     {out: "boom"},
		"exit 2":       {out: statusJSON, err: exitErr(2)},
		"too large":    {out: strings.Repeat(" ", maxCLIOutput+1) + statusJSON},
	}
	for name, f := range cases {
		if rec := get(t, NewServer(f), http.MethodGet, "/v1/status"); rec.Code != http.StatusBadGateway {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
}

func TestStatusDropsUnknownFields(t *testing.T) {
	// A newer CLI may add fields; the agent drops them instead of failing.
	f := &fakeRunner{out: `{"schema":1,"services":[],"extra":"x"}`}
	rec := get(t, NewServer(f), http.MethodGet, "/v1/status")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "extra") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestHealthAcceptsExitOne(t *testing.T) {
	f := &fakeRunner{out: `{"schema":1,"checks":[{"name":"postgres","ok":false}],"ok":false}`, err: exitErr(1)}
	rec := get(t, NewServer(f), http.MethodGet, "/v1/health")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	f.err = errors.New("not an exit error")
	if rec := get(t, NewServer(f), http.MethodGet, "/v1/health"); rec.Code != http.StatusBadGateway {
		t.Fatalf("plain error: got %d", rec.Code)
	}
}

func TestLogsValidation(t *testing.T) {
	f := &fakeRunner{}
	s := NewServer(f)
	for _, q := range []string{
		"service=ops-agent", "service=", "service=postgres%20redis", "service=-f",
		"service=redis&lines=0", "service=redis&lines=1001", "service=redis&lines=x",
		"service=redis&follow=true",
	} {
		if rec := get(t, s, http.MethodGet, "/v1/logs?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", q, rec.Code)
		}
	}
	if len(f.args) != 0 {
		t.Fatalf("runner called: %v", f.args)
	}
}

func TestLogsTailSanitisesAndCaps(t *testing.T) {
	f := &fakeRunner{out: "one\n\x1b[31mtwo\x1b[0m\nthree\r\nfour\n"}
	rec := get(t, NewServer(f), http.MethodGet, "/v1/logs?service=redis&lines=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "one\ntwo\nthree\n" {
		t.Fatalf("body %q", got)
	}
	if got := strings.Join(f.args[0], " "); got != "logs --no-follow --tail 3 redis" {
		t.Fatalf("argv %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
}

func TestLogsFollowLimitAndTimeout(t *testing.T) {
	f := &fakeRunner{out: "line\n", wait: make(chan struct{})}
	s := NewServer(f)
	s.followMax = 50 * time.Millisecond

	var wg sync.WaitGroup
	for range maxFollowers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			get(t, s, http.MethodGet, "/v1/logs?service=postgres&follow=1")
		}()
	}
	// Wait until both followers hold a slot.
	for deadline := time.Now().Add(time.Second); len(s.followers) < maxFollowers; {
		if time.Now().After(deadline) {
			t.Fatal("followers never started")
		}
		time.Sleep(time.Millisecond)
	}
	if rec := get(t, s, http.MethodGet, "/v1/logs?service=postgres&follow=1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third follower: got %d", rec.Code)
	}
	wg.Wait() // followMax ends both
	if got := strings.Join(f.args[0], " "); got != "logs --tail 200 postgres" {
		t.Fatalf("argv %q", got)
	}
}

func TestInFlightLimit(t *testing.T) {
	s := NewServer(&fakeRunner{})
	for range maxInFlight {
		s.inFlight <- struct{}{}
	}
	if rec := get(t, s, http.MethodGet, "/v1/status"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSanitizeLogLine(t *testing.T) {
	cases := map[string]string{
		"plain\ttab":                  "plain\ttab",
		"\x1b[2J\x1b[Hcleared":        "cleared",
		"bell\x07 nul\x00 del\x7f":    "bell nul del",
		"c1\u0085 ls  ps ":            "c1 ls ps",
		"osc\x1b]0;title\x07":         "osc0;title",
		"emoji ok 🎉":                  "emoji ok 🎉",
		"bidi\u202eevil\u2066x\u2069": "bidievilx",
		"zw\u200bsp\ufeffbom\u061c":   "zwspbom",
	}
	for in, want := range cases {
		if got := SanitizeLogLine(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if got := SanitizeLogLine(strings.Repeat("é", maxLogLineBytes)); len(got) > maxLogLineBytes {
		t.Fatalf("not capped: %d bytes", len(got))
	}
}

func TestLogsReportsRunFailures(t *testing.T) {
	// A refused or failed run must not look like an empty log.
	rec := get(t, NewServer(&fakeRunner{err: errors.New("untrusted")}), http.MethodGet, "/v1/logs?service=redis")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "logs_unavailable") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	// A line over the scanner limit is a failure too.
	rec = get(t, NewServer(&fakeRunner{out: strings.Repeat("x", 2<<20) + "\n"}), http.MethodGet, "/v1/logs?service=redis")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("long line: got %d", rec.Code)
	}
	// Once lines are sent the status is fixed; the lines stay.
	rec = get(t, NewServer(&fakeRunner{out: "a\n", err: exitErr(1)}), http.MethodGet, "/v1/logs?service=redis")
	if rec.Code != http.StatusOK || rec.Body.String() != "a\n" {
		t.Fatalf("partial: got %d %q", rec.Code, rec.Body.String())
	}
	// In follow mode the followMax kill is the normal end, not a failure.
	f := &fakeRunner{out: "a\n", err: errors.New("signal: killed"), wait: make(chan struct{})}
	s := NewServer(f)
	s.followMax = 20 * time.Millisecond
	if rec = get(t, s, http.MethodGet, "/v1/logs?service=redis&follow=1"); rec.Code != http.StatusOK || rec.Body.String() != "a\n" {
		t.Fatalf("follow: got %d %q", rec.Code, rec.Body.String())
	}
	// A quiet follow that ends at followMax is an empty 200, not a 502.
	f.out = ""
	if rec = get(t, s, http.MethodGet, "/v1/logs?service=redis&follow=1"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("quiet follow: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestStalledReaderReleasesSlot(t *testing.T) {
	// A client that never reads must not keep a follower slot once the
	// follow budget and the write grace have passed.
	f := &fakeRunner{out: strings.Repeat(strings.Repeat("x", 4000)+"\n", 4000), wait: make(chan struct{})}
	s := NewServer(f)
	s.followMax = 50 * time.Millisecond
	s.writeGrace = 50 * time.Millisecond
	ts := httptest.NewServer(s)
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /v1/logs?service=redis&follow=1 HTTP/1.1\r\nHost: agent\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		started := len(f.args) == 1
		f.mu.Unlock()
		if started && len(s.followers) == 0 && len(s.inFlight) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stalled reader still holds a slot")
		}
	}
}
