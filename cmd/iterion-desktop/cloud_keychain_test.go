package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// keychainBlip is the OS keychain failing its next reads (and deletes) the
// way go-keyring fails on a locked collection, a dismissed unlock prompt or
// a Secret Service that is not up yet — then answering again. gets counts
// every read the jar asks of it.
type keychainBlip struct {
	*fakeStore
	failGets    atomic.Int64
	failDeletes atomic.Int64
	gets        atomic.Int64
}

var errKeychainLocked = errors.New("failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'")

func (s *keychainBlip) Get(k string) (string, error) {
	s.gets.Add(1)
	if s.failGets.Add(-1) >= 0 {
		return "", errKeychainLocked
	}
	return s.fakeStore.Get(k)
}

func (s *keychainBlip) Delete(k string) error {
	if s.failDeletes.Add(-1) >= 0 {
		return errKeychainLocked
	}
	return s.fakeStore.Delete(k)
}

// ageReadFailure moves the jar's last failed store read one
// cloudRefreshRetryFloor back, as if that floor had passed since.
func ageReadFailure(jar *cloudTokenJar) { ageReadFailureBy(jar, cloudRefreshRetryFloor) }

// ageReadFailureBy moves the jar's last failed store read d back, as if d had
// passed since — a clock for the back-off, without waiting for it.
func ageReadFailureBy(jar *cloudTokenJar, d time.Duration) {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	jar.readFailedAt = jar.readFailedAt.Add(-d)
}

// readFailureCount is how many consecutive store reads have failed.
func readFailureCount(jar *cloudTokenJar) int {
	jar.mu.RLock()
	defer jar.mu.RUnlock()
	return jar.readFailures
}

// TestJar_AKeychainReadThatFailsIsNotASignOut drives a desktop restart whose
// first keychain read fails while the session it stores is live on the
// cloud. The failed read is the store's error, not "no session": the pane is
// answered 503 naming the KEYCHAIN — its session is kept and no server
// failed, so the remedy is the operator's — no re-login is asked for, the
// keychain entry is untouched, and once the keychain answers again the pane's
// next request reads it, renews the session and is served.
func TestJar_AKeychainReadThatFailsIsNotASignOut(t *testing.T) {
	c := newRealCloud(t)
	jar, store, other := c.seedJar(t)
	stored := jar.storedRefresh()
	key := cloudRefreshKeyPrefix + "conn-1"

	// The desktop restarts (openCloudConn / activateCloudConnection): a fresh
	// jar hydrated from a keychain whose first read fails.
	blip := &keychainBlip{fakeStore: store}
	blip.failGets.Store(1)
	jar2 := newCloudTokenJar("conn-1", c.url, blip)
	ok, herr := jar2.hydrate()
	if ok || !errors.Is(herr, errKeychainLocked) || !errors.Is(herr, errCloudSessionUnread) {
		t.Fatalf("hydrate over a failing keychain read = %v, %v; want false and the keychain's error", ok, herr)
	}

	code, body, expired := paneGET(t, jar2, c.url+"/api/auth/me")
	if code != http.StatusServiceUnavailable || body != keychainLocked {
		t.Fatalf("the pane's request while the keychain could not be read was answered %d %s, want 503 %s", code, body, keychainLocked)
	}
	if len(expired) > 0 {
		t.Fatalf("a keychain read that failed asked the shell for a re-login (%v): a live session declared over", expired)
	}
	if err := jar2.refreshNow(context.Background()); cloudRefreshRefused(err) || !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the loop's refresh while the keychain could not be read: %v, want errCloudSessionUnread (not a refusal)", err)
	}
	if got := store.m[key]; got != stored {
		t.Fatalf("the keychain entry changed during the blip: %q, want the stored session", got)
	}

	// The keychain answers again; a floor later the next request reads it.
	ageReadFailure(jar2)
	code, body, expired = paneGET(t, jar2, c.url+"/api/auth/me")
	if code != http.StatusOK || len(expired) > 0 {
		t.Fatalf("the pane's request once the keychain answered: %d %s (re-login asked: %v), want 200", code, body, expired)
	}
	if got := jar2.storedRefresh(); got == "" || got == stored || store.m[key] != got {
		t.Fatalf("the jar did not renew the stored session (jar %q, keychain %q)", got, store.m[key])
	}
	if _, err := c.svc.Refresh(context.Background(), other.RefreshToken, "browser", "ip2"); err != nil {
		t.Fatalf("the account's other device lost its session: %v", err)
	}
}

