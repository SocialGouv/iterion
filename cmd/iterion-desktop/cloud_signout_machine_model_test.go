package main

// The machine over SHORT EVENT SEQUENCES (round 7, org-tree-errors-1668):
// each sequence drives the desktop machine's events — a pane sign-out, a
// refresh, a sign-in, the keychain locking and unlocking, a presentation of
// a held sign-out, a connection removal, a restart — against the real
// pkg/server cloud, and checks the invariants of the design (§I mapped to
// §III) after EVERY event, not only at the end.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// logoutRecorder records the refresh token every sign-out presentation of
// this connection carried, so the model can check that no presentation ever
// named a session the operator did not ask to end.
type logoutRecorder struct {
	base   http.RoundTripper
	mu     sync.Mutex
	tokens []string
}

func (r *logoutRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/api/auth/logout") {
		r.mu.Lock()
		r.tokens = append(r.tokens, req.Header.Get("X-Iterion-Refresh"))
		r.mu.Unlock()
	}
	return r.base.RoundTrip(req)
}

// machineModel is the state of one connection's machine during a sequence.
type machineModel struct {
	t      *testing.T
	ctx    context.Context
	c      *realCloud
	store  *fakeStore
	jar    *cloudTokenJar
	rec    *logoutRecorder
	locked bool
	// asked are the refresh tokens whose session the operator asked to end
	// and the cloud has not confirmed yet. served is the token of the session
	// the jar currently serves ("" when it serves none).
	asked  []string
	served string
}

func newMachineModel(t *testing.T) *machineModel {
	t.Helper()
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)
	m := &machineModel{t: t, ctx: context.Background(), c: c, store: store, jar: jar}
	m.served = jar.storedRefresh()
	m.wrap(jar)
	return m
}

// wrap puts the presentation recorder under a jar's HTTP client.
func (m *machineModel) wrap(jar *cloudTokenJar) {
	base := jar.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	m.rec = &logoutRecorder{base: base}
	jar.hc.Transport = m.rec
}

// — the events —

// eSignOut runs a pane's sign-out; refuse makes the cloud's revocation fail.
func (m *machineModel) eSignOut(refuse bool) {
	m.t.Helper()
	if refuse {
		m.c.sessions.failNextRevocations(1)
	}
	code, _ := paneLogout(m.t, m.jar, http.DefaultTransport, m.c.url)
	if refuse && code != http.StatusInternalServerError {
		m.t.Fatalf("the pane's sign-out was answered %d, want the cloud's 500", code)
	}
	if !refuse {
		if code != http.StatusNoContent {
			m.t.Fatalf("the pane's confirmed sign-out was answered %d, want 204", code)
		}
		m.asked = nil
		m.served = ""
		return
	}
	if m.served != "" && !containsToken(m.asked, m.served) {
		m.asked = append(m.asked, m.served)
	}
}

// eRefresh runs one renewal; on success the jar serves the rotated token.
func (m *machineModel) eRefresh() {
	m.t.Helper()
	if err := m.jar.refreshNow(m.ctx); err != nil {
		m.t.Logf("the renewal answered %v", err)
		return
	}
	m.served = m.jar.storedRefresh()
}

// eSignIn mints a session and signs in through the machine; refuse makes the
// presentation of an owed sign-out fail.
func (m *machineModel) eSignIn(refuse bool) {
	m.t.Helper()
	if refuse {
		m.c.sessions.failNextRevocations(1)
	}
	res, err := m.c.svc.Login(m.ctx, "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		m.t.Fatalf("the sign-in's mint: %v", err)
	}
	if err := m.jar.prepareForSignIn(m.ctx, m.jar); err != nil {
		m.t.Fatalf("the sign-in's presentation: %v", err)
	}
	if err := m.jar.seed(cloudAuthResult{AccessToken: res.AccessToken, AccessExpiry: res.AccessExpires, RefreshToken: res.RefreshToken}); err != nil {
		m.t.Fatalf("the sign-in's seed: %v", err)
	}
	m.served = res.RefreshToken
}

// eLock / eUnlock fail and answer the keychain's reads again.
func (m *machineModel) eLock() {
	m.t.Helper()
	blip := &keychainBlip{fakeStore: m.store}
	blip.failGets.Store(1 << 30)
	m.jar.store = blip
	m.locked = true
}

