package channels_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// VULN-080 / VULN-207: the pending-keys reader is paged with one keyset cursor
// over the channel and DM sources and examines a bounded number of rows per
// request. Visibility applies to every examined row, whatever cursor is sent.

var pendingSeedBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type pendingPage struct {
	ids  []string
	rows []map[string]interface{}
	next string // "" when next_cursor is null
}

func getPendingPage(t *testing.T, ts *testhelpers.TestServer, token, query string) pendingPage {
	t.Helper()
	w := ts.DoRequest("GET", pathE2EEPendingKeys+query, nil, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := testhelpers.JSONField[[]interface{}](t, body, "pending_requests")
	var page pendingPage
	for i := range requests {
		row := testhelpers.JSONElem[map[string]interface{}](t, requests, i)
		page.rows = append(page.rows, row)
		page.ids = append(page.ids, testhelpers.JSONField[string](t, row, "id"))
	}
	next, present := body["next_cursor"]
	require.True(t, present, "next_cursor is always present")
	if next != nil {
		page.next = testhelpers.JSONAs[string](t, next, "next_cursor")
		require.NotEmpty(t, page.next)
	}
	return page
}

func pendingQuery(limit int, cursor string) string {
	v := url.Values{}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// drainPending follows next_cursor to the end and returns every row ID in order.
func drainPending(t *testing.T, ts *testhelpers.TestServer, token string, limit int) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		page := getPendingPage(t, ts, token, pendingQuery(limit, cursor))
		ids = append(ids, page.ids...)
		if page.next == "" {
			return ids
		}
		cursor = page.next
	}
	t.Fatal("pending-keys pages never ended")
	return nil
}

// seedPendingRequesters creates n users, each with a pending key request on
// channelID; the i-th (1-based) is created at base + i*step. member=true also
// makes each a member of serverID with its default roles, so it can view the
// channel. It returns the request IDs in reader order.
func seedPendingRequesters(t *testing.T, ts *testhelpers.TestServer, serverID, channelID string, n int, base time.Time, step time.Duration, member bool) []string {
	t.Helper()
	users := make([]string, n)
	for i := range users {
		users[i] = uuid.NewString()
	}
	_, err := ts.DB.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT u, u::text || '@test.local', 'pk_' || replace(left(u::text, 13), '-', ''), 'x', true, true
		FROM unnest($1::uuid[]) AS u`, pq.Array(users))
	require.NoError(t, err)
	if member {
		_, err = ts.DB.Exec(`INSERT INTO server_members (server_id, user_id, role)
			SELECT $1, u, 'member' FROM unnest($2::uuid[]) AS u`, serverID, pq.Array(users))
		require.NoError(t, err)
		_, err = ts.DB.Exec(`INSERT INTO member_roles (server_id, user_id, role_id)
			SELECT $1, u, r.id FROM unnest($2::uuid[]) AS u
			CROSS JOIN roles r WHERE r.server_id = $1 AND r.is_default = TRUE`, serverID, pq.Array(users))
		require.NoError(t, err)
	}
	_, err = ts.DB.Exec(`INSERT INTO pending_key_requests (channel_id, user_id, created_at)
		SELECT $1, s.u, $3::timestamptz + (s.o * $4::bigint) * interval '1 microsecond'
		FROM unnest($2::uuid[]) WITH ORDINALITY AS s(u, o)`,
		channelID, pq.Array(users), base, step.Microseconds())
	require.NoError(t, err)
	rows, err := ts.DB.Query(`SELECT p.id FROM pending_key_requests p
		JOIN unnest($2::uuid[]) AS u ON p.user_id = u
		WHERE p.channel_id = $1 ORDER BY p.created_at, p.id`, channelID, pq.Array(users))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	ids := make([]string, 0, n)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func seedPendingDMRequestAt(t *testing.T, ts *testhelpers.TestServer, conversationID, userID string, at time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO dm_pending_key_requests (conversation_id, user_id, created_at)
		VALUES ($1, $2, $3) RETURNING id`, conversationID, userID, at).Scan(&id))
	return id
}

// createKeyedChannel creates a channel as owner and distributes
// its key to each of holders.
func createKeyedChannel(t *testing.T, ts *testhelpers.TestServer, owner testhelpers.TestUser, serverID, channelType string, holders ...string) string {
	t.Helper()
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID:    serverID,
		"name":         "pk-" + uuid.NewString()[:8],
		"type":         channelType,
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channelID := testhelpers.JSONField[string](t, testhelpers.JSONField[map[string]interface{}](t, body, keyChannel), "id")
	for _, holder := range holders {
		dist := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{holder: testhelpers.ValidCiphertext()},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusOK, dist.Code, dist.Body.String())
	}
	return channelID
}

func rawCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestPendingKeys_PageSizeAndCursor(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	want := seedPendingRequesters(t, ts, serverID, channelID, 250, pendingSeedBase, time.Millisecond, true)

	p1 := getPendingPage(t, ts, owner.AccessToken, pendingQuery(100, ""))
	require.Len(t, p1.ids, 100)
	require.NotEmpty(t, p1.next, "page 1 has a next cursor")
	p2 := getPendingPage(t, ts, owner.AccessToken, pendingQuery(100, p1.next))
	require.Len(t, p2.ids, 100)
	require.NotEmpty(t, p2.next, "page 2 has a next cursor")
	p3 := getPendingPage(t, ts, owner.AccessToken, pendingQuery(100, p2.next))
	require.Len(t, p3.ids, 50)
	assert.Empty(t, p3.next, "the last page has a null next_cursor")

	got := append(append(append([]string{}, p1.ids...), p2.ids...), p3.ids...)
	assert.Equal(t, want, got, "pages are contiguous, ordered, and duplicate-free")
}

func TestPendingKeys_MixedSourcesStableOrder(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	at := pendingSeedBase

	type keyed struct{ source, id string }
	var same []keyed
	before := seedPendingRequesters(t, ts, serverID, channelID, 1, at.Add(-2*time.Millisecond), time.Millisecond, true)
	for _, id := range seedPendingRequesters(t, ts, serverID, channelID, 3, at, 0, true) {
		same = append(same, keyed{"channel", id})
	}
	for i := 0; i < 3; i++ {
		peer := ts.CreateTestUser(t, "pkmixdm"+uuid.NewString()[:6])
		conv := ts.CreateDMConversation(t, owner.ID, peer.ID)
		ts.SeedDMKey(t, conv, owner.ID, 1)
		same = append(same, keyed{"dm", seedPendingDMRequestAt(t, ts, conv, peer.ID, at)})
	}
	latePeer := ts.CreateTestUser(t, "pkmixlate")
	lateConv := ts.CreateDMConversation(t, owner.ID, latePeer.ID)
	ts.SeedDMKey(t, lateConv, owner.ID, 1)
	after := seedPendingDMRequestAt(t, ts, lateConv, latePeer.ID, at.Add(time.Millisecond))

	// Equal created_at: channel before dm, then by id (uuid order is the
	// lowercase hex string order).
	sort.Slice(same, func(i, j int) bool {
		if same[i].source != same[j].source {
			return same[i].source < same[j].source
		}
		return same[i].id < same[j].id
	})
	want := append([]string{}, before...)
	for _, k := range same {
		want = append(want, k.id)
	}
	want = append(want, after)

	for _, limit := range []int{1, 2, 3} {
		assert.Equal(t, want, drainPending(t, ts, owner.AccessToken, limit), "limit=%d", limit)
	}
}

func TestPendingKeys_ScanCapAdvancesCursor(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	// Non-members cannot view the channel, so their 1,200 rows are invisible.
	seedPendingRequesters(t, ts, serverID, channelID, 1200, pendingSeedBase, time.Millisecond, false)
	visible := seedPendingRequesters(t, ts, serverID, channelID, 1, pendingSeedBase.Add(time.Hour), time.Millisecond, true)

	p1 := getPendingPage(t, ts, owner.AccessToken, "")
	assert.Empty(t, p1.ids, "page 1 stops at the scan cap before reaching the visible row")
	require.NotEmpty(t, p1.next, "a capped scan returns a cursor past the rows it examined")

	p2 := getPendingPage(t, ts, owner.AccessToken, pendingQuery(0, p1.next))
	assert.Equal(t, visible, p2.ids, "row 1,201 is reached on page 2")
	assert.Empty(t, p2.next)
}

