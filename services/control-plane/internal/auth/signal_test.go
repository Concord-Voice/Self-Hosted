package auth

import "testing"

// regression for #3290
//
// Exhaustive over {"", "a", "b"}^2. The only false verdict is
// both-known-and-disagreeing; every absence is a pass, because absence is not
// evidence. If a future change makes an absent value compare as a mismatch,
// this table is what fails.
func TestSignalMatch(t *testing.T) {
	cases := []struct {
		name              string
		stored, presented string
		wantKnown, wantOK bool
	}{
		{"neither side knows", "", "", false, true},
		{"never learned it", "", "a", false, true},
		{"caller did not present it", "a", "", false, true},
		{"both known and agree", "a", "a", true, true},
		{"both known and disagree", "a", "b", true, false},
		{"both known and disagree, reversed", "b", "a", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			known, ok := signalMatch(c.stored, c.presented)
			if known != c.wantKnown || ok != c.wantOK {
				t.Errorf("signalMatch(%q, %q) = (known=%v, ok=%v), want (known=%v, ok=%v)",
					c.stored, c.presented, known, ok, c.wantKnown, c.wantOK)
			}
		})
	}
}
