package ownership

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJoinOwnershipRollbackError pins the deferred-rollback error folding of
// withOwnershipCaptureGuardedOutcome: ErrTxDone and nil are inert, any other
// rollback failure is wrapped with the fixed prefix and either becomes the
// return error or is joined after the existing one.
func TestJoinOwnershipRollbackError(t *testing.T) {
	writeErr := errors.New("write failed")
	rbErr := errors.New("connection reset")

	t.Run("tx done keeps nil", func(t *testing.T) {
		assert.NoError(t, joinOwnershipRollbackError(nil, sql.ErrTxDone))
	})
	t.Run("wrapped tx done keeps existing error", func(t *testing.T) {
		got := joinOwnershipRollbackError(writeErr, fmt.Errorf("driver: %w", sql.ErrTxDone))
		assert.Same(t, writeErr, got)
	})
	t.Run("nil rollback keeps nil", func(t *testing.T) {
		assert.NoError(t, joinOwnershipRollbackError(nil, nil))
	})
	t.Run("nil rollback keeps existing error", func(t *testing.T) {
		assert.Same(t, writeErr, joinOwnershipRollbackError(writeErr, nil))
	})
	t.Run("rollback failure becomes return error", func(t *testing.T) {
		got := joinOwnershipRollbackError(nil, rbErr)
		require.Error(t, got)
		assert.Equal(t, "rollback ownership transaction: connection reset", got.Error())
		assert.ErrorIs(t, got, rbErr)
	})
	t.Run("rollback failure joins existing error", func(t *testing.T) {
		got := joinOwnershipRollbackError(writeErr, rbErr)
		require.Error(t, got)
		assert.Equal(t, "write failed\nrollback ownership transaction: connection reset", got.Error())
		assert.ErrorIs(t, got, writeErr)
		assert.ErrorIs(t, got, rbErr)
	})
}

// legacyRollbackFold is the deferred closure body withOwnershipCaptureGuardedOutcome
// carried before the helper was extracted, kept verbatim so the equivalence
// below proves the extraction changed nothing.
func legacyRollbackFold(retErr, rollbackErr error) error {
	if errors.Is(rollbackErr, sql.ErrTxDone) {
		return retErr
	}
	if rollbackErr != nil {
		rollbackErr = fmt.Errorf("rollback ownership transaction: %w", rollbackErr)
		if retErr == nil {
			retErr = rollbackErr
		} else {
			retErr = errors.Join(retErr, rollbackErr)
		}
	}
	return retErr
}

func TestJoinOwnershipRollbackError_MatchesLegacyClosure(t *testing.T) {
	rets := []error{nil, errors.New("write failed"), errOwnershipAmbiguousCommit}
	rollbacks := []error{nil, sql.ErrTxDone, fmt.Errorf("w: %w", sql.ErrTxDone), errors.New("connection reset")}
	for _, r := range rets {
		for _, rb := range rollbacks {
			want := legacyRollbackFold(r, rb)
			got := joinOwnershipRollbackError(r, rb)
			if want == nil {
				assert.NoError(t, got)
				continue
			}
			require.Error(t, got)
			assert.Equal(t, want.Error(), got.Error(), "ret=%v rollback=%v", r, rb)
		}
	}
}
