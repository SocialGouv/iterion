package dispatcher

import (
	"testing"
	"time"
)

// TestRunningEntry_AtomicHeartbeatBeatsLaggingActor reproduces the
// false-positive stall observed in the 2026-05-21 dogfood: the actor
// hadn't yet applied queued cmdEvent commands so LastEventAt was
// stale, but the synchronously-updated atomic heartbeat (set by
// OnEvent) carried the real freshness. reconcileStalled must read
// the freshest of the two — otherwise an active run gets cancelled.
func TestRunningEntry_AtomicHeartbeatBeatsLaggingActor(t *testing.T) {
	now := time.Now()

	r := &runningEntry{
		LastEventAt: now.Add(-15 * time.Minute), // actor lag: 15min stale
	}

	// OnEvent fires "right now"; the atomic captures it even though
	// the actor hasn't processed the corresponding cmdEvent yet.
	r.touchEvent(now)

	if got := r.lastEventTime(); now.Sub(got) > time.Second {
		t.Fatalf("lastEventTime() returned stale LastEventAt (%s) instead of atomic heartbeat (~%s)", got, now)
	}
}

// TestRunningEntry_HeartbeatPrefersWhicheverIsFresher: if the actor
// has caught up and LastEventAt is fresher than the atomic, prefer it.
func TestRunningEntry_HeartbeatPrefersWhicheverIsFresher(t *testing.T) {
	now := time.Now()

	r := &runningEntry{
		LastEventAt: now,
	}
	r.touchEvent(now.Add(-5 * time.Minute)) // older atomic

	if got := r.lastEventTime(); !got.Equal(now) {
		t.Fatalf("lastEventTime() should prefer fresher actor-applied LastEventAt; got %s want %s", got, now)
	}
}

// TestRunningEntry_HeartbeatZeroAtomicFallsBackToLastEventAt: an
// entry that never saw a touchEvent (synthetic / test fixture) must
// still use the legacy field.
func TestRunningEntry_HeartbeatZeroAtomicFallsBackToLastEventAt(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Minute)
	r := &runningEntry{LastEventAt: t0}
	if got := r.lastEventTime(); !got.Equal(t0) {
		t.Fatalf("zero atomic should fall back to LastEventAt; got %s want %s", got, t0)
	}
}
