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
	// "failed" (the action errored; Error carries the cause).
	State     string    `json:"state" bson:"state"`
	Error     string    `json:"error,omitempty" bson:"error,omitempty"`
	ClaimedAt time.Time `json:"claimed_at" bson:"claimed_at"`
	// FinishedAt is stamped by FinishRouteDecision.
	FinishedAt *time.Time `json:"finished_at,omitempty" bson:"finished_at,omitempty"`
}

// RouteDecision states.
const (
	RouteDecisionClaimed   = "claimed"
	RouteDecisionSucceeded = "succeeded"
	RouteDecisionFailed    = "failed"
)

// RouteDecisionStore is the optional registry capability. The router
// REQUIRES it: without a durable claim there is no idempotence, so a
// store lacking the capability disables the router rather than running
// it unclaimed (fail safe, same doctrine as QueuedAttemptStore).
type RouteDecisionStore interface {
	// ClaimRouteDecision atomically inserts the row for
	// (run_id, outcome_seq) in state "claimed". claimed=false with the
	// EXISTING row when the episode is already decided — the caller
	// stops, whatever state the prior row is in (a failed action is
	// retried by an operator through the registry, not by every sweep
	// pass forever).
	ClaimRouteDecision(ctx context.Context, d RouteDecision) (claimed bool, existing *RouteDecision, err error)
	// FinishRouteDecision moves the claimed row to succeeded/failed.
	FinishRouteDecision(ctx context.Context, runID string, outcomeSeq int64, state, actionErr string) error
	// ListRouteDecisions returns a run's decisions, newest episode
	// first (audit surface).
	ListRouteDecisions(ctx context.Context, runID string) ([]RouteDecision, error)
	// ListRoutableRuns is the sweep net's query: ids of runs that
	// carry a routing policy and sit in a terminal status, updated
	// since the given instant. The bus is the fast path; this list is
	// the source of truth (six terminal paths never publish, and the
	// bus is lossy by design — its own doc says the poll is the
	// backstop).
	ListRoutableRuns(ctx context.Context, since time.Time, limit int) ([]string, error)
}

// AsRouteDecisionStore returns the registry capability, or nil when the
// backend has none. Callers must treat nil as "router disabled".
func AsRouteDecisionStore(s RunStore) RouteDecisionStore {
	if s == nil {
		return nil
	}
	r, _ := s.(RouteDecisionStore)
	return r
}
