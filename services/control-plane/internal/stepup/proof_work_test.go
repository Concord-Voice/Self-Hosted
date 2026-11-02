package stepup

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProofWorkTrackerRequiresExplicitArmAndNeverReopens(t *testing.T) {
	var nilTracker *proofWorkTracker
	handle, err := nilTracker.begin()
	require.ErrorIs(t, err, errProofWorkUnavailable)
	require.Nil(t, handle)
	_, err = nilTracker.stop()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = nilTracker.snapshot()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = nilTracker.drain(t.Context())
	require.ErrorIs(t, err, errProofWorkContract)

	tracker := newProofWorkTracker()
	handle, err = tracker.begin()
	require.ErrorIs(t, err, errProofWorkUnavailable)
	require.Nil(t, handle)
	_, err = tracker.stop()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = tracker.snapshot()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = tracker.drain(t.Context())
	require.ErrorIs(t, err, errProofWorkContract)
	require.NoError(t, tracker.arm())
	require.ErrorIs(t, tracker.arm(), errProofWorkContract)
	_, err = tracker.drain(t.Context())
	require.ErrorIs(t, err, errProofWorkContract)
	require.Equal(t, proofWorkSnapshot{}, mustProofWorkSnapshot(t, tracker))

	root, err := tracker.begin()
	require.NoError(t, err)
	require.NotNil(t, root)
	require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	stopped, err := tracker.stop()
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, stopped)
	handle, err = tracker.begin()
	require.ErrorIs(t, err, errProofWorkUnavailable)
	require.Nil(t, handle)
	require.ErrorIs(t, tracker.arm(), errProofWorkContract)
	again, err := tracker.stop()
	require.NoError(t, err)
	require.Equal(t, stopped, again)
	require.NoError(t, root.finish())
	drained, err := tracker.drain(t.Context())
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true}, drained)
	again, err = tracker.stop()
	require.NoError(t, err)
	require.Equal(t, drained, again)
}

func TestProofWorkHandlesRejectNilAndFinishedParentsWithoutChangingCounts(t *testing.T) {
	var nilWork *proofWorkHandle
	_, err := nilWork.child()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = nilWork.effect()
	require.ErrorIs(t, err, errProofWorkContract)
	require.ErrorIs(t, nilWork.finish(), errProofWorkContract)
	var zeroWork proofWorkHandle
	_, err = zeroWork.child()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = zeroWork.effect()
	require.ErrorIs(t, err, errProofWorkContract)
	require.ErrorIs(t, zeroWork.finish(), errProofWorkContract)
	var nilEffect *proofEffectHandle
	require.ErrorIs(t, nilEffect.finish(), errProofWorkContract)
	var zeroEffect proofEffectHandle
	require.ErrorIs(t, zeroEffect.finish(), errProofWorkContract)

	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	require.NoError(t, root.finish())
	_, err = root.child()
	require.ErrorIs(t, err, errProofWorkContract)
	_, err = root.effect()
	require.ErrorIs(t, err, errProofWorkContract)
	require.NoError(t, root.finish(), "a valid handle releases its reference once")
	require.Equal(t, proofWorkSnapshot{}, mustProofWorkSnapshot(t, tracker))
}

func TestProofWorkCopiedHandlesCannotForkOrReleaseReferences(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	effect, err := root.effect()
	require.NoError(t, err)
	copyOfRoot := *root
	child, err := (&copyOfRoot).child()
	require.ErrorIs(t, err, errProofWorkContract)
	require.Nil(t, child)
	copyOfEffectParent, err := (&copyOfRoot).effect()
	require.ErrorIs(t, err, errProofWorkContract)
	require.Nil(t, copyOfEffectParent)
	require.ErrorIs(t, (&copyOfRoot).finish(), errProofWorkContract)
	copyOfEffect := *effect
	require.ErrorIs(t, (&copyOfEffect).finish(), errProofWorkContract)
	require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 2, effectTransactionsOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, root.finish())
	require.NoError(t, effect.finish())
	require.Equal(t, proofWorkSnapshot{}, mustProofWorkSnapshot(t, tracker))
}

