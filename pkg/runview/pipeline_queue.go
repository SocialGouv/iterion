package runview

import (
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
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
	// reserved reports the native ticket ids currently HOLDING a slot for a
	// pipeline that died and needs a human. Those runs are gone — no
	// goroutine, no budget — but their slot must not be taken by another
	// card, or the operator's fix would queue behind whatever grabbed it.
	//
	// Injected by the server, which owns the board; nil means "no
	// reservations", i.e. byte-for-byte today's behaviour. Guarded by
	// reservedMu rather than mu because it is called from OUTSIDE mu: it
	// reads the board + run store, and holding mu across that would invert
	// the lock order against launchTicketNow's SetState-then-Launch.
	reservedMu sync.RWMutex
	reserved   func() map[string]struct{}
}

// queuedItem is one waiting root launch: the pre-minted run id plus the
// full LaunchSpec needed to start it when a slot frees. The spec is held
// in memory (lost on restart — rebuilt minimally from the persisted
// queued doc, see Service.rebuildPipelineQueue).
type queuedItem struct {
	runID string
	spec  LaunchSpec
	// ticketID is spec.PipelineTicketID, lifted out so the FIFO drain can
	// test a waiter against its OWN reservation without unpacking the spec.
	ticketID string
	at       time.Time
}

// setReservedProvider wires (or clears) the board-derived reservation
// source. Safe on a nil queue and callable after construction, so a Service
// built before its board exists — and a studio project switch, which
// rebuilds the Service — can both point it at the right board.
func (q *pipelineQueue) setReservedProvider(fn func() map[string]struct{}) {
	if q == nil {
		return
	}
	q.reservedMu.Lock()
	q.reserved = fn
	q.reservedMu.Unlock()
	q.signal()
}

// reservedSet snapshots the held-slot ticket ids. MUST be called outside
// mu (see the field comment). Never nil.
func (q *pipelineQueue) reservedSet() map[string]struct{} {
	if q == nil {
		return nil
	}
	q.reservedMu.RLock()
	fn := q.reserved
	q.reservedMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// clampedReserved counts held slots for an admission decision.
//
// Two adjustments, each closing a way the board could wedge:
//
//   - EXCLUSION. A launch for `exclude` is that ticket spending its own
//     reservation, so its entry must not count against it. Without this the
//     needs-attention card is refused by the very slot it is holding for
//     itself — a permanent deadlock, reachable from POST .../tasks/{id}/launch
//     which has no ready-state precondition at all.
//
//   - CLAMP to max. More broken pipelines than slots is entirely possible;
//     the count must not exceed the cap or the arithmetic goes negative-free
//     but meaningless.
//
// Note what is deliberately NOT here: a max-1 ceiling reserving "one slot
// that can never be held". It looks like prudent liveness insurance and is
// actually a hole. It would silently disable the whole feature on a
// single-slot board — exactly the configuration where losing your slot to
// another card hurts most — and it weakens the guarantee at every other
// max for no real gain, because reservations CANNOT deadlock the board:
// every holder can always relaunch itself (the exclusion above), Close
// always releases, and failures iterion caused itself never reserve at all
// (see pipelineLaneForRoot). A fully-reserved board is not a wedge, it is
// the feature doing its job — loudly, via the concurrency chip and the
// queue banner.
func clampedReserved(set map[string]struct{}, exclude string, max int) int {
	if len(set) == 0 || max <= 0 {
		return 0
	}
	n := len(set)
	if n > max {
		n = max
	}
	if exclude != "" {
		if _, held := set[exclude]; held && n > 0 {
			n--
		}
	}
	return n
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
	// Outside mu on purpose — the provider reads the board + run store.
	reserved := clampedReserved(q.reservedSet(), spec.PipelineTicketID, q.max)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.running)+reserved < q.max {
		q.running[runID] = struct{}{}
		return true, 0
	}
	q.fifo = append(q.fifo, queuedItem{
		runID:    runID,
		spec:     spec,
		ticketID: spec.PipelineTicketID,
		at:       time.Now().UTC(),
	})
	return false, len(q.fifo)
}

// dequeueReady pops as many waiting roots as there are free slots,
// marking each running before returning it. The caller starts each item
// OUTSIDE any lock. A nil queue returns nothing.
//
// The reservation test lives HERE, not only in the server's admission
// loop, and that is the whole reason enforcement was pulled down into the
// queue. slotFreed is called from the dying run's own goroutine and
// immediately signals the scheduler, so the FIFO drain hands the
// just-freed slot to a waiter microseconds after the failure — long
// before any 2s admission tick could notice a reservation was created.
// A gate that only sat upstream would be bypassed every single time.
// (It would also miss the five other Launch call sites — the studio
// Launch modal, triggers, two webhook paths, boarddispatch — none of
// which know anything about a board.)
func (q *pipelineQueue) dequeueReady() []queuedItem {
	if q == nil {
		return nil
	}
	set := q.reservedSet() // outside mu (see reservedSet)
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedItem
	for len(q.fifo) > 0 {
		head := q.fifo[0]
		// Per-head exclusion: a queued relaunch of the very ticket holding
		// the slot is allowed to spend it.
		if len(q.running)+clampedReserved(set, head.ticketID, q.max) >= q.max {
			break
		}
		q.fifo = q.fifo[1:]
		q.running[head.runID] = struct{}{}
		out = append(out, head)
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
	q.fifo = append(q.fifo, queuedItem{
		runID:    runID,
		spec:     spec,
		ticketID: spec.PipelineTicketID,
		at:       time.Now().UTC(),
	})
	q.mu.Unlock()
	q.signal()
}

// pipelineTicketIDOf recovers the native ticket a persisted run belongs to.
// The board stamps it as Source.IssueID at launch; anything else (manual,
// scheduled, webhook) has no ticket and therefore no reservation to spend.
func pipelineTicketIDOf(r *store.Run) string {
	if r == nil || r.Source == nil {
		return ""
	}
	return r.Source.IssueID
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
	// Reserved is how many slots are held open for pipelines that died and
	// need a human — no process is running against them. Reported from the
	// SAME provider the admission decisions use, so the studio's chip can
	// never disagree with why the board is refusing to launch. Without this
	// field a fully-reserved board reads as "1 active / 3 max" next to a
	// queue that never moves, i.e. as a hang.
	Reserved int `json:"reserved"`
}

func (q *pipelineQueue) status() PipelineConcurrencyStatus {
	if q == nil {
		return PipelineConcurrencyStatus{}
	}
	// The RAW count, not the admission-clamped one: this is what the operator
	// reads. With more broken pipelines than slots, reporting the clamped
	// value would say "3 need attention" next to a lane rendering 7, and the
	// lane is the thing they have to act on. Consumers clamp their own
	// arithmetic (the studio's slotsFree already floors at 0).
	reserved := len(q.reservedSet()) // outside mu
	q.mu.Lock()
	defer q.mu.Unlock()
	return PipelineConcurrencyStatus{
		Enabled:  true,
		Max:      q.max,
		Active:   len(q.running),
		Waiting:  len(q.fifo),
		Reserved: reserved,
	}
}
