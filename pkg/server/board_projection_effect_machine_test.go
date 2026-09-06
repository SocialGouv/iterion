package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestBoardProjection_DoesNotReflectAMachineTransition (#798): the fast path
// runs the same reflect as the pass, so a column iterion wrote on its own
// authority is refused here too — and refused on the CARD's persisted
// provenance, not on the event: a row that reaches the worker without its
// reason (or after a later machine write on the same card) must still leave
// the roadmap alone.
func TestBoardProjection_DoesNotReflectAMachineTransition(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 20, 55, 0, 0, time.UTC)
	id := seedSynced(t, board, 798, native.StateReady, "Planned", at)
	tok, err := board.Claim(id, tracker.ReaperMarkerPrefix+"host-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := board.SetStateOwned(id, native.StateBlocked, tok); err != nil {
		t.Fatalf("park: %v", err)
	}
	if !mustGet(t, board, id).StateByMachine() {
		t.Fatalf("fixture: provenance %q is not machine", mustGet(t, board, id).StateReason)
	}
	bc := &fakeBoardClient{project: testProject()}
	p, _ := projectionEffectWorld(t, bc, board)

	// With the event's provenance…
	ev := movedCardEvent(id, native.StateReady)
	ev.Payload["reason"] = tracker.ReasonWatchdog
	if err := p.ReflectCard(context.Background(), ev); err != nil {
		t.Fatalf("ReflectCard: %v", err)
	}
	// …and without it (a row queued by an older binary).
	if err := p.ReflectCard(context.Background(), movedCardEvent(id, native.StateReady)); err != nil {
		t.Fatalf("ReflectCard (bare event): %v", err)
	}
	if len(bc.writes) != 0 {
		t.Fatalf("writes = %+v, want none — a watchdog park is not a move the roadmap follows", bc.writes)
	}
	if rec := mustGet(t, board, id).External.Project.Status; rec != "Planned" {
		t.Fatalf("recorded status = %q, want %q untouched — nothing was pushed, so nothing may be recorded", rec, "Planned")
	}
}
