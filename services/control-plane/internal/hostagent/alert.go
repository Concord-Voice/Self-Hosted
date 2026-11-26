package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Out-of-band action alerts (#165). The control plane serves the admin
// console, so a compromised control plane can ask an admin to tap a key for a
// different action than the one shown. WebAuthn cannot show what is signed, so
// the agent tells the operator about every admitted action, and about denials,
// through a destination only root configures and the control plane never sees.

const (
	// AlertConfigPath is the root-owned alert destination. It is never read or
	// set through the socket.
	AlertConfigPath     = "/etc/concord-voice/agent-alert.json"
	maxAlertConfigBytes = 4 << 10
	maxAlertURLBytes    = 2048
	alertTimeout        = 5 * time.Second
	alertRetryDelay     = time.Second
	alertQueueSize      = 32
)

// ErrAlertNotConfigured means no alert file exists: actions are audited but
// not alerted out of band.
var ErrAlertNotConfigured = errors.New("host-agent alerts not configured")

// AlertEvent is what one notification says. Like AuditEvent it holds only
// fixed values, opaque IDs and validated arguments: no assertion data.
type AlertEvent struct {
	Outcome    string // admitted, denied or test
	Reason     string
	Action     string
	Tag        string
	Archive    string
	AdminID    string
	JobID      string
	Suppressed int
}

// Alerter delivers alerts best-effort from a bounded queue. Delivery never
// blocks or fails an action: the durable audit record is the authority.
type Alerter struct {
	url    string
	host   string
	client *http.Client
	now    func() time.Time
	retry  time.Duration
	queue  chan AlertEvent
	// dropLogged rate-limits the queue_full journal line to one per window.
	dropMu     sync.Mutex
	dropLogged time.Time
}

type alertConfig struct {
	WebhookURL string `json:"webhook_url"`
}

// LoadAlerter reads AlertConfigPath with the authorizer file's trust checks
// and starts the delivery worker. It returns ErrAlertNotConfigured when the
// file does not exist.
func LoadAlerter(ctx context.Context) (*Alerter, error) {
	webhook, err := loadAlertConfigAt(AlertConfigPath, 0, string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	a := newAlerter(webhook, alertHostName())
	go a.worker(ctx)
	return a, nil
}

// LoadAlerterForTest reads the alert file without starting the worker, for
// `concord-agent alert test`, which sends synchronously.
func LoadAlerterForTest() (*Alerter, error) {
	webhook, err := loadAlertConfigAt(AlertConfigPath, 0, string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	return newAlerter(webhook, alertHostName()), nil
}

func alertHostName() string {
	host, err := os.Hostname()
	if err != nil || host == "" || len(host) > 255 || strings.IndexFunc(host, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "unknown-host"
	}
	return host
}

func newAlerter(webhook, host string) *Alerter {
	return &Alerter{
		url:  webhook,
		host: host,
		client: &http.Client{
			Timeout: alertTimeout,
			// Only the configured https URL is ever contacted: no redirects, no
			// environment proxy.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{Proxy: nil, TLSHandshakeTimeout: alertTimeout, ResponseHeaderTimeout: alertTimeout},
		},
		now:   time.Now,
		retry: alertRetryDelay,
		queue: make(chan AlertEvent, alertQueueSize),
	}
}

// loadAlertConfigAt keeps the owner/path policy testable without weakening
// the root-only entry point.
func loadAlertConfigAt(path string, ownerUID uint32, trustRoot string) (string, error) {
	// A missing file (or a missing /etc/concord-voice) only means alerts are
	// off; everything that does exist is checked below.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", ErrAlertNotConfigured
	}
	if err := checkTrustedPath(path, trustRoot, ownerUID, "alert"); err != nil {
		return "", err
	}
	fd, err := syscall.Open(filepath.Clean(path), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return "", ErrAlertNotConfigured
		}
		return "", fmt.Errorf("open alert file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	// The URL may carry a receiver token, so the file is root-only.
	if err != nil || !info.Mode().IsRegular() || !ownedBy(info, ownerUID) || info.Mode().Perm()&0o077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() > maxAlertConfigBytes {
		return "", errors.New("unsafe alert file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAlertConfigBytes+1))
	if err != nil || len(data) > maxAlertConfigBytes || !utf8.Valid(data) {
		return "", errors.New("invalid alert file")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc alertConfig
	if err := dec.Decode(&doc); err != nil || dec.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid alert file")
	}
	if !validAlertURL(doc.WebhookURL) {
		return "", errors.New("alert webhook_url must be an absolute https URL")
	}
	return doc.WebhookURL, nil
}

func validAlertURL(raw string) bool {
	if raw == "" || len(raw) > maxAlertURLBytes {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.Opaque == "" && u.Fragment == ""
}

// Enqueue hands an event to the worker without waiting. A full queue drops
// the event and logs the fixed class queue_full.
func (a *Alerter) Enqueue(event AlertEvent) {
	if a == nil {
		return
	}
	select {
	case a.queue <- event:
	default:
		a.dropMu.Lock()
		defer a.dropMu.Unlock()
		if now := a.now(); now.Sub(a.dropLogged) >= denialWindow {
			a.dropLogged = now
			log.Print("concord-agent: alert delivery failed: class=queue_full")
		}
	}
}

func (a *Alerter) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-a.queue:
			if err := a.Send(ctx, event); err != nil {
				log.Printf("concord-agent: alert delivery failed: class=%s", AlertFailureClass(err))
			}
		}
	}
}

// Send delivers one event, retrying once after a short delay.
func (a *Alerter) Send(ctx context.Context, event AlertEvent) error {
	body, err := json.Marshal(a.payload(event))
	if err != nil {
		return &alertError{class: "encode"}
	}
	err = a.post(ctx, body)
	if err == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(a.retry):
	}
	return a.post(ctx, body)
}

type alertPayload struct {
	Text       string `json:"text"`
	Source     string `json:"source"`
	Event      string `json:"event"`
	Reason     string `json:"reason,omitempty"`
	Action     string `json:"action,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Archive    string `json:"archive,omitempty"`
	AdminID    string `json:"admin_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Suppressed int    `json:"suppressed_count,omitempty"`
	Host       string `json:"host"`
	Time       string `json:"time"`
}

func (a *Alerter) payload(e AlertEvent) alertPayload {
	at := a.now().UTC().Format(time.RFC3339)
	return alertPayload{
		Text: alertText(a.host, e), Source: "concord-host-agent", Event: e.Outcome, Reason: e.Reason,
		Action: e.Action, Tag: e.Tag, Archive: e.Archive, AdminID: e.AdminID, JobID: e.JobID,
		Suppressed: e.Suppressed, Host: a.host, Time: at,
	}
}

func alertText(host string, e AlertEvent) string {
	action := e.Action
	if action == "" {
		action = "a host action"
	}
	var b strings.Builder
	switch e.Outcome {
	case "admitted":
		fmt.Fprintf(&b, "Concord host agent on %s approved %s", host, action)
	case "denied":
		fmt.Fprintf(&b, "Concord host agent on %s refused %s (%s)", host, action, e.Reason)
	default:
		fmt.Fprintf(&b, "Concord host agent on %s: test alert. Host actions will be reported here.", host)
		return b.String()
	}
	if e.Tag != "" {
		fmt.Fprintf(&b, " to %s", e.Tag)
	}
	if e.Archive != "" {
		fmt.Fprintf(&b, " from %s", e.Archive)
	}
	if e.AdminID != "" {
		fmt.Fprintf(&b, " for admin %s", e.AdminID)
	}
	if e.JobID != "" {
		fmt.Fprintf(&b, ", job %s", e.JobID)
	}
	if e.Suppressed > 0 {
		fmt.Fprintf(&b, "; %d similar refusals not reported separately", e.Suppressed)
	}
	if e.Outcome == "admitted" {
		b.WriteString(". If no admin asked for this, treat the control plane as compromised.")
	}
	return b.String()
}

type alertError struct{ class string }

func (e *alertError) Error() string { return "alert delivery failed: " + e.class }

// AlertFailureClass maps a delivery error to a fixed word, so the URL and the
// receiver's answer never reach the journal.
func AlertFailureClass(err error) string {
	var ae *alertError
	if errors.As(err, &ae) {
		return ae.class
	}
	return "network"
}

func (a *Alerter) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, alertTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return &alertError{class: "request"}
	}
	req.Header.Set(contentType, "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return &alertError{class: "timeout"}
		}
		return &alertError{class: "network"}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &alertError{class: "status"}
	}
	return nil
}
