package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Sentinel errors for the admin repository.
var (
	// ErrAdminNotFound is returned when no admin matches the lookup.
	ErrAdminNotFound = errors.New("admin: admin not found")
	// ErrDuplicateUsername is returned when CreatePending hits the UNIQUE(username) constraint.
	ErrDuplicateUsername = errors.New("admin: username already exists")
	// ErrDuplicateCredential is returned when AddCredential hits a UNIQUE constraint:
	// the attestation reused a credential ID that is already registered. That is the
	// client's input, not a storage fault, so enrollment answers it as a rejection.
	ErrDuplicateCredential = errors.New("admin: credential already registered")
	// ErrEnrollmentStateChanged is returned when an enrollment ceremony is stale.
	ErrEnrollmentStateChanged = errors.New("admin: enrollment state changed")
	// ErrAdminAuthenticationStateChanged is returned when login state is stale.
	ErrAdminAuthenticationStateChanged = errors.New("admin: authentication state changed")
)

// pgUniqueViolation is the Postgres SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// AdminRepo provides parameterized CRUD over admin_users and
// admin_webauthn_credentials (#1688). It uses database/sql (*sql.DB) consistent
// with the rest of the control-plane.
//
//nolint:revive // Admin* prefix is the #1688 cross-task naming contract (see types.go header).
type AdminRepo struct {
	db *sql.DB
}

// NewAdminRepo wires an AdminRepo against the given DB.
func NewAdminRepo(db *sql.DB) *AdminRepo {
	return &AdminRepo{db: db}
}

// adminCreateLockKey is the PostgreSQL advisory key that serializes admin
// inserts, so the confusable-username check and the insert of one creation
// run before the next creation reads the usernames. It is taken
// transaction-scoped, before signInNoticeLockKey, by every path that inserts
// an admin row (createPendingTx). The domain string keeps it apart from every
// other advisory key space.
var adminCreateLockKey = advisoryKey("admin_create\x00v1")

