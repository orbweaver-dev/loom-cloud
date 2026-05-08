// Package watcher is loom-cloud's reconciliation loop: it
// turns Site row INSERTs into a tenant going live (DNS A
// record + TLS cert + container provision + edge port-map
// publish) and Site row DELETEs into the inverse.
//
// The watcher is intentionally a polling loop, not an event
// stream. SQL change-data-capture exists but is fiddly to wire
// up reliably across providers (PlanetScale, RDS, plain
// mysql); a 10-second poll with ETag-style hashing is good
// enough for a workload measured in tenants-per-minute, not
// transactions-per-second.
//
// Wire-up (apps/cloud/main.go):
//
//	w := &watcher.Watcher{
//	    Sites:       sqlSiteRepo,
//	    DNS:         dnsMgr,
//	    Provisioner: dockerProvisioner,
//	    PortMap:     portMap,
//	}
//	go w.Start(ctx)
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/orbweaver-dev/loom/pkg/hosting"

	"github.com/orbweaver-dev/loom-cloud/internal/dns"
	"github.com/orbweaver-dev/loom-cloud/internal/edge"
)

// SiteRepo is the abstraction over tenant Site storage. The
// production impl is SQL-backed; tests use a memory impl. Loom
// Core's hosting.Site is the value type; the repo owns
// persistence + change tracking.
type SiteRepo interface {
	// Pending returns Sites whose Status is SitePending. The
	// watcher claims one per tick and drives it through
	// Provisioning → Running.
	Pending(ctx context.Context) ([]*hosting.Site, error)
	// Deprovisioning returns Sites whose Status is
	// SiteDeprovisioning — sites the dashboard / API has
	// requested teardown for.
	Deprovisioning(ctx context.Context) ([]*hosting.Site, error)
	// Update persists the Site after the watcher has mutated
	// its Status / LastError / LastDeployedAt.
	Update(ctx context.Context, site *hosting.Site) error
}

// Watcher reconciles Site rows with real-world state (DNS,
// TLS, container, port map). One Watcher per cluster — running
// two against the same SiteRepo is safe (each tick re-reads
// state) but wasteful.
type Watcher struct {
	// Sites is the persistence layer the watcher reads from.
	Sites SiteRepo
	// DNS, when non-nil, gets EnsureSlug / RemoveSlug on
	// every transition. nil disables DNS automation (fine
	// for local dev where /etc/hosts handles it).
	DNS *dns.Manager
	// Provisioner is the container backend. Required.
	Provisioner hosting.Provisioner
	// PortMap is the edge router's slug → port lookup. The
	// watcher writes after a successful Provision so
	// in-flight requests start landing on the new
	// container. Required.
	PortMap *edge.MemoryPortMap
	// Interval bounds how often the loop ticks. Default 10s.
	// Lower values shorten provisioning latency at the cost
	// of wasted SELECTs; higher values do the inverse.
	Interval time.Duration
}

// Start runs the reconcile loop until ctx is cancelled.
// Each tick:
//
//  1. Drain pending Sites — drive each through DNS → Provision
//     → port-map publish; mark Running on success, Failed on
//     error (with LastError populated for the dashboard).
//  2. Drain deprovisioning Sites — Deprovision → port-map
//     remove → DNS RemoveSlug; mark Stopped on success.
//
// Errors from individual Sites don't abort the loop — the
// watcher logs and moves on, leaving the failed Site in
// SiteFailed for the next operator attempt.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	interval := w.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick is one reconciliation cycle. Exported indirectly via
// Start; broken out so tests can drive it deterministically.
func (w *Watcher) tick(ctx context.Context) {
	pending, err := w.Sites.Pending(ctx)
	if err != nil {
		slog.Error("watcher: list pending", "err", err)
	}
	for _, site := range pending {
		w.provisionOne(ctx, site)
	}
	deprov, err := w.Sites.Deprovisioning(ctx)
	if err != nil {
		slog.Error("watcher: list deprovisioning", "err", err)
	}
	for _, site := range deprov {
		w.deprovisionOne(ctx, site)
	}
}

