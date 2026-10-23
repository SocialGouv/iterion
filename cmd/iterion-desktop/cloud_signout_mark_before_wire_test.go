package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// TestAPaneSignOutWritesItsMarkBeforeTheWire (sign-out requested, invariant
// I5): the pane's sign-out writes the pending-sign-out mark to the keychain
// entry BEFORE the POST leaves, not when the answer marks. Between the send
// and the answer the ask is nowhere but on the entry: a crash in that window
// reads an entry that says the sign-out is owed, and a restart presents it
// instead of adopting the session as signed-in.
func TestAPaneSignOutWritesItsMarkBeforeTheWire(t *testing.T) {
	ctx := context.Background()
	key := cloudRefreshKeyPrefix + "conn-1"

	t.Run("the entry carries the mark while the ask is on the wire", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		arrived, release := make(chan struct{}), make(chan struct{})
		var dropRelease sync.Once
		defer dropRelease.Do(func() { close(release) })
		base := &heldTransport{base: http.DefaultTransport, path: "/api/auth/logout", arrived: arrived, release: release}
		done := make(chan error, 1)
		codeCh := make(chan int, 1)
		go func() {
			rt := &cloudRoundTripper{base: base, jar: jar, onExpired: func(string) {}}
			req, err := http.NewRequest(http.MethodPost, c.url+"/api/auth/logout", nil)
			if err != nil {
				done <- err
				return
			}
			if tok := jar.AccessToken(); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				done <- err
				return
			}
			resp.Body.Close()
			codeCh <- resp.StatusCode
		}()
		<-arrived
		// Between the send and the answer: the entry already carries the mark.
		if got := store.m[key]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the sign-out is on the wire and the answer is not back yet and the entry is %q, want it carrying the mark: a crash now would adopt the session the ask is ending", got)
		}
		dropRelease.Do(func() { close(release) })
		select {
		case err := <-done:
			t.Fatalf("the pane's sign-out through the tunnel failed: %v", err)
		case code := <-codeCh:
			if code != http.StatusNoContent {
				t.Fatalf("the pane's sign-out was answered %d, want 204 — the answer must reach the pane with the mark already written", code)
			}
		}
		// A 2xx clears the jar, and the mark goes with it.
		if jar.SignOutPending() || jar.storedRefresh() != "" || jar.AccessToken() != "" {
			t.Fatalf("the confirmed sign-out kept the session (pending %t, refresh %q, access %q)", jar.SignOutPending(), jar.storedRefresh(), jar.AccessToken())
		}
		if _, ok := store.m[key]; ok {
			t.Fatalf("the confirmed sign-out kept the keychain entry %q", store.m[key])
		}
	})

	t.Run("an answer lost after the send leaves the ask on the entry", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		lossy := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(req.URL.Path, "/api/auth/logout") {
				if resp.Body != nil {
					resp.Body.Close()
				}
				return nil, errors.New("the answer was lost")
			}
			return resp, nil
		})
		rt := &cloudRoundTripper{base: lossy, jar: jar, onExpired: func(string) {}}
		req, err := http.NewRequest(http.MethodPost, c.url+"/api/auth/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RoundTrip(req); err == nil {
			t.Fatal("a sign-out whose answer was lost reported success")
		}
		if got := store.m[key]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the entry is %q, want it carrying the mark: the ask was sent and its answer never came", got)
		}
		if !jar.SignOutPending() {
			t.Fatal("the jar stopped presenting the session whose sign-out was not confirmed")
		}
		// The restart-shaped reopen reads a marked entry and presents it; the
		// session the ask is ending is never renewed.
		reopened := newCloudTokenJar("conn-1", c.url, store)
		if _, err := reopened.hydrate(); err != nil {
			t.Fatal(err)
		}
		if !reopened.SignOutPending() {
			t.Fatal("the restart adopted the session whose sign-out the entry carries")
		}
		if err := reopened.refreshNow(ctx); !errors.Is(err, errCloudSignOutPending) {
			t.Fatalf("the restart renewed the session the ask is ending: %v, want errCloudSignOutPending", err)
		}
		if err := reopened.retrySignOut(ctx); err != nil {
			t.Fatalf("the restart's presentation of the mark: %v", err)
		}
		if _, ok := store.m[key]; ok {
			t.Fatal("the restart's presentation kept the keychain entry")
		}
		if c.lineageLive(t, stored) {
			t.Fatal("the session the ask named stayed live past the restart's presentation")
		}
	})

	t.Run("an other-session answer lifts the mark and keeps the session", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		code, body, header := paneLogoutOf(t, jar, c.url, "lineage="+url.QueryEscape("a-lineage-this-connection-never-held"))
		if code != http.StatusNoContent {
			t.Fatalf("the pane's sign-out naming another session was answered %d %s, want 204", code, body)
		}
		if got := header.Get(iserver.LogoutOutcomeHeader); got != iserver.LogoutOtherSession {
			t.Fatalf("the answer carries %s=%q, want %q", iserver.LogoutOutcomeHeader, got, iserver.LogoutOtherSession)
		}
		// The ask was not this jar's session: the mark the wire was preceded
		// by is lifted, in memory and on the entry, and the session is kept.
		if got := store.m[key]; got != stored {
			t.Fatalf("the keychain entry is %q, want it rewritten without the mark: a kept mark would end the KEPT session at the next presentation", got)
		}
		if jar.SignOutPending() {
			t.Fatal("the jar still withholds a session the answer says was never asked to end")
		}
		if jar.AccessToken() == "" || !jar.LoggedIn() {
			t.Fatalf("the jar stopped serving a session nothing signed out (access %q, logged in %t)", jar.AccessToken(), jar.LoggedIn())
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the cloud ended the session it answered other-session for")
		}
		if code, body, expired := paneGET(t, jar, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("the kept session no longer serves the pane: %d %s (renewal: %v)", code, body, expired)
		}
	})

	t.Run("an other-session answer does not lift a mark that predates the ask", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		// The jar already owes its own sign-out: a first ask the cloud did
		// not confirm.
		c.sessions.failNextRevocations(1)
		if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
			t.Fatalf("the pane's first sign-out was answered %d, want the cloud's 500", code)
		}
		if !jar.SignOutPending() {
			t.Fatal("the jar did not hold its own sign-out")
		}
		// A second ask names a lineage this connection never held: the cloud
		// signs nothing out. That answer says nothing about the jar's own
		// ask, whose mark stands — the loop keeps presenting it.
		code, body, header := paneLogoutOf(t, jar, c.url, "lineage="+url.QueryEscape("a-lineage-this-connection-never-held"))
		if code != http.StatusNoContent {
			t.Fatalf("the pane's sign-out naming another session was answered %d %s, want 204", code, body)
		}
		if got := header.Get(iserver.LogoutOutcomeHeader); got != iserver.LogoutOtherSession {
			t.Fatalf("the answer carries %s=%q, want %q", iserver.LogoutOutcomeHeader, got, iserver.LogoutOtherSession)
		}
		if got := store.m[key]; got != encodeStoredSession(stored, true, nil) {
			t.Fatalf("the keychain entry is %q, want it still carrying the jar's own mark: the other-session answer does not confirm it", got)
		}
		if !jar.SignOutPending() {
			t.Fatal("the jar resumed a session whose sign-out still stands")
		}
		if err := jar.refreshNow(ctx); !errors.Is(err, errCloudSignOutPending) {
			t.Fatalf("the jar renewed the session whose sign-out the cloud has not confirmed: %v, want errCloudSignOutPending", err)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the cloud ended the session whose sign-out still stands")
		}
	})
}