func TestPendingKeys_ForgedCursorStillFiltered(t *testing.T) {
	ts, owner, serverID, ch1 := setupEncryptedChannel(t)
	caller := ts.CreateTestUser(t, "pkforgecaller")
	ts.AddMemberToServer(t, serverID, caller.ID, roleMember)
	dist := ts.DoRequest("POST", pathChannelsPrefix+ch1+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{caller.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, dist.Code, dist.Body.String())
	ch2 := createKeyedChannel(t, ts, owner, serverID, "text", caller.ID)
	ts.CreateChannelOverride(t, ch2, "user", caller.ID, 0, int64(rbac.PermViewTextChannels))

	// Hidden from the caller: requesters who cannot view ch1, and every row of
	// ch2, which the caller cannot view.
	hidden := seedPendingRequesters(t, ts, serverID, ch1, 3, pendingSeedBase, time.Millisecond, false)
	hidden = append(hidden, seedPendingRequesters(t, ts, serverID, ch2, 3, pendingSeedBase, time.Millisecond, true)...)
	visible := seedPendingRequesters(t, ts, serverID, ch1, 1, pendingSeedBase.Add(time.Hour), time.Millisecond, true)

	// The owner sees the ch2 rows; take a cursor from the owner's first page.
	ownerPage := getPendingPage(t, ts, owner.AccessToken, pendingQuery(1, ""))
	require.NotEmpty(t, ownerPage.next)

	zero := "00000000-0000-0000-0000-000000000000"
	cursors := map[string]string{
		"forged channel cursor before every row": rawCursor("1970-01-01T00:00:00Z|channel|" + zero),
		"forged dm cursor before every row":      rawCursor("1970-01-01T00:00:00Z|dm|" + zero),
		"another user's cursor":                  ownerPage.next,
	}
	for name, cursor := range cursors {
		t.Run(name, func(t *testing.T) {
			page := getPendingPage(t, ts, caller.AccessToken, pendingQuery(200, cursor))
			for _, id := range hidden {
				assert.NotContains(t, page.ids, id, "a hidden row must not leak through a cursor")
			}
			assert.Equal(t, visible, page.ids, "the caller's own visible row is still returned")
		})
	}
}

func TestPendingKeys_VisibilityMatchesLegacy(t *testing.T) {
	ts, owner, s1, ch1 := setupEncryptedChannel(t)
	caller := ts.CreateTestUser(t, "pklegacycaller")
	ts.AddMemberToServer(t, s1, caller.ID, roleMember)
	dist := ts.DoRequest("POST", pathChannelsPrefix+ch1+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{caller.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, dist.Code, dist.Body.String())
	ch2 := createKeyedChannel(t, ts, owner, s1, "text", caller.ID)  // caller denied view
	ch3 := createKeyedChannel(t, ts, owner, s1, "voice", caller.ID) // caller: text allow, voice deny
	ch4 := createKeyedChannel(t, ts, owner, s1, "voice", caller.ID) // caller views
	ch6 := createKeyedChannel(t, ts, owner, s1, "text", caller.ID)  // caller's key goes stale

	owner2 := ts.CreateTestUser(t, "pklegacyowner2")
	s2 := ts.CreateTestServer(t, owner2.ID, "PK Legacy S2")
	ts.AddMemberToServer(t, s2, caller.ID, roleMember)
	ch5 := createKeyedChannel(t, ts, owner2, s2, "text", caller.ID)

	viewText, viewVoice := int64(rbac.PermViewTextChannels), int64(rbac.PermViewVoiceChannels)
	member := func(serverID, channelID string) string {
		return seedPendingRequesters(t, ts, serverID, channelID, 1, pendingSeedBase, time.Millisecond, true)[0]
	}
	userOf := func(requestID string) string {
		var userID string
		require.NoError(t, ts.DB.QueryRow(`SELECT user_id FROM pending_key_requests WHERE id = $1`, requestID).Scan(&userID))
		return userID
	}

	rView := member(s1, ch1)
	rDeny := member(s1, ch1)
	ts.CreateChannelOverride(t, ch1, "user", userOf(rDeny), 0, viewText)
	seedPendingRequesters(t, ts, s1, ch1, 1, pendingSeedBase, time.Millisecond, false) // non-member
	adminUser := ts.CreateTestUser(t, "pklegacyadmin")
	ts.AddMemberToServer(t, s1, adminUser.ID, roleMember)
	adminRole := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO roles (id, server_id, name, permissions, position) VALUES ($1, $2, 'pk-administrator', $3, 1)`,
		adminRole, s1, int64(rbac.PermAdministrator))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, s1, adminUser.ID, adminRole)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, ch1, "user", adminUser.ID, 0, viewText) // Administrator ignores it
	var rAdmin string
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2) RETURNING id`,
		ch1, adminUser.ID).Scan(&rAdmin))

	member(s1, ch2)
	ts.CreateChannelOverride(t, ch2, "user", caller.ID, 0, viewText)
	member(s1, ch3)
	ts.CreateChannelOverride(t, ch3, "user", caller.ID, viewText, viewVoice)
	rVoiceOK := member(s1, ch4)
	rVoiceDeny := member(s1, ch4)
	ts.CreateChannelOverride(t, ch4, "user", userOf(rVoiceDeny), viewText, viewVoice)
	r5 := member(s2, ch5)
	member(s1, ch6)
	// A revocation issuing epoch 2 makes the caller's v1 key on ch6 stale.
	_, err = ts.DB.Exec(`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_id
		) VALUES ($1, 1, 2, 'member_removal', $2, $2)`, ch6, owner.ID)
	require.NoError(t, err)

	d1 := ts.CreateTestUser(t, "pklegacyd1")
	conv1 := ts.CreateDMConversation(t, caller.ID, d1.ID)
	ts.SeedDMKey(t, conv1, caller.ID, 1)
	dmVisible := seedPendingDMRequestAt(t, ts, conv1, d1.ID, pendingSeedBase)
	seedPendingDMRequestAt(t, ts, conv1, caller.ID, pendingSeedBase) // the caller's own request
	d2 := ts.CreateTestUser(t, "pklegacyd2")
	conv2 := ts.CreateDMConversation(t, caller.ID, d2.ID)
	ts.SeedDMKey(t, conv2, caller.ID, 1)
	seedPendingDMRequestAt(t, ts, conv2, d2.ID, pendingSeedBase)
	ts.CreateFriendship(t, caller.ID, d2.ID, "blocked")

	want := []string{rView, rAdmin, rVoiceOK, r5, dmVisible}

	log := logger.NewWithWriter(io.Discard)
	h := channels.NewHandler(ts.DB, log, nil, rbac.NewResolver(ts.DB, rbac.NewPermissionCache(nil), log), nil)
	legacy, err := h.LegacyPendingKeyRequestIDsForTest(context.Background(), caller.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, want, legacy, "fixture check: the legacy reader shows exactly the expected rows")

	for _, limit := range []int{2, 200} {
		assert.ElementsMatch(t, legacy, drainPending(t, ts, caller.AccessToken, limit),
			"limit=%d: the paged reader returns the legacy row set", limit)
	}
}

