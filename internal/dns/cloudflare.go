package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CloudflareProvider is a production Provider backed by
// Cloudflare's v4 API.
//
// Construct via NewCloudflareProvider. The struct fields are
// exposed (rather than a private builder) so tests can swap the
// HTTPClient for a httptest.Server-driven client.
type CloudflareProvider struct {
	APIToken   string
	ZoneID     string
	BaseURL    string // override for tests; default https://api.cloudflare.com/client/v4
	HTTPClient *http.Client
}

// NewCloudflareProvider builds a Provider with the standard
// Cloudflare endpoint and a 15s HTTP timeout.
func NewCloudflareProvider(apiToken, zoneID string) *CloudflareProvider {
	return &CloudflareProvider{
		APIToken:   apiToken,
		ZoneID:     zoneID,
		BaseURL:    "https://api.cloudflare.com/client/v4",
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// cfRecord is Cloudflare's wire shape; converted to Record on read.
type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
}

type cfResponse struct {
	Success bool        `json:"success"`
	Errors  []cfError   `json:"errors"`
	Result  interface{} `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Upsert satisfies Provider.
func (c *CloudflareProvider) Upsert(ctx context.Context, r Record) (Record, error) {
	if err := c.validate(); err != nil {
		return r, err
	}
	// Look up existing record so we know whether to POST (create)
	// or PUT (update).
	existing, err := c.findRecord(ctx, r.Name, r.Type)
	if err != nil {
		return r, err
	}
	body := cfRecord{
		Name:    r.Name,
		Type:    r.Type,
		Content: r.Content,
		TTL:     ttlOrDefault(r.TTL),
	}
	if existing != nil {
		// Same content + TTL — no-op so we don't burn API quota.
		if existing.Content == body.Content && (existing.TTL == body.TTL || body.TTL == 1) {
			r.ID = existing.ID
			return r, nil
		}
		var got cfRecord
		if err := c.do(ctx, http.MethodPut,
			fmt.Sprintf("/zones/%s/dns_records/%s", c.ZoneID, existing.ID),
			body, &got); err != nil {
			return r, err
		}
		r.ID = got.ID
		return r, nil
	}
	var got cfRecord
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/zones/%s/dns_records", c.ZoneID),
		body, &got); err != nil {
		return r, err
	}
	r.ID = got.ID
	return r, nil
}

// Delete satisfies Provider — Cloudflare's 404 maps to nil.
func (c *CloudflareProvider) Delete(ctx context.Context, name, recordType string) error {
	if err := c.validate(); err != nil {
		return err
	}
	existing, err := c.findRecord(ctx, name, recordType)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // already gone
	}
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/zones/%s/dns_records/%s", c.ZoneID, existing.ID),
		nil, nil)
}

// List satisfies Provider — paginates through every record in
// the zone (Cloudflare default page size is 100).
func (c *CloudflareProvider) List(ctx context.Context) ([]Record, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	var out []Record
	page := 1
	for {
		var got []cfRecord
		path := fmt.Sprintf("/zones/%s/dns_records?per_page=100&page=%d", c.ZoneID, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, r := range got {
			out = append(out, Record{
				ID: r.ID, Name: r.Name, Type: r.Type,
				Content: r.Content, TTL: r.TTL,
			})
		}
		if len(got) < 100 {
			break
		}
		page++
	}
	return out, nil
}

func (c *CloudflareProvider) findRecord(ctx context.Context, name, recordType string) (*cfRecord, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("type", recordType)
	path := fmt.Sprintf("/zones/%s/dns_records?%s", c.ZoneID, q.Encode())
	var got []cfRecord
	if err := c.do(ctx, http.MethodGet, path, nil, &got); err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, nil
	}
	return &got[0], nil
}

// do is the shared request runner — adds the auth header,
// JSON-marshals the body, and unwraps Cloudflare's "result"
// envelope into the supplied dest.
func (c *CloudflareProvider) do(ctx context.Context, method, path string, body, dest interface{}) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dns/cloudflare: marshal: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("dns/cloudflare: request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("dns/cloudflare: read: %w", err)
	}
	var env cfResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("dns/cloudflare: parse: %w (status=%d body=%s)", err, resp.StatusCode, truncate(string(raw)))
	}
	if !env.Success {
		// Cloudflare's "record already exists" code is 81057;
		// caller's findRecord-then-upsert path means we
		// shouldn't see it, but report it cleanly if we do.
		return fmt.Errorf("dns/cloudflare: %s", formatErrors(env.Errors))
	}
	if dest != nil && env.Result != nil {
		// Re-marshal Result into the caller's struct.
		buf, _ := json.Marshal(env.Result)
		if err := json.Unmarshal(buf, dest); err != nil {
			return fmt.Errorf("dns/cloudflare: decode result: %w", err)
		}
	}
	return nil
}

func (c *CloudflareProvider) validate() error {
	if c.APIToken == "" {
		return errors.New("dns/cloudflare: APIToken is empty")
	}
	if c.ZoneID == "" {
		return errors.New("dns/cloudflare: ZoneID is empty")
	}
	if c.BaseURL == "" {
		return errors.New("dns/cloudflare: BaseURL is empty")
	}
	if c.HTTPClient == nil {
		return errors.New("dns/cloudflare: HTTPClient is nil")
	}
	return nil
}

// ttlOrDefault — Cloudflare uses TTL=1 to mean "automatic".
func ttlOrDefault(ttl int) int {
	if ttl <= 0 {
		return 1
	}
	return ttl
}

func formatErrors(es []cfError) string {
	if len(es) == 0 {
		return "unknown error"
	}
	parts := make([]string, 0, len(es))
	for _, e := range es {
		parts = append(parts, fmt.Sprintf("[%d] %s", e.Code, e.Message))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
