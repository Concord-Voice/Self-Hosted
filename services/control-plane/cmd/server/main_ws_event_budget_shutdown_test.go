package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type budgetShutdownRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *budgetShutdownRecorder) note(stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, stage)
}
func (r *budgetShutdownRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}
func budgetShutdownAwait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown fixture did not finish %s", label)
	}
}

func TestShutdownEventBudgetCloseOrderAndAllErrorsSurvive(t *testing.T) {
	r := &budgetShutdownRecorder{}
	httpErr := errors.New("HTTP shutdown sentinel")
	engineErr := errors.New("event engine close sentinel")
	metricsErr := errors.New("metrics shutdown sentinel")
	readerErr := errors.New("restricted reader close sentinel")
	var runErr error
	func() {
		// Model the production outer ownership stack without a live runtime.
		defer r.note("shared-client")
		defer r.note("managed-poll")
		runErr = shutdownControlPlane(
			context.Background(),
			func(string, bool) { t.Error("live shutdown unexpectedly abandoned a stage") },
			func() { r.note("cancel") },
			func() error { r.note("http"); return httpErr },
			func() { r.note("attestation") },
			func() { r.note("activity") },
			func() { r.note("presence") },
			func() { r.note("hub") },
			func() error { r.note("event-engine"); return engineErr },
			func() error { r.note("metrics"); return metricsErr },
			func() error { r.note("reader"); return readerErr },
			func() { r.note("nats") },
		)
	}()
	for _, cause := range []error{httpErr, engineErr, metricsErr, readerErr} {
		require.ErrorIs(t, runErr, cause)
	}
	require.Equal(t, []string{"cancel", "http", "attestation", "activity", "presence", "hub", "event-engine", "metrics", "reader", "nats", "managed-poll", "shared-client"}, r.snapshot())
}

func TestShutdownEventBudgetCloseRunsAfterExhaustedHubBudgetAndBlocksCleanup(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "Hub consumes deadline"
		if expired {
			name = "deadline already expired"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if expired {
				cancel()
			}
			r := &budgetShutdownRecorder{}
			hubEntered := make(chan struct{})
			hubRelease := make(chan struct{})
			hubDone := make(chan struct{})
			closeEntered := make(chan struct{})
			closeRelease := make(chan struct{})
			tailStarted := make(chan struct{}, 8)
			done := make(chan struct{})
			var releaseHubOnce, releaseCloseOnce sync.Once
			releaseHub := func() { releaseHubOnce.Do(func() { close(hubRelease) }) }
			releaseClose := func() { releaseCloseOnce.Do(func() { close(closeRelease) }) }
			// Even assertion failures release and JOIN every intentionally blocked
			// stage. No detached test goroutine may leak into the next case.
			t.Cleanup(func() {
				cancel()
				releaseHub()
				releaseClose()
				budgetShutdownAwait(t, done, "coordinator cleanup")
				budgetShutdownAwait(t, hubDone, "abandoned Hub cleanup")
			})
			engineErr := errors.New("blocked event engine close sentinel")
			var runErr error
			tail := func(stage string) { r.note(stage); tailStarted <- struct{}{} }
			go func() {
				defer close(done)
				runErr = func() error {
					defer tail("shared-client")
					defer tail("managed-poll")
					return shutdownControlPlane(
						ctx, func(string, bool) {}, func() {}, func() error { return nil }, func() {}, func() {}, func() {},
						func() { close(hubEntered); <-hubRelease; close(hubDone) },
						func() error {
							r.note("event-engine-enter")
							close(closeEntered)
							<-closeRelease
							r.note("event-engine-return")
							return engineErr
						},
						func() error { tail("metrics"); return nil },
						func() error { tail("reader"); return nil },
						func() { tail("nats") },
					)
				}()
			}()
			if !expired {
				budgetShutdownAwait(t, hubEntered, "Hub entry before exhausting budget")
				cancel()
			}
			budgetShutdownAwait(t, closeEntered, "engine Close despite exhausted Hub budget")
			// A deadline cannot skip Close, detach it, interrupt its wait, or
			// allow downstream runtime/client cleanup to overtake it.
			select {
			case <-tailStarted:
				t.Fatal("tail/outer cleanup ran while engine Close remained blocked")
			case <-done:
				t.Fatal("coordinator returned before engine Close completed")
			case <-time.After(20 * time.Millisecond):
			}
			require.Equal(t, []string{"event-engine-enter"}, r.snapshot())
			releaseClose()
			budgetShutdownAwait(t, done, "coordinator after engine release")
			require.ErrorIs(t, runErr, engineErr)
			require.Equal(t, []string{"event-engine-enter", "event-engine-return", "metrics", "reader", "nats", "managed-poll", "shared-client"}, r.snapshot())
			// The Hub may still be blocked after the coordinator's await budget.
			// Release/join it independently instead of claiming cancellation.
			releaseHub()
			budgetShutdownAwait(t, hubDone, "Hub after explicit release")
		})
	}
}

func TestShutdownEventBudgetCloseSuccessAllowsTail(t *testing.T) {
	calls := 0
	r := &budgetShutdownRecorder{}
	err := shutdownControlPlane(
		context.Background(), func(string, bool) {}, func() {}, func() error { return nil }, func() {}, func() {}, func() {}, func() { r.note("hub") },
		func() error { calls++; r.note("event-engine"); return nil },
		func() error { r.note("metrics"); return nil }, func() error { r.note("reader"); return nil }, func() { r.note("nats") },
	)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, []string{"hub", "event-engine", "metrics", "reader", "nats"}, r.snapshot())
}
