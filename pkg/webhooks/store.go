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
	// ListLaunchedBySubject returns every delivery that launched a run FOR
	// one subject in one project — matching the subject itself OR its
	// ParentSubjectID, whatever the event kind. The parent half is what
	// makes it complete: a `/billy` fixer records "comment:99" and a
	// review-thread reply "rc:99", so a caller asking "what did pr:7
	// launch" would otherwise see only the PR-event lane and miss exactly
	// the comment-launched runs.
	//
	// EXACT over the whole audit for the same reason CountLaunched is: its
	// caller stops the runs a closed pull request left behind, and the run
	// it most needs to reach is the one PARKED hours ago on a provider
	// quota — precisely the row a recency-bounded scan has already
	// dropped. Scoped by projectPath because a subject id carries no repo
	// and one webhook config can serve several.
	ListLaunchedBySubject(ctx context.Context, tenantID, webhookID, projectPath, subjectID string) ([]Delivery, error)
}

// DeferredLaunchStore parks resolved webhook launches for a quiet
// window (the push debounce) and hands due ones to the sweep exactly
// once across replicas.
type DeferredLaunchStore interface {
	// Upsert stores d keyed on SubjectKey: an existing row for the same
	// subject is REPLACED wholesale (newest payload wins), its FireAt
	// pushed back — the debounce — its Generation bumped and any sweep
	// lease cleared (a fresh push re-arms even a subject mid-claim).
	Upsert(ctx context.Context, d DeferredLaunch) error
	// ClaimDue atomically LEASES and returns up to limit rows whose
	// FireAt is at or before now and whose previous lease (if any) has
	// expired: each row goes to one caller across replicas for the
	// lease's duration. The claimer Deletes the row once the launch
	// tail has run; a claimer that dies mid-launch simply lets the
	// lease lapse and the row re-fires — the launch tail's idempotency
	// key turns the retry of an ALREADY-launched target into a
	// duplicate no-op, so the net is at-least-once fire, exactly-once
	// launch. (A destructive claim would instead LOSE the launch with
	// the delivery long since ACKed: a required check absent forever.)
	ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]DeferredLaunch, error)
	// Delete removes the row IFF it still holds the named generation —
	// the claimer's acknowledgement that exactly the payload it launched
	// is done. A subject that re-armed mid-claim (higher generation)
	// survives and fires again with its fresh payload.
	Delete(ctx context.Context, subjectKey string, generation int64) error
	// RescheduleFailed re-arms a row whose launch failed transiently:
	// bumps Attempts, sets FireAt and clears the lease — IFF the row
	// still holds the named generation. The CAS is load-bearing: a push
	// arriving mid-retry has already Upserted a fresher payload (higher
	// generation), and that one must win rather than be pushed back by
	// the loser's backoff.
	//
	// It exists because the deferred lane answered the forge 202 at park
	// time and wrote no delivery row, so NO redelivery is coming: the
	// ordinary "StatusLaunchError is retryable on redelivery" contract
	// has nothing to retry it. Without this the first transient blip
	// destroys the review outright.
	RescheduleFailed(ctx context.Context, subjectKey string, generation int64, fireAt time.Time) error
	// DeleteBySubject drops the subject's parked launch UNCONDITIONALLY —
	// no generation CAS, because the caller has learnt the subject itself
	// is over (the pull request closed or merged). No payload for it is
	// worth launching any more, including one that re-arms a millisecond
	// later. Missing rows are not an error.
	DeleteBySubject(ctx context.Context, subjectKey string) error
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
