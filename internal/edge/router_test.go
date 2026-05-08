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

func TestMemoryPortMap_SetAndLookup(t *testing.T) {
	m := NewMemoryPortMap()
	m.Set("acme", 8081)
	port, ok, err := m.Lookup(context.Background(), "acme")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 8081, port)

	// Unset.
	m.Set("acme", 0)
	_, ok, _ = m.Lookup(context.Background(), "acme")
	assert.False(t, ok)
}

func TestRouter_HandlerRequiresPortMap(t *testing.T) {
	_, err := (&Router{}).Handler("loom.dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PortMap is required")
}

func TestRouter_ProxiesToBackend(t *testing.T) {
	// Stand up a fake backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from acme: " + r.URL.Path))
	}))
	defer backend.Close()

	// Extract the port from backend.URL ("http://127.0.0.1:NNNNN").
	pm := NewMemoryPortMap()
	// Backend func captures the test backend's full URL so we
	// don't depend on parsing the port from a string.
	r := &Router{
		PortMap: pm,
		Backend: func(_ int) string { return backend.URL },
	}
	pm.Set("acme", 1) // any non-zero port; Backend ignores it

	h, err := r.Handler("loom.dev")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "http://acme.loom.dev/hello", nil)
	req.Host = "acme.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	assert.Equal(t, "from acme: /hello", string(body))
}

func TestRouter_UnknownSlug503(t *testing.T) {
	r := &Router{PortMap: NewMemoryPortMap()}
	h, _ := r.Handler("loom.dev")
	req := httptest.NewRequest(http.MethodGet, "http://unknown.loom.dev/", nil)
	req.Host = "unknown.loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRouter_ApexHostBypass(t *testing.T) {
	// Apex (loom.dev itself, no subdomain) doesn't match any
	// slug — should also yield 503 since dispatch doesn't see
	// a host-tenant in context.
	r := &Router{PortMap: NewMemoryPortMap()}
	h, _ := r.Handler("loom.dev")
	req := httptest.NewRequest(http.MethodGet, "http://loom.dev/", nil)
	req.Host = "loom.dev"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
