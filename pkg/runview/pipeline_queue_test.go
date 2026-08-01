package runview

import (
	"sync"
	"testing"
)

func TestPipelineQueueNilIsUnlimited(t *testing.T) {
	var q *pipelineQueue // disabled cap
	for i := 0; i < 100; i++ {
		admitted, pos := q.admitOrEnqueue("run", LaunchSpec{})
		if !admitted || pos != 0 {
			t.Fatalf("nil queue must always admit; got admitted=%v pos=%d", admitted, pos)
		}
	}
	// Every other method must be a safe no-op on a nil receiver.
	q.slotFreed("run")
	q.signal()
	q.enqueueRebuilt("run", LaunchSpec{})
	if got := q.dequeueReady(); got != nil {
		t.Fatalf("nil dequeueReady = %v, want nil", got)
	}
	if got := q.positions(); got != nil {
		t.Fatalf("nil positions = %v, want nil", got)
	}
	if st := q.status(); st.Enabled {
		t.Fatalf("nil status Enabled = true, want false")
	}
}

func TestPipelineQueueAdmitsUpToMaxThenEnqueues(t *testing.T) {
	q := newPipelineQueue(2)

	if admitted, _ := q.admitOrEnqueue("r1", LaunchSpec{}); !admitted {
		t.Fatal("r1 should be admitted")
	}
	if admitted, _ := q.admitOrEnqueue("r2", LaunchSpec{}); !admitted {
		t.Fatal("r2 should be admitted")
	}
	admitted, pos := q.admitOrEnqueue("r3", LaunchSpec{})
	if admitted || pos != 1 {
		t.Fatalf("r3 admitted=%v pos=%d, want queued at position 1", admitted, pos)
	}
	admitted, pos = q.admitOrEnqueue("r4", LaunchSpec{})
	if admitted || pos != 2 {
		t.Fatalf("r4 admitted=%v pos=%d, want queued at position 2", admitted, pos)
	}

	if st := q.status(); st.Max != 2 || st.Active != 2 || st.Waiting != 2 {
		t.Fatalf("status = %+v, want max=2 active=2 waiting=2", st)
	}
	if got := q.positions(); got["r3"] != 1 || got["r4"] != 2 {
		t.Fatalf("positions = %+v, want r3=1 r4=2", got)
	}

	// Freeing one slot admits exactly one waiter, FIFO (r3 before r4).
	q.slotFreed("r1")
	ready := q.dequeueReady()
	if len(ready) != 1 || ready[0].runID != "r3" {
		t.Fatalf("dequeue after 1 free = %+v, want [r3]", ready)
	}
	q.slotFreed("r2")
	ready = q.dequeueReady()
	if len(ready) != 1 || ready[0].runID != "r4" {
		t.Fatalf("dequeue after 2nd free = %+v, want [r4]", ready)
	}
	if got := q.dequeueReady(); len(got) != 0 {
		t.Fatalf("dequeue with empty fifo = %+v, want none", got)
	}
}

func TestPipelineQueueDequeueRespectsFreeSlots(t *testing.T) {
	q := newPipelineQueue(1)
	q.admitOrEnqueue("r1", LaunchSpec{}) // admitted (slot full)
	q.admitOrEnqueue("r2", LaunchSpec{}) // queued
	q.admitOrEnqueue("r3", LaunchSpec{}) // queued
	// No slot free yet → nothing dequeues.
	if got := q.dequeueReady(); len(got) != 0 {
		t.Fatalf("dequeue with full slot = %+v, want none", got)
	}
	q.slotFreed("r1")
	if got := q.dequeueReady(); len(got) != 1 || got[0].runID != "r2" {
		t.Fatalf("dequeue = %+v, want [r2] only (max=1)", got)
	}
}

