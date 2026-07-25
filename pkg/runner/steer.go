package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// Cross-process steering, runner side: the per-run NATS subscriber
// decodes a runview.SteerCommand, pushes the matching runtime override
// into the in-flight engine's channel, waits for the engine ack, and
// publishes the typed runview.SteerReply on the per-command ack
// subject. Duplicate command IDs (publisher retry racing a slow ack)
// replay the cached reply instead of double-applying.

// steerEngineAckTimeout is the runner-side wait for the engine ack —
// kept under the publisher's 5s wire budget so the reply (even a
// "still busy" one) makes it back before the API times out.
const steerEngineAckTimeout = 4 * time.Second

// steerDedupCap bounds the per-run reply cache. Operators rarely send
// more than a handful of commands to one run; eviction just means a
// very late duplicate re-applies (visible in the run_steered timeline).
const steerDedupCap = 32

// runSteerState is the per-run steering state: the engine's override
// channel plus the bounded command-id → reply cache.
type runSteerState struct {
	ch     chan *runtime.OverrideMsg
	seen   map[string][]byte
	order  []string
	runCtx context.Context
}

// registerSteerChannel creates and registers the override channel for
// an in-flight run. Called in processOne BEFORE the engine is built;
// the engine picks the channel up at construction via
// steerChannelFor.
func (r *Runner) registerSteerChannel(runCtx context.Context, runID string) chan *runtime.OverrideMsg {
	ch := make(chan *runtime.OverrideMsg, 8)
	r.steerMu.Lock()
	if r.steer == nil {
		r.steer = make(map[string]*runSteerState)
	}
	r.steer[runID] = &runSteerState{ch: ch, seen: make(map[string][]byte), runCtx: runCtx}
	r.steerMu.Unlock()
	return ch
}

func (r *Runner) unregisterSteerChannel(runID string) {
	r.steerMu.Lock()
	delete(r.steer, runID)
	r.steerMu.Unlock()
}

func (r *Runner) steerChannelFor(runID string) chan *runtime.OverrideMsg {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	if st := r.steer[runID]; st != nil {
		return st.ch
	}
	return nil
}

// handleSteerDelivery is the NATS subscriber callback for one steering
// command. Runs on the NATS delivery goroutine — everything it does is
// channel/IO, never a direct touch of engine state.
func (r *Runner) handleSteerDelivery(runID string, body []byte, commandID string) {
	logger := r.cfg.Logger

	var cmd runview.SteerCommand
	if err := json.Unmarshal(body, &cmd); err != nil {
		logger.Warn("runner: steer %s: bad command payload: %v", runID, err)
		r.publishSteerReply(runID, commandID, runview.SteerReply{
			CommandID: commandID, RunID: runID,
			Err: &runview.SteerError{Code: "invalid", Message: "malformed steer command: " + err.Error()},
		})
		return
	}
	if commandID == "" {
		commandID = cmd.CommandID
	}

	// Dedup: a publisher retry with the same command id replays the
	// first reply rather than re-applying the override.
	r.steerMu.Lock()
	st := r.steer[runID]
	if st != nil {
		if cached, ok := st.seen[commandID]; ok {
			r.steerMu.Unlock()
			if err := r.publishSteerAckFn()(runID, commandID, cached); err != nil {
				logger.Warn("runner: steer %s: replay ack %s: %v", runID, commandID, err)
			}
			return
		}
	}
	r.steerMu.Unlock()

	reply := r.applySteerCommand(runID, st, cmd, commandID)
	r.publishSteerReply(runID, commandID, reply)
}

