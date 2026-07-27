package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	// TokenTheftRecoveryInterval bounds how long a durable pending sweep can wait
	// after a transient database or Redis failure.
	TokenTheftRecoveryInterval = 10 * time.Second
	tokenTheftRecoveryTimeout  = 5 * time.Second
)

// EnqueueTokenTheftRevocation records a theft-triggered sweep before it starts,
// allowing another process to resume it after a crash.
func EnqueueTokenTheftRevocation(ctx context.Context, db *sql.DB, userID string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO token_theft_pending_revocations (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET updated_at = NOW()`, userID)
	if err != nil {
		return fmt.Errorf("enqueue token theft revocation: %w", err)
	}
	return nil
}

// ProcessTokenTheftRevocation keeps the deny marker set until all active refresh
// tokens are durably revoked. The pending row is removed only after the marker
// clears, so a crash in either order is replayed safely.
func ProcessTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string) error {
	if redisClient == nil {
		return errors.New("token theft block store unavailable")
	}
	if err := redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), "1", 0).Err(); err != nil {
		return fmt.Errorf("set token theft block: %w", err)
	}
	if _, err := RevokeAllRefreshTokens(ctx, db, userID, RevokeAllRefreshTokensOptions{}); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	if err := redisClient.Del(ctx, middleware.TokenTheftBlockedKey(userID)).Err(); err != nil {
		return fmt.Errorf("clear token theft block: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM token_theft_pending_revocations WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("complete token theft revocation: %w", err)
	}
	return nil
}

// RecoverPendingTokenTheftRevocations replays durable theft sweeps. It is safe
// for several replicas to run: each sweep serializes through the user-row lock.
func RecoverPendingTokenTheftRevocations(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger) error {
	rows, err := db.QueryContext(ctx,
		`SELECT user_id FROM token_theft_pending_revocations ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("list pending token theft revocations: %w", err)
	}
	var userIDs []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return errors.Join(
					fmt.Errorf("scan pending token theft revocation: %w", err),
					fmt.Errorf("close pending token theft revocations: %w", closeErr),
				)
			}
			return fmt.Errorf("scan pending token theft revocation: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return errors.Join(
				fmt.Errorf("iterate pending token theft revocations: %w", err),
				fmt.Errorf("close pending token theft revocations: %w", closeErr),
			)
		}
		return fmt.Errorf("iterate pending token theft revocations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close pending token theft revocations: %w", err)
	}

	var errs []error
	for _, userID := range userIDs {
		attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
		err := ProcessTokenTheftRevocation(attemptCtx, db, redisClient, userID)
		cancel()
		if err != nil {
			log.Error("Pending token theft revocation recovery failed", "error", err, "user_id", userID)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// StartTokenTheftRecoveryWorker replays pending theft sweeps at startup and on
// a fixed interval. It stops when ctx is cancelled.
func StartTokenTheftRecoveryWorker(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		run := func() {
			if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log); err != nil {
				log.Error("Pending token theft revocation recovery failed", "error", err)
			}
		}
		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
