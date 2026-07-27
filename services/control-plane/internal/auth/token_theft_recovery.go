package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// TokenTheftRecoveryInterval bounds how long a durable pending sweep can wait
	// after a transient database or Redis failure.
	TokenTheftRecoveryInterval     = 10 * time.Second
	tokenTheftRecoveryTimeout      = 5 * time.Second
	tokenTheftLegacyStateValue     = "1"
	tokenTheftLegacyFallbackSetKey = "token_theft_pending_revocations:fallback"    // #nosec G101 -- Redis namespace, not a credential
	tokenTheftFallbackHashKey      = "token_theft_pending_revocations:fallback:v2" // #nosec G101 -- Redis namespace, not a credential
)

var persistTokenTheftRecoveryStateScript = redis.NewScript(`
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1
`)

var finalizeTokenTheftRecoveryStateScript = redis.NewScript(`
if redis.call("HGET", KEYS[1], ARGV[1]) == ARGV[2] then
  redis.call("HDEL", KEYS[1], ARGV[1])
end
if redis.call("GET", KEYS[2]) == ARGV[2] then
  redis.call("DEL", KEYS[2])
end
return 1
`)

var finalizeLegacyTokenTheftRecoveryStateScript = redis.NewScript(`
redis.call("SREM", KEYS[1], ARGV[1])
if redis.call("GET", KEYS[2]) == ARGV[2] then
  redis.call("DEL", KEYS[2])
end
return 1
`)

func newTokenTheftRecoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), tokenTheftRecoveryTimeout)
}

// EnqueueTokenTheftRevocation records a theft-triggered sweep before it starts,
// allowing another process to resume it after a crash.
func EnqueueTokenTheftRevocation(ctx context.Context, db *sql.DB, userID string) error {
	operationID := uuid.NewString()
	return enqueueTokenTheftRevocation(ctx, db, userID, &operationID)
}

func enqueueTokenTheftRevocation(ctx context.Context, db *sql.DB, userID string, operationID *string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO token_theft_pending_revocations (user_id, operation_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE
		SET operation_id = EXCLUDED.operation_id, completed_at = NULL, updated_at = NOW()`, userID, operationID)
	if err != nil {
		return fmt.Errorf("enqueue token theft revocation: %w", err)
	}
	return nil
}

// queueTokenTheftRevocation records the normal PostgreSQL recovery row. When
// that store is temporarily unavailable, it atomically persists a Redis deny
// marker and fallback record so recovery can promote it once PostgreSQL returns.
func queueTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string) (queuedInDB bool, err error) {
	operationID := uuid.NewString()
	enqueueErr := enqueueTokenTheftRevocation(ctx, db, userID, &operationID)
	if redisClient == nil {
		if enqueueErr != nil {
			return false, errors.Join(
				fmt.Errorf("enqueue token theft revocation: %w", enqueueErr),
				errors.New("persist token theft recovery state: token theft block store unavailable"),
			)
		}
		return true, errors.New("persist token theft recovery state: token theft block store unavailable")
	}
	fallbackCtx, cancelFallback := newTokenTheftRecoveryContext(ctx)
	defer cancelFallback()
	if _, stateErr := persistTokenTheftRecoveryStateScript.Run(fallbackCtx, redisClient,
		[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID)}, userID, operationID,
	).Result(); stateErr != nil {
		if enqueueErr != nil {
			return false, errors.Join(
				fmt.Errorf("enqueue token theft revocation: %w", enqueueErr),
				fmt.Errorf("persist token theft recovery state: %w", stateErr),
			)
		}
		return true, fmt.Errorf("persist token theft recovery state: %w", stateErr)
	}
	if enqueueErr == nil {
		return true, nil
	}
	return false, nil
}

// ProcessTokenTheftRevocation keeps the deny marker set until all active refresh
// tokens are durably revoked. A completed row retains cleanup state after the
// SQL commit, so a Redis cleanup retry never repeats the token sweep.
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

	var operationID sql.NullString
	var completedAt sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1 FOR UPDATE`, userID,
	).Scan(&operationID, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("commit token theft no-op transaction: %w", commitErr)
			}
			return nil
		}
		return fmt.Errorf("lock pending token theft revocation: %w", err)
	}
	if !completedAt.Valid {
		stateValue := tokenTheftLegacyStateValue
		if operationID.Valid {
			stateValue = operationID.String
		}
		if err := redisClient.Set(ctx, middleware.TokenTheftBlockedKey(userID), stateValue, 0).Err(); err != nil {
			return fmt.Errorf("set token theft block: %w", err)
		}
		if _, err := revokeAllRefreshTokensTx(ctx, tx, userID, RevokeAllRefreshTokensOptions{}); err != nil {
			return fmt.Errorf("revoke refresh tokens: %w", err)
		}
		var result sql.Result
		if operationID.Valid {
			result, err = tx.ExecContext(ctx,
				`UPDATE token_theft_pending_revocations SET completed_at = NOW(), updated_at = NOW() WHERE user_id = $1 AND operation_id = $2`, userID, operationID.String,
			)
		} else {
			result, err = tx.ExecContext(ctx,
				`UPDATE token_theft_pending_revocations SET completed_at = NOW(), updated_at = NOW() WHERE user_id = $1 AND operation_id IS NULL`, userID,
			)
		}
		if err != nil {
			return fmt.Errorf("complete token theft revocation: %w", err)
		}
		if updated, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("check completed token theft revocation: %w", err)
		} else if updated != 1 {
			return fmt.Errorf("complete token theft revocation: expected one row, updated %d", updated)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token theft revocation transaction: %w", err)
	}
	if err := finalizeTokenTheftRevocation(ctx, db, redisClient, userID, operationID); err != nil {
		return err
	}
	return nil
}

