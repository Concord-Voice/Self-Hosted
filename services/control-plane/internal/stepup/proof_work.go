package stepup

import (
	"context"
	"errors"
	"sync"
)

var (
	errProofWorkUnavailable = errors.New("proof work unavailable")
	errProofWorkContract    = errors.New("proof work contract violation")
)

type proofWorkState uint8

const (
	proofWorkUnbound proofWorkState = iota
	proofWorkActive
	proofWorkStopped
)

// This process-local tracker is dormant until a future compatible runtime owner
// binds and arms it. Its counts are not lifecycle or database-work evidence on
// their own, and no production route currently begins work through it.
type proofWorkTracker struct {
	mu      sync.Mutex
	state   proofWorkState
	work    int
	effects int
	changed chan struct{}
}

type proofWorkSnapshot struct {
	stopped                       bool
	proofWorkOutstanding          int
	effectTransactionsOutstanding int
}

// Handles are pointers with self checks so copying a value cannot release a
// second reference. All handle state and counts are guarded by tracker.mu.
type proofWorkHandle struct {
	tracker  *proofWorkTracker
	self     *proofWorkHandle
	finished bool
}

type proofEffectHandle struct {
	tracker  *proofWorkTracker
	self     *proofEffectHandle
	finished bool
}

func newProofWorkTracker() *proofWorkTracker {
	return &proofWorkTracker{changed: make(chan struct{})}
}

// arm is private and has no production caller in this unit. It may transition
// one process generation from unbound to active only once.
func (t *proofWorkTracker) arm() error {
	if t == nil {
		return errProofWorkContract
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != proofWorkUnbound {
		return errProofWorkContract
	}
	t.state = proofWorkActive
	t.notifyLocked()
	return nil
}

func (t *proofWorkTracker) begin() (*proofWorkHandle, error) {
	if t == nil {
		return nil, errProofWorkUnavailable
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != proofWorkActive {
		return nil, errProofWorkUnavailable
	}
	h := &proofWorkHandle{tracker: t}
	h.self = h
	t.work++
	t.notifyLocked()
	return h, nil
}

// stop closes only root admission. Work already admitted may still register a
// continuation or effect before its parent handle finishes.
func (t *proofWorkTracker) stop() (proofWorkSnapshot, error) {
	if t == nil {
		return proofWorkSnapshot{}, errProofWorkContract
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == proofWorkUnbound {
		return proofWorkSnapshot{}, errProofWorkContract
	}
	if t.state == proofWorkActive {
		t.state = proofWorkStopped
		t.notifyLocked()
	}
	return t.snapshotLocked(), nil
}

func (t *proofWorkTracker) snapshot() (proofWorkSnapshot, error) {
	if t == nil {
		return proofWorkSnapshot{}, errProofWorkContract
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == proofWorkUnbound {
		return proofWorkSnapshot{}, errProofWorkContract
	}
	return t.snapshotLocked(), nil
}

// drain waits for this process's registered work only. A canceled caller gets
// no snapshot it could mistake for a drain certificate.
func (t *proofWorkTracker) drain(ctx context.Context) (proofWorkSnapshot, error) {
	if t == nil || ctx == nil {
		return proofWorkSnapshot{}, errProofWorkContract
	}
	for {
		t.mu.Lock()
		if t.state != proofWorkStopped {
			t.mu.Unlock()
			return proofWorkSnapshot{}, errProofWorkContract
		}
		snapshot := t.snapshotLocked()
		changed := t.changed
		t.mu.Unlock()
		// Context implementations are caller-supplied. Check cancellation only
		// after unlocking, including when counts have already reached zero.
		if err := ctx.Err(); err != nil {
			return proofWorkSnapshot{}, err
		}
		if snapshot.proofWorkOutstanding == 0 && snapshot.effectTransactionsOutstanding == 0 {
			return snapshot, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return proofWorkSnapshot{}, ctx.Err()
		}
	}
}

func (h *proofWorkHandle) child() (*proofWorkHandle, error) {
	if h == nil || h.tracker == nil || h.self != h {
		return nil, errProofWorkContract
	}
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.finished {
		return nil, errProofWorkContract
	}
	child := &proofWorkHandle{tracker: t}
	child.self = child
	t.work++
	t.notifyLocked()
	return child, nil
}

// effect registers before transaction acquisition, so waiting for a pooled
// connection is part of the outstanding effect interval.
func (h *proofWorkHandle) effect() (*proofEffectHandle, error) {
	if h == nil || h.tracker == nil || h.self != h {
		return nil, errProofWorkContract
	}
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.finished {
		return nil, errProofWorkContract
	}
	effect := &proofEffectHandle{tracker: t}
	effect.self = effect
	t.work++
	t.effects++
	t.notifyLocked()
	return effect, nil
}

func (h *proofWorkHandle) finish() error {
	if h == nil || h.tracker == nil || h.self != h {
		return errProofWorkContract
	}
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.finished {
		return nil
	}
	if t.work <= 0 {
		return errProofWorkContract
	}
	h.finished = true
	t.work--
	t.notifyLocked()
	return nil
}

func (h *proofEffectHandle) finish() error {
	if h == nil || h.tracker == nil || h.self != h {
		return errProofWorkContract
	}
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.finished {
		return nil
	}
	if t.work <= 0 || t.effects <= 0 {
		return errProofWorkContract
	}
	h.finished = true
	t.work--
	t.effects--
	t.notifyLocked()
	return nil
}

func (t *proofWorkTracker) snapshotLocked() proofWorkSnapshot {
	return proofWorkSnapshot{
		stopped:                       t.state == proofWorkStopped,
		proofWorkOutstanding:          t.work,
		effectTransactionsOutstanding: t.effects,
	}
}

// notifyLocked wakes every drain waiter without retaining a mutex during their
// work. Replacing the channel lets a waiter wait for the next transition.
func (t *proofWorkTracker) notifyLocked() {
	if t.changed != nil {
		close(t.changed)
	}
	t.changed = make(chan struct{})
}
