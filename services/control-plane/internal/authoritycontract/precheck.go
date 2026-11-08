package authoritycontract

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

// ErrPrecheck marks a delegation or succession that must not be signed.
var ErrPrecheck = errors.New("refused before signing")

// PrecheckDelegation is what a signer checks before the first product-root
// signature. nil means that msg, dual-signed by the current
// generation's primaries A and B, would be retained (T3) without halting the
// realm (T4), and does not move the cutover backward (VP-1).
//
// It is T3 without the signatures: the realm, the generation, the §2.5 bounds
// and the §2.4 key separation. Beyond T3 it refuses:
//   - a generation other than CurrentGen: a retired generation's delegation
//     is never used, and an unreached one is ignored;
//   - a (gen, serial) this Trust already holds: other bytes there halt the
//     realm (T4), and the same bytes are already signed;
//   - a serial below one already issued in its generation: T4 halts when a
//     higher serial lowers the cutover, and HW (T5) never selects it;
//   - a min_valid_serial below one already issued in its generation: HW takes
//     the higher serial's min_valid, so a lower one would undo a supersession
//     (D-159) that clients already apply (T7);
//   - a cutover_at_ms below the highest one in any retained delegation of any
//     generation. T4 holds the cutover monotone within a generation only, so
//     across a succession this check is the only one (LD-6, VP-1). A zero
//     cutover after a non-zero one is below it, as T4 reads it.
func (t *Trust) PrecheckDelegation(msg []byte) error {
	if t == nil {
		return malformed("nil trust")
	}
	d, err := DecodeDelegation(msg)
	if err != nil {
		return err
	}
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrPrecheck}, a...)...)
	}
	switch {
	case d.RealmID != t.realm:
		return refuse("realm %s is not the anchors' realm %s", d.RealmID, t.realm)
	case d.ProductRootGen != t.CurrentGen():
		return refuse("product_root_gen %d is not the current generation %d", d.ProductRootGen, t.CurrentGen())
	case !delegationBoundsOK(d):
		return refuse("outside the §2.5 bounds (min_valid_serial, lifetime, tau_ms, delta_ms, g_ms, accepted_versions)")
	case !t.keySeparationOK(d):
		return refuse("breaks §2.4 key separation (a point repeated, an anchor or succession key, or a point whose pin is in cp_tls_pins)")
	}
	if held := t.dels[[2]uint64{d.ProductRootGen, d.Serial}]; len(held) > 0 {
		if slices.ContainsFunc(held, func(r delRec) bool { return bytes.Equal(r.Msg, msg) }) {
			return refuse("generation %d serial %d is already issued with these bytes", d.ProductRootGen, d.Serial)
		}
		return refuse("generation %d serial %d is already issued with other bytes; signing it would halt the realm (T4)",
			d.ProductRootGen, d.Serial)
	}
	for k, recs := range t.dels {
		if k[0] != d.ProductRootGen {
			continue
		}
		if k[1] > d.Serial {
			return refuse("serial %d is below serial %d, already issued in generation %d; T4 would halt on any cutover above that one's, and HW never uses it",
				d.Serial, k[1], k[0])
		}
		for _, r := range recs {
			if r.D.MinValidSerial > d.MinValidSerial {
				return refuse("min_valid_serial %d is below %d, issued at generation %d serial %d; HW would drop it and undo that supersession (T7)",
					d.MinValidSerial, r.D.MinValidSerial, k[0], k[1])
			}
		}
	}
	for k, recs := range t.dels {
		for _, r := range recs {
			if r.D.CutoverAtMs > d.CutoverAtMs {
				return refuse("cutover_at_ms %d is below %d, issued at generation %d serial %d (VP-1)",
					d.CutoverAtMs, r.D.CutoverAtMs, k[0], k[1])
			}
		}
	}
	return nil
}

// PrecheckSuccession is what a signer checks before the first recovery-key
// signature. nil means that msg, signed by the current generation's
// recovery keys A′ and B′, would be accepted (T2) as the next generation.
//
// It refuses a succession that is not from CurrentGen to CurrentGen + 1: a
// second succession from a generation already succeeded is a fork, which
// halts the realm (T4) when both are validly signed. It also refuses new keys
// that repeat each other or any key of an earlier generation (T2).
func (t *Trust) PrecheckSuccession(msg []byte) error {
	if t == nil {
		return malformed("nil trust")
	}
	m, err := DecodeSuccession(msg)
	if err != nil {
		return err
	}
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrPrecheck}, a...)...)
	}
	switch cur := t.CurrentGen(); {
	case m.RealmID != t.realm:
		return refuse("realm %s is not the anchors' realm %s", m.RealmID, t.realm)
	case m.PrevRootGen != cur:
		return refuse("prev_root_gen %d is not the current generation %d; a second succession from one generation halts the realm (T4)", m.PrevRootGen, cur)
	case m.NewRootGen != cur+1:
		return refuse("new_root_gen %d is not %d", m.NewRootGen, cur+1)
	case !t.freshSuccessor(genKeys{m.NewPrimaryA, m.NewPrimaryB, m.NewRecoveryA, m.NewRecoveryB}, cur):
		return refuse("the four new keys must be distinct and new to the realm (T2)")
	}
	return nil
}

// CurrentRecoveries is the current generation's recovery A′ and recovery B′:
// the keys that sign its succession. A nil Trust has none.
func (t *Trust) CurrentRecoveries() (a, b []byte) {
	if t == nil || len(t.gens) == 0 {
		return nil, nil
	}
	g := t.gens[len(t.gens)-1]
	return slices.Clone(g.RA), slices.Clone(g.RB)
}

// CurrentPrimaries is the current generation's primary A and primary B: the
// keys that sign its delegations. A nil Trust has none.
func (t *Trust) CurrentPrimaries() (a, b []byte) {
	if t == nil || len(t.gens) == 0 {
		return nil, nil
	}
	g := t.gens[len(t.gens)-1]
	return slices.Clone(g.PA), slices.Clone(g.PB)
}
