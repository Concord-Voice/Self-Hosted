package channels

import (
	"context"
	"database/sql"
	"io"
	"testing"

	_ "github.com/lib/pq" // registers the "postgres" driver for the closed-pool case
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// Characterisation tests for dmPendingConversationAvailable's two
// database-free exits. The participant, unavailable and success arms are
// covered through GET pending-requests by
// TestGetPendingKeyRequests_DMPendingRequiresCurrentUnblockedConversation and
// TestGetPendingKeyRequests_DMPendingAvailabilityErrorFailsRequest.

func TestDMPendingConversationAvailable_InvalidCallerIsNotParticipant(t *testing.T) {
	// No database: an unparseable caller must be refused before any
	// transaction is opened, so a nil pool proves BeginTx is never reached.
	h := &Handler{log: logger.NewWithWriter(io.Discard)}

	available, err := h.dmPendingConversationAvailable(context.Background(),
		"00000000-0000-0000-0000-000000000001", "not-a-uuid")

	require.ErrorIs(t, err, errDMKeyFetchNotParticipant)
	assert.False(t, available)
}

func TestDMPendingConversationAvailable_BeginTxErrorIsReturnedUnchanged(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	h := &Handler{db: db, log: logger.NewWithWriter(io.Discard)}

	available, err := h.dmPendingConversationAvailable(context.Background(),
		"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002")

	// The BeginTx error is returned as-is: not mapped to (false, nil) the way
	// dmblock.ErrUnavailable is, and not retried the way
	// dmblock.ErrMembershipChanged is.
	require.EqualError(t, err, "sql: database is closed")
	assert.False(t, available)
}
