package billing

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"
)

func TestChangeTiming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, oldPlan, oldInterval, newPlan, newInterval, want string
		wantErr                                                bool
	}{
		{"upgrade monthly", PlanSupersonic, "month", PlanWing, "month", "immediate", false},
		{"upgrade yearly", PlanSupersonic, "year", PlanWing, "year", "immediate", false},
		{"month to year", PlanSupersonic, "month", PlanSupersonic, "year", "immediate", false},
		{"upgrade and month to year", PlanSupersonic, "month", PlanWing, "year", "immediate", false},
		{"downgrade monthly", PlanWing, "month", PlanSupersonic, "month", "scheduled", false},
		{"year to month", PlanWing, "year", PlanWing, "month", "scheduled", false},
		{"upgrade and year to month", PlanSupersonic, "year", PlanWing, "month", "immediate_then_scheduled", false},
		{"downgrade and month to year", PlanWing, "month", PlanSupersonic, "year", "scheduled", false},
		{"same price", PlanWing, "year", PlanWing, "year", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := changeTiming(tt.oldPlan, tt.oldInterval, tt.newPlan, tt.newInterval)
			if tt.wantErr {
				require.ErrorIs(t, err, errUnchangeable)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClassifySubscriberAmount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		amount   int64
		coupon   string
		currency stripe.Currency
		want     sql.NullInt64
	}{
		{"regular", 899, "", stripe.CurrencyUSD, sql.NullInt64{Int64: 899, Valid: true}},
		{"founders rounds down", 899, CouponFoundersID, stripe.CurrencyUSD, sql.NullInt64{Int64: 449, Valid: true}},
		{"other currency omitted", 899, "", stripe.CurrencyEUR, sql.NullInt64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub := validClassifySub()
			sub.Items.Data[0].Price.UnitAmount = tc.amount
			sub.Items.Data[0].Price.Currency = tc.currency
			if tc.coupon != "" {
				sub.Discounts = []*stripe.Discount{{Source: &stripe.DiscountSource{Coupon: &stripe.Coupon{ID: tc.coupon}}}}
			}
			got, err := classify(sub)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.UnitAmount)
		})
	}
}

func TestParsePendingAt(t *testing.T) {
	t.Parallel()
	got, valid := parsePendingAt("1900000000")
	require.True(t, valid)
	assert.Equal(t, time.Unix(1900000000, 0).UTC(), got)
	for _, bad := range []string{"", "0", "bad", "1900000000 trailing"} {
		_, valid := parsePendingAt(bad)
		require.False(t, valid)
	}
}

func TestClassifyPendingChange(t *testing.T) {
	fake := newFakeStripe(t)
	fake.schedules["sub_sched_test"] = &fakeSchedule{
		ID: "sub_sched_test", Object: "subscription_schedule", Status: "active",
		Metadata: map[string]string{planChangeMarker: "v1", planChangePlan: PlanWing,
			planChangeInterval: "month", planChangeAt: "1900000000"},
	}
	fake.schedules["sub_sched_test"].CurrentPhase.EndDate = 1900000000
	svc := NewService(Deps{Stripe: fake.client()})
	svc.now = func() time.Time { return time.Unix(1800000000, 0) }
	fields := rowFields{ScheduleID: sql.NullString{String: "sub_sched_test", Valid: true}}
	require.NoError(t, svc.classifyPendingChange(t.Context(), &fields))
	assert.Equal(t, PlanWing, fields.PendingPlan.String)
	assert.Equal(t, "month", fields.PendingInterval.String)
	assert.Equal(t, time.Unix(1900000000, 0).UTC(), fields.PendingChangeAt.Time)

	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"foreign schedule", func() { fake.schedules["sub_sched_test"].Metadata[planChangeMarker] = "" }},
		{"past date", func() { svc.now = func() time.Time { return time.Unix(2000000000, 0) } }},
		{"invalid target", func() { fake.schedules["sub_sched_test"].Metadata[planChangePlan] = "unknown" }},
		{"inactive schedule", func() { fake.schedules["sub_sched_test"].Status = "released" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake.schedules["sub_sched_test"].Status = "active"
			fake.schedules["sub_sched_test"].Metadata[planChangeMarker] = "v1"
			fake.schedules["sub_sched_test"].Metadata[planChangePlan] = PlanWing
			svc.now = func() time.Time { return time.Unix(1800000000, 0) }
			tc.change()
			f := rowFields{ScheduleID: fields.ScheduleID}
			require.NoError(t, svc.classifyPendingChange(t.Context(), &f))
			assert.False(t, f.PendingPlan.Valid)
		})
	}
	f := rowFields{}
	require.NoError(t, svc.classifyPendingChange(t.Context(), &f))
	assert.False(t, f.PendingPlan.Valid)
	fake.failOn("GET", "/v1/subscription_schedules/sub_sched_test", 500, serverErrorBody)
	f.ScheduleID = fields.ScheduleID
	require.Error(t, svc.classifyPendingChange(t.Context(), &f))
}
