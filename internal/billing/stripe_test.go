package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStripeInvoicer_RequiresAPIKey(t *testing.T) {
	_, err := NewStripeInvoicer("", func(context.Context, string) (string, error) { return "", nil })
	require.Error(t, err)
}

func TestNewStripeInvoicer_RequiresLookup(t *testing.T) {
	_, err := NewStripeInvoicer("sk_test_x", nil)
	require.Error(t, err)
}

func TestStripeInvoicer_AddInvoiceItem_PostsExpectedFields(t *testing.T) {
	var captured url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/invoiceitems", r.URL.Path)
		require.NoError(t, r.ParseForm())
		captured = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ii_x"})
	}))
	defer srv.Close()

	inv, _ := NewStripeInvoicer("sk_test_x", func(_ context.Context, tID string) (string, error) {
		return "cus_" + tID, nil
	})
	inv.BaseURL = srv.URL
	inv.HTTPClient = srv.Client()

	period := Period{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	err := inv.AddInvoiceItem(context.Background(), InvoiceItem{
		TenantID: "acme", Kind: "api_request",
		Quantity: 1_000_000, UnitPriceUSD: 0.000005,
		Description: "1M API requests in May 2026", Period: period,
	})
	require.NoError(t, err)
	assert.Equal(t, "cus_acme", captured.Get("customer"))
	// 1M × $0.000005 = $5 = 500 cents
	assert.Equal(t, "500", captured.Get("amount"))
	assert.Equal(t, "usd", captured.Get("currency"))
	assert.Equal(t, "api_request", captured.Get("metadata[kind]"))
	assert.Equal(t, "acme", captured.Get("metadata[tenant_id]"))
}

func TestStripeInvoicer_AddInvoiceItem_SkipsZeroAmount(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
	}))
	defer srv.Close()

	inv, _ := NewStripeInvoicer("sk_test_x", func(context.Context, string) (string, error) { return "cus_acme", nil })
	inv.BaseURL = srv.URL
	inv.HTTPClient = srv.Client()
	require.NoError(t, inv.AddInvoiceItem(context.Background(), InvoiceItem{
		TenantID: "acme", Quantity: 0, UnitPriceUSD: 5,
	}))
	assert.False(t, called, "zero-quantity items should NOT POST")
}

func TestStripeInvoicer_NoCustomer_SkipsSilently(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	inv, _ := NewStripeInvoicer("sk_test_x", func(context.Context, string) (string, error) { return "", nil })
	inv.BaseURL = srv.URL
	inv.HTTPClient = srv.Client()
	require.NoError(t, inv.AddInvoiceItem(context.Background(), InvoiceItem{
		TenantID: "acme", Quantity: 100, UnitPriceUSD: 0.01,
	}))
	assert.False(t, called, "missing customer should skip silently")
}

func TestStripeInvoicer_FinalizeInvoice_HappyPath(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		switch step {
		case 1:
			assert.Equal(t, "/v1/invoices", r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "in_xyz"})
		case 2:
			assert.Equal(t, "/v1/invoices/in_xyz/finalize", r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "in_xyz", "status": "open"})
		}
	}))
	defer srv.Close()

	inv, _ := NewStripeInvoicer("sk_test_x", func(context.Context, string) (string, error) { return "cus_x", nil })
	inv.BaseURL = srv.URL
	inv.HTTPClient = srv.Client()
	period := Period{Start: time.Now().UTC(), End: time.Now().UTC().AddDate(0, 1, 0)}
	require.NoError(t, inv.FinalizeInvoice(context.Background(), "acme", period))
	assert.Equal(t, 2, step)
}

func TestStripeInvoicer_FinalizeInvoice_StripeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "no payment source"},
		})
	}))
	defer srv.Close()

	inv, _ := NewStripeInvoicer("sk_test_x", func(context.Context, string) (string, error) { return "cus_x", nil })
	inv.BaseURL = srv.URL
	inv.HTTPClient = srv.Client()
	err := inv.FinalizeInvoice(context.Background(), "acme",
		Period{Start: time.Now().UTC(), End: time.Now().UTC().AddDate(0, 1, 0)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no payment source")
}

func TestStripeInvoicer_LookupError_Propagates(t *testing.T) {
	inv, _ := NewStripeInvoicer("sk_test_x",
		func(context.Context, string) (string, error) { return "", assertErr("db down") })
	err := inv.AddInvoiceItem(context.Background(), InvoiceItem{TenantID: "x", Quantity: 1, UnitPriceUSD: 1})
	require.Error(t, err)
}

func TestSQLUsageStore_CreateTableSQL_HasShape(t *testing.T) {
	ddl := CreateTableSQL()
	for _, want := range []string{
		"loom_cloud_usage_events", "PRIMARY KEY",
		"idx_usage_tenant_period", "idx_usage_kind",
		"site_id", "tenant_id", "kind", "quantity", "occurred_at", "metadata",
	} {
		assert.Contains(t, ddl, want)
	}
}

func TestSQLUsageStore_RecordRejectsNilDB(t *testing.T) {
	s := &SQLUsageStore{}
	err := s.Record(context.Background(), UsageEvent{SiteID: "s", TenantID: "t", Kind: "k", Quantity: 1})
	require.Error(t, err)
}

func TestSQLUsageStore_RecordRejectsMissingFields(t *testing.T) {
	// SQLStore with nil DB still validates required fields BEFORE
	// touching the DB. Reduces silent-data-loss footguns.
	s := &SQLUsageStore{DB: nil}
	for _, ev := range []UsageEvent{
		{TenantID: "t", Kind: "k"},                     // no SiteID
		{SiteID: "s", Kind: "k"},                       // no TenantID
		{SiteID: "s", TenantID: "t"},                   // no Kind
	} {
		err := s.Record(context.Background(), ev)
		require.Error(t, err, "ev=%+v", ev)
	}
}

type stringErr string

func (s stringErr) Error() string { return string(s) }
func assertErr(s string) error    { return stringErr(s) }
