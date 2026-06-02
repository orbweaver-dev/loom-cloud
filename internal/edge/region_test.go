package edge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryLocator_SetAndLocate(t *testing.T) {
	m := NewMemoryLocator()
	m.Set("acme", 9001, "us-east")

	loc, ok, err := m.Locate(context.Background(), "acme")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 9001, loc.Port)
	assert.Equal(t, "us-east", loc.Region)

	_, ok, _ = m.Locate(context.Background(), "ghost")
	assert.False(t, ok)

	m.Set("acme", 0, "us-east") // unregister
	_, ok, _ = m.Locate(context.Background(), "acme")
	assert.False(t, ok)
}

func TestMemoryRegionResolver(t *testing.T) {
	r := MemoryRegionResolver{"eu-west": "https://edge-eu.loom.dev"}
	u, ok := r.Upstream("eu-west")
	assert.True(t, ok)
	assert.Equal(t, "https://edge-eu.loom.dev", u)
	_, ok = r.Upstream("ap-south")
	assert.False(t, ok)
}

func TestRouter_RegionAware_LocalProxies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local-backend: " + r.URL.Path))
	}))
	defer backend.Close()

	loc := NewMemoryLocator()
	loc.Set("acme", 1, "us-east") // same as LocalRegion
	r := &Router{
		Locator:     loc,
		LocalRegion: "us-east",
		Backend:     func(int) string { return backend.URL },
	}
	h, err := r.Handler("loom.dev")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/hi", nil)
	req.Host = "acme.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	assert.Equal(t, "local-backend: /hi", string(body))
}

func TestRouter_RegionAware_ForwardsToRemoteRegion(t *testing.T) {
	// The remote region's edge records the Host it received — it
	// must be the original slug host so it re-resolves locally.
	var gotHost, gotPath string
	remoteEdge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("served-by-eu"))
	}))
	defer remoteEdge.Close()

	loc := NewMemoryLocator()
	loc.Set("acme", 1, "eu-west") // homed in another region
	r := &Router{
		Locator:     loc,
		LocalRegion: "us-east",
		Regions:     MemoryRegionResolver{"eu-west": remoteEdge.URL},
	}
	h, err := r.Handler("loom.dev")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/path", nil)
	req.Host = "acme.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	assert.Equal(t, "served-by-eu", string(body))
	assert.Equal(t, "acme.loom.dev", gotHost, "original Host preserved for remote re-resolution")
	assert.Equal(t, "/path", gotPath)
}

func TestRouter_RegionAware_NoUpstream503(t *testing.T) {
	loc := NewMemoryLocator()
	loc.Set("acme", 1, "ap-south")
	r := &Router{
		Locator:     loc,
		LocalRegion: "us-east",
		Regions:     MemoryRegionResolver{}, // no route to ap-south
	}
	h, _ := r.Handler("loom.dev")
	req := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/", nil)
	req.Host = "acme.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRouter_LocatorSingleRegion(t *testing.T) {
	// Locator set but LocalRegion empty → single-region mode; the
	// Locator's port is used and region is ignored.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	loc := NewMemoryLocator()
	loc.Set("acme", 1, "eu-west")
	r := &Router{Locator: loc, Backend: func(int) string { return backend.URL }}
	require.False(t, r.regionAware())

	h, err := r.Handler("loom.dev")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/", nil)
	req.Host = "acme.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
