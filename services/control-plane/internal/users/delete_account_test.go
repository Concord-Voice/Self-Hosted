package users_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/users"
	"github.com/Concord-Voice/Self-Hosted/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteAccount_HappyPath verifies the full erasure flow: the user
// row is deleted (cascading through FK-CASCADE tables), an audit row
// is inserted into account_deletions with user_id = NULL, and the
// transaction commits.
func TestDeleteAccount_HappyPath(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct1")

	svc := users.NewAccountService(ts.DB, logger.New("test"))

	err := svc.DeleteAccountForTest(testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID), user.ID)
	require.NoError(t, err)

	var userRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userRows))
	assert.Equal(t, 0, userRows, "user row must be deleted")

	var auditRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions WHERE user_id IS NULL AND deleted_at >= NOW() - INTERVAL '1 minute'`,
	).Scan(&auditRows))
	assert.GreaterOrEqual(t, auditRows, 1, "audit row must be inserted with NULL user_id")
}

func TestPrepareAccountErasure_ExhaustedMFABudgetDoesNotReservePassword(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erase-budget-before-password")
	service := users.NewAccountService(ts.DB, logger.New("test"))
	service.SetRedis(ts.Redis)
	ctx := context.Background()

	require.NoError(t, ts.Redis.Set(ctx, stepup.DangerousActionBudget(ts.Redis).Key(user.ID), stepup.BudgetLimit, stepup.BudgetWindow).Err())
	for i := 0; i < stepup.BudgetLimit; i++ {
		attempt, _, stepErr := service.PrepareAccountErasure(ctx, user.ID, user.Password, "rejected-code")
		require.Nil(t, attempt)
		require.NotNil(t, stepErr)
		require.Equal(t, http.StatusTooManyRequests, stepErr.Status)
	}

	require.False(t, ts.Redis.Exists(ctx, "login_attempts:"+user.Email).Val() > 0,
		"an MFA-budget denial must not reserve an unchecked password guess")
	login := ts.DoRequest(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": user.Password,
	}, nil)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
}

// TestDeleteAccount_RemovesPendingServerVoiceTerminalOutbox verifies that
// account erasure cannot leave a durable voice-terminal obligation that would
// later broadcast the erased user's identifier.
func TestDeleteAccount_RemovesPendingServerVoiceTerminalOutbox(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct-outbox")
	_, err := ts.DB.Exec(`
		INSERT INTO server_voice_terminal_outbox
			(channel_id, user_id, server_id, operation_id)
		VALUES ($1, $2, $3, $4)`, uuid.New(), user.ID, uuid.New(), uuid.New())
	require.NoError(t, err)

	var pendingBefore int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE user_id = $1`, user.ID,
	).Scan(&pendingBefore))
	require.Equal(t, 1, pendingBefore, "positive control: pending outbox obligation must be present")

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	actorCtx := testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID)
	require.NoError(t, svc.DeleteAccountForTest(actorCtx, user.ID))

	var pendingAfter int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE user_id = $1`, user.ID,
	).Scan(&pendingAfter))
	assert.Zero(t, pendingAfter,
		"account erasure must delete pending server voice terminal obligations before a later drain can broadcast the erased user ID")
}

// TestDeleteAccount_MissingUser verifies the sentinel-error contract:
// targeting a non-existent UUID returns ErrUserNotFound and writes no
// audit row.
func TestDeleteAccount_MissingUser(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	svc := users.NewAccountService(ts.DB, logger.New("test"))

	const ghostUUID = "00000000-0000-0000-0000-000000000001"

	var auditBefore int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&auditBefore))

	// A missing account still reaches the transaction only as its own actor.
	ghostCtx := loginfamily.WithAccessIdentity(context.Background(), loginfamily.AccessIdentity{UserID: uuid.MustParse(ghostUUID)})
	err := svc.DeleteAccountForTest(ghostCtx, ghostUUID)
	require.ErrorIs(t, err, users.ErrUserNotFound)

	var auditAfter int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM account_deletions`).Scan(&auditAfter))
	assert.Equal(t, auditBefore, auditAfter,
		"no audit row must be written when the user did not exist")
}

