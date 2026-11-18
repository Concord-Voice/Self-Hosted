package devicerecovery

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The shared fixture otherwise falls back to the developer's concord database.
// Accept explicitly configured scratch/CI databases without fixing host, port,
// user or test database name to this implementation session's environment.
func explicitDatabaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return false
	}
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" || strings.EqualFold(database, "concord") {
		return false
	}
	// Refuse query overrides that could redirect the parsed path's database.
	query := u.Query()
	return !query.Has("dbname") && !query.Has("database")
}

func TestExplicitDatabaseURLGuard(t *testing.T) {
	for _, raw := range []string{
		"postgres://test@localhost:5432/concord_test?sslmode=disable",
		"postgres://recovery_test@127.0.0.1:55432/recovery_test?sslmode=disable",
		"postgresql://runner@db:6432/disposable_suite?sslmode=disable",
	} {
		require.True(t, explicitDatabaseURL(raw), raw)
	}
	for _, raw := range []string{
		"", "postgres://runner@localhost:5432/", "postgres://runner@localhost:5432/concord",
		"postgres://runner@db:5432/Concord", "postgres://runner@db:5432/%63oncord",
		"postgres://runner@db:5432/concord_test?dbname=concord", "postgres://runner@db:5432/concord_test?database=concord",
		"https://db/concord_test", "postgres:/concord_test",
	} {
		require.False(t, explicitDatabaseURL(raw), raw)
	}
}

func setupDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	require.True(t, explicitDatabaseURL(os.Getenv("DATABASE_URL")), "DATABASE_URL must explicitly select an isolated database; the default developer concord database is refused")
	db, _ := dbtest.SetupTestDB(t)
	id := dbtest.CreateUser(t, db).String()
	dbtest.RememberActor(t, db, id)
	trustSession(t, db, id, rememberedSID(t, id))
	return db, id
}

// rememberedSID is the session id of userID's remembered access identity.
func rememberedSID(t *testing.T, userID string) uuid.UUID {
	t.Helper()
	identity, ok := loginfamily.AccessIdentityFromContext(dbtest.WithRememberedActor(context.Background(), userID))
	require.True(t, ok)
	return identity.SID
}

// trustSession records sid's session on a fresh machine and designates that
// machine as userID's trusted recovery device. Fixture identities are
// access-only, so most have no refresh_tokens row until this adds one.
func trustSession(t *testing.T, db *sql.DB, userID string, sid uuid.UUID) string {
	t.Helper()
	machineID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (id) DO UPDATE SET machine_id = EXCLUDED.machine_id`, sid, userID, uuid.NewString(), time.Now().Add(time.Hour), machineID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'synthetic device',$2)`, userID, machineID)
	require.NoError(t, err)
	return machineID
}

// untrustedActor mints a second live session of userID with no designation.
func untrustedActor(t *testing.T, db *sql.DB, userID string) context.Context {
	t.Helper()
	return dbtest.ActorContext(context.Background(), t, db, userID)
}

