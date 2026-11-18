package auth

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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
	h.recoveryProcess = func(job recoveryJob) {
		started <- job.addr
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
	require.JSONEq(t, `{"message":"If an account exists with that email, a recovery email has been sent","format":"code"}`, response.Body.String())

	releaseWorker()
}

func TestRecoveryBeginRejectsInvalidBodyWithoutQueueing(t *testing.T) {
	h := NewHandler(nil, nil, logger.New("test"), "test", nil)
	processed := make(chan struct{}, 1)
	h.recoveryProcess = func(recoveryJob) { processed <- struct{}{} }
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
	h.recoveryProcess = func(job recoveryJob) {
		processedMu.Lock()
		processed[job.addr]++
		processedMu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}

	for range recoveryWorkerCount {
		h.enqueueRecoveryBegin(recoveryJob{addr: "worker@example.test", format: recoveryFormatCode})
	}
	for range recoveryWorkerCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("recovery worker did not start")
		}
	}
	for range recoveryQueueCapacity {
		h.enqueueRecoveryBegin(recoveryJob{addr: "queued@example.test", format: recoveryFormatCode})
	}
	h.enqueueRecoveryBegin(recoveryJob{addr: "overflow@example.test", format: recoveryFormatCode})
	require.Contains(t, logs.String(), "recovery_queue_saturated")

	releaseOnce.Do(func() { close(release) })
	h.CloseRecoveryJobs()
	h.CloseRecoveryJobs()
	h.enqueueRecoveryBegin(recoveryJob{addr: "after-close@example.test", format: recoveryFormatCode})

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
	h.recoveryProcess = func(recoveryJob) {
		processed.Add(1)
		started <- struct{}{}
		<-release
	}

	for range recoveryWorkerCount + 1 {
		h.enqueueRecoveryBegin(recoveryJob{addr: "stalled@example.test", format: recoveryFormatCode})
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

func newRecoveryRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mini
}

// An abandoned drain clears every code still awaiting delivery while Redis is
// open, and a delivery that starts afterwards sends nothing.
func TestAbandonedRecoveryDrainClearsUnsentCodes(t *testing.T) {
	rdb, mini := newRecoveryRedis(t)
	var logs bytes.Buffer
	h := NewHandler(nil, rdb, logger.NewWithWriter(&logs), "test", nil)
	h.recoveryDrainTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorkers)
	started := make(chan struct{}, 1)
	h.recoveryProcess = func(recoveryJob) {
		started <- struct{}{}
		<-release
	}

	const inflightAddr = "inflight@example.test"
	const lateAddr = "late@example.test"
	inflight := &recoveryDelivery{addr: inflightAddr, secret: "123456", format: recoveryFormatToken, minutes: 15,
		issue: testRecoveryIssue("u1", "s1", recoveryTokenKeyPrefix+"inflight")}
	late := &recoveryDelivery{addr: lateAddr, secret: "654321", format: recoveryFormatCode, minutes: 15,
		issue: testRecoveryIssue("u2", "s2", recoveryRedisKey(lateAddr))}
	for _, d := range []*recoveryDelivery{inflight, late} {
		_, held, err := h.issueRecoveryRecord(t.Context(), d.issue)
		require.NoError(t, err)
		require.False(t, held)
	}
	h.recoveryInflightMu.Lock()
	h.recoveryInflight[inflight] = struct{}{}
	h.recoveryInflightMu.Unlock()

	h.enqueueRecoveryBegin(recoveryJob{addr: "stalled@example.test", format: recoveryFormatCode})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recovery worker did not start")
	}
	h.CloseRecoveryJobs()
	require.Contains(t, logs.String(), "recovery_drain_abandoned")
	require.False(t, mini.Exists(inflight.issue.RecordKey), "an in-flight key must be cleared on abandonment")
	require.False(t, mini.Exists(inflight.issue.SlotKey), "an in-flight send must release its slot")
	sends, err := mini.Get(inflight.issue.SendsKey)
	require.NoError(t, err)
	require.Equal(t, "0", sends, "an unsent email is refunded")

	h.deliverRecoveryCode(late)
	require.False(t, mini.Exists(late.issue.RecordKey), "a delivery after abandonment must clear its code")
	require.False(t, mini.Exists(late.issue.SlotKey))
	require.NotContains(t, logs.String(), "Failed to send recovery code email", "a delivery after abandonment must not try SMTP")
}

// Recovery failure logs name a failure class and never the address.
func TestRecoveryFailureLogsOmitAddress(t *testing.T) {
	const addr = "private-person@example.test"
	broken, err := sql.Open("postgres", "postgres://127.0.0.1:1/unused")
	require.NoError(t, err)
	t.Cleanup(func() { _ = broken.Close() })
	rdb, mini := newRecoveryRedis(t)
	var logs bytes.Buffer
	h := NewHandler(broken, rdb, logger.NewWithWriter(&logs), "test", nil)
	t.Cleanup(h.CloseRecoveryJobs)

	h.processRecoveryBegin(recoveryJob{addr: addr, format: recoveryFormatToken})
	mini.Close()
	h.deliverRecoveryCode(&recoveryDelivery{addr: addr, secret: "123456", format: recoveryFormatToken, issue: testRecoveryIssue("u", "s", recoveryTokenKeyPrefix+"x")})

	out := logs.String()
	require.Contains(t, out, "failure_class=recovery_account_lookup")
	require.Contains(t, out, "failure_class=recovery_email_delivery")
	require.Contains(t, out, "failure_class=recovery_unsent_code_cleanup")
	require.NotContains(t, out, addr)
}
