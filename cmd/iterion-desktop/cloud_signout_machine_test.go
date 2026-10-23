package main

// Witnesses of the client sign-out state machine (round 7, org-tree-errors-1668)
// on the desktop half: the token jar, its keychain entry, the pane tunnel's
// jar and the connections registry. The machine and its invariants live in
// the design doc the round was written from (§I invariants, §III the desktop
// machine); each witness here is one finding of rounds 6-7 replayed as a
// machine transition against the real pkg/server cloud, asserted on the
// state the keychain and the cloud hold afterwards.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeGateStore blocks the first Set after arm until release is closed: a
// keychain write that a hold overtakes (a slow OS keyring, a prompt).
type writeGateStore struct {
	*fakeStore
	mu      sync.Mutex
	arm     bool
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *writeGateStore) Set(k, v string) error {
	s.mu.Lock()
	armed := s.arm
	s.arm = false
	s.mu.Unlock()
	if armed {
		s.once.Do(func() { close(s.blocked) })
		<-s.release
	}
	return s.fakeStore.Set(k, v)
}

// TestARefreshWriteNeverLandsAfterTheHoldItPrecedes (r7c F6, invariant I5):
// the entry writes of one jar are serialised under the jar's write lock, so
// a refresh whose keychain write is slow and the hold the operator takes
// while it is in flight cannot reorder: the entry ends carrying the mark, and
// a restart reading it adopts the hold, never the session the operator signed
// out of.
func TestARefreshWriteNeverLandsAfterTheHoldItPrecedes(t *testing.T) {
	ctx := context.Background()
	c := newRealCloud(t)
	seeded, base, _ := c.seedJar(t)
	stored := seeded.storedRefresh()
	key := cloudRefreshKeyPrefix + "conn-1"
	st := &writeGateStore{fakeStore: base, blocked: make(chan struct{}), release: make(chan struct{})}
	jar := newCloudTokenJar("conn-1", c.url, st)
	if _, err := jar.hydrate(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	st.mu.Lock()
	st.arm = true
	st.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- jar.refreshNow(ctx) }()
	select {
	case <-st.blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the refresh never reached its keychain write")
	}
	// The refresh answered and holds the jar's write lock while it writes its
	// successor. The operator signs out in the pane: the mark waits for that
	// write to land (serialised), and the cloud fails its revocation, so the
	// hold is taken once the sign-out reaches the cloud.
	c.sessions.failNextRevocations(1)
	logoutDone := make(chan int, 1)
	go func() {
		code, _ := paneLogout(t, jar, http.DefaultTransport, c.url)
		logoutDone <- code
	}()
	close(st.release)
	if err := <-done; err != nil {
		t.Fatalf("the refresh whose write was slow: %v", err)
	}
	if code := <-logoutDone; code != http.StatusInternalServerError {
		t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
	}
	if !jar.SignOutPending() {
		t.Fatal("the jar took no hold")
	}
	if got := st.m[key]; !strings.HasPrefix(got, cloudSignOutPendingMark) {
		t.Fatalf("the entry after the write and the hold is %q: a write reordered past the hold unmarks it and a restart adopts the session the operator signed out of", got)
	}
	// The reverse order stands too: a hold that lands before the refresh's
	// write drops the whole answer (the existing witness
	// TestJar_ARefreshInFlightAcrossAHeldSignOutDoesNotReAdoptTheSession).
	//
	// The restart that reads the entry: still owed, still presenting nothing.
	restarted := newCloudTokenJar("conn-1", c.url, base)
	if hydrated, herr := restarted.hydrate(); !hydrated || herr != nil {
		t.Fatalf("hydrate after the restart = %v, %v", hydrated, herr)
	}
	if !restarted.SignOutPending() {
		t.Fatal("the restart adopted the session the operator signed out of")
	}
	if rerr := restarted.refreshNow(ctx); rerr == nil || !strings.Contains(asCloudAuthError(rerr).Message, "sign-out") {
		t.Fatalf("the restarted connection's renewal: %v, want errCloudSignOutPending", rerr)
	}
	if !c.lineageLive(t, stored) {
		t.Fatal("the session the operator signed out of did not survive for its sign-out to be presented")
	}
}

