package boardmongo_test

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// countStateEvents counts EvtIssueState records for id — the oracle for
// "nothing was written" on a drifted CAS.
func countStateEvents(t *testing.T, s native.BoardStore, id string) int {
	t.Helper()
	n := 0
	if err := s.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueState && e.IssueID == id {
			n++
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	return n
}

// runOwnedFromSuite pins BoardStore.SetStateOwnedFrom on both twins: ONE
// CAS carrying the fence AND the source state. It is what the finish
// worker's auto-transition writes through — a state probe followed by a
// fenced overwrite lost an operator move that landed in between, while
// the watchdog (which judges on the CAS-observed state) honoured it.
func runOwnedFromSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	iss, err := store.Create(native.Issue{Title: "owned-from card", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := store.Claim(iss.ID, "worker-a")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Lands when the card is exactly where the owner believes it is.
	got, changed, err := store.SetStateOwnedFrom(iss.ID, native.StateReady, native.StateInProgress, tok)
	if err != nil || !changed || got == nil || got.State != native.StateInProgress {
		t.Fatalf("SetStateOwnedFrom(ready→in_progress): got=%v changed=%v err=%v, want the move", got, changed, err)
	}
	if p := lastStatePayload(t, store, iss.ID); p["from"] != native.StateReady || p["to"] != native.StateInProgress {
		t.Fatalf("event payload = %v, want from=ready to=in_progress", p)
	}
	events := countStateEvents(t, store, iss.ID)

	// Drifted: the owner still believes "ready" but the card moved on —
	// nothing is written, no event, no error, changed=false.
	got, changed, err = store.SetStateOwnedFrom(iss.ID, native.StateReady, native.StateDone, tok)
	if err != nil || changed {
		t.Fatalf("drifted CAS: changed=%v err=%v, want changed=false and no error", changed, err)
	}
	if got == nil || got.State != native.StateInProgress {
		t.Fatalf("drifted CAS must return the card as it stands (in_progress), got %+v", got)
	}
	if cur, _ := store.Get(iss.ID); cur.State != native.StateInProgress {
		t.Fatalf("a drifted CAS wrote anyway: state now %q", cur.State)
	}
	if n := countStateEvents(t, store, iss.ID); n != events {
		t.Fatalf("a drifted CAS emitted %d state event(s)", n-events)
	}

	// Same source and target: nothing to perform, changed=false — but the
	// fence still speaks.
	if _, changed, err := store.SetStateOwnedFrom(iss.ID, native.StateInProgress, native.StateInProgress, tok); err != nil || changed {
		t.Fatalf("no-op CAS: changed=%v err=%v, want changed=false and no error", changed, err)
	}

	// Stolen: the claim moved to another holder, so the old token is
	// refused with the typed conflict — never reported as a drift.
	if err := store.Release(iss.ID, "worker-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	tok2, err := store.Claim(iss.ID, "worker-b")
	if err != nil {
		t.Fatalf("Claim (b): %v", err)
	}
	if _, _, err := store.SetStateOwnedFrom(iss.ID, native.StateInProgress, native.StateDone, tok); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("stale token on a matching state: err=%v, want ErrClaimConflict", err)
	}
	if _, _, err := store.SetStateOwnedFrom(iss.ID, native.StateInProgress, native.StateInProgress, tok); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("stale token on a no-op: err=%v, want ErrClaimConflict (an idempotent no-op must not mask a stolen claim)", err)
	}
	if cur, _ := store.Get(iss.ID); cur.State != native.StateInProgress || cur.Claim != "worker-b" {
		t.Fatalf("a refused CAS wrote anyway: state=%q claim=%q", cur.State, cur.Claim)
	}

	// The terminal sink binds the owned family too: a declared terminal
	// source is refused before the card is even read.
	if _, _, err := store.SetStateOwnedFrom(iss.ID, native.StateDone, native.StateReady, tok2); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("terminal source: err=%v, want ErrTransitionRejected (wrapped ErrTerminalStateExit)", err)
	}
	if _, _, err := store.SetStateOwnedFrom(iss.ID, native.StateInProgress, "does-not-exist", tok2); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("unknown target: err=%v, want ErrTransitionRejected", err)
	}

	// The new holder's move lands, and a done filing cascades like every
	// other done write.
	if _, changed, err := store.SetStateOwnedFrom(iss.ID, native.StateInProgress, native.StateDone, tok2); err != nil || !changed {
		t.Fatalf("holder's CAS: changed=%v err=%v, want the move", changed, err)
	}

	// A missing card is an error, never a silent drift.
	if _, _, err := store.SetStateOwnedFrom("native:00000000-0000-0000-0000-000000000000", native.StateReady, native.StateDone, tok2); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing card: err=%v, want ErrNotFound", err)
	}
}
