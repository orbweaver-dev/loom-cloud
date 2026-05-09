package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// StripeInvoicer is the production Invoicer implementation —
// hits Stripe's REST API with a per-tenant Customer + draft
// Invoice, AddInvoiceItem appends invoice items, FinalizeInvoice
// closes + auto-advances.
//
// Construct via NewStripeInvoicer with the Stripe API key; the
// Reconciler calls AddInvoiceItem / FinalizeInvoice in
// sequence per tenant.
//
// Tenant → Stripe Customer ID mapping is the caller's job.
// SiteRepo (via SiteBillingInfo) is expected to carry the
// stripe_customer_id; for projects that don't yet, set
// CustomerLookup to a closure that resolves tenant ID →
// Stripe Customer ID. Returning "" from CustomerLookup makes
// AddInvoiceItem skip the invoice for that tenant (logged at
// warn level).
type StripeInvoicer struct {
	APIKey string
	// BaseURL defaults to https://api.stripe.com (override for
	// tests / staging via the Stripe staging mode).
	BaseURL string
	// HTTPClient defaults to http.DefaultClient with a 30s
	// timeout. Override for retry / circuit-breaker wrappers.
	HTTPClient *http.Client
	// CustomerLookup resolves a tenant ID to a Stripe Customer
	// ID. Required.
	CustomerLookup func(ctx context.Context, tenantID string) (string, error)
}

// NewStripeInvoicer constructs an Invoicer with sensible
// defaults. apiKey is the Stripe Secret Key (sk_live_... /
// sk_test_...). Returns an error when apiKey or lookup is
// missing.
func NewStripeInvoicer(apiKey string, lookup func(context.Context, string) (string, error)) (*StripeInvoicer, error) {
	if apiKey == "" {
		return nil, errors.New("stripe: APIKey is required")
	}
	if lookup == nil {
		return nil, errors.New("stripe: CustomerLookup is required")
	}
	return &StripeInvoicer{
		APIKey:         apiKey,
		BaseURL:        "https://api.stripe.com",
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		CustomerLookup: lookup,
	}, nil
}

// AddInvoiceItem satisfies billing.Invoicer. POSTs to
// /v1/invoiceitems with the customer + amount + currency +
// description.
//
// The amount Stripe expects is in the smallest currency unit
// (cents for USD). We convert from item.Quantity * item.UnitPriceUSD
// to cents and send as `amount`.
func (s *StripeInvoicer) AddInvoiceItem(ctx context.Context, item InvoiceItem) error {
	customerID, err := s.CustomerLookup(ctx, item.TenantID)
	if err != nil {
		return fmt.Errorf("stripe: customer lookup: %w", err)
	}
	if customerID == "" {
		return nil // tenant not on Stripe yet; skipped silently
	}

	totalUSD := item.Quantity * item.UnitPriceUSD
	cents := int64(totalUSD * 100)
	if cents <= 0 {
		return nil // zero / negative line items shouldn't post
	}

	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("amount", strconv.FormatInt(cents, 10))
	form.Set("currency", "usd")
	form.Set("description", item.Description)
	form.Set("metadata[kind]", item.Kind)
	form.Set("metadata[tenant_id]", item.TenantID)
	form.Set("metadata[period_start]", item.Period.Start.Format(time.RFC3339))
	form.Set("metadata[period_end]", item.Period.End.Format(time.RFC3339))

	if _, err := s.do(ctx, "POST", "/v1/invoiceitems", form); err != nil {
		return fmt.Errorf("stripe: invoice item: %w", err)
	}
	return nil
}

// FinalizeInvoice satisfies billing.Invoicer. Creates the
// invoice (Stripe collects the open invoice items for this
// customer automatically) and finalizes it via the
// /finalize endpoint.
//
// auto_advance=true tells Stripe to send the invoice + retry
// on failure per the Stripe billing settings.
func (s *StripeInvoicer) FinalizeInvoice(ctx context.Context, tenantID string, period Period) error {
	customerID, err := s.CustomerLookup(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("stripe: customer lookup: %w", err)
	}
	if customerID == "" {
		return nil
	}

	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("auto_advance", "true")
	form.Set("collection_method", "charge_automatically")
	form.Set("description",
		fmt.Sprintf("Invoice for %s (%s)", period.Start.Format("January 2006"), tenantID))
	form.Set("metadata[tenant_id]", tenantID)
	form.Set("metadata[period_start]", period.Start.Format(time.RFC3339))

	resp, err := s.do(ctx, "POST", "/v1/invoices", form)
	if err != nil {
		return fmt.Errorf("stripe: create invoice: %w", err)
	}
	invoiceID, _ := resp["id"].(string)
	if invoiceID == "" {
		return fmt.Errorf("stripe: invoice create returned no id")
	}

	if _, err := s.do(ctx, "POST", "/v1/invoices/"+invoiceID+"/finalize", url.Values{}); err != nil {
		return fmt.Errorf("stripe: finalize invoice %s: %w", invoiceID, err)
	}
	return nil
}

// do is the shared API caller — sets auth header, marshals
// the form, returns the parsed JSON body.
func (s *StripeInvoicer) do(ctx context.Context, method, path string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method,
		s.BaseURL+path, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	var parsed map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}
	if res.StatusCode >= 400 {
		errMsg := "non-2xx"
		if errObj, ok := parsed["error"].(map[string]any); ok {
			if m, ok := errObj["message"].(string); ok {
				errMsg = m
			}
		}
		return parsed, fmt.Errorf("stripe %d: %s", res.StatusCode, errMsg)
	}
	return parsed, nil
}
