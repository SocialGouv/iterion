package native

import "testing"

// A launch give-up (the launch attempt cap) names no run: it is current for
// the card in the state it wrote, whatever run — if any — the card points
// at. A run give-up stays bound to its run id.
func TestGiveUpCurrent_LaunchGiveUpIsBoundToTheStateNotARun(t *testing.T) {
	launch := &GiveUp{State: StateBlocked, Attempts: 8, Launch: true}
	if !launch.Current(StateBlocked, "") {
		t.Error("a launch give-up must be current with no run at all")
	}
	if !launch.Current(StateBlocked, "run-old") {
		t.Error("a launch give-up must be current whatever run the card points at")
	}
	if launch.Current(StateReady, "") {
		t.Error("a give-up never describes a card that left the state it wrote")
	}
	run := &GiveUp{State: StateBlocked, RunID: "run-1", Attempts: 3}
	if !run.Current(StateBlocked, "run-1") || run.Current(StateBlocked, "run-2") || run.Current(StateBlocked, "") {
		t.Error("a run give-up must stay bound to its run id")
	}
	if SameGiveUp(launch, &GiveUp{State: StateBlocked, Attempts: 8}) {
		t.Error("Launch is part of a stamp's identity: a launch give-up and a run give-up are not the same stamp")
	}
}

// The filesystem twin round-trips the marker (the Mongo twin is pinned by the
// conformance suite): a stamp that lost it on disk would fall back to the
// run-bound rule and vanish from the needs-attention lane on the next read.
func TestStoreSetGaveUp_LaunchStampRoundTrips(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created, err := s.Create(Issue{Title: "never launched", State: StateReady})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState(created.ID, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGaveUp(created.ID, &GiveUp{State: StateBlocked, Attempts: 8, Reason: "refused 8 times", Launch: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GaveUp == nil || !got.GaveUp.Launch || !got.GaveUp.Current(got.State, "") {
		t.Fatalf("launch give-up not persisted as such: %+v", got.GaveUp)
	}
}
