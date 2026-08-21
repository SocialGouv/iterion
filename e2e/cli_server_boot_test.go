package e2e

// This file covers `iterion server` and `iterion runner` — the two cloud-mode
// CLI entry points. Their runE functions live in cmd/iterion/ (unexported),
// so unlike cli.RunDispatch these commands can only be exercised through the
// built binary. Every test spawns exactly one subprocess and signals THAT
// subprocess directly, side-stepping the defect noted in
// cli_dispatch_daemon_test.go (`syscall.Kill(syscall.Getpid(), SIGTERM)`
// would signal the whole test binary).
//
// Assertions target OBSERVABLE invariants only: subprocess exit code,
// subprocess stderr, and (for the server) the HTTP surface's own response
// bodies — never a value the test itself fed the server.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// iterionBinaryOnce builds ./cmd/iterion exactly once per `go test` run.
// Rebuilding per test would triple wall time on a cold cache without buying
// isolation (the binary is a read-only artefact). The build directory is
// deliberately outside t.TempDir() so it survives across tests in the same
// process; a small /tmp leak is acceptable for an artefact that lives for
// the test-run duration.
var (
	iterionBinaryOnce sync.Once
	iterionBinaryPath string
	iterionBinaryErr  error
)

// iterionBinary returns the path to a freshly built ./cmd/iterion binary.
// Fails the test on build failure with the compiler output.
func iterionBinary(t *testing.T) string {
	t.Helper()
	iterionBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "iterion-cli-server-e2e-bin-*")
		if err != nil {
			iterionBinaryErr = fmt.Errorf("mkdir bin dir: %w", err)
			return
		}
		bin := filepath.Join(dir, "iterion")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/iterion")
		cmd.Dir = ".."
		// CGO off: matches the shipped binary and dodges the "no gcc found"
		// path on hosts without a C toolchain.
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			iterionBinaryErr = fmt.Errorf("go build ./cmd/iterion: %v\n%s", err, out)
			return
		}
		iterionBinaryPath = bin
	})
	if iterionBinaryErr != nil {
		t.Fatalf("build iterion: %v", iterionBinaryErr)
	}
	return iterionBinaryPath
}

// freeLoopbackPort reserves and immediately releases a loopback port so the
// subprocess can bind it. Between release and re-bind is a TOCTOU race in
// principle; on loopback with no other binder it is deterministic in
// practice.
func freeLoopbackPort(t *testing.T) int {
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

// cleanEnvForSubprocess strips ITERION_* and HOME from the host environment
// so operator settings (an ITERION_MODE=cloud in the operator's shell, a
// stray ~/.iterion) never leak into the subprocess. Callers append their
// own overrides on top.
func cleanEnvForSubprocess() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "ITERION_") {
			continue
		}
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// waitServerHealthy polls /healthz until the subprocess answers with the
// expected JSON envelope or the deadline passes. If the subprocess dies
// before answering, we fail with its stderr so the operator sees why.
// Polling — not a fixed sleep — so the test is bound by readiness, not by
// a guessed startup time.
func waitServerHealthy(t *testing.T, base string, exitCh <-chan error, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exitCh:
			t.Fatalf("subprocess exited before serving: %v\nstderr:\n%s", err, stderr.String())
		default:
		}
		resp, err := http.Get(base + "/healthz") //nolint:gosec // loopback URL built by the test
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var payload map[string]any
				if json.Unmarshal(body, &payload) == nil && payload["status"] == "ok" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("subprocess never became healthy on /healthz\nstderr:\n%s", stderr.String())
}

// lameDuckWindow is the ITERION_SHUTDOWN_DELAY the boot test runs with:
// long enough to poll the terminating server, short enough not to drag
// the suite.
const lameDuckWindow = 5 * time.Second

