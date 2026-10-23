package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestJar_LogoutEndsOnlyWhatTheCloudConfirmed drives the sign-out LogoutCloud
// runs (cloudTokenJar.logout), against the real cloud, for each way it can
// end. Only a sign-out the cloud answered 2xx clears the jar and the keychain
// entry; one whose token could not be read, or that the cloud did not confirm
// — its revocation failed (500 "identity unavailable"), or it never answered
// — keeps the refresh token, which is the only thing that can still end the
// session, returns the error, and leaves the session live for the retry that
// ends it. What it does NOT keep is the use of that session: the sign-out is
// marked pending, in the jar and in the keychain entry.
func TestJar_LogoutEndsOnlyWhatTheCloudConfirmed(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"
	ctx := context.Background()

	t.Run("the keychain cannot be read", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		blip := &keychainBlip{fakeStore: store}
		blip.failGets.Store(1)
		// Not the current connection: LogoutCloud's jar reads the keychain.
		lj := newCloudTokenJar("conn-1", c.url, blip)
		err := lj.logout(ctx)
		if !errors.Is(err, errKeychainLocked) {
			t.Fatalf("a sign-out whose token could not be read: %v, want the keychain's error", err)
		}
		if store.m[key] != stored || !c.sessionLive(t, stored) {
			t.Fatalf("a sign-out that read nothing deleted the keychain entry (%q) or ended the session (live %t)", store.m[key], c.sessionLive(t, stored))
		}
		if err := lj.logout(ctx); err != nil {
			t.Fatalf("the retry once the keychain answers: %v", err)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the retried sign-out left the session live on the cloud")
		}
		if _, ok := store.m[key]; ok {
			t.Fatal("the retried sign-out kept the keychain entry")
		}
	})

	t.Run("the cloud's revocation fails", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		err := jar.logout(ctx)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("a sign-out whose revocation failed: %v, want the cloud's 500", err)
		}
		if jar.storedRefresh() != stored || store.m[key] != encodeStoredSession(stored, true, nil) {
			t.Fatalf("a sign-out the cloud did not confirm dropped the token that can still end the session (jar %q, keychain %q)", jar.storedRefresh(), store.m[key])
		}
		if jar.AccessToken() != "" || jar.LoggedIn() || !jar.SignOutPending() {
			t.Fatalf("a sign-out the cloud did not confirm left the session in use (access %q, logged in %t, pending %t)", jar.AccessToken(), jar.LoggedIn(), jar.SignOutPending())
		}
		if rerr := jar.refreshNow(ctx); !errors.Is(rerr, errCloudSignOutPending) {
			t.Fatalf("a refresh after the unconfirmed sign-out: %v, want errCloudSignOutPending (it renewed the session the operator ended)", rerr)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the cloud ended the session it answered 500 for")
		}
		if err := jar.logout(ctx); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if c.sessionLive(t, stored) || jar.storedRefresh() != "" || jar.AccessToken() != "" {
			t.Fatal("the retried sign-out did not end the session and clear the jar")
		}
		if _, ok := store.m[key]; ok {
			t.Fatal("the retried sign-out kept the keychain entry")
		}
	})

	t.Run("the cloud does not answer", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		jar.baseURL = gone.URL
		if err := jar.logout(ctx); err == nil {
			t.Fatal("a sign-out the cloud never received reported success")
		}
		if jar.storedRefresh() != stored || store.m[key] != encodeStoredSession(stored, true, nil) || !c.sessionLive(t, stored) {
			t.Fatalf("a sign-out the cloud never received dropped the only copy of a live session (jar %q, keychain %q)", jar.storedRefresh(), store.m[key])
		}
		if jar.AccessToken() != "" || !jar.SignOutPending() {
			t.Fatalf("a sign-out the cloud never received left the session in use (access %q, pending %t)", jar.AccessToken(), jar.SignOutPending())
		}
	})
}

