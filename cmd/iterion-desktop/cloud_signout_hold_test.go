package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldTransport holds every request to path until release is closed, and
// signals the first arrival: a refresh in flight while something else happens.
type heldTransport struct {
	base    http.RoundTripper
	path    string
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == h.path {
		h.once.Do(func() { close(h.arrived) })
		<-h.release
	}
	return h.base.RoundTrip(req)
}

// holdRefreshAtTheTransport makes the jar's own calls go through a transport
// that holds the refresh until the returned release is closed.
func holdRefreshAtTheTransport(jar *cloudTokenJar) *heldTransport {
	h := &heldTransport{base: http.DefaultTransport, path: "/api/auth/refresh", arrived: make(chan struct{}), release: make(chan struct{})}
	jar.hc = &http.Client{Transport: h, Timeout: cloudHTTPTimeout}
	return h
}

// TestJar_ARefreshInFlightAcrossAHeldSignOutDoesNotReAdoptTheSession: the
// background loop's renewal is on the wire when the pane's sign-out fails. Its
// answer is the SAME session's successor — a refresh renews what the operator
// just ended — so installing it would put that session back in use, hand the
// pane a working access token and write the keychain an unmarked successor a
// restart adopts. The hold moves the jar's epoch, which is what drops the
// answer, and the loop that has to present the sign-out survives the drop
// instead of reading it as an expiry.
func TestJar_ARefreshInFlightAcrossAHeldSignOutDoesNotReAdoptTheSession(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"
	ctx := context.Background()

	t.Run("the answer is dropped and the hold stands", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		held := holdRefreshAtTheTransport(jar)

		done := make(chan error, 1)
		go func() { done <- jar.refreshNow(ctx) }()
		<-held.arrived

		c.sessions.failNextRevocations(1)
		if code, body := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out whose revocation failed was answered %d %s, want the cloud's 500", code, body)
		}
		if !jar.SignOutPending() {
			t.Fatal("the pane's failed sign-out left no mark")
		}
		close(held.release)
		if err := <-done; err == nil {
			t.Fatal("the refresh answered across the held sign-out reported success: its successor was installed")
		}

		if !jar.SignOutPending() || jar.AccessToken() != "" || jar.LoggedIn() {
			t.Fatalf("the refresh answered across the held sign-out put the session back in use (pending %t, access %q, logged in %t)", jar.SignOutPending(), jar.AccessToken(), jar.LoggedIn())
		}
		if jar.storedRefresh() != stored {
			t.Fatalf("the jar holds %q, want the token that can still end the session", jar.storedRefresh())
		}
		if store.m[key] != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the keychain entry is %q, want the token carrying the held sign-out", store.m[key])
		}
		// The restart that reads that entry: still held, still presenting.
		restarted := newCloudTokenJar("conn-1", c.url, store)
		if hydrated, herr := restarted.hydrate(); !hydrated || herr != nil {
			t.Fatalf("hydrate after the restart = %v, %v; want the stored token", hydrated, herr)
		}
		if !restarted.SignOutPending() {
			t.Fatal("the restart adopted the session the operator signed out of")
		}
		if rerr := restarted.refreshNow(ctx); !errors.Is(rerr, errCloudSignOutPending) {
			t.Fatalf("the restarted connection's renewal: %v, want errCloudSignOutPending", rerr)
		}
		// The refresh the jar dropped DID rotate on the cloud, so the token
		// the jar kept is a retired one — and the session it belongs to is
		// still live, with a successor nobody holds. Presenting that token
		// signs out its LINEAGE, which is what ends the session.
		if !c.lineageLive(t, stored) {
			t.Fatal("the cloud ended the session it answered 500 for")
		}
		if err := jar.retrySignOut(ctx); err != nil {
			t.Fatalf("presenting the held sign-out: %v", err)
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the presented sign-out left the session live: the token the jar kept across the dropped refresh cannot end it")
		}
	})

	t.Run("the loop presents the sign-out instead of asking for a sign-in", func(t *testing.T) {
		c := newRealCloud(t)
		jar, _, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		held := holdRefreshAtTheTransport(jar)

		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		expired := make(chan string, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			cloudRefreshLoop(loopCtx, jar, func(id string) { expired <- id })
		}()
		// The loop's own renewal, held on the wire.
		jar.kickRefresh()
		select {
		case <-held.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the loop never started a renewal")
		}

		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		close(held.release)

		// The loop must live on and present the sign-out. A dropped refresh
		// answer carries 401, which any other reading takes for an expiry.
		// The session ends when its LINEAGE is signed out: the refresh the
		// jar dropped had already retired the row of the token it holds.
		deadline := time.Now().Add(60 * time.Second)
		for c.lineageLive(t, stored) {
			select {
			case id := <-expired:
				t.Fatalf("the loop asked the shell to sign %s in again: a sign-out the operator asked for is not an expiry", id)
			case <-done:
				t.Fatal("the loop returned over the refresh answer it dropped: nothing is left to present the sign-out")
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("the loop never presented the held sign-out")
			}
			time.Sleep(50 * time.Millisecond)
		}
		select {
		case id := <-expired:
			t.Fatalf("the loop asked the shell to sign %s in again after the sign-out was confirmed", id)
		default:
		}
	})
}

