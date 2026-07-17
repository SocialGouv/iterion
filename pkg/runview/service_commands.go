package runview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Live-run steering commands (bump_loop / raise_budget / answer_human).
//
// The truthful response contract: an unknown target is a typed 400, a
// run this process cannot steer is a typed 409, a valid command that
// changed nothing is a 200 with noop=true and a reason, and an applied
// command reports the effective post-apply values. The HTTP layer maps
// the sentinels; this file never lies about what happened.

// steerAckTimeout bounds the wait for the engine's ack. The engine only
// drains overrides at its safe boundary, so a run deep inside a long
// LLM call acks late — but the channel is buffered, the command is
// already delivered, and the caller should not hang an HTTP request on
// it. On timeout the command remains queued and WILL apply; the caller
// is told exactly that.
const steerAckTimeout = 2 * time.Second

// ErrRunNotHeld reports a steering command for a run this process does
// not hold (a runner-pod or dispatcher-owned run, or simply not
// running). Maps to 409.
var ErrRunNotHeld = errors.New("runview: run is not held by this process")

// ErrSteerPending reports a command that was DELIVERED to the live
// engine but not acknowledged within steerAckTimeout (the run is busy
// inside a long node execution). The command stays queued and applies
// at the next safe boundary; it is not lost. Maps to 202-style
// handling upstream.
var ErrSteerPending = errors.New("runview: steering command queued; the run is busy in a long node and will apply it at its next boundary")

// RunTerminalError reports steering on a run that already ended.
type RunTerminalError struct{ Status store.RunStatus }

func (e *RunTerminalError) Error() string {
	return fmt.Sprintf("runview: run is terminal (%s); steering is only valid on a live run", e.Status)
}

// runSteerer is the optional cross-process extension of
// LaunchPublisher: a publisher that can route a steering command to
// the runner pod holding the run. Implemented by the cloud publisher
// (NATS request/reply); asserted dynamically so local mode needs no
// stub.
type runSteerer interface {
	SteerRun(ctx context.Context, runID string, cmd SteerCommand) (SteerReply, error)
}

// BumpLoopRequest grants extra iterations to a named loop of a LIVE run.
type BumpLoopRequest struct {
	LoopName string `json:"loop_name"`
	Delta    int    `json:"delta"`
	// IssuedBy names the operator for the persisted run_steered event
	// (filled by the HTTP layer from the authenticated identity).
	IssuedBy string `json:"-"`
}

// BumpLoopResponse reports the applied grant and effective ceiling.
type BumpLoopResponse struct {
	RunID        string `json:"run_id"`
	Loop         string `json:"loop"`
	Delta        int    `json:"delta,omitempty"`
	Extra        int    `json:"extra,omitempty"`
	EffectiveMax int    `json:"effective_max,omitempty"`
	Current      int    `json:"current_iteration"`
	Noop         bool   `json:"noop,omitempty"`
	NoopReason   string `json:"noop_reason,omitempty"`
	// Warning surfaces a non-fatal degradation (applied in-memory but
	// not persisted — a resume would lose the grant).
	Warning string `json:"warning,omitempty"`
}

// RaiseBudgetRequest raises the run's budget caps to absolute values.
type RaiseBudgetRequest struct {
	Budget   ir.BudgetOverrides `json:"budget"`
	IssuedBy string             `json:"-"`
}

// RaiseBudgetResponse reports what was applied and the effective caps.
type RaiseBudgetResponse struct {
	RunID      string         `json:"run_id"`
	Applied    map[string]any `json:"applied,omitempty"`
	Effective  map[string]any `json:"effective,omitempty"`
	Noop       bool           `json:"noop,omitempty"`
	NoopReason string         `json:"noop_reason,omitempty"`
	Warning    string         `json:"warning,omitempty"`
}

// BumpLoopCtx grants req.Delta extra iterations to loop req.LoopName on
// a live run. Local-first: the in-process engine wins; a cloud
// publisher that implements runSteerer is the cross-process fallback.
func (s *Service) BumpLoopCtx(ctx context.Context, runID string, req BumpLoopRequest) (*BumpLoopResponse, error) {
	if err := s.steerPrecheck(ctx, runID); err != nil {
		return nil, err
	}
	if ch := s.steerChannelFor(runID); ch != nil {
		msg := runtime.NewBumpLoopOverride(req.LoopName, req.Delta, req.IssuedBy)
		if err := sendSteer(ctx, ch, msg); err != nil {
			return nil, err
		}
		res, err := msg.Await(ctx, steerAckTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrSteerPending
		}
		return bumpResponseFrom(runID, req, res)
	}
	if st, ok := s.publisher.(runSteerer); ok {
		reply, err := st.SteerRun(ctx, runID, SteerCommand{
			Kind:     SteerBumpLoop,
			LoopName: req.LoopName,
			Delta:    req.Delta,
			IssuedBy: req.IssuedBy,
		})
		if err != nil {
			return nil, err
		}
		return bumpResponseFromReply(runID, req, reply)
	}
	return nil, ErrRunNotHeld
}

