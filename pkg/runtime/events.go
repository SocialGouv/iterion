package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runEvents is the run-scoped reliable event registry backing the emit/wait
// node primitives (ADR-051). It is deliberately NOT the cross-run pkg/eventbus
// (which is lossy/at-least-once); in-run coordination needs reliable, sticky
// delivery so an emit/wait pair is not order-fragile.
//
// Sticky: signal records the payload AND closes the event's waiter channel. A
// wait that arrives after the emit reads an already-closed channel and returns
// immediately; a wait that arrives first parks on the open channel until signal
// closes it. Concurrent branches share one *runEvents under its mutex, riding
// the same discipline as the rest of runState.
type runEvents struct {
	mu      sync.Mutex
	fired   map[string]map[string]any // event name → immutable payload
	waiters map[string]chan struct{}  // event name → close-on-fire signal
}

func newRunEvents() *runEvents {
	return &runEvents{
		fired:   make(map[string]map[string]any),
		waiters: make(map[string]chan struct{}),
	}
}

// snapshot returns a deep copy of the sticky fired-event set for checkpoint
// persistence. Waiter channels are process-local and reconstructed as closed
// channels for these event names on restore.
func (re *runEvents) snapshot() map[string]map[string]any {
	if re == nil {
		return nil
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	if len(re.fired) == 0 {
		return nil
	}
	out := make(map[string]map[string]any, len(re.fired))
	for name, payload := range re.fired {
		out[name] = clonePayload(payload)
	}
	return out
}

// restore replaces the registry with a checkpoint's sticky fired-event set.
// A fresh resume has no live waiters yet, so rebuilding one already-closed
// channel per fired name preserves emit-before-wait delivery exactly.
func (re *runEvents) restore(fired map[string]map[string]any) {
	if re == nil {
		return
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	re.fired = make(map[string]map[string]any, len(fired))
	re.waiters = make(map[string]chan struct{}, len(fired))
	for name, payload := range fired {
		re.fired[name] = clonePayload(payload)
		ch := make(chan struct{})
		close(ch)
		re.waiters[name] = ch
	}
}

func restoreRunEvents(rs *runState, cp *store.Checkpoint) {
	if rs == nil || cp == nil {
		return
	}
	if rs.events == nil {
		rs.events = newRunEvents()
	}
	rs.events.restore(cp.FiredEvents)
}

// chanLocked returns the (lazily-created) signal channel for an event. Caller
// must hold re.mu.
func (re *runEvents) chanLocked(name string) chan struct{} {
	ch, ok := re.waiters[name]
	if !ok {
		ch = make(chan struct{})
		re.waiters[name] = ch
	}
	return ch
}

// signal records an event's payload and wakes every parked (and all future)
// waiters by closing its channel. Idempotent on the close.
func (re *runEvents) signal(name string, payload map[string]any) {
	re.mu.Lock()
	defer re.mu.Unlock()
	re.fired[name] = payload
	ch := re.chanLocked(name)
	select {
	case <-ch: // already closed by a prior signal
	default:
		close(ch)
	}
}

// waitChan returns the signal channel for an event (closed iff already fired).
func (re *runEvents) waitChan(name string) <-chan struct{} {
	re.mu.Lock()
	defer re.mu.Unlock()
	return re.chanLocked(name)
}

// payloadFor returns an isolated deep copy of the recorded payload for a fired
// event (empty map if absent). Cloning happens under the mutex so a caller can
// never observe — or alias — the registry's live `fired` entry; the ADR-051
// immutability boundary is guaranteed by the API, not by every caller
// remembering to clone afterward.
func (re *runEvents) payloadFor(name string) map[string]any {
	re.mu.Lock()
	defer re.mu.Unlock()
	return clonePayload(re.fired[name])
}

// clonePayload makes a deep copy of an event payload. emit and wait nodes each
// get their own output map, fully decoupled from the registry's stored payload
// (and from sibling waiters) so a downstream mutation — even of a *nested* map
// or slice — can't corrupt the event. Reuses deepCopyValue (the same recursive
// JSON-shaped clone the fan-out path uses for branch outputs) rather than a
// shallow per-key copy, which would leave nested structures aliased and break
// the ADR-051 immutability boundary. clonePayload(nil) returns an empty,
// non-nil map (the "event not yet fired" path stays behavior-preserving).
func clonePayload(p map[string]any) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = deepCopyValue(v)
	}
	return out
}

// emitEvent resolves an emit node's payload from its With data-mappings against
// the given scope, signals the run-scoped registry, and returns the node output
// (a copy of the payload so {{outputs.<emit>.field}} resolves). Scope is
// explicit so the main loop resolves against the trunk (rs.scope()) and a
// fan-out branch resolves against its merged parent+branch scope — letting a
// branch-local emit reference a sibling node produced earlier in the same branch.
func (e *Engine) emitEvent(rs *runState, en *ir.EmitNode, sc resolveScope) map[string]any {
	payload := make(map[string]any, len(en.With))
	for _, dm := range en.With {
		payload[dm.Key] = e.resolveMapping(dm, sc)
	}
	rs.events.signal(en.Event, payload)
	return clonePayload(payload)
}

// awaitEvent blocks until a wait node's event is emitted in the same run, the
// mandatory timeout fires, or ctx is cancelled. On success it returns a copy of
// the event payload. Shared by the main loop (execWait) and the branch path.
func (e *Engine) awaitEvent(ctx context.Context, rs *runState, nodeID string, wn *ir.WaitNode) (map[string]any, error) {
	ch := rs.events.waitChan(wn.Event)
	timer := time.NewTimer(wn.Timeout)
	defer timer.Stop()

	select {
	case <-ch:
	case <-timer.C:
		return nil, &RuntimeError{
			Code:    ErrCodeTimeout,
			Message: fmt.Sprintf("wait %q: event %q did not arrive within %s", nodeID, wn.Event, wn.Timeout),
			NodeID:  nodeID,
			Hint:    "ensure an emit node produces this event (check max_parallel_branches lets the emitter run), or raise the timeout",
		}
	case <-ctx.Done():
		return nil, &RuntimeError{
			Code:    ErrCodeCancelled,
			Message: fmt.Sprintf("wait %q: cancelled while waiting for event %q", nodeID, wn.Event),
			NodeID:  nodeID,
			Cause:   ctx.Err(),
		}
	}

	// payloadFor already returns an isolated deep copy (cloned under the
	// registry mutex), so no further clone is needed here.
	return rs.events.payloadFor(wn.Event), nil
}

// execEmit publishes an emit node's event with an immutable payload resolved
// from its With data-mappings, then advances. No LLM, no shell. emit has no
// output schema, so the envelope's validateNodeOutput is a guaranteed no-op.
func (e *Engine) execEmit(rs *runState, nodeID string, en *ir.EmitNode) (string, error) {
	return e.execSpecialNode(rs, nodeID, "emit", en,
		map[string]any{"event": en.Event},
		func() (map[string]any, error) { return e.emitEvent(rs, en, rs.scope()), nil },
		nil,
	)
}

// execWait blocks the current branch until its event is emitted in the same run
// (or the mandatory timeout / context cancellation fires), then advances with
// the event payload as the node's output. The timeout is the bornage — a wait
// can never hang the run.
func (e *Engine) execWait(ctx context.Context, rs *runState, nodeID string, wn *ir.WaitNode) (string, error) {
	return e.execSpecialNode(rs, nodeID, "wait", wn,
		map[string]any{"event": wn.Event},
		func() (map[string]any, error) { return e.awaitEvent(ctx, rs, nodeID, wn) },
		nil,
	)
}
