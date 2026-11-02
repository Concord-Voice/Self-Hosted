package websocket

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func contentDispatchFixture(t *testing.T, enabled, dm, subscribed bool) (*Client, *c2Engine, *finalizationObserver, uuid.UUID) {
	t.Helper()
	client, engine, _, observer := c2Client(t, enabled)
	target := uuid.New()
	client.SessionID = uuid.NewString()
	client.FamilyID = uuid.New()
	client.Hub.clients[client.ID] = client
	client.Hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	if subscribed {
		if dm {
			client.Hub.dmSubscriptions[target] = map[uuid.UUID]bool{client.ID: true}
		} else {
			client.Channels[target] = true
			client.Hub.channelSubscriptions[target] = map[uuid.UUID]bool{client.ID: true}
		}
	}
	return client, engine, observer, target
}
func contentDispatchAndApply(t *testing.T, client *Client, dm bool, target string) {
	t.Helper()
	name, key := "message", "channel_id"
	if dm {
		name, key = "dm_message", "conversation_id"
	}
	require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{%q:%q}}`, name, key, target))))
	require.Len(t, client.Hub.incoming, 1)
	frame := <-client.Hub.incoming
	// The read owner's child is already canceled. Actual Hub processing must
	// use stamped expiry and the socket lifetime, not that completed child.
	client.Hub.handleIncoming(frame)
}
func TestEventAdmissionContentOriginalGateCountsAllowedBeforeFieldValidation(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm%t", dm), func(t *testing.T) {
			client, engine, observer, target := contentDispatchFixture(t, true, dm, true)
			contentDispatchAndApply(t, client, dm, target.String())
			assert.Empty(t, engine.snapshot())
			assert.Equal(t, []finalizationRecord{{EventClassContent, true, false}}, observer.snapshot())
			require.Len(t, client.Send, 1)
			var frame struct {
				Type string `json:"type"`
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(<-client.Send, &frame))
			assert.Equal(t, "error", frame.Type)
			assert.Contains(t, frame.Data.Message, "content")
		})
	}
}
func TestEventAdmissionContentInvalidIDOrMissingSubscriptionNeverClaimsGate(t *testing.T) {
	for _, dm := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("dm%t_invalidID%t", dm, invalid), func(t *testing.T) {
				client, engine, observer, target := contentDispatchFixture(t, true, dm, false)
				id := target.String()
				if invalid {
					id = "not-a-uuid"
				}
				contentDispatchAndApply(t, client, dm, id)
				assert.Empty(t, engine.snapshot())
				assert.Empty(t, observer.snapshot())
				assert.Zero(t, client.rateTokens)
				assert.True(t, client.rateLastFill.IsZero())
			})
		}
	}
}
func TestEventAdmissionContentChannelAndDMShareOriginalDepletedBudget(t *testing.T) {
	client, engine, observer, channel := contentDispatchFixture(t, true, false, true)
	conversation := uuid.New()
	client.Hub.dmSubscriptions[conversation] = map[uuid.UUID]bool{client.ID: true}
	// A stable exhausted fixture avoids real-time refill during this assertion;
	// production still owns its unchanged 5/s, burst10 bucket.
	client.rateTokens = 0
	client.rateLastFill = time.Now().Add(time.Hour)
	contentDispatchAndApply(t, client, false, channel.String())
	contentDispatchAndApply(t, client, true, conversation.String())
	assert.Empty(t, engine.snapshot())
	assert.Equal(t, []finalizationRecord{{EventClassContent, false, false}, {EventClassContent, false, false}}, observer.snapshot())
	require.Len(t, client.Send, 2)
	for range 2 {
		var frame struct {
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(<-client.Send, &frame))
		assert.Contains(t, frame.Data.Message, "Rate limit exceeded")
	}
}
func TestEventAdmissionContentAbandonedStampedAuthorityEmitsNoGateCountOrRateAdvice(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm%t", dm), func(t *testing.T) {
			client, _, observer, target := contentDispatchFixture(t, true, dm, true)
			name, key := "message", "channel_id"
			if dm {
				name, key = "dm_message", "conversation_id"
			}
			require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{%q:%q}}`, name, key, target))))
			frame := <-client.Hub.incoming
			frame.TokenExpiresAt = time.Now().Add(-time.Second)
			client.Hub.handleIncoming(frame)
			assert.Empty(t, observer.snapshot())
			assert.Empty(t, client.Send)
			assert.True(t, client.rateLastFill.IsZero())
		})
	}
}
func TestEventAdmissionContentDisabledRetainsOriginalRefusalWithoutNewMetrics(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm%t", dm), func(t *testing.T) {
			client, engine, observer, target := contentDispatchFixture(t, false, dm, true)
			client.rateTokens = 0
			client.rateLastFill = time.Now().Add(time.Hour)
			contentDispatchAndApply(t, client, dm, target.String())
			assert.Empty(t, engine.snapshot())
			assert.Empty(t, observer.snapshot())
			require.Len(t, client.Send, 1)
		})
	}
}

func TestEventAdmissionContentAllowedMetricSurvivesActualEnvelopeRejection(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm%t", dm), func(t *testing.T) {
			client, engine, observer, target := contentDispatchFixture(t, true, dm, true)
			server, peer := renewalSocketPair(t)
			client.Conn = server
			name, key := "message", "channel_id"
			if dm {
				name, key = "dm_message", "conversation_id"
			}
			// Nonempty fixture content with the required key version deliberately absent
			// reaches the original gate and the real envelope rejection, before DB work.
			require.True(t, client.dispatchFrame([]byte(fmt.Sprintf(`{"type":%q,"data":{%q:%q,"content":"ciphertext-fixture"}}`, name, key, target.String()))))
			frame := <-client.Hub.incoming
			assert.Empty(t, observer.snapshot())
			client.Hub.handleIncoming(frame)
			assert.Empty(t, engine.snapshot())
			assert.Equal(t, []finalizationRecord{{EventClassContent, true, false}}, observer.snapshot())
			require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
			_, _, err := peer.ReadMessage()
			var closed *ws.CloseError
			require.ErrorAs(t, err, &closed)
			assert.Equal(t, 4400, closed.Code)
		})
	}
}
