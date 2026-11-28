package messages

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The in-transaction edit authorization must require ManageOwn strictly.
// ManageAll covers ManageOwn only on the delete path, so an author whose
// ManageOwn was removed after the preflight cannot edit through ManageAll.
// The race itself is not reachable over HTTP (the preflight refuses first), so
// this drives the in-transaction path directly.
func TestEditInTransactionRequiresManageOwnStrictly(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	log := logger.New("test")
	owner := dbtest.CreateUser(t, db)
	author := dbtest.CreateUser(t, db)
	dbtest.RememberActor(t, db, author.String())
	serverID, channelID, messageID, roleID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()

	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'strict edit server', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, author)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'strict edit channel', 'text')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed) VALUES ($1, $2, 'manage all', 1, $3, FALSE, FALSE)`,
		roleID, serverID, int64(rbac.PermManageAllMessages|rbac.PermViewTextChannels))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, author, roleID)
	require.NoError(t, err)
	// ManageOwn is explicitly denied for the author on this channel.
	_, err = db.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'user', $3, 0, $4)`,
		uuid.NewString(), channelID, author, int64(rbac.PermManageOwnMessages))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at) VALUES ($1, $2, $3, 'original', 1, NOW(), NOW())`, messageID, channelID, author)
	require.NoError(t, err)

	h := NewHandler(db, log, nil, rbac.NewResolver(db, rbac.NewPermissionCache(redistest.Client(t)), log), nil)

	t.Run("edit transaction refuses ManageAll without ManageOwn", func(t *testing.T) {
		c, w := authorityFenceContext()
		dbtest.AttachRememberedActor(c, author.String())
		_, ok := h.updateMessageCiphertext(c, messageID, channelID, author.String(), UpdateMessageRequest{Content: "edited", KeyVersion: 1})
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		var content string
		var edited *string
		require.NoError(t, db.QueryRow(`SELECT content, edited_at::text FROM messages WHERE id = $1`, messageID).Scan(&content, &edited))
		assert.Equal(t, "original", content)
		assert.Nil(t, edited)
	})

	authorize := func(t *testing.T, manageAllCoversOwn bool) (bool, int) {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		dbtest.AttachRememberedActor(c, author.String())
		ok := h.authorizeMessageChannelTx(context.Background(), c, tx, messageChannelAuthorization{
			serverID: serverID, channelID: channelID, userID: author.String(),
			required: rbac.PermManageOwnMessages, genericMsg: "failed", manageAllCoversOwn: manageAllCoversOwn,
		}, "text")
		return ok, w.Code
	}

	t.Run("authorization refuses when manageAllCoversOwn is false", func(t *testing.T) {
		ok, code := authorize(t, false)
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, code)
	})

	t.Run("authorization allows when manageAllCoversOwn is true (delete path)", func(t *testing.T) {
		ok, _ := authorize(t, true)
		assert.True(t, ok)
	})
}
