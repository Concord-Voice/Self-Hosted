package hostagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Setup-mode limits (#187).
const (
	SetupWindow        = 60 * time.Minute
	setupMaxBody       = 64 << 10
	setupMaxOutput     = 256 << 10
	setupInstallBudget = 10 * time.Minute
	setupMaxFailures   = 10
	setupFailureWindow = 10 * time.Minute
)

// answerKeys are the installer's --env-file fields (INSTALLER_ANSWER_KEYS in
// install-selfhost.sh). The installer validates every value; the agent only
// keeps the file format intact.
var answerKeys = map[string]bool{
	"CFG_DOMAIN": true, "CFG_TLS_MODE": true, "CFG_TURN_REALM": true, "CFG_PUBLIC_IP": true,
	"CFG_SMTP_HOST": true, "CFG_SMTP_PORT": true, "CFG_SMTP_USER": true, "CFG_SMTP_PASS": true, "CFG_SMTP_FROM": true,
	"CFG_TRUSTED_CIDRS": true, "CFG_ACTIVITY_HISTORY_OPERATOR_NAME": true, "CFG_ACTIVITY_HISTORY_PRIVACY_POLICY_URL": true,
	"CFG_FEEDBACK_ENABLED": true, "CFG_FEEDBACK_REPO": true, "CFG_FEEDBACK_PAT": true,
	"CFG_STORAGE_MODE": true, "CFG_STORAGE_BACKEND": true, "CFG_STORAGE_ENDPOINT": true, "CFG_STORAGE_REGION": true,
	"CFG_STORAGE_ACCESS_KEY": true, "CFG_STORAGE_SECRET_KEY": true, "CFG_STORAGE_USE_SSL": true, "CFG_STORAGE_BUCKET": true,
}

// Setup serves the first-run wizard over HTTPS while the host has no .env.
// Every API call carries the one-time token; the token dies with the first
// successful install, and the caller closes the listener when Done fires.
type Setup struct {
	token     []byte
	installer Runner // install-selfhost.sh, stdout and stderr combined
	envPath   string
	assets    http.Handler
	tmpDir    string // parent for the answers file; "" means os.TempDir()
	now       func() time.Time

	checking   sync.Mutex // one preflight at a time
	mu         sync.Mutex
	used       bool
	installing bool
	failures   []time.Time
	done       chan struct{}
}

// NewSetup returns a wizard server with a fresh 32-byte token, rendered as 64
// hex characters.
func NewSetup(installer Runner, envPath, assetsDir string) (*Setup, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	tok := hex.EncodeToString(raw)
	return &Setup{
		token:     []byte(tok),
		installer: installer,
		envPath:   envPath,
		assets:    http.FileServer(http.Dir(assetsDir)),
		now:       time.Now,
		done:      make(chan struct{}),
	}, tok, nil
}

// Done is closed after a successful install.
func (s *Setup) Done() <-chan struct{} { return s.done }

// Expire makes the token stop working, as when the setup window ends. An
// install already running still finishes.
func (s *Setup) Expire() {
	s.mu.Lock()
	s.used = true
	s.mu.Unlock()
}

// Needed reports whether the host is still uninstalled.
func Needed(envPath string) bool {
	_, err := os.Lstat(envPath)
	return errors.Is(err, os.ErrNotExist)
}

func (s *Setup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	switch r.URL.Path {
	case "/api/preflight":
		s.api(w, r, http.MethodGet, s.preflight)
	case "/api/install":
		s.api(w, r, http.MethodPost, s.install)
	default:
		if strings.HasPrefix(r.URL.Path, "/api/") {
			refuse(w, http.StatusNotFound, "unknown_action")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		s.assets.ServeHTTP(w, r)
	}
}

func (s *Setup) api(w http.ResponseWriter, r *http.Request, method string, next func(http.ResponseWriter, *http.Request)) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != method {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if code, reason := s.authorize(r); code != 0 {
		refuse(w, code, reason)
		return
	}
	next(w, r)
}

// authorize checks the bearer token in constant time. The right token always
// passes: at 256 bits it cannot be guessed, and a lockout that refused it would
// let any host on the LAN shut the operator out. Wrong tokens are counted
// across all callers; past setupMaxFailures in setupFailureWindow they get 429
// and are no longer logged, so a flood cannot fill the journal.
func (s *Setup) authorize(r *http.Request) (int, string) {
	given, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used {
		return http.StatusGone, "setup_complete"
	}
	if subtle.ConstantTimeCompare([]byte(given), s.token) == 1 {
		return 0, ""
	}
	now := s.now()
	kept := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < setupFailureWindow {
			kept = append(kept, t)
		}
	}
	s.failures = kept
	if len(s.failures) >= setupMaxFailures {
		return http.StatusTooManyRequests, "too_many_attempts"
	}
	s.failures = append(s.failures, now)
	log.Print("concord-agent: setup: rejected token")
	return http.StatusUnauthorized, "invalid_token"
}

type preflightDoc struct {
	Schema int `json:"schema"`
	Checks []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	} `json:"checks"`
	OK bool `json:"ok"`
}