// TestATokenlessHoldIsBoundToTheEntry (r7c F12, invariant I4): a sign-out
// held while the keychain could not be read holds no token. Bound to the
// ENTRY, it adopts at the next read and presents the token it finds there —
// the session the operator asked to end — never the token a sign-in wrote
// after it: that one is carried as the new session and the stale hold stays
// owed beside it.
func TestATokenlessHoldIsBoundToTheEntry(t *testing.T) {
	ctx := context.Background()

	t.Run("the machine's sign-in presents the entry's session first", func(t *testing.T) {
		c := newRealCloud(t)
		seeded, base, _ := c.seedJar(t)
		asked := seeded.storedRefresh()
		key := cloudRefreshKeyPrefix + "conn-1"
		blip := &keychainBlip{fakeStore: base}
		blip.failGets.Store(1 << 20) // the keychain is locked
		jar := newCloudTokenJar("conn-1", c.url, blip)
		code, body := paneLogout(t, jar, http.DefaultTransport, c.url)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("precondition: the pane's sign-out over a locked keychain was answered %d %s, want 503", code, body)
		}
		if !jar.SignOutPending() || jar.storedRefresh() != "" {
			t.Fatalf("precondition: the hold is not the tokenless in-memory one (pending %t, token %q)", jar.SignOutPending(), jar.storedRefresh())
		}
		blip.failGets.Store(0) // the operator unlocks the keychain
		// The sign-in mints its session, and the machine — having read the
		// entry first — presents the tokenless hold's session before seeding.
		c.sessions.failNextRevocations(1)
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatal(err)
		}
		if err := jar.prepareForSignIn(ctx, jar); err != nil {
			t.Fatalf("the sign-in's presentation: %v", err)
		}
		if err := jar.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if jar.storedRefresh() != res.RefreshToken || !jar.LoggedIn() {
			t.Fatalf("the jar does not hold the new session (%q, logged in %t)", jar.storedRefresh(), jar.LoggedIn())
		}
		if !jar.holdsSignOut() {
			t.Fatal("the tokenless hold was lost at the sign-in: the session the operator ended stays live with nothing able to end it")
		}
		if got := base.m[key]; got != encodeStoredSession(res.RefreshToken, false, []string{asked}) {
			t.Fatalf("the entry is %q, want the new token carrying the hold it adopted", got)
		}
		if err := jar.presentHeldSignOuts(ctx); err != nil {
			t.Fatalf("presenting the adopted hold: %v", err)
		}
		if c.lineageLive(t, asked) {
			t.Fatal("the adopted hold was not presented: the operator's sign-out is still owed")
		}
		if !c.sessionLive(t, res.RefreshToken) {
			t.Fatal("the presentation ended the session the sign-in opened")
		}
	})

	t.Run("a stale jar's tokenless hold never presents a token a sign-in wrote after it", func(t *testing.T) {
		c := newRealCloud(t)
		seeded, base, _ := c.seedJar(t)
		asked := seeded.storedRefresh()
		key := cloudRefreshKeyPrefix + "conn-1"
		blip := &keychainBlip{fakeStore: base}
		blip.failGets.Store(1 << 20)
		stale := newCloudTokenJar("conn-1", c.url, blip)
		if code, _ := paneLogout(t, stale, http.DefaultTransport, c.url); code != http.StatusServiceUnavailable {
			t.Fatal("precondition: the pane's sign-out over a locked keychain was not answered 503")
		}
		blip.failGets.Store(0)
		res, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
		if err != nil {
			t.Fatal(err)
		}
		// The sign-in through another jar of the connection (the shape the
		// two-jar window produced): it adopts the hold, presents, and carries
		// what the cloud refuses beside the new session.
		c.sessions.failNextRevocations(1)
		fresh := newCloudTokenJar("conn-1", c.url, blip)
		if err := fresh.prepareForSignIn(ctx, stale); err != nil {
			t.Fatalf("the sign-in's presentation: %v", err)
		}
		if err := fresh.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if !fresh.holdsSignOut() {
			t.Fatal("precondition: the sign-in did not carry the refused sign-out beside the new session")
		}
		// The stale jar's presentation — its loop's, or the pane's Retry. Its
		// tokenless hold reads the entry the sign-in wrote: the new session's
		// token is never presented; the hold stands down for the named one.
		ageReadFailureBy(stale, cloudKeychainReadBackoffCap)
		if err := stale.retrySignOut(ctx); err != nil {
			t.Logf("the stale hold's presentation answered %v", err)
		}
		if !c.sessionLive(t, res.RefreshToken) {
			t.Fatalf("the stale hold presented the session the sign-in opened: the entry is %q", base.m[key])
		}
		if err := fresh.presentHeldSignOuts(ctx); err != nil {
			t.Fatalf("presenting the held sign-out: %v", err)
		}
		if c.lineageLive(t, asked) {
			t.Fatal("the sign-out the operator asked for was never presented")
		}
		if got := base.m[key]; got != res.RefreshToken {
			t.Fatalf("the entry is %q after the held sign-out was confirmed, want the new token alone", got)
		}
	})
}

