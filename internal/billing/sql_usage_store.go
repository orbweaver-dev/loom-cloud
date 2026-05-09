package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// SQLUsageStore is the production UsageStore — persists
// UsageEvents to the loom_cloud_usage_events table. Concurrent
// Record calls are safe (the table is INSERT-only on the hot
// path; Range reads aggregate later).
type SQLUsageStore struct {
	DB *sqlx.DB
}

// NewSQLUsageStore constructs a UsageStore backed by the
// supplied DB.
func NewSQLUsageStore(db *sqlx.DB) *SQLUsageStore {
	return &SQLUsageStore{DB: db}
}

// CreateTableSQL returns the DDL for the usage table. Bootstrap
// path runs this at startup (similar to dbloader's meta-tables).
func CreateTableSQL() string {
	return `CREATE TABLE IF NOT EXISTS loom_cloud_usage_events (
		id           CHAR(36)        NOT NULL,
		site_id      CHAR(36)        NOT NULL,
		tenant_id    CHAR(36)        NOT NULL,
		kind         VARCHAR(64)     NOT NULL,
		quantity     DECIMAL(20,6)   NOT NULL DEFAULT 1,
		occurred_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		metadata     JSON            NULL,
		PRIMARY KEY (id),
		KEY idx_usage_tenant_period (tenant_id, occurred_at),
		KEY idx_usage_kind (kind, occurred_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
}

// Record satisfies UsageStore.
func (s *SQLUsageStore) Record(ctx context.Context, ev UsageEvent) error {
	if s.DB == nil {
		return errors.New("billing: SQLUsageStore.DB is nil")
	}
	if ev.SiteID == "" || ev.TenantID == "" || ev.Kind == "" {
		return errors.New("billing: site_id, tenant_id, and kind are required")
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	var metaJSON sql.NullString
	if len(ev.Metadata) > 0 {
		buf, err := json.Marshal(ev.Metadata)
		if err != nil {
			return fmt.Errorf("billing: marshal metadata: %w", err)
		}
		metaJSON = sql.NullString{String: string(buf), Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO loom_cloud_usage_events (id, site_id, tenant_id, kind, quantity, occurred_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), ev.SiteID, ev.TenantID, ev.Kind,
		ev.Quantity, ev.OccurredAt, metaJSON)
	if err != nil {
		return fmt.Errorf("billing: insert usage: %w", err)
	}
	return nil
}

// Range satisfies UsageStore. Returns events for the supplied
// tenant in [start, end). Caller is responsible for chunking
// when the row count is huge — the Reconciler currently sums
// in-memory, so very high-cardinality tenants may want to push
// aggregation into SQL via a wrapper.
func (s *SQLUsageStore) Range(ctx context.Context, tenantID string, start, end time.Time) ([]UsageEvent, error) {
	if s.DB == nil {
		return nil, errors.New("billing: SQLUsageStore.DB is nil")
	}
	type row struct {
		ID         string         `db:"id"`
		SiteID     string         `db:"site_id"`
		TenantID   string         `db:"tenant_id"`
		Kind       string         `db:"kind"`
		Quantity   float64        `db:"quantity"`
		OccurredAt time.Time      `db:"occurred_at"`
		Metadata   sql.NullString `db:"metadata"`
	}
	var rows []row
	err := s.DB.SelectContext(ctx, &rows, `
		SELECT id, site_id, tenant_id, kind, quantity, occurred_at, metadata
		FROM loom_cloud_usage_events
		WHERE tenant_id = ? AND occurred_at >= ? AND occurred_at < ?
		ORDER BY occurred_at`, tenantID, start, end)
	if err != nil {
		return nil, fmt.Errorf("billing: range query: %w", err)
	}
	out := make([]UsageEvent, len(rows))
	for i, r := range rows {
		out[i] = UsageEvent{
			SiteID: r.SiteID, TenantID: r.TenantID, Kind: r.Kind,
			Quantity: r.Quantity, OccurredAt: r.OccurredAt,
		}
		if r.Metadata.Valid && r.Metadata.String != "" {
			_ = json.Unmarshal([]byte(r.Metadata.String), &out[i].Metadata)
		}
	}
	return out, nil
}
