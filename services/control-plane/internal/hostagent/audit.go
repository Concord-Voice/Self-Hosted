package hostagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	// HostAgentAuditPath is the root process's local security-event stream.
	// Nightwatch ingestion of this independent schema requires separate review.
	HostAgentAuditPath = "/var/log/concord-security/host-agent/events.jsonl"
	maxAuditLineBytes  = 2048
	maxAuditFileBytes  = 20 << 20
	// maxAuditDenialBytes is where denial records stop. It sits below the
	// 19M logrotate size, so denials alone never trigger a rotation that
	// would evict older admitted records, and the space above it is kept for
	// admitted, started, succeeded and failed records.
	maxAuditDenialBytes = 16 << 20
)

var (
	errAuditFull = errors.New("host-agent audit full")
	// errAuditDenialReserve refuses a denial record past maxAuditDenialBytes.
	errAuditDenialReserve = fmt.Errorf("%w: denial records refused in the reserved space", errAuditFull)
)

// AuditEvent contains only fixed action/outcome/reason values, opaque IDs and
// the validated arguments a job ran with. Assertion bytes, credential IDs and
// bodies cannot be represented by this type: Tag must be a release tag the
// update parser accepts and Archive a CLI-generated backup name.
type AuditEvent struct {
	Action  string
	Outcome string
	Reason  string
	AdminID string
	JobID   string
	Tag     string
	Archive string
	// Suppressed counts identical denials dropped since the last record.
	Suppressed int
}

// Audit writes a bounded, fsynced host-agent JSONL stream. It is independent
// of the control-plane container and the asynchronous Nightwatch producer.
type Audit struct {
	mu        sync.Mutex
	path      string
	ownerUID  uint32
	trustRoot string
	syncFile  func(*os.File) error
	broken    bool
	full      bool // logged once until a write succeeds again
	// denialsFull is logged once until a denial write succeeds again.
	denialsFull bool
	// deniedPastReserve counts denials refused past maxAuditDenialBytes
	// since the last summary record.
	deniedPastReserve int
}

// OpenAudit requires a root-owned log boundary and a root-only stream. It
// creates only the host-agent child directory and file under that boundary.
func OpenAudit(path string) (*Audit, error) {
	if filepath.Clean(path) != HostAgentAuditPath {
		return nil, errors.New("invalid host-agent audit path")
	}
	return openAuditAt(path, 0, string(filepath.Separator))
}

func openAuditAt(path string, ownerUID uint32, trustRoot string) (*Audit, error) {
	a := &Audit{path: filepath.Clean(path), ownerUID: ownerUID, trustRoot: filepath.Clean(trustRoot), syncFile: (*os.File).Sync}
	parent := filepath.Dir(a.path)
	if err := a.checkDirectories(filepath.Dir(parent)); err != nil {
		return nil, err
	}
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create host-agent audit directory: %w", err)
	}
	if err := a.checkDirectories(parent); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(a.path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host-agent audit: %w", err)
	}
	f := os.NewFile(uintptr(fd), a.path)
	if err := a.checkFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sync host-agent audit: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close host-agent audit: %w", err)
	}
	dir, err := os.Open(parent) // #nosec G304 -- parent is the validated root-owned audit directory
	if err != nil {
		return nil, fmt.Errorf("open host-agent audit directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return nil, fmt.Errorf("sync host-agent audit directory: %w", err)
	}
	return a, nil
}

func (a *Audit) checkDirectories(start string) error {
	if !filepath.IsAbs(a.path) || !filepath.IsAbs(a.trustRoot) {
		return errors.New("invalid host-agent audit path")
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || !ownedBy(info, a.ownerUID) || info.Mode().Perm()&0o022 != 0 {
			return errors.New("unsafe host-agent audit directory")
		}
		if dir == a.trustRoot {
			return nil
		}
		if dir == filepath.Dir(dir) {
			return errors.New("host-agent audit path outside trust root")
		}
	}
}

func (a *Audit) checkFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedBy(info, a.ownerUID) || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() > maxAuditFileBytes {
		return errors.New("unsafe host-agent audit file")
	}
	return nil
}

func validAuditEvent(e AuditEvent) bool {
	if e.Action != "" && actionName(e.Action) != e.Action {
		return false
	}
	switch e.Outcome {
	case "denied", "admitted", "started", "succeeded", "failed":
	default:
		return false
	}
	switch e.Reason {
	case "peer_forbidden", "begin_denied", "assertion_denied", "replay_denied", "clone_detected", "counter_stale", "admission_approved", "admission_failed", "job_started", "job_succeeded", "job_failed", "write_in_progress", "job_unavailable", "backup_rate_limited", "no_action_credential", "request_refused", "authorizer_unavailable", "denials_suppressed":
	default:
		return false
	}
	if e.Suppressed < 0 || (e.Tag != "" && (len(e.Tag) > 128 || !updateTagPattern.MatchString(e.Tag))) || (e.Archive != "" && !archivePattern.MatchString(e.Archive)) {
		return false
	}
	if e.AdminID != "" {
		id, err := uuid.Parse(e.AdminID)
		if err != nil || id.String() != e.AdminID {
			return false
		}
	}
	return e.JobID == "" || jobIDPattern.MatchString(e.JobID)
}

