package rbac_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	chanText1  = "text-1"
	chanVoice1 = "voice-1"
)

// --- GetVisibleChannelIDs Tests ---

func TestGetVisibleChannelIDsOwnerSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner1")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 1")
	ch1 := ts.CreateTestChannel(t, serverID, chanText1, false)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, owner.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
	assert.Len(t, ids, 2)
}

func TestGetVisibleChannelIDsNonMemberSeesNone(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner2")
	outsider := ts.CreateTestUser(t, "visoutsider2")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 2")
	ts.CreateTestChannel(t, serverID, chanText1, false)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, outsider.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestGetVisibleChannelIDsBaseMemberSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner3")
	member := ts.CreateTestUser(t, "vismember3")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 3")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch1 := ts.CreateTestChannel(t, serverID, chanText1, false)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
}

func TestGetVisibleChannelIDsDenyHidesChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner4")
	member := ts.CreateTestUser(t, "vismember4")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 4")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chVisible := ts.CreateTestChannel(t, serverID, "visible", false)
	chHidden := ts.CreateTestChannel(t, serverID, "hidden", false)

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny PermViewTextChannels for hidden channel
	ts.CreateChannelOverride(t, chHidden, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chVisible, "visible channel should be visible")
	assert.NotContains(t, ids, chHidden, "hidden channel should not be visible")
}

func TestGetVisibleChannelIDsDenyVoiceHidesVoiceChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner5")
	member := ts.CreateTestUser(t, "vismember5")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 5")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText := ts.CreateTestChannel(t, serverID, "text-chan", false)
	chVoice := ts.CreateVoiceChannel(t, serverID, "voice-chan")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny PermViewVoiceChannels for voice channel
	ts.CreateChannelOverride(t, chVoice, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chText, "text channel should still be visible")
	assert.NotContains(t, ids, chVoice, "voice channel with deny should be hidden")
}

func TestGetVisibleChannelIDsAdminSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner6")
	admin := ts.CreateTestUser(t, "visadmin6")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 6")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")

	// Create admin role with Administrator bit
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	ch1 := ts.CreateTestChannel(t, serverID, chanText1, false)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, admin.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
	assert.Len(t, ids, 2)
}

func TestGetVisibleChannelIDsEmptyServer(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner7")
	serverID := ts.CreateTestServer(t, owner.ID, "Empty Vis Server")
	// No channels

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, owner.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestGetVisibleChannelIDsUserAllowOverridesRoleDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner8")
	member := ts.CreateTestUser(t, "vismember8")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 8")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "restricted", false)

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny view for @all role
	ts.CreateChannelOverride(t, ch, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	// But allow for this specific user
	ts.CreateChannelOverride(t, ch, "user", member.ID, int64(rbac.PermViewTextChannels), 0)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "user-allow should override role-deny for visibility")
}

// --- GetEffectivePermissions Additional Tests ---

func TestGetEffectivePermissionsCacheHit(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "effowner1")
	member := ts.CreateTestUser(t, "effmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "EffPerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// First call: computes and caches
	perms1, err := resolver.GetEffectivePermissions(ctx, serverID, member.ID, "")
	require.NoError(t, err)
	assert.True(t, perms1.Has(rbac.PermViewTextChannels))

	// Second call: should hit cache and return same result
	perms2, err := resolver.GetEffectivePermissions(ctx, serverID, member.ID, "")
	require.NoError(t, err)
	assert.Equal(t, perms1, perms2)
}

func TestGetEffectivePermissionsNonMemberReturnsError(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "effowner2")
	outsider := ts.CreateTestUser(t, "effoutsider2")
	serverID := ts.CreateTestServer(t, owner.ID, "EffPerm Server 2")

	perms, err := resolver.GetEffectivePermissions(ctx, serverID, outsider.ID, "")
	assert.Error(t, err)
	assert.Equal(t, rbac.Permission(0), perms)
}

func TestHasPermissionCacheHit(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "hpowner1")
	member := ts.CreateTestUser(t, "hpmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "HP Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// First call populates cache
	hasPerm, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermViewTextChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm)

	// Verify cache was populated
	cache := rbac.NewPermissionCache(ts.Redis)
	cached, ok := cache.Get(ctx, serverID, member.ID, "")
	assert.True(t, ok, "cache should be populated after HasPermission call")
	assert.True(t, cached.Has(rbac.PermViewTextChannels))
}
