package boardmongo_test

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// runRenewAfterReleaseSuite answers, on BOTH twins, whether a renewal that
// lands AFTER the owner's release can re-stamp a lease on a card the
// dispatcher no longer owns. native.Adapter.RenewClaim detaches the
// store call from the caller's cancel, so a renewal can outlive Stop()
// and land after the release that follows it — the detached call IS
// Store.RenewClaim, and this suite pins what that write does then.
func runRenewAfterReleaseSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	iss, err := store.Create(native.Issue{Title: "late renewal", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	old, err := store.Claim(iss.ID, "host-a")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.ReleaseOwned(iss.ID, old); err != nil {
		t.Fatalf("ReleaseOwned: %v", err)
	}
	// 1. Released card: the stale token renews nothing — no lease comes
	// back on an unclaimed card.
	if err := store.RenewClaim(iss.ID, old); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("renew after release: err=%v, want ErrClaimConflict", err)
	}
	if cur, _ := store.Get(iss.ID); cur.Claim != "" || !cur.ClaimLeaseUntil.IsZero() {
		t.Fatalf("a late renewal re-stamped a released card: claim=%q lease=%s", cur.Claim, cur.ClaimLeaseUntil)
	}
	// 2. Re-claimed by another owner: the stale token is refused and the
	// new owner's lease is untouched.
	other, err := store.Claim(iss.ID, "host-b")
	if err != nil {
		t.Fatalf("Claim (b): %v", err)
	}
	before, _ := store.Get(iss.ID)
	if err := store.RenewClaim(iss.ID, old); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("stale renew against a new owner: err=%v, want ErrClaimConflict", err)
	}
	if cur, _ := store.Get(iss.ID); cur.Claim != "host-b" || !cur.ClaimLeaseUntil.Equal(before.ClaimLeaseUntil) || cur.ClaimEpoch != other.Epoch {
		t.Fatalf("a stale renewal touched the new owner's claim: %+v vs %+v", cur.ClaimLeaseUntil, before.ClaimLeaseUntil)
	}
	// 3. Re-claimed by the SAME marker (a fresh hold of the same worker):
	// the epoch moved, so the OLD token — the one a late renewal carries
	// — is refused; only the new token renews.
	if err := store.ReleaseOwned(iss.ID, other); err != nil {
		t.Fatalf("ReleaseOwned (b): %v", err)
	}
	fresh, err := store.Claim(iss.ID, "host-a")
	if err != nil {
		t.Fatalf("Claim (a again): %v", err)
	}
	if fresh.Epoch <= old.Epoch {
		t.Fatalf("a fresh hold must carry a newer epoch: old %d, fresh %d", old.Epoch, fresh.Epoch)
	}
	if err := store.RenewClaim(iss.ID, old); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("the previous generation's token renewed a fresh hold of the same marker: err=%v", err)
	}
	if err := store.RenewClaim(iss.ID, fresh); err != nil {
		t.Fatalf("the live token must still renew: %v", err)
	}
}
