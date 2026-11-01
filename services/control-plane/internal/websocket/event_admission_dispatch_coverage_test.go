package websocket

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventAdmissionDispatchAllSixteenNamesHaveTheirActualOwner(t *testing.T) {
	for _, tt := range []struct {
		name  string
		class EventClass
		scope BudgetScope
		hold  bool
	}{
		{"message", EventClassContent, BudgetScopeExistingContent, false}, {"dm_message", EventClassContent, BudgetScopeExistingContent, false},
		{"typing", EventClassTyping, BudgetScopeAccount, false}, {"dm_typing", EventClassTyping, BudgetScopeAccount, false},
		{"subscribe", EventClassSubscription, BudgetScopeAccount, true}, {"unsubscribe", EventClassSubscription, BudgetScopeAccount, true},
		{"subscribe_server", EventClassSubscription, BudgetScopeAccount, true}, {"unsubscribe_server", EventClassSubscription, BudgetScopeAccount, true},
		{"subscribe_dm", EventClassSubscription, BudgetScopeAccount, true}, {"unsubscribe_dm", EventClassSubscription, BudgetScopeAccount, true},
		{"heartbeat", EventClassPresence, BudgetScopeLocalMaintenance, false}, {"set_status", EventClassPresence, BudgetScopeAccount, false},
		{"profile_update", EventClassProfile, BudgetScopeAccount, false}, {"server_update", EventClassServer, BudgetScopeAccount, false},
		{"connection_ready_probe", EventClassReady, BudgetScopeAccount, true}, {"auth_renew", EventClassRenewal, BudgetScopeAccount, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, engine, _, observer := c2Client(t, true)
			client.FamilyID = uuid.New()
			client.SessionID = uuid.NewString()
			client.CredEpoch = "server-owned-epoch"
			originalDeadline := client.tokenDeadline()
			payload := fmt.Sprintf(`{"type":%q,"user_id":%q,"client_id":%q,"cred_epoch":"forged","data":{}}`, tt.name, uuid.NewString(), uuid.NewString())
			require.True(t, client.dispatchFrame([]byte(payload)))
			calls := engine.snapshot()
			if tt.scope == BudgetScopeAccount {
				require.Len(t, calls, 1)
				assert.Equal(t, client.UserID, calls[0].user)
				assert.Equal(t, EventPolicy{Class: tt.class, Scope: tt.scope, MayHold: tt.hold}, calls[0].policy)
			} else {
				assert.Empty(t, calls)
			}
			if tt.name == "auth_renew" {
				assert.Empty(t, client.Hub.incoming)
				require.Len(t, client.Send, 1)
				assert.JSONEq(t, string(authRenewRejectedFrame), string(<-client.Send))
			} else {
				require.Len(t, client.Hub.incoming, 1)
				frame := <-client.Hub.incoming
				assert.Equal(t, tt.name, frame.Type)
				assert.Equal(t, client.UserID, frame.UserID)
				assert.Equal(t, client.ID, frame.ClientID)
				assert.Equal(t, client.CredEpoch, frame.CredEpoch)
				assert.Equal(t, uuid.MustParse(client.SessionID), frame.SID)
				assert.Equal(t, originalDeadline, frame.TokenExpiresAt)
			}
			if tt.scope == BudgetScopeExistingContent {
				assert.Empty(t, observer.snapshot())
			} else {
				assert.Equal(t, []finalizationRecord{{tt.class, true, false}}, observer.snapshot())
			}
		})
	}
}
func TestEventAdmissionDispatchSilentDenialsDoNotReachHubIngress(t *testing.T) {
	t.Run("typing", func(t *testing.T) {
		client, engine, _, observer := c2Client(t, true)
		engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(time.Millisecond) }
		for _, name := range []string{"typing", "dm_typing"} {
			require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{}}`, name))))
		}
		assert.Len(t, engine.snapshot(), 2)
		assert.Empty(t, client.Hub.incoming)
		assert.Empty(t, client.Send)
		assert.Equal(t, []finalizationRecord{{EventClassTyping, false, false}, {EventClassTyping, false, false}}, observer.snapshot())
	})
	t.Run("heartbeat", func(t *testing.T) {
		client, engine, _, observer := c2Client(t, true)
		for range 2 {
			require.True(t, client.dispatchFrame([]byte(`{"type":"heartbeat","data":{}}`)))
			require.Len(t, client.Hub.incoming, 1)
			<-client.Hub.incoming
		}
		require.True(t, client.dispatchFrame([]byte(`{"type":"heartbeat","data":{}}`)))
		assert.Empty(t, client.Hub.incoming)
		assert.Empty(t, client.Send)
		assert.Empty(t, engine.snapshot())
		assert.Equal(t, []finalizationRecord{{EventClassPresence, true, false}, {EventClassPresence, true, false}, {EventClassPresence, false, false}}, observer.snapshot())
	})
}
func TestEventAdmissionDispatchMetadataAdviceIsSharedAcrossActualRoutes(t *testing.T) {
	client, engine, clock, observer := c2Client(t, true)
	engine.run = func(int, c2EngineCall) AdmissionDecision { return c2Deny(5 * time.Second) }
	for index, name := range []string{"profile_update", "server_update", "set_status"} {
		require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{}}`, name))))
		if index == 0 {
			require.Len(t, client.Send, 1)
			responseAssertAdvisory(t, <-client.Send, "profile", 5000)
		} else {
			assert.Empty(t, client.Send)
		}
	}
	assert.Empty(t, client.Hub.incoming)
	assert.Len(t, engine.snapshot(), 3)
	assert.Len(t, observer.snapshot(), 3)
	clock.Advance(time.Second)
	require.True(t, client.dispatchFrame([]byte(`{"type":"set_status","data":{}}`)))
	require.Len(t, client.Send, 1)
	responseAssertAdvisory(t, <-client.Send, "presence", 5000)
}
func TestEventAdmissionDispatchQueueCancellationDoesNotRepeatDebitOrRetractFinalAllow(t *testing.T) {
	for _, cause := range []string{"socket canceled", "event expiry"} {
		t.Run(cause, func(t *testing.T) {
			client, engine, _, observer := c2Client(t, true)
			if cause == "event expiry" {
				client.TokenExpiresAt = time.Now().Add(40 * time.Millisecond)
			}
			client.Hub.incoming = make(chan IncomingMessage, 1)
			client.Hub.incoming <- IncomingMessage{Type: "already queued"}
			finalized := make(chan struct{}, 1)
			observer.onRecord = func() { finalized <- struct{}{} }
			result := make(chan bool, 1)
			go func() { result <- client.dispatchFrame([]byte(`{"type":"profile_update","data":{}}`)) }()
			lifecycleAwait(t, finalized, "account allowance finalized before enqueue waiting")
			calls := engine.snapshot()
			require.Len(t, calls, 1)
			if cause == "socket canceled" {
				client.cancelIngressLifecycle()
			} else {
				lifecycleAwait(t, calls[0].ctx.Done(), "old event expiry while queue remains full")
			}
			select {
			case continued := <-result:
				assert.False(t, continued)
			case <-time.After(time.Second):
				t.Fatal("admitted queue wait did not stop")
			}
			assert.Len(t, engine.snapshot(), 1)
			assert.Equal(t, []finalizationRecord{{EventClassProfile, true, false}}, observer.snapshot())
			require.Len(t, client.Hub.incoming, 1)
			assert.Equal(t, "already queued", (<-client.Hub.incoming).Type)
			assert.Empty(t, client.Send)
		})
	}
}
func TestEventAdmissionDispatchDisabledRoutesKeepNewOwnersInactive(t *testing.T) {
	client, engine, clock, observer := c2Client(t, false)
	for _, name := range []string{"heartbeat", "heartbeat", "heartbeat", "typing", "profile_update", "connection_ready_probe", "unknown_fixture"} {
		require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{}}`, name))))
		require.Len(t, client.Hub.incoming, 1)
		assert.Equal(t, name, (<-client.Hub.incoming).Type)
	}
	assert.Empty(t, engine.snapshot())
	assert.Empty(t, observer.snapshot())
	assert.Empty(t, clock.requests)
	assert.Nil(t, client.eventHeartbeatBudget)
	assert.Empty(t, client.Send)
}
func dispatchDecodeReady(t *testing.T, raw []byte) (string, int, int) {
	t.Helper()
	var frame struct {
		Type string `json:"type"`
		Data struct {
			Channels int `json:"subscribed_channels"`
			Protocol int `json:"protocol_version"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &frame))
	return frame.Type, frame.Data.Channels, frame.Data.Protocol
}