// assertLameDuck proves the graceful-shutdown contract on a server that
// has just been SIGTERM'd but has NOT exited yet: /readyz already says
// 503 "draining" (so the endpoints controller can pull the pod) while
// /healthz still says 200 (so the kubelet does not kill it mid-drain) and
// the listener still accepts connections.
//
// Without that window the listener closes while traffic is still routed
// here — a connection-refused, i.e. a 502 for a studio user and a dropped
// delivery for a forge webhook, on every rolling deploy and every HPA
// scale-down.
//
// Mutation coverage: drop the draining check in handleReadyz, or set
// ShutdownDelay to 0 in RunStudio → the poll never sees a 503 and this
// fails.
func assertLameDuck(t *testing.T, base string, exitCh <-chan error, stderr *bytes.Buffer) {
	t.Helper()

	deadline := time.Now().Add(lameDuckWindow)
	for time.Now().Before(deadline) {
		select {
		case err := <-exitCh:
			t.Fatalf("server exited during its lame-duck window (%v) — /readyz never got a chance to say draining\nstderr:\n%s", err, stderr.String())
		default:
		}
		resp, err := http.Get(base + "/readyz") //nolint:gosec // loopback URL built by the test
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			time.Sleep(20 * time.Millisecond) // drain not started yet
			continue
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("draining /readyz = %d, want 503; body: %s", resp.StatusCode, body)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode draining /readyz body %q: %v", body, err)
		}
		if payload["status"] != "draining" {
			t.Fatalf("draining /readyz status = %v, want draining", payload["status"])
		}
		// Liveness must stay green through the drain, on the same still-open
		// listener.
		liveResp, err := http.Get(base + "/healthz") //nolint:gosec // loopback URL built by the test
		if err != nil {
			t.Fatalf("GET /healthz during drain: %v", err)
		}
		liveBody, _ := io.ReadAll(liveResp.Body)
		_ = liveResp.Body.Close()
		if liveResp.StatusCode != http.StatusOK {
			t.Fatalf("draining /healthz = %d, want 200 (a liveness failure kills the pod mid-drain); body: %s", liveResp.StatusCode, liveBody)
		}
		return
	}
	t.Fatalf("/readyz never reported draining within the %s lame-duck window\nstderr:\n%s", lameDuckWindow, stderr.String())
}

