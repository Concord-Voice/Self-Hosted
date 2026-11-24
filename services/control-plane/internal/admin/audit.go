package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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
// before gets the event and may fill it in from what it wrote (CreateAdmin
// records the created admin's id). The detail is encoded before the
// transaction opens, so a detail that cannot be encoded writes nothing, and
// again after before returns. An error from before is returned as is and
// writes no audit row; an error in the audit step wraps errAuditWrite.
// before runs under the connecting role; the audit INSERT then runs under
// the restricted role.
func (a *AuditLog) WriteWith(ctx context.Context, ev AuditEvent, before func(*sql.Tx, *AuditEvent) error) error {
	detailJSON, err := a.marshalDetail(ctx, ev.Detail)
	if err != nil {
		return err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		a.emitAuditWriteFailure(ctx)
		return fmt.Errorf("%w: begin audit tx: %w", errAuditWrite, err)
	}
	// Roll back on any error path; a successful Commit makes Rollback a no-op.
	defer func() { _ = tx.Rollback() }()

	if before != nil {
		if err := before(tx, &ev); err != nil {
			return err
		}
		if detailJSON, err = a.marshalDetail(ctx, ev.Detail); err != nil {
			return err
		}
	}

	// An admin_created success row takes its id and commits under the sign-in
	// notice lock held shared, so a sign-in's count, which holds it exclusive,
	// either sees the row or the row's id is above that sign-in's row
	// (RecordSignIn).
	if ev.EventType == EventAdminCreated && ev.Result == AuditSuccess {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, signInNoticeLockKey); err != nil {
			a.emitAuditWriteFailure(ctx)
			return fmt.Errorf("%w: take sign-in notice lock: %w", errAuditWrite, err)
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

// marshalDetail encodes an audit detail, nil for none.
func (a *AuditLog) marshalDetail(ctx context.Context, detail map[string]any) ([]byte, error) {
	if detail == nil {
		return nil, nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		a.emitAuditWriteFailure(ctx)
		return nil, fmt.Errorf("%w: marshal audit detail: %w", errAuditWrite, err)
	}
	return b, nil
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

// detailCreatedAdminID is the detail key of an admin_created success row that
// holds the created admin's id. The id stays in admin_audit_log: no export or
// security event carries the detail.
const detailCreatedAdminID = "created_admin_id"

// detailAdminsAdded is the detail key of a login_success row whose notice
// count was read in the same transaction as the row; it holds that count.
// Such a row ends a notice window exactly (see RecordSignIn).
const detailAdminsAdded = "admins_added"

// detailAdminsAddedUnread marks a login_success row written after its notice
// count could not be read. Nothing was shown at that sign-in, so the row never
// ends a notice window.
const detailAdminsAddedUnread = "admins_added_unread"

// canonicalUUIDPattern matches the lowercase hyphenated id CreateAdmin writes.
// Any other value is treated as no id.
const canonicalUUIDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// signInNoticeLockKey is the PostgreSQL advisory key that orders admin
// creations against sign-in notice reads. WriteWith takes it shared, as the
// last step before inserting an admin_created success row, and holds it until
// that row commits; RecordSignIn takes it exclusive around its count and its
// login_success insert. The domain string keeps the key apart from every other
// advisory key space.
var signInNoticeLockKey = func() int64 {
	digest := sha256.Sum256([]byte("admin_signin_notice\x00v1"))
	return int64(binary.BigEndian.Uint64(digest[:8])) //nolint:gosec // bit-preserving conversion into the signed advisory key space
}()

// signInNoticeLockTimeout bounds RecordSignIn's wait for in-flight creations.
// A timeout writes the sign-in without a count, which ends no window.
const signInNoticeLockTimeout = "5s"

// AdminsAddedSinceLastSignIn counts the console-created admins (admin_created
// successes, by any admin) recorded since this admin's previous successful
// sign-in that still exist, capped at MaxAdminsAddedNotice. It is 0 on a
// first sign-in. It only reads; RecordSignIn reads the same count while it
// records a sign-in.
//
// A success row does not prove an admin was added: when the enrollment-token
// mint fails, CreateAdmin removes the pending admin but its committed success
// row stays (the log is append-only), and a retry writes another success row
// for the same username. So the count matches each success row to admin_users
// by the created admin's id (detail.created_admin_id, written in the same
// transaction as the admin) and counts distinct admins: a removed admin is not
// counted, a retried one is counted once, and an admin whose removal failed
// (token_mint_failed_row_kept) is counted because it still exists. An admin
// removed later by any path is also left out. The match is never by username:
// the actor is sanitized on write, so a username holding control characters
// would not match its own row.
//
// A row with no well-formed id (written before the id was recorded, or
// malformed) cannot be matched, so each such row counts as one: it can only
// over-count, never hide an admin.
//
// The window is bounded by audit row id, never by time alone, because ts is
// the writing transaction's start time and a creation can commit long after
// it started. The previous sign-in is the admin's newest login_success row
// that does not carry detailAdminsAddedUnread:
//   - A row carrying detailAdminsAdded was written by RecordSignIn under the
//     exclusive lock, so it saw exactly the success rows with a lower id; the
//     window is the rows with a higher id.
//   - A row with neither key was written before this rule. Its window is the
//     rows with a higher id plus those from ten seconds before it (the
//     earlier rule), which can only over-count.
//   - When every sign-in row is unread, nothing was ever shown, so every
//     success row counts.
//
// Picking an older row than the newest one, which ts order can do when two
// sign-ins of one admin overlap, only widens the window.
func (a *AuditLog) AdminsAddedSinceLastSignIn(ctx context.Context, adminID string) (int, error) {
	return adminsAddedSince(ctx, a.db, adminID)
}

type auditRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// adminsAddedQueryMarker starts the count query, so a fault test can match it.
const adminsAddedQueryMarker = "/* admins added since last sign-in */"

func adminsAddedSince(ctx context.Context, db auditRowQuerier, adminID string) (int, error) {
	const q = adminsAddedQueryMarker + `
		WITH prev AS (
		  SELECT id, ts, COALESCE(detail ? $7, false) AS exact
		  FROM admin_audit_log
		  WHERE admin_id = $1 AND event_type = $3 AND result = $5
		    AND NOT COALESCE(detail ? $8, false)
		  ORDER BY ts DESC
		  LIMIT 1
		), signed_in AS (
		  SELECT EXISTS (
		    SELECT 1 FROM admin_audit_log
		    WHERE admin_id = $1 AND event_type = $3 AND result = $5
		  ) AS any
		), created AS (
		  SELECT CASE WHEN l.detail->>'created_admin_id' ~ $6
		              THEN (l.detail->>'created_admin_id')::uuid END AS id
		  FROM admin_audit_log l
		  CROSS JOIN signed_in s
		  LEFT JOIN prev p ON true
		  WHERE l.event_type = $2 AND l.result = $5 AND s.any
		    AND (p.id IS NULL
		         OR l.id > p.id
		         OR (NOT p.exact AND l.ts > p.ts - interval '10 seconds'))
		)
		SELECT LEAST(
		  (SELECT COUNT(DISTINCT u.id) FROM created c JOIN admin_users u ON u.id = c.id)
		  + (SELECT COUNT(*) FROM created WHERE id IS NULL),
		  $4)::int
	`
	var n int
	if err := db.QueryRowContext(ctx, q,
		adminID, EventAdminCreated, EventLoginSuccess, MaxAdminsAddedNotice, AuditSuccess, canonicalUUIDPattern,
		detailAdminsAdded, detailAdminsAddedUnread,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins added since sign-in: %w", err)
	}
	return n, nil
}

// RecordSignIn writes this admin's login_success row and returns the sign-in
// notice count: the admins added since the previous sign-in
// (AdminsAddedSinceLastSignIn). read reports whether the count was read; the
// caller shows no count when it was not. err is set only when no
// login_success row could be written.
//
// The count and the row commit in one transaction that holds
// signInNoticeLockKey exclusive, and every admin_created success row is
// inserted and committed under the same key held shared. So each success row
// either committed before the count, and has a lower id than this sign-in's
// row, or took its id after this transaction committed, and has a higher one.
// The row records the count it shows (detailAdminsAdded), and the next sign-in
// counts the rows above its id, so no creation falls between two windows
// however long a creation transaction or a sign-in takes.
//
// When the count cannot be read, or that transaction fails for any other
// reason, the row is written again without the lock and marked
// detailAdminsAddedUnread, which the next sign-in skips: its window then runs
// from the last sign-in that did show a count, so a failed read never drops a
// creation.
func (a *AuditLog) RecordSignIn(ctx context.Context, adminID, actor string) (n int, read bool, err error) {
	id := adminID
	ev := AuditEvent{AdminID: &id, Actor: actor, EventType: EventLoginSuccess, Result: AuditSuccess}
	countErr := a.WriteWith(ctx, ev, func(tx *sql.Tx, ev *AuditEvent) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '"+signInNoticeLockTimeout+"'"); err != nil {
			return fmt.Errorf("bound sign-in notice lock wait: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, signInNoticeLockKey); err != nil {
			return fmt.Errorf("take sign-in notice lock: %w", err)
		}
		count, err := adminsAddedSince(ctx, tx, adminID)
		if err != nil {
			return err
		}
		n = count
		ev.Detail = map[string]any{detailAdminsAdded: count}
		return nil
	})
	if countErr == nil {
		return n, true, nil
	}
	ev.Detail = map[string]any{detailAdminsAddedUnread: true}
	if werr := a.Write(ctx, ev); werr != nil {
		return 0, false, errors.Join(countErr, werr)
	}
	return 0, false, nil
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
