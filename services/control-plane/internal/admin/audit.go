package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/securityevent"
)

// Audit result values — mirror the CHECK constraint on admin_audit_log.result.
const (
	// AuditSuccess marks a successful outcome.
	AuditSuccess = "success"
	// AuditFailure marks a failed outcome (e.g., bad password / assertion).
	AuditFailure = "failure"
	// AuditDenied marks an authorization denial (e.g., locked-out / no session).
	AuditDenied = "denied"
)

// auditRole is the restricted Postgres role adopted per-transaction so the
// audit INSERT runs append-only: the role holds INSERT/SELECT on
// admin_audit_log but NOT UPDATE/DELETE (migration 000077). Adopting it via
// SET LOCAL ROLE makes append-only an ENFORCED Postgres guarantee, not just
// app-layer intent. The constant name is fixed — do NOT parameterize it (it is
// a SQL identifier, not a value; SET ROLE does not accept placeholders).
const auditRole = "concord_admin_rt"

// AuditEvent is one append-only admin_audit_log entry (#1688). It records the
// OUTCOME of a security-relevant admin action — never secrets, assertion bytes,
// raw IPs, or end-user PII. Actor is the attempted operator handle (sanitized);
// SourceRef is an opaque reference (e.g., a CF Access subject), never a raw IP
// or email; Detail is a small scrubbed JSON object.
type AuditEvent struct {
	AdminID   *string        // nullable FK to admin_users.id
	Actor     string         // attempted handle; sanitized before storage
	EventType string         // one of the Event* constants in types.go
	Result    string         // AuditSuccess | AuditFailure | AuditDenied
	SourceRef string         // opaque reference; never a raw IP/email
	Detail    map[string]any // scrubbed JSON; no secrets/PII/assertion
}

// AuditLog writes append-only entries to admin_audit_log.
type AuditLog struct {
	db               *sql.DB
	securityEventsMu sync.RWMutex
	securityEvents   securityevent.Emitter
}

// NewAuditLog wires an AuditLog against the given DB.
func NewAuditLog(db *sql.DB) *AuditLog {
	return &AuditLog{db: db, securityEvents: securityevent.Discard}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without changing the
// established admin constructors.
func (a *AuditLog) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	a.securityEventsMu.Lock()
	a.securityEvents = events
	a.securityEventsMu.Unlock()
}

func (a *AuditLog) securityEventEmitter() securityevent.Emitter {
	a.securityEventsMu.RLock()
	events := a.securityEvents
	a.securityEventsMu.RUnlock()
	return events
}

// Write inserts exactly one audit row. It opens a transaction, adopts the
// restricted concord_admin_rt role via SET LOCAL ROLE (so the append-only
// guarantee is enforced by Postgres for the lifetime of the transaction), runs
// a parameterized INSERT, and commits. The Actor field is routed through
// sanitizeAuditString (CWE-117) before storage.
func (a *AuditLog) Write(ctx context.Context, ev AuditEvent) error {
	return a.WriteWith(ctx, ev, nil)
}

// errAuditWrite marks a WriteWith failure in the audit step (begin, role,
// insert or commit), as opposed to an error returned by its before function.
var errAuditWrite = errors.New("admin audit write failed")