// TestServerCommandBootsLocalModeAndShutsDownOnSignal exercises the local
// mode path of `iterion server` end to end: build the binary, spawn it
// against an isolated store dir, observe that /healthz + /api/server/info
// come up on the port we asked for, then SIGTERM the SUBPROCESS and confirm
// a clean exit while the HTTP surface stops answering.
//
// The subprocess (not the test binary) receives the SIGTERM — a deliberate
// departure from cli_dispatch_daemon_test.go which sends
// syscall.Kill(getpid(), SIGTERM), a signal to the whole `go test`
// process. That defect noted in the model file is not reproduced here.
//
// Mutation coverage:
//   - Drop `Port: serverOpts.port` in cmd/iterion/server.go → RunStudio
//     binds the default 4891 and the ephemeral-port /healthz poll times out.
//   - Flip the mode gate `cfg.Mode == iterconfig.ModeLocal` to `ModeCloud`
//     → the local run takes the cloud branch, NATS connect fails, /healthz
//     never comes up.
//   - Unregister `s.mux.HandleFunc("GET /healthz", …)` in
//     pkg/server/server_routes.go → the /healthz poll returns 404 and
//     times out.
func TestServerCommandBootsLocalModeAndShutsDownOnSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM handling differs on windows")
	}
	bin := iterionBinary(t)

	homeDir := t.TempDir()
	iterionHome := t.TempDir()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	port := freeLoopbackPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "server",
		"--port", fmt.Sprintf("%d", port),
		"--bind", "127.0.0.1", // override the server default of 0.0.0.0
		"--dir", workDir,
		"--store-dir", storeDir,
	)
	// Detach into its own process group so a SIGINT to the parent test
	// process (Ctrl-C) doesn't cascade and eat our own SIGTERM assertion.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = workDir // avoid the repo-root .env walk-up
	cmd.Env = append(cleanEnvForSubprocess(),
		"HOME="+homeDir,
		"ITERION_HOME="+iterionHome,
		"ITERION_MODE=local",
		// Lame-duck window, wide enough for the assertions below to
		// observe it. In a cluster this is what gives the endpoints
		// controller time to pull the pod out of the Service before its
		// listener closes.
		"ITERION_SHUTDOWN_DELAY="+lameDuckWindow.String(),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start iterion server: %v", err)
	}
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	// Failsafe: if the test panics or times out mid-way, kill the
	// subprocess so it doesn't leak an open port and a running server.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
		cancel()
	})

	waitServerHealthy(t, base, exitCh, &stderr)

	// /healthz JSON shape: proves we've reached pkg/server.Server's handler
	// (dispatch's own /healthz returns plain "ok" and would fail the decode
	// below). "mode":"local" proves the runServer mode gate routed to
	// RunStudio, not the cloud branch that would have hung on NATS.
	resp, err := http.Get(base + "/healthz") //nolint:gosec // loopback URL built by the test
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200; body: %s", resp.StatusCode, body)
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode /healthz body %q: %v", body, err)
	}
	if health["status"] != "ok" {
		t.Errorf("/healthz status = %v, want ok", health["status"])
	}
	if health["mode"] != "local" {
		t.Errorf("/healthz mode = %v, want local (server took the wrong config branch)", health["mode"])
	}

	// /api/server/info: local mode must advertise auth_required=false — the
	// SPA depends on it to skip the sign-in flow. This also confirms the
	// unauthenticated GET path is reachable (middleware allowlist).
	resp, err = http.Get(base + "/api/server/info") //nolint:gosec // loopback URL built by the test
	if err != nil {
		t.Fatalf("GET /api/server/info: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/server/info = %d, want 200; body: %s", resp.StatusCode, body)
	}
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode /api/server/info body %q: %v", body, err)
	}
	if info["auth_required"] == true {
		t.Errorf("/api/server/info auth_required=true in local mode, want false")
	}
	if info["mode"] != "local" {
		t.Errorf("/api/server/info mode = %v, want local", info["mode"])
	}

	// Store dir is real: RunStudio eagerly initialises the dispatcher-backing
	// native store at <store-dir>/dispatcher/. If --store-dir was ignored
	// this directory would be missing (and the store would land in
	// ITERION_HOME instead).
	if _, err := os.Stat(filepath.Join(storeDir, "dispatcher")); err != nil {
		t.Errorf("store dir contents missing: %v", err)
	}

	// Shutdown: signal the SUBPROCESS. RunStudio documents a 60s drain
	// budget; give the test 75s so the assertion outlives the drain.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal iterion server: %v", err)
	}

	assertLameDuck(t, base, exitCh, &stderr)

	select {
	case err := <-exitCh:
		if err != nil {
			t.Fatalf("iterion server exited with %v on SIGTERM, want clean exit\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(75 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGKILL)
		t.Fatal("iterion server did not exit within 75s of SIGTERM")
	}

	// Post-condition: the HTTP surface no longer responds. Give the OS a
	// moment to release the socket before probing.
	postShutdownDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(postShutdownDeadline) {
		if _, err := http.Get(base + "/healthz"); err != nil { //nolint:gosec // loopback URL built by the test
			return // gone — clean shutdown
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("HTTP surface still answers after shutdown")
}

// TestServerCommandFailsLoudlyOnBusyPort proves `iterion server` surfaces a
// bind failure as a non-zero exit. A silent nil return would leave a k8s
// pod deemed "started" while its API never comes up — the supervisor keyed
// on exit code would never restart it.
//
// Mutation coverage: swallow ListenAndServe's error in RunStudio (or in
// runServer) so the process exits 0 → this assertion fires.
func TestServerCommandFailsLoudlyOnBusyPort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("port-conflict semantics differ on windows")
	}
	bin := iterionBinary(t)
	homeDir := t.TempDir()
	iterionHome := t.TempDir()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	port := blocker.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "server",
		"--port", fmt.Sprintf("%d", port),
		"--bind", "127.0.0.1",
		"--dir", workDir,
		"--store-dir", storeDir,
	)
	cmd.Dir = workDir
	cmd.Env = append(cleanEnvForSubprocess(),
		"HOME="+homeDir,
		"ITERION_HOME="+iterionHome,
		"ITERION_MODE=local",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	err = cmd.Run()
	if err == nil {
		t.Fatal("iterion server exit 0 on a bind failure — a supervisor would never restart it")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exec.ExitError, got %T (%v)\nstderr:\n%s", err, err, stderr.String())
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("exit code = 0, want non-zero; stderr:\n%s", stderr.String())
	}
}

// TestRunnerCommandRefusesLocalMode proves the runner's mode gate is
// deterministic and FAST: it errors before any NATS/Mongo/S3 dial, so a
// misconfigured deployment (ITERION_MODE unset or =local) fails at boot
// with a clear message instead of hanging on a network dial the operator
// then has to reason about.
//
// Mutation coverage: delete the `if cfg.Mode != iterconfig.ModeCloud`
// guard in cmd/iterion/runner.go → the subprocess proceeds to
// `natsq.Connect`, whose error text ("connect NATS") lacks the required
// substring below. The substring pin also protects the wording — if the
// error is refactored, this test flags that the operator-facing message
// changed.
func TestRunnerCommandRefusesLocalMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("subprocess semantics differ on windows")
	}
	bin := iterionBinary(t)
	homeDir := t.TempDir()
	iterionHome := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "runner")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cleanEnvForSubprocess(),
		"HOME="+homeDir,
		"ITERION_HOME="+iterionHome,
		"ITERION_MODE=local",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	err := cmd.Run()
	if err == nil {
		t.Fatal("iterion runner exit 0 in local mode: the mode gate is broken (or wired to a no-op)")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exec.ExitError, got %T (%v)\nstderr:\n%s", err, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ITERION_MODE must be 'cloud'") {
		t.Fatalf("runner stderr missing the mode-gate error — the gate is probably bypassed and the runner dialled NATS instead; got:\n%s", stderr.String())
	}
}

