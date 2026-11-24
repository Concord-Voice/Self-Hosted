package store

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/authoritycontract"
	"github.com/google/uuid"
)

// These pin decodeDeviceSubmit's output field by field, with no database:
// the request it builds for each device-submit form and the refusal each
// failing branch returns.

func decodeTestAccount(t *testing.T) *authoritytest.Account {
	t.Helper()
	r := authoritytest.NewRealm(t)
	a := r.NewAccount(t)
	a.Actor = authoritytest.NewDevice(t)
	a.Devices[a.Actor.ID] = a.Actor
	a.Root = decodeTestKey(t)
	a.Head = authoritytest.HeadView{Seq: 3, Dseq: 2, Entry: bytes.Repeat([]byte{1}, 48),
		DeviceHead: bytes.Repeat([]byte{2}, 48), Root: authoritytest.Point(a.Root), State: ac.StateActive}
	return a
}

func decodeTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustDecodeDevice(t *testing.T, b []byte) *request {
	t.Helper()
	if _, err := ac.CheckDeviceSubmit(b); err != nil {
		t.Fatalf("fixture does not pass A2's pattern check: %v", err)
	}
	rq, out, ok := decodeDeviceSubmit(b)
	if !ok || rq == nil || out.HTTP != 0 || out.Kind != 0 || out.Code != "" || out.Body != nil {
		t.Fatalf("decodeDeviceSubmit refused a valid submit: ok=%v out=%+v", ok, out)
	}
	return rq
}

func assertRefused(t *testing.T, b []byte, http int, code ac.ErrorCode) {
	t.Helper()
	rq, out, ok := decodeDeviceSubmit(b)
	if ok || rq != nil {
		t.Fatalf("want refusal, got ok=%v rq=%v", ok, rq != nil)
	}
	want := errorOutcome(http, code)
	if out.HTTP != want.HTTP || out.Kind != want.Kind || out.Code != want.Code || !bytes.Equal(out.Body, want.Body) {
		t.Fatalf("outcome = %+v, want %+v", out, want)
	}
}

func assertIntentFromDI(t *testing.T, rq *request) {
	t.Helper()
	if rq.DI == nil {
		t.Fatal("DI not set")
	}
	if !bytes.Equal(rq.D, ac.Digest(rq.Dev.DeviceIntent)) || !bytes.Equal(rq.IntentDigest, rq.D) {
		t.Fatal("D / IntentDigest are not the device-intent digest")
	}
	want := ac.Intent{RealmID: rq.DI.RealmID, AccountID: rq.DI.AccountID, RequestID: rq.DI.RequestID,
		Op: rq.DI.Op, ExpiresAtMs: rq.DI.ExpiresAtMs}
	if rq.Intent.RealmID != want.RealmID || rq.Intent.AccountID != want.AccountID ||
		rq.Intent.RequestID != want.RequestID || rq.Intent.Op != want.Op || rq.Intent.ExpiresAtMs != want.ExpiresAtMs {
		t.Fatalf("Intent = %+v, want %+v", rq.Intent, want)
	}
}

func TestDecodeDeviceSubmitAddDevice(t *testing.T) {
	a := decodeTestAccount(t)
	built := a.AddDevice(t, authoritytest.NewDevice(t))
	rq := mustDecodeDevice(t, built.Bytes)
	assertIntentFromDI(t, rq)
	if rq.Bind != nil || rq.H != nil {
		t.Fatal("add_device has no bind")
	}
	if rq.Intent.Op != ac.OpAddDevice || !bytes.Equal(rq.LedgerDigest, rq.D) || rq.Class != classOfOp(ac.OpAddDevice) {
		t.Fatalf("op/ledger/class wrong: %+v", rq.Intent)
	}
	if rq.Dev == nil || !bytes.Equal(rq.Sub.CPAuthz, rq.Dev.CPAuthz) || !bytes.Equal(rq.Sub.CPAuthzSig, rq.Dev.CPAuthzSig) ||
		!bytes.Equal(rq.Sub.CPEvidence, rq.Dev.CPEvidence) {
		t.Fatal("Sub not copied from the device submit")
	}
	if rq.AZ.Actor != ac.ActorUser || rq.AZ.AccountID != a.ID || rq.Ev.RequestID != rq.Intent.RequestID {
		t.Fatal("cp-authz / cp-evidence not decoded")
	}
}

func TestDecodeDeviceSubmitBindRecovery(t *testing.T) {
	a := decodeTestAccount(t)
	built := a.BindRecovery(t, decodeTestKey(t), "", nil)
	rq := mustDecodeDevice(t, built.Bytes)
	assertIntentFromDI(t, rq)
	if rq.Bind == nil || !bytes.Equal(rq.H, ac.Digest(built.BindBytes)) {
		t.Fatal("bind / H not decoded")
	}
	if !bytes.Equal(rq.LedgerDigest, rq.D) || rq.Class != classOfOp(ac.OpBindRecovery) {
		t.Fatal("bind's ledger key is D")
	}
}