func TestPendingKeys_NoParamsBackCompat(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	ids := seedPendingRequesters(t, ts, serverID, channelID, 1, pendingSeedBase, time.Millisecond, true)

	w := ts.DoRequest("GET", pathE2EEPendingKeys, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Len(t, body, 2, "only pending_requests and next_cursor")
	next, present := body["next_cursor"]
	assert.True(t, present)
	assert.Nil(t, next, "a single page ends with a null cursor")

	requests := testhelpers.JSONField[[]interface{}](t, body, "pending_requests")
	require.Len(t, requests, 1)
	row := testhelpers.JSONElem[map[string]interface{}](t, requests, 0)
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"id", "channel_id", "user_id", "key_version", "created_at"}, keys, "row shape is unchanged")
	assert.Equal(t, ids[0], testhelpers.JSONField[string](t, row, "id"))
	assert.Equal(t, channelID, testhelpers.JSONField[string](t, row, "channel_id"))
	assert.Equal(t, float64(1), testhelpers.JSONField[float64](t, row, "key_version"))
	createdAt, err := time.Parse(time.RFC3339Nano, testhelpers.JSONField[string](t, row, "created_at"))
	require.NoError(t, err)
	assert.True(t, createdAt.Equal(pendingSeedBase.Add(time.Millisecond)), "created_at is RFC 3339: %s", createdAt)
}

func TestPendingKeys_LimitBounds(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	seedPendingRequesters(t, ts, serverID, channelID, 150, pendingSeedBase, time.Millisecond, true)

	for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?limit=-1", "?limit=", "?limit=1.5", "?limit=100&limit=5"} {
		w := ts.DoRequest("GET", pathE2EEPendingKeys+q, nil, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", q, w.Body.String())
	}

	absent := getPendingPage(t, ts, owner.AccessToken, "")
	assert.Len(t, absent.ids, 100, "an absent limit means 100")
	assert.NotEmpty(t, absent.next)
	maxPage := getPendingPage(t, ts, owner.AccessToken, pendingQuery(200, ""))
	assert.Len(t, maxPage.ids, 150, "limit=200 is accepted")
	assert.Empty(t, maxPage.next)
	assert.Len(t, getPendingPage(t, ts, owner.AccessToken, pendingQuery(1, "")).ids, 1)
}

func TestPendingCursor_RejectsMalformed(t *testing.T) {
	ts, owner, _, _ := setupEncryptedChannel(t)
	id := uuid.NewString()
	cases := map[string]string{
		"bad base64":      "!!!",
		"padded base64":   base64.URLEncoding.EncodeToString([]byte("2026-01-01T00:00:00Z|channel|" + id)),
		"missing field":   rawCursor("2026-01-01T00:00:00Z|channel"),
		"extra field":     rawCursor("2026-01-01T00:00:00Z|channel|" + id + "|x"),
		"unknown source":  rawCursor("2026-01-01T00:00:00Z|group|" + id),
		"bad time":        rawCursor("yesterday|channel|" + id),
		"bad id":          rawCursor("2026-01-01T00:00:00Z|dm|not-a-uuid"),
		"empty":           "",
		"overlong cursor": rawCursor("2026-01-01T00:00:00Z|channel|" + id + strings.Repeat("0", 200)),
	}
	for name, cursor := range cases {
		t.Run(name, func(t *testing.T) {
			w := ts.DoRequest("GET", pathE2EEPendingKeys+"?cursor="+url.QueryEscape(cursor), nil, testhelpers.AuthHeaders(owner.AccessToken))
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}
