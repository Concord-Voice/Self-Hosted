package websocket

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventAdmissionDispatchEnabledUnknownDropsButDisabledPreservesRouting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			client, engine, clock, observer := c2Client(t, enabled)
			require.True(t, client.dispatchFrame([]byte(`{"type":"unknown_fixture_event","data":{}}`)))
			if enabled {
				assert.Empty(t, client.Hub.incoming)
			} else {
				require.Len(t, client.Hub.incoming, 1)
				assert.Equal(t, "unknown_fixture_event", (<-client.Hub.incoming).Type)
			}
			assert.Empty(t, engine.snapshot())
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, clock.requests)
			assert.Nil(t, client.eventHeartbeatBudget)
			assert.Empty(t, client.Send)
		})
	}
}
func TestEventAdmissionDispatchMetadataDenialStopsBeforeHubAndProducesAdvisory(t *testing.T) {
	client, engine, _, observer := c2Client(t, true)
	engine.run = func(_ int, call c2EngineCall) AdmissionDecision {
		assert.Equal(t, admissionPolicy(EventClassProfile), call.policy)
		return c2Deny(5 * time.Second)
	}
	require.True(t, client.dispatchFrame([]byte(`{"type":"profile_update","data":{"display_name":"fixture"}}`)), "metadata refusal drops only this frame")
	require.Len(t, engine.snapshot(), 1)
	assert.Empty(t, client.Hub.incoming, "refused frame must not reach activity or handler work")
	require.Len(t, client.Send, 1)
	responseAssertAdvisory(t, <-client.Send, "profile", 5000)
	assert.Equal(t, []finalizationRecord{{EventClassProfile, false, false}}, observer.snapshot())
}
func TestTokenRenewalAdmissionEnabledEmptyTokenStillClaimsAccountOwner(t *testing.T) {
	client, engine, _, observer := c2Client(t, true)
	require.True(t, client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":""}}`)))
	calls := engine.snapshot()
	require.Len(t, calls, 1, "empty renewal must not bypass the account owner in enabled mode")
	assert.Equal(t, admissionPolicy(EventClassRenewal), calls[0].policy)
	assert.True(t, calls[0].hold.IsZero())
	assert.Empty(t, client.Hub.incoming, "renewal is intercepted before Hub.Run")
	require.Len(t, client.Send, 1)
	assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
	assert.Equal(t, []finalizationRecord{{EventClassRenewal, true, false}}, observer.snapshot(), "account allowance remains counted when token validation refuses")
}

// The source census must stay attached to behavior at dispatchFrame. Keeping
// helper comparisons while bypassing either delegated owner must go red here.
func TestEventAdmissionDispatchBothModesInterceptRenewalAndRouteOrdinaryFrames(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			client, engine, _, _ := c2Client(t, enabled)
			require.True(t, client.dispatchFrame([]byte(`{"type":"auth_renew","data":{"token":""}}`)))
			assert.Empty(t, client.Hub.incoming, "credentials must never be routed through Hub.Run")
			require.Len(t, client.Send, 1, "renewal must reach its actual interception owner")
			assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
			if enabled {
				require.Len(t, engine.snapshot(), 1)
				assert.Equal(t, admissionPolicy(EventClassRenewal), engine.snapshot()[0].policy)
			} else {
				assert.Empty(t, engine.snapshot(), "disabled mode does not call account admission")
			}
			// Live routing control rules out satisfying no-credential-replay by
			// silently dropping every frame in this dispatch mode.
			require.True(t, client.dispatchFrame([]byte(`{"type":"profile_update","data":{}}`)))
			require.Len(t, client.Hub.incoming, 1)
			assert.Equal(t, "profile_update", (<-client.Hub.incoming).Type)
			if enabled {
				require.Len(t, engine.snapshot(), 2)
				assert.Equal(t, admissionPolicy(EventClassProfile), engine.snapshot()[1].policy)
			} else {
				assert.Empty(t, engine.snapshot())
			}
		})
	}
}