// finalizeTokenTheftRevocation clears post-commit Redis state before removing
// the completed row. Failures leave the completed row for cleanup-only retry.
func finalizeTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID sql.NullString) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token theft cleanup transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft cleanup transaction: %w", rollbackErr))
		}
	}()

	var completedAt sql.NullTime
	var lockErr error
	if operationID.Valid {
		lockErr = tx.QueryRowContext(ctx,
			`SELECT completed_at FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id = $2 FOR UPDATE`, userID, operationID.String,
		).Scan(&completedAt)
	} else {
		lockErr = tx.QueryRowContext(ctx,
			`SELECT completed_at FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id IS NULL FOR UPDATE`, userID,
		).Scan(&completedAt)
	}
	if lockErr != nil {
		if errors.Is(lockErr, sql.ErrNoRows) {
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("commit token theft cleanup no-op transaction: %w", commitErr)
			}
			return nil
		}
		return fmt.Errorf("lock completed token theft revocation: %w", lockErr)
	}
	if !completedAt.Valid {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit token theft cleanup no-op transaction: %w", err)
		}
		return nil
	}
	var cleanupErr error
	if operationID.Valid {
		_, cleanupErr = finalizeTokenTheftRecoveryStateScript.Run(ctx, redisClient,
			[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID)}, userID, operationID.String,
		).Result()
	} else {
		_, cleanupErr = finalizeLegacyTokenTheftRecoveryStateScript.Run(ctx, redisClient,
			[]string{tokenTheftLegacyFallbackSetKey, middleware.TokenTheftBlockedKey(userID)}, userID, tokenTheftLegacyStateValue,
		).Result()
	}
	if cleanupErr != nil {
		return fmt.Errorf("clear token theft recovery state: %w", cleanupErr)
	}
	var deleteResult sql.Result
	if operationID.Valid {
		deleteResult, err = tx.ExecContext(ctx,
			`DELETE FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id = $2 AND completed_at IS NOT NULL`, userID, operationID.String,
		)
	} else {
		deleteResult, err = tx.ExecContext(ctx,
			`DELETE FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id IS NULL AND completed_at IS NOT NULL`, userID,
		)
	}
	if err != nil {
		return fmt.Errorf("retire completed token theft revocation: %w", err)
	}
	if deleted, err := deleteResult.RowsAffected(); err != nil {
		return fmt.Errorf("check retired token theft revocation: %w", err)
	} else if deleted != 1 {
		return fmt.Errorf("retire completed token theft revocation: expected one row, deleted %d", deleted)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token theft cleanup transaction: %w", err)
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
		fallbackOperations, fallbackErr := redisClient.HGetAll(ctx, tokenTheftFallbackHashKey).Result()
		if fallbackErr != nil {
			errs = append(errs, fmt.Errorf("list token theft revocation fallbacks: %w", fallbackErr))
		} else {
			seenUserIDs := make(map[string]struct{}, len(userIDs))
			for _, userID := range userIDs {
				seenUserIDs[userID] = struct{}{}
			}
			for userID, operationID := range fallbackOperations {
				if _, seen := seenUserIDs[userID]; seen {
					continue
				}
				attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
				err := enqueueTokenTheftRevocation(attemptCtx, db, userID, &operationID)
				cancel()
				if err != nil {
					retired, retireErr := retireTokenTheftRecoveryStateIfUserDeleted(ctx, db, redisClient, userID, &operationID)
					if retireErr != nil {
						errs = append(errs, retireErr)
						log.Error("Token theft fallback retirement failed", "error", retireErr, "user_id", userID)
						continue
					}
					if retired {
						continue
					}
					errs = append(errs, fmt.Errorf("promote token theft fallback: %w", err))
					log.Error("Token theft fallback promotion failed", "error", err, "user_id", userID)
					continue
				}
				seenUserIDs[userID] = struct{}{}
				userIDs = append(userIDs, userID)
			}
		}
		legacyFallbackUserIDs, legacyFallbackErr := redisClient.SMembers(ctx, tokenTheftLegacyFallbackSetKey).Result()
		if legacyFallbackErr != nil {
			errs = append(errs, fmt.Errorf("list legacy token theft revocation fallbacks: %w", legacyFallbackErr))
		} else {
			seenUserIDs := make(map[string]struct{}, len(userIDs))
			for _, userID := range userIDs {
				seenUserIDs[userID] = struct{}{}
			}
			for _, userID := range legacyFallbackUserIDs {
				if _, seen := seenUserIDs[userID]; seen {
					if err := redisClient.SRem(ctx, tokenTheftLegacyFallbackSetKey, userID).Err(); err != nil {
						errs = append(errs, fmt.Errorf("retire superseded legacy token theft fallback: %w", err))
						log.Error("Legacy token theft fallback retirement failed", "error", err, "user_id", userID)
					}
					continue
				}
				attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
				err := enqueueTokenTheftRevocation(attemptCtx, db, userID, nil)
				cancel()
				if err != nil {
					retired, retireErr := retireTokenTheftRecoveryStateIfUserDeleted(ctx, db, redisClient, userID, nil)
					if retireErr != nil {
						errs = append(errs, retireErr)
						log.Error("Legacy token theft fallback retirement failed", "error", retireErr, "user_id", userID)
						continue
					}
					if retired {
						continue
					}
					errs = append(errs, fmt.Errorf("promote legacy token theft fallback: %w", err))
					log.Error("Legacy token theft fallback promotion failed", "error", err, "user_id", userID)
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
			errs = append(errs, fmt.Errorf("recover token theft revocation for %s: %w", userID, err))
			log.Error("Pending token theft revocation recovery failed", "error", err, "user_id", userID)
		}
	}
	return errors.Join(errs...)
}

// retireTokenTheftRecoveryStateIfUserDeleted clears Redis-only recovery state
// after an account erase cascades its durable row. The comparison-and-delete
// scripts preserve state belonging to a newer theft event for the same user.
func retireTokenTheftRecoveryStateIfUserDeleted(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID *string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check token theft fallback user: %w", err)
	}
	if exists {
		return false, nil
	}
	if operationID != nil {
		if _, err := finalizeTokenTheftRecoveryStateScript.Run(ctx, redisClient,
			[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID)}, userID, *operationID,
		).Result(); err != nil {
			return false, fmt.Errorf("clear deleted-user token theft recovery state: %w", err)
		}
		return true, nil
	}
	if _, err := finalizeLegacyTokenTheftRecoveryStateScript.Run(ctx, redisClient,
		[]string{tokenTheftLegacyFallbackSetKey, middleware.TokenTheftBlockedKey(userID)}, userID, tokenTheftLegacyStateValue,
	).Result(); err != nil {
		return false, fmt.Errorf("clear deleted-user legacy token theft recovery state: %w", err)
	}
	return true, nil
}

// StartTokenTheftRecoveryWorker runs startup recovery before returning, then
// replays pending theft sweeps on a fixed interval until ctx is cancelled.
func StartTokenTheftRecoveryWorker(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, interval time.Duration) error {
	if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log); err != nil {
		return fmt.Errorf("startup token theft revocation recovery incomplete: %w", err)
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
