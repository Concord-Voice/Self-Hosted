package backuphash

import (
	"bytes"
	"encoding/base64"
	"errors"
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

// The B2 writer must emit exactly what the B1 reader already deployed accepts.
// The literal pins the reader's tuple so a writer-only parameter change fails
// here instead of producing codes a B1 replica refuses.
func TestNewV2EmitsTheB1ReaderFormatWithFreshSalts(t *testing.T) {
	const b1ReaderPrefix = "v2$argon2id$v=19$m=19456,t=2,p=1$"
	code := "ABCD1234"
	first, err := NewV2(code)
	if err != nil {
		t.Fatalf("NewV2: %v", err)
	}
	second, err := NewV2(code)
	if err != nil {
		t.Fatalf("NewV2: %v", err)
	}
	if !strings.HasPrefix(first, b1ReaderPrefix) {
		t.Fatalf("v2 writer prefix drifted from the B1 reader: %q", first[:len(b1ReaderPrefix)])
	}
	firstSalt, firstDigest, err := Parse(first)
	if err != nil || firstSalt == nil {
		t.Fatalf("B1 reader refused the writer's output: salt=%v err=%v", firstSalt, err)
	}
	secondSalt, _, err := Parse(second)
	if err != nil {
		t.Fatalf("B1 reader refused the writer's output: %v", err)
	}
	if bytes.Equal(firstSalt, secondSalt) || first == second {
		t.Fatal("two hashes of one code share a salt")
	}
	if !bytes.Equal(DeriveV2(code, firstSalt), firstDigest) {
		t.Fatal("stored digest is not the bounded Argon2id of the code")
	}
	if bytes.Equal(DeriveV2("ABCD1235", firstSalt), firstDigest) {
		t.Fatal("a different code derived the same digest")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewV2FailsClosedWithoutASalt(t *testing.T) {
	stored, err := newV2("ABCD1234", failingReader{})
	if err == nil || stored != "" {
		t.Fatalf("newV2 = %q, %v; want no hash and an error", stored, err)
	}
	stored, err = newV2("ABCD1234", strings.NewReader("short"))
	if err == nil || stored != "" {
		t.Fatalf("newV2 with a short salt = %q, %v; want no hash and an error", stored, err)
	}
}
