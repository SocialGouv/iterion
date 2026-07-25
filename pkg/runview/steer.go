package runview

import (
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Cross-process steering wire types. The local path never marshals
// these (it talks to the engine channel directly); the cloud publisher
// serializes SteerCommand onto iterion.steer.<run_id> and the runner
// answers with SteerReply on iterion.steer.<run_id>.ack.<command_id>.
// Kept in runview — not pkg/queue — so pure-local builds and tests
// never drag the NATS package in.

// SteerCommandKind discriminates the steering commands on the wire.
type SteerCommandKind string

const (
	SteerBumpLoop    SteerCommandKind = "bump_loop"
	SteerRaiseBudget SteerCommandKind = "raise_budget"
)

// SteerCommand is one steering request in flight to a runner pod.
type SteerCommand struct {
	// CommandID is minted by the sender (ULID/UUID); the runner dedups
	// on it so a retried publish cannot double-apply.
	CommandID string              `json:"command_id"`
	Kind      SteerCommandKind    `json:"kind"`
	LoopName  string              `json:"loop_name,omitempty"`
	Delta     int                 `json:"delta,omitempty"`
	Budget    *ir.BudgetOverrides `json:"budget,omitempty"`
	IssuedAt  time.Time           `json:"issued_at"`
	// IssuedBy names the operator for the persisted run_steered event.
	IssuedBy string `json:"issued_by,omitempty"`
}

// SteerReply is the runner's typed answer — the cross-process carrier
// of the truthful contract.
type SteerReply struct {
	CommandID  string         `json:"command_id"`
	RunID      string         `json:"run_id"`
	Applied    map[string]any `json:"applied,omitempty"`
	Effective  map[string]any `json:"effective,omitempty"`
	Noop       bool           `json:"noop,omitempty"`
	NoopReason string         `json:"noop_reason,omitempty"`
	Warning    string         `json:"warning,omitempty"`
	Err        *SteerError    `json:"error,omitempty"`
	RunnerID   string         `json:"runner_id,omitempty"`
}

// SteerError codes mirror the local typed errors so the HTTP layer maps
// both paths identically.
type SteerError struct {
	// Code: "unknown_loop" | "invalid" | "no_budget" | "terminal" |
	// "not_active" | "engine_stalled" | "internal".
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *SteerError) Error() string {
	return fmt.Sprintf("steer: %s: %s", e.Code, e.Message)
}

// bumpResponseFromReply maps a runner reply onto the local response
// shape (or its typed error).
func bumpResponseFromReply(runID string, req BumpLoopRequest, reply SteerReply) (*BumpLoopResponse, error) {
	if reply.Err != nil {
		return nil, reply.Err
	}
	out := &BumpLoopResponse{
		RunID:      runID,
		Loop:       req.LoopName,
		Delta:      req.Delta,
		Noop:       reply.Noop,
		NoopReason: reply.NoopReason,
		Warning:    reply.Warning,
	}
	out.Extra = intFromAny(reply.Applied["extra"])
	out.EffectiveMax = intFromAny(reply.Effective["effective_max"])
	out.Current = intFromAny(reply.Effective["current"])
	return out, nil
}

func raiseResponseFromReply(runID string, reply SteerReply) (*RaiseBudgetResponse, error) {
	if reply.Err != nil {
		return nil, reply.Err
	}
	return &RaiseBudgetResponse{
		RunID:      runID,
		Applied:    reply.Applied,
		Effective:  reply.Effective,
		Noop:       reply.Noop,
		NoopReason: reply.NoopReason,
		Warning:    reply.Warning,
	}, nil
}

// intFromAny tolerates the JSON round-trip (float64) and native ints.
func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
