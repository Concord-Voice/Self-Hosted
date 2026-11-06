package hostagent

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runFunc func(ctx context.Context, args []string, w io.Writer) error

func (f runFunc) Run(ctx context.Context, args []string, w io.Writer) error { return f(ctx, args, w) }

const preflightOK = `{"schema":1,"checks":[{"name":"os","ok":true},{"name":"ports","ok":true}],"ok":true}`

func newTestSetup(t *testing.T, r Runner) (*Setup, string) {
	t.Helper()
	dir := t.TempDir()
	assets := filepath.Join(dir, "wizard")
	if err := os.Mkdir(assets, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<h1>wizard</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, tok, err := NewSetup(r, filepath.Join(dir, ".env"), assets)
	if err != nil {
		t.Fatal(err)
	}
	s.tmpDir = dir
	return s, tok
}

func call(s *Setup, method, target, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestSetupTokenShape(t *testing.T) {
	_, tok := newTestSetup(t, runFunc(nil))
	if len(tok) != 64 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Fatalf("token %q is not 64 hex characters", tok)
	}
	_, other := newTestSetup(t, runFunc(nil))
	if tok == other {
		t.Fatal("two setups share a token")
	}
}

func TestSetupServesAssetsWithoutTokenAndAPIWithIt(t *testing.T) {
	calls := 0
	s, tok := newTestSetup(t, runFunc(func(_ context.Context, args []string, w io.Writer) error {
		calls++
		if strings.Join(args, " ") != "--preflight-only --json" {
			t.Errorf("args %v", args)
		}
		_, _ = io.WriteString(w, "reason on stderr\n"+preflightOK+"\n")
		return exitErr(2) // a failed check still yields the report
	}))
	rec := call(s, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wizard") {
		t.Fatalf("assets: %d %q", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("csp %q", csp)
	}
	for _, bad := range []string{"", "nope", tok + "x", strings.ToUpper(tok)} {
		if rec := call(s, http.MethodGet, "/api/preflight", bad, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", bad, rec.Code)
		}
	}
	if calls != 0 {
		t.Fatal("installer ran without a valid token")
	}
	rec = call(s, http.MethodGet, "/api/preflight", tok, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preflight: %d %v", rec.Code, rec.Header())
	}
	var doc preflightDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || !doc.OK || len(doc.Checks) != 2 {
		t.Fatalf("preflight body %q", rec.Body.String())
	}
}

func TestSetupRefusesUnknownRoutesAndMethods(t *testing.T) {
	s, tok := newTestSetup(t, runFunc(nil))
	cases := []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/up", http.StatusNotFound},
		{http.MethodPost, "/api/preflight", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/install", http.StatusMethodNotAllowed},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		if rec := call(s, c.method, c.path, tok, ""); rec.Code != c.code {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestSetupPreflightUnavailable(t *testing.T) {
	for name, r := range map[string]runFunc{
		"not json": func(_ context.Context, _ []string, w io.Writer) error { _, _ = io.WriteString(w, "boom"); return nil },
		"wrong schema": func(_ context.Context, _ []string, w io.Writer) error {
			_, _ = io.WriteString(w, `{"schema":2}`)
			return nil
		},
		"runtime error": func(_ context.Context, _ []string, w io.Writer) error {
			_, _ = io.WriteString(w, preflightOK)
			return exitErr(1)
		},
		"no output": func(context.Context, []string, io.Writer) error { return nil },
	} {
		s, tok := newTestSetup(t, r)
		if rec := call(s, http.MethodGet, "/api/preflight", tok, ""); rec.Code != http.StatusBadGateway {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestSetupRateLimitsFailedTokens(t *testing.T) {
	s, tok := newTestSetup(t, runFunc(func(_ context.Context, _ []string, w io.Writer) error {
		_, _ = io.WriteString(w, preflightOK)
		return nil
	}))
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	for range setupMaxFailures {
		if rec := call(s, http.MethodGet, "/api/preflight", "wrong", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Code)
		}
	}
	// Locked out, even with the right token.
	if rec := call(s, http.MethodGet, "/api/preflight", tok, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("lockout: %d", rec.Code)
	}
	now = now.Add(setupFailureWindow)
	if rec := call(s, http.MethodGet, "/api/preflight", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("after window: %d", rec.Code)
	}
}

func TestSetupInstallWritesPrivateAnswersAndBurnsToken(t *testing.T) {
	var answersPath string
	var s *Setup
	s, tok := newTestSetup(t, runFunc(func(_ context.Context, args []string, w io.Writer) error {
		if len(args) != 2 || args[0] != "--env-file" {
			t.Fatalf("args %v", args)
		}
		answersPath = args[1]
		st, err := os.Stat(answersPath)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("answers file: %v %v", st, err)
		}
		if dst, _ := os.Stat(filepath.Dir(answersPath)); dst.Mode().Perm() != 0o700 {
			t.Fatalf("answers dir mode %v", dst.Mode())
		}
		data, _ := os.ReadFile(answersPath) //nolint:gosec // path from the code under test
		if string(data) != "CFG_DOMAIN=example.com\n" {
			t.Fatalf("answers %q", data)
		}
		_, _ = io.WriteString(w, "\x1b[0;32m✓ wrote .env\x1b[0m\nnext steps\u202e\n")
		return os.WriteFile(s.envPath, []byte("X=1\n"), 0o600)
	}))
	rec := call(s, http.MethodPost, "/api/install", tok, `{"CFG_DOMAIN":"example.com"}`)
	var res struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK || !res.OK {
		t.Fatalf("install: %d %q", rec.Code, rec.Body.String())
	}
	if res.Output != "✓ wrote .env\nnext steps\n" {
		t.Fatalf("output not sanitized: %q", res.Output)
	}
	if _, err := os.Stat(filepath.Dir(answersPath)); !os.IsNotExist(err) {
		t.Fatal("answers directory left behind")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after install")
	}
	if rec := call(s, http.MethodGet, "/api/preflight", tok, ""); rec.Code != http.StatusGone {
		t.Fatalf("token reused: %d", rec.Code)
	}
}

func TestSetupInstallFailureKeepsSetupOpen(t *testing.T) {
	var answersPath string
	for name, r := range map[string]runFunc{
		"installer error": func(_ context.Context, args []string, w io.Writer) error {
			answersPath = args[1]
			_, _ = io.WriteString(w, "installer: invalid domain\n")
			return exitErr(1)
		},
		"no env written": func(_ context.Context, args []string, _ io.Writer) error {
			answersPath = args[1]
			return nil
		},
	} {
		s, tok := newTestSetup(t, r)
		rec := call(s, http.MethodPost, "/api/install", tok, `{"CFG_DOMAIN":"bad"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":false`) {
			t.Fatalf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(answersPath); !os.IsNotExist(err) {
			t.Fatalf("%s: answers file left behind", name)
		}
		select {
		case <-s.Done():
			t.Fatalf("%s: setup closed after a failed install", name)
		default:
		}
		if rec := call(s, http.MethodGet, "/", "", ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: wizard gone: %d", name, rec.Code)
		}
	}
}

func TestSetupInstallRefusesBadAnswers(t *testing.T) {
	ran := false
	s, tok := newTestSetup(t, runFunc(func(context.Context, []string, io.Writer) error { ran = true; return nil }))
	for _, body := range []string{
		``, `[]`, `{}`, `{"CFG_DOMAIN":1}`, `{"PATH":"/tmp"}`, `{"CFG_DOMAIN":"a\nCFG_TLS_MODE=local"}`,
		`{"CFG_DOMAIN":"a\r"}`, `{"CFG_DOMAIN":"a\u0000"}`, `{"CFG_DOMAIN":"a"} {"CFG_DOMAIN":"b"}`,
		`{"CFG_DOMAIN":"` + strings.Repeat("a", setupMaxBody) + `"}`,
	} {
		if rec := call(s, http.MethodPost, "/api/install", tok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %.40q: %d", body, rec.Code)
		}
	}
	if ran {
		t.Fatal("installer ran on refused answers")
	}
}

func TestSetupInstallIsExclusive(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s, tok := newTestSetup(t, runFunc(func(context.Context, []string, io.Writer) error {
		close(started)
		<-release
		return exitErr(1)
	}))
	first := make(chan int)
	go func() { first <- call(s, http.MethodPost, "/api/install", tok, `{"CFG_DOMAIN":"a"}`).Code }()
	<-started
	if rec := call(s, http.MethodPost, "/api/install", tok, `{"CFG_DOMAIN":"a"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second install: %d", rec.Code)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first install: %d", code)
	}
}

func TestSetupInstallOutputIsCapped(t *testing.T) {
	s, tok := newTestSetup(t, runFunc(func(_ context.Context, _ []string, w io.Writer) error {
		// Writes past the cap must succeed, or the installer dies of EPIPE.
		for range 2 {
			if _, err := io.WriteString(w, strings.Repeat("x\n", setupMaxOutput/2+1)); err != nil {
				t.Errorf("write past the cap failed: %v", err)
			}
		}
		return exitErr(1)
	}))
	rec := call(s, http.MethodPost, "/api/install", tok, `{"CFG_DOMAIN":"a"}`)
	if rec.Body.Len() > setupMaxOutput+1024 || !strings.Contains(rec.Body.String(), "output incomplete") {
		t.Fatalf("output not capped: %d bytes", rec.Body.Len())
	}
}

func TestSetupInstallOutlivesTheBrowser(t *testing.T) {
	s, tok := newTestSetup(t, runFunc(func(ctx context.Context, _ []string, _ io.Writer) error {
		if ctx.Err() != nil {
			t.Error("installer context ended with the request")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("installer has no time budget")
		}
		return exitErr(1)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser has gone
	req := httptest.NewRequest(http.MethodPost, "/api/install", strings.NewReader(`{"CFG_DOMAIN":"a"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok)
	s.ServeHTTP(httptest.NewRecorder(), req)
}

func TestSetupPreflightFindsTheReportAmongStderr(t *testing.T) {
	s, tok := newTestSetup(t, runFunc(func(_ context.Context, _ []string, w io.Writer) error {
		_, _ = io.WriteString(w, "before\n"+preflightOK+"\nwarning printed after the report\n")
		return nil
	}))
	if rec := call(s, http.MethodGet, "/api/preflight", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestNeeded(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if !Needed(env) {
		t.Fatal("missing .env not reported as needing setup")
	}
	if err := os.Symlink("/nonexistent", env); err != nil {
		t.Fatal(err)
	}
	if Needed(env) {
		t.Fatal("a dangling .env symlink must not reopen setup")
	}
}

func TestSelfSignedTLS(t *testing.T) {
	now := time.Now()
	cfg, fp, err := SelfSignedTLS(now)
	if err != nil {
		t.Fatal(err)
	}
	der := cfg.Certificates[0].Certificate[0]
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if cert.NotAfter.Before(now.Add(SetupWindow)) {
		t.Fatalf("certificate expires before the window: %v", cert.NotAfter)
	}
	sum := sha256.Sum256(der)
	if want := strings.ToUpper(fmt.Sprintf("%X", sum[:1])); !strings.HasPrefix(fp, want+":") || len(fp) != 32*3-1 {
		t.Fatalf("fingerprint %q", fp)
	}
}
