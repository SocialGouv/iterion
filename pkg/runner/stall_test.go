package runner

import (
	"testing"
	"time"
)

// The tracker must trip only after hasWork has held — with the runner
// idle — across the whole stall window, fire the onset edge exactly
// once, and report recovery exactly once when the queue drains. The Oct
// 30 incident held five messages in ack-pending (NumPending reading
// zero!) for 15 hours while every pod polled an empty fetch — these
// transitions are the alarm that must fire in minutes instead (issue
// #2890).
func TestStallTrackerTransitions(t *testing.T) {
	t.Parallel()

	tr := stallTracker{stalledAfter: 10 * time.Minute}
	base := time.Now()

	// No work: nothing ever stalls.
	if stalled, edge := tr.observe(false, false, base); stalled || edge != stallNone {
		t.Fatalf("empty queue at %+v: stalled=%v edge=%v, want false/none", base, stalled, edge)
	}

	// Work under the window: samples advance the clock, no stall.
	if stalled, edge := tr.observe(true, false, base.Add(5*time.Minute)); stalled || edge != stallNone {
		t.Fatalf("work under the window: stalled=%v edge=%v, want false/none", stalled, edge)
	}

	// Crossing the window: the episode clock started at the first
	// hasWork sample (+5m), so onset lands at +15m, exactly once.
	stalled, edge := tr.observe(true, false, base.Add(15*time.Minute))
	if !stalled || edge != stallOnset {
		t.Fatalf("at the window edge: stalled=%v edge=%v, want true/onset", stalled, edge)
	}
	if stalled, edge := tr.observe(true, false, base.Add(16*time.Minute)); !stalled || edge != stallNone {
		t.Fatalf("sustained stall: stalled=%v edge=%v, want true/none (no repeated onset)", stalled, edge)
	}

	// Drain: recovery, exactly once, and the episode clock resets.
	if stalled, edge := tr.observe(false, false, base.Add(17*time.Minute)); stalled || edge != stallRecovered {
		t.Fatalf("drain after stall: stalled=%v edge=%v, want false/recovered", stalled, edge)
	}
	if stalled, edge := tr.observe(false, false, base.Add(18*time.Minute)); stalled || edge != stallNone {
		t.Fatalf("drained idle: stalled=%v edge=%v, want false/none", stalled, edge)
	}
}

// The incident's exact signature: work sits ONLY in ack-pending (five
// delivered-never-acknowledged messages, NumPending reading zero) while
// the loop idles — the tracker must treat ack-pending alone as work and
// stall on it.
func TestStallTrackerAcksPendingAloneStall(t *testing.T) {
	t.Parallel()

	tr := stallTracker{stalledAfter: 10 * time.Minute}
	base := time.Now()

	// hasWork is the caller's pending>0 || ackPending>0: the tracker
	// itself only sees work-or-not, which is the contract. The first
	// sample starts the episode clock; the window fires on a later one.
	if stalled, edge := tr.observe(true, false, base); stalled || edge != stallNone {
		t.Fatalf("first ack-pending sample: stalled=%v edge=%v, want false/none (clock start)", stalled, edge)
	}
	if stalled, edge := tr.observe(true, false, base.Add(10*time.Minute)); !stalled || edge != stallOnset {
		t.Fatalf("ack-pending-only wedge: stalled=%v edge=%v, want true/onset", stalled, edge)
	}
}

// A delivery mid-run holds ack-pending for as long as the run takes,
// and the loop goes quiet while it executes: busy suppresses the stall
// however long the work holds.
func TestStallTrackerBusyRunnerNeverStalls(t *testing.T) {
	t.Parallel()

	tr := stallTracker{stalledAfter: 10 * time.Minute}
	base := time.Now()

	for _, at := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 30 * time.Minute, 4 * time.Hour,
	} {
		if stalled, edge := tr.observe(true, true, base.Add(at)); stalled || edge != stallNone {
			t.Fatalf("busy runner at %s: stalled=%v edge=%v, want false/none", at, stalled, edge)
		}
	}

	// The run finished and the loop went idle with nothing held: no
	// stall, and the episode clock restarted from this sample.
	if stalled, edge := tr.observe(false, false, base.Add(4*time.Hour+time.Minute)); stalled || edge != stallNone {
		t.Fatalf("idle after busy: stalled=%v edge=%v, want false/none", stalled, edge)
	}
	if stalled, edge := tr.observe(true, false, base.Add(4*time.Hour+time.Minute)); stalled || edge != stallNone {
		t.Fatalf("work right after busy: stalled=%v edge=%v, want false/none (clock restarted)", stalled, edge)
	}
}

// A busy sample is proof of progress — work was claimed and a delivery
// executes — so it resets the episode clock: the stall window measures
// time since the LAST sign of life, and a wedge re-alarms 10 minutes
// after that.
func TestStallTrackerBusySampleResetsTheClock(t *testing.T) {
	t.Parallel()

	tr := stallTracker{stalledAfter: 10 * time.Minute}
	base := time.Now()

	tr.observe(true, false, base.Add(5*time.Minute)) // episode starts
	tr.observe(true, true, base.Add(6*time.Minute))  // busy: progress, clock restarts
	if stalled, edge := tr.observe(true, false, base.Add(16*time.Minute)); stalled || edge != stallNone {
		t.Fatalf("under the window since the busy blip: stalled=%v edge=%v, want false/none", stalled, edge)
	}
	if stalled, edge := tr.observe(true, false, base.Add(16*time.Minute+10*time.Minute)); !stalled || edge != stallOnset {
		t.Fatalf("past the window since the busy blip: stalled=%v edge=%v, want true/onset", stalled, edge)
	}
}

// Zero config applies the 10-minute default rather than tripping on
// the first sample.
func TestStallTrackerDefaultWindow(t *testing.T) {
	t.Parallel()

	tr := stallTracker{}
	if stalled, _ := tr.observe(true, false, time.Now()); stalled {
		t.Fatal("default window tripped on the first sample")
	}
}
