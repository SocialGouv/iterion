package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A card the dispatcher gave up on BEFORE any run existed — the launch
// attempt cap of #814 — has no run to carry the flag, and a give-up bound to
// a run id would not describe it. The pipeline board's needs-attention lane
// must show it anyway (#841): filed among the done tickets, a card nobody
// could launch is the "invisible but not lost" failure again.

func launchGiveUp() *native.GiveUp {
	return &native.GiveUp{
		State:    native.StateBlocked,
		Attempts: 8,
		Reason:   "the launch was refused 8 times; last refusal: launch gate: concurrency_cap_exceeded",
		Launch:   true,
	}
}

// TestPipelineBoard_LaunchGiveUpLandsInNeedsAttention: the never-launched
// card (no run at all) is a ticket card in the needs-attention lane, carrying
// the stamp; a card an operator filed blocked by hand stays Closed.
func TestPipelineBoard_LaunchGiveUpLandsInNeedsAttention(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	issue, err := env.board.Create(native.Issue{Title: "Never launched", State: native.StateReady, Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.board.SetState(issue.ID, native.StateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := env.board.SetGaveUp(issue.ID, launchGiveUp()); err != nil {
		t.Fatal(err)
	}
	byHand, err := env.board.Create(native.Issue{Title: "Filed by hand", State: native.StateReady, Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.board.SetState(byHand.ID, native.StateBlocked); err != nil {
		t.Fatal(err)
	}

	cards := env.projection(t).Cards
	card := findPipelineCard(t, cards, "task:"+issue.ID)
	if card.ColumnID != pipelineColumnNeedsAttention {
		t.Errorf("a launch give-up landed in %q, want %q", card.ColumnID, pipelineColumnNeedsAttention)
	}
	if card.GaveUp == nil || !card.GaveUp.Launch || card.GaveUp.Attempts != 8 {
		t.Errorf("the card does not carry the launch give-up stamp: %+v", card.GaveUp)
	}
	if card.ReservesSlot {
		t.Error("a terminal ticket never reserves a slot")
	}
	ctrl := findPipelineCard(t, cards, "task:"+byHand.ID)
	if ctrl.ColumnID != pipelineColumnClosed || ctrl.GaveUp != nil {
		t.Errorf("a card filed by hand projected as %q with stamp %+v, want %q and no stamp", ctrl.ColumnID, ctrl.GaveUp, pipelineColumnClosed)
	}
}

// TestPipelineBoard_LaunchGiveUpOnACardWithAnOldRun: the card once ran (an
// older failed attempt), was restaged, and then could not be launched again
// — the launch give-up is current whatever run the card points at, so the
// run card takes the lane and the stamp.
func TestPipelineBoard_LaunchGiveUpOnACardWithAnOldRun(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	run := env.seedRun(t, "old-attempt", "review", store.RunStatusFailed, nil)
	issue, err := env.board.Create(native.Issue{Title: "Relaunch refused", State: native.StateReady, Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.board.SetLastRun(issue.ID, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.board.SetState(issue.ID, native.StateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := env.board.SetGaveUp(issue.ID, launchGiveUp()); err != nil {
		t.Fatal(err)
	}

	card := findPipelineCard(t, env.projection(t).Cards, "run:"+run.ID)
	if card.ColumnID != pipelineColumnNeedsAttention {
		t.Errorf("a launch give-up on a card with an old run landed in %q, want %q", card.ColumnID, pipelineColumnNeedsAttention)
	}
	if card.GaveUp == nil || !card.GaveUp.Launch {
		t.Errorf("the run card does not carry the launch give-up stamp: %+v", card.GaveUp)
	}
}
