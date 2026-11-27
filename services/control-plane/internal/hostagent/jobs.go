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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Write actions (#188). The agent requires its own action-bound WebAuthn
// assertion for every root write, validates the stored arguments again at
// admission, and runs one write at a time. A write returns a job id at once
// and the caller polls the job.

const (
	jsonFlag       = "--json"
	maxJobs        = 16
	maxJobOutput   = 64 << 10 // tail kept per job
	maxActionBody  = 34 << 10 // 32 KiB assertion plus bounded JSON envelope
	jobIDBytes     = 16
	shortJobBudget = 15 * time.Minute
	// up rebuilds images; backup, restore and update move whole databases.
	longJobBudget = 2 * time.Hour
	backupWindow  = time.Hour
	// securityLogRoot is the boundary `up` refuses to start without, checked
	// before restart stops anything (as concord-selfhost's SECURITY_LOG_ROOT).
	securityLogRoot = "/var/log/concord-security"
)

var (
	jobIDPattern       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	errBackupLimited   = errors.New("backup admission rate limited")
	errWriteInProgress = errors.New("write in progress")
	updateTagPattern   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`) // as selfhost-update.sh
	archivePattern     = regexp.MustCompile(`^concord-backup-[0-9]{8}T[0-9]{6}Z\.tar\.gz$`)
)

// actionRequest is accepted only at authorization begin. The final write
// receives a one-use handle and assertion, never caller-supplied arguments.
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

	out     tailBuffer
	adminID string
}

type jobs struct {
	mu      sync.Mutex
	run     Runner
	backups string // the install's backups directory
	marker  string // exists while a write runs
	// admission is the backup admission timestamp, kept beside the durable
	// audit (see backupAdmissionPath); "" refuses every backup.
	admission string
	boundary  string // the security-event log root
	// version is the install's VERSION file, the running release that
	// update refuses to go below.
	version string
	audit   *Audit
	alerts  *Alerter
	list    []*Job // oldest first
	running bool
	now     func() time.Time
}

// EnableWrites lets s run write actions through r, whose output should
// include stderr (the CLI reports progress there). backups is the
// install's backups directory, the only place restore reads from. marker
// is a file that exists only while a write runs, so the package scripts
// can hold off restarting the agent mid-restore; a marker left by an
// earlier run is removed.
func (s *Server) EnableWrites(r Runner, backups, marker string) {
	removeMarker(marker, "") // that run's job died with it
	s.writes = &jobs{
		run: r, backups: backups, marker: marker,
		admission: backupAdmissionPath(s.audit),
		boundary:  securityLogRoot,
		version:   filepath.Join(filepath.Dir(backups), "VERSION"),
		audit:     s.audit,
		alerts:    s.alerts,
		now:       time.Now,
	}
	if s.authorizations != nil {
		s.authorizations.jobs = s.writes
	}
}

// backupAdmissionFile holds the last backup admission time.
const backupAdmissionFile = "backup-last-admitted"

// backupAdmissionPath keeps the backup admission state in the audit's own
// root-only directory under /var/log, not beside the marker in /run: /run is
// a tmpfs, so a reboot would hand out a fresh backup window. Failing closed on
// a missing state file instead would refuse every backup and update for an
// hour after each reboot and on a new install, which blocks recovery, so the
// state is persisted. Writes are enabled only with an audit, so the path is
// set whenever a write can run.
func backupAdmissionPath(audit *Audit) string {
	if audit == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(audit.path), backupAdmissionFile)
}

// refusal is a refused write: its HTTP status and reason.
type refusal struct {
	code   int
	reason string
}

func (r *refusal) Error() string { return r.reason }

// removeMarker deletes the write marker, logging any failure other than its
// absence: a marker left behind holds off package restarts of the agent.
func removeMarker(marker, jobID string) {
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("concord-agent: write marker removal failed job=%s: %v", jobID, err)
	}
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
		if no := j.refuseDowngrade(req.Tag); no != nil {
			return nil, 0, no
		}
		return [][]string{{"update", "--to", req.Tag, "--confirm", jsonFlag}}, longJobBudget, nil
	case "restore":
		return j.restoreCommand(req)
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

// restoreCommand checks a restore request against the backups folder.
func (j *jobs) restoreCommand(req actionRequest) ([][]string, time.Duration, *refusal) {
	if req.Tag != "" || !archivePattern.MatchString(req.Archive) {
		return nil, 0, &refusal{http.StatusBadRequest, "invalid_archive"}
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

func newJobID() (string, error) {
	b := make([]byte, jobIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// start records a job and runs it in the background. It returns nil when a
// write is already running. req is the validated request the commands were
// built from; its tag or archive name is recorded in the audit.
//
// The admitted event is written before the marker and backup admission state
// so a job never runs unaudited. If either of those then fails, the job is
// rolled back and a terminal failed/admission_failed event is written for the
// same job id, so every admitted record in the stream has a terminal record.
func (j *jobs) start(action, adminID string, req actionRequest, cmds [][]string, budget time.Duration) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	backupAt := j.now().UTC()
	if err := j.admissibleLocked(action, backupAt); err != nil {
		if errors.Is(err, errWriteInProgress) {
			return nil, nil
		}
		return nil, err
	}
	event := AuditEvent{Action: action, AdminID: adminID, JobID: id, Tag: req.Tag, Archive: req.Archive}
	if j.audit == nil {
		return nil, errors.New("host-agent admission audit unavailable")
	}
	admitted := event
	admitted.Outcome, admitted.Reason = "admitted", "admission_approved"
	if err := j.audit.Write(admitted); err != nil {
		log.Printf("concord-agent: host-agent audit admission event failed job=%s: %v", id, err)
		return nil, errors.New("host-agent admission audit unavailable")
	}
	// The alert follows the fsynced admitted record and never gates the job.
	j.alerts.Enqueue(AlertEvent{Outcome: "admitted", Reason: admitted.Reason, Action: action, Tag: req.Tag, Archive: req.Archive, AdminID: adminID, JobID: id})
	rollback := func(cause error) (*Job, error) {
		failed := event
		failed.Outcome, failed.Reason = "failed", "admission_failed"
		if err := j.audit.Write(failed); err != nil {
			log.Printf("concord-agent: host-agent audit admission rollback event failed job=%s: %v", id, err)
		}
		return nil, cause
	}
	if err := os.WriteFile(j.marker, []byte(id+"\n"), 0o600); err != nil {
		return rollback(err)
	}
	if takesBackup(action) {
		if err := j.writeBackupAdmission(filepath.Dir(j.admission), backupAt); err != nil {
			if removeErr := os.Remove(j.marker); removeErr != nil {
				log.Printf("concord-agent: write marker removal failed job=%s: %v", id, removeErr)
				return rollback(errors.Join(err, fmt.Errorf("remove job marker: %w", removeErr)))
			}
			return rollback(err)
		}
	}
	job := &Job{ID: id, Action: action, State: "running", Started: time.Now().UTC(), adminID: adminID}
	job.out.max = maxJobOutput
	j.running = true
	j.list = append(j.list, job)
	if len(j.list) > maxJobs {
		// Copy so evicted jobs are freed; only the newest can be running.
		j.list = append([]*Job(nil), j.list[len(j.list)-maxJobs:]...)
	}
	// Snapshot before the goroutine starts: the 202 reports the just-started
	// job, even if it finishes before the handler encodes the response.
	snap := j.snapshot(job, false)
	go j.execute(job, cmds, budget)
	startedEvent := event
	startedEvent.Outcome, startedEvent.Reason = "started", "job_started"
	if err := j.audit.Write(startedEvent); err != nil {
		log.Printf("concord-agent: host-agent audit start event failed job=%s: %v", id, err)
	}
	return &snap, nil
}

// admissible runs the admission refusals that need no proof, so a busy agent
// or a closed backup window refuses before the admin's one-use handle is
// spent. It is advisory only: start re-checks the same state under j.mu, so a
// write admitted between the two is still refused there, at the cost of that
// one handle, and a handle is never reused.
func (j *jobs) admissible(action string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.admissibleLocked(action, j.now().UTC())
}

// admissibleLocked runs under j.mu. No alert destination refuses every write:
// the out-of-band alert is the only detection control for a control plane
// that spends a harvested login tap on a host action.
func (j *jobs) admissibleLocked(action string, now time.Time) error {
	switch {
	case j.alerts == nil:
		return ErrAlertNotConfigured
	case j.running:
		return errWriteInProgress
	case takesBackup(action):
		return j.checkBackupAdmission(now)
	}
	return nil
}

// takesBackup reports whether action runs a backup and so spends the hourly
// backup admission: update runs verb_backup before it switches release.
func takesBackup(action string) bool { return action == "backup" || action == "update" }

// checkBackupAdmission runs under j.mu before the audit, marker and timestamp
// commit. A failed backup job still consumes its window once admitted.
func (j *jobs) checkBackupAdmission(now time.Time) error {
	if j.admission == "" {
		return errors.New("backup admission state unavailable")
	}
	dir := filepath.Dir(j.admission)
	if err := trustedEntry(dir); err != nil {
		return fmt.Errorf("backup admission directory: %w", err)
	}
	last, found, err := j.backupAdmissionTimestamp()
	if err != nil {
		return err
	}
	if found && now.Before(time.Unix(0, last).Add(backupWindow)) {
		return errBackupLimited
	}
	return nil
}

func (j *jobs) backupAdmissionTimestamp() (int64, bool, error) {
	st, err := os.Lstat(j.admission)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("inspect backup admission state: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || trustedEntry(j.admission) != nil {
		return 0, false, errors.New("backup admission state is untrusted")
	}
	f, err := os.OpenFile(j.admission, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, false, fmt.Errorf("open backup admission state: %w", err)
	}
	b, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return 0, false, errors.New("read backup admission state")
	}
	last, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || last < 0 {
		return 0, false, errors.New("backup admission state is invalid")
	}
	return last, true, nil
}

func (j *jobs) writeBackupAdmission(dir string, now time.Time) error {
	tmp, err := os.CreateTemp(dir, ".backup-admission-")
	if err != nil {
		return fmt.Errorf("create backup admission state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Print("concord-agent: backup admission temporary cleanup failed")
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("close backup admission state after chmod failure: %w", closeErr)
		}
		return fmt.Errorf("protect backup admission state: %w", err)
	}
	if _, err := fmt.Fprintf(tmp, "%d\n", now.UnixNano()); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("close backup admission state after write failure: %w", closeErr)
		}
		return fmt.Errorf("write backup admission state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("close backup admission state after sync failure: %w", closeErr)
		}
		return fmt.Errorf("sync backup admission state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close backup admission state: %w", err)
	}
	if err := os.Rename(tmpName, j.admission); err != nil {
		return fmt.Errorf("publish backup admission state: %w", err)
	}
	return nil
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
	removeMarker(j.marker, job.ID)
	j.running = false
	if j.audit != nil {
		outcome, reason := "succeeded", "job_succeeded"
		if err != nil {
			outcome, reason = "failed", "job_failed"
		}
		if auditErr := j.audit.Write(AuditEvent{Action: job.Action, Outcome: outcome, Reason: reason, AdminID: job.adminID, JobID: job.ID}); auditErr != nil {
			log.Printf("concord-agent: host-agent audit completion event failed job=%s: %v", job.ID, auditErr)
		}
	}
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
		s.auditDenial(AuditEvent{Outcome: "denied", Reason: "request_refused"})
		refuse(w, http.StatusNotFound, "unknown_action")
		log.Printf("concord-agent: refused unknown action")
		return
	}
	if r.Method != http.MethodPost {
		refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if s.authorizations == nil || s.audit == nil {
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "authorizer_unavailable"})
		refuse(w, http.StatusServiceUnavailable, "authorizer_unavailable")
		return
	}
	var proof struct {
		Handle    string          `json:"handle"`
		Assertion json.RawMessage `json:"assertion"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxActionBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&proof); err != nil || dec.Decode(new(any)) != io.EOF || proof.Handle == "" || len(proof.Assertion) == 0 {
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "assertion_denied"})
		refuse(w, http.StatusBadRequest, "invalid_body")
		return
	}
	// Refusals that need no proof come before the handle is spent, so a busy
	// agent or a closed backup window does not burn the admin's approval.
	if err := s.writes.admissible(name); err != nil {
		s.refuseAdmission(w, name, "", err)
		return
	}
	req, adminID, err := s.authorizations.consume(proof.Handle, name, proof.Assertion)
	if err != nil {
		reason := "assertion_denied"
		switch {
		case errors.Is(err, ErrAuthorizationReplay):
			reason = "replay_denied"
		case errors.Is(err, errCloneDetected):
			reason = "clone_detected"
		case errors.Is(err, errCounterStale):
			reason = "counter_stale"
		case errors.Is(err, errNoActionCredential):
			reason = "no_action_credential"
		case errors.Is(err, ErrAuthorizerUnavailable):
			// Trusted verification could not run; the assertion was not judged.
			reason = "authorizer_unavailable"
		}
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: reason, AdminID: adminID})
		if errors.Is(err, ErrAuthorizerUnavailable) {
			refuse(w, http.StatusServiceUnavailable, "authorizer_unavailable")
			return
		}
		refuse(w, http.StatusForbidden, "authorization_denied")
		log.Printf("concord-agent: action=%s outcome=refused reason=authorization_denied", name)
		return
	}
	cmds, budget, no := s.writes.commands(name, req)
	if no != nil {
		// The assertion verified; the request itself no longer holds.
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "request_refused", AdminID: adminID})
		refuse(w, no.code, no.reason)
		log.Printf("concord-agent: action=%s outcome=refused reason=%s", name, no.reason)
		return
	}
	job, err := s.writes.start(name, adminID, req, cmds, budget)
	if err == nil && job == nil {
		err = errWriteInProgress
	}
	if err != nil {
		s.refuseAdmission(w, name, adminID, err)
		return
	}
	snap := *job
	log.Printf("concord-agent: action=%s outcome=started job=%s", name, job.ID)
	w.Header().Set(contentType, "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(snap)
}

