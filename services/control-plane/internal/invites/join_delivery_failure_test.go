package invites_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/invites"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
)

// deliveryFailingCapture commits the join and then reports that presence
// delivery failed, which is the durable-join-with-unsettled-fan-out outcome.
type deliveryFailingCapture struct{ db *sql.DB }

func (c deliveryFailingCapture) WithGatedTx(ctx context.Context, _ presencecapture.Subject, work func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			panic(rbErr)
		}
	}()
	return work(tx)
}

func (deliveryFailingCapture) CaptureInTx(context.Context, *sql.Tx, presencecapture.Subject) (presencecapture.Plan, error) {
	return nil, nil
}

func (deliveryFailingCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	return fmt.Errorf("deliver: %w", presencecapture.ErrPostCommitDelivery)
}

func (deliveryFailingCapture) Abandon(presencecapture.Plan, presencecapture.Cause) {}

// A join whose presence delivery fails after commit is still a join: 200 with
// presence "pending", the server populated, and the joiner's pending key
// requests written. Returning early here would leave the joiner with no path
// to any channel key.
func TestJoinServer_PostCommitDeliveryFailureStillCompletesJoin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "join_delivery_owner")
	joiner := ts.CreateTestUser(t, "join_delivery_joiner")
	serverID := ts.CreateTestServer(t, owner.ID, "Join Delivery")
	ts.CreateTestChannel(t, serverID, "general-delivery")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	log := logger.New("test")
	h := invites.NewHandler(ts.DB, log, ts.Hub, rbac.NewResolver(ts.DB, nil, log))
	h.SetGraphPresenceCapture(deliveryFailingCapture{db: ts.DB})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/join", func(c *gin.Context) {
		c.Set("user_id", joiner.ID)
		h.JoinServer(c)
	})

	body, err := json.Marshal(map[string]string{"code": code})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/join", bytes.NewReader(body)))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, "pending", testhelpers.JSONField[string](t, resp, "presence"))
	server := testhelpers.JSONField[map[string]interface{}](t, resp, "server")
	assert.Equal(t, serverID, testhelpers.JSONField[string](t, server, "id"))

	var members, keyRequests int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM server_members WHERE server_id=$1 AND user_id=$2`, serverID, joiner.ID).Scan(&members))
	assert.Equal(t, 1, members)
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM pending_key_requests p JOIN channels c ON c.id = p.channel_id WHERE c.server_id=$1 AND p.user_id=$2`, serverID, joiner.ID).Scan(&keyRequests))
	assert.Positive(t, keyRequests, "the joiner's pending key requests are written despite the delivery failure")
}
