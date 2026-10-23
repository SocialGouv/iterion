package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// countingTransport counts the requests to one path and forwards everything.
type countingTransport struct {
	base http.RoundTripper
	path string
	hits atomic.Int64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == t.path {
		t.hits.Add(1)
	}
	return t.base.RoundTrip(req)
}

// countJarRequests makes the jar's own calls (refresh, logout) go through a
// counter on path.
func countJarRequests(jar *cloudTokenJar, path string) *countingTransport {
	c := &countingTransport{base: http.DefaultTransport, path: path}
	jar.hc = &http.Client{Transport: c, Timeout: cloudHTTPTimeout}
	return c
}

// paneGETWithHeaders is paneGET with extra request headers (the pane's
// keychain-retry control sends one).
func paneGETWithHeaders(t *testing.T, jar *cloudTokenJar, url string, headers map[string]string) (int, string, []string) {
	t.Helper()
	return paneRequest(t, jar, http.MethodGet, url, headers)
}

// TestCloudRoundTripper_APaneSignOutTheCloudDidNotConfirmIsHeldAndRePresented
// drives the pane's "Sign out" against the real cloud whose revocation fails
// (500): the session is still live there, and the desktop holds the only
// token that can end it. So the desktop holds the SIGN-OUT — it presents the
// session no more and renews it no more, and the background loop presents the
// sign-out until the cloud confirms it. Without that, the pane's banner ("you
// may not be signed out yet") was the only trace: the loop kept rotating the
// session and the next pane load was served it.
func TestCloudRoundTripper_APaneSignOutTheCloudDidNotConfirmIsHeldAndRePresented(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"
	ctx := context.Background()

	t.Run("nothing presents or renews the session, and the pane is answered 401", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		stored := jar.storedRefresh()
		refreshes := countJarRequests(jar, "/api/auth/refresh")
		c.sessions.failNextRevocations(1)

		if code, body := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out whose revocation failed was answered %d %s, want the cloud's 500", code, body)
		}
		if !jar.SignOutPending() {
			t.Fatal("a sign-out the cloud did not confirm left no mark: the next pane load is served the session")
		}
		if jar.AccessToken() != "" || jar.LoggedIn() {
			t.Fatalf("the jar still presents the session (access %q, logged in %t)", jar.AccessToken(), jar.LoggedIn())
		}
		if store.m[key] != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the keychain entry is %q, want the token carrying the pending sign-out", store.m[key])
		}

		// The background loop's renewal: refused, and nothing presented.
		if err := jar.refreshNow(ctx); !errors.Is(err, errCloudSignOutPending) {
			t.Fatalf("the renewal of a session whose sign-out is pending: %v, want errCloudSignOutPending", err)
		}
		if n := refreshes.hits.Load(); n != 0 {
			t.Fatalf("the jar presented its refresh token %d time(s) after the sign-out: the session the operator ended is rotated and extended", n)
		}
		if jar.storedRefresh() != stored {
			t.Fatalf("the token that can still end the session changed to %q", jar.storedRefresh())
		}

		// A pane request reaches the cloud with no credential.
		code, body, expired := paneGET(t, jar, c.url+"/api/auth/me")
		if code != http.StatusUnauthorized {
			t.Fatalf("a pane request while the sign-out is pending was answered %d %s, want the cloud's 401", code, body)
		}
		if len(expired) != 1 || expired[0] != "conn-1" {
			t.Fatalf("the shell was asked for a new sign-in %v, want once for conn-1", expired)
		}
		if !c.sessionLive(t, other.RefreshToken) {
			t.Fatal("the pane's sign-out ended the account's other device too")
		}
	})

	t.Run("the loop presents it until the cloud confirms", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		// One counter for every sign-out presentation: the pane's, which goes
		// through the tunnel's transport, and the loop's, which the jar sends.
		logouts := countJarRequests(jar, "/api/auth/logout")
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, logouts, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}

		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		expired := make(chan string, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			cloudRefreshLoop(loopCtx, jar, func(id string) { expired <- id })
		}()
		select {
		case <-done:
		case id := <-expired:
			t.Fatalf("the loop asked the shell to sign %s in again: a sign-out the operator asked for is not an expiry", id)
		case <-time.After(20 * time.Second):
			t.Fatal("the loop never presented the pending sign-out again (it is waiting on its expiry timer)")
		}
		if n := logouts.hits.Load(); n < 2 {
			t.Fatalf("the sign-out reached the cloud %d time(s) in all, want the pane's failed one and at least one more from the loop", n)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the loop reported the sign-out confirmed while the cloud still holds the session live")
		}
		if jar.SignOutPending() || jar.storedRefresh() != "" {
			t.Fatalf("a confirmed sign-out left the mark (%t) or the token (%q)", jar.SignOutPending(), jar.storedRefresh())
		}
		if v, ok := store.m[key]; ok {
			t.Fatalf("a confirmed sign-out kept the keychain entry %q", v)
		}
		select {
		case id := <-expired:
			t.Fatalf("the loop asked the shell to sign %s in again after a confirmed sign-out", id)
		default:
		}
	})

	t.Run("a restart does not adopt the session either", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}

		// The app restarts: a fresh jar over the same keychain (openCloudConn).
		restarted := newCloudTokenJar("conn-1", c.url, store)
		hydrated, err := restarted.hydrate()
		if !hydrated || err != nil {
			t.Fatalf("hydrate after the restart = %v, %v; want the stored token", hydrated, err)
		}
		if !restarted.SignOutPending() {
			t.Fatal("the restart read the token without its pending sign-out: the pane is served the session the operator ended")
		}
		if restarted.storedRefresh() != stored {
			t.Fatalf("the restart holds %q, want the token that can still end the session", restarted.storedRefresh())
		}
		if err := restarted.refreshNow(ctx); !errors.Is(err, errCloudSignOutPending) {
			t.Fatalf("the restarted connection's renewal: %v, want errCloudSignOutPending", err)
		}
		if code, body, _ := paneGET(t, restarted, c.url+"/api/auth/me"); code != http.StatusUnauthorized {
			t.Fatalf("a pane request after the restart was answered %d %s, want the cloud's 401", code, body)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the restart ended the session on the cloud without presenting the sign-out")
		}
	})

	t.Run("a new sign-in presents the sign-out, then clears the mark", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
		}
		// LoginCloud's seed, on the jar the shell re-authenticated.
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatalf("the new sign-in: %v", err)
		}
		if err := jar.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// The mark goes because the sign-out was PRESENTED, not because a
		// sign-in overrides it: the entry a sign-in replaces holds the only
		// token that can end the session the operator signed out of.
		if c.lineageLive(t, stored) {
			t.Fatal("the new sign-in cleared the mark of a session still live on the cloud: its sign-out was abandoned")
		}
		if jar.SignOutPending() || jar.AccessToken() == "" || !jar.LoggedIn() {
			t.Fatalf("the new session is held back by the old mark (pending %t, access %q)", jar.SignOutPending(), jar.AccessToken())
		}
		if store.m[key] != res.RefreshToken {
			t.Fatalf("the keychain entry is %q, want the new session's token unmarked", store.m[key])
		}
		if code, body, _ := paneGET(t, jar, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("a pane request after the new sign-in was answered %d %s, want 200", code, body)
		}
	})

	t.Run("a sign-out the keychain kept from being sent is held too", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		blip := &keychainBlip{fakeStore: store}
		blip.failGets.Store(1)
		unread := newCloudTokenJar("conn-1", c.url, blip)

		code, body := paneLogout(t, unread, http.DefaultTransport, c.url)
		if code != http.StatusServiceUnavailable || body != signOutUnavailable {
			t.Fatalf("a pane's sign-out whose token could not be read was answered %d %s, want 503 %s", code, body, signOutUnavailable)
		}
		if !unread.SignOutPending() {
			t.Fatal("a sign-out that could not be sent left no mark: the keychain answers again and the pane is served the session")
		}
		if store.m[key] != stored {
			t.Fatalf("a sign-out that was not sent rewrote the keychain entry to %q", store.m[key])
		}
		// The keychain answers again: the loop presents the sign-out.
		ageReadFailure(unread)
		if err := unread.retrySignOut(ctx); err != nil {
			t.Fatalf("the sign-out presented once the keychain answered: %v", err)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the presented sign-out left the session live on the cloud")
		}
		if v, ok := store.m[key]; ok {
			t.Fatalf("the confirmed sign-out kept the keychain entry %q", v)
		}
	})
}
