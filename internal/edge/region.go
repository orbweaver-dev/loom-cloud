package edge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"github.com/orbweaver-dev/loom/pkg/hosting"
)

// Multi-region routing. A loom-cloud deployment may run an edge
// node in several regions (us-east, eu-west, ...). A request can
// land on any region's edge — anycast/GeoDNS routes the user to
// the *nearest* edge, but the Site they want may be homed in a
// different region. The region-aware Router resolves a slug to its
// home region and, when that isn't the local region, forwards the
// request to that region's edge; otherwise it proxies to the local
// backend exactly like the single-region path.
//
// "Nearest-region selection" itself lives one layer up, in DNS
// (GeoDNS / latency records pointing each user at the closest edge
// IP). This package handles the consequence: an edge that receives
// traffic for a Site it doesn't host forwards to the Site's home
// region rather than 503-ing.

// Location is where a Site lives: the backend port on its home
// host plus that host's Region. Region is "" for single-region
// deployments (the legacy SitePortMap path).
type Location struct {
	Port   int
	Region string
}

// SiteLocator resolves a slug to its Location. It's the region-
// aware superset of SitePortMap: where SitePortMap returns only a
// port, SiteLocator also reports the Site's home region so the
// router can decide local-proxy vs cross-region-forward.
type SiteLocator interface {
	Locate(ctx context.Context, slug string) (loc Location, ok bool, err error)
}

// RegionResolver maps a region name to that region's edge base URL
// (e.g. "eu-west" → "https://edge-eu-west.loom.dev"). The router
// forwards cross-region traffic to this upstream, preserving the
// original Host header so the remote edge re-resolves the slug.
type RegionResolver interface {
	Upstream(region string) (baseURL string, ok bool)
}

// MemoryRegionResolver is a static region → upstream map.
type MemoryRegionResolver map[string]string

// Upstream implements RegionResolver.
func (m MemoryRegionResolver) Upstream(region string) (string, bool) {
	u, ok := m[region]
	return u, ok
}

// MemoryLocator is the in-memory SiteLocator for tests + single-
// host dev. Unlike MemoryPortMap it carries a region per slug.
type MemoryLocator struct {
	mu   sync.RWMutex
	locs map[string]Location
}

// NewMemoryLocator builds an empty locator.
func NewMemoryLocator() *MemoryLocator {
	return &MemoryLocator{locs: map[string]Location{}}
}

// Set registers slug → (port, region). Port 0 unregisters.
func (m *MemoryLocator) Set(slug string, port int, region string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if port == 0 {
		delete(m.locs, slug)
		return
	}
	m.locs[slug] = Location{Port: port, Region: region}
}

// Locate implements SiteLocator.
func (m *MemoryLocator) Locate(_ context.Context, slug string) (Location, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	loc, ok := m.locs[slug]
	return loc, ok, nil
}

// dispatchRegionAware runs after the SubdomainResolver has set the
// slug in context. It proxies locally when the Site is in this
// edge's region, and forwards to the Site's home region otherwise.
//
// Only used when the Router has a Locator and a non-empty
// LocalRegion; the legacy single-region path stays in
// Router.dispatch.
func (r *Router) dispatchRegionAware(w http.ResponseWriter, req *http.Request) {
	slug, ok := hosting.HostTenantFromContext(req.Context())
	if !ok {
		http.Error(w, "site not available", http.StatusServiceUnavailable)
		return
	}
	loc, ok, err := r.Locator.Locate(req.Context(), slug)
	if err != nil || !ok {
		http.Error(w, "site not available", http.StatusServiceUnavailable)
		return
	}

	// Local region (or unplaced) → proxy to the local backend.
	if loc.Region == "" || loc.Region == r.LocalRegion {
		r.proxyTo(w, req, r.backendFor(loc.Port))
		return
	}

	// Remote region → forward to that region's edge. The original
	// Host header is preserved by the reverse proxy, so the remote
	// edge re-resolves the same slug locally.
	if r.Regions == nil {
		http.Error(w, "site in another region (no region resolver configured)", http.StatusServiceUnavailable)
		return
	}
	upstream, ok := r.Regions.Upstream(loc.Region)
	if !ok {
		http.Error(w, fmt.Sprintf("no upstream for region %q", loc.Region), http.StatusServiceUnavailable)
		return
	}
	r.proxyTo(w, req, upstream)
}

// proxyTo reverse-proxies the request to a backend base URL.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, backend string) {
	target, err := url.Parse(backend)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, req)
}
