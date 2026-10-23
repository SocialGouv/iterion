package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// keychainWritesFailing fails the next n writes (Set) — a keyring that locks
// before a write — and answers again afterwards. Reads and deletes pass
// through to the fake underneath.
type keychainWritesFailing struct {
	*fakeStore
	failSets atomic.Int64
}

func (s *keychainWritesFailing) Set(k, v string) error {
	if s.failSets.Add(-1) >= 0 {
		return errKeychainLocked
	}
	return s.fakeStore.Set(k, v)
}

// TestAClosedPaneDropsNoMemoryOnlyHold (pane closed, invariant I2): closing a
// pane deletes the connection's registry entry — the jar and the loop
// presenting what it owes. A jar that owes the cloud a sign-out presentation
// does not let the close drop it: the keychain entry is made to carry the
// hold first; an entry the keychain still refuses carries it by presentation;
// a close that would drop the hold is refused and names it.
func TestAClosedPaneDropsNoMemoryOnlyHold(t *testing.T) {
	ctx := context.Background()
	key := cloudRefreshKeyPrefix + "conn-1"

	t.Run("a signed-in pane closes clean and the entry survives the close", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		if err := jar.closeForPane(ctx); err != nil {
			t.Fatalf("closing a pane that owes the cloud nothing: %v", err)
		}
		if store.m[key] != stored {
			t.Fatalf("the close rewrote the keychain entry to %q: closing a pane is not a logout", store.m[key])
		}
		reopened := newCloudTokenJar("conn-1", c.url, store)
		if _, err := reopened.hydrate(); err != nil {
			t.Fatal(err)
		}
		if err := reopened.refreshNow(ctx); err != nil {
			t.Fatalf("the reopened connection did not renew the session the close kept: %v", err)
		}
	})

	t.Run("a close presents the hold the keychain does not carry", func(t *testing.T) {
		c := newRealCloud(t)
		store := &keychainWritesFailing{fakeStore: newFakeStore()}
		jar := registerJar(t, c, "desk@example.org", "conn-1", store)
		stored := jar.storedRefresh()
		store.failSets.Store(1 << 40) // the keychain locks before every write
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}
		if !jar.SignOutPending() {
			t.Fatal("the jar did not hold the sign-out")
		}
		if got := store.m[key]; got != stored {
			t.Fatalf("the keychain entry is %q, want it unmarked: the hold lives in the jar's memory only", got)
		}
		// The close presents the hold; the cloud confirms it; nothing is owed
		// any more and the jar and its entry go with the pane.
		if err := jar.closeForPane(ctx); err != nil {
			t.Fatalf("closing a pane whose hold the keychain could not carry, with the cloud confirming the presentation: %v", err)
		}
		if jar.SignOutPending() || jar.storedRefresh() != "" || jar.AccessToken() != "" {
			t.Fatalf("the close kept a session the presentation confirmed (pending %t, refresh %q, access %q)", jar.SignOutPending(), jar.storedRefresh(), jar.AccessToken())
		}
		if _, ok := store.m[key]; ok {
			t.Fatalf("the keychain kept the entry of a session the close presented: %q", store.m[key])
		}
		// A reopen adopts nothing: the session the operator signed out is
		// never renewed or presented again.
		reopened := newCloudTokenJar("conn-1", c.url, store)
		if hydrated, err := reopened.hydrate(); hydrated || err != nil {
			t.Fatalf("the reopen hydrated %v (err %v), want nothing stored", hydrated, err)
		}
		if reopened.SignOutPending() {
			t.Fatal("the reopen adopted the hold the close presented")
		}
		if err := reopened.refreshNow(ctx); !errors.Is(err, errNoCloudSession) {
			t.Fatalf("the reopened connection renewed the session the operator signed out: %v, want errNoCloudSession", err)
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the session the operator signed out stayed live past the close's presentation")
		}
	})

	t.Run("a close the presentation does not confirm is refused and names the hold", func(t *testing.T) {
		c := newRealCloud(t)
		store := &keychainWritesFailing{fakeStore: newFakeStore()}
		jar := registerJar(t, c, "desk@example.org", "conn-1", store)
		stored := jar.storedRefresh()
		store.failSets.Store(1 << 40)
		c.sessions.failNextRevocations(2) // the pane's, and the close's presentation
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}
		err := jar.closeForPane(ctx)
		if err == nil {
			t.Fatal("closing the pane dropped a sign-out the keychain does not carry and the cloud refused to confirm")
		}
		for _, want := range []string{"pane stays open", "sign-out", "remove the connection"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal does not say %q: %v", want, err)
			}
		}
		if !jar.SignOutPending() {
			t.Fatal("the refused close dropped the hold in memory")
		}
		if got := store.m[key]; got != stored {
			t.Fatalf("the refused close rewrote the keychain entry to %q", got)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the refused close presented nothing, yet the session is gone")
		}
		// The named remedy: once the keychain answers, the close writes the
		// hold onto the entry and proceeds.
		store.failSets.Store(0)
		if err := jar.closeForPane(ctx); err != nil {
			t.Fatalf("the close once the keychain answers: %v", err)
		}
		if got := store.m[key]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the entry is %q, want it carrying the mark the close left it with", got)
		}
	})

	t.Run("a persisted mark closes clean and a reopen presents it", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}
		if got := store.m[key]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the entry is %q, want the mark persisted with the token", got)
		}
		if err := jar.closeForPane(ctx); err != nil {
			t.Fatalf("closing a pane whose hold the keychain carries: %v", err)
		}
		reopened := newCloudTokenJar("conn-1", c.url, store)
		if _, err := reopened.hydrate(); err != nil {
			t.Fatal(err)
		}
		if !reopened.SignOutPending() {
			t.Fatal("the reopen adopted a marked entry as signed-in")
		}
		if err := reopened.refreshNow(ctx); !errors.Is(err, errCloudSignOutPending) {
			t.Fatalf("the reopen renewed a session whose sign-out is owed: %v, want errCloudSignOutPending", err)
		}
		if err := reopened.retrySignOut(ctx); err != nil {
			t.Fatalf("the reopen's presentation of the mark: %v", err)
		}
		if reopened.SignOutPending() || reopened.storedRefresh() != "" {
			t.Fatal("the presented mark did not clear the reopened jar")
		}
		if _, ok := store.m[key]; ok {
			t.Fatal("the presented mark kept the keychain entry")
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the reopen presented nothing: the session stayed live")
		}
	})

	t.Run("a hold whose keychain cannot even be read refuses the close", func(t *testing.T) {
		c := newRealCloud(t)
		store := newFakeStore()
		seeded := registerJar(t, c, "desk@example.org", "conn-1", store)
		stored := seeded.storedRefresh()
		blip := &keychainBlip{fakeStore: store}
		blip.failGets.Store(1 << 30)
		unread := newCloudTokenJar("conn-1", c.url, blip)
		if code, body := paneLogout(t, unread, http.DefaultTransport, c.url); code != http.StatusServiceUnavailable {
			t.Fatalf("the pane's sign-out over a locked keychain was answered %d %s, want 503", code, body)
		}
		if !unread.SignOutPending() {
			t.Fatal("the jar did not hold the sign-out it could not read for")
		}
		err := unread.closeForPane(ctx)
		if err == nil {
			t.Fatal("closing a pane whose hold the keychain cannot even read for reported success")
		}
		if !strings.Contains(err.Error(), "keychain entry could not be read") {
			t.Fatalf("the refusal does not name the read that failed: %v", err)
		}
		if !unread.SignOutPending() {
			t.Fatal("the refused close dropped the hold")
		}
		if got := store.m[key]; got != stored {
			t.Fatalf("the refused close rewrote the keychain entry to %q", got)
		}
	})
}

