package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/natstest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/google/uuid"
	rawnats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

const dmBlockVoiceEjectionTestSecret = "test-dm-block-voice-ejection-secret" // #nosec G101 -- test-only shared key // pragma: allowlist secret -- deterministic test fixture, not a credential

func TestDMBlockVoiceEjectionProofFixtureMatchesMediaPlane(t *testing.T) {
	const (
		secret    = "fixed-dm-block-proof-fixture" // #nosec G101 -- test-only cross-language fixture // pragma: allowlist secret -- deterministic test fixture, not a credential
		timestamp = "1700000000"
		channelID = "conversation-fixture"
		userID    = "user-fixture"
		nonce     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // pragma: allowlist secret -- deterministic proof fixture, not a credential
	)
	require.Equal(t,
		"a975a97cd86382e57ba00424648893863267e089d3d8c962a22dd9c106ca35a9", // pragma: allowlist secret -- deterministic proof fixture, not a credential
		mediaproof.Sign(
			mediaproof.DeriveKey(secret, dmBlockVoiceEjectionRequestProof),
			dmBlockVoiceEjectionProofVersion,
			timestamp,
			channelID,
			userID,
			"disconnect",
			nonce,
		),
	)
	require.Equal(t,
		"46917e81931fc7be183f2fb8ffc9b100656ccda5c0285d65651301df3a8b243d", // pragma: allowlist secret -- deterministic proof fixture, not a credential
		mediaproof.Sign(
			mediaproof.DeriveKey(secret, dmBlockVoiceEjectionACKProof),
			dmBlockVoiceEjectionProofVersion,
			timestamp,
			channelID,
			userID,
			"disconnect",
			nonce,
			"true",
		),
	)
}

func TestCredentialEpochVoiceEjectionProofFixtureMatchesMediaPlane(t *testing.T) {
	const (
		fixtureKey = "fixed-credential-epoch-fixture"
		ts         = "1700000000"
		user       = "11111111-1111-4111-8111-111111111111"
		newEpoch   = "abcdefabcdefabcdefabcdefabcdefab"                                 // pragma: allowlist secret
		oldEpoch   = "0123456789abcdef0123456789abcdef"                                 // pragma: allowlist secret
		nonce      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	)
	require.Equal(t, "b7401410830282b68ce3d0dbf0f40c0c02976563dc989979161669da769b3bd7", // pragma: allowlist secret
		mediaproof.Sign(mediaproof.DeriveKey(fixtureKey, credentialEpochVoiceEjectionRequestProof), "v1", ts, user, newEpoch, oldEpoch, "disconnect", nonce))
	require.Equal(t, "7f247e435dc4c9704836b0723f2281e473fb4b34f2165e279d1035e02a53ce48", // pragma: allowlist secret
		mediaproof.Sign(mediaproof.DeriveKey(fixtureKey, credentialEpochVoiceEjectionACKProof), "v1", ts, user, newEpoch, oldEpoch, "disconnect", nonce, "true"))
}

type dmBlockVoiceEjectionRequesterStub struct {
	subject  string
	payload  map[string]interface{}
	timeout  time.Duration
	response []byte
	respond  func(map[string]interface{}) []byte
	err      error
}

func (s *dmBlockVoiceEjectionRequesterStub) RequestWithContext(ctx context.Context, subject string, data interface{}) ([]byte, error) {
	s.subject = subject
	payload, ok := data.(map[string]interface{})
	if !ok {
		return nil, errors.New("unexpected NATS request payload type")
	}
	s.payload = payload
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		s.timeout = time.Until(deadline)
	}
	if s.respond != nil {
		return s.respond(payload), s.err
	}
	return s.response, s.err
}

func stringPayload(t *testing.T, payload map[string]interface{}, field string) string {
	t.Helper()
	value, ok := payload[field].(string)
	require.True(t, ok, field)
	return value
}

