package messagecreate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type vector struct {
	Name      string      `json:"name"`
	Input     vectorInput `json:"input"`
	Preimage  string      `json:"preimage"`
	SHA256Hex string      `json:"sha256_hex"`
}

type vectorInput FingerprintInput

func (in vectorInput) fingerprintInput() FingerprintInput { return FingerprintInput(in) }

func TestFingerprintV1Vectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "fingerprint-v1.json"))
	require.NoError(t, err)
	var vectors []vector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, test := range vectors {
		t.Run(test.Name, func(t *testing.T) {
			input := test.Input.fingerprintInput()
			preimage, err := canonicalPreimageV1(input)
			require.NoError(t, err)
			assert.Equal(t, test.Preimage, string(preimage))

			got, err := FingerprintV1(input)
			require.NoError(t, err)
			assert.Equal(t, test.SHA256Hex, hex.EncodeToString(got[:]))
		})
	}
}

func TestFingerprintV1BindsEveryField(t *testing.T) {
	base := validInput()
	want, err := FingerprintV1(base)
	require.NoError(t, err)

	mutations := map[string]func(*FingerprintInput){
		"scope": func(in *FingerprintInput) {
			in.Scope = ScopeDM
			in.Mentions.RoleIDs = nil
			in.Mentions.Everyone = false
		},
		"scope ID":   func(in *FingerprintInput) { in.ScopeID = "11111111-1111-4111-8111-111111111112" },
		"message ID": func(in *FingerprintInput) { in.MessageID = "22222222-2222-4222-8222-222222222223" },
		"sender ID":  func(in *FingerprintInput) { in.SenderID = "33333333-3333-4333-8333-333333333334" },
		"key epoch":  func(in *FingerprintInput) { in.KeyEpoch++ },
		"envelope":   func(in *FingerprintInput) { in.Envelope = "m1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwe" },
		"reply":      func(in *FingerprintInput) { in.ReplyToID = "44444444-4444-4444-8444-444444444445" },
		"GIF":        func(in *FingerprintInput) { slug := "replacement"; in.GIFSlug = &slug },
		"attachment order": func(in *FingerprintInput) {
			in.AttachmentIDs[0], in.AttachmentIDs[1] = in.AttachmentIDs[1], in.AttachmentIDs[0]
		},
		"mentioned user": func(in *FingerprintInput) { in.Mentions.UserIDs = []string{"66666666-6666-4666-8666-666666666667"} },
		"mentioned role": func(in *FingerprintInput) { in.Mentions.RoleIDs = []string{"77777777-7777-4777-8777-777777777778"} },
		"everyone":       func(in *FingerprintInput) { in.Mentions.Everyone = true },
		"here":           func(in *FingerprintInput) { in.Mentions.Here = true },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := cloneInput(base)
			mutate(&candidate)
			got, err := FingerprintV1(candidate)
			require.NoError(t, err)
			assert.NotEqual(t, want, got)
		})
	}
}

func TestFingerprintV1NormalizesMentionOrderAndGIFWhitespace(t *testing.T) {
	first := validInput()
	first.Mentions.UserIDs = []string{"66666666-6666-4666-8666-666666666667", "55555555-5555-4555-8555-555555555556"}
	first.Mentions.RoleIDs = []string{"88888888-8888-4888-8888-888888888889", "77777777-7777-4777-8777-777777777778"}
	first.GIFSlug = stringPtr("  happy-gif  ")
	second := cloneInput(first)
	second.Mentions.UserIDs[0], second.Mentions.UserIDs[1] = second.Mentions.UserIDs[1], second.Mentions.UserIDs[0]
	second.Mentions.RoleIDs[0], second.Mentions.RoleIDs[1] = second.Mentions.RoleIDs[1], second.Mentions.RoleIDs[0]
	second.GIFSlug = stringPtr("happy-gif")

	firstFingerprint, err := FingerprintV1(first)
	require.NoError(t, err)
	secondFingerprint, err := FingerprintV1(second)
	require.NoError(t, err)
	assert.Equal(t, firstFingerprint, secondFingerprint)
}