// RaiseBudgetCtx raises the live run's budget caps (absolute,
// raise-only). Same local-first/publisher-fallback shape as BumpLoopCtx.
func (s *Service) RaiseBudgetCtx(ctx context.Context, runID string, req RaiseBudgetRequest) (*RaiseBudgetResponse, error) {
	if req.Budget.IsZero() {
		return nil, fmt.Errorf("%w: raise_budget requires at least one cap", runtime.ErrInvalidOverride)
	}
	if err := req.Budget.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", runtime.ErrInvalidOverride, err)
	}
	if err := s.steerPrecheck(ctx, runID); err != nil {
		return nil, err
	}
	if ch := s.steerChannelFor(runID); ch != nil {
		msg := runtime.NewRaiseBudgetOverride(req.Budget, req.IssuedBy)
		if err := sendSteer(ctx, ch, msg); err != nil {
			return nil, err
		}
		res, err := msg.Await(ctx, steerAckTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrSteerPending
		}
		return raiseResponseFrom(runID, res)
	}
	if st, ok := s.publisher.(runSteerer); ok {
		reply, err := st.SteerRun(ctx, runID, SteerCommand{
			Kind:     SteerRaiseBudget,
			Budget:   &req.Budget,
			IssuedBy: req.IssuedBy,
		})
		if err != nil {
			return nil, err
		}
		return raiseResponseFromReply(runID, reply)
	}
	return nil, ErrRunNotHeld
}

// AnswerHumanCtx pre-fills answers on a paused_waiting_human run and
// resumes it. NOT live steering: the engine goroutine exited at the
// pause, so the correct primitive is the existing Resume with answers —
// this is its convenience wrapper (the run's own FilePath, no force).
func (s *Service) AnswerHumanCtx(ctx context.Context, runID string, answers map[string]any) (*LaunchResult, error) {
	r, err := s.LoadRunCtx(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		return nil, fmt.Errorf("%w: run is %s, want %s", ErrNotAwaitingHuman, r.Status, store.RunStatusPausedWaitingHuman)
	}
	return s.Resume(ctx, ResumeSpec{RunID: runID, FilePath: r.FilePath, Answers: answers})
}

// ErrNotAwaitingHuman reports answer_human on a run that is not paused
// on a human gate. Maps to 409.
var ErrNotAwaitingHuman = errors.New("runview: run is not awaiting human input")

// steerPrecheck loads the run under the caller's tenant and rejects
// terminal states with the typed 409.
func (s *Service) steerPrecheck(ctx context.Context, runID string) error {
	r, err := s.LoadRunCtx(ctx, runID)
	if err != nil {
		return err
	}
	if r.Status.IsTerminal() {
		return &RunTerminalError{Status: r.Status}
	}
	return nil
}

// sendSteer delivers on the buffered channel without ever hanging the
// HTTP handler: a full buffer (8 unacked commands) is refused loudly
// rather than queued invisibly.
func sendSteer(ctx context.Context, ch chan *runtime.OverrideMsg, msg *runtime.OverrideMsg) error {
	select {
	case ch <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("runview: steering queue full for this run (unacknowledged commands pending) — retry after the run reaches its next boundary")
	}
}

func bumpResponseFrom(runID string, req BumpLoopRequest, res runtime.OverrideResult) (*BumpLoopResponse, error) {
	if res.Err != nil && res.Applied == nil {
		return nil, res.Err
	}
	out := &BumpLoopResponse{
		RunID:      runID,
		Loop:       req.LoopName,
		Delta:      req.Delta,
		Noop:       res.Noop,
		NoopReason: res.NoopReason,
	}
	if v, ok := res.Applied["extra"].(int); ok {
		out.Extra = v
	}
	if v, ok := res.Effective["effective_max"].(int); ok {
		out.EffectiveMax = v
	}
	if v, ok := res.Effective["current"].(int); ok {
		out.Current = v
	}
	if res.Err != nil {
		out.Warning = res.Err.Error()
	}
	return out, nil
}

func raiseResponseFrom(runID string, res runtime.OverrideResult) (*RaiseBudgetResponse, error) {
	if res.Err != nil && res.Applied == nil && !res.Noop {
		return nil, res.Err
	}
	out := &RaiseBudgetResponse{
		RunID:      runID,
		Applied:    res.Applied,
		Effective:  res.Effective,
		Noop:       res.Noop,
		NoopReason: res.NoopReason,
	}
	if res.Err != nil {
		out.Warning = res.Err.Error()
	}
	return out, nil
}
