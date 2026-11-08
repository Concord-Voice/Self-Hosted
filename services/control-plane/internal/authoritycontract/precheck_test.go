package authoritycontract

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrecheckDelegationAcceptsAValidNextSerial(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	require.NoError(t, tr.PrecheckDelegation(r.del(2, nil).Encode()))
}

// The precheck must agree with T3: whatever it passes, the dual-signed object
// is retained, and whatever T3 ignores, it refuses.
func TestPrecheckDelegationAgreesWithT3(t *testing.T) {
	cases := map[string]func(*testRealm, *Delegation){
		"valid":                         nil,
		"tau_ms = 0":                    func(_ *testRealm, d *Delegation) { d.TauMs = 0 },
		"g_ms above delta_ms":           func(_ *testRealm, d *Delegation) { d.GMs = d.DeltaMs + 1 },
		"lifetime 400 d + 1 ms":         func(_ *testRealm, d *Delegation) { d.NotAfterMs = d.NotBeforeMs + DelegationLifetimeMaxMs + 1 },
		"min_valid_serial above serial": func(_ *testRealm, d *Delegation) { d.MinValidSerial = d.Serial + 1 },
		"no \"1\" in accepted_versions": func(_ *testRealm, d *Delegation) { d.AcceptedVersions = []string{"2"} },
		"shared RSK and CAK point":      func(_ *testRealm, d *Delegation) { d.CAKPublicKeys = d.RSKPublicKeys },
		"an anchor key used as an RSK":  func(r *testRealm, d *Delegation) { d.RSKPublicKeys = [][]byte{r.Anchors.PrimaryA} },
		"another realm":                 func(_ *testRealm, d *Delegation) { d.RealmID = newID() },
		"an unreached generation":       func(_ *testRealm, d *Delegation) { d.ProductRootGen = 1 },
		"an authority point whose pin is in cp_tls_pins": func(r *testRealm, d *Delegation) {
			pin, err := TLSPin(d.AuthorityTLSKeys[0])
			require.NoError(r.t, err)
			d.CPTLSPins = [][]byte{pin}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRealm(t)
			r.addDel(1, nil)
			tr := r.trust(t)
			d := r.del(2, func(d *Delegation) {
				if mut != nil {
					mut(r, d)
				}
			})
			pre := tr.PrecheckDelegation(d.Encode())
			g := r.Gens[0]
			add := tr.AddSigned(r.signDel(d, g.A, g.B))
			if mut == nil {
				require.NoError(t, pre)
				require.NoError(t, add)
				return
			}
			require.ErrorIs(t, pre, ErrPrecheck)
			require.ErrorIs(t, add, ErrIgnored)
		})
	}
}

func TestPrecheckDelegationRefusesARetiredGeneration(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.succeed(nil)
	r.addDel(1, nil)
	tr := r.trust(t)
	require.Equal(t, uint64(1), tr.CurrentGen())
	err := tr.PrecheckDelegation(r.del(2, func(d *Delegation) { d.ProductRootGen = 0 }).Encode())
	require.ErrorIs(t, err, ErrPrecheck)
	require.ErrorContains(t, err, "not the current generation 1")
}

// Two different delegations at one (gen, serial) halt the realm (T4).
func TestPrecheckDelegationRefusesAReusedSerial(t *testing.T) {
	r := newTestRealm(t)
	issued := r.del(1, nil)
	g := r.Gens[0]
	r.Items = append(r.Items, r.signDel(issued, g.A, g.B))
	tr := r.trust(t)

	require.ErrorContains(t, tr.PrecheckDelegation(issued.Encode()), "already issued with these bytes")
	other := issued
	other.TauMs++
	err := tr.PrecheckDelegation(other.Encode())
	require.ErrorIs(t, err, ErrPrecheck)
	require.ErrorContains(t, err, "already issued with other bytes")
	requireHalt(t, tr.AddSigned(r.signDel(other, g.A, g.B)))
}

// VP-1: across a succession T4 no longer compares cutovers, so only the
// precheck stops the first delegation of a new generation lowering it.
func TestPrecheckDelegationRefusesABackwardCutoverAcrossASuccession(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, func(d *Delegation) { d.CutoverAtMs = testT0 })
	r.succeed(nil)
	r.addDel(1, func(d *Delegation) { d.CutoverAtMs = testT0 })
	tr := r.trust(t)

	for name, cut := range map[string]uint64{"below": testT0 - 1, "zero": 0} {
		t.Run(name, func(t *testing.T) {
			d := r.del(2, func(d *Delegation) { d.CutoverAtMs = cut })
			err := tr.PrecheckDelegation(d.Encode())
			require.ErrorIs(t, err, ErrPrecheck)
			require.ErrorContains(t, err, "(VP-1)")
		})
	}
	require.NoError(t, tr.PrecheckDelegation(r.del(2, func(d *Delegation) { d.CutoverAtMs = testT0 }).Encode()))
	require.NoError(t, tr.PrecheckDelegation(r.del(2, func(d *Delegation) { d.CutoverAtMs = testT0 + 1 }).Encode()))

	// The gap the precheck closes: a fresh generation's first delegation with
	// a lower cutover is retained and does not halt.
	fresh := newTestRealm(t)
	fresh.addDel(1, func(d *Delegation) { d.CutoverAtMs = testT0 })
	fresh.succeed(nil)
	fresh.addDel(1, func(d *Delegation) { d.CutoverAtMs = testT0 - 1 })
	require.Nil(t, fresh.trust(t).Halted(), "T4 does not compare cutovers across generations")
}

