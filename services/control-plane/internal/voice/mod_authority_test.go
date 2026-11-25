package voice

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasAllPerms(t *testing.T) {
	viewJoin := rbac.PermViewVoiceChannels | rbac.PermJoinVoice
	cases := []struct {
		name     string
		have     rbac.Permission
		required rbac.Permission
		want     bool
	}{
		{"all bits present", viewJoin, viewJoin, true},
		{"one of two bits is not enough", rbac.PermViewVoiceChannels, viewJoin, false},
		{"administrator satisfies everything", rbac.PermAdministrator, viewJoin | rbac.PermSpeak, true},
		{"zero holds nothing", 0, rbac.PermViewVoiceChannels, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasAllPerms(tc.have, tc.required))
		})
	}
}

func TestResolveActorVoiceChannel_ChannelNotInServerBeforeNotMember(t *testing.T) {
	t.Run("wrapped channel-not-in-server means absent", func(t *testing.T) {
		present, err := classifyVoiceChannelResolveErr(fmt.Errorf("x: %w", rbac.ErrChannelNotInServer))
		require.NoError(t, err)
		assert.False(t, present)
	})
	t.Run("not-member maps to the actor sentinel", func(t *testing.T) {
		_, err := classifyVoiceChannelResolveErr(rbac.ErrNotMember)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errVoiceModActorNotMember))
	})
	t.Run("both sentinels wrapped: channel-not-in-server wins", func(t *testing.T) {
		present, err := classifyVoiceChannelResolveErr(
			fmt.Errorf("%w: %w", rbac.ErrNotMember, rbac.ErrChannelNotInServer))
		require.NoError(t, err)
		assert.False(t, present)
	})
	t.Run("unrelated error propagates and is not a sentinel", func(t *testing.T) {
		boom := errors.New("db down")
		_, err := classifyVoiceChannelResolveErr(boom)
		require.Error(t, err)
		assert.True(t, errors.Is(err, boom))
		assert.False(t, errors.Is(err, errVoiceModActorNotMember))
		assert.False(t, errors.Is(err, errVoiceModPermission))
	})
}
