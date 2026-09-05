package runner

import (
	"context"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Credential-slot occupancy.
//
// The per-key concurrency ceiling (secrets.ApiKey.MaxConcurrentRuns)
// counts alive runs stamped with the key's fingerprint. The stamp is the
// right protection while a delegate is spending the key, and pure waste
// while the run executes tool-only work: an agent node that finished
// cleanly and then sat in a sixty-minute verify gate held one of a key's
// two slots the whole time, starving every launch pinned to that key. So
// the runner marks the run idle (store.Run.LLMIdleSince) the moment its
// last model-calling node finishes, and un-marks it the moment one starts
// again; the ceiling counts only un-marked runs.
//
// Explicit toggles, not a lease: a pod that dies mid-node leaves a run the
// orphan sweeper flips to failed_resumable, and a parked run holds no slot
// through the status filter already — so nothing needs a heartbeat. The
// marker is conservative by default: a run counts from claim until it
// PROVES idle, and every re-stamp (a resumed attempt) starts it over.

// credSlotTracker turns node start/finish events into idle transitions.
// Concurrent branches may start and finish model nodes at once, so it
// counts ACTIVE model nodes rather than flipping on the last event seen.
type credSlotTracker struct {
	isLLM func(nodeID string) bool
	set   func(idleSince *time.Time)
	now   func() time.Time

	mu     sync.Mutex
	active int
	idle   bool
}

// observe is a runtime.WithEventObserver callback.
func (t *credSlotTracker) observe(evt store.Event) {
	if evt.Type != store.EventNodeStarted && evt.Type != store.EventNodeFinished {
		return
	}
	if evt.NodeID == "" || !t.isLLM(evt.NodeID) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch evt.Type {
	case store.EventNodeStarted:
		t.active++
		if t.idle {
			t.idle = false
			t.set(nil)
		}
	case store.EventNodeFinished:
		if t.active > 0 {
			t.active--
		}
		if t.active == 0 && !t.idle {
			t.idle = true
			at := t.now().UTC()
			t.set(&at)
		}
	}
}

// credSlotObserver builds the tracker for one attempt, or nil when the run
// holds no sealed credentials (nothing is stamped, so nothing to release)
// or the store cannot be written.
func (r *Runner) credSlotObserver(ctx context.Context, msg *queue.RunMessage, wf *ir.Workflow, logger *iterlog.Logger) *credSlotTracker {
	if r.cfg.Store == nil || msg == nil || msg.SecretsRef == "" || wf == nil {
		return nil
	}
	nodes := wf.Nodes
	return &credSlotTracker{
		isLLM: func(id string) bool {
			n, ok := nodes[id]
			return ok && ir.NodeUsesLLM(n)
		},
		now: time.Now,
		set: func(idleSince *time.Time) {
			// Detached from the engine's context: the finish of the last
			// model node may coincide with the run's own end, and the
			// marker is exactly what the NEXT launch's ceiling reads.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
			defer cancel()
			sctx = store.WithIdentity(sctx, msg.TenantID, msg.OwnerID)
			if err := r.cfg.Store.SetRunLLMIdle(sctx, msg.RunID, idleSince); err != nil && logger != nil {
				// Best effort: a lost marker leaves the ceiling over-
				// protective (today's behaviour), never under-protective.
				logger.Warn("runner: run %s: credential-slot idle marker: %v", msg.RunID, err)
			}
		},
	}
}