func (s *Setup) preflight(w http.ResponseWriter, r *http.Request) {
	if !s.checking.TryLock() {
		refuse(w, http.StatusConflict, "preflight_running")
		return
	}
	defer s.checking.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	var out limitedBuffer
	out.max = maxCLIOutput
	runErr := s.installer.Run(ctx, []string{"--preflight-only", "--json"}, &out)
	// stderr is interleaved with the report, so take the last line that
	// decodes as one.
	lines := strings.Split(out.buf.String(), "\n")
	var doc preflightDoc
	for i := len(lines) - 1; i >= 0; i-- {
		if json.Unmarshal([]byte(lines[i]), &doc) == nil && doc.Schema == 1 {
			break
		}
		doc = preflightDoc{}
	}
	var ee interface{ ExitCode() int }
	if out.overflow || doc.Schema != 1 ||
		(runErr != nil && (!errors.As(runErr, &ee) || ee.ExitCode() != 2)) {
		refuse(w, http.StatusBadGateway, "preflight_unavailable")
		return
	}
	writeJSON(w, doc)
}

func (s *Setup) install(w http.ResponseWriter, r *http.Request) {
	var answers map[string]string
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, setupMaxBody))
	if dec.Decode(&answers) != nil || dec.More() {
		refuse(w, http.StatusBadRequest, "invalid_body")
		return
	}
	file, ok := answersFile(answers)
	if !ok {
		refuse(w, http.StatusBadRequest, "invalid_answers")
		return
	}
	// used is checked again here: a request authorized while another install
	// ran may reach this point after that install succeeded.
	s.mu.Lock()
	if s.used {
		s.mu.Unlock()
		refuse(w, http.StatusGone, "setup_complete")
		return
	}
	if s.installing {
		s.mu.Unlock()
		refuse(w, http.StatusConflict, "install_running")
		return
	}
	s.installing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.installing = false
		s.mu.Unlock()
	}()

	output, err := s.runInstaller(r, file)
	// The output carries the next steps; the journal keeps them for an
	// operator whose browser left before the answer arrived.
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		log.Printf("concord-agent: setup: installer: %s", line)
	}
	// Once .env exists the installer refuses to run again, so setup is over
	// even if a step after writing it failed.
	if Needed(s.envPath) {
		if err == nil {
			err = errors.New("installer exited without writing .env")
		}
		log.Printf("concord-agent: setup: install failed: %v", err)
		writeJSON(w, map[string]any{"ok": false, "clean": false, "output": output})
		return
	}
	s.mu.Lock()
	s.used = true
	s.mu.Unlock()
	if err != nil {
		log.Printf("concord-agent: setup: installer wrote .env but then failed: %v", err)
	}
	log.Print("concord-agent: setup: install complete; setup mode closing")
	writeJSON(w, map[string]any{"ok": true, "clean": err == nil, "output": output})
	close(s.done)
}

// answersFile renders the answers in the installer's KEY=value format. It
// refuses unknown keys and any value that could break a line.
func answersFile(answers map[string]string) ([]byte, bool) {
	if len(answers) == 0 {
		return nil, false
	}
	var b bytes.Buffer
	for k, v := range answers {
		if !answerKeys[k] || strings.ContainsAny(v, "\r\n\x00") {
			return nil, false
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.Bytes(), true
}

// runInstaller writes the answers to a 0600 file in a fresh 0700 directory,
// runs the installer on it and removes both, whatever the outcome.
func (s *Setup) runInstaller(r *http.Request, file []byte) (string, error) {
	dir, err := os.MkdirTemp(s.tmpDir, "concord-setup-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "answers.env")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		return "", err
	}
	// A browser that leaves must not kill the installer halfway: only the
	// install budget ends it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), setupInstallBudget)
	defer cancel()
	out := limitedBuffer{max: setupMaxOutput, keepRunning: true}
	runErr := s.installer.Run(ctx, []string{"--env-file", path}, &out)
	lines := strings.Split(strings.ToValidUTF8(out.buf.String(), "?"), "\n")
	for i, l := range lines {
		lines[i] = SanitizeLogLine(l)
	}
	text := strings.Join(lines, "\n")
	if out.overflow {
		text += "\n" + truncatedMarker
	}
	return text, runErr
}

// SelfSignedTLS returns a TLS config with a fresh ECDSA P-384 certificate
// valid for the setup window, and the certificate's SHA-256 fingerprint in the
// colon-separated form browsers show.
func SelfSignedTLS(now time.Time) (*tls.Config, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Concord Voice setup"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(SetupWindow + time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, c := range sum {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
	return cfg, strings.Join(parts, ":"), nil
}