func TestProofWorkChildrenAndEffectsSurviveParentsAndStopWithoutZeroGap(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	child, err := root.child()
	require.NoError(t, err)
	effect, err := root.effect()
	require.NoError(t, err)
	grandchild, err := child.child()
	require.NoError(t, err)
	childEffect, err := child.effect()
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 5, effectTransactionsOutstanding: 2}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, root.finish())
	require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 4, effectTransactionsOutstanding: 2}, mustProofWorkSnapshot(t, tracker), "registered children keep proof work live after request return")
	require.NoError(t, child.finish())
	require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 3, effectTransactionsOutstanding: 2}, mustProofWorkSnapshot(t, tracker))
	stopped, err := tracker.stop()
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 3, effectTransactionsOutstanding: 2}, stopped)
	_, err = tracker.begin()
	require.ErrorIs(t, err, errProofWorkUnavailable)
	continuation, err := grandchild.child()
	require.NoError(t, err, "a live child can register follow-on work after roots stop")
	lateEffect, err := grandchild.effect()
	require.NoError(t, err, "a live child can register an effect after roots stop")
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 5, effectTransactionsOutstanding: 3}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, effect.finish())
	require.NoError(t, childEffect.finish())
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 3, effectTransactionsOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, lateEffect.finish())
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 2}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, grandchild.finish())
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, continuation.finish())
	require.NoError(t, root.finish())
	require.NoError(t, child.finish())
	require.NoError(t, effect.finish())
	require.NoError(t, childEffect.finish())
	require.NoError(t, lateEffect.finish())
	require.NoError(t, grandchild.finish())
	require.NoError(t, continuation.finish())
	require.Equal(t, proofWorkSnapshot{stopped: true}, mustProofWorkSnapshot(t, tracker), "duplicate completion cannot underflow either counter")
}

func TestProofWorkBeginStopRaceCountsEveryAcceptedRootAndRefusesLaterRoots(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	prior, err := tracker.begin()
	require.NoError(t, err)
	const contenders = 32
	type result struct {
		handle *proofWorkHandle
		err    error
	}
	started := make(chan struct{})
	results := make(chan result, contenders)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-started
			handle, err := tracker.begin()
			results <- result{handle, err}
		}()
	}
	close(started)
	stopResult, err := tracker.stop()
	require.NoError(t, err)
	require.True(t, stopResult.stopped)
	workers.Wait()
	close(results)
	accepted := []*proofWorkHandle{prior}
	for got := range results {
		if got.err != nil {
			require.ErrorIs(t, got.err, errProofWorkUnavailable)
			require.Nil(t, got.handle)
			continue
		}
		require.NotNil(t, got.handle)
		accepted = append(accepted, got.handle)
	}
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: len(accepted)}, mustProofWorkSnapshot(t, tracker))
	_, err = tracker.begin()
	require.ErrorIs(t, err, errProofWorkUnavailable)
	for _, handle := range accepted {
		require.NoError(t, handle.finish())
	}
	drained, err := tracker.drain(t.Context())
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true}, drained)
}

func TestProofWorkStopRaceStillAcceptsChildOfLiveParent(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	parent, err := tracker.begin()
	require.NoError(t, err)
	start := make(chan struct{})
	type childResult struct {
		handle *proofWorkHandle
		err    error
	}
	children := make(chan childResult, 1)
	stops := make(chan proofWorkDrainResult, 1)
	go func() {
		<-start
		child, err := parent.child()
		children <- childResult{child, err}
	}()
	go func() {
		<-start
		snapshot, err := tracker.stop()
		stops <- proofWorkDrainResult{snapshot, err}
	}()
	close(start)
	child := <-children
	stopped := <-stops
	require.NoError(t, child.err)
	require.NotNil(t, child.handle)
	require.NoError(t, stopped.err)
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 2}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, parent.finish())
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	require.NoError(t, child.handle.finish())
}

func TestProofWorkConcurrentChildRegistrationAndParentFinishNeverLosesReference(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	parent, err := tracker.begin()
	require.NoError(t, err)
	start := make(chan struct{})
	childResult := make(chan struct {
		handle *proofWorkHandle
		err    error
	}, 1)
	finished := make(chan error, 1)
	go func() {
		<-start
		child, err := parent.child()
		childResult <- struct {
			handle *proofWorkHandle
			err    error
		}{child, err}
	}()
	go func() {
		<-start
		finished <- parent.finish()
	}()
	close(start)
	child := <-childResult
	require.NoError(t, <-finished)
	if child.err == nil {
		require.NotNil(t, child.handle)
		require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 1}, mustProofWorkSnapshot(t, tracker), "a child accepted before parent completion keeps the count positive")
		require.NoError(t, child.handle.finish())
	} else {
		require.ErrorIs(t, child.err, errProofWorkContract)
		require.Nil(t, child.handle)
	}
	require.Equal(t, proofWorkSnapshot{}, mustProofWorkSnapshot(t, tracker))
}