// TestARefusedRefreshOfAReplacedTokenLeavesTheNewSession (r7c F13, invariant
// I3): a refresh refused with a credential-dead answer clears the entry only
// if the jar still holds the token that was refused — compared under the
// lock. The password change revoked the old session while its renewal was on
// the wire and the pane's new session already sits in the jar and the entry:
// the refusal concerns the replaced session, and the entry is left to its new
// holder.
func TestARefusedRefreshOfAReplacedTokenLeavesTheNewSession(t *testing.T) {
	ctx := context.Background()
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)
	old := jar.storedRefresh()
	key := cloudRefreshKeyPrefix + "conn-1"
	held := holdRefreshAtTheTransport(jar)
	done := make(chan error, 1)
	go func() { done <- jar.refreshNow(ctx) }()
	<-held.arrived
	// The in-app password change: every session of the account revoked, a new
	// one delivered through the pane, its cookies harvested by the tunnel.
	if _, err := c.svc.LogoutOf(ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.svc.Login(ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if err := jar.applyRotation(fresh.AccessToken, fresh.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if store.m[key] != fresh.RefreshToken {
		t.Fatalf("precondition: the entry does not hold the new session (%q)", store.m[key])
	}
	close(held.release)
	rerr := <-done
	if rerr == nil || !cloudRefreshRefused(rerr) {
		t.Fatalf("precondition: the replaced token's refresh was not refused (%v)", rerr)
	}
	if got := jar.storedRefresh(); got != fresh.RefreshToken || !jar.LoggedIn() {
		t.Fatalf("the refusal cleared the jar's NEW session (token %q, logged in %t)", got, jar.LoggedIn())
	}
	if got := store.m[key]; got != fresh.RefreshToken {
		t.Fatalf("the refusal cleared the entry's NEW session: %q", got)
	}
	if !c.sessionLive(t, fresh.RefreshToken) {
		t.Fatal("the new session is no longer live on the cloud")
	}
	// The jar renews the session it holds, and the loop does not ask the shell
	// for a sign-in over a session that is alive.
	if err := jar.refreshNow(ctx); err != nil {
		t.Fatalf("the renewal of the session the jar holds: %v", err)
	}
}

// flakyMe401 answers the first request to a path with a 401 and proxies the
// rest: a pane request whose Bearer was already stale once.
type flakyMe401 struct {
	base  http.RoundTripper
	path  string
	mu    sync.Mutex
	fired bool
}

func (f *flakyMe401) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	fire := req.URL.Path == f.path && !f.fired
	if fire {
		f.fired = true
	}
	f.mu.Unlock()
	if fire {
		return jsonResponse(req, http.StatusUnauthorized, `{"error":"authentication required"}`), nil
	}
	return f.base.RoundTrip(req)
}

// TestARefusalOfAReplacedTokenIsNotAnExpiry (r7c F13, invariant I1): the
// password change replaced the session while its renewal was on the wire; the
// refusal that comes back names the replaced token, and neither the
// background loop nor the pane tunnel may read it as the END of the session
// the jar now holds — the shell is not asked for a sign-in, and the pane's
// request is answered with the live session.
func TestARefusalOfAReplacedTokenIsNotAnExpiry(t *testing.T) {
	c := newRealCloud(t)
	jar, _, _ := c.seedJar(t)
	old := jar.storedRefresh()
	// The pane's renewal of the old token is on the wire when the in-app
	// password change revokes every session and delivers the new one, whose
	// cookies the tunnel harvests (applyRotation).
	held := holdRefreshAtTheTransport(jar)
	fault := &flakyMe401{base: http.DefaultTransport, path: "/api/auth/me"}
	expired := make(chan string, 1)
	tun := &cloudRoundTripper{base: fault, jar: jar, onExpired: func(id string) { expired <- id }}
	req, err := http.NewRequest(http.MethodGet, c.url+"/api/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok := jar.AccessToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	type paneAnswer struct {
		code int
		err  error
	}
	answered := make(chan paneAnswer, 1)
	go func() {
		resp, rerr := tun.RoundTrip(req)
		code := 0
		if resp != nil {
			code = resp.StatusCode
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		answered <- paneAnswer{code, rerr}
	}()
	<-held.arrived
	if _, err := c.svc.LogoutOf(context.Background(), old, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.svc.Login(context.Background(), "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if err := jar.applyRotation(fresh.AccessToken, fresh.RefreshToken); err != nil {
		t.Fatal(err)
	}
	close(held.release)
	ans := <-answered
	if ans.err != nil {
		t.Fatalf("the pane's request: %v", ans.err)
	}
	if ans.code != http.StatusOK {
		t.Fatalf("the pane's request was answered %d: the refusal of a replaced token ended the live session's service", ans.code)
	}
	select {
	case id := <-expired:
		t.Fatalf("the shell was asked to sign %s in again over a session the jar holds live", id)
	default:
	}
	// The loop keeps renewing the session it holds: the next renewal succeeds.
	if err := jar.refreshNow(context.Background()); err != nil {
		t.Fatalf("the renewal of the session the jar holds after the refusal: %v", err)
	}
}

// TestTheLoopRenewsPastARefusalOfAReplacedToken (r7c F13, invariant I1): the
// loop's own renewal is the one the password change's refusal answers — the
// session it named was replaced while the renewal was on the wire. The loop
// asks the shell for no sign-in and keeps renewing the session the jar now
// holds.
func TestTheLoopRenewsPastARefusalOfAReplacedToken(t *testing.T) {
	c := newRealCloud(t)
	jar, _, _ := c.seedJar(t)
	old := jar.storedRefresh()
	held := holdRefreshAtTheTransport(jar)
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expired := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cloudRefreshLoop(loopCtx, jar, func(id string) { expired <- id })
	}()
	jar.kickRefresh()
	select {
	case <-held.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the loop never started a renewal")
	}
	if _, err := c.svc.LogoutOf(context.Background(), old, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.svc.Login(context.Background(), "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		t.Fatal(err)
	}
	// The harvested access is near its expiry, so the loop's next wait floors
	// to cloudRefreshRetryFloor: the renewal after the refusal is observable.
	if err := jar.applyRotation(mkJWT(time.Now().Add(2*time.Minute+20*time.Second).Unix()), fresh.RefreshToken); err != nil {
		t.Fatal(err)
	}
	replaced := fresh.RefreshToken
	close(held.release)
	// The loop's renewal is refused for the replaced token; the next pass
	// renews the session the jar holds, and the token rotates under it.
	deadline := time.Now().Add(30 * time.Second)
	for jar.storedRefresh() == replaced {
		select {
		case id := <-expired:
			t.Fatalf("the loop asked the shell to sign %s in again over a session the jar holds live", id)
		case <-done:
			t.Fatal("the loop returned over a refusal that named a replaced token")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop never renewed the session the jar holds after the refusal")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case id := <-expired:
		t.Fatalf("the loop asked the shell to sign %s in again after renewing", id)
	default:
	}
}