func TestDecodeDeviceSubmitCancelByRoot(t *testing.T) {
	a := decodeTestAccount(t)
	a.Recovery = decodeTestKey(t)
	built := a.CancelBindByRoot(t, bytes.Repeat([]byte{7}, 48))
	rq := mustDecodeDevice(t, built.Bytes)
	assertIntentFromDI(t, rq)
	if rq.Intent.Op != ac.OpCancelRecoveryBind || !bytes.Equal(rq.LedgerDigest, rq.H) ||
		bytes.Equal(rq.LedgerDigest, rq.IntentDigest) || rq.Class != classCancel {
		t.Fatal("root-form cancel's ledger key is H(B), its intent digest D")
	}
}

func TestDecodeDeviceSubmitCancelBySheet(t *testing.T) {
	a := decodeTestAccount(t)
	key := decodeTestKey(t)
	built := a.CancelBindBySheet(t, key, bytes.Repeat([]byte{7}, 48))
	rq := mustDecodeDevice(t, built.Bytes)
	if rq.DI != nil || rq.D != nil {
		t.Fatal("the sheet form has no device-intent")
	}
	b := built.Parts.Bind
	if rq.Intent.RealmID != b.RealmID || rq.Intent.AccountID != b.AccountID || rq.Intent.RequestID != b.RequestID ||
		rq.Intent.Op != ac.OpCancelRecoveryBind || rq.Intent.ExpiresAtMs != b.ExpiresAtMs {
		t.Fatalf("sheet-form intent not taken from B: %+v", rq.Intent)
	}
	h := ac.Digest(built.BindBytes)
	if !bytes.Equal(rq.H, h) || !bytes.Equal(rq.IntentDigest, h) || !bytes.Equal(rq.LedgerDigest, h) ||
		rq.Class != classCancel {
		t.Fatal("sheet-form digests are H(B)")
	}
}

func TestDecodeDeviceSubmitRefusesGarbage(t *testing.T) {
	assertRefused(t, []byte("not a device submit"), 400, ac.ErrorMalformed)
	assertRefused(t, nil, 400, ac.ErrorMalformed)
}

func TestDecodeDeviceSubmitRefusesNonUserActor(t *testing.T) {
	a := decodeTestAccount(t)
	built := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, NewDevice: authoritytest.NewDevice(t),
		Factors: []string{ac.FactorPassword}, Mutate: func(p *authoritytest.DeviceParts) { p.CPAuthz.Actor = ac.ActorOperator }})
	if _, err := ac.CheckDeviceSubmit(built.Bytes); err != nil {
		t.Fatalf("fixture must pass A2: %v", err)
	}
	assertRefused(t, built.Bytes, 400, ac.ErrorBadAuthz)
}

// A bind naming another account than its device-intent, and a device-intent
// whose #18 is not H(B), are both refused after A2 passes them.
func TestDecodeDeviceSubmitRefusesBindMismatch(t *testing.T) {
	a := decodeTestAccount(t)
	key := decodeTestKey(t)
	ref := a.BindRecovery(t, key, "", nil)
	spec := func(b *ac.RecoveryBind, mut func(*authoritytest.DeviceParts)) authoritytest.DeviceSpec {
		return authoritytest.DeviceSpec{Op: ac.OpBindRecovery, Bind: b, BindKey: key,
			Factors: []string{ac.FactorEmail, ac.FactorPassword, ac.FactorTOTP}, MFAEnrolled: true, Mutate: mut}
	}
	cases := map[string]authoritytest.BuiltDevice{}
	other := *ref.Parts.Bind
	other.AccountID = uuid.NewString()
	cases["account"] = a.BuildDevice(t, spec(&other, nil))
	otherRealm := *ref.Parts.Bind
	otherRealm.RealmID = uuid.NewString()
	cases["realm"] = a.BuildDevice(t, spec(&otherRealm, nil))
	same := *ref.Parts.Bind
	cases["digest"] = a.BuildDevice(t, spec(&same, func(p *authoritytest.DeviceParts) {
		if p.DeviceIntent != nil {
			p.DeviceIntent.RecoveryBindDigest = bytes.Repeat([]byte{9}, 48)
		}
	}))
	for name, built := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ac.CheckDeviceSubmit(built.Bytes); err != nil {
				t.Fatalf("fixture must pass A2: %v", err)
			}
			assertRefused(t, built.Bytes, 400, ac.ErrorMalformed)
		})
	}
}
