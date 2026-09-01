package webhooks

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors. Callers compare with errors.Is.
var (
	ErrNotFound  = errors.New("webhooks: not found")
	ErrDuplicate = errors.New("webhooks: duplicate idempotency key")
)

// ConfigStore persists webhook configs. Get is intentionally NOT
// tenant-scoped (the inbound auth path resolves the tenant FROM the
// webhook, so it has no tenant context yet); the HTTP CRUD layer
// enforces tenant ownership before mutating. All other reads are by
// explicit tenant.
type ConfigStore interface {
	Create(ctx context.Context, c Config) error
	Get(ctx context.Context, id string) (Config, error)
	Update(ctx context.Context, c Config) error
	Delete(ctx context.Context, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]Config, error)
	MarkUsed(ctx context.Context, id string, t time.Time) error
}

// DeliveryStore records deliveries for audit + idempotent replay
// suppression. Insert returns ErrDuplicate when IdempotencyKey already
// exists — that unique constraint is the durable dedupe.
type DeliveryStore interface {
	Insert(ctx context.Context, d Delivery) error
	GetByIdempotencyKey(ctx context.Context, key string) (Delivery, error)
	Update(ctx context.Context, d Delivery) error
	ListByWebhook(ctx context.Context, tenantID, webhookID string, limit int) ([]Delivery, error)
	// CountLaunched counts the deliveries of one event kind on one subject
	// that actually launched a run (RunID set). This is a CEILING query —
	// the gate-autofix per-PR bound reads it — so it must be exact over the
	// whole audit, never a recent-window scan a busy webhook can push the
	// rows out of.
	CountLaunched(ctx context.Context, tenantID, webhookID, eventKind, projectPath, subjectID string) (int, error)
}

// Limits are the monthly call caps applied to a delivery. Zero means
// "no cap at that level".
type Limits struct {
	PerWebhookMonthly int
	PerOrgMonthly     int
}

// Counter enforces per-org (and optional per-webhook) monthly call
// quotas. Allow atomically increments the current month's counters and
// reports whether the call is within every applicable cap; a denied
// call does NOT consume quota.
type Counter interface {
	Allow(ctx context.Context, tenantID, webhookID string, when time.Time, limits Limits) (bool, error)
	OrgCount(ctx context.Context, tenantID string, when time.Time) (int, error)
}