func signedDMBlockVoiceEjectionACK(t *testing.T, secret string, request map[string]interface{}) []byte {
	t.Helper()
	channelID := stringPayload(t, request, "channelId")
	userID := stringPayload(t, request, "userId")
	action := stringPayload(t, request, "action")
	timestamp := stringPayload(t, request, "timestamp")
	nonce := stringPayload(t, request, "nonce")
	acknowledgement := map[string]interface{}{
		"version":   2,
		"channelId": channelID,
		"userId":    userID,
		"action":    action,
		"timestamp": timestamp,
		"nonce":     nonce,
		"ok":        true,
	}
	acknowledgement["proof"] = mediaproof.Sign(
		mediaproof.DeriveKey(secret, dmBlockVoiceEjectionACKProof),
		dmBlockVoiceEjectionProofVersion,
		timestamp,
		channelID,
		userID,
		action,
		nonce,
		"true",
	)
	encoded, err := json.Marshal(acknowledgement)
	require.NoError(t, err)
	return encoded
}

func TestPublishDMBlockVoiceEjectionAcknowledgesOnlySignedMediaSuccess(t *testing.T) {
	conversationID := uuid.NewString()
	userID := uuid.New()
	requester := &dmBlockVoiceEjectionRequesterStub{}
	requester.respond = func(request map[string]interface{}) []byte {
		return signedDMBlockVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, request)
	}

	require.NoError(t, publishDMBlockVoiceEjection(
		context.Background(), requester, dmBlockVoiceEjectionTestSecret, conversationID, userID,
	))
	require.Equal(t, dmBlockVoiceEjectionACKSubject, requester.subject)
	require.Equal(t, 2, requester.payload["version"])
	require.Equal(t, conversationID, requester.payload["channelId"])
	require.Equal(t, userID.String(), requester.payload["userId"])
	require.Equal(t, "disconnect", requester.payload["action"])
	timestamp := stringPayload(t, requester.payload, "timestamp")
	nonce := stringPayload(t, requester.payload, "nonce")
	proof := stringPayload(t, requester.payload, "proof")
	require.Len(t, nonce, 64)
	require.True(t, dmBlockVoiceEjectionTimestampIsCurrent(timestamp))
	require.True(t, mediaproof.Verify(
		mediaproof.DeriveKey(dmBlockVoiceEjectionTestSecret, dmBlockVoiceEjectionRequestProof),
		proof,
		dmBlockVoiceEjectionProofVersion,
		timestamp,
		conversationID,
		userID.String(),
		"disconnect",
		nonce,
	))
	require.Positive(t, requester.timeout)
	require.LessOrEqual(t, requester.timeout, dmBlockVoiceEjectionRequestTimeout)
}

func TestPublishCredentialEpochVoiceEjectionAcknowledgesOnlySignedSuccess(t *testing.T) {
	userID := uuid.New()
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	requester := &dmBlockVoiceEjectionRequesterStub{}
	requester.respond = func(request map[string]interface{}) []byte {
		ack := map[string]interface{}{"version": 1, "userId": request["userId"], "credentialEpoch": request["credentialEpoch"], "supersededCredentialEpoch": request["supersededCredentialEpoch"], "action": request["action"], "timestamp": request["timestamp"], "nonce": request["nonce"], "ok": true}
		ack["proof"] = mediaproof.Sign(
			mediaproof.DeriveKey(dmBlockVoiceEjectionTestSecret, credentialEpochVoiceEjectionACKProof),
			"v1",
			stringPayload(t, request, "timestamp"),
			stringPayload(t, request, "userId"),
			stringPayload(t, request, "credentialEpoch"),
			stringPayload(t, request, "supersededCredentialEpoch"),
			stringPayload(t, request, "action"),
			stringPayload(t, request, "nonce"),
			"true",
		)
		encoded, err := json.Marshal(ack)
		require.NoError(t, err)
		return encoded
	}
	require.NoError(t, publishCredentialEpochVoiceEjection(context.Background(), requester, dmBlockVoiceEjectionTestSecret, userID, newEpoch, oldEpoch))
	require.Equal(t, credentialEpochVoiceEjectionACKSubject, requester.subject)
	requester.respond = func(map[string]interface{}) []byte {
		return []byte(`{"version":1,"ok":true}`)
	}
	require.Error(t, publishCredentialEpochVoiceEjection(context.Background(), requester, dmBlockVoiceEjectionTestSecret, userID, newEpoch, oldEpoch))
}

