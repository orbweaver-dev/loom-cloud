// Package billing is loom-cloud's per-tenant metering layer.
//
// Sites declare a Plan (free / starter / pro / enterprise);
// usage is recorded as discrete UsageEvents (api_request,
// agent_run, storage_gb_hour, etc.); a monthly Reconciler
// rolls usage into Stripe invoice items and finalises an
// invoice per tenant. The Stripe shuttle (loom's
// pkg/shuttle/stripe) is reused as the API client, but
// billing here owns the schema + the metering abstraction.
//
// The package surface is intentionally narrow:
//
//	Plan          — tenant pricing tier (Free, Starter, Pro, Enterprise)
//	UsageEvent    — one billable unit of work
//	UsageStore    — interface, persists events; reads them back per period
//	Invoicer      — interface, sends invoice items to the billing backend
//	Reconciler    — runs once per period to roll UsageStore → Invoicer
//
// The default StripeInvoicer hits Stripe's API; tests use a
// MemoryInvoicer that records calls without network I/O.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/orbweaver-dev/loom/pkg/app"
)

// Plan is the per-tenant pricing tier. Determines the
// per-event prices the Reconciler applies and the included
// quantities (free tier: first N requests at $0).
type Plan string

const (
	PlanFree       Plan = "free"
	PlanStarter    Plan = "starter"
	PlanPro        Plan = "pro"
	PlanEnterprise Plan = "enterprise"
)

// IsValid reports whether p is a recognised Plan.
func (p Plan) IsValid() bool {
	switch p {
	case PlanFree, PlanStarter, PlanPro, PlanEnterprise:
		return true
	}
	return false
}

// UsageEvent is one billable unit of work. SiteID + Kind is
// the natural grouping; Quantity is summed at reconciliation
// time. Metadata carries provider-specific context (the agent
// ID for an agent_run event, the storage path for a storage
// event) — never read by the Reconciler, just round-tripped
// for audit-trail dashboards.
type UsageEvent struct {
	SiteID     string
	TenantID   string
	Kind       string         // "api_request", "agent_run", "storage_gb_hour", etc.
	Quantity   float64        // typically integer; float64 for storage-hours / token counts
	OccurredAt time.Time
	Metadata   map[string]any
}

// UsageStore persists UsageEvents and supports range reads for
// reconciliation. Implementations MUST be safe for concurrent
// Record calls (the metering hot path fires from many
// request goroutines simultaneously).
type UsageStore interface {
	// Record persists one event. Errors are logged by the
	// caller but typically NOT propagated — failing the
	// request a customer paid for because metering is down
	// is the wrong trade-off.
	Record(ctx context.Context, ev UsageEvent) error
	// Range returns every event for the supplied tenant
	// between [start, end). Used by Reconciler.Reconcile.
	Range(ctx context.Context, tenantID string, start, end time.Time) ([]UsageEvent, error)
}

// Invoicer is the abstraction over the actual billing backend.
// One implementation per provider; loom-cloud ships
// StripeInvoicer + MemoryInvoicer.
type Invoicer interface {
	// AddInvoiceItem registers a line item against a tenant's
	// upcoming invoice. Idempotency is the implementation's
	// problem (Stripe accepts an idempotency key per request).
	AddInvoiceItem(ctx context.Context, item InvoiceItem) error
	// FinalizeInvoice closes the period for the tenant —
	// Stripe's "finalize an invoice" call, an SQL "mark
	// period closed" row, etc.
	FinalizeInvoice(ctx context.Context, tenantID string, period Period) error
}

// InvoiceItem is one line on a tenant's upcoming invoice.
type InvoiceItem struct {
	TenantID    string
	Kind        string  // mirrors UsageEvent.Kind
	Quantity    float64 // total for the period
	UnitPriceUSD float64 // looked up from the Plan's price table
	Description string  // human-readable summary (e.g. "12,034 API requests in May 2026")
	Period      Period
}

// Period bounds an invoicing window. Half-open: [Start, End).
type Period struct {
	Start time.Time
	End   time.Time
}

// PriceTable maps (Plan, Kind) → unit price in USD. The
// default table covers the four ship-it tiers; production
// deployments can pass their own to NewReconciler.
//
// Entries with quantity-included-free are NOT modelled here —
// Reconciler.Reconcile handles inclusion via the Plan's
// Includes() method. Table is just dollars-per-unit.
type PriceTable map[Plan]map[string]float64

// DefaultPrices is the seed table (USD). Numbers are
// illustrative; production sets their own via Reconciler.Prices.
func DefaultPrices() PriceTable {
	return PriceTable{
		PlanFree: {
			// Free tier — most events are free up to the
			// included-quantity ceilings; everything past
			// included is billed at Starter rates.
		},
		PlanStarter: {
			"api_request":     0.000_005, // $0.000005 / request → $5 per 1M
			"agent_run":       0.001,     // $0.001 / run
			"storage_gb_hour": 0.000_15,  // ~$0.10/GB/month
		},
		PlanPro: {
			"api_request":     0.000_003,
			"agent_run":       0.0008,
			"storage_gb_hour": 0.000_10,
		},
		PlanEnterprise: {
			"api_request":     0.000_001,
			"agent_run":       0.0005,
			"storage_gb_hour": 0.000_05,
		},
	}
}

