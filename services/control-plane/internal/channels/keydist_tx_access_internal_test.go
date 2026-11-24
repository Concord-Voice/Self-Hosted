package channels

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// VULN-079: the per-target access check of a key distribution must run on the
// distribution transaction. The pool-backed check needs a second connection
// while the first holds the channel row lock, so enough concurrent
// distributions exhaust the shared pool and deadlock it.

// keyAccessFixture builds servers, channels and members with raw SQL; the
// testhelpers package imports this one, so it cannot be used here.
type keyAccessFixture struct {
	t  *testing.T
	db *sql.DB
}

func (f keyAccessFixture) user() string {
	return dbtest.CreateUser(f.t, f.db).String()
}

func (f keyAccessFixture) server(ownerID string) string {
	f.t.Helper()
	id := uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, $2, $3)`, id, "s_"+id[:8], ownerID)
	require.NoError(f.t, err)
	f.member(id, ownerID)
	return id
}

func (f keyAccessFixture) channel(serverID, channelType string) string {
	f.t.Helper()
	id := uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO channels (id, server_id, name, type, position)
		VALUES ($1, $2, $3, $4, 0)`, id, serverID, "c_"+id[:8], channelType)
	require.NoError(f.t, err)
	return id
}

func (f keyAccessFixture) member(serverID, userID string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, userID)
	require.NoError(f.t, err)
}

func (f keyAccessFixture) grantRole(serverID, userID string, perms rbac.Permission) string {
	f.t.Helper()
	roleID := uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO roles (id, server_id, name, permissions, position)
		VALUES ($1, $2, $3, $4, 1)`, roleID, serverID, "r_"+roleID[:8], int64(perms))
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, userID, roleID)
	require.NoError(f.t, err)
	return roleID
}

func (f keyAccessFixture) userOverride(channelID, userID string, allow, deny rbac.Permission) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO channel_permission_overrides (channel_id, target_type, target_id, allow, deny)
		VALUES ($1, 'user', $2, $3, $4)`, channelID, userID, int64(allow), int64(deny))
	require.NoError(f.t, err)
}

// openSingleConnPool opens a private pool of one connection. A regression that
// leaks that connection then fails its own test instead of stalling the shared
// truncate cleanup. Call it after SetupTestDB so it closes before that cleanup.
func openSingleConnPool(t *testing.T) *sql.DB {
	t.Helper()
	single, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := single.Close(); closeErr != nil {
			t.Errorf("close one-connection pool: %v", closeErr)
		}
	})
	single.SetMaxOpenConns(1)
	return single
}

func newKeyAccessHandler(db *sql.DB) *Handler {
	log := logger.NewWithWriter(io.Discard)
	return NewHandler(db, log, nil, rbac.NewResolver(db, rbac.NewPermissionCache(nil), log), nil)
}

