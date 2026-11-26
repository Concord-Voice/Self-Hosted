package stepup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	sharedCharge, e := shared.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	ownCharge, e := own.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	require.NoError(t, RefundIfBusy(ctx, errors.New("wrong code"), sharedCharge, ownCharge))
	require.NoError(t, RefundIfBusy(ctx, &Error{Status: http.StatusForbidden}, sharedCharge, ownCharge))
	for _, k := range []string{"stepup:test:u1", "stepup:own:u1"} {
		got, err := mr.Get(k)
		require.NoError(t, err)
		require.Equal(t, "1", got, "a judged or failed check keeps its charge")
	}

	ttlBefore := mr.TTL("stepup:test:u1")
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), sharedCharge, ownCharge))
	for _, k := range []string{"stepup:test:u1", "stepup:own:u1"} {
		got, err := mr.Get(k)
		require.NoError(t, err)
		require.Equal(t, "0", got, "a busy check gets its charge back")
	}
	require.Equal(t, ttlBefore, mr.TTL("stepup:test:u1"), "the refund keeps the window")

	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), sharedCharge))
	got, err := mr.Get("stepup:test:u1")
	require.NoError(t, err)
	require.Equal(t, "0", got, "a refund never goes below zero")
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), Charged{}))
	require.False(t, mr.Exists("stepup:test:u2"), "the zero charge refunds nothing")
}

func TestRefundIfBusy_FiveBusyThenCorrectNeverReaches429(t *testing.T) {
	rdb, _ := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()
	for range BudgetLimit + 1 {
		charged, e := b.ConsumeAttempt(ctx, "u1")
		require.Nil(t, e)
		require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), charged))
	}
	require.Nil(t, b.Consume(ctx, "u1"), "busy checks never exhaust the budget")
}

// A busy request can wait past the end of the window it was charged in. Its
// refund must not take back a charge made in the next window, or one request
// straddling each boundary keeps every window short of its limit.
func TestRefundIfBusy_LateRefundLeavesTheNextWindowAlone(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	late, e := b.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	mr.FastForward(BudgetWindow + time.Second)
	require.False(t, mr.Exists("stepup:test:u1"), "the first window has ended")

	_, e = b.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), late))
	got, err := mr.Get("stepup:test:u1")
	require.NoError(t, err)
	require.Equal(t, "1", got, "a refund from an ended window leaves the new window's charge")
}

// A refund inside its own window still applies after time has moved on.
func TestRefundIfBusy_RefundWithinTheWindowAfterTimePasses(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	charged, e := b.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	mr.FastForward(BudgetWindow / 2)
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), charged))
	got, err := mr.Get("stepup:test:u1")
	require.NoError(t, err)
	require.Equal(t, "0", got)
}

// A cleared budget starts a new window: a refund from before the clear must
// not reach a charge made after it.
func TestRefundIfBusy_ClearStartsANewWindow(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	before, e := b.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	require.NoError(t, b.Clear(ctx, "u1"))
	_, e = b.ConsumeAttempt(ctx, "u1")
	require.Nil(t, e)
	require.NoError(t, RefundIfBusy(ctx, BusyError(backuphash.ErrBusy), before))
	got, err := mr.Get("stepup:test:u1")
	require.NoError(t, err)
	require.Equal(t, "1", got)
}