// refuseAdmission answers a write that start, or the check before the proof
// is spent, did not admit. adminID is empty before the proof is verified.
func (s *Server) refuseAdmission(w http.ResponseWriter, name, adminID string, err error) {
	switch {
	case errors.Is(err, ErrAlertNotConfigured):
		refuse(w, http.StatusServiceUnavailable, "alerts_not_configured")
		log.Printf("concord-agent: action=%s outcome=refused reason=alerts_not_configured", name)
	case errors.Is(err, errWriteInProgress):
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "write_in_progress", AdminID: adminID})
		refuse(w, http.StatusConflict, "write_in_progress")
		log.Printf("concord-agent: action=%s outcome=busy", name)
	case errors.Is(err, errBackupLimited):
		if s.recordDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "backup_rate_limited", AdminID: adminID}, 0) != nil {
			refuse(w, http.StatusServiceUnavailable, "job_unavailable")
			log.Printf("concord-agent: action=%s outcome=error", name)
			return
		}
		refuse(w, http.StatusTooManyRequests, "backup_rate_limited")
	default:
		s.auditDenial(AuditEvent{Action: name, Outcome: "denied", Reason: "job_unavailable", AdminID: adminID})
		refuse(w, http.StatusServiceUnavailable, "job_unavailable")
		log.Printf("concord-agent: action=%s outcome=error", name)
	}
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