// Under contention the gate never admits more than max: concurrent
// admitOrEnqueue calls are serialized by the mutex, so exactly `max`
// callers see admitted=true.
func TestPipelineQueueNeverOverAdmitsUnderContention(t *testing.T) {
	const max = 3
	const callers = 50
	q := newPipelineQueue(max)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _ := q.admitOrEnqueue(runIDf(i), LaunchSpec{})
			if ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if admitted != max {
		t.Fatalf("admitted %d under contention, want exactly %d", admitted, max)
	}
	if st := q.status(); st.Active != max || st.Waiting != callers-max {
		t.Fatalf("status = %+v, want active=%d waiting=%d", st, max, callers-max)
	}
}

func runIDf(i int) string {
	return "run-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

// reservedProvider builds a static reservation set for the tests below.
func reservedProvider(ticketIDs ...string) func() map[string]struct{} {
	return func() map[string]struct{} {
		set := make(map[string]struct{}, len(ticketIDs))
		for _, id := range ticketIDs {
			set[id] = struct{}{}
		}
		return set
	}
}

// TestPipelineQueueFIFODrainRespectsReservations is THE regression this
// whole mechanism turns on.
//
// slotFreed runs in the dying run's own goroutine and immediately signals
// the scheduler, so the FIFO drain is offered the just-freed slot
// microseconds after a failure — long before any 2s server-side admission
// tick could observe that the failure created a reservation. An admission
// gate that lived only in pkg/server would therefore be bypassed EVERY
// time, and the needs-attention card would find its slot already taken by
// whatever happened to be queued. Hence: the test lives here, at the gate.
func TestPipelineQueueFIFODrainRespectsReservations(t *testing.T) {
	q := newPipelineQueue(2)
	if admitted, _ := q.admitOrEnqueue("run-a", LaunchSpec{}); !admitted {
		t.Fatal("run-a should be admitted")
	}
	if admitted, _ := q.admitOrEnqueue("run-b", LaunchSpec{}); !admitted {
		t.Fatal("run-b should be admitted")
	}
	// A third launch waits in line.
	if admitted, pos := q.admitOrEnqueue("run-waiter", LaunchSpec{}); admitted || pos != 1 {
		t.Fatalf("run-waiter admitted=%v pos=%d, want queued at 1", admitted, pos)
	}
	// run-b dies. Its ticket now needs a human and holds the slot.
	q.setReservedProvider(reservedProvider("ticket-b"))
	q.slotFreed("run-b")

	if got := q.dequeueReady(); len(got) != 0 {
		t.Fatalf("dequeueReady admitted %d waiter(s) into a RESERVED slot — the fix cannot restart into it", len(got))
	}
	// Once the reservation is released (retry / close), the waiter proceeds.
	q.setReservedProvider(reservedProvider())
	got := q.dequeueReady()
	if len(got) != 1 || got[0].runID != "run-waiter" {
		t.Fatalf("after release, dequeueReady = %+v, want run-waiter", got)
	}
}

// TestPipelineQueueRelaunchConsumesItsOwnReservation pins the primary
// hazard: the card must not be refused by the slot it is holding FOR
// ITSELF. Max=1 makes the deadlock unavoidable if the exclusion is missing.
func TestPipelineQueueRelaunchConsumesItsOwnReservation(t *testing.T) {
	q := newPipelineQueue(1)
	q.setReservedProvider(reservedProvider("ticket-x"))

	// Someone else's launch is correctly refused — the slot is spoken for.
	if admitted, _ := q.admitOrEnqueue("run-other", LaunchSpec{PipelineTicketID: "ticket-y"}); admitted {
		t.Fatal("an unrelated ticket was admitted into a reserved slot")
	}
	// The holder's own relaunch spends the reservation and starts.
	admitted, _ := q.admitOrEnqueue("run-retry", LaunchSpec{PipelineTicketID: "ticket-x"})
	if !admitted {
		t.Fatal("the reserving ticket was refused by its OWN reservation — permanent deadlock")
	}
}

// TestPipelineQueueFullyReservedBoardStillLetsHoldersRestart pins the
// property that makes it safe for reservations to occupy EVERY slot.
//
// A board whose slots are all held refuses new work — that is the feature,
// not a bug. What must never happen is a state the operator cannot leave.
// It cannot: each holder can always relaunch itself (its own reservation is
// excluded), and Close releases. So the count is clamped to max, never to
// max-1: a max-1 ceiling would silently disable reservations entirely on a
// single-slot board, which is the setup where losing the slot hurts most.
func TestPipelineQueueFullyReservedBoardStillLetsHoldersRestart(t *testing.T) {
	q := newPipelineQueue(3)
	q.setReservedProvider(reservedProvider("t1", "t2", "t3", "t4", "t5"))

	// The wire field carries the RAW count — 5 pipelines genuinely need
	// attention, and that is the number on the lane the operator has to act
	// on. Clamping is an admission-arithmetic concern (clampedReserved), not
	// something to hide from the chip.
	if st := q.status(); st.Reserved != 5 {
		t.Fatalf("status().Reserved = %d, want the raw 5 (clamping belongs to the gate, not the wire)", st.Reserved)
	}
	// Unrelated work waits — nothing may take a held slot.
	if admitted, _ := q.admitOrEnqueue("run-fresh", LaunchSpec{}); admitted {
		t.Fatal("a fresh launch took a slot held for a broken pipeline")
	}
	// Every holder can still restart into its own slot: no deadlock.
	for _, ticket := range []string{"t1", "t2", "t3"} {
		if admitted, _ := q.admitOrEnqueue("retry-"+ticket, LaunchSpec{PipelineTicketID: ticket}); !admitted {
			t.Fatalf("holder %s could not restart into its own reserved slot", ticket)
		}
		q.slotFreed("retry-" + ticket)
	}
}

// TestPipelineQueueReservesOnASingleSlotBoard: max=1 is the configuration a
// max-1 clamp would have silently broken.
func TestPipelineQueueReservesOnASingleSlotBoard(t *testing.T) {
	q := newPipelineQueue(1)
	q.setReservedProvider(reservedProvider("ticket-x"))
	if admitted, _ := q.admitOrEnqueue("run-other", LaunchSpec{PipelineTicketID: "ticket-y"}); admitted {
		t.Fatal("the only slot was handed to another card while a broken pipeline held it")
	}
}

// TestPipelineQueueNilProviderIsTodaysBehaviour: an embedding with no board
// (CLI, cloud, every test that predates this) must see zero change.
func TestPipelineQueueNilProviderIsTodaysBehaviour(t *testing.T) {
	q := newPipelineQueue(2)
	for _, id := range []string{"r1", "r2"} {
		if admitted, _ := q.admitOrEnqueue(id, LaunchSpec{}); !admitted {
			t.Fatalf("%s should be admitted", id)
		}
	}
	if admitted, _ := q.admitOrEnqueue("r3", LaunchSpec{}); admitted {
		t.Fatal("r3 should have queued behind the cap")
	}
	st := q.status()
	if st.Reserved != 0 || st.Active != 2 || st.Waiting != 1 {
		t.Fatalf("status = %+v, want active=2 waiting=1 reserved=0", st)
	}
}

// TestClampedReserved covers the arithmetic directly.
func TestClampedReserved(t *testing.T) {
	set := map[string]struct{}{"a": {}, "b": {}}
	cases := []struct {
		name    string
		exclude string
		max     int
		want    int
	}{
		{"no exclusion", "", 5, 2},
		{"holder excluded", "a", 5, 1},
		{"non-holder ignored", "zz", 5, 2},
		{"clamped to max", "", 1, 1},
		{"clamp then exclude", "a", 1, 0},
		{"disabled cap reserves nothing", "", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampedReserved(set, c.exclude, c.max); got != c.want {
				t.Errorf("clampedReserved(%q, max=%d) = %d, want %d", c.exclude, c.max, got, c.want)
			}
		})
	}
	if got := clampedReserved(nil, "", 3); got != 0 {
		t.Errorf("clampedReserved(nil) = %d, want 0", got)
	}
}
