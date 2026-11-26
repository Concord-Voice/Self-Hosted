package backuphash

import (
	"sync/atomic"
	"time"
)

// derivationWait is maxDerivationWait except while HoldAllSlots shortens it.
var derivationWait atomic.Int64

func init() { derivationWait.Store(int64(maxDerivationWait)) }

// HoldAllSlots takes every derivation slot, and shortens the slot wait to
// wait, until the returned function frees them. While held, every derivation
// returns ErrBusy after wait, the same path a saturated process takes. It
// exists for tests in other packages that prove a busy check spends nothing;
// production code never calls it.
func HoldAllSlots(wait time.Duration) (release func()) {
	for range MaxConcurrentDerivations {
		derivationSlots <- struct{}{}
	}
	derivationWait.Store(int64(wait))
	return func() {
		derivationWait.Store(int64(maxDerivationWait))
		for range MaxConcurrentDerivations {
			<-derivationSlots
		}
	}
}