func canonicalAuditAdminID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || id.String() != value {
		return ""
	}
	return value
}

// Write durably appends one bounded record. Admission callers must refuse the
// job when this returns an error.
func (a *Audit) Write(event AuditEvent) error {
	if a == nil || !validAuditEvent(event) {
		return errors.New("invalid host-agent audit event")
	}
	data, err := encodeAuditEvent(event)
	if err != nil {
		return err
	}
	return a.appendRecord(event, data)
}

// encodeAuditEvent renders one validated event as a bounded JSONL line.
func encodeAuditEvent(event AuditEvent) ([]byte, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("host-agent audit ID unavailable")
	}
	record := struct {
		Schema     string `json:"schema_version"`
		EventID    string `json:"event_id"`
		OccurredAt string `json:"occurred_at"`
		Service    string `json:"service"`
		EventType  string `json:"event_type"`
		Action     string `json:"action,omitempty"`
		Outcome    string `json:"outcome"`
		Reason     string `json:"reason_code"`
		AdminID    string `json:"admin_id,omitempty"`
		JobID      string `json:"job_id,omitempty"`
		Tag        string `json:"tag,omitempty"`
		Archive    string `json:"archive,omitempty"`
		Suppressed int    `json:"suppressed_count,omitempty"`
	}{"host-agent-audit.v1", id.String(), time.Now().UTC().Format(time.RFC3339Nano), "host-agent", "privileged_action", event.Action, event.Outcome, event.Reason, event.AdminID, event.JobID, event.Tag, event.Archive, event.Suppressed}
	data, err := json.Marshal(record)
	if err != nil || len(data)+1 > maxAuditLineBytes {
		return nil, errors.New("host-agent audit event too large")
	}
	return append(data, '\n'), nil
}

// appendRecord writes one encoded record under the stream's checks and caps.
func (a *Audit) appendRecord(event AuditEvent, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken {
		return errors.New("host-agent audit requires repair")
	}
	// Write opens the path afresh every time, so a stream that logrotate
	// renamed away and recreated (create 0600 root root), or one truncated in
	// place, is picked up by the next write after the same checks as OpenAudit.
	if err := a.checkDirectories(filepath.Dir(a.path)); err != nil {
		return err
	}
	fd, err := syscall.Open(a.path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open host-agent audit: %w", err)
	}
	f := os.NewFile(uintptr(fd), a.path)
	info, err := f.Stat()
	if err != nil || a.checkFile(f) != nil {
		_ = f.Close()
		return errors.New("unsafe host-agent audit file")
	}
	if err := a.checkRoomLocked(event, info.Size(), len(data)); err != nil {
		_ = f.Close()
		return err
	}
	data, summarized := a.withSummaryLocked(event, info.Size(), data)
	written, writeErr := f.Write(data)
	if written != len(data) || writeErr != nil {
		_ = f.Truncate(info.Size())
		_ = f.Close()
		return a.breakStream("append")
	}
	if err := a.syncFile(f); err != nil {
		_ = f.Close()
		return a.breakStream("sync")
	}
	if err := f.Close(); err != nil {
		return a.breakStream("close")
	}
	a.full = false
	a.deniedPastReserve -= summarized
	if event.Outcome == "denied" {
		a.denialsFull = false
	}
	return nil
}

// checkRoomLocked refuses a record that does not fit: a denial past the
// denial reserve (counted for a later summary) or any record past the file
// limit. It logs each condition once until a write succeeds again.
func (a *Audit) checkRoomLocked(event AuditEvent, size int64, n int) error {
	if event.Outcome == "denied" && size+int64(n) > maxAuditDenialBytes {
		a.deniedPastReserve += 1 + event.Suppressed
		if !a.denialsFull {
			a.denialsFull = true
			log.Print("concord-agent: host-agent audit near full; denials are no longer recorded until the stream is rotated")
		}
		return errAuditDenialReserve
	}
	if size+int64(n) > maxAuditFileBytes {
		if !a.full {
			a.full = true
			log.Print("concord-agent: host-agent audit full; privileged writes refused until the stream is rotated")
		}
		return errAuditFull
	}
	return nil
}

// withSummaryLocked puts one summary record, counting the denials refused
// past the reserve, ahead of data when both fit: an admission inside the
// reserve, or anything after a rotation. It returns the records to write and
// how many denials the summary covers.
func (a *Audit) withSummaryLocked(event AuditEvent, size int64, data []byte) ([]byte, int) {
	n := a.deniedPastReserve
	if n == 0 {
		return data, 0
	}
	limit := int64(maxAuditFileBytes)
	if event.Outcome == "denied" {
		limit = maxAuditDenialBytes
	}
	summary, err := encodeAuditEvent(AuditEvent{Outcome: "denied", Reason: "denials_suppressed", Suppressed: n})
	if err != nil || size+int64(len(summary)+len(data)) > limit {
		return data, 0
	}
	return append(summary, data...), n
}

// breakStream latches the audit off after a failed append, sync or close: the
// stream may hold a torn or unsynced record, so only a restart after repair
// re-enables it.
func (a *Audit) breakStream(stage string) error {
	a.broken = true
	log.Printf("concord-agent: host-agent audit %s failed; audit disabled until repaired and the agent restarted", stage)
	return fmt.Errorf("host-agent audit %s failed", stage)
}
