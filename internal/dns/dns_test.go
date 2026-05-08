package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryProvider_UpsertList(t *testing.T) {
	p := NewMemoryProvider()
	ctx := context.Background()

	r1, err := p.Upsert(ctx, Record{Name: "acme.loom.dev", Type: "A", Content: "1.2.3.4"})
	require.NoError(t, err)
	assert.NotEmpty(t, r1.ID)

	// Re-upsert preserves ID.
	r2, err := p.Upsert(ctx, Record{Name: "acme.loom.dev", Type: "A", Content: "1.2.3.5"})
	require.NoError(t, err)
	assert.Equal(t, r1.ID, r2.ID)
	assert.Equal(t, "1.2.3.5", r2.Content)

	all, err := p.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

func TestMemoryProvider_DeleteIsIdempotent(t *testing.T) {
	p := NewMemoryProvider()
	ctx := context.Background()
	require.NoError(t, p.Delete(ctx, "ghost.loom.dev", "A"))
	_, err := p.Upsert(ctx, Record{Name: "x.loom.dev", Type: "A", Content: "1.2.3.4"})
	require.NoError(t, err)
	require.NoError(t, p.Delete(ctx, "x.loom.dev", "A"))
	require.NoError(t, p.Delete(ctx, "x.loom.dev", "A")) // already gone
	all, _ := p.List(ctx)
	assert.Empty(t, all)
}

func TestManager_EnsureSlug_AndRemove(t *testing.T) {
	p := NewMemoryProvider()
	m := &Manager{Provider: p, BaseDomain: "loom.dev", EdgeIP: "203.0.113.10"}
	ctx := context.Background()

	require.NoError(t, m.EnsureSlug(ctx, "acme"))
	all, _ := p.List(ctx)
	require.Len(t, all, 1)
	assert.Equal(t, "acme.loom.dev", all[0].Name)
	assert.Equal(t, "203.0.113.10", all[0].Content)

	require.NoError(t, m.RemoveSlug(ctx, "acme"))
	all, _ = p.List(ctx)
	assert.Empty(t, all)
}

func TestManager_RequiresProvider(t *testing.T) {
	m := &Manager{BaseDomain: "loom.dev", EdgeIP: "1.1.1.1"}
	err := m.EnsureSlug(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Provider")
}

func TestManager_RequiresSlug(t *testing.T) {
	m := &Manager{Provider: NewMemoryProvider(), BaseDomain: "loom.dev", EdgeIP: "1.1.1.1"}
	err := m.EnsureSlug(context.Background(), "")
	require.Error(t, err)
}

func TestManager_Reconcile_AddsMissing_RemovesSurplus(t *testing.T) {
	p := NewMemoryProvider()
	ctx := context.Background()

	// Pre-seed: an old tenant we should remove, plus a foreign
	// record we should leave alone.
	_, _ = p.Upsert(ctx, Record{Name: "stale.loom.dev", Type: "A", Content: "203.0.113.10"})
	_, _ = p.Upsert(ctx, Record{Name: "external.loom.dev", Type: "A", Content: "8.8.8.8"})

	m := &Manager{Provider: p, BaseDomain: "loom.dev", EdgeIP: "203.0.113.10"}
	require.NoError(t, m.Reconcile(ctx, []string{"acme", "wayne"}))

	all, _ := p.List(ctx)
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"acme.loom.dev", "external.loom.dev", "wayne.loom.dev"}, names)
}

func TestManager_Reconcile_NoChangeWhenInSync(t *testing.T) {
	p := NewMemoryProvider()
	ctx := context.Background()
	m := &Manager{Provider: p, BaseDomain: "loom.dev", EdgeIP: "203.0.113.10"}
	require.NoError(t, m.EnsureSlug(ctx, "acme"))

	r1, _ := p.List(ctx)
	require.NoError(t, m.Reconcile(ctx, []string{"acme"}))
	r2, _ := p.List(ctx)
	assert.Equal(t, r1, r2) // no churn
}

