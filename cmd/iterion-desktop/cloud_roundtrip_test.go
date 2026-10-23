package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// expiredAccessCloud answers every API request 401 (the access token
// expired) and the refresh with refreshStatus — or, for 200, a rotated
// session whose access token the replay is accepted with.
func expiredAccessCloud(t *testing.T, refreshStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		if refreshStatus != http.StatusOK {
			w.WriteHeader(refreshStatus)
			_, _ = w.Write([]byte(`{"error":"identity unavailable"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cloudRefreshCookieName, Value: "rotated-1"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"u1"},"access_token":"renewed-access"}`))
	})
	mux.HandleFunc("GET /api/probe", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer renewed-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestCloudRoundTripper_OnlyARefusedRenewalEndsTheSession drives a pane's
// request whose access token expired, through the tunnel, for each way the
// renewal can end. A renewal the cloud could not serve (5xx, rate limit)
// answers 503 — the pane keeps its session, the jar its token, and no
// re-login is asked for; only a renewal the cloud refused (401/403) passes
// the 401 through and asks the shell for a re-login; a renewal that
// succeeds replays the request.
func TestCloudRoundTripper_OnlyARefusedRenewalEndsTheSession(t *testing.T) {
	for _, tc := range []struct {
		refresh    int
		wantStatus int
		wantExpiry bool
	}{
		{http.StatusInternalServerError, http.StatusServiceUnavailable, false},
		{http.StatusTooManyRequests, http.StatusServiceUnavailable, false},
		{http.StatusUnauthorized, http.StatusUnauthorized, true},
		{http.StatusForbidden, http.StatusUnauthorized, true},
		{http.StatusOK, http.StatusOK, false},
	} {
		t.Run(http.StatusText(tc.refresh), func(t *testing.T) {
			srv := expiredAccessCloud(t, tc.refresh)
			store := newFakeStore()
			jar := newCloudTokenJar("conn-1", srv.URL, store)
			if err := jar.seed(cloudAuthResult{AccessToken: "expired-access", RefreshToken: "held-refresh"}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			var expired []string
			rt := &cloudRoundTripper{base: http.DefaultTransport, jar: jar, onExpired: func(id string) { expired = append(expired, id) }}
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/probe", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer expired-access")
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("a request whose renewal answered %d was answered %d %s, want %d", tc.refresh, resp.StatusCode, body, tc.wantStatus)
			}
			if got := len(expired) > 0; got != tc.wantExpiry {
				t.Fatalf("a renewal answered %d asked for a re-login: %t, want %t", tc.refresh, got, tc.wantExpiry)
			}
			if tc.wantStatus == http.StatusServiceUnavailable && (string(body) != sessionRenewalUnavailable || jar.storedRefresh() != "held-refresh") {
				t.Fatalf("an unserved renewal answered %q and left the jar %q, want %q and the held token", body, jar.storedRefresh(), sessionRenewalUnavailable)
			}
		})
	}
}

// paneGET sends a pane's request through the tunnel and returns its status,
// its body, and the connection ids the tunnel asked a re-login for.
func paneGET(t *testing.T, jar *cloudTokenJar, url string) (int, string, []string) {
	t.Helper()
	return paneRequest(t, jar, http.MethodGet, url, nil)
}

// paneRequest is paneGET for any method and extra headers. The Bearer is
// injected exactly as the demux proxy's Rewrite does — only when the jar
// answers one — so a jar that withholds its token reaches the cloud
// unauthenticated here too.
func paneRequest(t *testing.T, jar *cloudTokenJar, method, url string, headers map[string]string) (int, string, []string) {
	t.Helper()
	var expired []string
	rt := &cloudRoundTripper{base: http.DefaultTransport, jar: jar, onExpired: func(id string) { expired = append(expired, id) }}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok := jar.AccessToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body), expired
}

