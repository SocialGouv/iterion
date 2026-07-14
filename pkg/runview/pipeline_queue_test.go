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
