package channels

import (
	"context"
	"database/sql"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// setPendingServerViews leaves a server's channels hidden when the permission
// resolve fails, and shows them all when no resolver is wired.
func TestSetPendingServerViews(t *testing.T) {
	targets := []channelKeyTarget{{channelID: "c1"}, {channelID: "c2"}}

	failingResolver := func(t *testing.T) *Handler {
		t.Helper()
		db, err := sql.Open("postgres", "postgres://invalid@127.0.0.1:1/none?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close() // every resolver query now fails
		rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
		t.Cleanup(func() { _ = rdb.Close() })
		log := logger.New("test")
		return &Handler{log: log, resolver: rbac.NewResolver(db, rbac.NewPermissionCache(rdb), log)}
	}
	const serverID, userID = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"

	// Hidden channels stay hidden: catches a resolve error that marks channels visible.
	t.Run("resolve error keeps channels hidden", func(t *testing.T) {
		views := map[string]bool{"c1": false, "c2": false}
		failingResolver(t).setPendingServerViews(context.Background(), views, serverID, userID, targets)
		if views["c1"] || views["c2"] {
			t.Fatalf("resolve error exposed channels: %v", views)
		}
	})

	// The error returns before any write: catches a missing error check, which
	// would overwrite views from an empty permission map.
	t.Run("resolve error writes nothing", func(t *testing.T) {
		views := map[string]bool{"c1": true, "c2": true}
		failingResolver(t).setPendingServerViews(context.Background(), views, serverID, userID, targets)
		if !views["c1"] || !views["c2"] {
			t.Fatalf("resolve error rewrote views: %v", views)
		}
	})

	t.Run("nil resolver shows every channel", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		views := map[string]bool{"c1": false, "c2": false}
		h.setPendingServerViews(context.Background(), views, "s", "u", targets)
		if !views["c1"] || !views["c2"] {
			t.Fatalf("nil resolver hid channels: %v", views)
		}
	})
}
