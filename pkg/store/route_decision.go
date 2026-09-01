package store

import (
	"context"
	"time"
)

// RouteDecision is one durable row of the outcome router's decision
// registry: WHAT the router decided about one terminal EPISODE of one
// run, under WHICH contract, and what happened when it acted. The
// registry is the router's idempotence AND its audit: the unique
// (run_id, outcome_seq) key means a redelivered run that dies again (a
// new episode) gets a new decision, while a re-offered SAME episode —
// bus + sweep race, replica crash and reclaim, restart — finds the
// existing row and stops. An event alone cannot serve (the bus is
// lossy); a log line cannot be claimed.
type RouteDecision struct {
	ID         string `json:"id" bson:"_id"`
	TenantID   string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	RunID      string `json:"run_id" bson:"run_id"`
	OutcomeSeq int64  `json:"outcome_seq" bson:"outcome_seq"`
	// Decision is the routing verdict ("merge", "relaunch", "escalate").
	Decision string `json:"decision" bson:"decision"`
	// Reason is the verdict's evidence (which expression held/failed).
	Reason string `json:"reason" bson:"reason"`
	// PolicyHash pins WHICH contract decided — an audit replays why.
	PolicyHash string `json:"policy_hash,omitempty" bson:"policy_hash,omitempty"`
	// State tracks execution: "claimed" (decided, acting), "succeeded",
	// "failed" (the action errored TRANSIENTLY; Error carries the cause
	// and the bounded re-claim retries it), "requires_action" (the
	// action stopped on something no retry can move — Error says what
	// the operator must do).
	State     string    `json:"state" bson:"state"`
	Error     string    `json:"error,omitempty" bson:"error,omitempty"`
	ClaimedAt time.Time `json:"claimed_at" bson:"claimed_at"`
	// Attempts counts how many times this episode's decision was
	// claimed (first claim = 1). Bounds the failed-retry loop.
	Attempts int `json:"attempts,omitempty" bson:"attempts,omitempty"`
	// FinishedAt is stamped by FinishRouteDecision.
	FinishedAt *time.Time `json:"finished_at,omitempty" bson:"finished_at,omitempty"`
}

// RouteDecision states. "failed" is the RETRYABLE terminal (the
// bounded re-claim tries the action again); "requires_action" is the
// terminal for an action that stopped on a condition no retry can move
// and a human must — a merge that hit a content conflict (retrying it
// runs git against an already-conflicted tree and overwrites the
// merge_status the conflict resolver needs), or a decision whose
// execution is not wired. Both are terminal for the sweep: neither is
// re-claimable, both count as settled by the anti-join.
const (
	RouteDecisionClaimed        = "claimed"
	RouteDecisionSucceeded      = "succeeded"
	RouteDecisionFailed         = "failed"
	RouteDecisionRequiresAction = "requires_action"
)

// RouteDecisionStore is the optional registry capability. The router
// REQUIRES it: without a durable claim there is no idempotence, so a
// store lacking the capability disables the router rather than running
// it unclaimed (fail safe, same doctrine as QueuedAttemptStore).
type RouteDecisionStore interface {
	// ClaimRouteDecision atomically claims the (run_id, outcome_seq)
	// episode. Fresh episode ⇒ a "claimed" row is inserted. An existing
	// row is RE-claimable in exactly two cases, BOTH bounded by the
	// attempt cap (an episode that keeps killing its claimant is poison —
	// unbounded steal re-arms it forever):
	//   - "claimed" stamped before staleBefore — the claimant died
	//     between claim and action; without the steal the row is
	//     orphaned forever and a green run never lands (the router's
	//     own worst case). The caller passes the threshold
	//     (time.Now().Add(-RouteClaimLease) in production) — the
	//     ClaimMerge precedent, which is what makes the steal testable;
	//   - "failed" with fewer than MaxRouteDecisionAttempts — a
	//     transient action error must not burn the episode permanently.
	// Everything else returns claimed=false with the existing row —
	// including "requires_action", whose whole point is that no retry
	// helps and re-running the action would make things worse.
	ClaimRouteDecision(ctx context.Context, d RouteDecision, staleBefore time.Time) (claimed bool, existing *RouteDecision, err error)
	// EnsureRouterWatermark returns the router's activation instant,
	// establishing it first-writer-wins on the first call. The sweep
	// never reaches behind it: flipping the fleet switch on must not
	// retro-route a lookback's worth of historical terminals (up to a
	// full sweep batch of merges pushed to the forge in the first
	// minute). Pre-watermark terminals stay the operator's.
	EnsureRouterWatermark(ctx context.Context) (time.Time, error)
	// FinishRouteDecision moves the claimed row to succeeded/failed.
	FinishRouteDecision(ctx context.Context, runID string, outcomeSeq int64, state, actionErr string) error
	// ListRouteDecisions returns a run's decisions, newest episode
	// first (audit surface).
	ListRouteDecisions(ctx context.Context, runID string) ([]RouteDecision, error)
	// ListRoutableRuns is the sweep net's query: ids of runs that
	// carry a routing policy and sit in a terminal status, updated
	// since the given instant, oldest first. Runs whose CURRENT episode
	// is settled — a "succeeded" row, or a "failed" one at the attempt
	// cap — are excluded (anti-join): without it, past one batch of
	// decided terminals per lookback the head of the list is all
	// already-decided runs and the very runs the net exists for become
	// unreachable. The bus is the fast path; this list is the source of
	// truth (six terminal paths never publish, and the bus is lossy by
	// design — its own doc says the poll is the backstop).
	ListRoutableRuns(ctx context.Context, since time.Time, limit int) ([]string, error)
}

// RouteClaimLease bounds how long a "claimed" registry row protects
// its holder; MaxRouteDecisionAttempts bounds failed-action retries.
const (
	RouteClaimLease          = 15 * time.Minute
	MaxRouteDecisionAttempts = 3
)

// AsRouteDecisionStore returns the registry capability, or nil when the
// backend has none. Callers must treat nil as "router disabled".
func AsRouteDecisionStore(s RunStore) RouteDecisionStore {
	if s == nil {
		return nil
	}
	r, _ := s.(RouteDecisionStore)
	return r
}
