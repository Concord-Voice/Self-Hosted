package channels_test

// A row the unread handlers cannot scan must fail the request. Skipping it
// would answer 200 with a list that silently omits a channel.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/stmthook"
)

const (
	// Each fragment appears in exactly one statement: the handler's own query.
	// No resolver statement selects an unread count.
	unreadCountsFragment       = `COUNT(m.id)::int AS unread_count`
	serverUnreadStatusFragment = `COUNT(*) AS unread_count`
)

// assertScanErrorAnswers500 drives one request three ways against the same
// armed fragment: a query failure (the reference body), a row whose column
// cannot be scanned, and a control whose fragment matches nothing.
func assertScanErrorAnswers500(
	t *testing.T, hook *stmthook.Hook, fragment string, column int,
	get func() *httptest.ResponseRecorder, controlKey string,
) {
	t.Helper()

	hook.Arm([]string{fragment}, nil, stmthook.ErrInjected)
	failed := get()
	require.Equal(t, http.StatusInternalServerError, failed.Code, "body: %s", failed.Body.String())

	t.Run("control: unmatched fragment leaves the rows", func(t *testing.T) {
		hook.ArmRowValue("no statement contains this fragment", column, "x")
		w := get()
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Zero(t, hook.RowValueHits())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.NotEmpty(t, testhelpers.JSONField[[]interface{}](t, body, controlKey))
	})

	t.Run("unscannable column answers the query-failure body", func(t *testing.T) {
		hook.ArmRowValue(fragment, column, "x")
		w := get()
		assert.GreaterOrEqual(t, hook.RowValueHits(), 1, "a row must have been rewritten")
		assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		assert.Equal(t, failed.Body.String(), w.Body.String())
	})
}

func TestGetUnreadCounts_ScanErrorAnswers500(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	owner := ts.CreateTestUser(t, "unscanown")
	serverID := ts.CreateTestServer(t, owner.ID, "Unread Scan Error")
	ts.CreateTestChannel(t, serverID, "unread-scan")

	get := func() *httptest.ResponseRecorder {
		return ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(owner.AccessToken))
	}
	// Every readable channel yields a row, so no messages are needed.
	assertScanErrorAnswers500(t, hook, unreadCountsFragment, 1, get, "unreads")
}

func TestGetServerUnreadStatus_ScanErrorAnswers500(t *testing.T) {
	hook, hookedDB := stmthook.Open(t)
	ts := testhelpers.SetupTestServerWithRouterDB(t, hookedDB)

	owner := ts.CreateTestUser(t, "usscanown")
	member := ts.CreateTestUser(t, "usscanmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Unread Status Scan Error")
	channelID := ts.CreateTestChannel(t, serverID, "status-scan")
	// The member joins first, then the owner posts: the query INNER JOINs
	// messages, so without a newer unread message there is no row to rewrite.
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	insertUnreadMessage(t, ts, channelID, owner.ID)

	get := func() *httptest.ResponseRecorder {
		return ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(member.AccessToken))
	}
	assertScanErrorAnswers500(t, hook, serverUnreadStatusFragment, 2, get, "channels")
}
