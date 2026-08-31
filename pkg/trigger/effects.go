package trigger

import (
	"context"
	"time"
)

// The effect outbox (ADR-094) is what makes a matched board trigger's effect
// SURVIVE the instant it was matched. The previous shape — CAS-advance the
// per-tenant event cursor, then publish onto the lossy bus, then let the
// evaluator swallow effect errors — had four permanent-loss windows: a crash
// between the cursor advance and the publish, a publish error (warn-only), a
// bus delivery with no live consumer, and a launch failure AFTER the one-shot
// label consume. An event the cursor has passed never comes back, so every
// one of those was a trigger that silently never fired.
//
// The outbox inverts the order: the source normalizes + matches FIRST, makes
// one durable row per (event, subscription) pair, and only then advances the
// cursor. Execution happens under a leased claim with bounded retries, so a
// crashed replica's rows are reclaimed instead of lost. Exactly-once is NOT
// promised end to end (Core NATS could not promise it either); the contract
// is at-least-once execution with idempotent/one-shot effects:
//   - board promote is idempotent (same-bot check);
//   - direct+consume_labels rows persist ConsumeMarked between the atomic
//     label consume and the launch, so a retry after a launch failure (or a
//     reclaim after a crash) SKIPS the consume and retries the launch —
//     the one-shot is spent exactly once and the launch still happens;
//   - direct-without-consume launches may double only in the crash window
//     between the launch and MarkDone, which is the documented residue.

// Effect row states.
const (
	// EffectPending is runnable (or awaiting its retry backoff, encoded in
	// NotBefore).
	EffectPending = "pending"
	// EffectClaimed is being executed by one worker; reclaimable once
	// NotBefore (the lease horizon) passes.
	EffectClaimed = "claimed"
	// EffectDone is terminal success (or a one-shot spent by another event).
	EffectDone = "done"
	// EffectFailed is terminal exhaustion: MaxEffectAttempts real errors.
	// Visible dead-letter — rows stay queryable, and the worker Warns once.
	EffectFailed = "failed"
)

// MaxEffectAttempts bounds execution retries per row before it parks as
// failed. Backoff is exponential from EffectRetryBase.
const MaxEffectAttempts = 5

// EffectRetryBase is the first retry delay; attempt n waits base<<n.
const EffectRetryBase = 15 * time.Second

// EffectLease bounds one execution attempt: a claimed row whose lease
// passed is presumed orphaned (worker crashed mid-effect) and reclaimable.
// Generous — an effect is a couple of Mongo writes plus a launch publish.
const EffectLease = 2 * time.Minute

// EffectRow is one (event, subscription) pair owed an effect execution.
type EffectRow struct {
	// ID is EffectID(event, subID) — the idempotency key a duplicate
	// materialization (two replicas racing the same batch) collapses on.
	ID       string `bson:"_id" json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	Event    Event  `bson:"event" json:"event"`
	SubID    string `bson:"sub_id" json:"sub_id"`
	State    string `bson:"state" json:"state"`
	Attempts int    `bson:"attempts" json:"attempts"`
	// ConsumeMarked records that THIS row's atomic label consume succeeded,
	// so a retry/reclaim must not re-consume (it would read "already spent"
	// and drop the launch).
	ConsumeMarked bool `bson:"consume_marked" json:"consume_marked"`
	// NotBefore is the row's next eligibility instant: retry backoff for
	// pending rows, the lease horizon for claimed ones.
	NotBefore time.Time `bson:"not_before" json:"not_before"`
	LastError string    `bson:"last_error,omitempty" json:"last_error,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// EffectID derives the row key. The event ID already encodes the board event
// seq (unique per tenant), so (event, sub) is the natural idempotency grain.
func EffectID(eventID, subID string) string { return eventID + "|" + subID }

// EffectOutbox is the durable store the source writes and the worker drains.
// Implemented by boardmongo (cloud) and MemoryEffectOutbox (tests/local).
type EffectOutbox interface {
	// UpsertPending inserts rows that do not exist yet ($setOnInsert
	// semantics): re-materializing an already-known pair — a racing
	// replica, a re-scan after a partial batch — is a no-op, whatever
	// state the existing row reached.
	UpsertPending(ctx context.Context, rows []EffectRow) error
	// ClaimDue atomically flips up to limit eligible rows (pending past
	// their NotBefore, or claimed past their lease) to claimed with a fresh
	// lease, returning the claimed snapshots. Two workers cannot claim the
	// same row.
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]EffectRow, error)
	// MarkConsumed persists ConsumeMarked=true on a claimed row — written
	// between the atomic label consume and the launch.
	MarkConsumed(ctx context.Context, id string) error
	// MarkDone terminates a row successfully.
	MarkDone(ctx context.Context, id string) error
	// MarkRetry returns a row to pending with its backoff and error.
	MarkRetry(ctx context.Context, id string, attempts int, notBefore time.Time, lastErr string) error
	// MarkFailed parks a row as terminally failed (visible dead-letter).
	MarkFailed(ctx context.Context, id string, lastErr string) error
}

// EffectBackoff is the retry schedule: attempt n (1-based) waits base<<(n-1).
func EffectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := EffectRetryBase << (attempt - 1)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}
