package hostagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// Write actions (#188). The control-plane checks the admin session and the
// fresh WebAuthn assertion and writes the audit row before it calls the
// agent; the agent keeps the allowlist, validates every argument itself, and
// runs one write at a time. A write returns a job id at once and the console
// polls the job.

const (
	maxJobs        = 16
	maxJobOutput   = 64 << 10 // tail kept per job
	maxActionBody  = 1 << 10
	jobIDBytes     = 16
	shortJobBudget = 15 * time.Minute
	longJobBudget  = 2 * time.Hour // backup, restore and update move whole databases
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
	mu      sync.Mutex
	run     Runner
	backups string // the install's backups directory
	list    []*Job // oldest first
	running bool
}

// EnableWrites lets s run write actions through r, whose output should
// include stderr (the CLI reports progress there). backups is the
// install's backups directory, the only place restore reads from.
func (s *Server) EnableWrites(r Runner, backups string) {
	s.writes = &jobs{run: r, backups: backups}
}

// commands maps a write action and its request to the CLI runs it makes.
// reason is set when the request is refused.
func (j *jobs) commands(action string, req actionRequest) (cmds [][]string, budget time.Duration, reason string) {
	noArgs := req == (actionRequest{})
	switch action {
	case "start":
		cmds, budget = [][]string{{"up"}}, shortJobBudget
	case "stop":
		cmds, budget = [][]string{{"stop"}}, shortJobBudget
	case "restart":
		cmds, budget = [][]string{{"stop"}, {"up"}}, shortJobBudget
	case "backup":
		cmds, budget = [][]string{{"backup", "--json"}}, longJobBudget
	case "update":
		if req.Archive != "" || !updateTagPattern.MatchString(req.Tag) {
			return nil, 0, "invalid_tag"
		}
		return [][]string{{"update", "--to", req.Tag, "--confirm", "--json"}}, longJobBudget, ""
	case "restore":
		if req.Tag != "" || !archivePattern.MatchString(req.Archive) {
			return nil, 0, "invalid_archive"
		}
		p := filepath.Join(j.backups, req.Archive)
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
			return nil, 0, "unknown_archive"
		}
		return [][]string{{"restore", p, "--confirm", "--json"}}, longJobBudget, ""
	default:
		return nil, 0, "unknown_action"
	}
	if !noArgs {
		return nil, 0, "unexpected_argument"
	}
	return cmds, budget, ""
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
	job := &Job{ID: id, Action: action, State: "running", Started: time.Now().UTC()}
	job.out.max = maxJobOutput
	j.running = true
	j.list = append(j.list, job)
	if len(j.list) > maxJobs {
		j.list = j.list[len(j.list)-maxJobs:] // only the newest can be running
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
	}
	if job.Action == "backup" && err == nil {
		job.Archive = backupArchive(job.out.String())
	}
	j.running = false
	log.Printf("concord-agent: job action=%s state=%s", job.Action, job.State)
}

// backupArchive reads the archive name from the JSON line `backup --json`
// prints last, keeping it only if it is a name the CLI generates.
func backupArchive(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var doc struct {
		Schema  int    `json:"schema"`
		OK      bool   `json:"ok"`
		Archive string `json:"archive"`
	}
	if json.Unmarshal([]byte(lines[len(lines)-1]), &doc) != nil || doc.Schema != 1 || !doc.OK ||
		!archivePattern.MatchString(doc.Archive) {
		return ""
	}
	return doc.Archive
}

// snapshot copies a job for the response; withOutput adds the sanitised tail.
func (j *jobs) snapshot(job *Job, withOutput bool) Job {
	c := Job{ID: job.ID, Action: job.Action, State: job.State, Started: job.Started, Archive: job.Archive}
	if job.Finished != nil {
		f := *job.Finished
		c.Finished = &f
	}
	if withOutput {
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimRight(job.out.String(), "\n"), "\n") {
			b.WriteString(SanitizeLogLine(line))
			b.WriteByte('\n')
		}
		c.Output = b.String()
	}
	return c
}

// handleWrite serves POST /v1/actions/<name>.
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, action string) {
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
	cmds, budget, reason := s.writes.commands(action, req)
	if reason == "unknown_action" {
		refuse(w, http.StatusNotFound, reason)
		log.Printf("concord-agent: refused unknown action")
		return
	}
	if reason != "" {
		refuse(w, http.StatusBadRequest, reason)
		log.Printf("concord-agent: action=%s outcome=refused reason=%s", action, reason)
		return
	}
	job, err := s.writes.start(action, cmds, budget)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "job_unavailable")
		return
	}
	if job == nil {
		refuse(w, http.StatusConflict, "write_in_progress")
		log.Printf("concord-agent: action=%s outcome=busy", action)
		return
	}
	s.writes.mu.Lock()
	snap := s.writes.snapshot(job, false)
	s.writes.mu.Unlock()
	log.Printf("concord-agent: action=%s outcome=started job=%s", action, job.ID)
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
	j := s.writes
	j.mu.Lock()
	defer j.mu.Unlock()
	if id == "" {
		out := make([]Job, 0, len(j.list))
		for i := len(j.list) - 1; i >= 0; i-- {
			out = append(out, j.snapshot(j.list[i], false))
		}
		writeJSON(w, map[string]any{"jobs": out})
		return
	}
	if jobIDPattern.MatchString(id) {
		for _, job := range j.list {
			if job.ID == id {
				writeJSON(w, j.snapshot(job, true))
				return
			}
		}
	}
	refuse(w, http.StatusNotFound, "unknown_job")
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