func TestPublishDMBlockVoiceEjectionRejectsNoResponderOrTimeout(t *testing.T) {
	requestErr := errors.New("nats: no responders")
	requester := &dmBlockVoiceEjectionRequesterStub{err: requestErr}

	err := publishDMBlockVoiceEjection(context.Background(), requester, dmBlockVoiceEjectionTestSecret, uuid.NewString(), uuid.New())

	require.ErrorIs(t, err, requestErr)
}

func TestPublishDMBlockVoiceEjectionRejectsUnsignedOrMismatchedAcknowledgement(t *testing.T) {
	for _, mutation := range []struct {
		name    string
		respond func(request map[string]interface{}) []byte
	}{
		{name: "malformed", respond: func(map[string]interface{}) []byte { return []byte(`not-json`) }},
		{name: "unsigned", respond: func(map[string]interface{}) []byte { return []byte(`{"version":2,"ok":true}`) }},
		// Echoes none of the request, so it is rejected for the missing fields before
		// the proof is read. The echoed-forgery tests cover the proof check itself.
		{name: "zero proof and no echoed fields", respond: func(map[string]interface{}) []byte {
			return []byte(`{"version":2,"ok":true,"proof":"0000000000000000000000000000000000000000000000000000000000000000"}`)
		}},
		{name: "wrong nonce", respond: func(request map[string]interface{}) []byte {
			payloadCopy := clonePayload(request)
			payloadCopy["nonce"] = "different-nonce"
			return signedDMBlockVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, payloadCopy)
		}},
		{name: "wrong channel", respond: func(request map[string]interface{}) []byte {
			payloadCopy := clonePayload(request)
			payloadCopy["channelId"] = uuid.NewString()
			return signedDMBlockVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, payloadCopy)
		}},
		{name: "stale timestamp", respond: func(request map[string]interface{}) []byte {
			payloadCopy := clonePayload(request)
			payloadCopy["timestamp"] = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
			return signedDMBlockVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, payloadCopy)
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			requester := &dmBlockVoiceEjectionRequesterStub{respond: mutation.respond}
			err := publishDMBlockVoiceEjection(
				context.Background(), requester, dmBlockVoiceEjectionTestSecret, uuid.NewString(), uuid.New(),
			)
			require.Error(t, err)
		})
	}
}

// echoedACKForeignSecret signs a well-formed acknowledgement under a key the
// control plane does not hold.
const echoedACKForeignSecret = "secret-the-control-plane-does-not-hold" // #nosec G101 -- test-only key // pragma: allowlist secret -- deterministic test fixture, not a credential

type echoedACKForgery struct {
	name  string
	reply func(t *testing.T, request map[string]interface{}) []byte
}

// forgedEchoedACKs builds the replies an anonymous subscriber on an ACK subject
// can send. Every field the verifier compares travels in the request in
// plaintext, so each reply is the genuine acknowledgement with only its proof
// replaced: the proof check is the one thing that can refuse it. The negative
// cases above omit those fields and are refused before the proof is read.
func forgedEchoedACKs(sign func(t *testing.T, secret string, request map[string]interface{}) []byte) []echoedACKForgery {
	withProof := func(t *testing.T, request map[string]interface{}, proof interface{}) []byte {
		t.Helper()
		var acknowledgement map[string]interface{}
		require.NoError(t, json.Unmarshal(sign(t, dmBlockVoiceEjectionTestSecret, request), &acknowledgement))
		acknowledgement["proof"] = proof
		encoded, err := json.Marshal(acknowledgement)
		require.NoError(t, err)
		return encoded
	}
	return []echoedACKForgery{
		{name: "replayed request proof", reply: func(t *testing.T, request map[string]interface{}) []byte {
			return withProof(t, request, request["proof"])
		}},
		{name: "zero proof", reply: func(t *testing.T, request map[string]interface{}) []byte {
			return withProof(t, request, strings.Repeat("0", 64))
		}},
		{name: "empty proof", reply: func(t *testing.T, request map[string]interface{}) []byte {
			return withProof(t, request, "")
		}},
		{name: "proof under another secret", reply: func(t *testing.T, request map[string]interface{}) []byte {
			return sign(t, echoedACKForeignSecret, request)
		}},
	}
}