func requestSnapshot(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(r)::text FROM recovery_requests r WHERE id=$1`, id).Scan(&snapshot))
	return snapshot
}
func newRequest(t *testing.T, db *sql.DB, id string) Row {
	t.Helper()
	b := createBody(t)
	b.AccountBinding = AccountBinding(id)
	r, err := Create(context.Background(), db, id, "public-jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.NoError(t, err)
	return r
}
func offerBody(t *testing.T, r Row) RespondBody {
	t.Helper()
	v := fixture(t)
	o := v.Offer
	_, h, err := Transcript(r.Context, o)
	require.NoError(t, err)
	return RespondBody{Action: "offer", ProtocolVersion: Version, ResponderPublicKey: o.ResponderPublicKey, ResponderNonce: o.ResponderNonce, TranscriptHash: Encode(h)}
}
func approveBody(t *testing.T, o RespondBody) RespondBody {
	t.Helper()
	return RespondBody{Action: "approve", ProtocolVersion: Version, TranscriptHash: o.TranscriptHash, EncryptedPayload: fixture(t).Envelope}
}
func TestDatabaseHappyPathOwnershipEpochAndCompletion(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	other := dbtest.CreateUser(t, db).String()
	dbtest.RememberActor(t, db, other)
	// Another account's own trusted device still cannot see this request.
	trustSession(t, db, other, rememberedSID(t, other))
	_, err := Poll(ctx, db, other, "public-jti", r.RequestID, allowRecovery)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Poll(ctx, db, id, "other-jti", r.RequestID, allowRecovery)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Poll(ctx, db, id, "public-jti", "NOT-A-UUID", allowRecovery)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Respond(dbtest.WithRememberedActor(ctx, other), db, other, "", r.RequestID, o)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, approveBody(t, o))
	require.ErrorIs(t, err, ErrConflict)
	bad := o
	bad.TranscriptHash = Encode(Hash("wrong"))
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, bad)
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
	require.ErrorIs(t, err, ErrConflict)
	polled, err := Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
	require.NoError(t, err)
	require.Equal(t, "offered", polled.Status)
	require.Equal(t, o.TranscriptHash, polled.TranscriptHash)
	listed, err := List(dbtest.WithRememberedActor(ctx, id), db, id)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
	require.ErrorIs(t, err, ErrConflict)
	a := approveBody(t, o)
	bad = a
	bad.TranscriptHash = Encode(Hash("wrong"))
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, bad)
	require.ErrorIs(t, err, ErrConflict)
	const rotated = "0123456789abcdef0123456789abcdef"
	_, err = db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, id, rotated)
	require.NoError(t, err)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, a)
	require.ErrorIs(t, err, ErrUnauthorized)
	rotatedActor := dbtest.ActorContext(ctx, t, db, id)
	rotatedIdentity, ok := loginfamily.AccessIdentityFromContext(rotatedActor)
	require.True(t, ok)
	trustSession(t, db, id, rotatedIdentity.SID)
	_, err = Respond(rotatedActor, db, id, rotated, r.RequestID, a)
	require.NoError(t, err)
	listed, err = List(dbtest.WithRememberedActor(ctx, id), db, id)
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = Complete(ctx, db, id, "other-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, Encode(Hash("wrong"))}, allowRecovery)
	require.ErrorIs(t, err, ErrConflict)
	// The store never reads the requester's credential epoch; the recovery guard
	// (allowRecovery here) is where the token's epoch binding is enforced.
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
	require.NoError(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
	require.ErrorIs(t, err, ErrConflict)
	polled, err = Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
	require.NoError(t, err)
	require.Nil(t, polled.Payload)
	require.Equal(t, "complete", polled.Status)
	require.NotContains(t, marshal(t, polled.Wire()), "requester_public_key")
	require.NotContains(t, marshal(t, polled.Wire()), "encrypted_payload")
}
func runRace(t *testing.T, actions ...func() error) {
	t.Helper()
	start := make(chan struct{})
	out := make(chan error, len(actions))
	var wg sync.WaitGroup
	for _, act := range actions {
		wg.Go(func() { <-start; out <- act() })
	}
	close(start)
	wg.Wait()
	close(out)
	wins := 0
	for err := range out {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrConflict)
		}
	}
	require.Equal(t, 1, wins)
}
func TestDatabaseConflictingWritersAndExactCAS(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	runRace(t, func() error {
		_, e := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
		return e
	}, func() error {
		_, e := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
		return e
	})
	a := approveBody(t, o)
	reject := RespondBody{Action: "reject", ProtocolVersion: Version}
	runRace(t, func() error {
		_, e := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, a)
		return e
	}, func() error {
		_, e := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, reject)
		return e
	})
	r = newRequest(t, db, id)
	o = offerBody(t, r)
	_, err := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, approveBody(t, o))
	require.NoError(t, err)
	runRace(t, func() error {
		_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
		return e
	}, func() error {
		_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
		return e
	})
	// An altered immutable snapshot can never produce an acknowledged write.
	r = newRequest(t, db, id)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, lockUser(ctx, tx, id))
	before, err := lockRequest(ctx, tx, id, r.RequestID, "")
	require.NoError(t, err)
	after := before
	after.Status = "rejected"
	before.ServerOrigin = "https://other.example.test"
	require.ErrorIs(t, cas(ctx, tx, before, after, false), ErrConflict)
	require.NoError(t, tx.Rollback())
}
func TestDatabaseLockWaitExpiresApprovedAndWithholdsCiphertext(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	// Preserve valid creation/expiry constraints while arranging a short deadline.
	_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '1 minute', expires_at=clock_timestamp()+interval '700 milliseconds' WHERE id=$1`, r.RequestID)
	require.NoError(t, err)
	r, err = Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
	require.NoError(t, err)
	o := offerBody(t, r)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, approveBody(t, o))
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, lockUser(ctx, tx, id))
	result := make(chan Row, 1)
	failure := make(chan error, 1)
	go func() { p, e := Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery); result <- p; failure <- e }()
	select {
	case <-result:
		t.Fatal("poll passed a held owner lock")
	case <-time.After(800 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	p := <-result
	require.NoError(t, <-failure)
	require.Equal(t, "expired", p.Status)
	require.Nil(t, p.Payload)
	require.NotContains(t, marshal(t, p.Wire()), "encrypted_payload")
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, approveBody(t, o))
	require.ErrorIs(t, err, ErrConflict)
}
func TestDatabaseExpiryListCreateAndDependencyErrors(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, r.RequestID)
	require.NoError(t, err)
	listed, err := List(dbtest.WithRememberedActor(ctx, id), db, id)
	require.NoError(t, err)
	require.Empty(t, listed)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM recovery_requests WHERE id=$1`, r.RequestID).Scan(&status))
	require.Equal(t, "expired", status)
	b := createBody(t)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.Error(t, err)
	b.AccountBinding = AccountBinding(id)
	_, err = Create(ctx, db, id, "public-jti", time.Time{}, b, allowRecovery)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(-time.Hour), b, allowRecovery)
	require.ErrorIs(t, err, ErrUnauthorized)
	short := time.Now().Add(time.Minute).Truncate(time.Second)
	r, err = Create(ctx, db, id, "public-jti", short, b, allowRecovery)
	require.NoError(t, err)
	require.Equal(t, short.UnixMilli(), r.ExpiresAt)
	_, err = db.Exec(`DELETE FROM trusted_recovery_devices WHERE user_id=$1`, id)
	require.NoError(t, err)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(time.Hour), b, allowRecovery)
	var api *APIError
	require.ErrorAs(t, err, &api)
	require.Equal(t, 400, api.Status)
	closed, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	_, err = Poll(ctx, closed, id, "jti", r.RequestID, allowRecovery)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrNotFound))
}

func TestDatabaseRequestLockExpiryPreventsOfferAndComplete(t *testing.T) {
	for _, action := range []string{"offer", "complete"} {
		t.Run(action, func(t *testing.T) {
			db, id := setupDB(t)
			ctx := context.Background()
			r := newRequest(t, db, id)
			_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '1 minute',expires_at=clock_timestamp()+interval '600 milliseconds' WHERE id=$1`, r.RequestID)
			require.NoError(t, err)
			r, err = Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
			require.NoError(t, err)
			o := offerBody(t, r)
			if action == "complete" {
				_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
				require.NoError(t, err)
				_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, approveBody(t, o))
				require.NoError(t, err)
			}
			tx, err := db.Begin()
			require.NoError(t, err)
			var requestID string
			require.NoError(t, tx.QueryRow(`SELECT id FROM recovery_requests WHERE id=$1 FOR UPDATE`, r.RequestID).Scan(&requestID))
			done := make(chan error, 1)
			go func() {
				if action == "offer" {
					_, e := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, o)
					done <- e
				} else {
					_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash}, allowRecovery)
					done <- e
				}
			}()
			select {
			case <-done:
				t.Fatal("write passed held request lock")
			case <-time.After(700 * time.Millisecond):
			}
			require.NoError(t, tx.Commit())
			require.ErrorIs(t, <-done, ErrConflict)
			p, err := Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
			require.NoError(t, err)
			require.Equal(t, "expired", p.Status)
			require.Nil(t, p.Payload)
		})
	}
}

