package sentry

import (
	"strings"
	"testing"

	sentrygo "github.com/getsentry/sentry-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubEvent_NilInput(t *testing.T) {
	assert.Nil(t, scrubEvent(nil, nil))
}

func TestScrubEvent_StripsUnknownContexts(t *testing.T) {
	event := &sentrygo.Event{
		Contexts: map[string]sentrygo.Context{
			"os":       {"name": "Linux"},
			"electron": {"version": "41"},
			"custom":   {"data": "leaked"},
		},
	}
	result := scrubEvent(event, nil)
	require.NotNil(t, result)
	assert.Contains(t, result.Contexts, "os")
	assert.NotContains(t, result.Contexts, "electron")
	assert.NotContains(t, result.Contexts, "custom")
}

func TestScrubEvent_PreservesAllAllowedContexts(t *testing.T) {
	event := &sentrygo.Event{
		Contexts: map[string]sentrygo.Context{
			"os": {}, "runtime": {}, "app": {},
		},
	}
	result := scrubEvent(event, nil)
	assert.Len(t, result.Contexts, 3)
}

func TestScrubEvent_StripsUnknownTags(t *testing.T) {
	event := &sentrygo.Event{
		Tags: map[string]string{
			"environment": "production",
			"release":     "v1",
			"customTag":   "leaked",
		},
	}
	result := scrubEvent(event, nil)
	assert.Contains(t, result.Tags, "environment")
	assert.Contains(t, result.Tags, "release")
	assert.NotContains(t, result.Tags, "customTag")
}

func TestScrubEvent_PreservesAllAllowedTagKeys(t *testing.T) {
	tags := make(map[string]string)
	for key := range AllowedTagKeys {
		tags[key] = "test"
	}
	event := &sentrygo.Event{Tags: tags}
	result := scrubEvent(event, nil)
	assert.Len(t, result.Tags, len(AllowedTagKeys))
}

func TestScrubEvent_StripsRoomID(t *testing.T) {
	event := &sentrygo.Event{Tags: map[string]string{"environment": "prod", "room.id": "abc"}}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Tags, "room.id")
}

func TestScrubEvent_StripsUserID(t *testing.T) {
	event := &sentrygo.Event{Tags: map[string]string{"environment": "prod", "user.id": "abc"}}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Tags, "user.id")
}

func TestScrubEvent_StripsChannelID(t *testing.T) {
	event := &sentrygo.Event{Tags: map[string]string{"environment": "prod", "channel.id": "abc"}}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Tags, "channel.id")
}

func TestScrubEvent_StripsServerID(t *testing.T) {
	event := &sentrygo.Event{Tags: map[string]string{"environment": "prod", "server.id": "abc"}}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Tags, "server.id")
}

func TestScrubEvent_StripsExtra(t *testing.T) {
	event := &sentrygo.Event{Extra: map[string]interface{}{"secret": "leaked"}}
	result := scrubEvent(event, nil)
	assert.Nil(t, result.Extra)
}

func TestScrubEvent_StripsRequest(t *testing.T) {
	event := &sentrygo.Event{Request: &sentrygo.Request{URL: "/api/v1/servers"}}
	result := scrubEvent(event, nil)
	assert.Nil(t, result.Request)
}

func TestScrubEvent_StripsBreadcrumbs(t *testing.T) {
	event := &sentrygo.Event{
		Breadcrumbs: []*sentrygo.Breadcrumb{{Category: "http", Message: "GET /health"}},
	}
	result := scrubEvent(event, nil)
	assert.Nil(t, result.Breadcrumbs)
}

func TestScrubEvent_ReducesUserToID(t *testing.T) {
	event := &sentrygo.Event{
		User: sentrygo.User{ID: "anon-123", Email: "user@example.com", IPAddress: "1.2.3.4"},
	}
	result := scrubEvent(event, nil)
	assert.Equal(t, "anon-123", result.User.ID)
	assert.Empty(t, result.User.Email)
	assert.Empty(t, result.User.IPAddress)
}

func TestScrubEvent_RedactsSensitiveUserID(t *testing.T) {
	event := &sentrygo.Event{User: sentrygo.User{ID: "alice@example.com"}}
	result := scrubEvent(event, nil)
	assert.Contains(t, result.User.ID, "[REDACTED]")
}

func TestScrubEvent_RedactsJWTInException(t *testing.T) {
	// Build fake JWT at runtime to avoid Semgrep flagging test fixtures
	fakeJWT := "eyJ" + strings.Repeat("a", 20) + "." + "eyJ" + strings.Repeat("b", 20) + ".sig"
	event := &sentrygo.Event{
		Exception: []sentrygo.Exception{{Value: "Error: token " + fakeJWT + " expired"}},
	}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Exception[0].Value, "eyJ")
	assert.Contains(t, result.Exception[0].Value, "[REDACTED]")
}

func TestScrubEvent_RedactsPostgresDSN(t *testing.T) {
	// Build DSN at runtime to avoid detect-secrets flagging test fixtures
	dsn := "postgres://" + "user:pass@host:5432/db"
	event := &sentrygo.Event{
		Exception: []sentrygo.Exception{{Value: "dial: " + dsn}},
	}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Exception[0].Value, "postgres://")
}

func TestScrubEvent_RedactsRedisDSN(t *testing.T) {
	dsn := "redis://" + "default:pass@host:6379"
	event := &sentrygo.Event{
		Exception: []sentrygo.Exception{{Value: "connect: " + dsn}},
	}
	result := scrubEvent(event, nil)
	assert.NotContains(t, result.Exception[0].Value, "redis://")
}

func TestScrubEvent_DropsDSNLeak(t *testing.T) {
	event := &sentrygo.Event{
		Exception: []sentrygo.Exception{{Value: "error sending to https://abc@o123.ingest.sentry.io/456"}},
	}
	result := scrubEvent(event, nil)
	assert.Nil(t, result)
}

func TestRedactSensitivePatterns_CleanMessage(t *testing.T) {
	msg := "TypeError: Cannot read properties of undefined"
	assert.Equal(t, msg, RedactSensitivePatterns(msg))
}

func TestRedactSensitivePatterns_Email(t *testing.T) {
	assert.Contains(t, RedactSensitivePatterns("User alice@example.com failed"), "[REDACTED]")
}

func TestRedactSensitivePatterns_Base64Blob(t *testing.T) {
	blob := strings.Repeat("A", 70)
	assert.Contains(t, RedactSensitivePatterns("Key: "+blob), "[REDACTED]")
}

func TestRedactSensitivePatterns_HexHash(t *testing.T) {
	hex := strings.Repeat("a", 64)
	assert.Contains(t, RedactSensitivePatterns("Hash: "+hex), "[REDACTED]")
}