// TestJar_AFailingKeychainIsAskedAtMostOncePerBackoffStep: a locked keyring
// may answer every read with an unlock prompt, and a pane retries every few
// seconds. While reads fail, the refreshes inside the current back-off step
// answer the last failure without asking the keychain again. The first step
// is the floor; TestJar_AFailingKeychainIsAskedOnAGrowingBackoffThenOnlyOnAUserAction
// covers how it grows and ends.
func TestJar_AFailingKeychainIsAskedAtMostOncePerBackoffStep(t *testing.T) {
	blip := &keychainBlip{fakeStore: newFakeStore()}
	blip.failGets.Store(1 << 30)
	jar := newCloudTokenJar("conn-1", "http://cloud.invalid", blip)
	if _, err := jar.hydrate(); err == nil {
		t.Fatal("hydrate over a failing keychain read reported no error")
	}
	for i := 0; i < 20; i++ {
		if err := jar.refreshNow(context.Background()); !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("refresh %d: %v, want errCloudSessionUnread", i, err)
		}
	}
	if n := blip.gets.Load(); n != 1 {
		t.Fatalf("20 refreshes inside the floor asked the failing keychain %d times, want once (the hydrate)", n)
	}
	ageReadFailure(jar)
	if err := jar.refreshNow(context.Background()); !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the refresh a floor later: %v, want errCloudSessionUnread", err)
	}
	if n := blip.gets.Load(); n != 2 {
		t.Fatalf("a floor later the keychain was asked %d times in all, want 2: the refresh did not read it again", n)
	}
	// The second step is longer than the first: a floor is no longer enough.
	ageReadFailure(jar)
	if err := jar.refreshNow(context.Background()); !errors.Is(err, errCloudSessionUnread) {
		t.Fatalf("the refresh one floor after the second failure: %v, want errCloudSessionUnread", err)
	}
	if n := blip.gets.Load(); n != 2 {
		t.Fatalf("the keychain was asked %d times in all one floor after the second failure, want 2: the back-off did not grow", n)
	}
}