func TestDatabaseRejectedWireAndUnsupportedVersions(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	_, err := Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 2})
	require.NoError(t, err)
	p, err := Poll(ctx, db, id, "public-jti", r.RequestID, allowRecovery)
	require.NoError(t, err)
	require.Equal(t, "rejected", p.Status)
	require.NotContains(t, marshal(t, p.Wire()), "transcript_hash")
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 2})
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(dbtest.WithRememberedActor(ctx, id), db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 1})
	require.Error(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{1, fixture(t).Hash}, allowRecovery)
	require.Error(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{2, "AA=="}, allowRecovery)
	require.Error(t, err)
	b := createBody(t)
	b.AccountBinding = AccountBinding(id)
	b.ProtocolVersion = 1
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.Error(t, err)
	b.ProtocolVersion = 2
	b.ServerOrigin = "https://example.test/"
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.Error(t, err)
	b.ServerOrigin = fixture(t).Context.ServerOrigin
	b.RequesterNonce = "AA=="
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.Error(t, err)
	b.RequesterNonce = fixture(t).Context.RequesterNonce
	b.RequesterPublicKey = "AA=="
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b, allowRecovery)
	require.Error(t, err)
}

func allowRecovery(context.Context, *sql.Tx) error { return nil }

func TestDatabaseRecoveryGuardIsRequiredBeforeProtectedAccess(t *testing.T) {
	db, id := setupDB(t)
	row := newRequest(t, db, id)
	body := createBody(t)
	body.AccountBinding = AccountBinding(id)
	ctx := context.Background()
	_, err := Create(ctx, db, id, "public-jti", time.Now().Add(time.Hour), body, nil)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = Poll(ctx, db, id, "public-jti", row.RequestID, nil)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = Complete(ctx, db, id, "public-jti", row.RequestID, CompleteBody{Version, fixture(t).Hash}, nil)
	require.ErrorIs(t, err, ErrUnauthorized)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM recovery_requests WHERE user_id=$1`, id).Scan(&count))
	require.Equal(t, 1, count)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM recovery_requests WHERE id=$1`, row.RequestID).Scan(&status))
	require.Equal(t, "pending", status)
}