// TestChannelKeyAccessTx_MatchesPoolVariant pins that moving the check onto the
// transaction changes no authorization result. Each row asserts a fixed
// expectation AND equality with the pool variant, so a regression in either
// cannot hide behind the other. Where the caller is a member it also compares
// the full permission masks of the two resolver entry points.
func TestChannelKeyAdmissionTx_MatchesPoolVariant(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	h := newKeyAccessHandler(db)

	viewText := rbac.PermViewTextChannels

	cases := []struct {
		name                 string
		setup                func(f keyAccessFixture) (channelID, userID string)
		wantMember, wantView bool
	}{
		{"owner", func(f keyAccessFixture) (string, string) {
			owner := f.user()
			return f.channel(f.server(owner), "text"), owner
		}, true, true},
		{"administrator role ignores a channel deny", func(f keyAccessFixture) (string, string) {
			srv := f.server(f.user())
			ch := f.channel(srv, "text")
			admin := f.user()
			f.member(srv, admin)
			f.grantRole(srv, admin, rbac.PermAdministrator)
			f.userOverride(ch, admin, 0, viewText)
			return ch, admin
		}, true, true},
		{"channel allow override grants view", func(f keyAccessFixture) (string, string) {
			srv := f.server(f.user())
			ch := f.channel(srv, "text")
			u := f.user()
			f.member(srv, u)
			f.userOverride(ch, u, viewText, 0)
			return ch, u
		}, true, true},
		{"channel deny override removes view", func(f keyAccessFixture) (string, string) {
			srv := f.server(f.user())
			ch := f.channel(srv, "text")
			u := f.user()
			f.member(srv, u)
			f.grantRole(srv, u, viewText)
			f.userOverride(ch, u, 0, viewText)
			return ch, u
		}, true, false},
		{"MFA-masked administrator", func(f keyAccessFixture) (string, string) {
			srv := f.server(f.user())
			_, err := db.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, srv)
			require.NoError(f.t, err)
			ch := f.channel(srv, "text")
			u := f.user() // not enrolled, so the mask applies
			f.member(srv, u)
			f.grantRole(srv, u, rbac.PermAdministrator)
			f.userOverride(ch, u, 0, viewText)
			return ch, u
		}, true, true},
		{"text-only role cannot view a voice channel", func(f keyAccessFixture) (string, string) {
			srv := f.server(f.user())
			ch := f.channel(srv, "voice")
			u := f.user()
			f.member(srv, u)
			f.grantRole(srv, u, viewText)
			return ch, u
		}, true, false},
		{"non-member", func(f keyAccessFixture) (string, string) {
			return f.channel(f.server(f.user()), "text"), f.user()
		}, false, false},
		{"channel in another server", func(f keyAccessFixture) (string, string) {
			u := f.user()
			own := f.server(f.user())
			f.member(own, u)
			f.grantRole(own, u, viewText)
			return f.channel(f.server(f.user()), "text"), u
		}, false, false},
	}

	nilResolver := NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channelID, userID := tc.setup(keyAccessFixture{t: t, db: db})
			var serverID string
			require.NoError(t, db.QueryRow(`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID))

			// The batch admission must give the per-user pool check's answer,
			// with the resolver and without one (membership alone admits).
			variants := []struct {
				name      string
				h         *Handler
				wantAdmit bool
			}{
				{"resolver", h, tc.wantMember && tc.wantView},
				{"nil resolver", nilResolver, tc.wantMember},
			}
			for _, v := range variants {
				poolMember, poolView, poolErr := v.h.channelKeyAccess(ctx, channelID, userID)
				require.NoError(t, poolErr)
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				admitted, txErr := v.h.admitChannelKeyTargetsTx(ctx, tx, serverID, channelID, []string{userID})
				require.NoError(t, tx.Rollback())
				require.NoError(t, txErr)
				assert.Equal(t, v.wantAdmit, len(admitted) == 1, "%s: batch admission", v.name)
				assert.Equal(t, poolMember && poolView, len(admitted) == 1, "%s: batch admission must match the pool check", v.name)
			}

			if !tc.wantMember {
				return
			}
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			poolMask, err := h.resolver.ResolveEffectivePermissionsUncached(ctx, serverID, userID, channelID)
			require.NoError(t, err)
			txMask, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, serverID, userID, channelID)
			require.NoError(t, err)
			assert.Equal(t, poolMask, txMask, "permission masks must match")
		})
	}
}

// TestChannelKeyAdmissionTx_MixedBatch: one statement answers for every target
// in the batch, so a viewer, a member without view, and a stranger sent
// together get the same answers they get alone.
func TestChannelKeyAdmissionTx_MixedBatch(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	f := keyAccessFixture{t: t, db: db}
	srv := f.server(f.user())
	ch := f.channel(srv, "text")
	viewer, hidden, stranger := f.user(), f.user(), f.user()
	f.member(srv, viewer)
	f.grantRole(srv, viewer, rbac.PermViewTextChannels)
	f.member(srv, hidden)
	h := newKeyAccessHandler(db)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	admitted, err := h.admitChannelKeyTargetsTx(ctx, tx, srv, ch, []string{stranger, hidden, viewer})
	require.NoError(t, err)
	assert.Equal(t, []string{viewer}, admitted)
}

// TestDistribution_NoPoolConnWhileTxOpen is the VULN-079 shape at its
// smallest: a pool of one connection, held by the distribution transaction.
// A check that asks the pool for a second connection blocks until the
// deadline; a check on the transaction returns at once.
func TestDistribution_NoPoolConnWhileTxOpen(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	f := keyAccessFixture{t: t, db: db}
	srv := f.server(f.user())
	ch := f.channel(srv, "text")
	target := f.user()
	f.member(srv, target)
	f.grantRole(srv, target, rbac.PermViewTextChannels)

	single := openSingleConnPool(t)
	h := newKeyAccessHandler(single)
	tx, err := single.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	const deadline = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	admitted, err := h.admitChannelKeyTargetsTx(ctx, tx, srv, ch, []string{target})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, deadline, "the check must not wait for a second pool connection")
	require.NoError(t, err, "the check must not fail")
	assert.Equal(t, []string{target}, admitted, "a viewing member is admitted")
}

// TestDistribution_AdmissionTimeoutFailsBatch: a lock timeout in the batch's
// view check fails the whole batch with an error the route maps to 503. It is
// never skip-and-continue: no target is admitted on a failed check.
func TestDistribution_AdmissionTimeoutFailsBatch(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	f := keyAccessFixture{t: t, db: db}
	owner := f.user()
	srv := f.server(owner)
	ch := f.channel(srv, "text")
	member := f.user()
	f.member(srv, member)
	f.grantRole(srv, member, rbac.PermViewTextChannels)
	h := newKeyAccessHandler(db)

	locker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = locker.Rollback() }()
	_, err = locker.ExecContext(ctx, `LOCK TABLE roles IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '200ms'`)
	require.NoError(t, err)

	admitted, err := h.admitChannelKeyTargetsTx(ctx, tx, srv, ch, []string{member, owner})
	require.Error(t, err)
	assert.Nil(t, admitted, "a failed check admits nobody")
	class, timedOut := keyDistributionTimeoutClass(err)
	assert.True(t, timedOut, "the lock timeout must reach the 503 mapping: %v", err)
	assert.Equal(t, "distribution_lock_timeout", class)
}

// TestSetDistributionTimeouts_AppliesInTxOnly: the bounds hold inside the
// distribution transaction and are gone after it. The pool has one
// connection, so the post-commit read is on the session the transaction used.
func TestSetDistributionTimeouts_AppliesInTxOnly(t *testing.T) {
	_, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	single := openSingleConnPool(t)
	ctx := context.Background()
	show := func(q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}, setting string) string {
		t.Helper()
		var v string
		require.NoError(t, q.QueryRowContext(ctx, `SELECT current_setting($1)`, setting).Scan(&v))
		return v
	}
	bounds := map[string]string{
		"lock_timeout":                        "5s",
		"statement_timeout":                   "10s",
		"idle_in_transaction_session_timeout": "15s",
	}

	tx, err := single.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for setting := range bounds {
		require.Equal(t, "0", show(tx, setting), "precondition: %s is unset before the call", setting)
	}
	require.NoError(t, setDistributionTimeouts(ctx, tx))
	for setting, want := range bounds {
		assert.Equal(t, want, show(tx, setting), "%s inside the transaction", setting)
	}
	require.NoError(t, tx.Commit())

	for setting := range bounds {
		assert.Equal(t, "0", show(single, setting), "%s must not outlive the transaction", setting)
	}
}

// TestRespondKeyDistributionTimeout pins the 503 mapping for a distribution
// that gave up on a bound, with each route's own generic body.
func TestRespondKeyDistributionTimeout(t *testing.T) {
	h := &Handler{log: logger.NewWithWriter(io.Discard)}
	lockTimeout := fmt.Errorf("lock channel for key distribution: %w", &pq.Error{Code: pgLockNotAvailable})
	stmtTimeout := fmt.Errorf("store key: %w", &pq.Error{Code: pgQueryCanceled})

	t.Run("lock timeout is a retryable 503", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, lockTimeout, "ctx-1")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedDistributeKeys)
	})
	t.Run("statement timeout is a retryable 503", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, stmtTimeout, "ctx-2")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedDistributeKeys)
	})
	t.Run("rotation keeps its own body", func(t *testing.T) {
		c, w := newInternalTestContext()
		require.True(t, h.respondKeyDistributionTimeout(c, lockTimeout, "ctx-3", errMsgFailedRotateKey))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedRotateKey)
	})
	t.Run("an expired request deadline is a retryable 503", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, errors.Join(sql.ErrTxDone, context.DeadlineExceeded), "ctx-5")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedDistributeKeys)
	})
	t.Run("a client cancel is logged with its own class", func(t *testing.T) {
		var logs bytes.Buffer
		logged := &Handler{log: logger.NewWithWriter(&logs)}
		c, w := newInternalTestContext()
		reqCtx, cancelReq := context.WithCancel(context.Background())
		cancelReq()
		c.Request = c.Request.WithContext(reqCtx)
		logged.respondKeyDistributionError(c, stmtTimeout, "ctx-6")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, logs.String(), "distribution_client_canceled")
		assert.NotContains(t, logs.String(), "distribution_statement_timeout")
	})
	t.Run("a statement timeout without a cancel keeps its class", func(t *testing.T) {
		var logs bytes.Buffer
		logged := &Handler{log: logger.NewWithWriter(&logs)}
		c, _ := newInternalTestContext()
		logged.respondKeyDistributionError(c, stmtTimeout, "ctx-7")
		assert.Contains(t, logs.String(), "distribution_statement_timeout")
		assert.NotContains(t, logs.String(), "distribution_client_canceled")
	})
	t.Run("another database error stays a 500", func(t *testing.T) {
		c, w := newInternalTestContext()
		h.respondKeyDistributionError(c, fmt.Errorf("store key: %w", &pq.Error{Code: "23505"}), "ctx-4")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}
