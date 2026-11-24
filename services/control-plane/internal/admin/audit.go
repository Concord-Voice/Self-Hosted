package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

	tx, err := a.db.BeginTx(ctx, readCommitted)
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

	// A row that adds an admin (addsAdmin) takes its id and commits under the
	// sign-in notice lock held shared, so a sign-in count, which holds it
	// exclusive, either sees the row or the row's id is above that count's
	// cursor (RecordSignIn).
	if addsAdmin(ev) {
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

// detailCreatedAdminID is the detail key of an admin_created success row, and
// of an adminctl bootstrap row, that holds the created admin's id. The id
// stays in admin_audit_log: no export or security event carries the detail.
const detailCreatedAdminID = "created_admin_id"

// Detail keys of the sign-in notice. A login_success row written by
// RecordSignIn always carries detailNoticeSession; when its count was read it
// also carries detailAdminsAdded and detailNoticeCursor. A signin_notice_ack
// row carries detailNoticeCursor. The literals are repeated in the SQL below,
// so a partial index never depends on how a parameter is bound.
const (
	detailNoticeSession = "notice_session"
	detailAdminsAdded   = "admins_added"
	detailNoticeCursor  = "notice_cursor"
)

// readCommitted is the isolation of every transaction whose correctness
// relies on reading, after an advisory lock is granted, what the previous lock
// holder committed: WriteWith (so RecordSignIn) and CreatePending. A
// REPEATABLE READ or SERIALIZABLE snapshot, taken by the first statement
// before the lock wait ends, would miss those rows, so the isolation is set
// explicitly instead of inheriting default_transaction_isolation.
var readCommitted = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// advisoryKey derives a PostgreSQL advisory key from a domain string, which
// keeps each key apart from every other advisory key space.
func advisoryKey(domain string) int64 {
	digest := sha256.Sum256([]byte(domain))
	return int64(binary.BigEndian.Uint64(digest[:8])) //nolint:gosec // bit-preserving conversion into the signed advisory key space
}

// signInNoticeLockKey is the PostgreSQL advisory key that orders admin
// creations against sign-in notice reads. WriteWith takes it shared, as the
// last step before inserting a row that adds an admin (addsAdmin), and holds
// it until that row commits; RecordSignIn takes it exclusive around its count
// and its login_success insert.
var signInNoticeLockKey = advisoryKey("admin_signin_notice\x00v1")

// signInNoticeLockTimeout bounds RecordSignIn's wait for in-flight creations.
// A timeout writes the sign-in without a count.
const signInNoticeLockTimeout = "5s"

// errNoticeRead marks a RecordSignIn failure before its row was inserted:
// the lock wait or the count. An insert or commit failure wraps errAuditWrite
// instead.
var errNoticeRead = errors.New("admin sign-in notice read failed")

// Errors of AcknowledgeSignInNotice.
var (
	// ErrNoticeCursorInvalid marks a cursor this session was never shown, or
	// one above what it was shown.
	ErrNoticeCursorInvalid = errors.New("admin sign-in notice cursor was not shown to this session")
	// ErrNoticeCursorStale marks a cursor below one this admin already
	// acknowledged.
	ErrNoticeCursorStale = errors.New("admin sign-in notice cursor is below the acknowledged one")
)

// addsAdmin reports whether ev is the audit row of an added admin: a console
// creation (admin_created success) or an adminctl bootstrap.
func addsAdmin(ev AuditEvent) bool {
	if ev.Result != AuditSuccess {
		return false
	}
	switch ev.EventType {
	case EventAdminCreated:
		return true
	case EventBootstrap:
		verb, _ := ev.Detail["verb"].(string)
		return verb == verbBootstrap
	}
	return false
}

// noticeSessionBinding names an admin session in the audit log without
// storing the session id: a domain-separated SHA-256 of it. The session id is
// 256 random bits, so the digest cannot be reversed or guessed.
func noticeSessionBinding(sessionID string) string {
	digest := sha256.Sum256([]byte("admin_signin_notice_session\x00v1\x00" + sessionID))
	return hex.EncodeToString(digest[:])
}

// SignInNotice is what a sign-in tells the admin: how many admins were added
// since the last notice they acknowledged, and the cursor that acknowledges
// it, the highest admin_audit_log row id the count covered.
type SignInNotice struct {
	AdminsAdded int
	Cursor      int64
}

// SignInRecord is the outcome of RecordSignIn.
type SignInRecord struct {
	// Notice is valid only when Shown.
	Notice SignInNotice
	// Shown reports that the count was read and committed with the
	// login_success row, so the caller may show it.
	Shown bool
	// NoticeErr is why the count could not be shown: it wraps errNoticeRead
	// (lock wait or count) or errAuditWrite (insert or commit). Nil when Shown.
	NoticeErr error
}

// AdminsAddedSinceLastSignIn counts the admins added since this admin last
// acknowledged a sign-in notice, capped at MaxAdminsAddedNotice. It only
// reads; RecordSignIn reads the same count while it records a sign-in.
//
// An added admin is an admin_created success row (a console creation, by any
// admin) or an adminctl bootstrap row, matched to admin_users by the created
// admin's id and counted once while that admin still exists (the viewer's
// own creation is never counted):
//   - A success row does not prove an admin was added: when the enrollment
//     token mint fails, CreateAdmin removes the pending admin but its success
//     row stays (the log is append-only), and a retry writes another row for
//     the same username. So a removed admin is not counted, a retried one is
//     counted once, and an admin whose removal failed is counted because it
//     still exists. The match is never by username: the actor is sanitized on
//     write, so a username holding control characters would not match.
//   - An admin_created row with no well-formed id (written before the id was
//     recorded) counts as one, and so does a bootstrap row with no detail that
//     names a username, which is how the console recorded a creation before
//     this release: they can only over-count. An adminctl bootstrap row names
//     the created admin in admin_id (and, from this release, in the detail);
//     admin_id is set to NULL when that admin is deleted, so such a row is
//     not counted.
//
// The window is bounded by audit row id, never by time alone, because ts is
// the writing transaction's start time and a creation can commit long after
// it started. It starts above the floor: the greater of the highest cursor
// this admin acknowledged (AcknowledgeSignInNotice) and the cursor of their
// newest sign-in whose count was zero, which had nothing to show. Without a
// floor:
//   - When the admin has a login_success row written before this release
//     (no detailNoticeSession), the window is the rows above its id plus
//     those from ten seconds before it (the earlier rule).
//   - Otherwise every added admin counts.
//
// So a notice that was counted but never shown (a client that disconnected,
// a commit whose outcome the server never learned) is shown again, and the
// fallbacks can only over-count. A first sign-in (no login_success row yet)
// counts 0.
func (a *AuditLog) AdminsAddedSinceLastSignIn(ctx context.Context, adminID string) (int, error) {
	notice, err := signInNoticeFor(ctx, a.db, adminID)
	return notice.AdminsAdded, err
}

type auditRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// adminsAddedQueryMarker starts the count query, so a fault test can match it.
const adminsAddedQueryMarker = "/* admins added since last sign-in */"

// noticeFloorSQL is the floor of an admin's notice window ($1 is the admin):
// the highest acknowledged cursor, or the cursor of the newest sign-in that
// counted zero, whichever is greater; NULL when there is neither.
const noticeFloorSQL = `
	SELECT GREATEST(
	  (SELECT MAX((detail->>'notice_cursor')::bigint)
	     FROM admin_audit_log
	    WHERE event_type = 'signin_notice_ack' AND result = 'success'
	      AND admin_id = $1
	      AND detail->>'notice_cursor' ~ '^[0-9]{1,18}$'),
	  (SELECT (detail->>'notice_cursor')::bigint
	     FROM admin_audit_log
	    WHERE admin_id = $1 AND event_type = 'login_success' AND result = 'success'
	      AND detail->>'admins_added' = '0'
	      AND detail->>'notice_cursor' ~ '^[0-9]{1,18}$'
	    ORDER BY ts DESC
	    LIMIT 1)
	) AS cursor`

// maxAdminsAddedNoticeSQL is MaxAdminsAddedNotice as an SQL literal.
var maxAdminsAddedNoticeSQL = strconv.Itoa(MaxAdminsAddedNotice)

// signInNoticeQuery reads an admin's sign-in notice ($1 is the admin). Every
// literal the partial indexes match on is inline, so the plan is the same
// whether PostgreSQL plans the statement generically or for the bound id.
var signInNoticeQuery = adminsAddedQueryMarker + `
	WITH floor AS (` + noticeFloorSQL + `
	), legacy AS (
	  SELECT id, ts
	    FROM admin_audit_log
	   WHERE admin_id = $1 AND event_type = 'login_success' AND result = 'success'
	     AND NOT COALESCE(detail ? 'notice_session', false)
	   ORDER BY ts DESC
	   LIMIT 1
	), signed_in AS (
	  SELECT EXISTS (
	    SELECT 1 FROM admin_audit_log
	     WHERE admin_id = $1 AND event_type = 'login_success' AND result = 'success'
	  ) AS any
	), added AS (
	  SELECT l.id, l.ts,
	         l.event_type = 'admin_created' OR l.detail IS NULL AS anonymous_counts,
	         CASE WHEN l.event_type = 'admin_created'
	              THEN l.detail->>'created_admin_id'
	              WHEN l.detail IS NULL THEN NULL
	              ELSE COALESCE(l.detail->>'created_admin_id', l.admin_id::text)
	         END AS created_id
	    FROM admin_audit_log l
	   WHERE (l.event_type = 'admin_created' AND l.result = 'success')
	      OR (l.event_type = 'bootstrap' AND l.result = 'success'
	          AND ((l.detail IS NULL AND l.actor IS NOT NULL) OR l.detail->>'verb' = 'bootstrap'))
	), windowed AS (
	  SELECT a.anonymous_counts,
	         CASE WHEN a.created_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
	              THEN a.created_id::uuid END AS id
	    FROM added a
	    CROSS JOIN signed_in s
	    CROSS JOIN floor f
	    LEFT JOIN legacy p ON true
	   WHERE s.any AND a.created_id IS DISTINCT FROM $1::text
	     AND CASE WHEN f.cursor IS NOT NULL THEN a.id > f.cursor
	              WHEN p.id IS NOT NULL THEN a.id > p.id OR a.ts > p.ts - interval '10 seconds'
	              ELSE true END
	)
	SELECT LEAST(
	         (SELECT COUNT(DISTINCT u.id) FROM windowed w JOIN admin_users u ON u.id = w.id)
	         + (SELECT COUNT(*) FROM windowed WHERE id IS NULL AND anonymous_counts),
	         ` + maxAdminsAddedNoticeSQL + `)::int,
	       GREATEST(COALESCE((SELECT MAX(id) FROM added), 0),
	                COALESCE((SELECT cursor FROM floor), 0))::bigint
`

func signInNoticeFor(ctx context.Context, db auditRowQuerier, adminID string) (SignInNotice, error) {
	var notice SignInNotice
	if err := db.QueryRowContext(ctx, signInNoticeQuery, adminID).Scan(&notice.AdminsAdded, &notice.Cursor); err != nil {
		return SignInNotice{}, fmt.Errorf("count admins added since sign-in: %w", err)
	}
	return notice, nil
}

// RecordSignIn writes this admin's login_success row, bound to the new
// session, and returns the sign-in notice (AdminsAddedSinceLastSignIn). The
// returned error is set only when no login_success row could be written.
//
// The count and the row commit in one READ COMMITTED transaction that holds
// signInNoticeLockKey exclusive, and every row that adds an admin is inserted
// and committed under the same key held shared. So each such row either
// committed before the count, and has an id at or below the notice cursor, or
// took its id after this transaction committed, and has a higher one. The
// row records the count and the cursor it shows, keyed to the session
// (detailNoticeSession), and the window moves only when that session
// acknowledges the cursor (AcknowledgeSignInNotice) or the count was zero. A
// notice the admin never saw is therefore shown again at the next sign-in.
//
// When the count cannot be read, or that transaction fails for any other
// reason, the row is written again without the lock and without a count. It
// moves no window, so a failed read never drops a creation.
func (a *AuditLog) RecordSignIn(ctx context.Context, adminID, actor, sessionID string) (SignInRecord, error) {
	id := adminID
	binding := noticeSessionBinding(sessionID)
	ev := AuditEvent{AdminID: &id, Actor: actor, EventType: EventLoginSuccess, Result: AuditSuccess}
	var notice SignInNotice
	noticeErr := a.WriteWith(ctx, ev, func(tx *sql.Tx, ev *AuditEvent) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '"+signInNoticeLockTimeout+"'"); err != nil {
			return fmt.Errorf("%w: bound sign-in notice lock wait: %w", errNoticeRead, err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, signInNoticeLockKey); err != nil {
			return fmt.Errorf("%w: take sign-in notice lock: %w", errNoticeRead, err)
		}
		read, err := signInNoticeFor(ctx, tx, adminID)
		if err != nil {
			return fmt.Errorf("%w: %w", errNoticeRead, err)
		}
		notice = read
		ev.Detail = map[string]any{
			detailNoticeSession: binding,
			detailAdminsAdded:   read.AdminsAdded,
			detailNoticeCursor:  read.Cursor,
		}
		return nil
	})
	if noticeErr == nil {
		return SignInRecord{Notice: notice, Shown: true}, nil
	}
	ev.Detail = map[string]any{detailNoticeSession: binding}
	if werr := a.Write(ctx, ev); werr != nil {
		return SignInRecord{NoticeErr: noticeErr}, errors.Join(noticeErr, werr)
	}
	return SignInRecord{NoticeErr: noticeErr}, nil
}

