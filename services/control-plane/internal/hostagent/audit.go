package hostagent

import (
	"encoding/json"
	"errors"
	"fmt"
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
)

// AuditEvent contains only fixed action/outcome/reason values and opaque IDs.
// Assertion bytes, credential IDs, tags, archive names, and bodies cannot be
// represented by this type.
type AuditEvent struct {
	Action  string
	Outcome string
	Reason  string
	AdminID string
	JobID   string
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
	case "peer_forbidden", "begin_denied", "assertion_denied", "replay_denied", "admission_approved", "job_started", "job_succeeded", "job_failed", "write_in_progress", "job_unavailable", "backup_rate_limited":
	default:
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
	id, err := uuid.NewRandom()
	if err != nil {
		return errors.New("host-agent audit ID unavailable")
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
	}{"host-agent-audit.v1", id.String(), time.Now().UTC().Format(time.RFC3339Nano), "host-agent", "privileged_action", event.Action, event.Outcome, event.Reason, event.AdminID, event.JobID}
	data, err := json.Marshal(record)
	if err != nil || len(data)+1 > maxAuditLineBytes {
		return errors.New("host-agent audit event too large")
	}
	data = append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken {
		return errors.New("host-agent audit requires repair")
	}
	if err := a.checkDirectories(filepath.Dir(a.path)); err != nil {
		return err
	}
	fd, err := syscall.Open(a.path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open host-agent audit: %w", err)
	}
	f := os.NewFile(uintptr(fd), a.path)
	info, err := f.Stat()
	if err != nil || a.checkFile(f) != nil || info.Size()+int64(len(data)) > maxAuditFileBytes {
		_ = f.Close()
		return errors.New("unsafe or full host-agent audit file")
	}
	written, writeErr := f.Write(data)
	if written != len(data) || writeErr != nil {
		_ = f.Truncate(info.Size())
		_ = f.Close()
		a.broken = true
		return errors.New("host-agent audit append failed")
	}
	if err := a.syncFile(f); err != nil {
		_ = f.Close()
		a.broken = true
		return errors.New("host-agent audit sync failed")
	}
	if err := f.Close(); err != nil {
		a.broken = true
		return errors.New("host-agent audit close failed")
	}
	return nil
}
