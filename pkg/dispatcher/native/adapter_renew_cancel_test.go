package native

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAdapterRenewClaim_HonoursCancelMidCall: claimSession documents that
// "Stop() is not hostage to a slow renewal" and cancels the renewal's
// context to make it so. That only holds if the adapter honours the
// cancel for the WHOLE call — the store's renew takes no context and
// blocks on the store-wide lock, so an entry-only check left Stop()
// waiting for whatever held that lock, ON THE ACTOR GOROUTINE, and made
// the session's context.Canceled arm unreachable from the dispatcher.
// The cloud twin already honours it (RenewClaimCtx); this backend did
// not.
//
// In-package on purpose: the probe has to hold the store's own lock, the
// way a sweep or a bulk column migration does.
func TestAdapterRenewClaim_HonoursCancelMidCall(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := st.Create(Issue{Title: "held", State: StateReady})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(st)
	// Read the lease BEFORE the probe takes the store lock: Get needs it too.
	before, err := st.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}

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
	select {
	case e := <-errc:
		t.Fatalf("renew returned %v while the store lock was held — this probe's premise is broken", e)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	select {
	case e := <-errc:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("renew returned %v, want context.Canceled", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renew ignored the cancel and stayed blocked on the store lock — Stop() waits for the heartbeat " +
			"loop on the actor goroutine, so the whole dispatcher is held hostage by one slow renewal")
	}

	// The detached renewal is still parked on the lock. Free it and WAIT
	// for its write to land before returning: the adapter documents that
	// a late renewal merely extends a lease we still own, and t.TempDir's
	// cleanup would otherwise race that write ("directory not empty").
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, err := st.Get(iss.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.ClaimLeaseUntil.After(before.ClaimLeaseUntil) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached renewal never landed once the store lock was freed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
