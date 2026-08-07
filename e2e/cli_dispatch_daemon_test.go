package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// `iterion dispatch <config.yaml>` is the long-running daemon: it loads the
// YAML, opens the native tracker store, starts the dispatcher actor, serves
// the operator REST surface, and runs until SIGINT/SIGTERM. Every piece of
// that boot chain is observable from the outside — the health endpoint, the
// server-info advertisement the SPA gates on, the manager's own status and
// the config it is actually running, the board routes — and so is the
// shutdown: a signal must return cleanly (exit 0) while a bind failure must
// NOT (a supervisor keyed on the exit code would otherwise never restart a
// daemon whose API never came up).
//
// Mutation check: skip mgr.Start() and /status stops reporting running; drop
// SaveConfig and /config no longer echoes this YAML; mount the routes on the
// wrong prefix and the calls 404; swallow the httpErrCh send and the
// bind-failure case returns nil.

// freePort reserves and releases a loopback port, returning its number.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// writeDispatchConfig lays down a minimal native-tracker dispatcher YAML and
// returns its path.
func writeDispatchConfig(t *testing.T, dir string) string {
	t.Helper()
	wf, err := filepath.Abs(filepath.Join("testdata", "preset_mini.bot"))
	if err != nil {
		t.Fatalf("abs workflow: %v", err)
	}
	path := filepath.Join(dir, "iterion.dispatcher.yaml")
	body := fmt.Sprintf("name: e2e-dispatch\nworkflow: %q\ntracker:\n  kind: native\npolling:\n  interval_ms: 60000\n", wf)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// getJSON issues a GET against the daemon and decodes the JSON body.
func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // loopback URL built by the test
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s (%q): %v", url, string(body), err)
		}
	}
	return resp.StatusCode, out
}

// waitHealthy polls /healthz until the daemon answers or the deadline passes.
// A poll — not a fixed sleep — so the test is bound by readiness, not by a
// guessed startup time.
func waitHealthy(t *testing.T, base string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("daemon exited before serving: %v", err)
		default:
		}
		resp, err := http.Get(base + "/healthz") //nolint:gosec // loopback URL built by the test
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == "ok" {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("daemon never became healthy on /healthz")
}

func TestDispatchDaemonBootsServesAndStopsOnSignal(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDispatchConfig(t, dir)
	storeDir := filepath.Join(dir, "store")
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	done := make(chan error, 1)
	go func() {
		done <- cli.RunDispatch(&cli.Printer{W: io.Discard, Format: cli.OutputJSON}, cli.DispatchOptions{
			ConfigPath: cfgPath,
			StoreDir:   storeDir,
			Port:       port,
		})
	}()
	waitHealthy(t, base, done)

	// The SPA gates its /board and /dispatcher routes on this advertisement.
	code, info := getJSON(t, base+"/api/server/info")
	if code != http.StatusOK {
		t.Fatalf("GET /api/server/info = %d, want 200", code)
	}
	if info["mode"] != "dispatch" {
		t.Errorf("server info mode = %v, want dispatch", info["mode"])
	}
	if info["dispatcher_enabled"] != true || info["native_tracker_enabled"] != true {
		t.Errorf("server info = %v, want both dispatcher and native tracker advertised", info)
	}

	// The actor is not merely constructed — it was started.
	code, status := getJSON(t, base+"/api/v1/dispatcher/status")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/dispatcher/status = %d, want 200", code)
	}
	if status["state"] != "running" {
		t.Errorf("dispatcher state = %v, want running", status["state"])
	}
	if status["has_config"] != true {
		t.Errorf("dispatcher status = %v, want has_config", status)
	}

	// …and it is running THIS invocation's YAML, not a stale persisted one.
	code, cfg := getJSON(t, base+"/api/v1/dispatcher/config")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/dispatcher/config = %d, want 200", code)
	}
	if cfg["name"] != "e2e-dispatch" {
		t.Errorf("served config name = %v, want the YAML's name — the file never reached the manager", cfg["name"])
	}
	if got, ok := cfg["workflow"].(string); !ok || filepath.Base(got) != "preset_mini.bot" {
		t.Errorf("served config workflow = %v, want the YAML's workflow", cfg["workflow"])
	}

	// The native board REST surface is mounted alongside (the /board SPA route).
	code, _ = getJSON(t, base+"/api/v1/native/board")
	if code != http.StatusOK {
		t.Errorf("GET /api/v1/native/board = %d, want 200 — the native tracker routes are not mounted", code)
	}

	// The store is real: the dispatcher's own tree is on disk under --store-dir.
	if _, err := os.Stat(filepath.Join(storeDir, "dispatcher", "board.json")); err != nil {
		t.Errorf("native board.json not written under the store dir: %v", err)
	}

	// Shutdown: the daemon's documented stop is a signal, and a signal-driven
	// stop is a CLEAN exit.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunDispatch returned %v on SIGTERM, want a clean exit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunDispatch did not return within 30s of SIGTERM")
	}

	if _, err := http.Get(base + "/healthz"); err == nil { //nolint:gosec // loopback URL built by the test
		t.Error("the HTTP surface still answers after shutdown")
	}
}

func TestDispatchDaemonFailsLoudlyOnABusyPort(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDispatchConfig(t, dir)

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	port := blocker.Addr().(*net.TCPAddr).Port

	done := make(chan error, 1)
	go func() {
		done <- cli.RunDispatch(&cli.Printer{W: io.Discard, Format: cli.OutputJSON}, cli.DispatchOptions{
			ConfigPath: cfgPath,
			StoreDir:   filepath.Join(dir, "store"),
			Port:       port,
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunDispatch returned nil on a bind failure: a supervisor would read exit 0 and never restart the daemon")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunDispatch did not return after failing to bind")
	}
}

func TestDispatchDaemonRejectsAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	// A tracker kind with no workflow: the daemon must refuse to boot rather
	// than come up half-configured.
	if err := os.WriteFile(path, []byte("name: broken\ntracker:\n  kind: native\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := cli.RunDispatch(&cli.Printer{W: io.Discard, Format: cli.OutputJSON}, cli.DispatchOptions{
		ConfigPath: path,
		StoreDir:   filepath.Join(dir, "store"),
		NoServer:   true,
	})
	if err == nil {
		t.Fatal("RunDispatch booted on a config with no workflow")
	}
}
