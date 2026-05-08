// Package edge is loom-cloud's edge router: incoming HTTP
// traffic for *.loom.dev gets routed to the right tenant's
// container based on the Host header.
//
// Composes pkg/hosting.SubdomainResolver with a reverse-proxy
// dispatch layer that loom-cloud owns. The framework primitive
// extracts the slug + populates context; this package owns the
// "given a slug, where does the request go" mapping.
//
// Routing strategy in v0.0.1:
//
//   - Slug → port lookup via a SitePortMap (caller-supplied;
//     typically a DB-backed map of slug → host port the
//     Provisioner published).
//   - httputil.NewSingleHostReverseProxy on the resolved port.
//   - Slug not found / Site not Running → 503 with a friendly
//     "site not available" message.
//
// Production strategy (v0.1.0+) replaces the port-map with a
// real service-discovery layer (DNS, K8s service refs, Consul,
// etc.). The interface below stays the same.
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

// SitePortMap is what the edge router queries to translate a
// slug to a backend port on the local host. loom-cloud's
// Provisioner publishes the mapping when a Site goes Running.
// Apps can implement this against any storage (in-memory map
// for single-host dev, SQL table for multi-host production).
type SitePortMap interface {
	// Lookup returns the host port + true when a Running site
	// matches the slug; "", 0, false otherwise. Errors are
	// transport / lookup failures, not "not found".
	Lookup(ctx context.Context, slug string) (port int, ok bool, err error)
}

// MemoryPortMap is the simplest SitePortMap — a synchronised
// map in process memory. Useful for tests + single-host dev.
type MemoryPortMap struct {
	mu    sync.RWMutex
	ports map[string]int
}

// NewMemoryPortMap builds an empty in-memory map.
func NewMemoryPortMap() *MemoryPortMap {
	return &MemoryPortMap{ports: map[string]int{}}
}

// Set registers slug → port. Pass port=0 to unregister.
func (m *MemoryPortMap) Set(slug string, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if port == 0 {
		delete(m.ports, slug)
	} else {
		m.ports[slug] = port
	}
}

// Lookup implements SitePortMap.
func (m *MemoryPortMap) Lookup(_ context.Context, slug string) (int, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	port, ok := m.ports[slug]
	return port, ok, nil
}

// Router is the edge router's http.Handler. Wraps a
// SubdomainResolver around a per-slug reverse proxy.
type Router struct {
	// PortMap resolves slug → backend port. Required.
	PortMap SitePortMap
	// Backend formats a URL given (host, port). Default
	// "http://127.0.0.1:<port>". Override for non-loopback
	// backends (e.g. "http://10.0.<svc>.<port>").
	Backend func(port int) string
}

// Handler returns the http.Handler chain ready to mount on the
// edge listener. baseDomain is the parent domain (e.g.
// "loom.dev") under which tenant subdomains live.
func (r *Router) Handler(baseDomain string) (http.Handler, error) {
	if r.PortMap == nil {
		return nil, fmt.Errorf("edge: PortMap is required")
	}
	resolver := &hosting.SubdomainResolver{
		BaseDomain: baseDomain,
		Lookup: func(ctx context.Context, slug string) (string, bool, error) {
			// hosting.SubdomainResolver expects (tenantID,
			// found, err) — we don't have the tenant ID at
			// the edge layer, so we synthesise one from the
			// slug. The downstream proxy doesn't read the
			// host-tenant context (the tenant's Loom binary
			// has its own auth chain); the resolver is
			// here as a documented hook for future per-
			// tenant edge logic (rate limiting, custom
			// domain routing).
			_, ok, err := r.PortMap.Lookup(ctx, slug)
			if err != nil || !ok {
				return "", false, err
			}
			return slug, true, nil
		},
	}
	mw, err := resolver.Middleware()
	if err != nil {
		return nil, err
	}
	return mw(http.HandlerFunc(r.dispatch)), nil
}

// dispatch is the per-request handler that runs after the
// SubdomainResolver populated context. Looks up the backend
// port and proxies; returns 503 when the site isn't running.
func (r *Router) dispatch(w http.ResponseWriter, req *http.Request) {
	slug, ok := hosting.HostTenantFromContext(req.Context())
	if !ok {
		http.Error(w, "site not available", http.StatusServiceUnavailable)
		return
	}
	port, ok, err := r.PortMap.Lookup(req.Context(), slug)
	if err != nil || !ok {
		http.Error(w, "site not available", http.StatusServiceUnavailable)
		return
	}
	backendURL := r.backendFor(port)
	target, err := url.Parse(backendURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, req)
}

func (r *Router) backendFor(port int) string {
	if r.Backend != nil {
		return r.Backend(port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}