func (r *Runner) applySteerCommand(runID string, st *runSteerState, cmd runview.SteerCommand, commandID string) runview.SteerReply {
	reply := runview.SteerReply{CommandID: commandID, RunID: runID, RunnerID: r.cfg.RunnerID}
	if st == nil {
		reply.Err = &runview.SteerError{Code: "not_active", Message: "run is not executing on this runner"}
		return reply
	}

	var msg *runtime.OverrideMsg
	switch cmd.Kind {
	case runview.SteerBumpLoop:
		msg = runtime.NewBumpLoopOverride(cmd.LoopName, cmd.Delta, cmd.IssuedBy)
	case runview.SteerRaiseBudget:
		if cmd.Budget == nil {
			reply.Err = &runview.SteerError{Code: "invalid", Message: "raise_budget requires a budget object"}
			return reply
		}
		msg = runtime.NewRaiseBudgetOverride(*cmd.Budget, cmd.IssuedBy)
	default:
		reply.Err = &runview.SteerError{Code: "invalid", Message: "unknown steer kind " + string(cmd.Kind)}
		return reply
	}

	select {
	case st.ch <- msg:
	default:
		reply.Err = &runview.SteerError{Code: "engine_stalled", Message: "steering queue full for this run — retry after it reaches its next boundary"}
		return reply
	}

	ackTimeout := r.steerAckTimeout
	if ackTimeout <= 0 {
		ackTimeout = steerEngineAckTimeout
	}
	res, err := msg.Await(st.runCtx, ackTimeout)
	if err != nil {
		if st.runCtx.Err() != nil {
			reply.Err = &runview.SteerError{Code: "terminal", Message: "run ended before the command was applied"}
			return reply
		}
		reply.Err = &runview.SteerError{Code: "engine_stalled", Message: "command queued; the run is busy in a long node and will apply it at its next boundary"}
		return reply
	}
	reply.Applied = res.Applied
	reply.Effective = res.Effective
	reply.Noop = res.Noop
	reply.NoopReason = res.NoopReason
	if res.Err != nil {
		reply.Err = steerErrorFromRuntime(res.Err)
		// An "applied but not persisted" degradation keeps the applied
		// fields AND carries a warning instead of a hard error.
		if res.Applied != nil && reply.Err != nil && reply.Err.Code == "internal" {
			reply.Warning = res.Err.Error()
			reply.Err = nil
		}
	}
	return reply
}

// steerErrorFromRuntime maps the typed runtime errors onto wire codes.
func steerErrorFromRuntime(err error) *runview.SteerError {
	var unknownLoop *runtime.UnknownLoopError
	switch {
	case errors.As(err, &unknownLoop):
		details := map[string]any{}
		if len(unknownLoop.Available) > 0 {
			details["available_loops"] = unknownLoop.Available
		}
		return &runview.SteerError{Code: "unknown_loop", Message: unknownLoop.Error(), Details: details}
	case errors.Is(err, runtime.ErrInvalidOverride):
		return &runview.SteerError{Code: "invalid", Message: err.Error()}
	case errors.Is(err, runtime.ErrNoBudgetDeclared):
		return &runview.SteerError{Code: "no_budget", Message: err.Error()}
	default:
		return &runview.SteerError{Code: "internal", Message: err.Error()}
	}
}

// publishSteerAckFn is the transport seam for tests; production uses
// the NATS conn.
func (r *Runner) publishSteerAckFn() func(runID, commandID string, body []byte) error {
	if r.steerAckFn != nil {
		return r.steerAckFn
	}
	return r.cfg.NATS.PublishSteerAck
}

// publishSteerReply serializes, caches (bounded) and publishes the
// reply.
func (r *Runner) publishSteerReply(runID, commandID string, reply runview.SteerReply) {
	body, err := json.Marshal(reply)
	if err != nil {
		r.cfg.Logger.Warn("runner: steer %s: marshal reply: %v", runID, err)
		return
	}
	r.steerMu.Lock()
	if st := r.steer[runID]; st != nil && commandID != "" {
		if _, dup := st.seen[commandID]; !dup {
			st.seen[commandID] = body
			st.order = append(st.order, commandID)
			if len(st.order) > steerDedupCap {
				evict := st.order[0]
				st.order = st.order[1:]
				delete(st.seen, evict)
			}
		}
	}
	r.steerMu.Unlock()
	if err := r.publishSteerAckFn()(runID, commandID, body); err != nil {
		r.cfg.Logger.Warn("runner: steer %s: publish ack %s: %v", runID, commandID, err)
	}
}
