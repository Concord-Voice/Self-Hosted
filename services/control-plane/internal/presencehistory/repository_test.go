package presencehistory

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryPaginationAndSafeDecoding(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	repository := NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	user := ts.CreateTestUser(t, "history_pagination")
	userID := uuid.MustParse(user.ID)
	other := ts.CreateTestUser(t, "history_pagination_other")
	otherID := uuid.MustParse(other.ID)
	recordedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	expiresAt := recordedAt.Add(24 * time.Hour)
	endedAt := recordedAt.Add(time.Second)

	ids := []uuid.UUID{
		uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	}
	for index, id := range ids {
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO presence_history (
				id, sender_id, category, payload_version, payload,
				started_at, ended_at, recorded_at, expires_at
			) VALUES ($1, $2, 'custom_text', 1, $3::JSONB, $4, $5, $4, $6)
		`, id, userID, `{"text":"item `+string(rune('1'+index))+`"}`,
			recordedAt, endedAt, expiresAt)
		require.NoError(t, err)
	}
	insertHistoryRow(t, ts.DB, otherID, CategoryCustomText, 1,
		`{"text":"not yours"}`, recordedAt, endedAt, recordedAt, expiresAt)

	first, err := repository.List(ctx, userID, ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Items, 2)
	assert.Equal(t, ids[2], first.Items[0].ID)
	assert.Equal(t, ids[1], first.Items[1].ID)
	require.NotNil(t, first.NextCursor)
	decodedCursor, err := DecodeCursor(*first.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, first.Items[1].ID, decodedCursor.ID)
	assert.Equal(t, first.Items[1].RecordedAt, decodedCursor.RecordedAt)

	second, err := repository.List(ctx, userID, ListOptions{Limit: 2, Before: &decodedCursor})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, ids[0], second.Items[0].ID)
	assert.Nil(t, second.NextCursor)
	for _, item := range append(first.Items, second.Items...) {
		assert.Equal(t, ItemStatusSupported, item.Status)
		require.NotNil(t, item.Payload)
		assert.Equal(t, CategoryCustomText, item.Category)
	}
}

func TestRepositoryFullTerminalPageHasNoCursor(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repository := NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	user := ts.CreateTestUser(t, "history_terminal")
	userID := uuid.MustParse(user.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for _, text := range []string{"one", "two"} {
		insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
			`{"text":"`+text+`"}`, now, now, now, now.Add(time.Hour))
	}

	page, err := repository.List(context.Background(), userID, ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Nil(t, page.NextCursor, "limit+1, not len==limit, determines more pages")
}

func TestRepositoryExpirySelfScopeAndUnsupportedPayloads(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	repository := NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	user := ts.CreateTestUser(t, "history_decode")
	userID := uuid.MustParse(user.ID)
	other := ts.CreateTestUser(t, "history_decode_other")
	otherID := uuid.MustParse(other.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	supportedID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"supported"}`, now, now, now, now.Add(time.Hour))
	malformedID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":""}`, now.Add(time.Microsecond), now.Add(time.Second),
		now.Add(time.Microsecond), now.Add(time.Hour))
	unknownVersionID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 2,
		`{"text":"future"}`, now.Add(2*time.Microsecond), now.Add(time.Second),
		now.Add(2*time.Microsecond), now.Add(time.Hour))
	unsupportedCategoryID := insertHistoryRow(t, ts.DB, userID, CategoryServerVoice, 1,
		`{"channel":"private"}`, now.Add(3*time.Microsecond), now.Add(time.Second),
		now.Add(3*time.Microsecond), now.Add(time.Hour))
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"expired"}`, now.Add(4*time.Microsecond), now.Add(time.Second),
		now.Add(4*time.Microsecond), time.Now().UTC().Add(-time.Second))
	insertHistoryRow(t, ts.DB, otherID, CategoryCustomText, 1,
		`{"text":"other user"}`, now.Add(5*time.Microsecond), now.Add(time.Second),
		now.Add(5*time.Microsecond), now.Add(time.Hour))

	page, err := repository.List(context.Background(), userID, ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 4)
	byID := make(map[uuid.UUID]HistoryItem, len(page.Items))
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	assert.Equal(t, ItemStatusSupported, byID[supportedID].Status)
	require.NotNil(t, byID[supportedID].Payload)
	for _, id := range []uuid.UUID{malformedID, unknownVersionID, unsupportedCategoryID} {
		assert.Equal(t, ItemStatusUnsupported, byID[id].Status)
		assert.Nil(t, byID[id].Payload, "raw malformed or unsupported JSON must not cross the API boundary")
	}
}

func TestRepositoryAccountDeletionCascadesHistory(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "history_repo_cascade")
	userID := uuid.MustParse(user.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"delete me"}`, now, nil, now, now.Add(time.Hour))
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	assert.Zero(t, historyRowCount(t, ts.DB, userID))
}

func TestRepositoryListRejectsInvalidLimits(t *testing.T) {
	repository := NewRepository(&sql.DB{}, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}))
	for _, limit := range []int{-1, 101} {
		_, err := repository.List(context.Background(), uuid.New(), ListOptions{Limit: limit})
		require.Error(t, err)
	}
}
