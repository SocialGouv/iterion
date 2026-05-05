//go:build desktop

package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

type recordingServer struct {
	mu     sync.Mutex
	starts []serverStart
	stops  int
	// addrs is the queue of host:port strings the server returns from
	// successive Start calls. We deliberately rotate values so the test can
	// catch any caller that latched the first addr — production
	// net.Listen('127.0.0.1:0') picks a fresh port on every restart, so the
	// fake mirrors that.
	addrs   []string
	addrIdx int
}

type serverStart struct {
	dir          string
	storeDir     string
	sessionToken string
}

func (s *recordingServer) Start(_ context.Context, dir, storeDir, sessionToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, serverStart{dir: dir, storeDir: storeDir, sessionToken: sessionToken})
	if len(s.addrs) == 0 {
		return "127.0.0.1:12345", nil
	}
	addr := s.addrs[s.addrIdx%len(s.addrs)]
	s.addrIdx++
	return addr, nil
}

func (s *recordingServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
}

// withStubWindowReloader swaps the package-level windowReloader for the
// duration of a test. Returns a counter the test can read after the
// operation under test completes.
func withStubWindowReloader(t *testing.T) *int {
	t.Helper()
	var reloads int
	prev := windowReloader
	windowReloader = func(_ context.Context) { reloads++ }
	t.Cleanup(func() { windowReloader = prev })
	return &reloads
}

func TestAddProjectSilentlyRestartsServerForSelectedDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := NewConfig()
	cfg.path = cfgPath
	srv := &recordingServer{addrs: []string{"127.0.0.1:50001", "127.0.0.1:50002"}}
	reloads := withStubWindowReloader(t)
	app := &App{
		ctx:          context.Background(),
		config:       cfg,
		server:       srv,
		sessionToken: "test-token",
	}

	p, err := app.AddProjectSilently(dir)
	if err != nil {
		t.Fatalf("AddProjectSilently: %v", err)
	}
	if p == nil {
		t.Fatalf("AddProjectSilently returned nil project")
	}
	if p.Dir != dir {
		t.Fatalf("project dir = %q, want %q", p.Dir, dir)
	}
	if got := len(srv.starts); got != 1 {
		t.Fatalf("server starts = %d, want 1", got)
	}
	if srv.starts[0].dir != dir {
		t.Fatalf("server started with dir = %q, want %q", srv.starts[0].dir, dir)
	}
	if srv.starts[0].sessionToken != "test-token" {
		t.Fatalf("server token = %q, want test-token", srv.starts[0].sessionToken)
	}
	if srv.stops != 1 {
		t.Fatalf("server stops = %d, want 1", srv.stops)
	}
	if app.serverURL != "http://127.0.0.1:50001/" {
		t.Fatalf("serverURL = %q, want http://127.0.0.1:50001/", app.serverURL)
	}
	if cfg.CurrentProjectID != p.ID {
		t.Fatalf("CurrentProjectID = %q, want %q", cfg.CurrentProjectID, p.ID)
	}
	if *reloads != 1 {
		t.Fatalf("WindowReloadApp invocations = %d, want 1 (silent onboarding still needs to drive re-bootstrap)", *reloads)
	}
}

