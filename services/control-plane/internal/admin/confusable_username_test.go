package admin_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// confusablePairs are (existing, new) usernames that read alike. The existing
// name may be a legacy one the current rule would refuse.
var confusablePairs = [][2]string{
	{"Alice", "alice"},
	{"aIice", "alice"},
	{"a1ice", "alice"},
	{"a|ice", "alice"},
	{"alice", "aiice"},
	{"ops0", "opso"},
	{"opso", "ops0"},
	{"OPSO", "ops0"},
	{"corn", "com"},
	{"vvalt", "walt"},
	{"clan", "dan"},
	{"c1an", "dan"},
	{"first.last", "first_last"},
	{"first_last", "first-last"},
	{"first-last", "first.last"},
	{"kim", "klm"},
	// Non-ASCII legacy names: an accent, another script, an invisible
	// character, a space and case.
	{"Zoë", "zoe"},
	{"José", "jose"},
	{"\u0430lice", "alice"}, // Cyrillic a
	{"bob\u200b", "bob"},    // zero-width space
	{"Ops Team", "ops-team"},
	{"carol ", "carol"},
	// Separator runs and separators at either end.
	{"dave.", "dave"},
	{"erin-", "erin"},
	{"_frank", "frank"},
	{"gina..h", "gina.h"},
	// The pair rules reach one skeleton from either side.
	{"wvhank", "vwhank"},
}

func TestUsernameSkeleton_FoldsLookalikes(t *testing.T) {
	for _, p := range confusablePairs {
		assert.Equal(t, admin.UsernameSkeleton(p[0]), admin.UsernameSkeleton(p[1]), "%q and %q", p[0], p[1])
	}
	for _, p := range [][2]string{{"alice", "bob"}, {"bob", "rob"}, {"ops1", "ops2"}, {"dan", "dam"}, {"walt", "wall"}} {
		assert.NotEqual(t, admin.UsernameSkeleton(p[0]), admin.UsernameSkeleton(p[1]), "%q and %q", p[0], p[1])
	}
}

// legacyAdminNamed inserts an admin row with any username, as an admin created
// before the current rule could have, and removes it when the test ends.
func legacyAdminNamed(t *testing.T, db *sql.DB, username string) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(
		`INSERT INTO admin_users (username, password_hash, status) VALUES ($1, 'unused-hash', 'pending_enrollment') RETURNING id`,
		username,
	).Scan(&id))
	registerAdminCleanup(t, db, id)
	return id
}

// A new username whose skeleton matches an existing admin's is refused, and
// CheckUsernameAvailable refuses it the same way without the lock.
func TestCreatePending_RefusesConfusableOfExisting(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	repo := admin.NewAdminRepo(db)

	for i, p := range confusablePairs {
		sfx := fmt.Sprintf("-cf%d-%d", time.Now().UnixNano(), i)
		existing, fresh := p[0]+sfx, p[1]+sfx
		t.Run(existing+"/"+fresh, func(t *testing.T) {
			legacyAdminNamed(t, db, existing)
			created, err := repo.CreatePending(t.Context(), fresh, "unused-hash")
			if err == nil {
				registerAdminCleanup(t, db, created.ID)
			}
			require.ErrorIs(t, err, admin.ErrConfusableUsername)
			assert.ErrorIs(t, repo.CheckUsernameAvailable(t.Context(), fresh), admin.ErrConfusableUsername)
			assertNoAdmin(t, db, fresh)
		})
	}

	sfx := fmt.Sprintf("-cf%d", time.Now().UnixNano())
	pendingAdminNamed(t, db, "bob"+sfx)
	_, err := repo.CreatePending(t.Context(), "bob"+sfx, "unused-hash")
	require.ErrorIs(t, err, admin.ErrDuplicateUsername, "an exact match is still a duplicate")
	pendingAdminNamed(t, db, "rob"+sfx)
}

// Two creations of lookalike names cannot both pass the check: the second
// waits on the create lock until the first commits, then sees its name.
func TestCreatePending_ConcurrentLookalikesSerialize(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	repo := admin.NewAdminRepo(db)
	sfx := fmt.Sprintf("-race%d", time.Now().UnixNano())

	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	first, err := repo.CreatePendingTx(t.Context(), tx, "opso"+sfx, "unused-hash")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		created, err := repo.CreatePending(t.Context(), "ops0"+sfx, "unused-hash")
		if err == nil {
			registerAdminCleanup(t, db, created.ID)
		}
		done <- err
	}()
	select {
	case err := <-done:
		assert.Fail(t, "the second creation ran while the first held the create lock", "err %v", err)
		done <- err
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	registerAdminCleanup(t, db, first.ID)

	select {
	case err := <-done:
		assert.ErrorIs(t, err, admin.ErrConfusableUsername)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the second creation did not finish after the first committed")
	}
	assertNoAdmin(t, db, "ops0"+sfx)
}

// An admin whose name the current rule refuses (uppercase, a space, & and
// non-ASCII) still signs in under that name.
func TestWebAuthnLogin_LegacyUsernameStillSignsIn(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	adminID, _, va := enrolledAdmin(t, db, rdb)
	legacy := fmt.Sprintf("Legacy Ops&Zoë %d", time.Now().UnixNano())
	require.Error(t, admin.ValidateAdminUsername(legacy))
	_, err := db.Exec(`UPDATE admin_users SET username = $1 WHERE id = $2`, legacy, adminID)
	require.NoError(t, err)

	code, body, cookie := signInAdmin(t, adminAuthEngine(t, db, rdb), legacy, va)
	require.Equal(t, http.StatusOK, code, string(body))
	assert.NotEmpty(t, cookie)
}

// The console refuses a lookalike name with a 400 before the step-up is
// redeemed, so the same proof then creates an admin under another name.
func TestCreateAdmin_ConfusableUsername_RefusedBeforeStepUpIsSpent(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	sfx := fmt.Sprintf("-cs%d", time.Now().UnixNano())
	legacyAdminNamed(t, db, "Ops0"+sfx)

	handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
	assertion := signStepUp(t, va, challenge, flagUP|flagUV)
	rec := postJSON(engine, "/admin/api/v1/admins", createBody("opso"+sfx, handle, assertion))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "looks like an existing admin username")
	assertNoAdmin(t, db, "opso"+sfx)

	rec = postJSON(engine, "/admin/api/v1/admins", createBody("other"+sfx, handle, assertion))
	require.Equal(t, http.StatusCreated, rec.Code, "the proof was not spent: %s", rec.Body.String())
	created, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), "other"+sfx)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
}