// TestJar_ASignInPresentsTheHeldSignOutAndNeverAbandonsIt drives what the
// shell does right after a sign-out the cloud did not confirm: it asks for a
// new sign-in (the tunnel answers the pane 401 for a held session), and a
// sign-in builds a NEW jar over the SAME keychain key. The entry it replaces
// is the only copy of the token that can end the session the operator signed
// out of — the cloud keeps it live for the whole refresh TTL — so the sign-in
// presents that sign-out FIRST, and keeps the token when the cloud does not
// confirm it.
func TestJar_ASignInPresentsTheHeldSignOutAndNeverAbandonsIt(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"
	ctx := context.Background()

	t.Run("the sign-in presents it and the cloud confirms", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}

		// LoginCloud / finishCloudConnect: a new jar over the same key.
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("prepareForSignIn: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the sign-in left the session the operator signed out of live on the cloud")
		}
		if fresh.holdsSignOut() {
			t.Fatal("the sign-in held a sign-out the cloud confirmed")
		}
		if store.m[key] != res.RefreshToken {
			t.Fatalf("the keychain entry is %q, want the new session's token alone", store.m[key])
		}
		if fresh.SignOutPending() || fresh.AccessToken() == "" || !fresh.LoggedIn() {
			t.Fatalf("the new session is held back by the old mark (pending %t, access %q)", fresh.SignOutPending(), fresh.AccessToken())
		}
		if code, body, _ := paneGET(t, fresh, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("a pane request after the new sign-in was answered %d %s, want 200", code, body)
		}
		if !c.sessionLive(t, other.RefreshToken) {
			t.Fatal("the sign-in ended the account's other device")
		}
	})

	t.Run("a sign-in path that only seeds presents it too", func(t *testing.T) {
		// seed is the one door every sign-in goes through, and the next path
		// added will call it and nothing else: the presentation lives THERE,
		// not in each caller, so this drives seed alone.
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if c.lineageLive(t, stored) {
			t.Fatal("a sign-in that only seeded left the session the operator signed out of live, with its only token overwritten")
		}
		if store.m[key] != res.RefreshToken {
			t.Fatalf("the keychain entry is %q, want the new session's token alone", store.m[key])
		}
		if !fresh.LoggedIn() {
			t.Fatal("the new session is not usable")
		}
	})

	t.Run("a sign-out the cloud still refuses is held beside the new session and presented until it confirms", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		// The pane's sign-out AND the one the sign-in presents both fail.
		c.sessions.failNextRevocations(2)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}

		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("prepareForSignIn: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// The new session works, and the old token is kept, not overwritten.
		if !fresh.LoggedIn() || fresh.SignOutPending() {
			t.Fatalf("the new session is not usable (logged in %t, pending %t)", fresh.LoggedIn(), fresh.SignOutPending())
		}
		if !fresh.holdsSignOut() {
			t.Fatal("the sign-in abandoned the sign-out the cloud did not confirm: the session stays live with nothing able to end it")
		}
		if got := store.m[key]; got != encodeStoredSession(res.RefreshToken, false, []string{stored}) {
			t.Fatalf("the keychain entry is %q, want the new token plus the held sign-out: a restart has to keep presenting it", got)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the cloud ended a session it answered 500 for")
		}

		// A restart reads both: the session it adopts and the sign-out it owes.
		restarted := newCloudTokenJar("conn-1", c.url, store)
		if hydrated, herr := restarted.hydrate(); !hydrated || herr != nil {
			t.Fatalf("hydrate after the restart = %v, %v; want the new session's token", hydrated, herr)
		}
		if restarted.storedRefresh() != res.RefreshToken || restarted.SignOutPending() {
			t.Fatalf("the restart holds %q (pending %t), want the new session unmarked", restarted.storedRefresh(), restarted.SignOutPending())
		}
		if !restarted.holdsSignOut() {
			t.Fatal("the restart lost the sign-out this connection still owes the cloud")
		}
		// The cloud answers now: the presentation ends it, and the entry keeps
		// the live session alone.
		if err := restarted.presentHeldSignOuts(ctx); err != nil {
			t.Fatalf("presenting the held sign-out once the cloud answers: %v", err)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the presented sign-out left the session live on the cloud")
		}
		if restarted.holdsSignOut() || store.m[key] != res.RefreshToken {
			t.Fatalf("the confirmed sign-out stayed in the entry (%q)", store.m[key])
		}
		if code, body, _ := paneGET(t, restarted, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("a pane request of the session kept alongside was answered %d %s, want 200", code, body)
		}
	})

	t.Run("the background loop presents it beside the renewals", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(2)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("prepareForSignIn: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if !c.sessionLive(t, stored) || !fresh.holdsSignOut() {
			t.Fatalf("precondition: the old session live %t, held %t", c.sessionLive(t, stored), fresh.holdsSignOut())
		}

		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		expired := make(chan string, 4)
		go func() { cloudRefreshLoop(loopCtx, fresh, func(id string) { expired <- id }) }()
		fresh.kickRefresh()
		deadline := time.Now().Add(60 * time.Second)
		for c.lineageLive(t, stored) {
			if time.Now().After(deadline) {
				t.Fatal("the loop never presented the sign-out held for the session the sign-in replaced")
			}
			time.Sleep(50 * time.Millisecond)
		}
		// The sign-out ends ITS session and no other: the one the operator
		// signed into is renewed by the same loop and keeps serving the pane.
		if !c.lineageLive(t, res.RefreshToken) {
			t.Fatal("presenting the held sign-out ended the session the operator signed into")
		}
		if code, body, _ := paneGET(t, fresh, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("a pane request of the new session was answered %d %s, want 200", code, body)
		}
		select {
		case id := <-expired:
			t.Fatalf("the loop asked the shell to sign %s in again while renewing a live session", id)
		default:
		}
	})

	t.Run("the loop outlives the session it renews to present it", func(t *testing.T) {
		// The live session ends — the cloud refuses its renewal — and the
		// loop's own exit is where a held sign-out would be abandoned: the
		// pass that learns the session is over is the pass whose presentation
		// failed, and nothing else is left running.
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(2)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("prepareForSignIn: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if !fresh.holdsSignOut() || !c.lineageLive(t, stored) {
			t.Fatalf("precondition: held %t, the old session live %t", fresh.holdsSignOut(), c.lineageLive(t, stored))
		}
		// The session the operator signed into ends elsewhere (another device
		// signed this one out), so the loop's renewal is refused.
		if _, err := c.svc.LogoutOf(ctx, res.RefreshToken, ""); err != nil {
			t.Fatalf("ending the new session: %v", err)
		}
		// And the presentation of THAT pass fails, so the hold survives the
		// pass that discovers the session is over.
		c.sessions.failNextRevocations(1)

		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		expired := make(chan string, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			cloudRefreshLoop(loopCtx, fresh, func(id string) { expired <- id })
		}()
		fresh.kickRefresh()
		deadline := time.Now().Add(60 * time.Second)
		for c.lineageLive(t, stored) {
			if time.Now().After(deadline) {
				t.Fatal("the loop stopped with a sign-out it holds unconfirmed: the session stays live with nothing able to end it")
			}
			time.Sleep(50 * time.Millisecond)
		}
		select {
		case <-expired:
		case <-time.After(5 * time.Second):
			t.Fatal("the shell was never asked for a sign-in for the session the cloud refused")
		}
	})

	t.Run("the loop outlives its own confirmed sign-out to present it", func(t *testing.T) {
		// The loop's other exit: the jar's OWN sign-out is confirmed while a
		// sign-out held for a session it replaced is not. The exit is the same
		// abandonment, so it owes the same presentations.
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		first := jar.storedRefresh()
		c.sessions.failNextRevocations(2)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		fresh := newCloudTokenJar("conn-1", c.url, store)
		if err := fresh.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("prepareForSignIn: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// The operator signs out of the NEW session too, and that fails: the
		// jar now owes two sign-outs.
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, fresh, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the second pane sign-out was not answered the cloud's 500")
		}
		if !fresh.SignOutPending() || !fresh.holdsSignOut() {
			t.Fatalf("precondition: pending %t, held %t", fresh.SignOutPending(), fresh.holdsSignOut())
		}
		// In the pass that confirms the jar's own sign-out, the held one
		// fails: only the exit's own presentations can still end it.
		c.sessions.failNextRevocations(1)

		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			cloudRefreshLoop(loopCtx, fresh, func(string) {})
		}()
		deadline := time.Now().Add(60 * time.Second)
		for c.lineageLive(t, first) || c.lineageLive(t, res.RefreshToken) {
			if time.Now().After(deadline) {
				t.Fatalf("the loop stopped with a session live (replaced %t, its own %t)", c.lineageLive(t, first), c.lineageLive(t, res.RefreshToken))
			}
			time.Sleep(50 * time.Millisecond)
		}
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the loop never returned once both sign-outs were confirmed")
		}
		if v, ok := store.m[key]; ok {
			t.Fatalf("both sign-outs are confirmed and the keychain entry is still %q", v)
		}
	})

	t.Run("a keychain that cannot be read refuses the sign-in instead of overwriting the entry", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatal("the pane's sign-out was not answered the cloud's 500")
		}
		blip := &keychainBlip{fakeStore: store}
		blip.failGets.Store(1)
		fresh := newCloudTokenJar("conn-1", c.url, blip)
		err := fresh.prepareForSignIn(ctx, nil)
		if err == nil || !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("a sign-in over an unreadable keychain entry: %v, want the read's error", err)
		}
		if !strings.Contains(err.Error(), "sign-out") {
			t.Fatalf("the refusal does not say what is at stake: %v", err)
		}
		if store.m[key] != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the refused sign-in rewrote the entry to %q", store.m[key])
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the refused sign-in ended the session on the cloud")
		}
	})
}