// TestAClosedPanePresentsTheSignOutsItsEntryCarries (pane closed): what the
// close presents and refuses on is the state the entry read left, not the
// snapshot the early return decided on. An unread jar holds a sign-out in
// memory only (the keychain could not be written when it was asked); the
// entry it reads at the close carries the held sign-outs of sessions the
// connection replaced, and that read stands the memory mark down — what the
// jar owes at the close is the held sign-out, not the live session it rides
// beside. A close that presents the live session ends a lineage the operator
// never asked to end. Once every presentation is confirmed the jar ends
// exactly what the presentations named: the held token goes, the live
// session stays, and the entry follows.
func TestAClosedPanePresentsTheSignOutsItsEntryCarries(t *testing.T) {
	ctx := context.Background()
	key := cloudRefreshKeyPrefix + "conn-1"

	setup := func(t *testing.T) (*realCloud, *cloudTokenJar, *fakeStore, string) {
		t.Helper()
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		// The live session rotates once; the entry carries a HELD sign-out
		// for the account's other session — the shape a held-beside sign-in
		// leaves, whose jar is gone (a restart's jar has not read the entry
		// yet).
		if err := jar.refreshNow(ctx); err != nil {
			t.Fatal(err)
		}
		live := jar.storedRefresh()
		if err := store.Set(key, encodeStoredSession(live, false, []string{other.RefreshToken})); err != nil {
			t.Fatal(err)
		}
		if !c.sessionLive(t, other.RefreshToken) {
			t.Fatal("precondition: the held sign-out's session must be live")
		}
		// The pane's jar: unread, holding a memory mark for a third session
		// (the keychain was locked when that sign-out was asked).
		third, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "tablet", "ip3")
		if err != nil {
			t.Fatal(err)
		}
		prev := newCloudTokenJar("conn-1", c.url, newFakeStore())
		if err := prev.seed(cloudAuthResult{AccessToken: third.AccessToken, AccessExpiry: third.AccessExpires, RefreshToken: third.RefreshToken}); err != nil {
			t.Fatal(err)
		}
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, prev, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the previous jar's sign-out was answered %d, want 500", code)
		}
		if !prev.SignOutPending() {
			t.Fatal("precondition: the previous jar must hold its sign-out")
		}
		unread := newCloudTokenJar("conn-1", c.url, store)
		unread.adoptSignOutHold(prev)
		if !unread.SignOutPending() || unread.read {
			t.Fatalf("precondition: the pane's jar must hold an unread mark (pending %t, read %t)", unread.SignOutPending(), unread.read)
		}
		return c, unread, store, live
	}

	t.Run("the keychain refuses every write the close needs", func(t *testing.T) {
		c, unread, store, live := setup(t)
		other := store.m[key]
		_, _, heldLines := decodeStoredSession(other)
		if len(heldLines) != 1 {
			t.Fatalf("precondition: the entry must carry the held sign-out, got %q", other)
		}
		otherToken := heldLines[0]
		failing := &keychainWritesFailing{fakeStore: store}
		failing.failSets.Store(1 << 40)
		unread.store = failing

		closeErr := unread.closeForPane(ctx)
		// The close presents what the entry read left the jar owing: the
		// held sign-out ends its session. The live session is not a sign-out
		// the close's ask named; it survives.
		if !c.lineageLive(t, live) {
			t.Fatal("the close presented the jar's live session: what it owes is what the entry read left, not the mark the read stood down")
		}
		if c.sessionLive(t, otherToken) {
			t.Fatal("the held sign-out the entry carries was not presented by the close")
		}
		// The keychain still refuses the writes, so the close is refused
		// with the remedies, not with the raw store error.
		if closeErr == nil {
			t.Fatal("the close proceeded while the keychain refuses every write")
		}
		for _, want := range []string{"pane stays open", "remove the connection"} {
			if !strings.Contains(closeErr.Error(), want) {
				t.Fatalf("the refusal does not say %q: %v", want, closeErr)
			}
		}
		// The refused close leaves the entry — the persisted state a reopen
		// reads — in place.
		if _, ok := store.m[key]; !ok {
			t.Fatal("the refused close deleted the keychain entry")
		}
	})

	t.Run("the keychain answers again by the sync write", func(t *testing.T) {
		c, unread, store, live := setup(t)
		other := store.m[key]
		_, _, heldLines := decodeStoredSession(other)
		if len(heldLines) != 1 {
			t.Fatalf("precondition: the entry must carry the held sign-out, got %q", other)
		}
		otherToken := heldLines[0]
		failing := &keychainWritesFailing{fakeStore: store}
		failing.failSets.Store(1) // the close's first write, then the keychain answers
		unread.store = failing

		if err := unread.closeForPane(ctx); err != nil {
			t.Fatalf("closing a pane whose presentations the cloud all confirmed: %v", err)
		}
		if !c.lineageLive(t, live) {
			t.Fatal("the close presented the jar's live session: what it owes is what the entry read left, not the mark the read stood down")
		}
		if c.sessionLive(t, otherToken) {
			t.Fatal("the held sign-out the entry carries was not presented by the close")
		}
		// The entry follows the jar: the presented held sign-out is gone and
		// the live session is what remains on it.
		if got := store.m[key]; got != encodeStoredSession(live, false, nil) {
			t.Fatalf("the entry is %q, want the live token alone: the presented hold is dropped and the session the close kept stays", got)
		}
		if unread.SignOutPending() || unread.holdsSignOut() {
			t.Fatal("the close kept a hold its presentations confirmed")
		}
		if unread.storedRefresh() != live {
			t.Fatalf("the close dropped the live session it kept: %q", unread.storedRefresh())
		}
	})
}
