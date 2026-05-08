package watcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/orbweaver-dev/loom/pkg/hosting"

	"github.com/orbweaver-dev/loom-cloud/internal/dns"
	"github.com/orbweaver-dev/loom-cloud/internal/edge"
)

// fakeProvisioner is the test double for hosting.Provisioner.
type fakeProvisioner struct {
	provisionCalls    atomic.Int32
	deprovisionCalls  atomic.Int32
	provisionErr      error
	deprovisionErr    error
	statusOnProvision hosting.SiteStatus
}

func (f *fakeProvisioner) Provision(_ context.Context, site *hosting.Site) error {
	f.provisionCalls.Add(1)
	if f.provisionErr != nil {
		site.Status = hosting.SiteFailed
		site.LastError = f.provisionErr.Error()
		return f.provisionErr
	}
	if f.statusOnProvision != "" {
		site.Status = f.statusOnProvision
	} else {
		site.Status = hosting.SiteRunning
	}
	site.LastDeployedAt = time.Now()
	return nil
}

func (f *fakeProvisioner) Deprovision(_ context.Context, site *hosting.Site) error {
	f.deprovisionCalls.Add(1)
	if f.deprovisionErr != nil {
		site.Status = hosting.SiteFailed
		site.LastError = f.deprovisionErr.Error()
		return f.deprovisionErr
	}
	site.Status = hosting.SiteStopped
	return nil
}

func (f *fakeProvisioner) Status(context.Context, *hosting.Site) (hosting.SiteStatus, error) {
	return hosting.SiteRunning, nil
}

func newWatcher(t *testing.T) (*Watcher, *MemorySiteRepo, *fakeProvisioner, *dns.MemoryProvider, *edge.MemoryPortMap) {
	t.Helper()
	repo := NewMemorySiteRepo()
	prov := &fakeProvisioner{}
	dnsProv := dns.NewMemoryProvider()
	dnsMgr := &dns.Manager{Provider: dnsProv, BaseDomain: "loom.dev", EdgeIP: "1.2.3.4"}
	pm := edge.NewMemoryPortMap()
	w := &Watcher{Sites: repo, DNS: dnsMgr, Provisioner: prov, PortMap: pm}
	return w, repo, prov, dnsProv, pm
}