// TestDeleteAccount_IdempotentRetry verifies that calling DeleteAccount
// twice for the same user yields ErrUserNotFound on the second call and
// produces exactly one audit row total.
func TestDeleteAccount_IdempotentRetry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct2")

	svc := users.NewAccountService(ts.DB, logger.New("test"))

	actorCtx := testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID)
	require.NoError(t, svc.DeleteAccountForTest(actorCtx, user.ID))

	// The same actor retries: the user row is gone, so ErrUserNotFound precedes the guard.
	err := svc.DeleteAccountForTest(actorCtx, user.ID)
	require.ErrorIs(t, err, users.ErrUserNotFound)

	var auditCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM account_deletions WHERE deleted_at >= NOW() - INTERVAL '1 minute'`,
	).Scan(&auditCount))
	assert.Equal(t, 1, auditCount, "exactly one audit row must exist after success+retry")
}

// TestDeleteAccount_AuditRowInsertWithoutSentryColumn pins the
// post-migration-000060 schema: the INSERT in DeleteAccount must NOT
// reference the dropped sentry_delete_attempted column. If a future
// edit accidentally re-adds the column reference, this test catches it
// (the INSERT would fail with column-not-found error).
func TestDeleteAccount_AuditRowInsertWithoutSentryColumn(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct3")

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	err := svc.DeleteAccountForTest(testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID), user.ID)
	require.NoError(t, err, "INSERT must succeed without sentry_delete_attempted column")
}

func TestDeleteAccount_BroadcastsIncompleteChannelDeletion(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erasechannelowner")
	creator := ts.CreateTestUser(t, "erasechannelcreator")
	serverID := ts.CreateTestServer(t, owner.ID, "Erase Channel Server")
	channelID := ts.CreateTestChannel(t, serverID, "incomplete-channel")
	ts.AddMemberToServer(t, serverID, creator.ID, "member")
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, creator.ID,
	)
	require.NoError(t, err)

	var broadcasts [][2]string
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetChannelDeletedBroadcaster(func(serverID, channelID string) {
		broadcasts = append(broadcasts, [2]string{serverID, channelID})
	})
	require.NoError(t, svc.DeleteAccountForTest(testhelpers.ActorContextForDB(context.Background(), t, ts.DB, creator.ID), creator.ID))

	assert.Equal(t, [][2]string{{serverID, channelID}}, broadcasts)
	var channelRows int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID).Scan(&channelRows))
	assert.Zero(t, channelRows)
}

func TestDeleteAccount_BroadcastsCascadedIncompleteLinkedChannelDeletion(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erasevoiceowner")
	creator := ts.CreateTestUser(t, "erasevoicecreator")
	serverID := ts.CreateTestServer(t, owner.ID, "Erase Voice Channel Server")
	voiceID := ts.CreateTestChannel(t, serverID, "incomplete-voice")
	linkedTextID := ts.CreateTestChannel(t, serverID, "completed-linked-text")
	ts.AddMemberToServer(t, serverID, creator.ID, "member")
	_, err := ts.DB.Exec(
		`UPDATE channels SET linked_voice_channel_id = $2 WHERE id = $1`, linkedTextID, voiceID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, voiceID, creator.ID,
	)
	require.NoError(t, err)

	var broadcasts [][2]string
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetChannelDeletedBroadcaster(func(serverID, channelID string) {
		broadcasts = append(broadcasts, [2]string{serverID, channelID})
	})
	require.NoError(t, svc.DeleteAccountForTest(testhelpers.ActorContextForDB(context.Background(), t, ts.DB, creator.ID), creator.ID))

	assert.ElementsMatch(t, [][2]string{{serverID, voiceID}, {serverID, linkedTextID}}, broadcasts)
	var channelRows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channels WHERE id IN ($1, $2)`, voiceID, linkedTextID,
	).Scan(&channelRows))
	assert.Zero(t, channelRows)
}

// TestDeleteAccount_CascadeDeletesUserRelatedData verifies that the
// ON DELETE CASCADE behavior correctly cleans up all user-associated
// data (servers, channels, messages, roles, etc.) when the user row
// is deleted. This ensures account erasure is complete.
func TestDeleteAccount_CascadeDeletesUserRelatedData(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "deleteacct4")

	// Create a server and channel owned by the user.
	serverID := ts.CreateTestServer(t, user.ID, "test_server")
	channelID := ts.CreateTestChannel(t, serverID, "test_channel")

	// Add a role and assign it to the user.
	roleID := ts.CreateTestRole(t, serverID, "test_role", 1, 1)
	ts.AssignRoleToUser(t, serverID, user.ID, roleID)

	svc := users.NewAccountService(ts.DB, logger.New("test"))
	actorCtx := testhelpers.ActorContextForDB(context.Background(), t, ts.DB, user.ID)
	require.NoError(t, svc.DeleteAccountForTest(actorCtx, user.ID))

	// Verify user is gone.
	var userExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID,
	).Scan(&userExists))
	assert.Equal(t, 0, userExists)

	// Verify server is cascaded (owner deletion cascades).
	var serverExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID,
	).Scan(&serverExists))
	assert.Equal(t, 0, serverExists, "owned server must be cascaded when user is deleted")

	// Verify channel is also cascaded transitively.
	var channelExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channels WHERE id = $1`, channelID,
	).Scan(&channelExists))
	assert.Equal(t, 0, channelExists, "channel must be cascaded when server is deleted")

	// Verify role is cascaded.
	var roleExists int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM roles WHERE id = $1`, roleID,
	).Scan(&roleExists))
	assert.Equal(t, 0, roleExists, "role must be cascaded when server is deleted")
}

