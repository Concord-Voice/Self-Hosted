package friends

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removeFriendProbeFindsNoEdge may only short-circuit RemoveFriend when it
// proved there is no accepted edge. Every way it can fail to prove that (an
// unreadable pool, an unparseable id) must return false so the request falls
// through to the gated path.
func TestRemoveFriendProbeFindsNoEdge_FailsOpen(t *testing.T) {
	closed, err := sql.Open("postgres", "postgres://127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	h := &Handler{db: closed}
	actor, target := uuid.NewString(), uuid.NewString()

	cases := map[string][2]string{
		"closed pool":     {actor, target},
		"non-uuid actor":  {"not-a-uuid", target},
		"non-uuid target": {actor, "not-a-uuid"},
	}
	for name, ids := range cases {
		t.Run(name, func(t *testing.T) {
			assert.False(t, h.removeFriendProbeFindsNoEdge(context.Background(), ids[0], ids[1]))
		})
	}
}
