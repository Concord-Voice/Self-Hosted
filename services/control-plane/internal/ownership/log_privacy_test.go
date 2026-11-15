package ownership

import (
	"bytes"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSendTransferEmailFailureOmitsRecipient(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	ownerID := dbtest.CreateUser(t, db)
	targetID := dbtest.CreateUser(t, db)
	serverID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'Transfer Log Privacy', $2)`,
		serverID,
		ownerID,
	)
	require.NoError(t, err)
	var ownerEmail string
	require.NoError(t, db.QueryRow(`SELECT email FROM users WHERE id = $1`, ownerID).Scan(&ownerEmail))

	var logs bytes.Buffer
	log := logger.NewWithWriter(&logs)
	h := &Handler{
		db:       db,
		log:      log,
		emailSvc: email.NewService(&config.Config{Environment: "production"}, log),
	}

	h.sendTransferEmail(t.Context(), ownerID.String(), serverID.String(), targetID.String(), "test-reversal-token")

	require.Contains(t, logs.String(), "Failed to send ownership transfer email")
	require.NotContains(t, logs.String(), ownerEmail)
	require.Contains(t, logs.String(), "failure_class=ownership_transfer_email_delivery")
}
