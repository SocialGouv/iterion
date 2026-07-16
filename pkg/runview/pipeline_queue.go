package runview

import (
	"sync"
	"time"
)

// pipelineQueue is the local admission gate + FIFO waiting line for ROOT
// pipeline launches. It bounds how many top-level pipelines run at once
// on a single machine — the cross-run cap that `max_parallel_branches`
// (an intra-run limit) never provided — so an operator can queue many
// pipelines without swamping the host. Child runs (ParentRunID set) never
// pass through here: a child belongs to a root that already holds a slot.
//
// A nil *pipelineQueue means "no cap" and every method is nil-receiver
// safe, so the launch path needs no nil checks and existing single-run
// callers (limit unset) see zero behaviour change.
//
// Modelled on runtime.DailyCapGuard's nil-safe shape. Concurrency
// correctness rests on one rule: every admit/enqueue/dequeue decision is
// taken under mu, but the caller must NEVER start a run (which locks the
// store + registers with the manager) while holding mu — that is why the
// scheduler dequeues under the lock, releases it, then starts.
type pipelineQueue struct {
	mu      sync.Mutex
	max     int
	running map[string]struct{} // root run IDs currently occupying a slot
	fifo    []queuedItem        // waiting roots, oldest first
	// wake is a buffered(1) signal to the scheduler goroutine: a slot
	// freed or an item was enqueued. Non-blocking sends coalesce, so the
	// scheduler never misses the fact that there is work, only its count.
	wake chan struct{}
}

// queuedItem is one waiting root launch: the pre-minted run id plus the
// full LaunchSpec needed to start it when a slot frees. The spec is held
// in memory (lost on restart — rebuilt minimally from the persisted
// queued doc, see Service.rebuildPipelineQueue).
type queuedItem struct {
	runID string
	spec  LaunchSpec
	at    time.Time
}

// newPipelineQueue returns a guard capping concurrent root pipelines at
// max, or nil (unlimited) when max <= 0.
func newPipelineQueue(max int) *pipelineQueue {
	if max <= 0 {
		return nil
	}
	return &pipelineQueue{
		max:     max,
		running: make(map[string]struct{}),
		wake:    make(chan struct{}, 1),
	}
}

// admitOrEnqueue decides a root launch under one lock. When a slot is
// free it records the run as running and returns admitted=true (the
// caller starts it immediately). Otherwise it appends the run to the
// FIFO and returns admitted=false with its 1-based queue position (the
// caller persists a queued doc + returns without spawning). A nil queue
// always admits.
func (q *pipelineQueue) admitOrEnqueue(runID string, spec LaunchSpec) (admitted bool, position int) {
	if q == nil {
		return true, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.running) < q.max {
		q.running[runID] = struct{}{}
		return true, 0
	}
	q.fifo = append(q.fifo, queuedItem{runID: runID, spec: spec, at: time.Now().UTC()})
	return false, len(q.fifo)
}

// dequeueReady pops as many waiting roots as there are free slots,
// marking each running before returning it. The caller starts each item
// OUTSIDE any lock. A nil queue returns nothing.
func (q *pipelineQueue) dequeueReady() []queuedItem {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedItem
	for len(q.running) < q.max && len(q.fifo) > 0 {
		it := q.fifo[0]
		q.fifo = q.fifo[1:]
		q.running[it.runID] = struct{}{}
		out = append(out, it)
	}
	return out
}

// slotFreed releases a root's slot (called when its run goroutine exits)
// and wakes the scheduler to admit the next waiter. A no-op for run IDs
// that never held a slot (children, resumes), so the launch path can call
// it unconditionally.
func (q *pipelineQueue) slotFreed(runID string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	delete(q.running, runID)
	q.mu.Unlock()
	q.signal()
}

// signal pokes the scheduler without blocking (coalescing wakeups).
func (q *pipelineQueue) signal() {
	if q == nil {
		return
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// enqueueRebuilt re-adds a run recovered from a persisted queued doc on
// boot (see Service.rebuildPipelineQueue). It bypasses the admission test
// on purpose — the run is already persisted queued and its slot will be
// resolved by the scheduler's first tick.
func (q *pipelineQueue) enqueueRebuilt(runID string, spec LaunchSpec) {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.fifo = append(q.fifo, queuedItem{runID: runID, spec: spec, at: time.Now().UTC()})
	q.mu.Unlock()
	q.signal()
}

// positions returns a 1-based queue position per waiting run id, for the
// board's TODO cards. A nil queue returns nil.
func (q *pipelineQueue) positions() map[string]int {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	m := make(map[string]int, len(q.fifo))
	for i, it := range q.fifo {
		m[it.runID] = i + 1
	}
	return m
}

// PipelineConcurrencyStatus is the read-only view of the local pipeline
// concurrency gate, surfaced in server_info so the studio can render the
// limit + how many pipelines are waiting.
type PipelineConcurrencyStatus struct {
	// Enabled is true when a finite cap is configured. When false the
	// other fields are zero and the board's TODO lane only holds
	// not-yet-launched native tasks.
	Enabled bool `json:"enabled"`
	Max     int  `json:"max"`
	Active  int  `json:"active"`
	Waiting int  `json:"waiting"`
}

func (q *pipelineQueue) status() PipelineConcurrencyStatus {
	if q == nil {
		return PipelineConcurrencyStatus{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return PipelineConcurrencyStatus{
		Enabled: true,
		Max:     q.max,
		Active:  len(q.running),
		Waiting: len(q.fifo),
	}
}
