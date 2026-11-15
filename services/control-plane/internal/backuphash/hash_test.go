package backuphash

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseAcceptsOnlyExactStoredFormats(t *testing.T) {
	bare := strings.Repeat("a", 64)
	salt := []byte("0123456789abcdef")
	digest := bytes.Repeat([]byte{0x42}, 32)
	v2 := V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$" + base64.RawURLEncoding.EncodeToString(digest)

	for _, stored := range []string{bare, "v1$" + bare, v2} {
		gotSalt, gotDigest, err := Parse(stored)
		if err != nil {
			t.Fatalf("Parse(%q): %v", stored, err)
		}
		if stored == v2 {
			if !bytes.Equal(gotSalt, salt) || !bytes.Equal(gotDigest, digest) {
				t.Fatal("v2 salt or digest changed")
			}
		} else if gotSalt != nil || len(gotDigest) != 32 {
			t.Fatal("legacy hash has incorrect salt or digest")
		}
	}

	for name, stored := range map[string]string{
		"uppercase legacy": strings.ToUpper(bare),
		"bad legacy hex":   strings.Repeat("z", 64),
		"uppercase v1":     "v1$" + strings.ToUpper(bare),
		"short v1":         "v1$" + bare[:63],
		"old weak v2":      strings.Replace(v2, "m=19456,t=2", "m=8192,t=1", 1),
		"short salt":       V2Prefix + "AA$" + base64.RawURLEncoding.EncodeToString(digest),
		"short digest":     V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$AA",
		"extra field":      v2 + "$extra",
		"bad salt":         V2Prefix + strings.Repeat("!", 22) + "$" + base64.RawURLEncoding.EncodeToString(digest),
		"bad digest":       V2Prefix + base64.RawURLEncoding.EncodeToString(salt) + "$" + strings.Repeat("!", 43),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Parse(stored); err == nil {
				t.Fatal("invalid stored hash accepted")
			}
		})
	}
}

func TestCountAvailableValidatesAllHashesBeforeCounting(t *testing.T) {
	bare := strings.Repeat("a", 64)
	if count, err := CountAvailable([]string{bare, "v1$" + bare, bare}, []bool{false, true}); err != nil || count != 1 {
		t.Fatalf("count = %d, err = %v; want 1, nil", count, err)
	}
	if _, err := CountAvailable([]string{bare, "invalid"}, []bool{false, false}); err == nil {
		t.Fatal("malformed later hash did not fail closed")
	}
}