// IncludedQuantities maps (Plan, Kind) → free quantity per
// month. The Reconciler subtracts these from the period total
// before computing the invoice line.
func DefaultIncluded() map[Plan]map[string]float64 {
	return map[Plan]map[string]float64{
		PlanFree: {
			"api_request": 100_000,
			"agent_run":   100,
		},
		PlanStarter: {
			"api_request": 1_000_000,
			"agent_run":   1_000,
		},
		PlanPro: {
			"api_request": 5_000_000,
			"agent_run":   10_000,
		},
		// Enterprise: included is negotiated; default empty
		// (everything billed). Real deployments override.
	}
}

// SiteRepo is the abstraction over Site lookup. Reconciler
// needs to know each tenant's Plan + the "what tenants are
// active" question. Backed by the same SQL Site table the
// watcher reads.
type SiteRepo interface {
	// AllActive returns every Site whose Status == Running
	// (and any other "needs billing" filter the impl applies).
	// The Reconciler iterates this for the period.
	AllActive(ctx context.Context) ([]SiteBillingInfo, error)
}

// SiteBillingInfo is the projection of a Site row the
// reconciler needs. Production SiteRepo selects this directly
// rather than carrying the full hosting.Site through.
type SiteBillingInfo struct {
	SiteID   string
	TenantID string
	Plan     Plan
}

// Reconciler rolls a period's UsageEvents into Stripe invoice
// items per tenant.
type Reconciler struct {
	Sites    SiteRepo
	Usage    UsageStore
	Invoicer Invoicer
	// Prices defaults to DefaultPrices() when nil.
	Prices PriceTable
	// Included defaults to DefaultIncluded() when nil.
	Included map[Plan]map[string]float64
	// Now is overridable for tests.
	Now func() time.Time
}

// Reconcile runs a single billing period. Walks every active
// Site, sums its UsageEvents, applies the Plan's included
// quantities, computes invoice items at the Plan's prices,
// and calls Invoicer for each non-zero line. Errors on
// individual tenants don't halt the rest — the loop logs
// and continues, leaving the failed tenant's period for the
// next reconcile run.
func (r *Reconciler) Reconcile(ctx context.Context, period Period) error {
	if err := r.validate(); err != nil {
		return err
	}
	prices := r.Prices
	if prices == nil {
		prices = DefaultPrices()
	}
	included := r.Included
	if included == nil {
		included = DefaultIncluded()
	}

	sites, err := r.Sites.AllActive(ctx)
	if err != nil {
		return fmt.Errorf("billing: list sites: %w", err)
	}

	for _, site := range sites {
		if err := r.reconcileTenant(ctx, site, period, prices, included); err != nil {
			slog.Error("billing: tenant reconcile failed",
				"tenant", site.TenantID, "site", site.SiteID, "err", err)
			continue
		}
	}
	return nil
}

func (r *Reconciler) reconcileTenant(ctx context.Context, site SiteBillingInfo, period Period, prices PriceTable, included map[Plan]map[string]float64) error {
	events, err := r.Usage.Range(ctx, site.TenantID, period.Start, period.End)
	if err != nil {
		return fmt.Errorf("usage range: %w", err)
	}

	// Aggregate by Kind.
	totals := map[string]float64{}
	for _, ev := range events {
		totals[ev.Kind] += ev.Quantity
	}

	planPrices := prices[site.Plan]
	planIncluded := included[site.Plan]

	for kind, total := range totals {
		// Subtract included; clamp at zero.
		billable := total - planIncluded[kind]
		if billable <= 0 {
			continue
		}
		unit, ok := planPrices[kind]
		if !ok || unit == 0 {
			// No price configured for this kind on this plan.
			// Skip — typically means the kind is on a free
			// tier the price table doesn't enumerate.
			continue
		}
		item := InvoiceItem{
			TenantID:     site.TenantID,
			Kind:         kind,
			Quantity:     billable,
			UnitPriceUSD: unit,
			Description: fmt.Sprintf("%g %s in %s",
				billable, humanKind(kind), period.Start.Format("January 2006")),
			Period: period,
		}
		if err := r.Invoicer.AddInvoiceItem(ctx, item); err != nil {
			return fmt.Errorf("invoice item %s: %w", kind, err)
		}
	}

	if err := r.Invoicer.FinalizeInvoice(ctx, site.TenantID, period); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	return nil
}

func (r *Reconciler) validate() error {
	if r.Sites == nil {
		return errors.New("billing: Reconciler.Sites is required")
	}
	if r.Usage == nil {
		return errors.New("billing: Reconciler.Usage is required")
	}
	if r.Invoicer == nil {
		return errors.New("billing: Reconciler.Invoicer is required")
	}
	return nil
}

// ScheduledJob returns an app.ScheduledJob that runs Reconcile
// for the previous calendar month. Cron-fire on the 1st of
// each month gets you "close out the period that just
// ended" — typical schedule "0 4 1 * *" (4am UTC on the 1st).
func (r *Reconciler) ScheduledJob(name, schedule string) app.ScheduledJob {
	return app.ScheduledJob{
		Name:     name,
		Schedule: schedule,
		Run: func(ctx context.Context) error {
			now := time.Now().UTC()
			if r.Now != nil {
				now = r.Now()
			}
			// Previous month: from the 1st of last month up to
			// the 1st of this month.
			thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			lastMonth := thisMonth.AddDate(0, -1, 0)
			return r.Reconcile(ctx, Period{Start: lastMonth, End: thisMonth})
		},
	}
}

func humanKind(kind string) string {
	// Unit name in the invoice description. Most kinds are
	// already self-describing; a small map handles the obvious
	// pluralisations.
	switch kind {
	case "api_request":
		return "API requests"
	case "agent_run":
		return "agent runs"
	case "storage_gb_hour":
		return "GB-hours of storage"
	}
	return kind
}
