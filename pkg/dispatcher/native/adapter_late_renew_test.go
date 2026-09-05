package native

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestAdapterRenewClaim_LateRenewalCannotReStampAReleasedCard: the
// adapter detaches the store renewal from the caller's cancel (so Stop()
// is not hostage to the store lock), which means a renewal can land AFTER
// the release that follows Stop(). It cannot re-stamp anything: the
// detached call is Store.RenewClaim, fenced on (claim, epoch), and the
// release cleared the claim. Deterministic: the store lock is held while
// the release lands, so the parked renewal provably runs after it.
func TestAdapterRenewClaim_LateRenewalCannotReStampAReleasedCard(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := st.Create(Issue{Title: "held", State: StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(st)

	release, held := make(chan struct{}), make(chan struct{})
	go func() {
		st.mu.Lock()
		close(held)
		<-release
		st.mu.Unlock()
	}()
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.RenewClaim(ctx, iss.ID, tok) }()
	time.Sleep(20 * time.Millisecond) // the renewal is parked on the lock
	cancel()
	if e := <-errc; !errors.Is(e, context.Canceled) {
		t.Fatalf("renew returned %v, want context.Canceled", e)
	}

	// Stop() has returned to the caller; the owner now releases — under
	// the lock the detached renewal is still waiting for.
	cur := cloneIssue(st.index[iss.ID])
	if err := st.releaseLocked(cur, tok.Marker); err != nil {
		t.Fatal(err)
	}
	close(release)

	// The parked renewal runs next. Whatever it does, the released card
	// must stay released: probe the same fenced write it performs, then
	// read the card back.
	if err := st.RenewClaim(iss.ID, tok); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("the late renewal's own write must be refused on a released card, got %v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, err := st.Get(iss.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Claim != "" || !got.ClaimLeaseUntil.IsZero() {
			t.Fatalf("a late renewal re-stamped a released card: claim=%q lease=%s", got.Claim, got.ClaimLeaseUntil)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
