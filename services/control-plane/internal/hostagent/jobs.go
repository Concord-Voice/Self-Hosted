package hostagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Write actions (#188). The control-plane proxy (a later change) is to check
// the admin session and a fresh WebAuthn assertion and write the audit row
// before it calls the agent; the agent keeps the allowlist, validates every
// argument itself, and runs one write at a time. A write returns a job id at
// once and the caller polls the job.

const (
	jsonFlag       = "--json"
	maxJobs        = 16
	maxJobOutput   = 64 << 10 // tail kept per job
	maxActionBody  = 1 << 10
	jobIDBytes     = 16
	shortJobBudget = 15 * time.Minute
	// up rebuilds images; backup, restore and update move whole databases.
	longJobBudget = 2 * time.Hour
	// securityLogRoot is the boundary `up` refuses to start without, checked
	// before restart stops anything (as concord-selfhost's SECURITY_LOG_ROOT).
	securityLogRoot = "/var/log/concord-security"
)

var (
	jobIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	updateTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`) // as selfhost-update.sh
	archivePattern   = regexp.MustCompile(`^concord-backup-[0-9]{8}T[0-9]{6}Z\.tar\.gz$`)
)

// actionRequest is the only body a write accepts.
type actionRequest struct {
	Tag     string `json:"tag,omitempty"`
	Archive string `json:"archive,omitempty"`
}

// Job is what the console sees of a write action. Output is the sanitised
// tail of the CLI's combined output.
type Job struct {
	ID       string     `json:"id"`
	Action   string     `json:"action"`
	State    string     `json:"state"` // running, succeeded, failed
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	// Archive is the backup a backup job wrote.
	Archive string `json:"archive,omitempty"`
	Output  string `json:"output,omitempty"`

	out tailBuffer
}

type jobs struct {
	mu       sync.Mutex
	run      Runner
	backups  string // the install's backups directory
	marker   string // exists while a write runs
	boundary string // the security-event log root
	list     []*Job // oldest first
	running  bool
}

// EnableWrites lets s run write actions through r, whose output should
// include stderr (the CLI reports progress there). backups is the
// install's backups directory, the only place restore reads from. marker
// is a file that exists only while a write runs, so the package scripts
// can hold off restarting the agent mid-restore; a marker left by an
// earlier run is removed.
func (s *Server) EnableWrites(r Runner, backups, marker string) {
	_ = os.Remove(marker) // that run's job died with it
	s.writes = &jobs{run: r, backups: backups, marker: marker, boundary: securityLogRoot}
}

// refusal is a refused write: its HTTP status and reason.
type refusal struct {
	code   int
	reason string
}

// actionName returns the fixed name of a known write action, so nothing the
// caller sent reaches the log, or "" for any other.
func actionName(action string) string {
	switch action {
	case "start":
		return "start"
	case "stop":
		return "stop"
	case "restart":
		return "restart"
	case "backup":
		return "backup"
	case "update":
		return "update"
	case "restore":
		return "restore"
	}
	return ""
}

// commands maps a known write action and its request to the CLI runs it
// makes. reason is set when the request is refused.
func (j *jobs) commands(name string, req actionRequest) ([][]string, time.Duration, *refusal) {
	bad := func(reason string) *refusal { return &refusal{http.StatusBadRequest, reason} }
	switch name {
	case "update":
		if req.Archive != "" || !updateTagPattern.MatchString(req.Tag) {
			return nil, 0, bad("invalid_tag")
		}
		return [][]string{{"update", "--to", req.Tag, "--confirm", jsonFlag}}, longJobBudget, nil
	case "restore":
		if req.Tag != "" || !archivePattern.MatchString(req.Archive) {
			return nil, 0, bad("invalid_archive")
		}
		// Only root may change what is in the folder: restore puts back the
		// archive's .env, which the root agent then runs with.
		if st, err := os.Lstat(j.backups); err != nil || !st.IsDir() || trustedEntry(j.backups) != nil {
			return nil, 0, &refusal{http.StatusConflict, "untrusted_backups"}
		}
		p := filepath.Join(j.backups, req.Archive)
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
			return nil, 0, &refusal{http.StatusNotFound, "unknown_archive"}
		}
		return [][]string{{"restore", p, "--confirm", jsonFlag}}, longJobBudget, nil
	}
	if req != (actionRequest{}) {
		return nil, 0, bad("unexpected_argument")
	}
	switch name {
	case "start":
		return [][]string{{"up"}}, longJobBudget, nil
	case "stop":
		return [][]string{{"stop"}}, shortJobBudget, nil
	case "restart":
		// up refuses without the log boundary; check it before stopping a
		// running server. ponytail: up's storage guards still run after the
		// stop; add a guarded restart verb to the CLI if they ever refuse.
		if st, err := os.Lstat(j.boundary); err != nil || !st.IsDir() {
			return nil, 0, &refusal{http.StatusConflict, "security_log_boundary_missing"}
		}
		return [][]string{{"stop"}, {"up"}}, longJobBudget, nil
	default: // backup
		return [][]string{{"backup", jsonFlag}}, longJobBudget, nil
	}
}

func newJobID() (string, error) {
	b := make([]byte, jobIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// start records a job and runs it in the background. It returns nil when a
// write is already running.
func (j *jobs) start(action string, cmds [][]string, budget time.Duration) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return nil, nil
	}
	if err := os.WriteFile(j.marker, []byte(id+"\n"), 0o600); err != nil {
		return nil, err
	}
	job := &Job{ID: id, Action: action, State: "running", Started: time.Now().UTC()}
	job.out.max = maxJobOutput
	j.running = true
	j.list = append(j.list, job)
	if len(j.list) > maxJobs {
		// Copy so evicted jobs are freed; only the newest can be running.
		j.list = append([]*Job(nil), j.list[len(j.list)-maxJobs:]...)
	}
	go j.execute(job, cmds, budget)
	return job, nil
}

// execute runs cmds in order, stopping at the first failure. The run is not
// tied to the request: the console has already been answered.
func (j *jobs) execute(job *Job, cmds [][]string, budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var err error
	for _, args := range cmds {
		if err = j.run.Run(ctx, args, &lockedWriter{mu: &j.mu, w: &job.out}); err != nil {
			break
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	job.Finished = &now
	job.State = "succeeded"
	if err != nil {
		job.State = "failed"
		// Say why in-band: a CLI the trust check refused prints nothing.
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			_, _ = fmt.Fprintf(&job.out, "[concord-agent: %s failed: exit status %d]\n", job.Action, ee.ExitCode())
		} else {
			_, _ = fmt.Fprintf(&job.out, "[concord-agent: %s failed: the command could not run or was stopped]\n", job.Action)
		}
	}
	if job.Action == "backup" && err == nil {
		job.Archive = backupArchive(job.out.String())
	}
	_ = os.Remove(j.marker)
	j.running = false
	log.Printf("concord-agent: job action=%s state=%s", job.Action, job.State)
}

// backupArchive reads the archive name from the last schema-1 JSON line
// `backup --json` printed (stderr may follow it), keeping it only if it is
// a name the CLI generates.
func backupArchive(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var doc struct {
			Schema  int    `json:"schema"`
			OK      bool   `json:"ok"`
			Archive string `json:"archive"`
		}
		if json.Unmarshal([]byte(lines[i]), &doc) != nil || doc.Schema != 1 {
			continue
		}
		if doc.OK && archivePattern.MatchString(doc.Archive) {
			return doc.Archive
		}
		return ""
	}
	return ""
}

// snapshot copies a job for the response; withOutput adds the sanitised tail.
func (j *jobs) snapshot(job *Job, withOutput bool) Job {
	c := Job{ID: job.ID, Action: job.Action, State: job.State, Started: job.Started, Archive: job.Archive}
	if job.Finished != nil {
		f := *job.Finished
		c.Finished = &f
	}
	if out := job.out.String(); withOutput && out != "" {
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			b.WriteString(SanitizeLogLine(line))
			b.WriteByte('\n')
		}
		c.Output = b.String()
	}
	return c
}

// handleWrite serves POST /v1/actions/<name>.
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, action string) {
	name := actionName(action)
	if name == "" {
		refuse(w, http.StatusNotFound, "unknown_action")
		log.Printf("concord-agent: refused unknown action")
		return
	}
	if r.Method != http.MethodPost {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var req actionRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxActionBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			refuse(w, http.StatusBadRequest, "invalid_body")
			return
		}
	}
	cmds, budget, no := s.writes.commands(name, req)
	if no != nil {
		refuse(w, no.code, no.reason)
		log.Printf("concord-agent: action=%s outcome=refused reason=%s", name, no.reason)
		return
	}
	job, err := s.writes.start(name, cmds, budget)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "job_unavailable")
		log.Printf("concord-agent: action=%s outcome=error", name)
		return
	}
	if job == nil {
		refuse(w, http.StatusConflict, "write_in_progress")
		log.Printf("concord-agent: action=%s outcome=busy", name)
		return
	}
	s.writes.mu.Lock()
	snap := s.writes.snapshot(job, false)
	s.writes.mu.Unlock()
	log.Printf("concord-agent: action=%s outcome=started job=%s", name, job.ID)
	w.Header().Set(contentType, "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(snap)
}

// handleJobs serves GET /v1/jobs (recent jobs, newest first, no output) and
// GET /v1/jobs/<id> (one job with its output tail).
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Never write while holding the lock: a client that stops reading would
	// stall the running job's output, and with it the job.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(readTimeout))
	if id == "" {
		writeJSON(w, map[string]any{"jobs": s.writes.recent()})
		return
	}
	if job, ok := s.writes.find(id); ok {
		writeJSON(w, job)
		return
	}
	refuse(w, http.StatusNotFound, "unknown_job")
}

// recent snapshots the jobs, newest first, without output.
func (j *jobs) recent() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.list))
	for i := len(j.list) - 1; i >= 0; i-- {
		out = append(out, j.snapshot(j.list[i], false))
	}
	return out
}

// find snapshots one job with its output.
func (j *jobs) find(id string) (Job, bool) {
	if !jobIDPattern.MatchString(id) {
		return Job{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.list {
		if job.ID == id {
			return j.snapshot(job, true), true
		}
	}
	return Job{}, false
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	b   []byte
	max int
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.cut {
		// The first line may be a fragment; drop it.
		if i := strings.IndexByte(string(t.b), '\n'); i >= 0 {
			return string(t.b[i+1:])
		}
	}
	return string(t.b)
}

// lockedWriter serialises the run's writes with readers of the job.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