func TestProofWorkConcurrentDuplicateFinishesNeverUnderflow(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	effect, err := root.effect()
	require.NoError(t, err)
	_, err = tracker.stop()
	require.NoError(t, err)
	const calls = 32
	start := make(chan struct{})
	results := make(chan error, calls)
	var workers sync.WaitGroup
	for i := 0; i < calls; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			if i%2 == 0 {
				results <- root.finish()
			} else {
				results <- effect.finish()
			}
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, proofWorkSnapshot{stopped: true}, mustProofWorkSnapshot(t, tracker))
}

// Done signals that drain reached its wait path. No timer or sleep is used as
// evidence that a goroutine has started or registered its waiter.
type proofWorkObservedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

// Caller-provided Context methods can block. A tracker must not invoke them
// while holding the mutex needed by snapshot and stop.
type proofWorkBlockingContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *proofWorkBlockingContext) wait() {
	c.once.Do(func() { close(c.entered) })
	<-c.release
}

func (c *proofWorkBlockingContext) Err() error {
	c.wait()
	return c.Context.Err()
}

func (c *proofWorkBlockingContext) Done() <-chan struct{} {
	c.wait()
	return c.Context.Done()
}

func (c *proofWorkObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

type proofWorkDrainResult struct {
	snapshot proofWorkSnapshot
	err      error
}

func proofWorkDrainAsync(ctx context.Context, tracker *proofWorkTracker) <-chan proofWorkDrainResult {
	done := make(chan proofWorkDrainResult, 1)
	go func() {
		snapshot, err := tracker.drain(ctx)
		done <- proofWorkDrainResult{snapshot, err}
	}()
	return done
}

func TestProofWorkDrainKeepsBlockedVerifierAndEffectVisibleToMultipleWaiters(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	effect, err := root.effect()
	require.NoError(t, err)
	verifierEntered := make(chan struct{})
	releaseVerifier := make(chan struct{})
	defer func() {
		select {
		case <-releaseVerifier:
		default:
			close(releaseVerifier)
		}
	}()
	verifierDone := make(chan error, 1)
	go func() {
		close(verifierEntered) // simulated reserve is complete; verifier remains live
		<-releaseVerifier
		verifierDone <- root.finish()
	}()
	<-verifierEntered
	stopped, err := tracker.stop()
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 2, effectTransactionsOutstanding: 1}, stopped)

	contexts := make([]*proofWorkObservedContext, 3)
	results := make([]<-chan proofWorkDrainResult, len(contexts))
	cancels := make([]context.CancelFunc, len(contexts))
	defer func() {
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
	}()
	waitWatchdog, stopWaiting := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopWaiting()
	for i := range contexts {
		ctx, cancel := context.WithCancel(t.Context())
		cancels[i] = cancel
		contexts[i] = &proofWorkObservedContext{Context: ctx, entered: make(chan struct{})}
		results[i] = proofWorkDrainAsync(contexts[i], tracker)
		select {
		case <-contexts[i].entered:
		case <-waitWatchdog.Done():
			t.Fatal("drain did not reach its wait path")
		}
	}
	cancels[0]()
	canceled := <-results[0]
	require.ErrorIs(t, canceled.err, context.Canceled)
	require.Equal(t, proofWorkSnapshot{}, canceled.snapshot, "cancellation supplies no drain certificate")
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 2, effectTransactionsOutstanding: 1}, mustProofWorkSnapshot(t, tracker))
	close(releaseVerifier)
	require.NoError(t, <-verifierDone)
	require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1, effectTransactionsOutstanding: 1}, mustProofWorkSnapshot(t, tracker), "request return does not finish its effect transaction")
	require.NoError(t, effect.finish())
	for _, waiter := range results[1:] {
		got := <-waiter
		require.NoError(t, got.err)
		require.Equal(t, proofWorkSnapshot{stopped: true}, got.snapshot)
	}
}

