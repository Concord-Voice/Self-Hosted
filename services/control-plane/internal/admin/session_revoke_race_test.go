package admin_test

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// revokeBeforeSlideHook runs a logout on a second connection at the exact
// moment the sliding Get is about to rewrite the session key, which is the
// widest window between Get's read and its write.
type revokeBeforeSlideHook struct {
	sid    string
	revoke func(context.Context) error
	fired  atomic.Bool
}

func (h *revokeBeforeSlideHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *revokeBeforeSlideHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) > 1 && strings.EqualFold(cmd.Name(), "set") && strings.Contains(fmt.Sprint(args[1]), h.sid) && h.fired.CompareAndSwap(false, true) {
			if err := h.revoke(ctx); err != nil {
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (h *revokeBeforeSlideHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestSessionStore_ConcurrentLogoutWinsOverSlidingGet is the VULN-136
// regression: a logout that lands between the sliding Get's read and its
// rewrite must leave the session revoked. Before the fix the slide recreated
// the deleted key, so the logged-out cookie stayed valid.
func TestSessionStore_ConcurrentLogoutWinsOverSlidingGet(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	plain := admin.NewSessionStore(rdb, fixedClock(&now))
	sid, err := plain.Mint(ctx, uniqueAdminUsername("race-admin"), 1)
	require.NoError(t, err)

	hooked := redis.NewClient(rdb.Options())
	t.Cleanup(func() { _ = hooked.Close() })
	hook := &revokeBeforeSlideHook{sid: sid, revoke: func(ctx context.Context) error { return plain.Revoke(ctx, sid) }}
	hooked.AddHook(hook)
	racing := admin.NewSessionStore(hooked, fixedClock(&now))

	now = now.Add(time.Minute)
	_, err = racing.Get(ctx, sid)
	require.True(t, hook.fired.Load(), "the logout must land inside the sliding Get")
	assert.ErrorIs(t, err, admin.ErrSessionInvalid, "a Get that loses to a logout must not authenticate")

	_, err = plain.Get(ctx, sid)
	assert.ErrorIs(t, err, admin.ErrSessionInvalid, "a logged-out session must stay revoked")
	n, err := rdb.Exists(ctx, "admin_session:"+sid).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the sliding Get must not recreate a revoked session key")
}

// TestSessionStore_MethodSetHasNoUnfencedWrites pins the store's exported
// surface. Every method that writes a session key must keep a concurrent
// Revoke final: Mint writes only a fresh random id, Get slides with SET XX, and
// Revoke deletes. A read-then-copy primitive such as the removed Rotate (Get,
// then a plain SET of a new id, then Revoke) lets a logout that lands between
// its steps leave a live session, so adding one must come with its own race
// test and an update here.
func TestSessionStore_MethodSetHasNoUnfencedWrites(t *testing.T) {
	typ := reflect.TypeOf(&admin.SessionStore{})
	got := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	assert.Equal(t, []string{"Get", "Mint", "Revoke"}, got)
}
