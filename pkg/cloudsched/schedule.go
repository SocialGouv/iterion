// Package cloudsched is the cloud-mode recurring-bot scheduler: a per-org
// store of cron-scheduled bots and a multi-replica-safe ticker that fires each
// due schedule exactly once (CAS on the next-fire time, no leader election
// needed). The self-hosted equivalent is `iterion schedule` (host crontab);
// this is its cloud counterpart.
package cloudsched

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// ScheduledBot is one cron-scheduled bot run. When RepoURL is set the cloud
// runner clones that repo before the bot starts (RepoRef pins the ref);
// stateful bots that persist to git (e.g. feed-watch state_commit=true) need
// this to have a workspace to push to. When RepoURL is empty the run executes
// against the runner pod's base WorkDir, matching the pre-repo behaviour.
type ScheduledBot struct {
	ID                string `bson:"_id" json:"id"`
	TenantID          string `bson:"tenant_id" json:"tenant_id"`
	RepoIntegrationID string `bson:"repo_integration_id,omitempty" json:"repo_integration_id,omitempty"`
	BotID             string `bson:"bot_id" json:"bot_id"`
	Cron              string `bson:"cron" json:"cron"` // 5-field standard cron
	// IntervalSeconds drives an always-on (keepalive) schedule instead of Cron:
	// the ticker relaunches the bot every IntervalSeconds (sub-minute allowed,
	// bounded by the ticker's own Interval). Exactly one of Cron/IntervalSeconds
	// is set. Overlap=keepalive gives at-most-one-live + staleness reaping.
	IntervalSeconds int               `bson:"interval_seconds,omitempty" json:"interval_seconds,omitempty"`
	Vars            map[string]string `bson:"vars,omitempty" json:"vars,omitempty"`
	RepoURL         string            `bson:"repo_url,omitempty" json:"repo_url,omitempty"`
	RepoRef         string            `bson:"repo_ref,omitempty" json:"repo_ref,omitempty"`
	Disabled        bool              `bson:"disabled,omitempty" json:"disabled,omitempty"`

	// Overlap policy + pre-launch guard (pkg/schedgate). Overlap ""
	// normalizes to "skip": a slot whose previous run is still live is
	// consumed without launching (audited), instead of piling up runs.
	Overlap       string `bson:"overlap,omitempty" json:"overlap,omitempty"`
	MaxConcurrent int    `bson:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
	Guard         string `bson:"guard,omitempty" json:"guard,omitempty"`
	GuardTimeout  string `bson:"guard_timeout,omitempty" json:"guard_timeout,omitempty"`
	GuardVar      string `bson:"guard_var,omitempty" json:"guard_var,omitempty"`
	// StaleAfter is the keepalive silence cutoff (Go duration); empty defaults
	// to schedgate.DefaultStaleAfter. Only meaningful with Overlap=keepalive.
	StaleAfter string `bson:"stale_after,omitempty" json:"stale_after,omitempty"`

	// NextFireAt is the next UTC instant this schedule is due. The ticker
	// CAS-advances it the moment it claims a tick, so a second replica racing
	// on the same row finds next_fire_at already moved and backs off — exactly
	// one fire per slot without a leader.
	NextFireAt time.Time  `bson:"next_fire_at" json:"next_fire_at"`
	LastFireAt *time.Time `bson:"last_fire_at,omitempty" json:"last_fire_at,omitempty"`

	CreatedBy string    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Store persists scheduled bots. Mongo (cloud) and an in-memory impl (tests)
// satisfy it.
type Store interface {
	Create(ctx context.Context, sb ScheduledBot) error
	Get(ctx context.Context, id string) (ScheduledBot, error)
	ListByIntegration(ctx context.Context, tenantID, integrationID string) ([]ScheduledBot, error)
	// ListByTenant returns every schedule owned by tenantID (including rows
	// with an empty RepoIntegrationID — the manual CRUD path). Ordered by
	// CreatedAt ascending for stable rendering.
	ListByTenant(ctx context.Context, tenantID string) ([]ScheduledBot, error)
	// ListDue returns enabled schedules whose next_fire_at <= now (capped by
	// limit, 0 = no cap), oldest-due first.
	ListDue(ctx context.Context, now time.Time, limit int) ([]ScheduledBot, error)
	// ClaimTick atomically advances a schedule's next_fire_at from expectedNext
	// to newNext (and stamps last_fire_at = firedAt), returning true only when
	// THIS caller won the CAS. A losing replica gets (false, nil). This is the
	// exactly-once primitive — no leader election.
	ClaimTick(ctx context.Context, id string, expectedNext, newNext, firedAt time.Time) (bool, error)
	// Update applies a partial mutation to an existing schedule. Only the
	// non-nil fields of patch are written; NextFireAt is recomputed from the
	// new Cron when Cron is set.
	Update(ctx context.Context, id string, patch SchedulePatch) (ScheduledBot, error)
	Delete(ctx context.Context, id string) error
	DeleteByIntegration(ctx context.Context, tenantID, integrationID string) error
}

// SchedulePatch describes the mutable slice of ScheduledBot the manual CRUD
// endpoint exposes. Nil field = leave untouched. Cron carries the new
// expression when set (already validated by ValidateCron); the store
// recomputes NextFireAt through NextFire(cron, now).
type SchedulePatch struct {
	Cron            *string
	IntervalSeconds *int
	NextFireAt      *time.Time
	Vars            *map[string]string
	RepoURL         *string
	RepoRef         *string
	Disabled        *bool
	Overlap         *string
	MaxConcurrent   *int
	Guard           *string
	GuardTimeout    *string
	GuardVar        *string
	StaleAfter      *string
	UpdatedAt       time.Time
}

// ErrNotFound is returned by Get/Delete for an unknown id.
var ErrNotFound = fmt.Errorf("cloudsched: scheduled bot not found")

// ValidateCron reports whether expr is a valid 5-field standard cron.
func ValidateCron(expr string) error {
	if _, err := cron.ParseStandard(expr); err != nil {
		return fmt.Errorf("cloudsched: invalid cron %q: %w", expr, err)
	}
	return nil
}

// NextFire returns the next instant after `after` at which expr fires.
func NextFire(expr string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("cloudsched: invalid cron %q: %w", expr, err)
	}
	return sched.Next(after), nil
}

// NextFireForBot computes a schedule's next-fire instant: a fixed interval for
// keepalive (IntervalSeconds > 0), else the cron expression. The single seam
// every ticker/store path uses so keepalive and cron stay consistent.
func NextFireForBot(sb ScheduledBot, after time.Time) (time.Time, error) {
	if sb.IntervalSeconds > 0 {
		return after.Add(time.Duration(sb.IntervalSeconds) * time.Second), nil
	}
	return NextFire(sb.Cron, after)
}

// applySchedulePatch mutates sb in place with the non-nil fields of patch.
// UpdatedAt is stamped from patch.UpdatedAt (caller-provided so tests are
// deterministic). NextFireAt takes patch.NextFireAt when set — callers with a
// new Cron pre-compute this via NextFire so the store never calls time.Now.
func applySchedulePatch(sb *ScheduledBot, patch SchedulePatch) {
	if patch.Cron != nil {
		sb.Cron = *patch.Cron
	}
	if patch.IntervalSeconds != nil {
		sb.IntervalSeconds = *patch.IntervalSeconds
	}
	if patch.NextFireAt != nil {
		sb.NextFireAt = *patch.NextFireAt
	}
	if patch.Vars != nil {
		sb.Vars = *patch.Vars
	}
	if patch.RepoURL != nil {
		sb.RepoURL = *patch.RepoURL
	}
	if patch.RepoRef != nil {
		sb.RepoRef = *patch.RepoRef
	}
	if patch.Disabled != nil {
		sb.Disabled = *patch.Disabled
	}
	if patch.Overlap != nil {
		sb.Overlap = *patch.Overlap
	}
	if patch.MaxConcurrent != nil {
		sb.MaxConcurrent = *patch.MaxConcurrent
	}
	if patch.Guard != nil {
		sb.Guard = *patch.Guard
	}
	if patch.GuardTimeout != nil {
		sb.GuardTimeout = *patch.GuardTimeout
	}
	if patch.GuardVar != nil {
		sb.GuardVar = *patch.GuardVar
	}
	if patch.StaleAfter != nil {
		sb.StaleAfter = *patch.StaleAfter
	}
	if !patch.UpdatedAt.IsZero() {
		sb.UpdatedAt = patch.UpdatedAt
	}
}

// Policy projects the schedule's schedgate fields into a normalized
// overlap/guard policy.
func (sb ScheduledBot) Policy() schedgate.Policy {
	return schedgate.Normalize(schedgate.Policy{
		Overlap:       sb.Overlap,
		MaxConcurrent: sb.MaxConcurrent,
		Guard:         sb.Guard,
		GuardTimeout:  sb.GuardTimeout,
		GuardVar:      sb.GuardVar,
		StaleAfter:    sb.StaleAfter,
	})
}