// TestJar_AHeldSignOutIsWrittenAtTheFirstKeychainReadThatAnswers: a sign-out
// the keychain kept from being SENT is held in memory — the keyring that
// could not be read cannot be written either — and the operator is told that
// a restart before the keychain answers loses it, instead of the bare promise
// that it is retried. The first presentation whose read answers writes the
// hold, so a restart from then on does not adopt the session.
func TestJar_AHeldSignOutIsWrittenAtTheFirstKeychainReadThatAnswers(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"
	ctx := context.Background()
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)
	stored := jar.storedRefresh()

	blip := &keychainBlip{fakeStore: store}
	blip.failGets.Store(1)
	unread := newCloudTokenJar("conn-1", c.url, blip)
	err := unread.logout(ctx)
	if err == nil || !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("a sign-out whose token could not be read: %v, want the keychain's error", err)
	}
	if !unread.SignOutPending() {
		t.Fatal("a sign-out that could not be sent left no mark")
	}
	for _, want := range []string{"in this app's memory only", "a restart loses it", "closing the pane is refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}
	if store.m[key] != stored {
		t.Fatalf("a sign-out that was not sent rewrote the keychain entry to %q", store.m[key])
	}
	// A restart right here reads an entry that says nothing of the hold —
	// which is exactly what the error warns about.
	lost := newCloudTokenJar("conn-1", c.url, store)
	if _, herr := lost.hydrate(); herr != nil {
		t.Fatalf("hydrate: %v", herr)
	}
	if lost.SignOutPending() {
		t.Fatal("the entry carries the hold already: the error's warning is false")
	}

	// The keychain answers again and the loop presents the sign-out. The
	// cloud's revocation fails, so the sign-out is still held — in the entry
	// now.
	c.sessions.failNextRevocations(1)
	ageReadFailure(unread)
	if rerr := unread.retrySignOut(ctx); rerr == nil {
		t.Fatal("the presentation the cloud refused reported success")
	}
	if store.m[key] != encodeStoredSession(stored, true, nil) {
		t.Fatalf("the keychain entry is %q, want the token carrying the hold: a restart would adopt the session", store.m[key])
	}
	kept := newCloudTokenJar("conn-1", c.url, store)
	if _, herr := kept.hydrate(); herr != nil {
		t.Fatalf("hydrate: %v", herr)
	}
	if !kept.SignOutPending() {
		t.Fatal("a restart after the hold was written adopts the session the operator signed out of")
	}
	if !c.sessionLive(t, stored) {
		t.Fatal("the cloud ended the session it refused to revoke")
	}
}