func (m *machineModel) eUnlock() {
	m.t.Helper()
	m.jar.store = m.store
	m.locked = false
}

// eRetrySignOut presents a held sign-out once.
func (m *machineModel) eRetrySignOut() {
	m.t.Helper()
	if err := m.jar.retrySignOut(m.ctx); err != nil {
		m.t.Logf("the presentation answered %v", err)
	}
}

// eRemove removes the connection through the machine's transition.
func (m *machineModel) eRemove() {
	m.t.Helper()
	if err := m.jar.removeConnection(m.ctx); err != nil {
		m.t.Logf("the removal was refused: %v", err)
		return
	}
	m.served = ""
	m.asked = nil
}

// eRestart reads the entry the way a fresh process does: hydrate, then the
// startup renewal (activateCloudConnection / openCloudConn).
func (m *machineModel) eRestart() {
	m.t.Helper()
	if m.locked {
		m.t.Skip("a restart over a locked keychain reads nothing; the hold is process-local by design")
	}
	restarted := newCloudTokenJar("conn-1", m.c.url, m.store)
	if _, err := restarted.hydrate(); err != nil {
		m.t.Fatalf("the restart's hydrate: %v", err)
	}
	m.jar = restarted
	m.wrap(restarted)
	if restarted.storedRefresh() != "" {
		m.served = restarted.storedRefresh()
		if err := restarted.refreshNow(m.ctx); err != nil {
			m.t.Logf("the restart's renewal answered %v", err)
		} else {
			m.served = restarted.storedRefresh()
		}
	}
}

// eSwitchProject makes the connection the current one the way the project
// switch does (activateCloudConnection → activateConnectionJar): the
// connection's ONE jar is resolved — the registry jar the model holds — and
// the renewal runs through it. The one-jar invariant is the assertion: a
// switch that built a second jar over the entry would leave the stale jar
// presenting a rotated-away token, whose next tick revokes the lineage.
func (m *machineModel) eSwitchProject() {
	m.t.Helper()
	got := activateConnectionJar(m.ctx, m.jar, "conn-1", m.c.url, m.store)
	if got != m.jar {
		m.t.Fatal("the project switch built a second jar over the connection's keychain entry (one jar per connection)")
	}
	if tok := m.jar.storedRefresh(); tok != "" {
		m.served = tok
	}
}

// eClosePane closes the connection's pane the way CloseConnection does: the
// machine's PANE CLOSED transition runs first (closeForPane), and a close
// that would drop what the jar owes is refused — the model keeps the jar.
func (m *machineModel) eClosePane() {
	m.t.Helper()
	if err := m.jar.closeForPane(m.ctx); err != nil {
		m.t.Logf("the pane close was refused: %v", err)
		return
	}
}

func containsToken(list []string, tok string) bool {
	for _, t := range list {
		if t == tok {
			return true
		}
	}
	return false
}

// — the invariants, checked after every event —

func (m *machineModel) check() {
	m.t.Helper()
	// I3: no presentation named a session the operator did not ask to end.
	m.rec.mu.Lock()
	presented := append([]string(nil), m.rec.tokens...)
	m.rec.mu.Unlock()
	for _, tok := range presented {
		if tok == "" || containsToken(m.asked, tok) {
			continue
		}
		if m.c.sessionLive(m.t, tok) {
			m.t.Fatalf("a presentation carried %q, a live session the operator never asked to end (asked %v)", tok, m.asked)
		}
	}
	// I1: a session the jar serves and the cloud holds live is presented to
	// the pane — never withheld without a mark of its own.
	if m.served != "" && !m.locked && !m.jar.SignOutPending() && m.c.sessionLive(m.t, m.served) {
		if !m.jar.LoggedIn() {
			m.t.Fatalf("the jar serves %q, live on the cloud, and hands the pane no token (logged in %t)", m.served, m.jar.LoggedIn())
		}
	}
	// I5: the persisted entry is the machine — a restart adopts exactly it.
	if !m.locked {
		fresh := newCloudTokenJar("conn-1", m.c.url, m.store)
		if _, err := fresh.hydrate(); err != nil {
			m.t.Fatalf("the restart's hydrate: %v", err)
		}
		entry := m.store.m[cloudRefreshKeyPrefix+"conn-1"]
		refresh, pending, held := decodeStoredSession(entry)
		if fresh.SignOutPending() != pending || len(held) > 0 != fresh.holdsSignOut() {
			m.t.Fatalf("a restart of entry %q sees pending %t held %t; the machine holds pending %t held %t", entry, pending, len(held) > 0, fresh.SignOutPending(), fresh.holdsSignOut())
		}
		if pending && refresh != m.jar.storedRefresh() && m.jar.storedRefresh() != "" {
			m.t.Logf("note: the jar renewed past the restart's view (served %q)", m.jar.storedRefresh())
		}
		// What the entry names as owed, the cloud still holds live: nothing
		// owed is lost with the token that ends it.
		for _, tok := range append(append([]string(nil), held...), func() []string {
			if pending {
				return []string{refresh}
			}
			return nil
		}()...) {
			if tok != "" && !m.c.lineageLive(m.t, tok) {
				m.t.Fatalf("the entry still carries the sign-out of %q, but the cloud ended that session already", tok)
			}
		}
	}
}

