package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRecoveryBeginReturnsBeforeAccountSpecificWork(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), "test", nil)
	started := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(func() {
		releaseWorker()
		h.CloseRecoveryJobs()
	})
	h.recoveryProcess = func(addr string) {
		started <- addr
		<-release
	}

	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/begin",
		strings.NewReader(`{"email":"  USER@Example.Test  "}`))
	c.Request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		h.RecoveryBegin(c)
		close(done)
	}()

	select {
	case addr := <-started:
		require.Equal(t, "user@example.test", addr)
	case <-time.After(time.Second):
		t.Fatal("recovery work was not queued")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery response waited for account-specific work")
	}
	require.Equal(t, http.StatusOK, response.Code)
	require.JSONEq(t, `{"message":"If an account exists with that email, a recovery code has been sent"}`, response.Body.String())

	releaseWorker()
}

func TestRecoveryBeginRejectsInvalidBodyWithoutQueueing(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), "test", nil)
	processed := make(chan struct{}, 1)
	h.recoveryProcess = func(string) { processed <- struct{}{} }
	t.Cleanup(h.CloseRecoveryJobs)

	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/recovery/begin", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.RecoveryBegin(c)

	require.Equal(t, http.StatusBadRequest, response.Code)
	// Drain first so a wrongly queued job has run before the check.
	h.CloseRecoveryJobs()
	select {
	case <-processed:
		t.Fatal("invalid recovery body reached the queue")
	default:
	}
}

func TestRecoveryBeginQueueSaturationAndClose(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(nil, nil, logger.NewWithWriter(&logs), "test", nil)
	started := make(chan struct{}, recoveryWorkerCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		h.CloseRecoveryJobs()
	})
	var processedMu sync.Mutex
	processed := map[string]int{}
	h.recoveryProcess = func(addr string) {
		processedMu.Lock()
		processed[addr]++
		processedMu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}

	for range recoveryWorkerCount {
		h.enqueueRecoveryBegin("worker@example.test")
	}
	for range recoveryWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not start")
		}
	}
	for range recoveryQueueCapacity {
		h.enqueueRecoveryBegin("queued@example.test")
	}
	h.enqueueRecoveryBegin("overflow@example.test")
	require.Contains(t, logs.String(), "recovery_queue_saturated")

	releaseOnce.Do(func() { close(release) })
	h.CloseRecoveryJobs()
	h.CloseRecoveryJobs()
	h.enqueueRecoveryBegin("after-close@example.test")

	processedMu.Lock()
	defer processedMu.Unlock()
	require.Zero(t, processed["overflow@example.test"], "a saturated queue must drop the address")
	require.Zero(t, processed["after-close@example.test"], "a closed queue must not accept work")
	require.Equal(t, recoveryWorkerCount+recoveryQueueCapacity,
		processed["worker@example.test"]+processed["queued@example.test"])
}

// A stalled drain must give up so later shutdown stages keep their budget, and
// a job still queued at that point must not run.
func TestCloseRecoveryJobsAbandonsStalledDrain(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(nil, nil, logger.NewWithWriter(&logs), "test", nil)
	h.recoveryDrainTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorkers)
	started := make(chan struct{}, recoveryWorkerCount+1)
	var processed atomic.Int32
	h.recoveryProcess = func(string) {
		processed.Add(1)
		started <- struct{}{}
		<-release
	}

	for range recoveryWorkerCount + 1 {
		h.enqueueRecoveryBegin("stalled@example.test")
	}
	for range recoveryWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not start")
		}
	}

	closed := make(chan struct{})
	go func() {
		h.CloseRecoveryJobs()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery drain did not give up on stalled jobs")
	}
	require.Contains(t, logs.String(), "recovery_drain_abandoned")

	releaseWorkers()
	h.recoveryWorkers.Wait()
	require.Equal(t, int32(recoveryWorkerCount), processed.Load(), "a job still queued at abandonment must be skipped")
}
