package channels_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// VULN-080 regression: stale wrapped_key_versions entries for targets that
// already hold the key must not write pending key requests, on either branch.

// A group DM is capped at 10 participants, so its branch uses fewer targets.
const (
	floodTargets   = 50
	floodDMTargets = 8
)

// floodUsers creates n verified users whose identity key is at version 6, so a
// submitted version of 5 is stale.
func floodUsers(t *testing.T, ts *testhelpers.TestServer, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	_, err := ts.DB.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT u, u::text || '@test.local', 'u_' || left(u::text, 12), 'x', true, true
		FROM unnest($1::uuid[]) AS u`, pq.Array(ids))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO public_keys (user_id, public_key, key_version)
		SELECT u, 'test-public-key'::bytea, 6 FROM unnest($1::uuid[]) AS u`, pq.Array(ids))
	require.NoError(t, err)
	return ids
}

func staleVersions(ids []string) map[string]int {
	versions := make(map[string]int, len(ids))
	for _, id := range ids {
		versions[id] = 5
	}
	return versions
}

func TestStaleFlood_HeldTargetsWriteNoPendingRows(t *testing.T) {
	t.Run("channel", func(t *testing.T) {
		ts, owner, serverID, channelID := setupEncryptedChannel(t)
		targets := floodUsers(t, ts, floodTargets)
		_, err := ts.DB.Exec(`INSERT INTO server_members (server_id, user_id, role)
			SELECT $1, u, 'member' FROM unnest($2::uuid[]) AS u`, serverID, pq.Array(targets))
		require.NoError(t, err)
		_, err = ts.DB.Exec(`INSERT INTO member_roles (server_id, user_id, role_id)
			SELECT $1, u, r.id FROM unnest($2::uuid[]) AS u
			JOIN roles r ON r.server_id = $1 AND r.is_default = TRUE
			ON CONFLICT DO NOTHING`, serverID, pq.Array(targets))
		require.NoError(t, err)
		_, err = ts.DB.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			SELECT $1, u, $3, 1 FROM unnest($2::uuid[]) AS u`, channelID, pq.Array(targets), testhelpers.ValidCiphertext())
		require.NoError(t, err)

		w := distributePost(ts, owner.AccessToken, channelID, wrapAll(targets), staleVersions(targets))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		// The post-commit cleanup would hide a row written for a holder, so also
		// pin the outcome: holders are duplicates, never stale re-requests.
		duplicates, skippedStale := distributionOutcomeCounts(t, w)
		assert.Equal(t, float64(floodTargets), duplicates)
		assert.Equal(t, float64(0), skippedStale)
		assert.Equal(t, 0, rowCount(t, ts, `SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1`, channelID),
			"stale submissions for holders must write no pending rows")
	})

	t.Run("group DM", func(t *testing.T) {
		ts := setupTS(t)
		sender := ts.CreateTestUser(t, "floodsender")
		targets := floodUsers(t, ts, floodDMTargets)
		convID := ts.CreateGroupDMConversation(t, append([]string{sender.ID}, targets...)...)
		ts.SeedDMKey(t, convID, sender.ID, 1)
		for _, id := range targets {
			ts.SeedDMKey(t, convID, id, 1)
		}

		w := distributePost(ts, sender.AccessToken, convID, wrapAll(targets), staleVersions(targets))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		assert.Equal(t, 0, rowCount(t, ts, `SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1`, convID),
			"stale submissions for holders must write no pending rows")
	})
}
