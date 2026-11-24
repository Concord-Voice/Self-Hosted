package api

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type recordingVoiceEjector struct {
	calls [][3]string
	err   error
}

func (r *recordingVoiceEjector) DeliverCredentialEpochVoiceEjection(_ context.Context, userID, credentialEpoch, supersededCredentialEpoch string) error {
	r.calls = append(r.calls, [3]string{userID, credentialEpoch, supersededCredentialEpoch})
	return r.err
}

func TestCredentialEpochVoiceEvictor_ForwardsArgumentsInOrder(t *testing.T) {
	var logs bytes.Buffer
	ejector := &recordingVoiceEjector{}
	evict := credentialEpochVoiceEvictor(ejector, logger.NewWithWriter(&logs))

	evict(context.Background(), "user-1", "epoch-new", "epoch-old")

	require.Len(t, ejector.calls, 1)
	assert.Equal(t, [3]string{"user-1", "epoch-new", "epoch-old"}, ejector.calls[0])
	assert.NotContains(t, logs.String(), "Credential media eviction retained")
}

func TestCredentialEpochVoiceEvictor_LogsFixedClassOnFailure(t *testing.T) {
	var logs bytes.Buffer
	ejector := &recordingVoiceEjector{err: errors.New("bus down for user-1")}
	evict := credentialEpochVoiceEvictor(ejector, logger.NewWithWriter(&logs))

	evict(context.Background(), "user-1", "epoch-new", "epoch-old")

	require.Len(t, ejector.calls, 1)
	assert.Contains(t, logs.String(), "Credential media eviction retained")
	assert.Contains(t, logs.String(), "delivery")
	assert.NotContains(t, logs.String(), "bus down", "the raw delivery error is never logged")
}
