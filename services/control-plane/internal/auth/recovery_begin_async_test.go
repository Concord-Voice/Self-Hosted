package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	h.recoveryProcess = func(string) {
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
}
