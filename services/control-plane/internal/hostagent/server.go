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
	"unicode"
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
	healthTimeout    = 60 * time.Second // health runs several probes with their own budgets
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
	// writeGrace is how long past an action's budget a response may take to
	// write before the connection is cut.
	writeGrace time.Duration
	followMax  time.Duration
}

// NewServer returns a Server that runs actions through r.
func NewServer(r Runner) *Server {
	return &Server{
		run:        r,
		inFlight:   make(chan struct{}, maxInFlight),
		followers:  make(chan struct{}, maxFollowers),
		followMax:  followMaxRuntime,
		writeGrace: 5 * time.Second,
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
	// A client that stops reading must not hold a slot past the action's own
	// budget; the run's timeout cannot interrupt a blocked write.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(healthTimeout + s.writeGrace))
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
func (s *Server) runJSON(r *http.Request, timeout time.Duration, args []string, dst any) bool {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var out limitedBuffer
	out.max = maxCLIOutput
	runErr := s.run.Run(ctx, args, &out)
	if out.overflow {
		return false
	}
	dec := json.NewDecoder(&out.buf)
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
	if !s.runJSON(r, readTimeout, []string{"status", "--json"}, &doc) || doc.Schema != 1 {
		refuse(w, http.StatusBadGateway, "status_unavailable")
		return "error"
	}
	writeJSON(w, doc)
	return "ok"
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) string {
	var doc healthDoc
	if !s.runJSON(r, healthTimeout, []string{"health", "--json"}, &doc) || doc.Schema != 1 {
		refuse(w, http.StatusBadGateway, "health_unavailable")
		return "error"
	}
	writeJSON(w, doc)
	return "ok"
}

// logQuery validates a logs request; reason is set when it is refused.
func logQuery(r *http.Request) (service string, lines int, follow bool, reason string) {
	q := r.URL.Query()
	service = q.Get("service")
	if !logServices[service] {
		return "", 0, false, "unknown_service"
	}
	lines = defaultLogLines
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLines {
			return "", 0, false, "invalid_lines"
		}
		lines = n
	}
	switch q.Get("follow") {
	case "", "0":
	case "1":
		follow = true
	default:
		return "", 0, false, "invalid_follow"
	}
	return service, lines, follow, ""
}

// copyLines writes sanitised lines to w: at most limit of them unless
// following, flushing each one when following. It returns how many it wrote
// and false if the client could not be written to.
func copyLines(w http.ResponseWriter, sc *bufio.Scanner, limit int, follow bool) (int, bool) {
	flusher, _ := w.(http.Flusher)
	written := 0
	for sc.Scan() {
		if !follow && written >= limit {
			continue // drain; never send more than asked
		}
		if _, err := io.WriteString(w, SanitizeLogLine(sc.Text())+"\n"); err != nil {
			return written, false
		}
		written++
		if flusher != nil && follow {
			flusher.Flush()
		}
	}
	return written, true
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) string {
	service, lines, follow, reason := logQuery(r)
	if reason != "" {
		refuse(w, http.StatusBadRequest, reason)
		return "refused"
	}

	args := []string{"logs", "--no-follow", "--tail", strconv.Itoa(lines), service}
	timeout := readTimeout
	if follow {
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
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + s.writeGrace))

	w.Header().Set(contentType, "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.run.Run(ctx, args, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	written, ok := copyLines(w, sc, lines, follow)
	if !ok {
		cancel()
	}
	// A deadline (followMax) or a client that went away ends a run normally;
	// anything else is a real failure and must not look like an empty log.
	stopped := r.Context().Err() != nil || (follow && ctx.Err() != nil)
	cancel()
	_ = pr.Close()
	runErr := <-done
	// The scanner's other errors are the run's own error, passed through the
	// pipe, so only an over-long line is a separate failure.
	if (runErr != nil && !stopped) || errors.Is(sc.Err(), bufio.ErrTooLong) {
		if written == 0 {
			refuse(w, http.StatusBadGateway, "logs_unavailable")
		} else {
			// The status is already sent; say in-band that lines are missing so
			// a cut-off log never reads as complete.
			_, _ = io.WriteString(w, truncatedMarker)
		}
		return "error"
	}
	return "ok"
}

// truncatedMarker ends a log reply that failed after lines were sent, so the
// console shows the log as cut off instead of complete.
const truncatedMarker = "[concord-agent: output incomplete]\n"

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]`)

// SanitizeLogLine removes terminal escapes, control characters (tab kept) and
// invisible format characters such as bidi overrides,
// and caps the length, so a log line cannot drive a terminal or forge lines.
func SanitizeLogLine(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	out := make([]rune, 0, len(s))
	size := 0
	for _, c := range s {
		if c == '\t' || (c >= 0x20 && c != 0x7f && (c < 0x80 || c > 0x9f) && c != 0x2028 && c != 0x2029 && c != 0xfffd && !unicode.Is(unicode.Cf, c)) {
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
	// keepRunning drops all output from the first write past max instead of
	// failing the write, so a command whose run must not be cut short (the
	// installer) is not killed by a broken pipe.
	keepRunning bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	// ponytail: keepRunning cuts at the first overflow and keeps the head; keep
	// a tail ring too if the installer's closing next steps ever get cut.
	if b.overflow || b.buf.Len()+len(p) > b.max {
		b.overflow = true
		if b.keepRunning {
			return len(p), nil
		}
		return 0, errors.New("output too large")
	}
	return b.buf.Write(p)
}
