package mfa

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// A verified MFA challenge proves one account, not its source, so it clears the
// account's own verify-attempt counter but must leave the source failure
// counter that every client in the same /32 or /64 shares. Otherwise anyone
// with an MFA account in the bucket could fail up to the threshold, sign in,
// and repeat without reaching the ban.
func TestVerifySuccessKeepsTheSharedSourceFailureCount(t *testing.T) {
	fixture := newRealTOTPFixture(t)
	h, redisClient := scriptedHandlerFull(t, fixture.state(), "test", fixture.keyring, nil, nil)
	completer := &recordingLoginCompleter{}
	h.SetLoginCompleter(completer)

	ctx := context.Background()
	const sourceKey = "auth_failures:ip:192.0.2.1" // httptest.NewRequest's RemoteAddr
	accountKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, enrollUser)
	require.NoError(t, redisClient.Set(ctx, sourceKey, "9", 15*time.Minute).Err())
	require.NoError(t, redisClient.Set(ctx, accountKey, "2", 15*time.Minute).Err())

	token, _ := loginChallenge(t, h)
	response := verifyWith(h, `{"mfa_challenge_token":"`+token+`","method":"totp","code":"`+fixture.code+`"}`)

	require.True(t, completer.called, "the verified challenge must complete the sign-in: %d %s", response.Code, response.Body.String())
	_, err := redisClient.Get(ctx, accountKey).Result()
	require.ErrorIs(t, err, redis.Nil, "the verified account's own attempt counter still clears")
	got, err := redisClient.Get(ctx, sourceKey).Result()
	require.NoError(t, err, "a verified challenge must not delete the shared source failure counter")
	require.Equal(t, "9", got)
	ttl, err := redisClient.TTL(ctx, sourceKey).Result()
	require.NoError(t, err)
	require.Positive(t, ttl, "the counter must keep decaying with its original window")
}