// ---- Cloudflare provider tests (httptest, no real API) ----

func TestCloudflareProvider_Upsert_Create(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zones/Z/dns_records" && r.Method == http.MethodGet {
			// findRecord -> not found
			writeOK(w, []cfRecord{})
			return
		}
		if r.URL.Path == "/zones/Z/dns_records" && r.Method == http.MethodPost {
			var body cfRecord
			_ = json.NewDecoder(r.Body).Decode(&body)
			body.ID = "new-id"
			writeOK(w, body)
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
	}))
	defer srv.Close()

	c := &CloudflareProvider{APIToken: "tok", ZoneID: "Z", BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Upsert(context.Background(), Record{Name: "acme.loom.dev", Type: "A", Content: "1.2.3.4"})
	require.NoError(t, err)
	assert.Equal(t, "new-id", got.ID)
}

func TestCloudflareProvider_Upsert_UpdateExisting(t *testing.T) {
	called := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called[r.Method+" "+strings.Split(r.URL.Path, "?")[0]]++
		if r.Method == http.MethodGet {
			writeOK(w, []cfRecord{{ID: "rec-1", Name: "acme.loom.dev", Type: "A", Content: "1.2.3.4", TTL: 1}})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/zones/Z/dns_records/rec-1" {
			var body cfRecord
			_ = json.NewDecoder(r.Body).Decode(&body)
			body.ID = "rec-1"
			writeOK(w, body)
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := &CloudflareProvider{APIToken: "tok", ZoneID: "Z", BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Upsert(context.Background(), Record{Name: "acme.loom.dev", Type: "A", Content: "5.6.7.8"})
	require.NoError(t, err)
	assert.Equal(t, "rec-1", got.ID)
	assert.Equal(t, 1, called["PUT /zones/Z/dns_records/rec-1"])
}

func TestCloudflareProvider_Upsert_NoOpWhenSame(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeOK(w, []cfRecord{{ID: "rec-1", Name: "acme.loom.dev", Type: "A", Content: "1.2.3.4", TTL: 1}})
			return
		}
		if r.Method == http.MethodPut {
			puts++
		}
		writeOK(w, cfRecord{})
	}))
	defer srv.Close()

	c := &CloudflareProvider{APIToken: "tok", ZoneID: "Z", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Upsert(context.Background(), Record{Name: "acme.loom.dev", Type: "A", Content: "1.2.3.4"})
	require.NoError(t, err)
	assert.Equal(t, 0, puts, "no PUT should be issued when content matches")
}

func TestCloudflareProvider_Delete_NotFoundIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeOK(w, []cfRecord{})
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := &CloudflareProvider{APIToken: "tok", ZoneID: "Z", BaseURL: srv.URL, HTTPClient: srv.Client()}
	require.NoError(t, c.Delete(context.Background(), "ghost.loom.dev", "A"))
}

func TestCloudflareProvider_List_Paginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			recs := make([]cfRecord, 100)
			for i := range recs {
				recs[i] = cfRecord{ID: "p1", Name: "n", Type: "A", Content: "1.1.1.1"}
			}
			writeOK(w, recs)
		case "2":
			writeOK(w, []cfRecord{{ID: "p2", Name: "n", Type: "A", Content: "1.1.1.1"}})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	c := &CloudflareProvider{APIToken: "tok", ZoneID: "Z", BaseURL: srv.URL, HTTPClient: srv.Client()}
	all, err := c.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, all, 101)
}

func TestCloudflareProvider_RejectsEmptyConfig(t *testing.T) {
	c := &CloudflareProvider{}
	_, err := c.Upsert(context.Background(), Record{Name: "a", Type: "A", Content: "1.1.1.1"})
	require.Error(t, err)
}

func writeOK(w http.ResponseWriter, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfResponse{Success: true, Result: result})
}