func TestRespondRequiresTrustedDevice(t *testing.T) {
	db, id := setupDB(t)
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	untrusted := untrustedActor(t, db, id)
	before := requestSnapshot(t, db, r.RequestID)
	for _, body := range []RespondBody{o, approveBody(t, o), {Action: "reject", ProtocolVersion: Version}} {
		_, err := Respond(untrusted, db, id, "", r.RequestID, body)
		require.ErrorIs(t, err, ErrForbidden, body.Action)
	}
	_, err := Respond(untrusted, db, id, "", uuid.NewString(), o)
	require.ErrorIs(t, err, ErrForbidden, "an untrusted session cannot probe for request ids")
	_, err = Respond(context.Background(), db, id, "", r.RequestID, o)
	require.Error(t, err, "a missing identity never reaches the request")
	require.Equal(t, before, requestSnapshot(t, db, r.RequestID))
}

func TestListHidesRequestsFromUntrustedSession(t *testing.T) {
	db, id := setupDB(t)
	r := newRequest(t, db, id)
	_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, r.RequestID)
	require.NoError(t, err)
	before := requestSnapshot(t, db, r.RequestID)
	for _, ctx := range []context.Context{untrustedActor(t, db, id), context.Background()} {
		listed, err := List(ctx, db, id)
		require.NoError(t, err)
		require.NotNil(t, listed)
		require.Empty(t, listed)
	}
	require.Equal(t, before, requestSnapshot(t, db, r.RequestID), "an untrusted list must not transition the request")
}

