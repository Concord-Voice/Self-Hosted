package admin_test

import (
	"bytes"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
)

// Regression for the SH#394 notice bypass. The notice once matched success
// rows to admins by username, but the audit writer drops control characters
// from the actor, so an admin created as "evil\x01" (or as a name of only
// control characters, stored as a NULL actor) never matched and the notice
// reported 0. Both ends are closed: such a name is refused (400) before the
// step-up is spent, and the notice matches on the created admin's id, so an
// admin that already has such a name is still counted.
var hiddenAdminNames = map[string]string{
	"control character": "\x01",
	"only controls":     "",
}

func TestCreateAdmin_ControlCharacterUsername_RefusedBeforeStepUp(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	for name, suffix := range hiddenAdminNames {
		t.Run(name, func(t *testing.T) {
			username := "\x02\x7f\x1b"
			if suffix != "" {
				username = uniqueAdminUsername("hidden") + suffix
			}
			handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
			assertion := signStepUp(t, va, challenge, flagUP|flagUV)
			rec := postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, assertion))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "enrollment_token")
			cleanupAdminNamed(t, db, username)
			assertNoAdmin(t, db, username)

			// The refusal did not spend the step-up: the same proof creates a
			// well-formed admin.
			valid := uniqueAdminUsername("hidden-valid")
			rec = postJSON(engine, "/admin/api/v1/admins", createBody(valid, handle, assertion))
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			created, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), valid)
			require.NoError(t, err)
			registerAdminCleanup(t, db, created.ID)
		})
	}
}

// An admin whose name the audit writer rewrites is counted by id.
func TestAdminsAddedSinceLastSignIn_SanitizedActorStillCounted(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	for name, suffix := range hiddenAdminNames {
		t.Run(name, func(t *testing.T) {
			viewerID, _, _ := enrolledAdmin(t, db, rdb)
			actingID, _, _ := enrolledAdmin(t, db, rdb)
			insertAuditRow(t, db, &viewerID, "viewer", admin.EventLoginSuccess, admin.AuditSuccess, "-1 minute")
			username := "\x01\x02"
			if suffix != "" {
				username = uniqueAdminUsername("hidden") + suffix
			}
			// An admin created before the username rule existed.
			var createdID string
			require.NoError(t, db.QueryRow(
				`INSERT INTO admin_users (username, password_hash, status)
				 VALUES ($1, 'unused-hash', 'pending_enrollment') RETURNING id`, username,
			).Scan(&createdID))
			registerAdminCleanup(t, db, createdID)
			// Its success row, through the audit writer that sanitizes the actor.
			acting := actingID
			require.NoError(t, admin.NewAuditLog(db).Write(t.Context(), admin.AuditEvent{
				AdminID: &acting, Actor: username, EventType: admin.EventAdminCreated, Result: admin.AuditSuccess,
				Detail: map[string]any{"created_admin_id": createdID},
			}))
			t.Cleanup(func() {
				_, err := db.Exec(`DELETE FROM admin_audit_log WHERE detail->>'created_admin_id' = $1`, createdID)
				assert.NoError(t, err)
			})

			n, err := admin.NewAuditLog(db).AdminsAddedSinceLastSignIn(t.Context(), viewerID)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "an admin created with a stolen session must show in the notice")
		})
	}
}

// CreateAdmin records the created admin's id in its success row, and the
// notice counts that admin.
func TestCreateAdmin_SuccessRowCarriesCreatedAdminID(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rCleanup)

	actingID, _, va := enrolledAdmin(t, db, rdb)
	engine := adminCreateEngine(t, db, rdb, actingID)
	handle, challenge := stepUpBegin(t, engine, "/admin/api/v1/admins/stepup", map[string]any{})
	username := uniqueAdminUsername("carries-id")
	rec := postJSON(engine, "/admin/api/v1/admins", createBody(username, handle, signStepUp(t, va, challenge, flagUP|flagUV)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created, err := admin.NewAdminRepo(db).GetByUsername(t.Context(), username)
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)

	var recorded string
	require.NoError(t, db.QueryRow(
		`SELECT detail->>'created_admin_id' FROM admin_audit_log
		 WHERE event_type = $1 AND admin_id = $2 AND result = $3 AND actor = $4`,
		admin.EventAdminCreated, actingID, admin.AuditSuccess, username,
	).Scan(&recorded))
	assert.Equal(t, created.ID, recorded)
}

func TestAdminCtl_Bootstrap_RejectsInvalidUsername(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, rdbCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(rdbCleanup)

	for _, username := range []string{uniqueAdminUsername("boot") + "\x01", "\x01\x02", " " + uniqueAdminUsername("boot")} {
		var stdout bytes.Buffer
		code := admin.RunAdminCtlForTest(t.Context(), db, rdb, strings.NewReader("Str0ng-P@ssw0rd-123\n"), &stdout, testEnrollBaseURL,
			[]string{"bootstrap", "--username", username, "--password-stdin"})
		assert.Equal(t, 1, code, "%q", username)
		assert.Contains(t, stdout.String(), "invalid username")
		cleanupAdminNamed(t, db, username)
		assertNoAdmin(t, db, username)
	}
}

// cleanupAdminNamed removes any admin with this username when the test ends,
// so a run against code that wrongly creates it leaves nothing behind.
func cleanupAdminNamed(t *testing.T, db *sql.DB, username string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_users WHERE username = $1`, username)
		assert.NoError(t, err)
	})
}
