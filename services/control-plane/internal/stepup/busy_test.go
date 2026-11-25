package stepup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Self-Hosted/services/control-plane/internal/backuphash"
)

func TestVerifyMFAFactor_BusyIs503WithRetryAfter(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, verifyErr: fmt.Errorf("verify backup code: %w", backuphash.ErrBusy)}

	for _, e := range []*Error{
		VerifyMFAFactor(context.Background(), v, "user-1", testPurpose, "ABCD1234", nil),
		VerifyMFAFactorTx(context.Background(), nil, v, "user-1", testPurpose, "ABCD1234", nil),
	} {
		require.NotNil(t, e)
		require.True(t, IsBusy(e), "the busy cause stays reachable through *Error")
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		e.Write(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.Equal(t, "1", w.Header().Get("Retry-After"))
		require.JSONEq(t, `{"error":"Verification is busy. Try again.","verification_busy":true}`, w.Body.String())
	}
}

func TestRefundIfBusy_RefundsOnlyABusyCheck(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	shared := NewBudget(rdb, "stepup:test:")
	own := NewBudget(rdb, "stepup:own:")
	ctx := context.Background()

	require.Nil(t, shared.Consume(ctx, "u1"))
	require.Nil(t, own.Consume(ctx, "u1"))
	require.NoError(t, RefundIfBusy(ctx, errors.New("wrong code"), "u1", shared, own))
	require.NoError(t, RefundIfBusy(ctx, &Error{Status: http.StatusForbidden}, "u1", shared, own))
	for _, k := range []string{"stepup:test:u1", "stepup:own:u1"} {
		got, err := mr.Get(k)
		require.NoError(t, err)
		require.Equal(t, "1", got, "a judged or failed check keeps its charge")
	}

	ttlBefore := mr.TTL("stepup:test:u1")
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), "u1", shared, own))
	for _, k := range []string{"stepup:test:u1", "stepup:own:u1"} {
		got, err := mr.Get(k)
		require.NoError(t, err)
		require.Equal(t, "0", got, "a busy check gets its charge back")
	}
	require.Equal(t, ttlBefore, mr.TTL("stepup:test:u1"), "the refund keeps the window")

	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), "u1", shared))
	got, err := mr.Get("stepup:test:u1")
	require.NoError(t, err)
	require.Equal(t, "0", got, "a refund never goes below zero")
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), "u2", shared))
	require.False(t, mr.Exists("stepup:test:u2"), "a refund never creates a key")
}

func TestRefundIfBusy_FiveBusyThenCorrectNeverReaches429(t *testing.T) {
	rdb, _ := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()
	for range BudgetLimit + 1 {
		require.Nil(t, b.Consume(ctx, "u1"))
		require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), "u1", b))
	}
	require.Nil(t, b.Consume(ctx, "u1"), "busy checks never exhaust the budget")
}