// TestJar_AHeldSignOutIsPresentedPastTheUnpromptedReadBudget: the unprompted
// read budget bounds the OS unlock prompts a connection nobody is signing
// into charges. A sign-out the operator asked for is the opposite case: the
// app promised to present it until the cloud confirms it, and the keychain
// holds the only credential that can end that session. So the presentations
// keep reading the keychain on the back-off once the budget is spent — one
// read per back-off step, never a prompt per request.
func TestJar_AHeldSignOutIsPresentedPastTheUnpromptedReadBudget(t *testing.T) {
	ctx := context.Background()
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)
	stored := jar.storedRefresh()

	blip := &keychainBlip{fakeStore: store}
	blip.failGets.Store(int64(cloudKeychainUnpromptedReads) + 1)
	unread := newCloudTokenJar("conn-1", c.url, blip)
	// Spend the budget on renewals, as a connection open all day does.
	for i := 0; i < cloudKeychainUnpromptedReads; i++ {
		ageReadFailureBy(unread, cloudKeychainReadBackoffCap)
		if err := unread.refreshNow(ctx); !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("the refresh spending the budget: %v, want errCloudSessionUnread", err)
		}
	}
	if n := readFailureCount(unread); n < cloudKeychainUnpromptedReads {
		t.Fatalf("the budget is not spent: %d failed reads", n)
	}
	spent := blip.gets.Load()
	// A renewal still asks for nothing: that bound is untouched.
	ageReadFailureBy(unread, cloudKeychainReadBackoffCap)
	if err := unread.refreshNow(ctx); !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the refresh past the budget: %v, want errCloudSessionUnread", err)
	}
	if n := blip.gets.Load(); n != spent {
		t.Fatalf("a renewal past the budget read the keychain (%d, was %d): an unlock prompt nobody asked for", n, spent)
	}

	// The operator signs out: held, in memory only.
	if err := unread.logout(ctx); err == nil {
		t.Fatal("the sign-out whose token could not be read reported success")
	}
	if !unread.SignOutPending() {
		t.Fatal("the sign-out left no mark")
	}
	// Inside the back-off step, the presentation asks the keychain for
	// nothing: one read per step, not one per attempt.
	before := blip.gets.Load()
	if err := unread.retrySignOut(ctx); !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the presentation inside the back-off step: %v, want errCloudSessionUnread", err)
	}
	if n := blip.gets.Load(); n != before {
		t.Fatalf("the presentation inside the back-off step read the keychain %d time(s)", n-before)
	}
	// A step later it reads, the keychain answers, and the session ends.
	ageReadFailureBy(unread, cloudKeychainReadBackoffCap)
	if err := unread.retrySignOut(ctx); err != nil {
		t.Fatalf("the presentation once the keychain answers: %v", err)
	}
	if n := blip.gets.Load(); n != before+1 {
		t.Fatalf("the presentation read the keychain %d time(s), want one", n-before)
	}
	if c.sessionLive(t, stored) {
		t.Fatal("'the sign-out is presented once the keychain answers' is false: the keychain answered and the session is still live")
	}
	if v, ok := store.m[cloudRefreshKeyPrefix+"conn-1"]; ok {
		t.Fatalf("the confirmed sign-out kept the keychain entry %q", v)
	}
}
