package billing

import (
	"context"
	"sync"
	"time"
)

// MemoryUsageStore is the in-process UsageStore for tests + dev.
type MemoryUsageStore struct {
	mu     sync.Mutex
	events []UsageEvent
}

// NewMemoryUsageStore constructs an empty store.
func NewMemoryUsageStore() *MemoryUsageStore {
	return &MemoryUsageStore{}
}

// Record satisfies UsageStore.
func (m *MemoryUsageStore) Record(_ context.Context, ev UsageEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	m.events = append(m.events, ev)
	return nil
}

// Range satisfies UsageStore.
func (m *MemoryUsageStore) Range(_ context.Context, tenantID string, start, end time.Time) ([]UsageEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []UsageEvent
	for _, ev := range m.events {
		if ev.TenantID != tenantID {
			continue
		}
		if !ev.OccurredAt.Before(end) && !ev.OccurredAt.Equal(end) || ev.OccurredAt.Before(start) {
			// outside [start, end)
		}
		if ev.OccurredAt.Before(start) || !ev.OccurredAt.Before(end) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// MemoryInvoicer captures invoice calls without network I/O.
// Useful for tests + dry-run reconciliation passes.
type MemoryInvoicer struct {
	mu        sync.Mutex
	Items     []InvoiceItem
	Finalized []TenantPeriod
}

// TenantPeriod is what FinalizeInvoice calls record.
type TenantPeriod struct {
	TenantID string
	Period   Period
}

// AddInvoiceItem satisfies Invoicer.
func (m *MemoryInvoicer) AddInvoiceItem(_ context.Context, item InvoiceItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Items = append(m.Items, item)
	return nil
}

// FinalizeInvoice satisfies Invoicer.
func (m *MemoryInvoicer) FinalizeInvoice(_ context.Context, tenantID string, period Period) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Finalized = append(m.Finalized, TenantPeriod{TenantID: tenantID, Period: period})
	return nil
}

// MemorySiteRepo is the in-memory SiteRepo for tests.
type MemorySiteRepo struct {
	mu    sync.Mutex
	sites []SiteBillingInfo
}

// NewMemorySiteRepo constructs an empty repo.
func NewMemorySiteRepo() *MemorySiteRepo { return &MemorySiteRepo{} }

// Add registers a tenant for billing.
func (m *MemorySiteRepo) Add(s SiteBillingInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sites = append(m.sites, s)
}

// AllActive satisfies SiteRepo.
func (m *MemorySiteRepo) AllActive(_ context.Context) ([]SiteBillingInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SiteBillingInfo, len(m.sites))
	copy(out, m.sites)
	return out, nil
}
