package backuphash

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	first, err := NewV2(context.Background(), code)
	if err != nil {
		t.Fatalf("NewV2: %v", err)
	}
	second, err := NewV2(context.Background(), code)
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
	if !bytes.Equal(mustDerive(t, code, firstSalt), firstDigest) {
		t.Fatal("stored digest is not the bounded Argon2id of the code")
	}
	if bytes.Equal(mustDerive(t, "ABCD1235", firstSalt), firstDigest) {
		t.Fatal("a different code derived the same digest")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewV2FailsClosedWithoutASalt(t *testing.T) {
	stored, err := newV2(context.Background(), "ABCD1234", failingReader{})
	if err == nil || stored != "" {
		t.Fatalf("newV2 = %q, %v; want no hash and an error", stored, err)
	}
	stored, err = newV2(context.Background(), "ABCD1234", strings.NewReader("short"))
	if err == nil || stored != "" {
		t.Fatalf("newV2 with a short salt = %q, %v; want no hash and an error", stored, err)
	}
}

// swapDeriveKey replaces the Argon2id call for one test so the semaphore can be
// observed without paying 19 MiB per derivation.
func swapDeriveKey(t *testing.T, fn func(string, []byte) []byte) {
	t.Helper()
	orig := deriveKey
	deriveKey = fn
	t.Cleanup(func() { deriveKey = orig })
}

func TestDerivationsNeverExceedTheProcessCap(t *testing.T) {
	var active, peak atomic.Int32
	swapDeriveKey(t, func(string, []byte) []byte {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return make([]byte, v2DigestLen)
	})

	var wg sync.WaitGroup
	errs := make(chan error, 4*MaxConcurrentDerivations)
	for range 4 * MaxConcurrentDerivations {
		wg.Go(func() {
			if _, err := DeriveV2(context.Background(), "ABCD1234", make([]byte, v2SaltLen)); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("DeriveV2: %v", err)
	}
	if got := peak.Load(); got > MaxConcurrentDerivations {
		t.Fatalf("peak concurrent derivations = %d, want at most %d", got, MaxConcurrentDerivations)
	}
}

func TestCancelledContextIsRefusedWithoutDeriving(t *testing.T) {
	var calls atomic.Int32
	swapDeriveKey(t, func(string, []byte) []byte { calls.Add(1); return make([]byte, v2DigestLen) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := DeriveV2(ctx, "ABCD1234", make([]byte, v2SaltLen)); !errors.Is(err, ErrBusy) {
		t.Fatalf("DeriveV2 with a cancelled context = %v, want ErrBusy", err)
	}
	if _, err := NewV2(ctx, "ABCD1234"); !errors.Is(err, ErrBusy) {
		t.Fatalf("NewV2 with a cancelled context = %v, want ErrBusy", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("derived %d times under a cancelled context, want 0", n)
	}
}

func TestFullSemaphoreReturnsBusyAtTheDeadline(t *testing.T) {
	swapDeriveKey(t, func(string, []byte) []byte { t.Fatal("derived without a slot"); return nil })
	for range MaxConcurrentDerivations {
		derivationSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range MaxConcurrentDerivations {
			<-derivationSlots
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := DeriveV2(ctx, "ABCD1234", make([]byte, v2SaltLen)); !errors.Is(err, ErrBusy) {
		t.Fatalf("DeriveV2 on a full semaphore = %v, want ErrBusy", err)
	}
}

func mustDerive(t *testing.T, code string, salt []byte) []byte {
	t.Helper()
	digest, err := DeriveV2(context.Background(), code, salt)
	if err != nil {
		t.Fatalf("DeriveV2: %v", err)
	}
	return digest
}

// A verifier derives every salt under one slot, so a busy answer means none
// was derived and nothing was compared.
func TestDeriveEachV2IsAllOrNothing(t *testing.T) {
	var calls atomic.Int32
	swapDeriveKey(t, func(string, []byte) []byte { calls.Add(1); return make([]byte, v2DigestLen) })
	salts := [][]byte{make([]byte, v2SaltLen), make([]byte, v2SaltLen), make([]byte, v2SaltLen)}

	release := HoldAllSlots(10 * time.Millisecond)
	_, err := DeriveEachV2(context.Background(), "ABCD1234", salts)
	release()
	if !errors.Is(err, ErrBusy) || calls.Load() != 0 {
		t.Fatalf("DeriveEachV2 on held slots = %v after %d derivations, want ErrBusy after none", err, calls.Load())
	}
	digests, err := DeriveEachV2(context.Background(), "ABCD1234", salts)
	if err != nil || len(digests) != len(salts) || int(calls.Load()) != len(salts) {
		t.Fatalf("DeriveEachV2 = %d digests, %v after %d derivations; want %d", len(digests), err, calls.Load(), len(salts))
	}
	if got := time.Duration(derivationWait.Load()); got != maxDerivationWait {
		t.Fatalf("release left the slot wait at %v, want %v", got, maxDerivationWait)
	}
}