func signedCredentialEpochVoiceEjectionACK(t *testing.T, secret string, request map[string]interface{}) []byte {
	t.Helper()
	acknowledgement := map[string]interface{}{
		"version":                   1,
		"userId":                    stringPayload(t, request, "userId"),
		"credentialEpoch":           stringPayload(t, request, "credentialEpoch"),
		"supersededCredentialEpoch": stringPayload(t, request, "supersededCredentialEpoch"),
		"action":                    stringPayload(t, request, "action"),
		"timestamp":                 stringPayload(t, request, "timestamp"),
		"nonce":                     stringPayload(t, request, "nonce"),
		"ok":                        true,
	}
	acknowledgement["proof"] = mediaproof.Sign(
		mediaproof.DeriveKey(secret, credentialEpochVoiceEjectionACKProof),
		"v1",
		stringPayload(t, request, "timestamp"),
		stringPayload(t, request, "userId"),
		stringPayload(t, request, "credentialEpoch"),
		stringPayload(t, request, "supersededCredentialEpoch"),
		stringPayload(t, request, "action"),
		stringPayload(t, request, "nonce"),
		"true",
	)
	encoded, err := json.Marshal(acknowledgement)
	require.NoError(t, err)
	return encoded
}

func TestPublishDMBlockVoiceEjectionRejectsEchoedAcknowledgementWithForgedProof(t *testing.T) {
	conversationID, userID := uuid.NewString(), uuid.New()
	genuine := &dmBlockVoiceEjectionRequesterStub{respond: func(request map[string]interface{}) []byte {
		return signedDMBlockVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, request)
	}}
	require.NoError(t, publishDMBlockVoiceEjection(
		context.Background(), genuine, dmBlockVoiceEjectionTestSecret, conversationID, userID,
	), "control: the genuine echoed acknowledgement must be accepted, or the forgeries prove nothing")

	for _, forgery := range forgedEchoedACKs(signedDMBlockVoiceEjectionACK) {
		t.Run(forgery.name, func(t *testing.T) {
			requester := &dmBlockVoiceEjectionRequesterStub{respond: func(request map[string]interface{}) []byte {
				return forgery.reply(t, request)
			}}
			err := publishDMBlockVoiceEjection(
				context.Background(), requester, dmBlockVoiceEjectionTestSecret, conversationID, userID,
			)
			require.ErrorContains(t, err, "DM block voice ejection acknowledgement rejected")
		})
	}
}

func TestPublishCredentialEpochVoiceEjectionRejectsEchoedAcknowledgementWithForgedProof(t *testing.T) {
	userID := uuid.New()
	newEpoch, oldEpoch := "abcdefabcdefabcdefabcdefabcdefab", "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	genuine := &dmBlockVoiceEjectionRequesterStub{respond: func(request map[string]interface{}) []byte {
		return signedCredentialEpochVoiceEjectionACK(t, dmBlockVoiceEjectionTestSecret, request)
	}}
	require.NoError(t, publishCredentialEpochVoiceEjection(
		context.Background(), genuine, dmBlockVoiceEjectionTestSecret, userID, newEpoch, oldEpoch,
	), "control: the genuine echoed acknowledgement must be accepted, or the forgeries prove nothing")

	for _, forgery := range forgedEchoedACKs(signedCredentialEpochVoiceEjectionACK) {
		t.Run(forgery.name, func(t *testing.T) {
			requester := &dmBlockVoiceEjectionRequesterStub{respond: func(request map[string]interface{}) []byte {
				return forgery.reply(t, request)
			}}
			err := publishCredentialEpochVoiceEjection(
				context.Background(), requester, dmBlockVoiceEjectionTestSecret, userID, newEpoch, oldEpoch,
			)
			require.ErrorContains(t, err, "credential media ejection acknowledgement rejected")
		})
	}
}