// ── Fresh-proof erasure (production DeleteAccount) ─────────────────────────

// erasureService returns an account service wired as production wires it for
// a password-only step-up: Redis for the shared budget and password lockout.
func erasureService(ts *testhelpers.TestServer) *users.AccountService {
	service := users.NewAccountService(ts.DB, logger.New("test"))
	service.SetRedis(ts.Redis)
	return service
}

// erasureRequest prepares a request the way the privacy handler does: the
// actor's verified identity, then the pre-transaction budget charge and
// password reservation.
func erasureRequest(
	t *testing.T, ts *testhelpers.TestServer, service *users.AccountService, user testhelpers.TestUser,
	password, mfaCode string, confirmed ...uuid.UUID,
) (context.Context, users.AccountErasureRequest) {
	t.Helper()
	ctx := ts.ActorContext(context.Background(), t, user.ID)
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	require.True(t, ok)
	attempt, mfaCharge, stepErr := service.PrepareAccountErasure(ctx, user.ID, password, mfaCode)
	require.Nil(t, stepErr)
	return ctx, users.AccountErasureRequest{
		Actor: identity, Password: password, MFACode: mfaCode,
		PasswordAttempt: attempt, MFACharge: mfaCharge, ConfirmedOwnedServerIDs: confirmed,
	}
}

func requireUserExists(t *testing.T, ts *testhelpers.TestServer, userID string, want bool) {
	t.Helper()
	var exists bool
	require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists))
	require.Equal(t, want, exists)
}

func requireStepUpStatus(t *testing.T, err error, status int) *stepup.Error {
	t.Helper()
	var stepErr *stepup.Error
	require.True(t, errors.As(err, &stepErr), "want a step-up refusal, got %v", err)
	require.Equal(t, status, stepErr.Status, "%v", stepErr.Body)
	return stepErr
}

func TestDeleteAccount_FreshPasswordProofErases(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erase-fresh-proof")
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, user, user.Password, "")
	require.NoError(t, service.DeleteAccount(ctx, request))

	requireUserExists(t, ts, user.ID, false)
}

func TestDeleteAccount_RefusesMissingAndWrongPassword(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erase-wrong-proof")
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, user, "", "")
	stepErr := requireStepUpStatus(t, service.DeleteAccount(ctx, request), http.StatusForbidden)
	assert.Equal(t, "step_up_required", stepErr.Body["error_code"])
	requireUserExists(t, ts, user.ID, true)

	ctx, request = erasureRequest(t, ts, service, user, "not-the-password", "")
	_ = requireStepUpStatus(t, service.DeleteAccount(ctx, request), http.StatusForbidden)
	requireUserExists(t, ts, user.ID, true)
}

func TestDeleteAccount_RefusesAnotherActor(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erase-actor")
	other := ts.CreateTestUser(t, "erase-actor-other")
	service := erasureService(ts)

	_, request := erasureRequest(t, ts, service, user, user.Password, "")
	otherCtx := ts.ActorContext(context.Background(), t, other.ID)
	for name, ctx := range map[string]context.Context{"no identity": context.Background(), "other identity": otherCtx} {
		err := service.DeleteAccount(ctx, request)
		require.ErrorIs(t, err, credepoch.ErrEpochMismatch, name)
		require.ErrorIs(t, err, loginfamily.ErrDenied, name)
	}
	requireUserExists(t, ts, user.ID, true)
}

// An owned server with another member must be confirmed by ID; an owned
// server the actor is alone in needs no confirmation.
func TestDeleteAccount_OwnedServerWithMembersNeedsConfirmation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "erase-owner")
	member := ts.CreateTestUser(t, "erase-owner-member")
	shared := ts.CreateTestServer(t, owner.ID, "erase-shared")
	ts.AddMemberToServer(t, shared, member.ID, "member")
	ts.CreateTestServer(t, owner.ID, "erase-solo")
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, owner, owner.Password, "")
	err := service.DeleteAccount(ctx, request)
	var drift *users.ErrErasureOwnedServerSetDrifted
	require.True(t, errors.As(err, &drift), "want owned-server drift, got %v", err)
	assert.Equal(t, []string{shared}, drift.RequiredOwnedServerIDs)
	assert.Equal(t, "account erasure owned-server confirmation set changed under lock", drift.Error())
	requireUserExists(t, ts, owner.ID, true)

	ctx, request = erasureRequest(t, ts, service, owner, owner.Password, "", uuid.MustParse(shared))
	require.NoError(t, service.DeleteAccount(ctx, request))
	requireUserExists(t, ts, owner.ID, false)
}

