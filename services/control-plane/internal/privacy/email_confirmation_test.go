package privacy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/privacy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const (
	emailConfirmationTestUserID   = "00000000-0000-4000-8000-000000000001"
	emailConfirmationTestFamilyID = "00000000-0000-4000-8000-000000000002"
	emailConfirmationTestServerA  = "00000000-0000-4000-8000-000000000003"
	emailConfirmationTestServerB  = "00000000-0000-4000-8000-000000000004"
)

func emailConfirmationTestBinding() privacy.EmailConfirmationBinding {
	return privacy.EmailConfirmationBinding{
		Recipient:       "unverified@example.test",
		UserID:          emailConfirmationTestUserID,
		FamilyID:        emailConfirmationTestFamilyID,
		CredentialEpoch: "epoch-1",
		OwnedServerIDs:  []string{emailConfirmationTestServerA, emailConfirmationTestServerB},
	}
}

func TestEmailConfirmationIssueStoresOnlyDigestAndBoundRecord(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	store := privacy.NewEmailConfirmationStore(rdb)
	binding := emailConfirmationTestBinding()

	intent, err := store.Issue(t.Context(), binding)
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, intent.RequestID)
	assert.Len(t, intent.Token, 43)
	assert.NotContains(t, intent.Token, "=")
	assert.Equal(t, privacy.EmailConfirmationTTL, intent.ExpiresIn)

	fields, err := rdb.HGetAll(t.Context(), emailConfirmationKey(intent.RequestID)).Result()
	require.NoError(t, err)
	tokenDigest := sha256.Sum256([]byte(intent.Token))
	assert.Equal(t, hex.EncodeToString(tokenDigest[:]), fields["token_digest"])
	assert.NotContains(t, strings.Join(mapValues(fields), ":"), intent.Token)
	assert.NotContains(t, strings.Join(mapValues(fields), ":"), binding.Recipient)
	assert.Equal(t, "pending", fields["state"])
	assert.Equal(t, binding.UserID, fields["user_id"])
	assert.Equal(t, binding.FamilyID, fields["family_id"])
	assert.Equal(t, binding.CredentialEpoch, fields["credential_epoch"])
	assert.NotEmpty(t, fields["recipient_digest"])
	assert.NotEmpty(t, fields["owned_set_digest"])

	ttl, err := rdb.TTL(t.Context(), emailConfirmationKey(intent.RequestID)).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, privacy.EmailConfirmationTTL)
}

func TestEmailConfirmationClaimBindsRecipientAndAuthority(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	store := privacy.NewEmailConfirmationStore(rdb)
	binding := emailConfirmationTestBinding()
	intent, err := store.Issue(t.Context(), binding)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*privacy.EmailConfirmationBinding, *string)
	}{
		{"recipient", func(b *privacy.EmailConfirmationBinding, _ *string) { b.Recipient = "other@example.test" }},
		{"family", func(b *privacy.EmailConfirmationBinding, _ *string) {
			b.FamilyID = "00000000-0000-4000-8000-000000000005"
		}},
		{"epoch", func(b *privacy.EmailConfirmationBinding, _ *string) { b.CredentialEpoch = "epoch-2" }},
		{"owned server set", func(b *privacy.EmailConfirmationBinding, _ *string) {
			b.OwnedServerIDs = []string{emailConfirmationTestServerA}
		}},
		{"token", func(_ *privacy.EmailConfirmationBinding, token *string) { *token = strings.Repeat("A", 43) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := binding
			candidate.OwnedServerIDs = append([]string(nil), binding.OwnedServerIDs...)
			token := intent.Token
			tt.mutate(&candidate, &token)
			claim, err := store.Claim(t.Context(), intent.RequestID, token, candidate)
			assert.Nil(t, claim)
			require.ErrorIs(t, err, privacy.ErrEmailConfirmationStale)
		})
	}

	claim, err := store.Claim(t.Context(), intent.RequestID, intent.Token, binding)
	require.NoError(t, err)
	require.NotNil(t, claim)
	ttlBeforeRelease, err := rdb.TTL(t.Context(), emailConfirmationKey(intent.RequestID)).Result()
	require.NoError(t, err)

	replayed, err := store.Claim(t.Context(), intent.RequestID, intent.Token, binding)
	assert.Nil(t, replayed)
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationStale)

	require.NoError(t, claim.Release(t.Context()))
	ttlAfterRelease, err := rdb.TTL(t.Context(), emailConfirmationKey(intent.RequestID)).Result()
	require.NoError(t, err)
	assert.Positive(t, ttlAfterRelease)
	assert.LessOrEqual(t, ttlAfterRelease, ttlBeforeRelease, "release must not extend the original expiry")
	claim, err = store.Claim(t.Context(), intent.RequestID, intent.Token, binding)
	require.NoError(t, err)
	require.NoError(t, claim.Commit(t.Context()))

	replayed, err = store.Claim(t.Context(), intent.RequestID, intent.Token, binding)
	assert.Nil(t, replayed)
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationStale)
}

func TestEmailConfirmationClaimExpiredOrUnavailableFailsClosed(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	store := privacy.NewEmailConfirmationStore(rdb)
	binding := emailConfirmationTestBinding()
	intent, err := store.Issue(t.Context(), binding)
	require.NoError(t, err)

	require.NoError(t, rdb.Del(t.Context(), emailConfirmationKey(intent.RequestID)).Err())
	claim, err := store.Claim(context.Background(), intent.RequestID, intent.Token, binding)
	assert.Nil(t, claim)
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationStale)

	_, err = privacy.NewEmailConfirmationStore(nil).Issue(t.Context(), binding)
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationUnavailable)
}

func TestEmailConfirmationRejectsNonCanonicalOwnedSet(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	store := privacy.NewEmailConfirmationStore(rdb)
	binding := emailConfirmationTestBinding()
	binding.OwnedServerIDs = []string{emailConfirmationTestServerB, emailConfirmationTestServerA}

	_, err := store.Issue(t.Context(), binding)
	require.ErrorIs(t, err, privacy.ErrEmailConfirmationInvalid)
}

func emailConfirmationKey(requestID string) string { return "account_erase_email:" + requestID }

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