func TestPrecheckDelegationRefusesMalformedBytesAndANilTrust(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	tr := r.trust(t)
	err := tr.PrecheckDelegation([]byte("not a delegation"))
	require.ErrorIs(t, err, ErrMalformed)
	require.False(t, errors.Is(err, ErrPrecheck))
	var nilTrust *Trust
	require.ErrorIs(t, nilTrust.PrecheckDelegation(r.del(2, nil).Encode()), ErrMalformed)
}

// T4 compares cutovers in serial order, so a lower serial with a higher
// cutover than a held higher serial would halt the realm.
func TestPrecheckDelegationRefusesASerialBelowOneIssued(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.addDel(5, func(d *Delegation) { d.CutoverAtMs = testT0 })
	tr := r.trust(t)
	below := r.del(3, func(d *Delegation) { d.CutoverAtMs = testT0 + 1 })
	err := tr.PrecheckDelegation(below.Encode())
	require.ErrorIs(t, err, ErrPrecheck)
	require.ErrorContains(t, err, "serial 3 is below serial 5")
	g := r.Gens[0]
	requireHalt(t, tr.AddSigned(r.signDel(below, g.A, g.B)))
}

// HW takes the highest serial's min_valid outright, so a lower min_valid on a
// new serial would bring superseded results back (T7).
func TestPrecheckDelegationRefusesALowerMinValid(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.addDel(3, nil) // min_valid 3
	tr := r.trust(t)
	err := tr.PrecheckDelegation(r.del(4, func(d *Delegation) { d.MinValidSerial = 1 }).Encode())
	require.ErrorIs(t, err, ErrPrecheck)
	require.ErrorContains(t, err, "min_valid_serial 1 is below 3")
	require.NoError(t, tr.PrecheckDelegation(r.del(4, func(d *Delegation) { d.MinValidSerial = 3 }).Encode()))
}

// succ is a succession from generation prev of r, before any signature.
func succ(r *testRealm, prev uint64, mut func(*Succession)) Succession {
	s := Succession{RealmID: r.ID, PrevRootGen: prev, NewRootGen: prev + 1,
		NewPrimaryA: pointOf(r.t, testKey(r.t)), NewPrimaryB: pointOf(r.t, testKey(r.t)),
		NewRecoveryA: pointOf(r.t, testKey(r.t)), NewRecoveryB: pointOf(r.t, testKey(r.t))}
	if mut != nil {
		mut(&s)
	}
	return s
}

func signSucc(r *testRealm, s Succession, g testGen) []byte {
	msg := s.Encode()
	require.NotNil(r.t, msg)
	return Signed{Type: "succession", Msg: msg, SigA: signP1363(r.t, g.RA, msg), SigB: signP1363(r.t, g.RB, msg)}.Encode()
}

// The precheck must agree with T2: whatever it passes is accepted as the next
// generation, and whatever T2 ignores, it refuses.
func TestPrecheckSuccessionAgreesWithT2(t *testing.T) {
	cases := map[string]func(*testRealm, *Succession){
		"valid":                 nil,
		"another realm":         func(_ *testRealm, s *Succession) { s.RealmID = newID() },
		"new_root_gen not +1":   func(_ *testRealm, s *Succession) { s.NewRootGen = 2 },
		"a repeated new key":    func(_ *testRealm, s *Succession) { s.NewRecoveryB = s.NewPrimaryA },
		"an earlier anchor key": func(r *testRealm, s *Succession) { s.NewPrimaryA = r.Anchors.RecoveryA },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRealm(t)
			r.addDel(1, nil)
			tr := r.trust(t)
			s := succ(r, 0, func(s *Succession) {
				if mut != nil {
					mut(r, s)
				}
			})
			pre := tr.PrecheckSuccession(s.Encode())
			add := tr.AddSigned(signSucc(r, s, r.Gens[0]))
			if mut == nil {
				require.NoError(t, pre)
				require.NoError(t, add)
				require.Equal(t, uint64(1), tr.CurrentGen())
				return
			}
			require.ErrorIs(t, pre, ErrPrecheck)
			require.ErrorIs(t, add, ErrIgnored)
		})
	}
}

// A second validly signed succession from one generation is a fork (T4).
func TestPrecheckSuccessionRefusesAFork(t *testing.T) {
	r := newTestRealm(t)
	r.addDel(1, nil)
	r.succeed(nil)
	tr := r.trust(t)
	fork := succ(r, 0, nil)
	err := tr.PrecheckSuccession(fork.Encode())
	require.ErrorIs(t, err, ErrPrecheck)
	require.ErrorContains(t, err, "prev_root_gen 0 is not the current generation 1")
	requireHalt(t, tr.AddSigned(signSucc(r, fork, r.Gens[0])))
}

func TestPrecheckSuccessionRefusesMalformedBytesAndANilTrust(t *testing.T) {
	r := newTestRealm(t)
	tr := r.trust(t)
	require.ErrorIs(t, tr.PrecheckSuccession([]byte("x")), ErrMalformed)
	var nilTrust *Trust
	require.ErrorIs(t, nilTrust.PrecheckSuccession(succ(r, 0, nil).Encode()), ErrMalformed)
	a, b := tr.CurrentRecoveries()
	require.Equal(t, r.Anchors.RecoveryA, a)
	require.Equal(t, r.Anchors.RecoveryB, b)
	a, b = nilTrust.CurrentRecoveries()
	require.Nil(t, a)
	require.Nil(t, b)
}