// CreatePending inserts a new admin in the pending_enrollment state in its own
// transaction. It returns ErrInvalidUsername if ValidateAdminUsername refuses
// the username, ErrDuplicateUsername if the username is already taken and
// ErrConfusableUsername if it looks like an existing admin's.
func (r *AdminRepo) CreatePending(ctx context.Context, username, passwordHash string) (AdminUser, error) {
	if err := ValidateAdminUsername(username); err != nil {
		return AdminUser{}, err
	}
	tx, err := r.db.BeginTx(ctx, readCommitted)
	if err != nil {
		return AdminUser{}, fmt.Errorf("begin create pending admin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	u, err := r.CreatePendingTx(ctx, tx, username, passwordHash)
	if err != nil {
		return AdminUser{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdminUser{}, fmt.Errorf("commit create pending admin: %w", err)
	}
	return u, nil
}

// CreatePendingTx is CreatePending inside the caller's transaction, so the
// row commits only with whatever else that transaction writes. The caller's
// transaction must be READ COMMITTED (readCommitted): the username read after
// the advisory lock must see the admin the previous lock holder committed.
//
// It takes adminCreateLockKey, which the transaction then holds until it ends,
// reads every admin username and refuses a new name whose
// UsernameSkeleton equals an existing one's. Two concurrent creations of
// lookalike names therefore cannot both pass the check.
func (r *AdminRepo) CreatePendingTx(ctx context.Context, tx *sql.Tx, username, passwordHash string) (AdminUser, error) {
	if err := ValidateAdminUsername(username); err != nil {
		return AdminUser{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, adminCreateLockKey); err != nil {
		return AdminUser{}, fmt.Errorf("take admin create lock: %w", err)
	}
	if err := refuseConfusable(ctx, tx, username); err != nil {
		return AdminUser{}, err
	}
	const q = `
		INSERT INTO admin_users (username, password_hash, status)
		VALUES ($1, $2, $3)
		RETURNING id, username, password_hash, status, credential_generation, session_generation,
		          created_at, updated_at, disabled_at
	`
	var u AdminUser
	var status string
	var disabledAt sql.NullTime
	err := tx.QueryRowContext(ctx, q, username, passwordHash, string(StatusPending)).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &status, &u.CredentialGeneration, &u.SessionGeneration,
		&u.CreatedAt, &u.UpdatedAt, &disabledAt,
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgUniqueViolation {
			return AdminUser{}, ErrDuplicateUsername
		}
		return AdminUser{}, fmt.Errorf("create pending admin: %w", err)
	}
	u.Status = AdminStatus(status)
	if disabledAt.Valid {
		u.DisabledAt = &disabledAt.Time
	}
	return u, nil
}

// CheckUsernameAvailable reports, without a lock, whether a new admin could
// take this username: ErrInvalidUsername, ErrDuplicateUsername or
// ErrConfusableUsername when not. CreateAdmin calls it before the step-up is
// redeemed, so a refused name does not spend the proof; CreatePendingTx checks
// again under the lock.
func (r *AdminRepo) CheckUsernameAvailable(ctx context.Context, username string) error {
	if err := ValidateAdminUsername(username); err != nil {
		return err
	}
	return refuseConfusable(ctx, r.db, username)
}

type usernameQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// refuseConfusable returns ErrDuplicateUsername when an admin has exactly
// this username and ErrConfusableUsername when one has the same
// UsernameSkeleton. Admins are few, so it reads every username.
func refuseConfusable(ctx context.Context, db usernameQuerier, username string) error {
	rows, err := db.QueryContext(ctx, `SELECT username FROM admin_users`)
	if err != nil {
		return fmt.Errorf("read admin usernames: %w", err)
	}
	defer func() { _ = rows.Close() }()
	skeleton := UsernameSkeleton(username)
	confusable := false
	for rows.Next() {
		var existing string
		if err := rows.Scan(&existing); err != nil {
			return fmt.Errorf("read admin usernames: %w", err)
		}
		if existing == username {
			return ErrDuplicateUsername
		}
		if UsernameSkeleton(existing) == skeleton {
			confusable = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read admin usernames: %w", err)
	}
	if confusable {
		return ErrConfusableUsername
	}
	return nil
}

// GetByUsername returns the admin with the given username, or ErrAdminNotFound.
func (r *AdminRepo) GetByUsername(ctx context.Context, username string) (*AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, status, credential_generation, session_generation,
		       created_at, updated_at, disabled_at
		FROM admin_users WHERE username = $1
	`
	var u AdminUser
	var status string
	var disabledAt sql.NullTime
	err := r.db.QueryRowContext(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &status, &u.CredentialGeneration, &u.SessionGeneration,
		&u.CreatedAt, &u.UpdatedAt, &disabledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAdminNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get admin by username: %w", err)
	}
	u.Status = AdminStatus(status)
	if disabledAt.Valid {
		u.DisabledAt = &disabledAt.Time
	}
	return &u, nil
}

// ActiveSessionGeneration returns the durable generation for an active admin.
func (r *AdminRepo) ActiveSessionGeneration(ctx context.Context, adminID string) (int64, error) {
	const q = `SELECT session_generation FROM admin_users WHERE id = $1 AND status = $2`
	var generation int64
	err := r.db.QueryRowContext(ctx, q, adminID, string(StatusActive)).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrAdminNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get active admin session generation: %w", err)
	}
	return generation, nil
}

// SetStatus updates an admin's status (and touches updated_at). Returns
// ErrAdminNotFound when no row matches the id.
func (r *AdminRepo) SetStatus(ctx context.Context, adminID string, status AdminStatus) error {
	const q = `UPDATE admin_users SET status = $2, updated_at = now() WHERE id = $1`
	res, err := r.db.ExecContext(ctx, q, adminID, string(status))
	if err != nil {
		return fmt.Errorf("set admin status: %w", err)
	}
	return requireOneRow(res)
}

// Disable marks an admin disabled and records disabled_at. Returns
// ErrAdminNotFound when no row matches the id.
func (r *AdminRepo) Disable(ctx context.Context, adminID string) error {
	const q = `
		UPDATE admin_users
		SET status = $2, disabled_at = now(), updated_at = now()
		WHERE id = $1
	`
	res, err := r.db.ExecContext(ctx, q, adminID, string(StatusDisabled))
	if err != nil {
		return fmt.Errorf("disable admin: %w", err)
	}
	return requireOneRow(res)
}

// AddCredential persists a registered WebAuthn credential for an admin and
// returns the stored row (with its generated id + created_at).
func (r *AdminRepo) AddCredential(ctx context.Context, c AdminCredential) (AdminCredential, error) {
	return addCredential(ctx, r.db, c)
}

type adminCredentialQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func addCredential(ctx context.Context, queryer adminCredentialQueryer, c AdminCredential) (AdminCredential, error) {
	const q = `
		INSERT INTO admin_webauthn_credentials
			(admin_id, credential_id, public_key, aaguid, sign_count, credential_name, transports)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at
	`
	var name sql.NullString
	if c.CredentialName != "" {
		name = sql.NullString{String: c.CredentialName, Valid: true}
	}
	err := queryer.QueryRowContext(ctx, q,
		c.AdminID, c.CredentialID, c.PublicKey, c.AAGUID, c.SignCount, name, pq.Array(c.Transports),
	).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgUniqueViolation {
			return AdminCredential{}, ErrDuplicateCredential
		}
		return AdminCredential{}, fmt.Errorf("add admin credential: %w", err)
	}
	return c, nil
}

// CompleteEnrollment adds a credential and activates a still-current ceremony.
func (r *AdminRepo) CompleteEnrollment(ctx context.Context, expectedGeneration int64, c AdminCredential) (stored AdminCredential, retErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminCredential{}, fmt.Errorf("begin complete admin enrollment: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback complete admin enrollment: %w", rollbackErr))
		}
	}()
	var status string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT status, credential_generation FROM admin_users WHERE id = $1 FOR UPDATE`, c.AdminID).Scan(&status, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminCredential{}, ErrAdminNotFound
	}
	if err != nil {
		return AdminCredential{}, fmt.Errorf("lock admin enrollment state: %w", err)
	}
	if AdminStatus(status) != StatusPending || generation != expectedGeneration {
		return AdminCredential{}, ErrEnrollmentStateChanged
	}
	stored, err = addCredential(ctx, tx, c)
	if err != nil {
		return AdminCredential{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE admin_users SET status = $2, updated_at = now() WHERE id = $1`, c.AdminID, string(StatusActive))
	if err != nil {
		return AdminCredential{}, fmt.Errorf("activate enrolled admin: %w", err)
	}
	if err := requireOneRow(res); err != nil {
		return AdminCredential{}, fmt.Errorf("activate enrolled admin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AdminCredential{}, fmt.Errorf("commit complete admin enrollment: %w", err)
	}
	return stored, nil
}

// ResetEnrollment atomically revokes credentials and advances both authority fences.
func (r *AdminRepo) ResetEnrollment(ctx context.Context, adminID string) (revoked int64, previousGeneration int64, newGeneration int64, retErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin reset admin enrollment: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback reset admin enrollment: %w", rollbackErr))
		}
	}()
	err = tx.QueryRowContext(ctx, `
		UPDATE admin_users
		SET status = $2, credential_generation = credential_generation + 1,
		    session_generation = session_generation + 1, updated_at = now()
		WHERE id = $1
		RETURNING credential_generation - 1, credential_generation
	`, adminID, string(StatusPending)).Scan(&previousGeneration, &newGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, ErrAdminNotFound
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("advance admin credential generation: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("delete admin credentials: %w", err)
	}
	revoked, err = res.RowsAffected()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count revoked admin credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("commit reset admin enrollment: %w", err)
	}
	return revoked, previousGeneration, newGeneration, nil
}

// FinalizeWebAuthnLogin persists a verified assertion before issuing its session.
func (r *AdminRepo) FinalizeWebAuthnLogin(ctx context.Context, sessions *SessionStore, adminID string, credentialID []byte, signCount int64) (_ string, retErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin admin login finalization: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback admin login finalization: %w", rollbackErr))
		}
	}()
	var activeAdminID string
	var sessionGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT id, session_generation FROM admin_users WHERE id = $1 AND status = $2 FOR UPDATE`, adminID, string(StatusActive)).Scan(&activeAdminID, &sessionGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAdminAuthenticationStateChanged
	}
	if err != nil {
		return "", fmt.Errorf("lock active admin for login: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE admin_webauthn_credentials
		SET sign_count = $1, last_used_at = now()
		WHERE credential_id = $2 AND admin_id = $3
		  AND (sign_count = 0 OR $1 > sign_count)
	`, signCount, credentialID, activeAdminID)
	if err != nil {
		return "", fmt.Errorf("persist login credential state: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("persist login credential state rows: %w", err)
	}
	if rows != 1 {
		return "", ErrAdminAuthenticationStateChanged
	}
	sid, err := sessions.Mint(ctx, activeAdminID, sessionGeneration)
	if err != nil {
		return "", fmt.Errorf("mint admin session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		revokeErr := sessions.Revoke(cleanupCtx, sid)
		cancel()
		return "", errors.Join(fmt.Errorf("commit admin login finalization: %w", err), revokeErr)
	}
	return sid, nil
}

// UpdateCredentialSignCount persists the authenticator's advanced sign counter
// after a successful assertion, and bumps last_used_at. The sign count is
// WebAuthn's clone-detection signal: go-webauthn validates each assertion's
// counter against the stored value, so the stored value MUST advance — otherwise
// a cloned authenticator replaying a stale counter is never detected (#1688,
// Gitar finding). Keyed by the unique credential_id.
func (r *AdminRepo) UpdateCredentialSignCount(ctx context.Context, credentialID []byte, signCount int64) error {
	const q = `
		UPDATE admin_webauthn_credentials
		SET sign_count = $1, last_used_at = now()
		WHERE credential_id = $2
		  AND (sign_count = 0 OR $1 > sign_count)
	`
	res, err := r.db.ExecContext(ctx, q, signCount, credentialID)
	if err != nil {
		return fmt.Errorf("update credential sign count: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update credential sign count rows: %w", err)
	}
	if n == 0 {
		return errSignCountNotAdvanced
	}
	if n != 1 {
		return fmt.Errorf("update credential sign count: expected 1 row, got %d", n)
	}
	return nil
}

// errSignCountNotAdvanced reports that no stored counter was below the new
// one: the assertion replayed a counter (or the credential is gone), which
// is a verification failure, not a storage fault.
var errSignCountNotAdvanced = errors.New("update credential sign count: counter did not advance")

// ListCredentials returns all WebAuthn credentials bound to an admin, ordered by
// creation time. The slice is empty (not nil error) when the admin has none.
func (r *AdminRepo) ListCredentials(ctx context.Context, adminID string) ([]AdminCredential, error) {
	const q = `
		SELECT id, admin_id, credential_id, public_key, aaguid, sign_count,
		       credential_name, transports, created_at, last_used_at
		FROM admin_webauthn_credentials
		WHERE admin_id = $1
		ORDER BY created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, q, adminID)
	if err != nil {
		return nil, fmt.Errorf("list admin credentials: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is not actionable (matches repo-wide pattern)

	var creds []AdminCredential
	for rows.Next() {
		var c AdminCredential
		var name sql.NullString
		var lastUsed sql.NullTime
		if err := rows.Scan(
			&c.ID, &c.AdminID, &c.CredentialID, &c.PublicKey, &c.AAGUID, &c.SignCount,
			&name, pq.Array(&c.Transports), &c.CreatedAt, &lastUsed,
		); err != nil {
			return nil, fmt.Errorf("scan admin credential: %w", err)
		}
		if name.Valid {
			c.CredentialName = name.String
		}
		if lastUsed.Valid {
			c.LastUsedAt = &lastUsed.Time
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin credentials: %w", err)
	}
	return creds, nil
}

// DeleteCredentials removes ALL WebAuthn credentials bound to an admin.
// `adminctl reset-enrollment` does not use it: ResetEnrollment removes the
// credentials in the same transaction that advances both generations. Returns the number of rows removed (0 when the
// admin already had no credentials — not an error).
func (r *AdminRepo) DeleteCredentials(ctx context.Context, adminID string) error {
	const q = `DELETE FROM admin_webauthn_credentials WHERE admin_id = $1`
	if _, err := r.db.ExecContext(ctx, q, adminID); err != nil {
		return fmt.Errorf("delete admin credentials: %w", err)
	}
	return nil
}

// DeleteUnenrolledPending removes the admin row CreateAdmin just committed
// when its enrollment token could not be minted, so a retry of the same
// username is not refused as a duplicate. It deletes only while the row is
// still that unenrolled shape: pending_enrollment, at the credential
// generation it was created with (no reset-enrollment has minted a token for
// it since), and holding no WebAuthn credential. Returns ErrAdminNotFound when
// no row matches, which leaves the row for an operator.
func (r *AdminRepo) DeleteUnenrolledPending(ctx context.Context, adminID string, credentialGeneration int64) error {
	const q = `
		DELETE FROM admin_users u
		WHERE u.id = $1 AND u.status = $2 AND u.credential_generation = $3
		  AND NOT EXISTS (SELECT 1 FROM admin_webauthn_credentials c WHERE c.admin_id = u.id)
	`
	res, err := r.db.ExecContext(ctx, q, adminID, string(StatusPending), credentialGeneration)
	if err != nil {
		return fmt.Errorf("delete unenrolled pending admin: %w", err)
	}
	return requireOneRow(res)
}

// CountActiveCredentials returns how many WebAuthn credentials an admin holds.
// Used by revocation flows to refuse removing an admin's last key.
func (r *AdminRepo) CountActiveCredentials(ctx context.Context, adminID string) (int, error) {
	const q = `SELECT COUNT(*) FROM admin_webauthn_credentials WHERE admin_id = $1`
	var n int
	if err := r.db.QueryRowContext(ctx, q, adminID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admin credentials: %w", err)
	}
	return n, nil
}

// requireOneRow maps a zero-rows-affected result to ErrAdminNotFound.
func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrAdminNotFound
	}
	return nil
}