// TestCloudRoundTripper_ADeadSessionReachesThePaneAsA401 drives the two ways
// a desktop ends up with nothing to present — a stored session that died
// while the app was closed (the 30-day refresh TTL, a password change, "sign
// out everywhere", an admin disabling the account: the refused rehydrate
// clears the jar) and a connection that never held a token. Both are the
// same dead end, and the pane must be answered the cloud's own 401 with a
// re-login asked of the shell, not a 503 "the server could not load your
// account" the SPA retries forever.
func TestCloudRoundTripper_ADeadSessionReachesThePaneAsA401(t *testing.T) {
	t.Run("a stored session revoked while the desktop was closed", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		ctx := context.Background()
		sess, err := c.sessions.GetSessionByTokenHash(ctx, auth.HashRefreshToken(jar.storedRefresh()))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.svc.RevokeUserSessions(ctx, sess.UserID); err != nil {
			t.Fatal(err)
		}
		// The desktop restarts: a fresh jar hydrated from the keychain, as
		// openCloudConn and activateCloudConnection do.
		jar2 := newCloudTokenJar("conn-1", c.url, store)
		if ok, _ := jar2.hydrate(); !ok {
			t.Fatal("hydrate found nothing")
		}
		rerr := jar2.refreshNow(ctx)
		if !cloudRefreshRefused(rerr) {
			t.Fatalf("the rehydrate refresh of a revoked session: %v, want a refusal", rerr)
		}
		if jar2.storedRefresh() != "" {
			t.Fatalf("the refused rehydrate kept a token %q", jar2.storedRefresh())
		}
		assertPaneSignsIn(t, c, jar2)
	})
	t.Run("a connection that never held a token", func(t *testing.T) {
		c := newRealCloud(t)
		jar := newCloudTokenJar("conn-1", c.url, newFakeStore())
		if ok, _ := jar.hydrate(); ok {
			t.Fatal("hydrate found a token")
		}
		assertPaneSignsIn(t, c, jar)
	})
}

// assertPaneSignsIn checks that a pane served by an empty jar is answered
// the cloud's 401 and that the shell is asked for a new sign-in, and that
// the refresh the loop would make classifies the same way (so it emits
// cloud:auth-expired and stops instead of retrying every floor forever).
func assertPaneSignsIn(t *testing.T, c *realCloud, jar *cloudTokenJar) {
	t.Helper()
	code, body, expired := paneGET(t, jar, c.url+"/api/auth/me")
	if code != http.StatusUnauthorized {
		t.Errorf("the pane of a desktop holding no session was answered %d %s, want the cloud's 401", code, body)
	}
	if len(expired) == 0 {
		t.Errorf("the tunnel asked for no re-login (%d %s): the shell never opens its sign-in prompt", code, body)
	}
	if err := jar.refreshNow(context.Background()); !cloudRefreshRefused(err) {
		t.Errorf("the background loop's refresh on a jar with nothing to present: %v, want a refusal (it retries for ever otherwise)", err)
	}
}

// TestCloudRoundTripper_AnUnservedRenewalWakesTheRefreshLoop: the tunnel
// schedules nothing of its own, so a renewal it could not get served has to
// hand the retry to the background loop — whose timer, set from the access
// expiry it saw last, is minutes away when the token lapsed early.
func TestCloudRoundTripper_AnUnservedRenewalWakesTheRefreshLoop(t *testing.T) {
	for _, tc := range []struct {
		refresh  int
		wantWake bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
		{http.StatusUnauthorized, false},
		{http.StatusOK, false},
	} {
		t.Run(http.StatusText(tc.refresh), func(t *testing.T) {
			srv := expiredAccessCloud(t, tc.refresh)
			jar := newCloudTokenJar("conn-1", srv.URL, newFakeStore())
			if err := jar.seed(cloudAuthResult{AccessToken: "expired-access", RefreshToken: "held-refresh"}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			paneGET(t, jar, srv.URL+"/api/probe")
			woken := false
			select {
			case <-jar.woken():
				woken = true
			default:
			}
			if woken != tc.wantWake {
				t.Fatalf("a renewal answered %d woke the refresh loop: %t, want %t", tc.refresh, woken, tc.wantWake)
			}
		})
	}
}

// deadlineRecorder answers every request 500 after recording its context's
// deadline.
type deadlineRecorder struct {
	mu        sync.Mutex
	deadlines []time.Time
	undated   int
}

func (d *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	d.mu.Lock()
	if ok {
		d.deadlines = append(d.deadlines, deadline)
	} else {
		d.undated++
	}
	d.mu.Unlock()
	return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(http.NoBody), Header: http.Header{}, Request: req}, nil
}