// TestRunnerCommandRefusesBrokenConfig proves the runner runs the config
// loader (and surfaces its failures) before any network I/O. A missing
// --config file must produce a "load config" error, not a downstream NATS
// timeout — because in production the failure the operator will actually
// hit is a typo'd --config path, and blaming NATS wastes their afternoon.
//
// The test sets ITERION_MODE=cloud so the mode gate does NOT short-circuit
// the load path: the FAILURE we want to catch is the loader's file-open
// error, not the mode check from TestRunnerCommandRefusesLocalMode.
//
// Mutation coverage: skip `iterconfig.Load(...)` entirely (or ignore its
// error) in runner.go → zero-valued cfg → the mode gate fires with
// `ITERION_MODE "" invalid` OR the runner charges into NATS. Either
// downstream error lacks the "load config" substring below.
func TestRunnerCommandRefusesBrokenConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary-spawning e2e in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("subprocess semantics differ on windows")
	}
	bin := iterionBinary(t)
	homeDir := t.TempDir()
	iterionHome := t.TempDir()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.yaml")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "runner", "--config", missing)
	cmd.Dir = dir
	cmd.Env = append(cleanEnvForSubprocess(),
		"HOME="+homeDir,
		"ITERION_HOME="+iterionHome,
		"ITERION_MODE=cloud", // pass the mode gate — the failure must be the loader
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	err := cmd.Run()
	if err == nil {
		t.Fatal("iterion runner exit 0 with a missing config file")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exec.ExitError, got %T (%v)\nstderr:\n%s", err, err, stderr.String())
	}
	// The runtime wraps loader failures as "runner: load config: config:
	// yaml: read <path>: ...". Assert on the outer "load config" prefix
	// only so the loader's internal wrapping can evolve without touching
	// this test.
	if !strings.Contains(stderr.String(), "load config") {
		t.Fatalf("runner stderr missing the load-config error — the loader is likely skipped; got:\n%s", stderr.String())
	}
}
