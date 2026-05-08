// Package dns owns the tenant subdomain lifecycle for loom-cloud.
//
// When a Site is provisioned the cloud needs `<slug>.<base-domain>`
// to resolve to the edge server's public IP. When a Site is
// deprovisioned that record needs to go away so released slugs
// can be reused without ghost DNS entries.
//
// Provider is the abstraction. CloudflareProvider is the default
// production implementation (Cloudflare's DNS API is the path of
// least resistance for the OrbWeaver setup); MemoryProvider is
// the test/dev implementation that just bookkeeps in-process.
//
// Manager wraps a Provider with the bookkeeping that callers
// actually want — "ensure this slug points at me" / "remove this
// slug" — plus an idempotent reconcile loop the cloud runtime
// can call on boot to repair drift.
package dns

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Record is the canonical shape of a tenant DNS entry. TTL is in
// seconds; 0 means "use provider default".
type Record struct {
	Name    string // fully-qualified — "acme.loom.dev"
	Type    string // "A" or "CNAME"
	Content string // IP for A, hostname for CNAME
	TTL     int
	// ID is the provider-assigned record ID; populated by Upsert
	// so Delete can target it without a second lookup.
	ID string
}

// Provider is the contract every DNS backend must satisfy.
//
// Implementations MUST be idempotent: Upsert must succeed when
// the record already exists with the same content (treating it
// as a no-op), and Delete must succeed when the record is
// already gone.
type Provider interface {
	// Upsert creates the record, or updates it in place if a
	// matching Name+Type already exists. Returns the record
	// with ID populated.
	Upsert(ctx context.Context, r Record) (Record, error)

	// Delete removes the record. A "not found" condition is NOT
	// an error — Delete is the idempotent inverse of Upsert.
	Delete(ctx context.Context, name, recordType string) error

	// List returns every record the provider currently holds
	// for this zone. Used by Manager.Reconcile to detect drift.
	List(ctx context.Context) ([]Record, error)
}

// Manager is the higher-level surface most callers want. It
// pins records to a fixed base domain (e.g. "loom.dev") and
// edge IP, so callers only pass slugs and the manager fills in
// the rest.
type Manager struct {
	Provider   Provider
	BaseDomain string // e.g. "loom.dev"
	EdgeIP     string // public IP of the edge server (A record target)
	TTL        int    // 0 = provider default
}

// EnsureSlug adds (or refreshes) the A record `<slug>.<base>` →
// EdgeIP. Safe to call repeatedly — idempotent at the provider
// layer.
func (m *Manager) EnsureSlug(ctx context.Context, slug string) error {
	if err := m.validate(); err != nil {
		return err
	}
	if slug == "" {
		return errors.New("dns: slug is required")
	}
	_, err := m.Provider.Upsert(ctx, Record{
		Name:    slug + "." + m.BaseDomain,
		Type:    "A",
		Content: m.EdgeIP,
		TTL:     m.TTL,
	})
	return err
}

// RemoveSlug deletes the A record for `<slug>.<base>`. No-op if
// the record doesn't exist.
func (m *Manager) RemoveSlug(ctx context.Context, slug string) error {
	if err := m.validate(); err != nil {
		return err
	}
	if slug == "" {
		return errors.New("dns: slug is required")
	}
	return m.Provider.Delete(ctx, slug+"."+m.BaseDomain, "A")
}

// Reconcile compares the supplied list of "slugs that should
// exist" against the provider's current records and fixes both
// directions: missing records are upserted, surplus records
// (anything pointing at our EdgeIP that isn't in the list) are
// deleted.
//
// Records pointing at OTHER IPs are left alone — they may be
// hand-managed apex records, MX entries, or another tenant
// fleet sharing the zone. Reconcile only owns the records it
// would create.
func (m *Manager) Reconcile(ctx context.Context, slugs []string) error {
	if err := m.validate(); err != nil {
		return err
	}
	want := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		want[s+"."+m.BaseDomain] = struct{}{}
	}

	have, err := m.Provider.List(ctx)
	if err != nil {
		return fmt.Errorf("dns: list: %w", err)
	}
	mine := make(map[string]Record, len(have))
	for _, r := range have {
		// Only touch records we own — A records pointing at
		// our EdgeIP. Anything else (MX, apex, foreign IP)
		// is somebody else's problem.
		if r.Type == "A" && r.Content == m.EdgeIP {
			mine[r.Name] = r
		}
	}

	// Surplus → delete.
	for name := range mine {
		if _, ok := want[name]; ok {
			continue
		}
		if err := m.Provider.Delete(ctx, name, "A"); err != nil {
			return fmt.Errorf("dns: delete %s: %w", name, err)
		}
	}

	// Missing → upsert.
	for name := range want {
		if _, ok := mine[name]; ok {
			continue
		}
		_, err := m.Provider.Upsert(ctx, Record{
			Name:    name,
			Type:    "A",
			Content: m.EdgeIP,
			TTL:     m.TTL,
		})
		if err != nil {
			return fmt.Errorf("dns: upsert %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) validate() error {
	if m.Provider == nil {
		return errors.New("dns: Manager.Provider is nil")
	}
	if m.BaseDomain == "" {
		return errors.New("dns: Manager.BaseDomain is empty")
	}
	if m.EdgeIP == "" {
		return errors.New("dns: Manager.EdgeIP is empty")
	}
	return nil
}

// MemoryProvider is the in-process Provider for tests + dev.
//
// Stores records in a map; safe for concurrent use. The unit
// tests for the broader cloud runtime swap a real Provider out
// for this so we don't hit Cloudflare from CI.
type MemoryProvider struct {
	mu      sync.Mutex
	records map[string]Record // key: name+"|"+type
	nextID  int
}

// NewMemoryProvider constructs an empty MemoryProvider.
func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{records: map[string]Record{}}
}

// Upsert satisfies Provider.
func (p *MemoryProvider) Upsert(_ context.Context, r Record) (Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := r.Name + "|" + r.Type
	if existing, ok := p.records[key]; ok {
		r.ID = existing.ID
	} else {
		p.nextID++
		r.ID = fmt.Sprintf("mem-%d", p.nextID)
	}
	p.records[key] = r
	return r, nil
}

// Delete satisfies Provider — not-found is a no-op.
func (p *MemoryProvider) Delete(_ context.Context, name, recordType string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.records, name+"|"+recordType)
	return nil
}

// List satisfies Provider.
func (p *MemoryProvider) List(_ context.Context) ([]Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Record, 0, len(p.records))
	for _, r := range p.records {
		out = append(out, r)
	}
	return out, nil
}