type machineEvent struct {
	name string
	run  func(m *machineModel)
}

// runSequence drives the events, checking the invariants after each.
func runSequence(t *testing.T, name string, events []machineEvent) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		m := newMachineModel(t)
		m.check()
		for _, e := range events {
			e.run(m)
			m.check()
		}
	})
}

// TestTheSignOutMachineKeepsItsInvariantsOverShortEventSequences enumerates
// the short sequences the findings of rounds 6-7 came from.
func TestTheSignOutMachineKeepsItsInvariantsOverShortEventSequences(t *testing.T) {
	runSequence(t, "a refused sign-out survives a renewal and a restart", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"renewal", func(m *machineModel) { m.eRefresh() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a sign-in over a refused sign-out carries it beside", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"sign-in, presentation refused", func(m *machineModel) { m.eSignIn(true) }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a locked keychain holds the sign-out in memory, then presents it", []machineEvent{
		{"keychain locked", func(m *machineModel) { m.eLock() }},
		{"sign-out over the lock", func(m *machineModel) { m.eSignOut(true) }},
		{"keychain unlocked", func(m *machineModel) { m.eUnlock() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a tokenless hold meets a sign-in and never ends it", []machineEvent{
		{"keychain locked", func(m *machineModel) { m.eLock() }},
		{"sign-out over the lock", func(m *machineModel) { m.eSignOut(true) }},
		{"keychain unlocked", func(m *machineModel) { m.eUnlock() }},
		{"sign-in, presentation refused", func(m *machineModel) { m.eSignIn(true) }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a sign-in over a confirmed sign-out starts clean", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"sign-in, presentation confirmed", func(m *machineModel) { m.eSignIn(false) }},
		{"restart", func(m *machineModel) { m.eRestart() }},
	})
	runSequence(t, "a confirmed sign-out and a removal", []machineEvent{
		{"sign-out confirmed", func(m *machineModel) { m.eSignOut(false) }},
		{"sign-in", func(m *machineModel) { m.eSignIn(false) }},
		{"removal", func(m *machineModel) { m.eRemove() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
	})
	runSequence(t, "a removal presents what a refused sign-out owes", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"removal", func(m *machineModel) { m.eRemove() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
	})
	runSequence(t, "renewals over a held sign-out", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"renewal", func(m *machineModel) { m.eRefresh() }},
		{"sign-in, presentation refused", func(m *machineModel) { m.eSignIn(true) }},
		{"renewal", func(m *machineModel) { m.eRefresh() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a project switch renews the jar the pane tunnel holds", []machineEvent{
		{"renewal", func(m *machineModel) { m.eRefresh() }},
		{"project switch", func(m *machineModel) { m.eSwitchProject() }},
		{"renewal", func(m *machineModel) { m.eRefresh() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
	})
	runSequence(t, "a refused sign-out survives a project switch", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"project switch", func(m *machineModel) { m.eSwitchProject() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a pane close leaves what it owes on the entry", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"pane closed", func(m *machineModel) { m.eClosePane() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
	runSequence(t, "a pane close of a held-beside sign-out keeps it", []machineEvent{
		{"sign-out refused", func(m *machineModel) { m.eSignOut(true) }},
		{"sign-in, presentation refused", func(m *machineModel) { m.eSignIn(true) }},
		{"pane closed", func(m *machineModel) { m.eClosePane() }},
		{"restart", func(m *machineModel) { m.eRestart() }},
		{"presentation", func(m *machineModel) { m.eRetrySignOut() }},
	})
}
