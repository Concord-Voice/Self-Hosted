// Package hostagent is the Concord Host Agent (#186): a root service on the
// self-hosted machine that runs a fixed set of concord-selfhost verbs for the
// Admin Console. It listens only on a unix socket and accepts nothing but the
// actions below; the control-plane proxy is a convenience, not the gate.
package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Limits. Each is a ceiling a caller cannot raise.
const (
	maxCLIOutput     = 1 << 20 // status/health JSON
	maxLogLineBytes  = 4096
	maxLogLines      = 1000
	defaultLogLines  = 200
	maxInFlight      = 8
	maxFollowers     = 2
	readTimeout      = 30 * time.Second
	followMaxRuntime = 10 * time.Minute
)

const contentType = "Content-Type"

// Services the agent will read logs for. Compose service names.
var logServices = map[string]bool{
	"control-plane": true,
	"media-plane":   true,
	"postgres":      true,
	"redis":         true,
	"nats":          true,
	"minio":         true,
	"coturn":        true,
}

// Runner runs the concord-selfhost CLI with args and copies its stdout to w.
// It returns the exit error, if any.
type Runner interface {
	Run(ctx context.Context, args []string, w io.Writer) error
}

// Server serves the agent's HTTP API on the unix socket.
type Server struct {
	run       Runner
	inFlight  chan struct{}
	followers chan struct{}
	followMax time.Duration
}

// NewServer returns a Server that runs actions through r.
func NewServer(r Runner) *Server {
	return &Server{
		run:       r,
		inFlight:  make(chan struct{}, maxInFlight),
		followers: make(chan struct{}, maxFollowers),
		followMax: followMaxRuntime,
	}
}

// ServeHTTP routes exactly three GET actions; everything else is refused here,
// whatever the proxy in front allowed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var handle func(http.ResponseWriter, *http.Request) string
	var action string
	switch r.URL.Path {
	case "/v1/status":
		handle, action = s.status, "status"
	case "/v1/health":
		handle, action = s.health, "health"
	case "/v1/logs":
		handle, action = s.logs, "logs"
	default:
		refuse(w, http.StatusNotFound, "unknown_action")
		log.Printf("concord-agent: refused unknown action")
		return
	}
	if r.Method != http.MethodGet {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	select {
	case s.inFlight <- struct{}{}:
		defer func() { <-s.inFlight }()
	default:
		refuse(w, http.StatusTooManyRequests, "busy")
		return
	}
	outcome := handle(w, r)
	log.Printf("concord-agent: action=%s outcome=%s", action, outcome)
}

func refuse(w http.ResponseWriter, code int, reason string) {
	w.Header().Set(contentType, "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set(contentType, "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// runJSON runs args and decodes the CLI's JSON into dst. A non-zero exit is
// accepted when the output still decodes: `health` exits 1 when a check fails.
func (s *Server) runJSON(r *http.Request, args []string, dst any) bool {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	var out limitedBuffer
	out.max = maxCLIOutput
	runErr := s.run.Run(ctx, args, &out)
	if out.overflow {
		return false
	}
	dec := json.NewDecoder(&out.buf)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return false
	}
	var ee interface{ ExitCode() int }
	return runErr == nil || (errors.As(runErr, &ee) && ee.ExitCode() == 1)
}

// Only these fields leave the agent; the CLI already restricts their values.
type statusDoc struct {
	Schema   int `json:"schema"`
	Services []struct {
		Service  string `json:"service"`
		Name     string `json:"name"`
		State    string `json:"state"`
		Health   string `json:"health"`
		Image    string `json:"image"`
		ExitCode *int   `json:"exit_code"`
	} `json:"services"`
}

type healthDoc struct {
	Schema int `json:"schema"`
	Checks []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	} `json:"checks"`
	OK bool `json:"ok"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) string {
	var doc statusDoc
	if !s.runJSON(r, []string{"status", "--json"}, &doc) || doc.Schema != 1 {
		refuse(w, http.StatusBadGateway, "status_unavailable")
		return "error"
	}
	writeJSON(w, doc)
	return "ok"
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) string {
	var doc healthDoc
	if !s.runJSON(r, []string{"health", "--json"}, &doc) || doc.Schema != 1 {
		refuse(w, http.StatusBadGateway, "health_unavailable")
		return "error"
	}
	writeJSON(w, doc)
	return "ok"
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) string {
	q := r.URL.Query()
	service := q.Get("service")
	if !logServices[service] {
		refuse(w, http.StatusBadRequest, "unknown_service")
		return "refused"
	}
	lines := defaultLogLines
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLines {
			refuse(w, http.StatusBadRequest, "invalid_lines")
			return "refused"
		}
		lines = n
	}
	follow := q.Get("follow")
	if follow != "" && follow != "0" && follow != "1" {
		refuse(w, http.StatusBadRequest, "invalid_follow")
		return "refused"
	}

	args := []string{"logs", "--no-follow", "--tail", strconv.Itoa(lines), service}
	timeout := readTimeout
	if follow == "1" {
		select {
		case s.followers <- struct{}{}:
			defer func() { <-s.followers }()
		default:
			refuse(w, http.StatusTooManyRequests, "too_many_followers")
			return "busy"
		}
		args = []string{"logs", "--tail", strconv.Itoa(lines), service}
		timeout = s.followMax
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	w.Header().Set(contentType, "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.run.Run(ctx, args, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	written := 0
	for sc.Scan() {
		if follow != "1" && written >= lines {
			continue // drain; never send more than asked
		}
		if _, err := io.WriteString(w, SanitizeLogLine(sc.Text())+"\n"); err != nil {
			cancel()
			break
		}
		written++
		if flusher != nil && follow == "1" {
			flusher.Flush()
		}
	}
	cancel()
	_ = pr.Close()
	<-done
	return "ok"
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]`)

// SanitizeLogLine removes terminal escapes and control characters (tab kept)
// and caps the length, so a log line cannot drive a terminal or forge lines.
func SanitizeLogLine(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	out := make([]rune, 0, len(s))
	size := 0
	for _, c := range s {
		if c == '\t' || (c >= 0x20 && c != 0x7f && (c < 0x80 || c > 0x9f) && c != 0x2028 && c != 0x2029 && c != 0xfffd) {
			if size += len(string(c)); size > maxLogLineBytes {
				break
			}
			out = append(out, c)
		}
	}
	return string(out)
}

type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.overflow = true
		return 0, errors.New("output too large")
	}
	return b.buf.Write(p)
}