func clonePayload(source map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func TestPublishDMBlockVoiceEjectionRejectsEmptySharedSecret(t *testing.T) {
	err := publishDMBlockVoiceEjection(context.Background(), &dmBlockVoiceEjectionRequesterStub{}, "", uuid.NewString(), uuid.New())
	require.Error(t, err)
}

func TestRedTeamUnauthenticatedNATSResponderCannotSpoofMediaTeardownACK(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = rawnats.DefaultURL
	}
	attacker, err := rawnats.Connect(url)
	if err != nil {
		natstest.Unavailable(t, "the raw connection failed")
	}
	t.Cleanup(attacker.Close)

	// The subscriber echoes every field of the request, which it reads in
	// plaintext. It answers the control request with the genuine proof and the
	// forged one with the request's own proof replayed, so the two replies
	// differ only in the proof. It answers nothing else: other packages'
	// ejection traffic can share this subject on the CI bus, and the
	// bookkeeping never blocks, so that traffic cannot stall it.
	userID := uuid.New()
	controlChannel, forgedChannel := uuid.NewString(), uuid.NewString()
	forgedSeen := make(chan struct{}, 1)
	sub, err := attacker.Subscribe(dmBlockVoiceEjectionACKSubject, func(msg *rawnats.Msg) {
		var request map[string]interface{}
		if json.Unmarshal(msg.Data, &request) != nil {
			return
		}
		proof := request["proof"]
		switch request["channelId"] {
		case controlChannel:
			proof = genuineDMBlockVoiceEjectionACKProof(request)
		case forgedChannel:
			select {
			case forgedSeen <- struct{}{}:
			default:
			}
		default:
			return
		}
		reply, marshalErr := json.Marshal(map[string]interface{}{
			"version": request["version"], "channelId": request["channelId"], "userId": request["userId"],
			"action": request["action"], "timestamp": request["timestamp"], "nonce": request["nonce"],
			"ok": true, "proof": proof,
		})
		if marshalErr == nil {
			_ = msg.Respond(reply)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, attacker.Flush())

	requester := natstest.Connect(t, url)
	require.NoError(t, publishDMBlockVoiceEjection(
		context.Background(), requester, dmBlockVoiceEjectionTestSecret, controlChannel, userID,
	), "control: the subscriber's echoed reply with the genuine proof was not accepted over this bus, so the forged case proves nothing")
	err = publishDMBlockVoiceEjection(
		context.Background(), requester, dmBlockVoiceEjectionTestSecret, forgedChannel, userID,
	)
	select {
	case <-forgedSeen:
	case <-time.After(dmBlockVoiceEjectionRequestTimeout):
		t.Fatal("attacker did not receive the teardown request")
	}
	// A timeout or any other failure does not match: the forged reply must
	// reach the verifier and be refused there.
	require.ErrorContains(t, err, "DM block voice ejection acknowledgement rejected",
		"an anonymous NATS subscriber forged a media teardown ACK, or its reply never reached the proof check")
}

// genuineDMBlockVoiceEjectionACKProof runs on the NATS callback goroutine, where
// require must not be called; a missing field yields a proof that fails.
func genuineDMBlockVoiceEjectionACKProof(request map[string]interface{}) string {
	field := func(name string) string {
		value, _ := request[name].(string)
		return value
	}
	return mediaproof.Sign(
		mediaproof.DeriveKey(dmBlockVoiceEjectionTestSecret, dmBlockVoiceEjectionACKProof),
		dmBlockVoiceEjectionProofVersion,
		field("timestamp"),
		field("channelId"),
		field("userId"),
		field("action"),
		field("nonce"),
		"true",
	)
}