func TestTrustedResponderSurvivesRefreshRotation(t *testing.T) {
	db, id := setupDB(t)
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	predecessor := untrustedActor(t, db, id)
	predecessorID, _ := loginfamily.AccessIdentityFromContext(predecessor)
	machineID := trustSession(t, db, id, predecessorID.SID)
	_, err := db.Exec(`UPDATE refresh_tokens SET revoked_at=now() WHERE id=$1`, predecessorID.SID)
	require.NoError(t, err)
	successor := untrustedActor(t, db, id)
	successorID, _ := loginfamily.AccessIdentityFromContext(successor)
	_, err = db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (id) DO UPDATE SET machine_id = EXCLUDED.machine_id`, successorID.SID, id, uuid.NewString(), time.Now().Add(time.Hour), machineID)
	require.NoError(t, err)
	_, err = Respond(predecessor, db, id, "", r.RequestID, o)
	require.NoError(t, err, "an access token minted before the rotation names the revoked predecessor row")
	_, err = Respond(successor, db, id, "", r.RequestID, approveBody(t, o))
	require.NoError(t, err)

	other := untrustedActor(t, db, id)
	otherID, _ := loginfamily.AccessIdentityFromContext(other)
	_, err = db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,$5)`,
		otherID.SID, id, uuid.NewString(), time.Now().Add(time.Hour), uuid.NewString())
	require.NoError(t, err)
	nullMachine := untrustedActor(t, db, id)
	nullID, _ := loginfamily.AccessIdentityFromContext(nullMachine)
	_, err = db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,NULL)`,
		nullID.SID, id, uuid.NewString(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	r2 := newRequest(t, db, id)
	for name, ctx := range map[string]context.Context{"different machine": other, "null machine": nullMachine} {
		_, err = Respond(ctx, db, id, "", r2.RequestID, offerBody(t, r2))
		require.ErrorIs(t, err, ErrForbidden, name)
	}
}

func TestRemovedTrustedDeviceCannotRespond(t *testing.T) {
	db, id := setupDB(t)
	r := newRequest(t, db, id)
	actor := untrustedActor(t, db, id)
	identity, _ := loginfamily.AccessIdentityFromContext(actor)
	machineID := trustSession(t, db, id, identity.SID)
	_, err := db.Exec(`DELETE FROM trusted_recovery_devices WHERE user_id=$1 AND machine_id=$2`, id, machineID)
	require.NoError(t, err)
	_, err = Respond(actor, db, id, "", r.RequestID, offerBody(t, r))
	require.ErrorIs(t, err, ErrForbidden)
	listed, err := List(actor, db, id)
	require.NoError(t, err)
	require.Empty(t, listed)
}

func TestTrustedDeviceOfAnotherAccountCannotRespond(t *testing.T) {
	db, id := setupDB(t)
	r := newRequest(t, db, id)
	other := dbtest.CreateUser(t, db).String()
	dbtest.RememberActor(t, db, other)
	machineID := trustSession(t, db, other, rememberedSID(t, other))
	_, err := Respond(dbtest.WithRememberedActor(context.Background(), other), db, other, "", r.RequestID, offerBody(t, r))
	require.ErrorIs(t, err, ErrNotFound, "B's own trusted device still cannot see A's request")

	// A's undesignated session on the machine B designated is not trusted for A.
	actor := untrustedActor(t, db, id)
	identity, _ := loginfamily.AccessIdentityFromContext(actor)
	_, err = db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,$4,$5)`,
		identity.SID, id, uuid.NewString(), time.Now().Add(time.Hour), machineID)
	require.NoError(t, err)
	_, err = Respond(actor, db, id, "", r.RequestID, offerBody(t, r))
	require.ErrorIs(t, err, ErrForbidden)
}
