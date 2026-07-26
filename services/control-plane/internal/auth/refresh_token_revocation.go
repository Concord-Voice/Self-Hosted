package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RevokeAllRefreshTokens revokes a user's active refresh tokens, optionally
// excluding a token hash. It serializes with session mints on the users row.
func RevokeAllRefreshTokens(ctx context.Context, db *sql.DB, userID, excludeTokenHash string) (result sql.Result, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin refresh-token revocation transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback refresh-token revocation transaction: %w", rollbackErr))
		}
	}()

	var lockedUserID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&lockedUserID); err != nil {
		return nil, fmt.Errorf("lock refresh-token revocation user: %w", err)
	}

	if excludeTokenHash != "" {
		result, err = tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND token_hash != $2 AND revoked_at IS NULL`,
			userID, excludeTokenHash)
	} else {
		result, err = tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("revoke refresh tokens: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refresh-token revocation transaction: %w", err)
	}
	return result, nil
}
