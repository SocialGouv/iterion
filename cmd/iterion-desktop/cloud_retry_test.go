package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// TestJar_TheRetryOutlastsTheCloudsSlowestRefresh pins the presentation a
// refresh makes at once (cloudRefreshRetryTimeout) above the cloud's own
// bound on a refresh (auth.RefreshTimeout): a retry that gives up first can
// abandon a refresh that was about to succeed. The jar's HTTP client must
// not cut it first either.
func TestJar_TheRetryOutlastsTheCloudsSlowestRefresh(t *testing.T) {
	if cloudRefreshRetryTimeout <= auth.RefreshTimeout {
		t.Fatalf("the presentation a refresh makes at once waits %v, no longer than the cloud's bound on a successful refresh (%v)", cloudRefreshRetryTimeout, auth.RefreshTimeout)
	}
	hc := newCloudTokenJar("conn-1", "http://cloud.invalid", newFakeStore()).hc
	if hc.Timeout != 0 && hc.Timeout < cloudRefreshRetryTimeout {
		t.Fatalf("the jar's HTTP client gives up after %v, before the retry's %v", hc.Timeout, cloudRefreshRetryTimeout)
	}
}

// stalledFirstCheck is a session store whose first post-write check of a
// successor — the first presentation's, once it rotated the token and wrote
// the successor — returns only when releaseCheck is closed, and then fails
// as a store that dropped the connection does. When releaseRead is set, the
// second read of a token — the retry's first step — waits for it too.
// Nothing counts until armed.
type stalledFirstCheck struct {
	*auth.MemorySessionStore
	armed        atomic.Bool
	checks       atomic.Int64
	reads        atomic.Int64
	releaseCheck chan struct{}
	releaseRead  chan struct{}
}

func (s *stalledFirstCheck) SessionRevoked(ctx context.Context, sess auth.Session) (bool, error) {
	if s.armed.Load() && s.checks.Add(1) == 1 {
		<-s.releaseCheck
		return false, errors.New("auth: check session: connection reset by peer")
	}
	return s.MemorySessionStore.SessionRevoked(ctx, sess)
}

func (s *stalledFirstCheck) GetSessionByTokenHash(ctx context.Context, h string) (auth.Session, error) {
	if s.armed.Load() && s.reads.Add(1) == 2 && s.releaseRead != nil {
		<-s.releaseRead
	}
	return s.MemorySessionStore.GetSessionByTokenHash(ctx, h)
}

// TestJar_AnAbandonedPresentationAndItsRetryAtOnceKeepTheSession drives the
// desktop's own refresh shape on the real cloud: the first presentation is
// abandoned at cloudRefreshTimeout while the cloud is past the rotation and
// the successor's write, and the token is presented again at once. The
// abandoned presentation's failure lands either before the retry reads the
// token or after the retry was answered — no order is guaranteed — and
// neither may cost the session: the successor the retry receives refreshes
// once the abandoned presentation has failed, and the account's other device
// keeps its session.
func TestJar_AnAbandonedPresentationAndItsRetryAtOnceKeepTheSession(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the desktop's real refresh deadline (~10 s)")
	}
	for _, order := range []string{
		"the abandoned presentation fails before the retry reads",
		"the abandoned presentation fails after the retry was answered",
	} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			failsFirst := order == "the abandoned presentation fails before the retry reads"
			signer, err := auth.NewJWTSigner(randomJWTSecret(t), 15*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			sessions := &stalledFirstCheck{MemorySessionStore: auth.NewMemorySessionStore(), releaseCheck: make(chan struct{})}
			if failsFirst {
				sessions.releaseRead = make(chan struct{})
			}
			svc, err := auth.NewService(auth.Config{Store: identity.NewMemoryStore(), Sessions: sessions, Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			h := iserver.New(iserver.Config{AuthService: svc}, iterlog.New(iterlog.LevelError, nil)).Handler()
			// The presentations as the cloud sees them: the first one ends
			// when the desktop abandons it, the second when it is answered.
			var refreshes atomic.Int64
			firstAbandoned, firstDone, retryDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/auth/refresh" {
					switch refreshes.Add(1) {
					case 1:
						go func() { <-r.Context().Done(); close(firstAbandoned) }()
						defer close(firstDone)
					case 2:
						defer close(retryDone)
					}
				}
				h.ServeHTTP(w, r)
			}))
			defer srv.Close()
			// The stalled check is released whatever happens, before the
			// server is closed (which waits for the stalled request).
			var releaseOnce sync.Once
			releaseCheck := func() { releaseOnce.Do(func() { close(sessions.releaseCheck) }) }
			defer releaseCheck()
			refreshReturned := make(chan struct{})
			if failsFirst {
				go func() { <-firstAbandoned; releaseCheck() }()
				go func() { <-firstDone; close(sessions.releaseRead) }()
			} else {
				go func() {
					select {
					case <-retryDone:
					case <-refreshReturned:
					}
					releaseCheck()
				}()
			}

			ctx := context.Background()
			desk, err := svc.Register(ctx, "desk@example.org", "password-forever-9", "", "", "desktop", "ip")
			if err != nil {
				t.Fatal(err)
			}
			other, err := svc.Login(ctx, "desk@example.org", "password-forever-9", "browser", "ip2")
			if err != nil {
				t.Fatal(err)
			}
			jar := newCloudTokenJar("conn-1", srv.URL, newFakeStore())
			if err := jar.seed(cloudAuthResult{AccessToken: desk.AccessToken, AccessExpiry: desk.AccessExpires, RefreshToken: desk.RefreshToken}); err != nil {
				t.Fatal(err)
			}
			t0 := jar.storedRefresh()
			var presented atomic.Int64
			jar.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				presented.Add(1)
				return http.DefaultTransport.RoundTrip(req)
			})
			sessions.armed.Store(true)

			start := time.Now()
			rerr := jar.refreshNow(ctx)
			close(refreshReturned)
			t.Logf("%s: %v after %v, %d presentations", order, rerr, time.Since(start).Round(100*time.Millisecond), presented.Load())
			if n := presented.Load(); n != 2 {
				t.Fatalf("the refresh presented the token %d times, want 2: the first was not abandoned at the %v deadline", n, cloudRefreshTimeout)
			}
			if rerr != nil {
				t.Fatalf("the presentation made at once after the abandoned one: %v", rerr)
			}
			if got := jar.storedRefresh(); got == "" || got == t0 {
				t.Fatalf("the jar kept the retired token %q", got)
			}
			select {
			case <-firstDone:
			case <-time.After(10 * time.Second):
				t.Fatal("the abandoned presentation never finished on the cloud")
			}
			// The abandoned presentation has failed. The successor the retry
			// received is the desktop's session: its next refresh is served.
			if err := jar.refreshNow(ctx); err != nil {
				t.Fatalf("the desktop's next refresh, after the abandoned presentation failed: %v — the session the retry was handed is gone", err)
			}
			if _, err := svc.Refresh(ctx, other.RefreshToken, "browser", "ip2"); err != nil {
				t.Fatalf("the account's other device lost its session: %v", err)
			}
		})
	}
}