// WriteWith runs before (when not nil) and then inserts ev in ONE transaction,
// so the rows before writes and the audit row commit together or not at all.
// An error from before is returned as is and writes no audit row; an error in
// the audit step wraps errAuditWrite. before runs under the connecting role;
// the audit INSERT then runs under the restricted role.
func (a *AuditLog) WriteWith(ctx context.Context, ev AuditEvent, before func(*sql.Tx) error) error {
	var detailJSON []byte
	if ev.Detail != nil {
		b, err := json.Marshal(ev.Detail)
		if err != nil {
			a.emitAuditWriteFailure(ctx)
			return fmt.Errorf("%w: marshal audit detail: %w", errAuditWrite, err)
		}
		detailJSON = b
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		a.emitAuditWriteFailure(ctx)
		return fmt.Errorf("%w: begin audit tx: %w", errAuditWrite, err)
	}
	// Roll back on any error path; a successful Commit makes Rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	if before != nil {
		if err := before(tx); err != nil {
			return err
		}
	}

	// Adopt the restricted role for the rest of this transaction. SET ROLE
	// takes an identifier, not a bind parameter, so the role name is a fixed
	// constant (never user input).
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+auditRole); err != nil {
		a.emitAuditWriteFailure(ctx)
		return fmt.Errorf("%w: adopt audit role: %w", errAuditWrite, err)
	}

	const q = `
		INSERT INTO admin_audit_log (admin_id, actor, event_type, result, source_ref, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	if _, err := tx.ExecContext(ctx, q,
		toNullString(adminIDValue(ev.AdminID)),
		toNullString(sanitizeAuditString(ev.Actor)),
		ev.EventType,
		ev.Result,
		toNullString(ev.SourceRef),
		detailJSONOrNil(detailJSON),
	); err != nil {
		a.emitAuditWriteFailure(ctx)
		return fmt.Errorf("%w: insert audit row: %w", errAuditWrite, err)
	}

	if err := tx.Commit(); err != nil {
		a.emitAuditWriteFailure(ctx)
		return fmt.Errorf("%w: commit audit row: %w", errAuditWrite, err)
	}
	a.emitAuditCommitted(ctx, ev.EventType, ev.Result)
	return nil
}

func (a *AuditLog) emitAuditCommitted(ctx context.Context, eventType, result string) {
	outcome := securityevent.OutcomeFailure
	switch result {
	case AuditSuccess:
		outcome = securityevent.OutcomeSuccess
	case AuditDenied:
		outcome = securityevent.OutcomeDenied
	}

	var event securityevent.Event
	switch eventType {
	case EventLoginSuccess:
		event = securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: outcome, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuthenticationSucceeded}
	case EventLoginFailure:
		event = securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: outcome, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonInvalidCredentials}
	case EventLogout:
		event = securityevent.Event{EventType: securityevent.EventSession, Outcome: outcome, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonSessionRevoked}
	case EventLockout:
		event = securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: outcome, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAccountLocked}
	case EventEnrollComplete, EventCredentialRevoked, EventBootstrap, EventAdminCreated, EventServerWrite:
		event = securityevent.Event{EventType: securityevent.EventPrivilegedAction, Outcome: outcome, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted}
	default:
		return
	}
	a.securityEventEmitter().Emit(ctx, event)
}

func (a *AuditLog) emitAuditWriteFailure(ctx context.Context) {
	a.securityEventEmitter().Emit(ctx, securityevent.Event{
		EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeFailure,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditWriteFailed,
	})
}

// MaxAdminsAddedNotice caps the sign-in notice count; the portal shows the
// cap as "N or more".
const MaxAdminsAddedNotice = 100

// AdminsAddedSinceLastSignIn counts the console-created admins (admin_created
// successes, by any admin) recorded since this admin's previous successful
// sign-in that still exist, capped at MaxAdminsAddedNotice. It is 0 on a
// first sign-in.
//
// A success row does not prove an admin was added: when the enrollment-token
// mint fails, CreateAdmin removes the pending admin but its committed success
// row stays (the log is append-only), and a retry writes another success row
// for the same username. So the count joins each success row to admin_users
// and counts distinct usernames: a removed admin is not counted, a retried
// one is counted once, and an admin whose removal failed
// (token_mint_failed_row_kept) is counted because it still exists. The audit
// row names the created admin only by username (actor), not by id, so the
// match is by username; an admin removed later by any path is also left out.
//
// The window opens ten seconds before the previous sign-in's row, so a
// creation that committed while that sign-in was being recorded is counted
// at the next one. Such a creation can be reported twice; none is missed. A
// lost login_success row widens the window, which also only over-reports.
func (a *AuditLog) AdminsAddedSinceLastSignIn(ctx context.Context, adminID string) (int, error) {
	const q = `
		SELECT LEAST(COUNT(DISTINCT u.id), $4)::int
		FROM admin_audit_log l
		JOIN admin_users u ON u.username = l.actor
		WHERE l.event_type = $2 AND l.result = $5
		  AND l.ts > (
		    SELECT ts FROM admin_audit_log
		    WHERE admin_id = $1 AND event_type = $3 AND result = $5
		    ORDER BY ts DESC
		    LIMIT 1
		  ) - interval '10 seconds'
	`
	var n int
	if err := a.db.QueryRowContext(ctx, q,
		adminID, EventAdminCreated, EventLoginSuccess, MaxAdminsAddedNotice, AuditSuccess,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins added since sign-in: %w", err)
	}
	return n, nil
}

// adminIDValue dereferences a nullable admin id pointer to a string ("" => NULL).
func adminIDValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// toNullString maps "" → SQL NULL, else a valid string.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// detailJSONOrNil returns the JSONB bytes or nil (so the column stores NULL
// rather than the literal text "null").
func detailJSONOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// sanitizeAuditString strips control characters from a user-derived string
// before it is written to the audit log, preventing CWE-117 log forging (CRLF
// injection of fabricated lines). It mirrors internal/websocket.sanitizeLogValue
// (unexported there, so duplicated here): `\n` and `\r` are removed via
// strings.ReplaceAll — the form CodeQL go/log-injection recognizes — and any
// remaining C0 control characters plus DEL are dropped. See
// [internal]rules/observability.md ("Logging Discipline").
func sanitizeAuditString(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