func TestProofWorkSnapshotAndStopCompleteWhileWorkCallbackIsBlocked(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	defer func() {
		select {
		case <-releaseCallback:
		default:
			close(releaseCallback)
		}
	}()
	callbackDone := make(chan error, 1)
	go func() {
		close(callbackEntered)
		<-releaseCallback
		callbackDone <- root.finish()
	}()
	<-callbackEntered
	// This deadline is only a deadlock guard. The observed state, not elapsed
	// time, is the oracle for a live callback while snapshot and stop execute.
	watchdog, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type observation struct {
		snapshot proofWorkSnapshot
		err      error
	}
	observed := make(chan observation, 1)
	go func() {
		snapshot, err := tracker.snapshot()
		observed <- observation{snapshot, err}
	}()
	select {
	case got := <-observed:
		require.NoError(t, got.err)
		require.Equal(t, proofWorkSnapshot{proofWorkOutstanding: 1}, got.snapshot)
	case <-watchdog.Done():
		close(releaseCallback)
		t.Fatal("snapshot blocked behind caller work")
	}
	stopped := make(chan observation, 1)
	go func() {
		snapshot, err := tracker.stop()
		stopped <- observation{snapshot, err}
	}()
	select {
	case got := <-stopped:
		require.NoError(t, got.err)
		require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, got.snapshot)
	case <-watchdog.Done():
		close(releaseCallback)
		t.Fatal("stop blocked behind caller work")
	}
	close(releaseCallback)
	require.NoError(t, <-callbackDone)
	drained, err := tracker.drain(t.Context())
	require.NoError(t, err)
	require.Equal(t, proofWorkSnapshot{stopped: true}, drained)
}

func TestProofWorkDrainNeverCallsContextUnderTrackerMutex(t *testing.T) {
	tracker := newProofWorkTracker()
	require.NoError(t, tracker.arm())
	root, err := tracker.begin()
	require.NoError(t, err)
	_, err = tracker.stop()
	require.NoError(t, err)
	ctx := &proofWorkBlockingContext{Context: t.Context(), entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-ctx.release:
		default:
			close(ctx.release)
		}
	}()
	drained := proofWorkDrainAsync(ctx, tracker)
	watchdog, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	select {
	case <-ctx.entered:
	case <-watchdog.Done():
		t.Fatal("drain never consulted the supplied context")
	}
	observed := make(chan proofWorkDrainResult, 1)
	go func() {
		snapshot, err := tracker.snapshot()
		observed <- proofWorkDrainResult{snapshot, err}
	}()
	select {
	case got := <-observed:
		require.NoError(t, got.err)
		require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, got.snapshot)
	case <-watchdog.Done():
		t.Fatal("snapshot blocked behind a caller-supplied Context method")
	}
	restopped := make(chan proofWorkDrainResult, 1)
	go func() {
		snapshot, err := tracker.stop()
		restopped <- proofWorkDrainResult{snapshot, err}
	}()
	select {
	case got := <-restopped:
		require.NoError(t, got.err)
		require.Equal(t, proofWorkSnapshot{stopped: true, proofWorkOutstanding: 1}, got.snapshot)
	case <-watchdog.Done():
		t.Fatal("stop blocked behind a caller-supplied Context method")
	}
	close(ctx.release)
	require.NoError(t, root.finish())
	got := <-drained
	require.NoError(t, got.err)
	require.Equal(t, proofWorkSnapshot{stopped: true}, got.snapshot)
	canceled, cancelCanceled := context.WithCancel(t.Context())
	cancelCanceled()
	certificate, err := tracker.drain(canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, proofWorkSnapshot{}, certificate, "canceled drain cannot publish an apparent zero certificate")
}

func mustProofWorkSnapshot(t *testing.T, tracker *proofWorkTracker) proofWorkSnapshot {
	t.Helper()
	snapshot, err := tracker.snapshot()
	require.NoError(t, err)
	require.GreaterOrEqual(t, snapshot.proofWorkOutstanding, 0)
	require.GreaterOrEqual(t, snapshot.effectTransactionsOutstanding, 0)
	require.LessOrEqual(t, snapshot.effectTransactionsOutstanding, snapshot.proofWorkOutstanding)
	return snapshot
}
