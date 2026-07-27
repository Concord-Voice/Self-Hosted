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
	tokenTheftFallbackSetKey   = "token_theft_pending_revocations:fallback" // #nosec G101 -- Redis namespace, not a credential
)

var enqueueTokenTheftFallbackScript = redis.NewScript(`
redis.call("SADD", KEYS[1], ARGV[1])
redis.call("SET", KEYS[2], "1")
return 1
`)

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

// queueTokenTheftRevocation records the normal PostgreSQL recovery row. When
// that store is temporarily unavailable, it atomically persists a Redis deny
// marker and fallback record so recovery can promote it once PostgreSQL returns.
func queueTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string) (queuedInDB bool, err error) {
	if err := EnqueueTokenTheftRevocation(ctx, db, userID); err == nil {
		return true, nil
	} else if redisClient == nil {
		return false, fmt.Errorf("persist token theft revocation fallback: %w", err)
	} else if _, fallbackErr := enqueueTokenTheftFallbackScript.Run(ctx, redisClient,
		[]string{tokenTheftFallbackSetKey, middleware.TokenTheftBlockedKey(userID)}, userID,
	).Result(); fallbackErr != nil {
		return false, errors.Join(
			fmt.Errorf("enqueue token theft revocation: %w", err),
			fmt.Errorf("persist token theft revocation fallback: %w", fallbackErr),
		)
	}
	return false, nil
}

// ProcessTokenTheftRevocation keeps the deny marker set until all active refresh
// tokens are durably revoked. It locks the pending row while sweeping, so stale
// recovery workers cannot revoke a session minted after another worker completed.
func ProcessTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string) (err error) {
	if redisClient == nil {
		return errors.New("token theft block store unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token theft revocation transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft revocation transaction: %w", rollbackErr))
		}
	}()

	var pendingUserID string
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM token_theft_pending_revocations WHERE user_id = $1 FOR UPDATE`, userID,
	).Scan(&pendingUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("commit token theft no-op transaction: %w", commitErr)
			}
			return nil
		}
		return fmt.Errorf("lock pending token theft revocation: %w", err)
	}
	if err := redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), "1", 0).Err(); err != nil {
		return fmt.Errorf("set token theft block: %w", err)
	}
	if _, err := revokeAllRefreshTokensTx(ctx, tx, userID, RevokeAllRefreshTokensOptions{}); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	if err := redisClient.SRem(ctx, tokenTheftFallbackSetKey, userID).Err(); err != nil {
		return fmt.Errorf("complete token theft revocation fallback: %w", err)
	}
	if err := redisClient.Del(ctx, middleware.TokenTheftBlockedKey(userID)).Err(); err != nil {
		return fmt.Errorf("clear token theft block: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM token_theft_pending_revocations WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("complete token theft revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token theft revocation transaction: %w", err)
	}
	return nil
}

// RecoverPendingTokenTheftRevocations replays durable theft sweeps. It is safe
// for several replicas to run: ProcessTokenTheftRevocation locks each pending row.
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
	if redisClient == nil {
		errs = append(errs, errors.New("list token theft revocation fallbacks: token theft block store unavailable"))
	} else {
		fallbackUserIDs, fallbackErr := redisClient.SMembers(ctx, tokenTheftFallbackSetKey).Result()
		if fallbackErr != nil {
			errs = append(errs, fmt.Errorf("list token theft revocation fallbacks: %w", fallbackErr))
		} else {
			seenUserIDs := make(map[string]struct{}, len(userIDs))
			for _, userID := range userIDs {
				seenUserIDs[userID] = struct{}{}
			}
			for _, userID := range fallbackUserIDs {
				if _, seen := seenUserIDs[userID]; seen {
					continue
				}
				attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
				err := EnqueueTokenTheftRevocation(attemptCtx, db, userID)
				cancel()
				if err != nil {
					log.Error("Token theft fallback promotion failed", "error", err, "user_id", userID)
					errs = append(errs, err)
					continue
				}
				seenUserIDs[userID] = struct{}{}
				userIDs = append(userIDs, userID)
			}
		}
	}
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

// StartTokenTheftRecoveryWorker completes startup recovery before returning, then
// replays pending theft sweeps on a fixed interval until ctx is cancelled.
func StartTokenTheftRecoveryWorker(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, interval time.Duration) error {
	if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log); err != nil {
		return fmt.Errorf("startup token theft revocation recovery: %w", err)
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		run := func() {
			if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log); err != nil {
				log.Error("Pending token theft revocation recovery failed", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return nil
}