func TestDeleteAccount_InlineMFAWithoutVerifierFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erase-mfa-unwired")
	_, err := ts.DB.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'Key', 'hardware', '\x00', 0, NOW())`, user.ID, []byte("erase-cred-"+user.ID))
	require.NoError(t, err)
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, user, user.Password, "123456")
	_ = requireStepUpStatus(t, service.DeleteAccount(ctx, request), http.StatusInternalServerError)
	requireUserExists(t, ts, user.ID, true)
}

func TestEmailVerified_ReadsTheAccountFlag(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	verified := ts.CreateTestUser(t, "erase-verified")
	unverified := ts.CreateTestUserUnverified(t, "erase-unverified")
	service := erasureService(ts)
	ctx := context.Background()

	got, err := service.EmailVerified(ctx, verified.ID)
	require.NoError(t, err)
	assert.True(t, got)
	got, err = service.EmailVerified(ctx, unverified.ID)
	require.NoError(t, err)
	assert.False(t, got)
	_, err = service.EmailVerified(ctx, uuid.NewString())
	require.Error(t, err, "an unknown account is an error, not an unverified address")
}

func TestEmailErasureConfirmationBinding(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	unverified := ts.CreateTestUserUnverified(t, "erase-binding")
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, unverified, unverified.Password, "")
	binding, err := service.EmailErasureConfirmationBinding(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, unverified.Email, binding.Recipient)
	assert.NotEmpty(t, binding.FamilyID)
	assert.Equal(t, request.Actor.CredentialEpoch, binding.CredentialEpoch)
	assert.Empty(t, binding.OwnedServerIDs)

	_, err = service.EmailErasureConfirmationBinding(context.Background(), request)
	require.ErrorIs(t, err, credepoch.ErrEpochMismatch)

	verified := ts.CreateTestUser(t, "erase-binding-verified")
	ctx, request = erasureRequest(t, ts, service, verified, verified.Password, "")
	_, err = service.EmailErasureConfirmationBinding(ctx, request)
	require.ErrorIs(t, err, users.ErrErasureEmailAlreadyVerified)
}

func TestPrepareEmailErasureConfirmation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	unverified := ts.CreateTestUserUnverified(t, "erase-prepare")
	service := erasureService(ts)

	ctx, request := erasureRequest(t, ts, service, unverified, unverified.Password, "")
	prepared, err := service.PrepareEmailErasureConfirmation(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, unverified.Email, prepared.Recipient)
	assert.NotEmpty(t, prepared.FamilyID)
	assert.Empty(t, prepared.OwnedServerIDs)
	requireUserExists(t, ts, unverified.ID, true)

	t.Run("refuses a wrong password", func(t *testing.T) {
		ctx, request := erasureRequest(t, ts, service, unverified, "not-the-password", "")
		_, err := service.PrepareEmailErasureConfirmation(ctx, request)
		_ = requireStepUpStatus(t, err, http.StatusForbidden)
	})
	t.Run("refuses another actor", func(t *testing.T) {
		_, err := service.PrepareEmailErasureConfirmation(context.Background(), request)
		require.ErrorIs(t, err, credepoch.ErrEpochMismatch)
	})
	t.Run("refuses an unconfirmed owned server with members", func(t *testing.T) {
		owner := ts.CreateTestUserUnverified(t, "erase-prepare-owner")
		member := ts.CreateTestUser(t, "erase-prepare-member")
		server := ts.CreateTestServer(t, owner.ID, "erase-prepare-shared")
		ts.AddMemberToServer(t, server, member.ID, "member")
		ctx, request := erasureRequest(t, ts, service, owner, owner.Password, "")
		_, err := service.PrepareEmailErasureConfirmation(ctx, request)
		var drift *users.ErrErasureOwnedServerSetDrifted
		require.True(t, errors.As(err, &drift), "want owned-server drift, got %v", err)
		assert.Equal(t, []string{server}, drift.RequiredOwnedServerIDs)

		ctx, request = erasureRequest(t, ts, service, owner, owner.Password, "", uuid.MustParse(server))
		prepared, err := service.PrepareEmailErasureConfirmation(ctx, request)
		require.NoError(t, err)
		assert.Equal(t, []string{server}, prepared.OwnedServerIDs)
	})
	t.Run("refuses a verified account", func(t *testing.T) {
		verified := ts.CreateTestUser(t, "erase-prepare-verified")
		ctx, request := erasureRequest(t, ts, service, verified, verified.Password, "")
		_, err := service.PrepareEmailErasureConfirmation(ctx, request)
		require.ErrorIs(t, err, users.ErrErasureEmailAlreadyVerified)
	})
}
