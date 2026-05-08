package billing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mayPeriod() Period {
	return Period{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestPlan_IsValid(t *testing.T) {
	for _, p := range []Plan{PlanFree, PlanStarter, PlanPro, PlanEnterprise} {
		assert.True(t, p.IsValid(), "%s should be valid", p)
	}
	assert.False(t, Plan("bogus").IsValid())
}

func TestMemoryUsageStore_RangeFiltersByTenantAndPeriod(t *testing.T) {
	store := NewMemoryUsageStore()
	ctx := context.Background()

	mid := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	require.NoError(t, store.Record(ctx, UsageEvent{TenantID: "t1", Kind: "api_request", Quantity: 5, OccurredAt: mid}))
	require.NoError(t, store.Record(ctx, UsageEvent{TenantID: "t1", Kind: "api_request", Quantity: 1, OccurredAt: before}))
	require.NoError(t, store.Record(ctx, UsageEvent{TenantID: "t1", Kind: "api_request", Quantity: 1, OccurredAt: after}))
	require.NoError(t, store.Record(ctx, UsageEvent{TenantID: "t2", Kind: "api_request", Quantity: 100, OccurredAt: mid}))

	got, err := store.Range(ctx, "t1", mayPeriod().Start, mayPeriod().End)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 5.0, got[0].Quantity)
}

func TestReconcile_AggregatesAndCharges(t *testing.T) {
	ctx := context.Background()
	usage := NewMemoryUsageStore()
	invoicer := &MemoryInvoicer{}
	sites := NewMemorySiteRepo()

	mid := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	sites.Add(SiteBillingInfo{SiteID: "s1", TenantID: "acme", Plan: PlanStarter})

	// Record 1.5M API requests + 2k agent runs (all over the
	// included tier).
	require.NoError(t, usage.Record(ctx, UsageEvent{TenantID: "acme", Kind: "api_request", Quantity: 1_500_000, OccurredAt: mid}))
	require.NoError(t, usage.Record(ctx, UsageEvent{TenantID: "acme", Kind: "agent_run", Quantity: 2_000, OccurredAt: mid}))

	r := &Reconciler{Sites: sites, Usage: usage, Invoicer: invoicer}
	require.NoError(t, r.Reconcile(ctx, mayPeriod()))

	// Expected: Starter included is 1M api_request + 1k agent_run.
	// Billable: 500k api_request × $0.000005 = $2.50
	//          1k agent_run × $0.001 = $1.00
	require.Len(t, invoicer.Items, 2)
	byKind := map[string]InvoiceItem{}
	for _, it := range invoicer.Items {
		byKind[it.Kind] = it
	}
	assert.Equal(t, 500_000.0, byKind["api_request"].Quantity)
	assert.InDelta(t, 0.000005, byKind["api_request"].UnitPriceUSD, 1e-9)
	assert.Equal(t, 1_000.0, byKind["agent_run"].Quantity)

	require.Len(t, invoicer.Finalized, 1)
	assert.Equal(t, "acme", invoicer.Finalized[0].TenantID)
}

func TestReconcile_BelowIncluded_ChargesNothing(t *testing.T) {
	ctx := context.Background()
	usage := NewMemoryUsageStore()
	invoicer := &MemoryInvoicer{}
	sites := NewMemorySiteRepo()

	sites.Add(SiteBillingInfo{SiteID: "s1", TenantID: "acme", Plan: PlanStarter})
	require.NoError(t, usage.Record(ctx, UsageEvent{TenantID: "acme", Kind: "api_request", Quantity: 50_000, OccurredAt: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)}))

	r := &Reconciler{Sites: sites, Usage: usage, Invoicer: invoicer}
	require.NoError(t, r.Reconcile(ctx, mayPeriod()))

	assert.Empty(t, invoicer.Items, "below included tier should produce no line items")
	assert.Len(t, invoicer.Finalized, 1, "finalize still fires (closes the period)")
}

func TestReconcile_PerTenantIsolation(t *testing.T) {
	ctx := context.Background()
	usage := NewMemoryUsageStore()
	invoicer := &MemoryInvoicer{}
	sites := NewMemorySiteRepo()

	sites.Add(SiteBillingInfo{SiteID: "s1", TenantID: "acme", Plan: PlanStarter})
	sites.Add(SiteBillingInfo{SiteID: "s2", TenantID: "wayne", Plan: PlanPro})

	mid := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	// Each tenant 2M api_request.
	require.NoError(t, usage.Record(ctx, UsageEvent{TenantID: "acme", Kind: "api_request", Quantity: 2_000_000, OccurredAt: mid}))
	require.NoError(t, usage.Record(ctx, UsageEvent{TenantID: "wayne", Kind: "api_request", Quantity: 2_000_000, OccurredAt: mid}))

	r := &Reconciler{Sites: sites, Usage: usage, Invoicer: invoicer}
	require.NoError(t, r.Reconcile(ctx, mayPeriod()))

	require.Len(t, invoicer.Items, 1, "only acme (Starter) is over its included tier; wayne (Pro, 5M included) is under")
	assert.Equal(t, "acme", invoicer.Items[0].TenantID)
	// Starter: 2M - 1M included = 1M billable.
	assert.Equal(t, 1_000_000.0, invoicer.Items[0].Quantity)
	// acme finalized AND wayne finalized (period close fires
	// regardless of whether items were added).
	assert.Len(t, invoicer.Finalized, 2)
}

func TestReconcile_RejectsMissingDeps(t *testing.T) {
	cases := []struct {
		name string
		r    *Reconciler
	}{
		{"no Sites", &Reconciler{Usage: NewMemoryUsageStore(), Invoicer: &MemoryInvoicer{}}},
		{"no Usage", &Reconciler{Sites: NewMemorySiteRepo(), Invoicer: &MemoryInvoicer{}}},
		{"no Invoicer", &Reconciler{Sites: NewMemorySiteRepo(), Usage: NewMemoryUsageStore()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Reconcile(context.Background(), mayPeriod())
			require.Error(t, err)
		})
	}
}

func TestReconciler_ScheduledJob_PreviousMonth(t *testing.T) {
	usage := NewMemoryUsageStore()
	invoicer := &MemoryInvoicer{}
	sites := NewMemorySiteRepo()
	sites.Add(SiteBillingInfo{SiteID: "s1", TenantID: "acme", Plan: PlanFree})

	r := &Reconciler{
		Sites: sites, Usage: usage, Invoicer: invoicer,
		Now: func() time.Time {
			// Pretend it's June 1st 2026.
			return time.Date(2026, 6, 1, 4, 0, 0, 0, time.UTC)
		},
	}
	job := r.ScheduledJob("monthly-billing", "0 4 1 * *")
	require.NoError(t, job.Run(context.Background()))

	// One finalize on acme; period covers May.
	require.Len(t, invoicer.Finalized, 1)
	assert.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), invoicer.Finalized[0].Period.Start)
	assert.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), invoicer.Finalized[0].Period.End)
}

func TestDefaultPrices_AllPlansEnumerated(t *testing.T) {
	prices := DefaultPrices()
	for _, p := range []Plan{PlanFree, PlanStarter, PlanPro, PlanEnterprise} {
		_, ok := prices[p]
		assert.True(t, ok, "DefaultPrices missing entry for %s", p)
	}
}