// TestJar_AFailingKeychainIsAskedOnAGrowingBackoffThenOnlyOnAUserAction is
// the DURATION bound a locked login keyring needs: every read of it is one OS
// unlock prompt, so a connection left open all day must not keep asking. The
// wait between two reads grows from the floor to cloudKeychainReadBackoffCap,
// and after cloudKeychainUnpromptedReads failures the jar asks no more on its
// own — only a user action (the pane's retry control, which the tunnel reads
// as one) buys another read. The pane meanwhile learns that the KEYCHAIN is
// what failed, not the server.
func TestJar_AFailingKeychainIsAskedOnAGrowingBackoffThenOnlyOnAUserAction(t *testing.T) {
	var sawRetryHeader atomic.Bool
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(cloudKeychainRetryHeader) != "" {
			sawRetryHeader.Store(true)
		}
		// No Bearer: the jar never read its refresh token, so it minted none.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"authentication required"}`)
	}))
	defer cloud.Close()

	blip := &keychainBlip{fakeStore: newFakeStore()}
	blip.failGets.Store(1 << 30)
	jar := newCloudTokenJar("conn-1", cloud.URL, blip)
	if _, err := jar.hydrate(); err == nil {
		t.Fatal("hydrate over a failing keychain read reported no error")
	}

	// A simulated hour, second by second: the connection is open and the pane
	// keeps asking. reads holds the second each keychain read happened at.
	reads := []int{0}
	const hour = 3600
	for s := 1; s <= hour; s++ {
		ageReadFailureBy(jar, time.Second)
		before := blip.gets.Load()
		if err := jar.refreshNow(context.Background()); !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("the refresh at second %d: %v, want errCloudSessionUnread", s, err)
		}
		if blip.gets.Load() > before {
			reads = append(reads, s)
		}
	}
	t.Logf("a permanently locked keychain was read at %v s of a simulated hour", reads)
	if len(reads) != cloudKeychainUnpromptedReads {
		t.Fatalf("over a simulated hour a permanently locked keychain was read %d times (at %v s), want %d: every read is an unlock prompt", len(reads), reads, cloudKeychainUnpromptedReads)
	}
	var prev time.Duration
	for i := 1; i < len(reads); i++ {
		gap := time.Duration(reads[i]-reads[i-1]) * time.Second
		if gap < cloudRefreshRetryFloor {
			t.Fatalf("reads %d and %d are %s apart, under the floor %s (at %v s)", i-1, i, gap, cloudRefreshRetryFloor, reads)
		}
		if gap > cloudKeychainReadBackoffCap {
			t.Fatalf("reads %d and %d are %s apart, over the cap %s (at %v s)", i-1, i, gap, cloudKeychainReadBackoffCap, reads)
		}
		if i > 1 && gap <= prev && prev < cloudKeychainReadBackoffCap {
			t.Fatalf("the wait did not grow: %s then %s, under the cap %s (at %v s)", prev, gap, cloudKeychainReadBackoffCap, reads)
		}
		prev = gap
	}

	// Another simulated hour: the budget is spent, so nothing asks.
	for s := 0; s < hour; s++ {
		ageReadFailureBy(jar, time.Second)
		if err := jar.refreshNow(context.Background()); !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("the refresh in the second hour: %v, want errCloudSessionUnread", err)
		}
	}
	if n := blip.gets.Load(); int(n) != cloudKeychainUnpromptedReads {
		t.Fatalf("the keychain was read %d times in all, want %d: the jar kept prompting after its budget", n, cloudKeychainUnpromptedReads)
	}

	// A pane request: told the keychain is locked, and asking for none.
	code, body, expired := paneGET(t, jar, cloud.URL+"/api/auth/me")
	if code != http.StatusServiceUnavailable || body != keychainLocked {
		t.Fatalf("a pane request while the keychain cannot be read was answered %d %s, want 503 %s", code, body, keychainLocked)
	}
	if len(expired) > 0 {
		t.Fatalf("a keychain that cannot be read asked the shell for a re-login (%v): a live session declared over", expired)
	}
	if n := blip.gets.Load(); int(n) != cloudKeychainUnpromptedReads {
		t.Fatalf("a pane request read the keychain (%d in all): only a user action may", n)
	}

	// The pane's retry control: one read now, whatever the back-off.
	code, body, _ = paneGETWithHeaders(t, jar, cloud.URL+"/api/auth/me", map[string]string{cloudKeychainRetryHeader: "1"})
	if code != http.StatusServiceUnavailable || body != keychainLocked {
		t.Fatalf("the retry the operator asked for was answered %d %s, want 503 %s", code, body, keychainLocked)
	}
	if n := blip.gets.Load(); int(n) != cloudKeychainUnpromptedReads+1 {
		t.Fatalf("the retry the operator asked for brought the reads to %d, want %d: it did not read the keychain", n, cloudKeychainUnpromptedReads+1)
	}
	if sawRetryHeader.Load() {
		t.Fatalf("%s reached the cloud: it is the desktop's own signal", cloudKeychainRetryHeader)
	}

	// And it buys ONE read, not a new cadence.
	code, _, _ = paneGET(t, jar, cloud.URL+"/api/auth/me")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("the pane request after the retry was answered %d, want 503", code)
	}
	if n := blip.gets.Load(); int(n) != cloudKeychainUnpromptedReads+1 {
		t.Fatalf("the reads reached %d after one user action, want %d", n, cloudKeychainUnpromptedReads+1)
	}
}

