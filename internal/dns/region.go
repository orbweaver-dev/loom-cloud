package dns

import (
	"context"
	"errors"
	"fmt"
)

// Region-aware DNS. With multi-region routing (loom-cloud#12), a
// Site is placed in a region and the edge router forwards traffic
// to that region. The matching DNS step is to publish each
// tenant's record at its home region's edge IP, so resolution
// lands users on the right region's edge rather than a single
// global one.
//
// True latency-based GeoDNS — one hostname steered to the nearest
// of N regional IPs per the resolver's location — is a provider
// feature (Cloudflare load-balancing / geo-steering). This package
// lands the Loom-side selection it builds on: map a Site's region
// to an edge IP and publish there.

// Region is one routable region: a name and the public IP of its
// edge server.
type Region struct {
	Name   string
	EdgeIP string
}

// RegionSelector resolves a region name to its edge IP. The host
// can back it with a static table, a config file, or a
// latency/health-aware lookup.
type RegionSelector interface {
	// Select returns the edge IP for region and true, or "" and
	// false when the region is unknown.
	Select(region string) (edgeIP string, ok bool)
}

// RegionTable is a static region-name → edge-IP map implementing
// RegionSelector.
type RegionTable map[string]string

// NewRegionTable builds a RegionTable from a slice of Regions.
func NewRegionTable(regions []Region) RegionTable {
	t := make(RegionTable, len(regions))
	for _, r := range regions {
		if r.Name != "" && r.EdgeIP != "" {
			t[r.Name] = r.EdgeIP
		}
	}
	return t
}

// Select satisfies RegionSelector.
func (t RegionTable) Select(region string) (string, bool) {
	ip, ok := t[region]
	return ip, ok
}

// EnsureSlugInRegion adds (or refreshes) the A record
// `<slug>.<base>` pointing at the edge IP for the given region. An
// empty region falls back to Manager.EdgeIP (single-region
// behaviour). A named region the selector doesn't recognise is an
// error — a misplaced Site must fail loudly rather than silently
// resolve to the wrong (or global) edge.
func (m *Manager) EnsureSlugInRegion(ctx context.Context, slug, region string) error {
	if m.Provider == nil {
		return errors.New("dns: Manager.Provider is nil")
	}
	if m.BaseDomain == "" {
		return errors.New("dns: Manager.BaseDomain is empty")
	}
	if slug == "" {
		return errors.New("dns: slug is required")
	}

	ip, err := m.edgeIPForRegion(region)
	if err != nil {
		return err
	}

	_, err = m.Provider.Upsert(ctx, Record{
		Name:    slug + "." + m.BaseDomain,
		Type:    "A",
		Content: ip,
		TTL:     m.TTL,
	})
	return err
}

// edgeIPForRegion resolves the A-record target for a region.
func (m *Manager) edgeIPForRegion(region string) (string, error) {
	if region == "" || m.Regions == nil {
		if m.EdgeIP == "" {
			return "", errors.New("dns: no EdgeIP for single-region publish")
		}
		return m.EdgeIP, nil
	}
	ip, ok := m.Regions.Select(region)
	if !ok {
		return "", fmt.Errorf("dns: no edge IP for region %q", region)
	}
	return ip, nil
}

// interface check
var _ RegionSelector = RegionTable{}
