package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestASignInSeedsTheJarThePaneTunnelHolds (r7c F4, invariants I4, I5): the
// machine's SIGN-IN seeds the jar the connection registry hands its pane
// tunnel — one jar per connection — so the pane's next request carries the
// new session's token, and the refused sign-out rides beside it in the SAME
// jar and entry (no second jar writes one keychain key). A jar the sign-in
// replaced is retired: its writes stand down, so an answer still in flight
// cannot land on the entry the sign-in wrote.
func TestASignInSeedsTheJarThePaneTunnelHolds(t *testing.T) {
	ctx := context.Background()
	c := newRealCloud(t)
	pane, store, _ := c.seedJar(t)
	stored := pane.storedRefresh()
	key := cloudRefreshKeyPrefix + "conn-1"
	// The pane's sign-out and the one the sign-in presents are both refused.
	c.sessions.failNextRevocations(2)
	if code, _ := paneLogout(t, pane, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
		t.Fatalf("precondition: the pane's sign-out was not answered the cloud's 500 (%d)", code)
	}
	res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		t.Fatal(err)
	}
	// The machine's SIGN-IN, on the jar the registry holds (prev == pane): the
	// sign-in presents first, keeps what the cloud refuses beside the new
	// session, and seeds the SAME jar the tunnel reads.
	if err := pane.prepareForSignIn(ctx, pane); err != nil {
		t.Fatalf("prepareForSignIn: %v", err)
	}
	if err := pane.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if pane.storedRefresh() != res.RefreshToken || !pane.LoggedIn() || pane.SignOutPending() {
		t.Fatalf("the pane's jar does not hold the new session (token %q, logged in %t, pending %t)", pane.storedRefresh(), pane.LoggedIn(), pane.SignOutPending())
	}
	if !pane.holdsSignOut() {
		t.Fatal("the sign-in abandoned the sign-out the cloud refused: I4 holds it beside the new session")
	}
	if got := store.m[key]; got != encodeStoredSession(res.RefreshToken, false, []string{stored}) {
		t.Fatalf("the keychain entry is %q, want the new token and the held sign-out: one jar wrote it", got)
	}
	if code, body, _ := paneGET(t, pane, c.url+"/api/auth/me"); code != http.StatusOK {
		t.Fatalf("a pane request through the registry's jar after the sign-in was answered %d %s, want 200 (the old jar answered 401)", code, body)
	}
	// The same jar presents the held sign-out, and it ends ITS session only.
	if err := pane.presentHeldSignOuts(ctx); err != nil {
		t.Fatalf("presenting the held sign-out: %v", err)
	}
	if c.lineageLive(t, stored) {
		t.Fatal("the held sign-out was not presented: the session stays live with the entry no longer carrying it")
	}
	if c.lineageLive(t, res.RefreshToken) == false {
		t.Fatal("the presentation ended the session the operator signed into")
	}
	if got := store.m[key]; got != res.RefreshToken {
		t.Fatalf("the entry is %q after the held sign-out was confirmed, want the new token alone", got)
	}

	// A jar the sign-in replaced — the shape the two-jar window left running —
	// is retired: an answer of its own still in flight cannot write the entry
	// the sign-in owns.
	stale := newCloudTokenJar("conn-1", c.url, store)
	if _, err := stale.hydrate(); err != nil {
		t.Fatal(err)
	}
	stale.retire()
	if err := stale.refreshNow(ctx); err != nil {
		t.Logf("the retired jar's refresh answered %v", err)
	}
	if got := store.m[key]; !strings.HasPrefix(got, res.RefreshToken) {
		t.Fatalf("the retired jar's write landed on the entry: %q", got)
	}
	if got := pane.storedRefresh(); got != res.RefreshToken {
		t.Fatalf("the live jar holds %q after the retired jar's answer", got)
	}
}

// TestARemovedConnectionPresentsWhatItOwesBeforeItsEntryGoes (r7c F5,
// invariant I2): removing a connection presents what it owes the cloud — its
// pending sign-out and the sign-outs held for sessions it replaced — before
// the keychain entry is deleted. A removal that cannot present is refused
// with the reason: discarding the hold would leave the session the operator
// ended live with nothing able to end it. The entry is deleted only once
// nothing is owed any more.
func TestARemovedConnectionPresentsWhatItOwesBeforeItsEntryGoes(t *testing.T) {
	ctx := context.Background()

	t.Run("a pending sign-out is presented, not discarded", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("precondition: the pane's sign-out was not answered the cloud's 500 (%d)", code)
		}
		if err := jar.removeConnection(ctx); err != nil {
			t.Fatalf("removing a connection whose sign-out the cloud confirms on this presentation: %v", err)
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the removal discarded the pending sign-out: the session the operator ended stays live with nothing able to end it")
		}
		if _, ok := store.m[cloudRefreshKeyPrefix+"conn-1"]; ok {
			t.Fatal("the removal kept the keychain entry of a connection it removed")
		}
	})

	t.Run("a held sign-out is presented, not discarded", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(2)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("precondition: the pane's sign-out was not answered the cloud's 500")
		}
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatal(err)
		}
		if err := jar.prepareForSignIn(ctx, jar); err != nil {
			t.Fatal(err)
		}
		if err := jar.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatal(err)
		}
		if !jar.holdsSignOut() {
			t.Fatal("precondition: the sign-out is not held beside the new session")
		}
		if err := jar.removeConnection(ctx); err != nil {
			t.Fatalf("removing a connection whose held sign-out the cloud confirms on this presentation: %v", err)
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the removal discarded the held sign-out: the replaced session stays live with nothing able to end it")
		}
		if _, ok := store.m[cloudRefreshKeyPrefix+"conn-1"]; ok {
			t.Fatal("the removal kept the keychain entry")
		}
		if !c.sessionLive(t, res.RefreshToken) {
			t.Fatal("the removal revoked the live session the operator did not ask to end: a removal forgets the local copy, it does not sign out")
		}
	})

	t.Run("a removal that cannot present is refused with the reason", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("precondition: the pane's sign-out was not answered the cloud's 500")
		}
		remover := newCloudTokenJar("conn-1", c.url, &keychainBlip{fakeStore: store})
		remover.store.(*keychainBlip).failGets.Store(1) // the keychain is locked
		err := remover.removeConnection(ctx)
		if err == nil {
			t.Fatal("a removal that could not read the hold it would discard reported success")
		}
		if !strings.Contains(err.Error(), "keychain") {
			t.Fatalf("the refusal does not name what blocked it: %v", err)
		}
		if got := store.m[cloudRefreshKeyPrefix+"conn-1"]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the refused removal rewrote the entry to %q", got)
		}
		if !c.lineageLive(t, stored) {
			t.Fatal("the refused removal ended the session")
		}
	})
}