// heldKeychain answers its reads with the value it held when the read
// STARTED, once release is closed: a slow keychain (an unlock prompt) that a
// rotation overtakes.
type heldKeychain struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
}

func (s *heldKeychain) Get(k string) (string, error) {
	v, err := s.fakeStore.Get(k)
	close(s.entered)
	<-s.release
	return v, err
}

// TestJar_ARotationDuringAKeychainReadIsNotOverwritten: a jar whose keychain
// read failed reads it again at its next refresh, and a pane's sign-in
// through the tunnel may rotate the jar's refresh token while that read is
// slow. The read answers the token it found when it started — an older
// session — and must not replace the one the rotation installed.
func TestJar_ARotationDuringAKeychainReadIsNotOverwritten(t *testing.T) {
	store := &heldKeychain{fakeStore: newFakeStore(), entered: make(chan struct{}), release: make(chan struct{})}
	store.fakeStore.m[cloudRefreshKeyPrefix+"conn-1"] = "older-session"
	jar := newCloudTokenJar("conn-1", "http://cloud.invalid", store)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = jar.hydrate()
	}()
	<-store.entered
	if err := jar.applyRotation(mkJWT(time.Now().Add(15*time.Minute).Unix()), "rotated-session"); err != nil {
		t.Fatalf("applyRotation: %v", err)
	}
	close(store.release)
	<-done
	if got := jar.storedRefresh(); got != "rotated-session" {
		t.Fatalf("the jar holds %q after a keychain read the rotation overtook, want the rotated session", got)
	}
}

// TestCloudRefreshLoop_AKeychainReadThatFailsKeepsItRunning: the loop over a
// jar whose keychain read failed neither emits cloud:auth-expired nor stops —
// the failure is transient, and its next attempt reads the keychain again —
// and once the jar's unprompted budget is spent it keeps running without
// asking the keychain at all: it is the pane's retry, not this loop, that
// buys the next unlock prompt.
func TestCloudRefreshLoop_AKeychainReadThatFailsKeepsItRunning(t *testing.T) {
	blip := &keychainBlip{fakeStore: newFakeStore()}
	blip.failGets.Store(1 << 30)
	jar := newCloudTokenJar("conn-1", "http://cloud.invalid", blip)
	if _, err := jar.hydrate(); err == nil {
		t.Fatal("hydrate over a failing keychain read reported no error")
	}
	ageReadFailure(jar)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expired := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cloudRefreshLoop(ctx, jar, func(id string) { expired <- id })
	}()
	jar.kickRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for blip.gets.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never attempted a refresh after the wake")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case id := <-expired:
		t.Fatalf("the loop emitted cloud:auth-expired for %s over a keychain read that failed", id)
	case <-done:
		t.Fatal("the loop stopped over a keychain read that failed")
	case <-time.After(time.Second):
	}

	// Spend the unprompted budget, then wake the loop again: it runs, and the
	// keychain is left alone.
	for readFailureCount(jar) < cloudKeychainUnpromptedReads {
		ageReadFailureBy(jar, cloudKeychainReadBackoffCap)
		if err := jar.refreshNow(ctx); !errors.Is(err, errCloudSessionUnread) {
			t.Fatalf("the refresh spending the budget: %v, want errCloudSessionUnread", err)
		}
	}
	spent := blip.gets.Load()
	ageReadFailureBy(jar, cloudKeychainReadBackoffCap)
	jar.kickRefresh()
	select {
	case id := <-expired:
		t.Fatalf("the loop emitted cloud:auth-expired for %s once the keychain budget was spent", id)
	case <-done:
		t.Fatal("the loop stopped once the keychain budget was spent")
	case <-time.After(2 * time.Second):
	}
	if n := blip.gets.Load(); n != spent {
		t.Fatalf("the loop read the keychain %d more time(s) after the budget was spent: an unlock prompt nobody asked for", n-spent)
	}
}
