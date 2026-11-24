package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

func TestEnrollmentToken_MintThenConsume(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	store := admin.NewEnrollmentStore(rdb)

	adminID := uniqueAdminUsername("admin-id")
	token, err := store.MintEnrollmentToken(ctx, adminID, 7)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	got, generation, err := store.ConsumeEnrollmentToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, adminID, got)
	assert.Equal(t, int64(7), generation)
}

func TestEnrollmentToken_StoresOnlyHashNotPlaintext(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	store := admin.NewEnrollmentStore(rdb)

	token, err := store.MintEnrollmentToken(ctx, uniqueAdminUsername("admin-id"), 1)
	require.NoError(t, err)

	// The plaintext token must never be a Redis key: looking up the plaintext
	// directly under the namespace yields nothing; only the hashed key exists.
	keys, err := rdb.Keys(ctx, "admin_enroll:*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.NotContains(t, keys[0], token, "plaintext token must not appear in the Redis key")
}

func TestEnrollmentToken_SingleUse(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	store := admin.NewEnrollmentStore(rdb)

	token, err := store.MintEnrollmentToken(ctx, uniqueAdminUsername("admin-id"), 1)
	require.NoError(t, err)

	_, _, err = store.ConsumeEnrollmentToken(ctx, token)
	require.NoError(t, err)

	// Second consume of the same token fails (GETDEL removed it).
	_, _, err = store.ConsumeEnrollmentToken(ctx, token)
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrEnrollTokenInvalid)
}

func TestEnrollmentToken_RejectsUnknownToken(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	store := admin.NewEnrollmentStore(rdb)

	_, _, err := store.ConsumeEnrollmentToken(ctx, "deadbeefdeadbeefdeadbeefdeadbeef")
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrEnrollTokenInvalid)

	_, _, err = store.ConsumeEnrollmentToken(ctx, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, admin.ErrEnrollTokenInvalid)
}

func TestEnrollmentToken_MintRequiresAdminID(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	store := admin.NewEnrollmentStore(rdb)

	_, err := store.MintEnrollmentToken(ctx, "", 1)
	require.Error(t, err)
}
