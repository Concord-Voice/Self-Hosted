package voice

// A refused revoke must not run the phase-1 presence capture (enumerating every
// active sender in the named channel) or open the audience-revocation fence on a
// channel the actor cannot see or that belongs to another server: the cost of the
// refusal would otherwise track occupancy of a channel the actor cannot see.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type revokeCaptureSpy struct {
	mu       sync.Mutex
	prepared []string
}

func (s *revokeCaptureSpy) PrepareCapture(_ context.Context, _ string, channelIDs []string, _ *string) (rbac.PresenceRecheckPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepared = append(s.prepared, channelIDs...)
	return nil, nil
}
func (s *revokeCaptureSpy) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}
func (s *revokeCaptureSpy) Execute(rbac.PresenceRecheckPlan)         {}
func (s *revokeCaptureSpy) Abandon(rbac.PresenceRecheckPlan, string) {}

func doRevokeTempAccess(t *testing.T, h *Handler, actorID, serverID, targetID, channelID string) (int, string) {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", actorID)
		c.Request = c.Request.WithContext(dbtest.ActorContext(c.Request.Context(), t, h.db, actorID))
		c.Next()
	})
	router.DELETE("/servers/:id/voice/:userId/temp-access", h.RevokeTempAccess)
	body, err := json.Marshal(map[string]string{"channel_id": channelID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodDelete, "/servers/"+serverID+"/voice/"+targetID+"/temp-access", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestRevokeTempAccess_RefusalRunsNoPresenceCapture(t *testing.T) {
	db, redisClient, ownerID, moverID, serverID, _, _ := commitFailureVoiceEffectFixture(t)

	// Mover: Move Members at server scope only.
	moverRole := uuid.New()
	_, err := db.Exec(`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, 'Organizer', 5, $3, FALSE, FALSE)`, moverRole, serverID, int64(rbac.PermMoveMembers))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, moverID, moverRole)
	require.NoError(t, err)
	var everyone uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&everyone))
	targetID := dbtest.CreateUser(t, db)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, targetID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, targetID, everyone)
	require.NoError(t, err)

	// Hidden staff channel (VIEW|JOIN denied to @all) with someone talking in it.
	hidden := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'staff', 'voice')`, hidden, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, 0, $4)`, uuid.New(), hidden, everyone, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)
	insertCommitFailureVoiceParticipant(t, db, hidden, ownerID)

	// A voice channel of a server the mover is not a member of, also occupied.
	foreignOwner, foreignMember := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	foreignServer, foreign := uuid.New(), uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, foreignOwner, foreignMember, foreignServer, foreign, uuid.New()))
	insertCommitFailureVoiceParticipant(t, db, foreign, foreignOwner)

	// Positive control: a visible voice channel the mover may moderate.
	visible := uuid.New()
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'lobby2', 'voice')`, visible, serverID)
	require.NoError(t, err)

	var effects atomic.Int32
	h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
	h.commitVoiceEffectTxForTest = nil
	h.beforeVoiceEffectForTest = nil
	spy := &revokeCaptureSpy{}
	h.SetPresenceRecheck(spy)

	for name, ch := range map[string]uuid.UUID{"hidden": hidden, "foreign": foreign} {
		code, body := doRevokeTempAccess(t, h, moverID.String(), serverID.String(), targetID.String(), ch.String())
		t.Logf("revoke %s channel -> %d %s", name, code, body)
		require.Equal(t, http.StatusForbidden, code, name)
		require.JSONEq(t, `{"error":"Insufficient permissions"}`, body, name)
	}
	code, body := doRevokeTempAccess(t, h, moverID.String(), serverID.String(), targetID.String(), visible.String())
	t.Logf("revoke visible channel (control) -> %d %s", code, body)
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"revoked":false}`, body)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	t.Logf("phase-1 captures run: %v (hidden=%s foreign=%s visible=%s)", spy.prepared, hidden, foreign, visible)
	require.Equal(t, []string{visible.String()}, spy.prepared,
		"a refused revoke must not run the phase-1 presence capture on a channel the actor cannot see or that belongs to another server")
}
