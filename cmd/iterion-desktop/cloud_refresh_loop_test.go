package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// refreshCountingCloud answers POST /api/auth/refresh with status, counting
// every presentation and closing arrived on the first.
type refreshCountingCloud struct {
	presented atomic.Int64
	arrived   chan struct{}
	status    int
}

func newRefreshCountingCloud(t *testing.T, status int) (*refreshCountingCloud, string) {
	t.Helper()
	c := &refreshCountingCloud{arrived: make(chan struct{}, 64), status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		c.presented.Add(1)
		select {
		case c.arrived <- struct{}{}:
		default:
		}
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(`{"error":"identity unavailable"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return c, srv.URL
}

// liveJar is a jar whose access token expires far enough out that the loop's
// own timer is minutes away: only a wake can make it refresh sooner.
func liveJar(t *testing.T, baseURL string) *cloudTokenJar {
	t.Helper()
	jar := newCloudTokenJar("conn-1", baseURL, newFakeStore())
	if err := jar.seed(cloudAuthResult{AccessToken: "access", AccessExpiry: time.Now().Add(15 * time.Minute), RefreshToken: "held-refresh"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return jar
}

// TestCloudRefreshLoop_ARefusalEndsItWithAuthExpired: a jar with nothing
// left to present is a dead session, not a transient failure — the loop asks
// the shell for a new sign-in and stops, instead of presenting nothing every
// floor for as long as the app runs.
func TestCloudRefreshLoop_ARefusalEndsItWithAuthExpired(t *testing.T) {
	_, url := newRefreshCountingCloud(t, http.StatusInternalServerError)
	jar := newCloudTokenJar("conn-1", url, newFakeStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expired := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cloudRefreshLoop(ctx, jar, func(id string) { expired <- id })
	}()
	jar.kickRefresh()
	select {
	case id := <-expired:
		if id != "conn-1" {
			t.Fatalf("cloud:auth-expired carried %q, want conn-1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop asked for no re-login on a jar with nothing to present: it retries for ever and the shell never prompts")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept running after the session ended")
	}
}

// TestCloudRefreshLoop_AnUnservedRenewalWakesItAtOnce: the tunnel's refresh
// failed, so the token the jar holds may have been rotated. The loop's own
// timer is set from the access expiry — minutes away when a token lapsed
// early — so the wake is what puts the next presentation inside the server's
// reuse window.
func TestCloudRefreshLoop_AnUnservedRenewalWakesItAtOnce(t *testing.T) {
	cloud, url := newRefreshCountingCloud(t, http.StatusInternalServerError)
	jar := liveJar(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cloudRefreshLoop(ctx, jar, func(string) {})
	// Nothing happens on its own: the loop is waiting on its timer.
	time.Sleep(200 * time.Millisecond)
	if n := cloud.presented.Load(); n != 0 {
		t.Fatalf("the loop presented the token %d times before any wake, with %v left on the access token", n, 15*time.Minute)
	}
	jar.kickRefresh()
	select {
	case <-cloud.arrived:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop did not present the token after an unserved renewal: the retry waits for its expiry-based timer, %v away", time.Until(jar.Expiry().Add(-cloudRefreshSkew)).Round(time.Second))
	}
}

// TestCloudRefreshLoop_WakesNeverPresentFasterThanTheFloor: a pane that
// keeps retrying through an outage fails the tunnel's refresh again and
// again. Every failure wakes the loop, and the loop still presents the token
// at most once per cloudRefreshRetryFloor — the wake moves a retry earlier,
// it never turns the loop into a hammer.
func TestCloudRefreshLoop_WakesNeverPresentFasterThanTheFloor(t *testing.T) {
	// 429, so each attempt is one presentation: the cloud answered, and a
	// refusal to serve is not a lost answer to present again at once.
	cloud, url := newRefreshCountingCloud(t, http.StatusTooManyRequests)
	jar := liveJar(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cloudRefreshLoop(ctx, jar, func(string) {})
	const wakes = 50
	for i := 0; i < wakes; i++ {
		jar.kickRefresh()
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-cloud.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never presented the token")
	}
	time.Sleep(time.Second)
	if n := cloud.presented.Load(); n != 1 {
		t.Fatalf("%d wakes over %v made the loop present the token %d times, want 1 (the floor is %v)", wakes, wakes*10*time.Millisecond, n, cloudRefreshRetryFloor)
	}
}

// TestCloudRefreshLoop_StopsWithItsContext: closing a pane cancels the
// loop's context, and a pending wake does not keep it alive.
func TestCloudRefreshLoop_StopsWithItsContext(t *testing.T) {
	_, url := newRefreshCountingCloud(t, http.StatusInternalServerError)
	jar := liveJar(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cloudRefreshLoop(ctx, jar, func(string) {})
	}()
	jar.kickRefresh()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop outlived its context")
	}
}