// paneLogout sends a pane's sign-out through the tunnel as the demux proxy
// forwards it: no cookie (stripped), the jar's access token as the Bearer.
// base is the transport under the tunnel.
func paneLogout(t *testing.T, jar *cloudTokenJar, base http.RoundTripper, url string) (int, string) {
	t.Helper()
	rt := &cloudRoundTripper{base: base, jar: jar, onExpired: func(string) {}}
	req, err := http.NewRequest(http.MethodPost, url+"/api/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok := jar.AccessToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestCloudRoundTripper_APaneSignOutEndsTheJarsSession drives the "Sign out"
// a cloud pane offers: its POST /api/auth/logout reaches the cloud through
// the tunnel, which strips the webview's cookies — the pane holds no refresh
// token. The tunnel presents the jar's, so the cloud revokes the session, and
// clears the jar once the cloud confirmed: the pane's next request is not
// signed back in by the jar. A sign-out the cloud did not confirm, or one the
// keychain kept from being sent, ends nothing locally either.
func TestCloudRoundTripper_APaneSignOutEndsTheJarsSession(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"

	t.Run("the cloud confirms", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		stored := jar.storedRefresh()
		code, body := paneLogout(t, jar, http.DefaultTransport, c.url)
		if code != http.StatusNoContent {
			t.Fatalf("the pane's sign-out was answered %d %s, want 204", code, body)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the pane signed out and the cloud still holds the session live: the tunnel presented no refresh token")
		}
		if jar.AccessToken() != "" || jar.storedRefresh() != "" {
			t.Fatal("the jar kept the session the pane signed out of")
		}
		if _, ok := store.m[key]; ok {
			t.Fatal("the keychain kept the session the pane signed out of")
		}
		if code, body, _ := paneGET(t, jar, c.url+"/api/auth/me"); code != http.StatusUnauthorized {
			t.Fatalf("the pane's next request after its sign-out was answered %d %s, want 401", code, body)
		}
		if !c.sessionLive(t, other.RefreshToken) {
			t.Fatal("the pane's sign-out ended the account's other device too")
		}
	})

	t.Run("the cloud's revocation fails", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		c.sessions.failNextRevocations(1)
		code, body := paneLogout(t, jar, http.DefaultTransport, c.url)
		if code != http.StatusInternalServerError {
			t.Fatalf("the pane's sign-out whose revocation failed was answered %d %s, want the cloud's 500", code, body)
		}
		if jar.storedRefresh() != stored || store.m[key] != encodeStoredSession(stored, true, nil) || !c.sessionLive(t, stored) {
			t.Fatalf("a sign-out the cloud did not confirm dropped the token that can still end the session (jar %q, keychain %q)", jar.storedRefresh(), store.m[key])
		}
		if jar.AccessToken() != "" || !jar.SignOutPending() {
			t.Fatalf("a sign-out the cloud did not confirm left the session in use (access %q, pending %t)", jar.AccessToken(), jar.SignOutPending())
		}
	})

	t.Run("the keychain cannot be read", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		blip := &keychainBlip{fakeStore: store}
		blip.failGets.Store(1)
		unread := newCloudTokenJar("conn-1", c.url, blip)
		var sent atomic.Int64
		counting := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sent.Add(1)
			return http.DefaultTransport.RoundTrip(req)
		})
		code, body := paneLogout(t, unread, counting, c.url)
		if code != http.StatusServiceUnavailable || body != signOutUnavailable {
			t.Fatalf("a pane's sign-out whose token could not be read was answered %d %s, want 503 %s", code, body, signOutUnavailable)
		}
		if n := sent.Load(); n != 0 {
			t.Fatalf("the tunnel sent %d sign-out(s) carrying no token: the cloud answers 204 and revokes nothing", n)
		}
		if store.m[key] != stored || !c.sessionLive(t, stored) {
			t.Fatal("a sign-out that was not sent dropped the session")
		}
	})
}

