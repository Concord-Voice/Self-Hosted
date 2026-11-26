package voice

import (
	"database/sql"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type midGrantFixture struct {
	h                  *Handler
	db                 *sql.DB
	serverID, from, to uuid.UUID
	moverID, targetID  uuid.UUID
}

// newMidGrantFixture builds a move whose destination is hidden from the target
// (so a temporary grant is required) and which the mover may perform until the
// test mutates state in beforeTemporaryGrantForTest.
func newMidGrantFixture(t *testing.T) midGrantFixture {
	t.Helper()
	db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
	moverID := memberID
	// The owner has every permission and never needs a grant; use a base member.
	targetID := dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`, serverID, targetID)
	require.NoError(t, err)
	_ = ownerID
	insertCommitFailureVoiceParticipant(t, db, fromChannelID, targetID)
	var everyoneRoleID, moverRoleID uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&everyoneRoleID))
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, targetID, everyoneRoleID)
	require.NoError(t, err)

	moverRoleID = uuid.New()
	_, err = db.Exec(`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		VALUES ($1, $2, 'Organizer', 5, $3, FALSE, FALSE)`, moverRoleID, serverID, int64(rbac.PermMoveMembers))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, moverID, moverRoleID)
	require.NoError(t, err)
	// Hide the destination from @all (the target); give the mover VIEW|CONNECT|SPEAK.
	_, err = db.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, 0, $4)`, uuid.New(), toChannelID, everyoneRoleID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'user', $3, $4, 0)`, uuid.New(), toChannelID, moverID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak))
	require.NoError(t, err)

	var effects atomic.Int32
	return midGrantFixture{
		h: newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects), db: db,
		serverID: serverID, from: fromChannelID, to: toChannelID, moverID: moverID, targetID: targetID,
	}
}

func (f midGrantFixture) move(t *testing.T) (int, string) {
	t.Helper()
	// Use the plain (non commit-failing) path: the handler hook is what we exercise.
	f.h.commitVoiceEffectTxForTest = nil
	w := invokeCommitFailureVoiceEffect(t, f.h, f.moverID.String(), http.MethodPost,
		"/servers/"+f.serverID.String()+"/voice/"+f.targetID.String()+"/move",
		map[string]string{"target_channel_id": f.to.String()})
	return w.Code, w.Body.String()
}

func (f midGrantFixture) grantExists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary)`,
		f.to, f.targetID).Scan(&exists))
	return exists
}

func TestServerMove_MidGrantControl_Succeeds(t *testing.T) {
	f := newMidGrantFixture(t)
	code, body := f.move(t)
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, f.grantExists(t), "positive control: the unmodified move writes the temporary grant")
}

func TestServerMove_PermissionRevokedBeforeGrant_403NoGrant(t *testing.T) {
	f := newMidGrantFixture(t)
	f.h.beforeTemporaryGrantForTest = func() {
		_, err := f.db.Exec(`UPDATE channel_permission_overrides SET allow = 0, deny = $3
			WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
			f.to, f.moverID, int64(rbac.PermViewVoiceChannels))
		require.NoError(t, err)
	}
	code, body := f.move(t)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, `{"error":"Insufficient permissions"}`, body)
	assert.False(t, f.grantExists(t))
}

func TestServerMove_DestinationDeletedBeforeGrant(t *testing.T) {
	f := newMidGrantFixture(t)
	f.h.beforeTemporaryGrantForTest = func() {
		_, err := f.db.Exec(`DELETE FROM channels WHERE id = $1`, f.to)
		require.NoError(t, err)
	}
	code, body := f.move(t)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, `{"error":"Insufficient permissions"}`, body, "must be the permission body, not 500 or the member body")
	assert.False(t, f.grantExists(t))
}

// denySpeakForMover rewrites the mover's destination override to VIEW|JOIN
// allowed and SPEAK denied: enough to see and join, not enough to grant.
func (f midGrantFixture) denySpeakForMover(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE channel_permission_overrides SET allow = $3, deny = $4
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		f.to, f.moverID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), int64(rbac.PermSpeak))
	require.NoError(t, err)
}

// The grant transaction re-decides authority; losing only SPEAK between the
// decision transaction and the grant must refuse with no grant written.
func TestServerMove_SpeakRevokedBeforeGrant_403NoGrant(t *testing.T) {
	f := newMidGrantFixture(t)
	f.h.beforeTemporaryGrantForTest = func() { f.denySpeakForMover(t) }
	code, body := f.move(t)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, `{"error":"Insufficient permissions"}`, body)
	assert.False(t, f.grantExists(t))
}

// A mover that lacks SPEAK up front is refused in the decision transaction,
// before the grant transaction is ever reached.
func TestServerMove_LacksSpeak_RefusedBeforeGrantTx(t *testing.T) {
	f := newMidGrantFixture(t)
	f.denySpeakForMover(t)
	var reached atomic.Bool
	f.h.beforeTemporaryGrantForTest = func() { reached.Store(true) }
	code, body := f.move(t)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.JSONEq(t, `{"error":"Insufficient permissions"}`, body)
	assert.False(t, reached.Load(), "the refusal must happen before the grant transaction")
	assert.False(t, f.grantExists(t))
}
