package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestDispatchDaemonRefusesCrossOriginWrites pins the browser-facing guard on
// the dispatcher daemon's own mux.
//
// It builds that mux by hand rather than through Server.routes(), so it used
// to reach none of the studio's protections: a page the operator had open
// could POST a board card cross-origin, transition it into the
// dispatcher-eligible state and force the poll — which launches a workflow,
// with tools, on the host. Loopback-bound is not a defence when the browser is
// on the host. Found by adversarial review, which drove the whole chain live.
//
// Content-Type text/plain makes it a CORS "simple request": no preflight, and
// the JSON decoders never inspect Content-Type.
func TestDispatchDaemonRefusesCrossOriginWrites(t *testing.T) {
	// Deliberately NOT t.Parallel(). TestDispatchDaemonBootsServesAndStopsOnSignal
	// exercises the daemon's SIGTERM path by signalling the TEST PROCESS
	// (syscall.Kill(syscall.Getpid(), …)) — the daemon-SIGTERM aliasing this
	// file's header already warns about. A parallel daemon here catches that
	// signal too and exits cleanly mid-test, which reads as
	// "daemon exited before serving: <nil>". Running in the sequential phase
	// means this finishes before the parallel batch resumes.
	dir := t.TempDir()
	cfgPath := writeDispatchConfig(t, dir)
	port := reserveLoopbackPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	done := make(chan error, 1)
	// A CLOSED channel, not a value on `done`: waitHealthy receives from
	// `done` on its own failure path, and `done` is written exactly once, so
	// a "has it exited?" check against it reads empty forever afterwards.
	// `exited` stays readable once closed, whoever drained what.
	exited := make(chan struct{})
	go func() {
		done <- cli.RunDispatch(&cli.Printer{W: io.Discard, Format: cli.OutputJSON}, cli.DispatchOptions{
			ConfigPath: cfgPath,
			StoreDir:   filepath.Join(dir, "store"),
			Port:       port,
		})
		close(exited)
	}()
	// Registered BEFORE the first assertion, not after the last one. Every
	// t.Fatalf between here and the end — waitHealthy timing out, the post
	// helper hitting a transport error under CI load — would otherwise skip
	// the stop and re-leak the daemon, and it would do so exactly on runs
	// that are already failing, where the descriptor cascade then buries the
	// original failure under every later test in the package.
	t.Cleanup(func() {
		select {
		case <-exited:
			// Already returned — it never started (bind race, bad config), or
			// a sibling's signal reached it. There is nothing to stop, and
			// signalling now would be worse than doing nothing: RunDispatch
			// registers its handler with signal.NotifyContext + defer cancel,
			// so once it returns nothing catches SIGTERM and the default
			// disposition kills the whole test binary — turning one readable
			// failure into "signal: terminated" for the entire package.
			return
		default:
		}
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("signal daemon: %v", err)
			return
		}
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Error("RunDispatch did not return within 30s of SIGTERM — the daemon is still holding its watches")
		}
	})
	waitHealthy(t, base, done)

	post := func(path, origin, body string) int {
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}

	for _, path := range []string{"/api/v1/native/issues", "/api/v1/dispatcher/refresh"} {
		if got := post(path, "https://evil.example", `{"title":"csrf"}`); got != http.StatusForbidden {
			t.Errorf("POST %s cross-origin = %d; want 403 — a drive-by page can drive the dispatcher", path, got)
		}
	}

	// The board UI is served from this same origin and must keep working, as
	// must a non-browser caller (curl, a script) that sends no Origin at all.
	if got := post("/api/v1/native/issues", base, `{"title":"same-origin"}`); got == http.StatusForbidden {
		t.Error("same-origin POST was refused; the board UI would be broken")
	}
	if got := post("/api/v1/native/issues", "", `{"title":"no-origin"}`); got == http.StatusForbidden {
		t.Error("POST with no Origin was refused; that is the CLI/script caller")
	}

}

func TestDispatchDaemonBootsServesAndStopsOnSignal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := writeDispatchConfig(t, dir)
	storeDir := filepath.Join(dir, "store")
	port := reserveLoopbackPort(t)
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
	t.Parallel()
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
	t.Parallel()
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
