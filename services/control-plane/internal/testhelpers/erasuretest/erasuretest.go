// Package erasuretest gives tests outside internal/users a real fresh-proof
// account erasure. AccountService.DeleteAccount has no proofless entry point
// outside that package, so these callers present the same password proof and
// owned-server confirmation a client does.
//
// It imports neither testhelpers nor the API router, so an internal test
// package (such as billing) can use it without an import cycle.
package erasuretest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
)

// fixturePassword is the proof every enrolled user presents.
const fixturePassword = "erasure-fixture-passphrase" // pragma: allowlist secret

// Eraser holds one test's fixture password hash and the Redis that backs the
// password-attempt reservation.
type Eraser struct {
	db   *sql.DB
	rdb  *redis.Client
	hash string
}

// New builds an Eraser whose lockout reservations use a private miniredis.
func New(t testing.TB, db *sql.DB) *Eraser {
	t.Helper()
	hash, err := auth.HashPasswordWithParams(fixturePassword, &auth.Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16})
	if err != nil {
		t.Fatalf("erasuretest: hash fixture password: %v", err)
	}
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Eraser{db: db, rdb: rdb, hash: hash}
}

// Enroll sets the fixture password on userID. Call it before any concurrent
// phase, so Erase adds no users-row write of its own to a lock-order test.
func (e *Eraser) Enroll(t testing.TB, userID string) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, e.hash); err != nil {
		t.Fatalf("erasuretest: enroll fixture password: %v", err)
	}
}

// Erase deletes an enrolled user's account with the actor identity bound to
// ctx, the fixture password and the user's current owned-server set. It
// reports failure as an error, so it is safe off the test goroutine.
func (e *Eraser) Erase(ctx context.Context, svc *users.AccountService, userID string) error {
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	if !ok {
		return errors.New("erasuretest: context carries no actor identity")
	}
	attempt, stepErr := stepup.ReserveUserPasswordAttempt(ctx, e.db, e.rdb, userID, fixturePassword)
	if stepErr != nil {
		return fmt.Errorf("erasuretest: reserve password attempt: %w", stepErr)
	}
	owned, err := ownedServers(ctx, e.db, userID)
	if err != nil {
		return err
	}
	return svc.DeleteAccount(ctx, users.AccountErasureRequest{
		Actor:                   identity,
		Password:                fixturePassword,
		PasswordAttempt:         attempt,
		ConfirmedOwnedServerIDs: owned,
	})
}

// MustErase enrolls userID and erases it, failing t on any error.
func (e *Eraser) MustErase(ctx context.Context, t testing.TB, svc *users.AccountService, userID string) {
	t.Helper()
	e.Enroll(t, userID)
	if err := e.Erase(ctx, svc, userID); err != nil {
		t.Fatalf("erasuretest: erase account: %v", err)
	}
}

func ownedServers(ctx context.Context, db *sql.DB, userID string) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM servers WHERE owner_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("erasuretest: read owned servers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owned := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("erasuretest: scan owned server: %w", err)
		}
		owned = append(owned, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("erasuretest: iterate owned servers: %w", err)
	}
	return owned, nil
}