// TestJar_ARefreshIsBoundedByTheRefreshTimeout: a refresh carries its own
// deadline, cloudRefreshTimeout from its start, whatever the caller's — a
// lost answer is known lost in time to be presented again inside the
// server's window — and the one presentation it then makes at once carries
// cloudRefreshRetryTimeout, long enough for the cloud's worst success path.
func TestJar_ARefreshIsBoundedByTheRefreshTimeout(t *testing.T) {
	rec := &deadlineRecorder{}
	jar := newCloudTokenJar("conn-1", "http://cloud.invalid", newFakeStore())
	jar.hc = &http.Client{Transport: rec}
	if err := jar.seed(cloudAuthResult{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	start := time.Now()
	_ = jar.refreshNow(context.Background())
	end := time.Now()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.undated > 0 {
		t.Fatalf("%d of the refresh's presentations carried no deadline: a lost answer is waited for until the HTTP client's own timeout", rec.undated)
	}
	want := []time.Duration{cloudRefreshTimeout, cloudRefreshRetryTimeout}
	if len(rec.deadlines) != len(want) {
		t.Fatalf("a refresh answered 500 presented the token %d times, want %d (the first, then one at once)", len(rec.deadlines), len(want))
	}
	for i, w := range want {
		if rec.deadlines[i].Before(start.Add(w)) || rec.deadlines[i].After(end.Add(w)) {
			t.Fatalf("presentation %d's deadline is %v after the refresh started, want %v", i+1, rec.deadlines[i].Sub(start), w)
		}
	}
}

// TestJar_ALostAnswerIsPresentedAgainAtOnce drives a refresh whose answer is
// lost after the server rotated the token and wrote its successor (a proxy
// dropping the response), on the real server, with the session store's clock
// — the window's — untouched: the presentation that recovers it is the one
// the refresh makes AT ONCE, not one a timer brings minutes later. It
// receives the rotation's successor and nothing is revoked.
func TestJar_ALostAnswerIsPresentedAgainAtOnce(t *testing.T) {
	if cloudRefreshTimeout >= auth.DefaultRefreshReuseGrace {
		t.Fatalf("the desktop abandons a presentation %v after it starts, at or past the server's default %v window: the presentation it then makes at once is judged outside the window",
			cloudRefreshTimeout, auth.DefaultRefreshReuseGrace)
	}
	c := newRealCloud(t)
	clock := &storeClock{now: time.Now().UTC()}
	c.sessions.SetClock(clock.Now)
	jar, store, other := c.seedJar(t)
	before := jar.storedRefresh()
	var presented atomic.Int64
	var dropped atomic.Bool
	jar.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		presented.Add(1)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && dropped.CompareAndSwap(false, true) {
			resp.Body.Close()
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(http.NoBody), Header: http.Header{}, Request: req}, nil
		}
		return resp, err
	})}
	if err := jar.refreshNow(context.Background()); err != nil {
		t.Fatalf("the refresh whose first answer was dropped: %v — nothing re-presented the rotated token", err)
	}
	if n := presented.Load(); n != 2 {
		t.Fatalf("the refresh presented the token %d times, want 2 (the lost one, then one at once)", n)
	}
	if got := jar.storedRefresh(); got == "" || got == before || store.m["cloud_refresh:conn-1"] != got {
		t.Fatalf("the retry did not seed the successor (jar %q, store %q)", got, store.m["cloud_refresh:conn-1"])
	}
	// The desktop's own session is live (its successor refreshes again), and
	// so is the account's other device: nothing read the re-presentation as
	// theft.
	if _, err := c.svc.Refresh(context.Background(), jar.storedRefresh(), "desktop", "ip"); err != nil {
		t.Fatalf("the successor the retry received is not a live session: %v", err)
	}
	if _, err := c.svc.Refresh(context.Background(), other.RefreshToken, "browser", "ip2"); err != nil {
		t.Fatalf("the account's other device lost its session: %v", err)
	}
}

// storeClock is the session store's clock, moved by hand.
type storeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *storeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *storeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
