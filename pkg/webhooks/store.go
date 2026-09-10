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
	// ClaimFailedRetry takes over a delivery row left in StatusLaunchError so
	// that ONE caller retries the event. A prior launch failure is
	// deliberately retryable — a redelivery of the same event must be able to
	// relaunch it — which makes the take-over a claim, not a read followed by
	// a write: two redeliveries arriving together both find the failed row,
	// and both would otherwise go on to launch a run for one event.
	//
	// The replacement lands iff the stored row is STILL that failure at the
	// attempt count the caller read. claimed=false with a NIL error means
	// another caller got there first, and the loser must answer as a
	// duplicate rather than launch. ErrNotFound when the row is gone.
	ClaimFailedRetry(ctx context.Context, d Delivery, expectAttempts int) (claimed bool, err error)
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
	//
	// accepted is false — with a nil error — when the stored payload is
	// strictly NEWER than d (DeferredPayloadIsStale): the row is left
	// untouched and the caller must treat its delivery as superseded, not
	// as a store failure. The distinction is load-bearing: the caller's
	// error path launches immediately rather than losing the review,
	// which on a stale payload is precisely the wrong thing to do.
	Upsert(ctx context.Context, d DeferredLaunch) (accepted bool, err error)
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
	// Reschedule pushes an UNLAUNCHED row's FireAt back and records the
	// attempt, IFF it still holds the named generation — the retry half
	// of the claim contract, for a launch the admission gate refused
	// transiently (org concurrency, launch rate) or that failed outright.
	// Generation-guarded for the same reason Delete is: a fresh push that
	// landed mid-claim must not be clobbered by the stale payload's
	// re-arm. Clears the lease, so the next sweep past FireAt claims it.
	Reschedule(ctx context.Context, subjectKey string, generation int64, fireAt time.Time, attempts int) error
	// Delete removes the row IFF it still holds the named generation —
	// the claimer's acknowledgement that exactly the payload it launched
	// is done. A subject that re-armed mid-claim (higher generation)
	// survives and fires again with its fresh payload.
	Delete(ctx context.Context, subjectKey string, generation int64) error
	// DeleteBySubject removes the subject's row unconditionally,
	// whatever its generation or lease — the purge for a pull request
	// that died (closed/merged) inside its quiet window, whose parked
	// review must never fire.
	DeleteBySubject(ctx context.Context, subjectKey string) error
}

// DeferredPayloadIsStale reports whether an INCOMING parked payload is
// older than the one already stored, using each side's DeferredLaunch
// OrderKey — the forge's own timestamp for the event.
//
// Arrival order cannot serve. Forges do not guarantee webhook delivery
// order, and a retried or slow delivery landing after a later one is
// exactly the regime the push debounce targets (pushes seconds apart).
// Replacing by arrival would park the STALE head: three minutes later
// the sweep reviews it and posts `revi/review` on a commit that is no
// longer the head, leaving the real head with no status and the merge
// blocked until someone pushes again.
//
// Both keys must be present to order anything — a payload without one
// is accepted, which is the pre-ordering behaviour. Comparison is
// lexicographic, which orders every fixed-width forge timestamp
// correctly (GitHub "2026-09-03T10:15:22Z", GitLab
// "2026-09-03 10:15:22 UTC"); equal keys are NOT stale, so two events
// the forge stamped in the same second keep the arrival-order outcome
// rather than dropping the second.
func DeferredPayloadIsStale(incoming, stored string) bool {
	if incoming == "" || stored == "" {
		return false
	}
	return incoming < stored
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