// noticeShownQuery reads the cursor the sign-in of one session showed ($1 is
// the admin, $2 the session binding).
const noticeShownQuery = `
	SELECT detail->>'notice_cursor'
	  FROM admin_audit_log
	 WHERE admin_id = $1 AND event_type = 'login_success' AND result = 'success'
	   AND detail->>'notice_session' = $2
	 ORDER BY ts DESC
	 LIMIT 1
`

// AcknowledgeSignInNotice records that this admin's session displayed its
// sign-in notice, so the next window starts above cursor. The cursor must be
// at most the one the server showed this session (the login_success row bound
// to it), so a client cannot skip creations it was never shown
// (ErrNoticeCursorInvalid), and at least this admin's current floor
// (ErrNoticeCursorStale). A cursor equal to the floor is already
// acknowledged and writes nothing. It needs no lock: every row that adds an
// admin at or below the cursor had committed when the cursor was counted.
func (a *AuditLog) AcknowledgeSignInNotice(ctx context.Context, adminID, sessionID string, cursor int64) error {
	if cursor <= 0 {
		return ErrNoticeCursorInvalid
	}
	var shown sql.NullString
	err := a.db.QueryRowContext(ctx, noticeShownQuery, adminID, noticeSessionBinding(sessionID)).Scan(&shown)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNoticeCursorInvalid
	case err != nil:
		return fmt.Errorf("read shown sign-in notice cursor: %w", err)
	}
	if !shown.Valid {
		return ErrNoticeCursorInvalid
	}
	shownCursor, err := strconv.ParseInt(shown.String, 10, 64)
	if err != nil || cursor > shownCursor {
		return ErrNoticeCursorInvalid
	}
	var floor sql.NullInt64
	if err := a.db.QueryRowContext(ctx, noticeFloorSQL, adminID).Scan(&floor); err != nil {
		return fmt.Errorf("read sign-in notice floor: %w", err)
	}
	if floor.Valid {
		if cursor < floor.Int64 {
			return ErrNoticeCursorStale
		}
		if cursor == floor.Int64 {
			return nil
		}
	}
	id := adminID
	return a.Write(ctx, AuditEvent{
		AdminID: &id, EventType: EventSignInNoticeAck, Result: AuditSuccess,
		Detail: map[string]any{detailNoticeCursor: cursor},
	})
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
