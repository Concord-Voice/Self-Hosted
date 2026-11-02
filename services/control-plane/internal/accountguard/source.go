// Package accountguard provides shared account-change transaction helpers.
package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// Source is the decision policy's auth-change source, without conversion.
type Source = decide.Source

// The six policy sources. SourceTx accepts only the four attributable sources
// supported by account changes; operator and unattributed remain unwritable.
const (
	SourceUserSession  = decide.SourceUserSession
	SourceLockdown     = decide.SourceLockdown
	SourceBreakGlass   = decide.SourceBreakGlass
	SourceRecoveryKey  = decide.SourceRecoveryKey
	SourceOperator     = decide.SourceOperator
	SourceUnattributed = decide.SourceUnattributed
)

// SourceTx sets provenance for the caller's transaction. It neither authorizes
// a change nor owns transaction completion. The future runner must call it with
// the authoritative outcome's source before handing the transaction to a writer.
func SourceTx(ctx context.Context, tx *sql.Tx, source Source) error {
	switch source {
	case SourceUserSession, SourceLockdown, SourceBreakGlass, SourceRecoveryKey:
	default:
		return errors.New("accountguard: invalid auth-change source")
	}
	if tx == nil {
		return errors.New("accountguard: transaction required")
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('concord.auth_change_source', $1, true)`, string(source)); err != nil {
		return fmt.Errorf("accountguard: set auth-change source: %w", err)
	}
	return nil
}
