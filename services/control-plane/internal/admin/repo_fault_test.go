package admin_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers/testdb"
)

// errInjectedFault is the error the fault connector returns at its fault point.
var errInjectedFault = errors.New("injected database fault")

// dbFault names the one point where a faultConnector fails. Statements whose
// text contains statement fail; begin, commit and rollback fail the matching
// transaction step. Everything else reaches PostgreSQL unchanged.
type dbFault struct {
	statement string
	begin     bool
	commit    bool
	rollback  bool
}

type faultConnector struct {
	dsn   string
	fault dbFault
}

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := pq.Driver{}.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: conn, fault: c.fault}, nil
}

func (c faultConnector) Driver() driver.Driver { return pq.Driver{} }

type faultConn struct {
	driver.Conn
	fault dbFault
}

func (c *faultConn) failsOn(query string) bool {
	return c.fault.statement != "" && strings.Contains(query, c.fault.statement)
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.failsOn(query) {
		return nil, errInjectedFault
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.failsOn(query) {
		return nil, errInjectedFault
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.fault.begin {
		return nil, errInjectedFault
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, fault: c.fault}, nil
}

type faultTx struct {
	driver.Tx
	fault dbFault
}

func (tx *faultTx) Commit() error {
	if tx.fault.commit {
		_ = tx.Tx.Rollback()
		return errInjectedFault
	}
	return tx.Tx.Commit()
}

func (tx *faultTx) Rollback() error {
	if err := tx.Tx.Rollback(); err != nil {
		return err
	}
	if tx.fault.rollback {
		return errInjectedFault
	}
	return nil
}

// faultRepo returns an AdminRepo on the shared test database whose connections
// fail at fault.
func faultRepo(t *testing.T, fault dbFault) *admin.AdminRepo {
	t.Helper()
	db := sql.OpenDB(faultConnector{dsn: testdb.DatabaseURL(), fault: fault})
	t.Cleanup(func() { _ = db.Close() })
	return admin.NewAdminRepo(db)
}

// pendingAdmin creates a pending administrator through the real repository.
func pendingAdmin(t *testing.T, db *sql.DB) admin.AdminUser {
	t.Helper()
	repo := admin.NewAdminRepo(db)
	created, err := repo.CreatePending(context.Background(), uniqueAdminUsername("fault"), "test-hash")
	require.NoError(t, err)
	registerAdminCleanup(t, db, created.ID)
	return created
}

func credentialCount(t *testing.T, db *sql.DB, adminID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM admin_webauthn_credentials WHERE admin_id = $1`, adminID).Scan(&n))
	return n
}

func TestAdminRepo_CompleteEnrollment_UnknownAdmin(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	_, err := admin.NewAdminRepo(db).CompleteEnrollment(context.Background(), 1, admin.AdminCredential{
		AdminID:      "00000000-0000-0000-0000-000000000000",
		CredentialID: []byte("unknown-admin-cred"),
		PublicKey:    []byte("pk"),
		AAGUID:       []byte("0123456789abcdef"),
	})
	require.ErrorIs(t, err, admin.ErrAdminNotFound)
}

func TestAdminRepo_CompleteEnrollment_DatabaseFaultsLeaveAdminPending(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	cases := map[string]struct {
		fault dbFault
		want  string
	}{
		"begin":    {dbFault{begin: true}, "begin complete admin enrollment"},
		"lock":     {dbFault{statement: "FOR UPDATE"}, "lock admin enrollment state"},
		"insert":   {dbFault{statement: "INSERT INTO admin_webauthn_credentials"}, "add admin credential"},
		"activate": {dbFault{statement: "SET status = $2, updated_at"}, "activate enrolled admin"},
		"commit":   {dbFault{commit: true}, "commit complete admin enrollment"},
		"rollback": {dbFault{statement: "INSERT INTO admin_webauthn_credentials", rollback: true}, "rollback complete admin enrollment"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			created := pendingAdmin(t, db)
			_, err := faultRepo(t, tc.fault).CompleteEnrollment(context.Background(), created.CredentialGeneration, admin.AdminCredential{
				AdminID:      created.ID,
				CredentialID: []byte("fault-cred-" + name + "-" + created.ID),
				PublicKey:    []byte("pk"),
				AAGUID:       []byte("0123456789abcdef"),
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, errInjectedFault)
			assert.Contains(t, err.Error(), tc.want)

			got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), created.Username)
			require.NoError(t, err)
			assert.Equal(t, admin.StatusPending, got.Status, "a failed enrollment must not activate the admin")
			assert.Zero(t, credentialCount(t, db, created.ID), "a failed enrollment must not keep the credential")
		})
	}
}

func TestAdminRepo_ResetEnrollment_DatabaseFaultsChangeNothing(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)

	cases := map[string]struct {
		fault dbFault
		want  string
	}{
		"begin":    {dbFault{begin: true}, "begin reset admin enrollment"},
		"advance":  {dbFault{statement: "credential_generation = credential_generation + 1"}, "advance admin credential generation"},
		"delete":   {dbFault{statement: "DELETE FROM admin_webauthn_credentials"}, "delete admin credentials"},
		"commit":   {dbFault{commit: true}, "commit reset admin enrollment"},
		"rollback": {dbFault{statement: "DELETE FROM admin_webauthn_credentials", rollback: true}, "rollback reset admin enrollment"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			created := pendingAdmin(t, db)
			_, _, _, err := faultRepo(t, tc.fault).ResetEnrollment(context.Background(), created.ID)
			require.Error(t, err)
			assert.ErrorIs(t, err, errInjectedFault)
			assert.Contains(t, err.Error(), tc.want)

			got, err := admin.NewAdminRepo(db).GetByUsername(context.Background(), created.Username)
			require.NoError(t, err)
			assert.Equal(t, created.CredentialGeneration, got.CredentialGeneration, "a failed reset must not advance the generation")
		})
	}
}
