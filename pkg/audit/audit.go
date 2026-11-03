// Package audit persists control-plane mutations into a queryable,
// tenant-scoped log. Two scopes: tenant events are readable by the
// org's admins (/api/teams/{id}/audit); platform events (super-admin
// actions on orgs/users) are super-admin only (/api/admin/audit).
//
// Writes are best-effort from the HTTP handlers (detached, logged on
// failure) — the audit trail is an operations/compliance surface, not
// a transactional ledger.
package audit

import (
	"context"
	"errors"
	"time"
)

// Scope partitions readability.
type Scope string

const (
	// ScopeTenant events are visible to the org's admins.
	ScopeTenant Scope = "tenant"
	// ScopePlatform events (super-admin actions) are platform-only.
	ScopePlatform Scope = "platform"
)

// Event is one audit row. Action is a stable dotted token
// (e.g. "org.status_changed", "byok.created", "webhook.rotated",
// "auth.login.failed") — the queryable contract; Meta carries small
// action-specific details (never secret material, and never an
// identifier a sign-in attempted in clear: its peppered digest).
type Event struct {
	ID        string         `bson:"_id" json:"id"`
	Scope     Scope          `bson:"scope" json:"scope"`
	TenantID  string         `bson:"tenant_id,omitempty" json:"tenant_id,omitempty"`
	ActorID   string         `bson:"actor_id,omitempty" json:"actor_id,omitempty"`
	ActorKind string         `bson:"actor_kind,omitempty" json:"actor_kind,omitempty"` // user|super_admin|webhook|system|anonymous (a refused sign-in)
	Action    string         `bson:"action" json:"action"`
	Target    string         `bson:"target,omitempty" json:"target,omitempty"` // kind of object acted on (org|user|webhook|secret|binding|byok|member|invitation|token)
	TargetID  string         `bson:"target_id,omitempty" json:"target_id,omitempty"`
	Meta      map[string]any `bson:"meta,omitempty" json:"meta,omitempty"`
	IP        string         `bson:"ip,omitempty" json:"ip,omitempty"`
	UserAgent string         `bson:"user_agent,omitempty" json:"user_agent,omitempty"`
	CreatedAt time.Time      `bson:"created_at" json:"created_at"`
}

// Page bounds + filters a listing. Zero values mean "no filter".
type Page struct {
	Offset  int
	Limit   int
	Action  string
	ActorID string
	From    time.Time
	To      time.Time
}

// RetentionDays bounds audit retention (Mongo TTL). 400 days covers
// an annual compliance cycle with margin; longer retention belongs in
// an exported archive, not the live collection.
const RetentionDays = 400

// ErrNotFound is reserved for symmetric store semantics.
var ErrNotFound = errors.New("audit: not found")

// Store persists and lists audit events. Implementations: MongoStore
// (production) and MemoryStore (tests/local). Keep in lock-step.
type Store interface {
	Insert(ctx context.Context, e Event) error
	// ListByTenant returns tenant-scoped events for one org, newest
	// first.
	ListByTenant(ctx context.Context, tenantID string, p Page) ([]Event, error)
	// ListPlatform returns platform-scoped events, newest first.
	ListPlatform(ctx context.Context, p Page) ([]Event, error)
}

// ClampLimit normalises a caller-supplied page size.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 500:
		return 500
	default:
		return n
	}
}
