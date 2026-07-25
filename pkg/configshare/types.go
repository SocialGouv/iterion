// Package configshare is the scoped, self-service config-file editor: a
// per-(bot × repo × config-file × category) grant, addressed by a dynamic URL
// and authenticated by its own token, that lets a non-operator edit ONLY a
// declared allow-list of fields (the veille's feeds[] + editorial) in one file
// of one repo — and nothing else in iterion.
//
// The grant is a synthetic principal (auth.KindShare): the auth layer refuses
// it every operator RBAC gate. Reads project the file down to the grant's
// visible paths (never the whole file); writes walk a strict pre-merge
// allow-list, merge onto the server-read file, re-validate, and land through
// forge.FileClient with an if-match SHA (no clone, no race with a bot's state
// push). Every commit-shaping field is server-derived from the pinned record.
package configshare

import (
	"context"
	"errors"
	"time"
)

// Share is one config-edit grant. The token is stored only as a salted hash
// (+ last4/fingerprint for the operator UI); the plaintext is shown once at
// create/rotate. Every field that shapes a write — RepoURL, RepoRef,
// ConfigPath, AllowedPaths — is pinned here at mint time and never taken from
// a request body, so a token holder can't retarget the file, branch or fields.
type Share struct {
	ID         string `json:"id" bson:"_id"`
	TenantID   string `json:"tenant_id" bson:"tenant_id"`
	BotID      string `json:"bot_id" bson:"bot_id"`
	Label      string `json:"label" bson:"label"`
	RepoURL    string `json:"repo_url" bson:"repo_url"`
	RepoRef    string `json:"repo_ref" bson:"repo_ref"`
	ConfigPath string `json:"config_path" bson:"config_path"`
	Category   string `json:"category,omitempty" bson:"category,omitempty"`
	SchemaRef  string `json:"schema_ref,omitempty" bson:"schema_ref,omitempty"`
	// AllowedPaths are literal dotted JSON paths the editor may WRITE (e.g.
	// "categories.a11y.feeds"). No globs — every entry is a full leaf path.
	AllowedPaths []string `json:"allowed_paths" bson:"allowed_paths"`
	// VisiblePaths are the dotted paths the editor may READ back (a superset of
	// AllowedPaths, plus read-only context like a category's digest_title). The
	// GET projection returns ONLY these; everything else in the file is
	// stripped before serialization.
	VisiblePaths []string `json:"visible_paths" bson:"visible_paths"`
	ReadOnly     bool     `json:"read_only" bson:"read_only"`

	TokenHash   string `json:"-" bson:"token_hash"`
	TokenLast4  string `json:"token_last4" bson:"token_last4"`
	Fingerprint string `json:"fingerprint" bson:"fingerprint"`

	Enabled       bool       `json:"enabled" bson:"enabled"`
	CreatedBy     string     `json:"created_by" bson:"created_by"`
	CreatedAt     time.Time  `json:"created_at" bson:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at" bson:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty" bson:"revoked_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty" bson:"last_used_at,omitempty"`
	MonthlyWrites int        `json:"monthly_writes,omitempty" bson:"monthly_writes,omitempty"`
}

// Active reports whether the grant may authenticate at `now`: enabled, not
// revoked, and unexpired (a zero ExpiresAt never expires — mint always sets one).
func (s *Share) Active(now time.Time) bool {
	return s != nil && s.Enabled && s.RevokedAt == nil &&
		(s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt))
}

// Delivery is one audit row per mutating (and, optionally, reading) call
// through a share — the forensic trail after a token leak: who (source IP +
// UA), when, what changed (before/after blob SHA + the changed leaf paths).
type Delivery struct {
	ID        string    `json:"id" bson:"_id"`
	ShareID   string    `json:"share_id" bson:"share_id"`
	TenantID  string    `json:"tenant_id" bson:"tenant_id"`
	At        time.Time `json:"at" bson:"at"`
	SourceIP  string    `json:"source_ip" bson:"source_ip"`
	UserAgent string    `json:"user_agent" bson:"user_agent"`
	Method    string    `json:"method" bson:"method"`
	// Actor attributes the edit: "share:<id>" for a token (capability-URL)
	// edit, "user:<id>" for an authenticated config-editor session (ADR-078).
	// Empty on legacy rows.
	Actor        string   `json:"actor,omitempty" bson:"actor,omitempty"`
	Status       int      `json:"status" bson:"status"`
	BeforeSHA    string   `json:"before_sha,omitempty" bson:"before_sha,omitempty"`
	AfterSHA     string   `json:"after_sha,omitempty" bson:"after_sha,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty" bson:"changed_paths,omitempty"`
	Error        string   `json:"error,omitempty" bson:"error,omitempty"`
}

// Store persists shares + their delivery audit, tenant-scoped. GetByID takes
// no tenant (the auth middleware resolves a share from the URL id before any
// tenant is known); operator CRUD handlers enforce tenancy via canManageTeam
// on the returned share's TenantID.
type Store interface {
	Create(ctx context.Context, s *Share) error
	GetByID(ctx context.Context, id string) (*Share, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*Share, error)
	Update(ctx context.Context, s *Share) error
	Delete(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, at time.Time) error
	RecordDelivery(ctx context.Context, d *Delivery) error
	ListDeliveries(ctx context.Context, shareID string, limit int) ([]*Delivery, error)
}

// ErrNotFound is returned by a Store when no share (or delivery) matches.
var ErrNotFound = errors.New("configshare: not found")

// ErrValidation wraps a patch/field rejection (off-list path, bad value) so the
// caller can map it to a 400 while a forge/transport failure surfaces as a 502.
var ErrValidation = errors.New("configshare: invalid edit")
