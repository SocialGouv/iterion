package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
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

// heldRefreshServer answers POST /api/auth/refresh with a rotated token, but
// only once release is closed: the refreshes in flight before then overlap.
// presented counts every presentation.
type heldRefreshServer struct {
	presented atomic.Int64
	arrived   chan struct{}
	once      sync.Once
	release   chan struct{}
}

func newHeldRefreshServer(t *testing.T) (*heldRefreshServer, *httptest.Server) {
	t.Helper()
	h := &heldRefreshServer{arrived: make(chan struct{}), release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		n := h.presented.Add(1)
		h.once.Do(func() { close(h.arrived) })
		<-h.release
		http.SetCookie(w, &http.Cookie{Name: cloudRefreshCookieName, Value: fmt.Sprintf("rotated-%d", n)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"user":{"id":"u1"},"access_token":"access-%d","expires_at":%q}`, n, time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

// TestJar_OverlappingRefreshesPresentTheTokenOnce: the background loop and
// the proxy's 401 retries call refreshNow concurrently. Overlapping callers
// share one refresh, so the stored token is presented once — two
// presentations of one refresh token is what the server reads as reuse
// once its grace window has passed.
func TestJar_OverlappingRefreshesPresentTheTokenOnce(t *testing.T) {
	h, srv := newHeldRefreshServer(t)
	store := newFakeStore()
	store.m["cloud_refresh:conn-1"] = "stored-token"
	jar := newCloudTokenJar("conn-1", srv.URL, store)
	if _, err := jar.hydrate(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = jar.refreshNow(context.Background())
		}(i)
	}
	<-h.arrived
	// Every caller is now either sharing the held refresh or about to:
	// give the stragglers time to join before the refresh completes.
	time.Sleep(50 * time.Millisecond)
	close(h.release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := h.presented.Load(); n != 1 {
		t.Fatalf("%d overlapping refreshes presented the stored token %d times, want once", callers, n)
	}
	if got := store.m["cloud_refresh:conn-1"]; got != "rotated-1" {
		t.Fatalf("persisted refresh token = %q, want the rotated one", got)
	}
}

// TestJar_ACallerGivingUpDoesNotAbandonTheRefresh: a caller whose context is
// cancelled mid-refresh stops waiting, but the shared refresh completes and
// the jar keeps its rotated token. Abandoned, the refresh's rotation would
// be lost and the jar left holding the retired token.
func TestJar_ACallerGivingUpDoesNotAbandonTheRefresh(t *testing.T) {
	h, srv := newHeldRefreshServer(t)
	store := newFakeStore()
	store.m["cloud_refresh:conn-1"] = "stored-token"
	jar := newCloudTokenJar("conn-1", srv.URL, store)
	if _, err := jar.hydrate(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- jar.refreshNow(ctx) }()
	<-h.arrived
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v, want context.Canceled", err)
	}
	close(h.release)
	deadline := time.Now().Add(5 * time.Second)
	for jar.storedRefresh() != "rotated-1" {
		if time.Now().After(deadline) {
			t.Fatalf("the refresh its caller gave up on never completed: the jar holds %q", jar.storedRefresh())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// realCloud is the real pkg/server HTTP surface on an auth.Service over
// in-memory stores, whose session store can fail its next write.
type realCloud struct {
	url      string
	svc      *auth.Service
	sessions *failingSessions
}

type failingSessions struct {
	*auth.MemorySessionStore
	failCreates atomic.Int64
	failRevokes atomic.Int64
}

// failNextCreates makes the next n successor writes fail, as a session store
// that drops the connection does.
func (s *failingSessions) failNextCreates(n int64) { s.failCreates.Store(n) }

// failNextRevocations makes the next n lineage revocations — a sign-out's —
// fail, as a session store that drops the connection does. It overrides the
// RECORD, which is the commit point a sign-out waits for and answers from;
// the detached pass (RevokeLineageSessions) runs after the answer and cannot
// make the sign-out fail.
func (s *failingSessions) failNextRevocations(n int64) { s.failRevokes.Store(n) }

func (s *failingSessions) RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error {
	for {
		left := s.failRevokes.Load()
		if left <= 0 {
			return s.MemorySessionStore.RecordLineageRevocation(ctx, userID, lineageID, at)
		}
		if s.failRevokes.CompareAndSwap(left, left-1) {
			return errors.New("auth: revoke lineage: connection reset by peer")
		}
	}
}

// sessionLive reports whether the cloud holds tok's session live — not
// revoked, and covered by no revocation — without presenting it.
func (c *realCloud) sessionLive(t *testing.T, tok string) bool {
	t.Helper()
	ctx := context.Background()
	sess, err := c.sessions.GetSessionByTokenHash(ctx, auth.HashRefreshToken(tok))
	if errors.Is(err, auth.ErrSessionNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read the session of a token: %v", err)
	}
	if sess.RevokedAt != nil {
		return false
	}
	covered, err := c.sessions.SessionRevoked(ctx, sess)
	if err != nil {
		t.Fatalf("check the session of a token against its revocations: %v", err)
	}
	return !covered
}

// lineageLive reports whether the cloud still holds the SESSION tok belongs to
// — its lineage — unrevoked. A rotation retires the row of the token
// presented and hands out a successor of the same lineage, so sessionLive goes
// false on a session that is very much alive: only a sign-out of the lineage
// ends it, and that is what this reads.
func (c *realCloud) lineageLive(t *testing.T, tok string) bool {
	t.Helper()
	ctx := context.Background()
	sess, err := c.sessions.GetSessionByTokenHash(ctx, auth.HashRefreshToken(tok))
	if errors.Is(err, auth.ErrSessionNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read the session of a token: %v", err)
	}
	covered, err := c.sessions.SessionRevoked(ctx, sess)
	if err != nil {
		t.Fatalf("check the session of a token against its revocations: %v", err)
	}
	return !covered
}

func (s *failingSessions) CreateSession(ctx context.Context, x auth.Session) error {
	for {
		left := s.failCreates.Load()
		if left <= 0 {
			return s.MemorySessionStore.CreateSession(ctx, x)
		}
		if s.failCreates.CompareAndSwap(left, left-1) {
			return errors.New("auth: insert session: connection reset by peer")
		}
	}
}

// randomJWTSecret is a fresh signing secret in the form the config takes.
func randomJWTSecret(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.RawStdEncoding.EncodeToString(key)
}

func newRealCloud(t *testing.T) *realCloud {
	t.Helper()
	signer, err := auth.NewJWTSigner(randomJWTSecret(t), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessions := &failingSessions{MemorySessionStore: auth.NewMemorySessionStore()}
	svc, err := auth.NewService(auth.Config{Store: identity.NewMemoryStore(), Sessions: sessions, Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s := iserver.New(iserver.Config{AuthService: svc, AuthSigner: signer}, iterlog.New(iterlog.LevelError, nil))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &realCloud{url: srv.URL, svc: svc, sessions: sessions}
}

// seedJar registers an account, gives the jar its session, and opens a
// second session on another device.
func (c *realCloud) seedJar(t *testing.T) (*cloudTokenJar, *fakeStore, auth.LoginResult) {
	t.Helper()
	ctx := context.Background()
	desk, err := c.svc.Register(ctx, "desk@example.org", "password-forever-9", "", "", "desktop", "ip")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	other, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "browser", "ip2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	store := newFakeStore()
	jar := newCloudTokenJar("conn-1", c.url, store)
	if err := jar.seed(cloudAuthResult{AccessToken: desk.AccessToken, AccessExpiry: desk.AccessExpires, RefreshToken: desk.RefreshToken}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return jar, store, other
}

// TestJar_RetryAfterALostSuccessorRefreshes drives the hazard end to end:
// the server rotates the jar's token but fails to write the successor, so it
// answers 500 and the jar keeps its token. That token has to be presented
// again — and refresh, not read as reuse — and the presentation the refresh
// makes at once is what does it. When that one fails too the jar still holds
// the token and the loop's next attempt recovers.
func TestJar_RetryAfterALostSuccessorRefreshes(t *testing.T) {
	t.Run("the presentation at once recovers it", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		before := jar.storedRefresh()
		c.sessions.failNextCreates(1)
		if err := jar.refreshNow(context.Background()); err != nil {
			t.Fatalf("the refresh whose successor write failed once: %v — nothing presented the token again", err)
		}
		if got := jar.storedRefresh(); got == "" || got == before || store.m["cloud_refresh:conn-1"] != got {
			t.Fatalf("the retry did not seed and persist the successor (jar %q, store %q)", got, store.m["cloud_refresh:conn-1"])
		}
		if _, err := c.svc.Refresh(context.Background(), other.RefreshToken, "browser", "ip2"); err != nil {
			t.Fatalf("the account's other device lost its session: %v", err)
		}
	})
	t.Run("both presentations fail, the loop's next attempt recovers it", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		before := jar.storedRefresh()
		c.sessions.failNextCreates(2)
		err := jar.refreshNow(context.Background())
		ae := asCloudAuthError(err)
		if ae == nil || ae.Status != http.StatusInternalServerError {
			t.Fatalf("the refresh whose two presentations both failed: got %v, want a 500", err)
		}
		if jar.storedRefresh() != before {
			t.Fatal("a 500 changed the jar's token")
		}
		if err := jar.refreshNow(context.Background()); err != nil {
			t.Fatalf("the loop's retry with the same token: %v", err)
		}
		if got := jar.storedRefresh(); got == "" || got == before || store.m["cloud_refresh:conn-1"] != got {
			t.Fatalf("the retry did not seed and persist the successor (jar %q, store %q)", got, store.m["cloud_refresh:conn-1"])
		}
		if _, err := c.svc.Refresh(context.Background(), other.RefreshToken, "browser", "ip2"); err != nil {
			t.Fatalf("the account's other device lost its session: %v", err)
		}
	})
}

// slowSessions makes every session-store step of a refresh take delay — a
// loaded identity store — honouring cancellation as the Mongo driver does.
// The delay stays under the server's own per-step bound
// (auth.sessionOpTimeout, 5 s), so the server keeps succeeding: what the
// load breaks is the DESKTOP's deadline over the whole refresh.
type slowSessions struct {
	*auth.MemorySessionStore
	delay atomic.Int64
}

func (s *slowSessions) hold(ctx context.Context) error {
	d := time.Duration(s.delay.Load())
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowSessions) GetSessionByTokenHash(ctx context.Context, h string) (auth.Session, error) {
	if err := s.hold(ctx); err != nil {
		return auth.Session{}, err
	}
	return s.MemorySessionStore.GetSessionByTokenHash(ctx, h)
}

func (s *slowSessions) CreateSession(ctx context.Context, x auth.Session) error {
	if err := s.hold(ctx); err != nil {
		return err
	}
	return s.MemorySessionStore.CreateSession(ctx, x)
}

func (s *slowSessions) RotateSession(ctx context.Context, id string, rot auth.Rotation) (time.Time, bool, error) {
	if err := s.hold(ctx); err != nil {
		return time.Time{}, false, err
	}
	return s.MemorySessionStore.RotateSession(ctx, id, rot)
}

func (s *slowSessions) SessionRevoked(ctx context.Context, sess auth.Session) (bool, error) {
	if err := s.hold(ctx); err != nil {
		return false, err
	}
	return s.MemorySessionStore.SessionRevoked(ctx, sess)
}

// TestJar_ALoadedSessionStoreDoesNotCostTheSession is the incident the
// desktop's refresh deadline turns into a theft response if nothing presents
// the token again at once: a session store slow enough that one refresh
// needs more than cloudRefreshTimeout. The first presentation is abandoned
// AFTER the rotation — the desktop holds a token the server retired — and
// the presentation the refresh makes at once, under its wider deadline,
// receives that rotation's successor. The desktop keeps its own session and
// the account's other device keeps its one.
func TestJar_ALoadedSessionStoreDoesNotCostTheSession(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the desktop's real refresh deadline (~20 s)")
	}
	// Four session-store steps on a refresh's success path (the read of the
	// presented token, the rotation, the successor's write, its check), each
	// held this long: enough to outlast cloudRefreshTimeout, each under the
	// server's own 5 s per-step bound.
	const delay = 3 * time.Second
	signer, err := auth.NewJWTSigner(randomJWTSecret(t), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessions := &slowSessions{MemorySessionStore: auth.NewMemorySessionStore()}
	svc, err := auth.NewService(auth.Config{Store: identity.NewMemoryStore(), Sessions: sessions, Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s := iserver.New(iserver.Config{AuthService: svc}, iterlog.New(iterlog.LevelError, nil))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
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
	base := jar.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	jar.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		presented.Add(1)
		return base.RoundTrip(req)
	})

	sessions.delay.Store(int64(delay))
	start := time.Now()
	rerr := jar.refreshNow(ctx)
	took := time.Since(start)
	t.Logf("the refresh under a store holding every step %v: %v after %v, %d presentations", delay, rerr, took.Round(100*time.Millisecond), presented.Load())
	if n := presented.Load(); n != 2 {
		t.Fatalf("the refresh presented the token %d times, want 2: its first presentation was not abandoned at the %v deadline (store too fast for this probe)", n, cloudRefreshTimeout)
	}
	if rerr != nil {
		t.Fatalf("the refresh of a loaded store: %v — the desktop holds a token the server retired", rerr)
	}
	if got := jar.storedRefresh(); got == "" || got == t0 {
		t.Fatalf("the jar kept the retired token %q", got)
	}
	// The incident ends. The desktop's own session is live, and so is the
	// account's other device: nothing read the re-presentation as theft.
	sessions.delay.Store(0)
	if _, err := svc.Refresh(ctx, jar.storedRefresh(), "desktop", "ip"); err != nil {
		t.Fatalf("the desktop's own session did not survive the load: %v", err)
	}
	if _, err := svc.Refresh(ctx, other.RefreshToken, "browser", "ip2"); err != nil {
		t.Fatalf("the account's other device lost its session: %v", err)
	}
}

// countingRefreshCloud answers POST /api/auth/refresh with status — or, for
// 200, a rotated session — and counts every presentation.
type countingRefreshCloud struct {
	presented atomic.Int64
	status    int
}

func (c *countingRefreshCloud) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		n := c.presented.Add(1)
		if c.status != http.StatusOK {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":"no"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cloudRefreshCookieName, Value: fmt.Sprintf("rotated-%d", n)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"user":{"id":"u1"},"access_token":"access-%d"}`, n)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// unwritableStore persists nothing: its Set fails, as an OS keychain that
// refuses a write does.
type unwritableStore struct{ *fakeStore }

func (s *unwritableStore) Set(string, string) error { return errors.New("keychain: write denied") }

// TestJar_ARefreshThatRotatedTheTokenDoesNotPresentAgain pins the seam the
// presentation-at-once rests on: only a presentation whose answer never
// arrived is made again, so the token presented twice is always the SAME
// one. A refresh the cloud DID answer — one that rotated the token and then
// failed to persist its successor — is not presented again: a second
// presentation would rotate the successor, and the duplicate the first one
// may have left in flight is what the server reads as theft.
func TestJar_ARefreshThatRotatedTheTokenDoesNotPresentAgain(t *testing.T) {
	cloud := &countingRefreshCloud{status: http.StatusOK}
	store := newFakeStore()
	jar := newCloudTokenJar("conn-1", cloud.start(t), store)
	if err := jar.seed(cloudAuthResult{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	jar.store = &unwritableStore{store} // the keychain starts refusing writes
	err := jar.refreshNow(context.Background())
	if err == nil {
		t.Fatal("a refresh whose successor could not be persisted reported success")
	}
	if n := cloud.presented.Load(); n != 1 {
		t.Fatalf("a refresh that rotated the token presented it %d times, want 1 — the second presentation rotates the successor", n)
	}
	if got := jar.storedRefresh(); got != "rotated-1" {
		t.Fatalf("the jar holds %q, want the successor it was answered", got)
	}
}

// TestJar_WhatOneRefreshCostsTheCloud counts what the at-once retry adds to
// legitimate traffic: nothing on a served refresh, nothing on a refusal, and
// exactly ONE extra request per refresh whose answer the cloud could not
// give. The loop's floor, not this retry, paces what comes after.
func TestJar_WhatOneRefreshCostsTheCloud(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int64
	}{
		{http.StatusOK, 1},
		{http.StatusUnauthorized, 1},
		{http.StatusForbidden, 1},
		{http.StatusTooManyRequests, 1},
		{http.StatusNotFound, 1},
		{http.StatusInternalServerError, 2},
		{http.StatusBadGateway, 2},
		{http.StatusServiceUnavailable, 2},
		{http.StatusGatewayTimeout, 2},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			cloud := &countingRefreshCloud{status: tc.status}
			jar := newCloudTokenJar("conn-1", cloud.start(t), newFakeStore())
			if err := jar.seed(cloudAuthResult{AccessToken: "a", RefreshToken: "r"}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_ = jar.refreshNow(context.Background())
			if got := cloud.presented.Load(); got != tc.want {
				t.Fatalf("a refresh the cloud answered %d presented the token %d times, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// TestJar_ReapedSessionSignsInAgain: Mongo's TTL index deletes an expired
// session, so the desktop's expired token reaches a server with no row for
// it. That is a dead credential — 401, the jar cleared, the loop's
// cloud:auth-expired — never a 500 the loop retries every 15 s forever.
func TestJar_ReapedSessionSignsInAgain(t *testing.T) {
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)
	if _, err := c.sessions.DeleteExpired(context.Background(), time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("reap the sessions: %v", err)
	}
	err := jar.refreshNow(context.Background())
	ae := asCloudAuthError(err)
	if ae == nil || ae.Status != http.StatusUnauthorized {
		t.Fatalf("a refresh with a reaped session: got %v, want a 401", err)
	}
	if jar.LoggedIn() || jar.storedRefresh() != "" {
		t.Fatal("the jar kept a dead credential")
	}
	if _, ok := store.m["cloud_refresh:conn-1"]; ok {
		t.Fatal("the persisted dead credential was not deleted")
	}
}