// TestRestartServerRebindsToNewPortAndDrivesReBootstrap pins the bug
// caught by the reviewer: production net.Listen('127.0.0.1:0') binds a fresh
// port each time, so the SPA — loaded from the OLD port — must be navigated
// back through the Wails AssetServer stub or it talks to a dead listener.
// The test rotates fake addrs to force a port change between successive
// restarts, then asserts:
//
//  1. a.serverURL is updated to the NEW addr (caller would otherwise hand a
//     stale URL to the Wails binding GetServerURL).
//  2. windowReloader fires exactly once per restart (so SwitchProject and
//     AddProjectSilently both drive the re-bootstrap that handles the port
//     change).
//
// We exercise restartServerForCurrentProject directly — calling SwitchProject
// would also invoke wruntime.EventsEmit, which requires a live Wails context
// the in-package test runner doesn't provide. The behaviour we're locking in
// (port-following + WindowReloadApp) lives in restartServerForCurrentProject
// itself; SwitchProject is a thin wrapper that funnels through it.
func TestRestartServerRebindsToNewPortAndDrivesReBootstrap(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := NewConfig()
	cfg.path = cfgPath
	// Three distinct addrs: one for the initial onboarding restart, one for a
	// second project's restart, one for the explicit switch-back restart.
	// Mirrors what production net.Listen('127.0.0.1:0') would produce.
	srv := &recordingServer{addrs: []string{
		"127.0.0.1:50001",
		"127.0.0.1:50002",
		"127.0.0.1:50003",
	}}
	reloads := withStubWindowReloader(t)
	app := &App{
		ctx:          context.Background(),
		config:       cfg,
		server:       srv,
		sessionToken: "test-token",
	}

	// Project 1 onboarded silently.
	p1, err := app.addProject(dir1)
	if err != nil {
		t.Fatalf("addProject dir1: %v", err)
	}
	if _, err := app.restartServerForCurrentProject(app.ctx); err != nil {
		t.Fatalf("restartServerForCurrentProject 1: %v", err)
	}
	if app.serverURL != "http://127.0.0.1:50001/" {
		t.Fatalf("serverURL after first restart = %q, want http://127.0.0.1:50001/", app.serverURL)
	}

	// Project 2 added on top — port rolls forward.
	if _, err := app.addProject(dir2); err != nil {
		t.Fatalf("addProject dir2: %v", err)
	}
	if _, err := app.restartServerForCurrentProject(app.ctx); err != nil {
		t.Fatalf("restartServerForCurrentProject 2: %v", err)
	}
	if app.serverURL != "http://127.0.0.1:50002/" {
		t.Fatalf("serverURL after second restart = %q, want http://127.0.0.1:50002/", app.serverURL)
	}

	// Switch-back: SetCurrentProject + restart, no SwitchProject wrapper so we
	// avoid Wails EventsEmit.
	app.mu.Lock()
	if !cfg.SetCurrentProject(p1.ID) {
		app.mu.Unlock()
		t.Fatalf("SetCurrentProject failed for %q", p1.ID)
	}
	app.mu.Unlock()
	if _, err := app.restartServerForCurrentProject(app.ctx); err != nil {
		t.Fatalf("restartServerForCurrentProject 3: %v", err)
	}
	if app.serverURL != "http://127.0.0.1:50003/" {
		t.Fatalf("serverURL after switch-back restart = %q, want http://127.0.0.1:50003/", app.serverURL)
	}
	if cfg.CurrentProjectID != p1.ID {
		t.Fatalf("CurrentProjectID after switch = %q, want %q", cfg.CurrentProjectID, p1.ID)
	}

	// Sanity: starts/stops count up consistently across all three restarts.
	if got := len(srv.starts); got != 3 {
		t.Fatalf("server starts = %d, want 3", got)
	}
	if srv.stops != 3 {
		t.Fatalf("server stops = %d, want 3", srv.stops)
	}
	if *reloads != 3 {
		t.Fatalf("WindowReloadApp invocations = %d, want 3 (one per server restart)", *reloads)
	}
}

// TestServerHostStartReturnsDistinctPortsAcrossRestarts is the lower-level
// proof that production behaviour mirrors the test's rotated fake: the
// underlying ServerHost wired to a real cli.RunEditor binds a new port on
// every Start, so any caller that doesn't follow up with WindowReloadApp
// would wedge the SPA. Skipped if we can't bind loopback (CI sandboxes).
func TestServerHostStartReturnsDistinctPortsAcrossRestarts(t *testing.T) {
	host := NewServerHost()
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".iterion")

	addrs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		addr, err := host.Start(context.Background(), dir, storeDir, "")
		if err != nil {
			t.Skipf("ServerHost.Start failed (likely sandbox restriction): %v", err)
		}
		addrs = append(addrs, addr)
		host.Stop()
	}

	if addrs[0] == addrs[1] {
		t.Fatalf("ServerHost rebound to same addr twice (%q) — random-port assumption broken; "+
			"if production really keeps the same port, the WindowReloadApp re-bootstrap is unnecessary "+
			"but harmless. Cross-check pkg/server/server.go ListenAndServe.", addrs[0])
	}
	t.Logf("ServerHost.Start addrs across restarts: %v", addrs)
}