func TestWatcher_RequiresAllDeps(t *testing.T) {
	cases := []struct {
		name string
		w    *Watcher
	}{
		{"no Sites", &Watcher{Provisioner: &fakeProvisioner{}, PortMap: edge.NewMemoryPortMap()}},
		{"no Provisioner", &Watcher{Sites: NewMemorySiteRepo(), PortMap: edge.NewMemoryPortMap()}},
		{"no PortMap", &Watcher{Sites: NewMemorySiteRepo(), Provisioner: &fakeProvisioner{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := tc.w.Start(ctx)
			require.Error(t, err)
		})
	}
}

func TestWatcher_PendingSite_GoesLive(t *testing.T) {
	w, repo, prov, dnsProv, pm := newWatcher(t)

	repo.Insert(&hosting.Site{ID: "s1", Slug: "acme", TenantID: "t1", Image: "img"})

	w.tick(context.Background())

	got := repo.Get("s1")
	assert.Equal(t, hosting.SiteRunning, got.Status, "should transition to Running")
	assert.Equal(t, int32(1), prov.provisionCalls.Load())

	port, ok, _ := pm.Lookup(context.Background(), "acme")
	assert.True(t, ok, "port-map should have an entry for acme")
	assert.Greater(t, port, 0)

	dnsRecs, _ := dnsProv.List(context.Background())
	require.Len(t, dnsRecs, 1)
	assert.Equal(t, "acme.loom.dev", dnsRecs[0].Name)
}

func TestWatcher_DNSFailure_HaltsBeforeProvision(t *testing.T) {
	w, repo, prov, _, _ := newWatcher(t)

	// Swap a failing DNS provider in.
	w.DNS = &dns.Manager{Provider: failingDNS{}, BaseDomain: "loom.dev", EdgeIP: "1.2.3.4"}

	repo.Insert(&hosting.Site{ID: "s1", Slug: "acme", TenantID: "t1"})
	w.tick(context.Background())

	got := repo.Get("s1")
	assert.Equal(t, hosting.SiteFailed, got.Status)
	assert.Contains(t, got.LastError, "dns:")
	assert.Equal(t, int32(0), prov.provisionCalls.Load(), "Provision should NOT run after DNS failure")
}

func TestWatcher_ProvisionFailure_StaysFailed(t *testing.T) {
	w, repo, prov, _, pm := newWatcher(t)
	prov.provisionErr = errors.New("docker: pull failed")

	repo.Insert(&hosting.Site{ID: "s1", Slug: "acme", TenantID: "t1"})
	w.tick(context.Background())

	got := repo.Get("s1")
	assert.Equal(t, hosting.SiteFailed, got.Status)
	assert.Equal(t, "docker: pull failed", got.LastError)

	_, ok, _ := pm.Lookup(context.Background(), "acme")
	assert.False(t, ok, "port-map should NOT publish a failed slug")
}

func TestWatcher_DeprovisioningSite_TearsDown(t *testing.T) {
	w, repo, prov, dnsProv, pm := newWatcher(t)

	// Pretend we're already running, with DNS + port-map populated.
	require.NoError(t, w.DNS.EnsureSlug(context.Background(), "acme"))
	pm.Set("acme", 8080)
	repo.Insert(&hosting.Site{ID: "s1", Slug: "acme", Status: hosting.SiteDeprovisioning})

	w.tick(context.Background())

	got := repo.Get("s1")
	assert.Equal(t, hosting.SiteStopped, got.Status)
	assert.Equal(t, int32(1), prov.deprovisionCalls.Load())

	_, ok, _ := pm.Lookup(context.Background(), "acme")
	assert.False(t, ok, "port-map should drop the slug")

	dnsRecs, _ := dnsProv.List(context.Background())
	assert.Empty(t, dnsRecs, "DNS record should be removed")
}

func TestWatcher_NoDNS_StillProvisions(t *testing.T) {
	w, repo, prov, _, pm := newWatcher(t)
	w.DNS = nil // /etc/hosts setup

	repo.Insert(&hosting.Site{ID: "s1", Slug: "acme", TenantID: "t1"})
	w.tick(context.Background())

	got := repo.Get("s1")
	assert.Equal(t, hosting.SiteRunning, got.Status)
	assert.Equal(t, int32(1), prov.provisionCalls.Load())
	_, ok, _ := pm.Lookup(context.Background(), "acme")
	assert.True(t, ok)
}

func TestWatcher_StartCancellation(t *testing.T) {
	w, _, _, _, _ := newWatcher(t)
	w.Interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	// Let a few ticks happen, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

func TestMemorySiteRepo_PendingFiltersByStatus(t *testing.T) {
	r := NewMemorySiteRepo()
	r.Insert(&hosting.Site{ID: "1", Status: hosting.SitePending})
	r.Insert(&hosting.Site{ID: "2", Status: hosting.SiteRunning})
	r.Insert(&hosting.Site{ID: "3"}) // empty status → defaulted to Pending on Insert

	pending, err := r.Pending(context.Background())
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestMemorySiteRepo_UpdateRequiresExisting(t *testing.T) {
	r := NewMemorySiteRepo()
	err := r.Update(context.Background(), &hosting.Site{ID: "ghost"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

type failingDNS struct{}

func (failingDNS) Upsert(context.Context, dns.Record) (dns.Record, error) {
	return dns.Record{}, errors.New("cloudflare: 503")
}
func (failingDNS) Delete(context.Context, string, string) error { return nil }
func (failingDNS) List(context.Context) ([]dns.Record, error)   { return nil, nil }