// TestJar_ARefreshAnsweredAfterASignOutDoesNotSignBackIn: the background
// loop's refresh is in flight when the pane signs out. The sign-out clears
// the jar, and the refresh's answer, arriving after it, is dropped:
// installed, it would hand the pane a working access token — and the
// keychain a successor — of the session the user just ended.
func TestJar_ARefreshAnsweredAfterASignOutDoesNotSignBackIn(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var arrivedOnce sync.Once
	var revoked atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrived) })
		<-release
		http.SetCookie(w, &http.Cookie{Name: cloudRefreshCookieName, Value: "refresh-1"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"u1"},"access_token":"access-1"}`))
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Iterion-Refresh") == "refresh-0" {
			revoked.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	store := newFakeStore()
	jar := newCloudTokenJar("conn-1", srv.URL, store)
	if err := jar.seed(cloudAuthResult{AccessToken: "access-0", RefreshToken: "refresh-0"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- jar.refreshNow(context.Background()) }()
	<-arrived
	if code, body := paneLogout(t, jar, http.DefaultTransport, srv.URL); code != http.StatusNoContent || revoked.Load() != 1 {
		t.Fatalf("the pane's sign-out was answered %d %s with %d revocation(s) of the jar's token, want 204 and 1", code, body, revoked.Load())
	}
	close(release)
	err := <-done
	if jar.AccessToken() != "" || jar.storedRefresh() != "" {
		t.Fatalf("the refresh answered after the sign-out (%v) signed the jar back in: access %q, refresh %q", err, jar.AccessToken(), jar.storedRefresh())
	}
	if v, ok := store.m[cloudRefreshKeyPrefix+"conn-1"]; ok {
		t.Fatalf("the refresh answered after the sign-out persisted %q to the keychain", v)
	}
}

// TestKeychain_GetSeparatesNothingStoredFromAFailedRead drives the real
// Keychain over go-keyring's own mock provider, which answers a missing
// secret with keyring.ErrNotFound as the OS providers do (Secret Service: a
// search that matched nothing; macOS: "could not be found"; Windows:
// ERROR_NOT_FOUND). Nothing stored is an empty answer — the jar of a
// connection that never signed in asks for a sign-in — and any other error
// is a failed read, which leaves the jar unread instead.
func TestKeychain_GetSeparatesNothingStoredFromAFailedRead(t *testing.T) {
	// Never the real OS keychain: the mock is installed before any call.
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	kc := NewKeychain()

	if v, err := kc.Get(cloudRefreshKeyPrefix + "conn-1"); v != "" || err != nil {
		t.Fatalf("Get of a key nothing is stored under = %q, %v; want \"\", nil", v, err)
	}
	empty := newCloudTokenJar("conn-1", "http://cloud.invalid", kc)
	if ok, err := empty.hydrate(); ok || err != nil {
		t.Fatalf("hydrate with nothing stored = %v, %v; want false, nil", ok, err)
	}
	if err := empty.refreshNow(context.Background()); !cloudRefreshRefused(err) {
		t.Fatalf("the refresh of a connection with nothing stored: %v, want a refusal (the shell asks for a sign-in)", err)
	}

	keyring.MockInitWithError(errKeychainLocked)
	if _, err := kc.Get(cloudRefreshKeyPrefix + "conn-1"); !errors.Is(err, errKeychainLocked) {
		t.Fatalf("Get over a locked keychain: %v, want its error", err)
	}
	locked := newCloudTokenJar("conn-1", "http://cloud.invalid", kc)
	if ok, err := locked.hydrate(); ok || !errors.Is(err, errKeychainLocked) {
		t.Fatalf("hydrate over a locked keychain = %v, %v; want false and its error", ok, err)
	}
	if err := locked.refreshNow(context.Background()); cloudRefreshRefused(err) || !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the refresh over a locked keychain: %v, want errCloudSessionUnread (not a refusal)", err)
	}
}