func TestFingerprintV1RejectsAmbiguousOrInvalidInput(t *testing.T) {
	tests := map[string]func(*FingerprintInput){
		"unknown scope":         func(in *FingerprintInput) { in.Scope = "server" },
		"uppercase UUID":        func(in *FingerprintInput) { in.ScopeID = "11111111-1111-4111-8111-11111111111A" },
		"nil UUID":              func(in *FingerprintInput) { in.MessageID = "00000000-0000-0000-0000-000000000000" },
		"invalid UUID variant":  func(in *FingerprintInput) { in.SenderID = "33333333-3333-4333-c333-333333333333" },
		"zero epoch":            func(in *FingerprintInput) { in.KeyEpoch = 0 },
		"too large epoch":       func(in *FingerprintInput) { in.KeyEpoch = maxSafeInteger + 1 },
		"wrong envelope prefix": func(in *FingerprintInput) { in.Envelope = "b1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwd" },
		"noncanonical envelope": func(in *FingerprintInput) { in.Envelope = "m1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwd=" },
		"short envelope":        func(in *FingerprintInput) { in.Envelope = "m1:AAECAwQFBgcICQoLDA0ODw==" },
		"invalid reply":         func(in *FingerprintInput) { in.ReplyToID = "bad" },
		"invalid GIF":           func(in *FingerprintInput) { in.GIFSlug = stringPtr("bad slug") },
		"too many attachments": func(in *FingerprintInput) {
			in.AttachmentIDs = []string{"88888888-8888-4888-8888-888888888888", "99999999-9999-4999-8999-999999999999", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}
		},
		"duplicate attachment": func(in *FingerprintInput) {
			in.AttachmentIDs = []string{"88888888-8888-4888-8888-888888888888", "88888888-8888-4888-8888-888888888888"}
		},
		"duplicate mentioned user": func(in *FingerprintInput) {
			in.Mentions.UserIDs = []string{"66666666-6666-4666-8666-666666666667", "66666666-6666-4666-8666-666666666667"}
		},
		"duplicate mentioned role": func(in *FingerprintInput) {
			in.Mentions.RoleIDs = []string{"77777777-7777-4777-8777-777777777778", "77777777-7777-4777-8777-777777777778"}
		},
		"too many mentioned users": func(in *FingerprintInput) {
			in.Mentions.UserIDs = uniqueUUIDs(maxMentionedUsers + 1)
		},
		"too many mentioned roles": func(in *FingerprintInput) {
			in.Mentions.RoleIDs = uniqueUUIDs(maxMentionedRoles + 1)
		},
		"DM everyone": func(in *FingerprintInput) { in.Scope = ScopeDM; in.Mentions.Everyone = true },
		"DM role": func(in *FingerprintInput) {
			in.Scope = ScopeDM
			in.Mentions.RoleIDs = []string{"77777777-7777-4777-8777-777777777778"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := cloneInput(validInput())
			mutate(&input)
			_, err := FingerprintV1(input)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func validInput() FingerprintInput {
	input := FingerprintInput{}
	input.Scope = ScopeChannel
	input.ScopeID = testUUID("1")
	input.MessageID = testUUID("2")
	input.SenderID = testUUID("3")
	input.KeyEpoch = 7
	input.Envelope = "m1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwd"
	input.ReplyToID = testUUID("4")
	input.GIFSlug = stringPtr("happy-gif")
	input.AttachmentIDs = []string{testUUID("8"), testUUID("9")}
	input.Mentions = MentionIntent{UserIDs: []string{testUUID("5")}, RoleIDs: []string{testUUID("7")}}
	return input
}

func cloneInput(in FingerprintInput) FingerprintInput {
	clone := in
	clone.AttachmentIDs = append([]string(nil), in.AttachmentIDs...)
	clone.Mentions.UserIDs = append([]string(nil), in.Mentions.UserIDs...)
	clone.Mentions.RoleIDs = append([]string(nil), in.Mentions.RoleIDs...)
	if in.GIFSlug != nil {
		clone.GIFSlug = stringPtr(*in.GIFSlug)
	}
	return clone
}

func stringPtr(value string) *string { return &value }

func testUUID(suffix string) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012s", suffix)
}

func uniqueUUIDs(count int) []string {
	result := make([]string, count)
	for i := range result {
		result[i] = fmt.Sprintf("66666666-6666-4666-8666-%012x", i+1)
	}
	return result
}

func TestFingerprintV1MatchesSHA256(t *testing.T) {
	preimage, err := canonicalPreimageV1(validInput())
	require.NoError(t, err)
	want := sha256.Sum256(preimage)
	got, err := FingerprintV1(validInput())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