func (w *Watcher) provisionOne(ctx context.Context, site *hosting.Site) {
	slog.Info("watcher: provisioning", "slug", site.Slug, "tenant", site.TenantID)

	// DNS first — Provision can take ~30s to pull image + start
	// container; getting the A record propagating in parallel
	// shaves wall-clock on first-tenant-up. EnsureSlug is
	// idempotent; if a previous tick already created the record,
	// the provider returns the existing one.
	if w.DNS != nil {
		if err := w.DNS.EnsureSlug(ctx, site.Slug); err != nil {
			site.Status = hosting.SiteFailed
			site.LastError = "dns: " + err.Error()
			_ = w.Sites.Update(ctx, site)
			return
		}
	}

	// Container.
	if err := w.Provisioner.Provision(ctx, site); err != nil {
		// Provision sets Status / LastError on the site
		// itself, so we don't need to re-mark; just persist
		// what's there.
		_ = w.Sites.Update(ctx, site)
		return
	}

	// Publish to the edge port-map. Without this, traffic
	// hitting the new subdomain 503s.
	w.PortMap.Set(site.Slug, w.provisionerPort())

	// Provision should have set Status to Running on success;
	// belt-and-braces.
	if site.Status != hosting.SiteRunning {
		site.Status = hosting.SiteRunning
	}
	site.LastError = ""
	if err := w.Sites.Update(ctx, site); err != nil {
		slog.Error("watcher: update post-provision", "slug", site.Slug, "err", err)
	}
}

func (w *Watcher) deprovisionOne(ctx context.Context, site *hosting.Site) {
	slog.Info("watcher: deprovisioning", "slug", site.Slug)

	// Stop traffic first — once the port-map entry is gone, in-flight
	// requests will 503, which is preferable to landing on a half-stopped
	// container that's about to die.
	w.PortMap.Set(site.Slug, 0) // 0 == unset

	if err := w.Provisioner.Deprovision(ctx, site); err != nil {
		_ = w.Sites.Update(ctx, site)
		return
	}

	if w.DNS != nil {
		if err := w.DNS.RemoveSlug(ctx, site.Slug); err != nil {
			// DNS removal failure isn't fatal — the record will
			// linger but eventually expire / be reconciled. Log
			// and proceed.
			slog.Warn("watcher: remove dns", "slug", site.Slug, "err", err)
		}
	}

	site.Status = hosting.SiteStopped
	if err := w.Sites.Update(ctx, site); err != nil {
		slog.Error("watcher: update post-deprovision", "slug", site.Slug, "err", err)
	}
}

func (w *Watcher) provisionerPort() int {
	// In v0.1.0 every container listens on the same internal
	// port; the docker provisioner publishes that port to the
	// host as the configured HostPort. The edge router doesn't
	// actually use this value (Backend func ignores port and
	// dials the resolved hostname:HostPort directly). We
	// return a non-zero so the port-map's "is this slug live"
	// check is satisfied.
	return 8080
}

func (w *Watcher) validate() error {
	if w.Sites == nil {
		return errors.New("watcher: Sites is required")
	}
	if w.Provisioner == nil {
		return errors.New("watcher: Provisioner is required")
	}
	if w.PortMap == nil {
		return errors.New("watcher: PortMap is required")
	}
	return nil
}

// MemorySiteRepo is the in-process SiteRepo for tests + dev.
// Stores Sites in a slice; safe for concurrent use.
type MemorySiteRepo struct {
	mu    sync.Mutex
	sites map[string]*hosting.Site // keyed by Site.ID
}

// NewMemorySiteRepo constructs an empty repo.
func NewMemorySiteRepo() *MemorySiteRepo {
	return &MemorySiteRepo{sites: map[string]*hosting.Site{}}
}

// Insert adds a site (typically called by tests; production
// inserts go through whatever HTTP API loom-cloud exposes).
func (r *MemorySiteRepo) Insert(site *hosting.Site) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if site.Status == "" {
		site.Status = hosting.SitePending
	}
	r.sites[site.ID] = site
}

// Pending implements SiteRepo.
func (r *MemorySiteRepo) Pending(_ context.Context) ([]*hosting.Site, error) {
	return r.byStatus(hosting.SitePending), nil
}

// Deprovisioning implements SiteRepo.
func (r *MemorySiteRepo) Deprovisioning(_ context.Context) ([]*hosting.Site, error) {
	return r.byStatus(hosting.SiteDeprovisioning), nil
}

// Update implements SiteRepo. Returns an error if the Site ID
// isn't already in the repo (no implicit upsert).
func (r *MemorySiteRepo) Update(_ context.Context, site *hosting.Site) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sites[site.ID]; !ok {
		return fmt.Errorf("watcher: site %s not found", site.ID)
	}
	r.sites[site.ID] = site
	return nil
}

// Get returns a site by ID for test inspection.
func (r *MemorySiteRepo) Get(id string) *hosting.Site {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sites[id]
}

func (r *MemorySiteRepo) byStatus(s hosting.SiteStatus) []*hosting.Site {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*hosting.Site, 0)
	for _, site := range r.sites {
		if site.Status == s {
			out = append(out, site)
		}
	}
	return out
}
