//go:build desktop

package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestASignInReSeedingAnOpenPaneKeepsOneRenewalLoop (one renewal loop per
// connection): a sign-in on a NON-current connection whose pane is open
// re-seeds the pane's own jar — the retire guard does not fire — and the
// install stops the renewal loop the entry already ran before it starts the
// fresh one. A loop left untracked runs to app exit and doubles the
// renewal/presentation cadence. The control is the same install with the
// connection current: the entry stops its own loop and borrows the active
// one.
func TestASignInReSeedingAnOpenPaneKeepsOneRenewalLoop(t *testing.T) {
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)

	// A config whose cloud connection's id IS the jar's connID ("conn-1",
	// seedJar's), so a.conns[jar.connID] finds the entry — the shape the
	// real flow always has.
	cfg := NewConfig()
	cfg.RecentProjects = append(cfg.RecentProjects, Project{ID: "conn-1", Kind: ProjectKindCloud, CloudURL: c.url, CloudUserID: "u1", Name: "conn-1"})
	cfg.RecentProjects = append(cfg.RecentProjects, Project{ID: "other-1", Kind: ProjectKindCloud, CloudURL: c.url + "/other", CloudUserID: "u2", Name: "other"})
	cfg.CurrentProjectID = "other-1"

	app := &App{ctx: context.Background(), config: cfg, keychain: store}

	// The registry entry as openCloudConn registered it: its OWN renewal
	// loop.
	entry := &activeConn{id: "conn-1", kind: ProjectKindCloud, serverURL: cloudServerURL(c.url), jar: jar}
	oldCtx, oldStop := context.WithCancel(app.ctx)
	loopCtx, loopStop := context.WithCancel(oldCtx)
	entry.cancel = loopStop
	go app.runCloudRefreshLoop(loopCtx, jar)
	app.mu.Lock()
	app.registerConnLocked(entry)
	app.mu.Unlock()
	t.Cleanup(oldStop)

	// The LoginCloud shape on the non-current connection: the machine
	// resolves the pane's OWN jar, re-seeds it, and installs it.
	resolved := resolveConnectionJar(app.cloudJarFor("conn-1"), "conn-1", c.url, store)
	if resolved != jar {
		t.Fatalf("the sign-in resolved a second jar for the connection (%p != %p)", resolved, jar)
	}
	lres, err := c.svc.Login(context.Background(), "desk@example.org", "password-forever-9", "desktop", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.seed(cloudAuthResult{AccessToken: lres.AccessToken, AccessExpiry: lres.AccessExpires, RefreshToken: lres.RefreshToken}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.installSignInJarLocked(resolved, c.url)
	app.mu.Unlock()

	// The entry keeps exactly one renewal loop: the one it ran is stopped,
	// the fresh one rides on the entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if loopCtx.Err() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loopCtx.Err() == nil {
		t.Fatal("the entry's previous renewal loop still runs: the install started a second loop and left the first untracked")
	}
	app.mu.RLock()
	cancel := app.conns["conn-1"].cancel
	app.mu.RUnlock()
	if cancel == nil {
		t.Fatal("the install started no renewal loop for the non-current connection")
	}

	// CONTROL: the same install with the connection CURRENT stops the
	// entry's loop and the entry borrows the active one — exactly one loop.
	c2 := newRealCloud(t)
	jar2, store2, _ := c2.seedJar(t)
	cfg2 := NewConfig()
	cfg2.RecentProjects = append(cfg2.RecentProjects, Project{ID: "conn-1", Kind: ProjectKindCloud, CloudURL: c2.url, CloudUserID: "u1", Name: "conn-1"})
	cfg2.CurrentProjectID = "conn-1"
	app2 := &App{ctx: context.Background(), config: cfg2, keychain: store2}
	entry2 := &activeConn{id: "conn-1", kind: ProjectKindCloud, serverURL: cloudServerURL(c2.url), jar: jar2}
	ctx2, cancel2 := context.WithCancel(app2.ctx)
	entry2.cancel = cancel2
	go app2.runCloudRefreshLoop(ctx2, jar2)
	t.Cleanup(cancel2)
	app2.mu.Lock()
	app2.registerConnLocked(entry2)
	app2.installSignInJarLocked(jar2, c2.url)
	app2.mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctx2.Err() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ctx2.Err() == nil {
		t.Fatal("control: the isCurrent install did not stop the entry's own loop")
	}
	app2.mu.RLock()
	borrowed := app2.conns["conn-1"].cancel == nil && app2.cloudCancel != nil
	app2.mu.RUnlock()
	if !borrowed {
		t.Fatal("control: the entry does not borrow the active loop after the isCurrent install")
	}
}

// TestASwitchToALocalProjectReArmsTheOpenPaneOwnLoop (one renewal loop per
// connection): the current connection's pane entry borrows the active loop,
// so the teardown of a switch to a LOCAL project — which stops that loop and
// drops the active jar — hands the still-open pane's entry a loop of its own
// when its jar holds something to renew or present. The entry's own loop is
// then the presenter of the sign-out the pane owes, with no active jar or
// loop left behind.
func TestASwitchToALocalProjectReArmsTheOpenPaneOwnLoop(t *testing.T) {
	ctx := context.Background()
	c := newRealCloud(t)
	jar, store, _ := c.seedJar(t)

	cfg := NewConfig()
	conn := cfg.AddCloudConnection(c.url, "u1", "desk@example.org", "")
	local := cfg.AddCloudConnection("/tmp/somewhere", "local", "", "")
	cfg.SetCurrentProject(conn.ID)
	app := &App{ctx: ctx, config: cfg, keychain: store}

	// The pane of the CURRENT connection: the entry borrows the active loop
	// (openCloudConn's condition, cancel nil).
	app.mu.Lock()
	app.setActiveCloudLocked(jar, c.url)
	app.startCloudRefreshLoopLocked(jar)
	app.registerConnLocked(&activeConn{id: conn.ID, kind: ProjectKindCloud, serverURL: cloudServerURL(c.url), jar: jar})
	app.mu.Unlock()

	// The pane owes a sign-out the active loop's first presentation cannot
	// confirm; the mark stands in memory and on the entry (the keychain
	// answers).
	c.sessions.failNextRevocations(1 << 20)
	if code, _ := paneLogout(t, jar, http.DefaultTransport, c.url); code != http.StatusInternalServerError {
		t.Fatalf("the pane's sign-out was answered %d, want 500", code)
	}
	if !jar.SignOutPending() {
		t.Fatal("precondition: the jar must hold the sign-out")
	}
	// The active loop consumes the wake the mark asked for and fails its own
	// presentation; wait for that pass, so the switch below leaves no stale
	// wake and no in-flight presentation behind the loop it cancels.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.sessions.failRevokes.Load() < 1<<20-1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.sessions.failRevokes.Load() >= 1<<20-1 {
		t.Fatal("precondition: the active loop never presented the sign-out the mark asked for")
	}

	// The switch to the local project, as SwitchProject's restart does.
	cfg.SetCurrentProject(local.ID)
	app.deactivateCloud()

	// The open pane's entry runs a loop of its own again.
	app.mu.RLock()
	entry := app.conns[conn.ID]
	noActive := app.cloudCancel == nil && app.cloudJar == nil
	app.mu.RUnlock()
	if entry == nil {
		t.Fatal("the switch closed the pane's registry entry")
	}
	if entry.cancel == nil {
		t.Fatal("the switch left the open pane without a renewal loop and without a presenter")
	}
	if !noActive {
		t.Fatal("the switch left an active loop or jar behind")
	}

	// The entry's own loop presents the sign-out once the cloud answers.
	c.sessions.failNextRevocations(0)
	jar.kickRefresh()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !jar.SignOutPending() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if jar.SignOutPending() {
		t.Fatal("the entry's own loop did not present the sign-out the pane owes")
	}
	if got := store.m[cloudRefreshKeyPrefix+conn.ID]; got != "" {
		t.Fatalf("the confirmed sign-out left the keychain entry %q behind", got)
	}
}
