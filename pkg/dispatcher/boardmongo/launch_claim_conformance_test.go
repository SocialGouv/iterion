package boardmongo_test

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// runLaunchClaimSuite pins the native.LaunchClaimer contract on BOTH
// twins. The studio admission loop's ClaimForLaunch is a CAS StateReady →
// StateInProgress that must ALSO read the claim family: the dispatcher
// wins a card with the CLAIM and moves it to in_progress afterwards, off
// the actor, so a claimed card can legally sit in Ready while its run is
// already launching. A launcher that reads the state alone starts a
// second run on it — and a backend without the method degrades the
// admission loop to exactly that launcher (a best-effort SetState), which
// on a multi-replica board is a cross-replica double-launch.
func runLaunchClaimSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	claimer := native.AsLaunchClaimer(store)
	if claimer == nil {
		t.Fatalf("%T does not implement native.LaunchClaimer — the pipeline admission loop degrades to a "+
			"best-effort SetState on this backend, a second launch authority that never reads the claim", store)
	}

	// A card outside StateReady is never won, whatever it carries.
	inbox, err := store.Create(native.Issue{Title: "inbox card", State: native.StateInbox})
	if err != nil {
		t.Fatalf("Create inbox: %v", err)
	}
	if _, won, err := claimer.ClaimForLaunch(inbox.ID); err != nil || won {
		t.Fatalf("ClaimForLaunch on an inbox card: won=%v err=%v, want a clean refusal", won, err)
	}
	if cur, _ := store.Get(inbox.ID); cur.State != native.StateInbox {
		t.Fatalf("a refused launch claim must leave the card untouched, state now %q", cur.State)
	}

	// Ready + unclaimed: won, moved, and the move is on the event log with
	// the same descriptive payload the FS twin writes.
	ready, err := store.Create(native.Issue{Title: "ready card", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create ready: %v", err)
	}
	got, won, err := claimer.ClaimForLaunch(ready.ID)
	if err != nil || !won || got == nil {
		t.Fatalf("ClaimForLaunch on a free ready card: got=%v won=%v err=%v, want won", got, won, err)
	}
	if got.State != native.StateInProgress {
		t.Fatalf("the returned issue must describe what was written: state %q, want %q", got.State, native.StateInProgress)
	}
	if cur, _ := store.Get(ready.ID); cur.State != native.StateInProgress {
		t.Fatalf("stored state %q after a won launch claim, want %q", cur.State, native.StateInProgress)
	}
	if p := lastStatePayload(t, store, ready.ID); p["from"] != native.StateReady || p["to"] != native.StateInProgress {
		t.Fatalf("launch claim event payload = %v, want from=ready to=in_progress", p)
	}
	// The same card cannot be won twice: the CAS is the mutual exclusion
	// between two admission ticks (or two replicas).
	if _, won, err := claimer.ClaimForLaunch(ready.ID); err != nil || won {
		t.Fatalf("a second ClaimForLaunch on the same card: won=%v err=%v, want refused", won, err)
	}

	// Ready under a LIVE lease: the dispatcher already owns this card and
	// its in_progress move is merely in flight. Refused, and nothing about
	// the claim family may move.
	held, err := store.Create(native.Issue{Title: "held card", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create held: %v", err)
	}
	tok, err := store.Claim(held.ID, "dispatcher-host-a")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, won, err := claimer.ClaimForLaunch(held.ID); err != nil || won {
		after, _ := store.Get(held.ID)
		t.Fatalf("ClaimForLaunch won a Ready card held under a LIVE lease by %q (epoch %d) — two launchers now own it "+
			"(state %q, claim %q); err=%v", tok.Marker, tok.Epoch, after.State, after.Claim, err)
	}
	cur, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("Get held: %v", err)
	}
	if cur.State != native.StateReady || cur.Claim != "dispatcher-host-a" || cur.ClaimEpoch != tok.Epoch || cur.ClaimLeaseUntil.IsZero() {
		t.Fatalf("a refused launch claim must leave the holder's claim intact: state=%q claim=%q epoch=%d lease=%s",
			cur.State, cur.Claim, cur.ClaimEpoch, cur.ClaimLeaseUntil)
	}
	// Once the holder releases, the card is free for the admission loop.
	if err := store.Release(held.ID, "dispatcher-host-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, won, err := claimer.ClaimForLaunch(held.ID); err != nil || !won {
		t.Fatalf("ClaimForLaunch after the holder released: won=%v err=%v, want won", won, err)
	}

	// A missing card is an error, never a silent "not won".
	if _, won, err := claimer.ClaimForLaunch("native:00000000-0000-0000-0000-000000000000"); !errors.Is(err, tracker.ErrNotFound) || won {
		t.Fatalf("ClaimForLaunch on a missing card: won=%v err=%v, want ErrNotFound", won, err)
	}
}
