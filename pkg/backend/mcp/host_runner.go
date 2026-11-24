package mcp

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// HostChromiumRunner launches Chromium directly on the host (no
// container, no docker exec) using `--remote-debugging-pipe`. The
// pipe transport opens FDs 3 (Chromium reads) and 4 (Chromium writes)
// in the child process; we provide them via cmd.ExtraFiles and wrap
// the parent ends as an io.ReadWriteCloser the registry hands to
// the studio's WS proxy.
//
// Pipe framing: Chromium emits one CDP JSON-RPC message per write,
// followed by a single null byte (`\0`). The WS proxy is responsible
// for honouring the framing on the wire (one frame per WS message);
// this runner keeps the bytes as-is.
//
// Binary discovery: PATH-based, defaulting to `chromium` (Debian /
// most distros) and falling back to `chromium-browser` and
// `google-chrome` so the host runner works on dev machines without
// the operator hand-tuning a flag.
//
// This runner is the simplest cross-surface implementation —
// suitable for cloud (k8s pod with Chromium installed) and for
// `sandbox.mode=none` workflows on a dev host. The Docker exec
// runner for sandboxed runs is a follow-up: same interface, larger
// scope (volume mounts, signal forwarding, container lifecycle).
type HostChromiumRunner struct {
	// BinaryPath overrides the auto-discovered Chromium. Empty falls
	// back to the search list.
	BinaryPath string
	// ExtraArgs is appended to the default headless + pipe flags.
	// Operators can use it to bump --remote-allow-origins or pin a
	// user-data-dir; the defaults stay safe for the studio pane.
	ExtraArgs []string
}

// NewHostChromiumRunner returns a runner that launches Chromium on
// the host via os/exec.
func NewHostChromiumRunner() *HostChromiumRunner {
	return &HostChromiumRunner{}
}

// chromiumCandidates is the PATH lookup order. Distros + macOS:
//   - chromium               — Debian / Ubuntu
//   - chromium-browser       — older Ubuntu, Snap
//   - google-chrome          — Google's official package
//   - google-chrome-stable   — Chrome stable channel
var chromiumCandidates = []string{
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
}

// Start launches Chromium and returns the bidirectional CDP pipe.
// On success the caller MUST Close the returned ReadWriteCloser to
// shut Chromium down.
func (r *HostChromiumRunner) Start(runID, nodeID string) (io.ReadWriteCloser, error) {
	bin := r.BinaryPath
	if bin == "" {
		var lookErr error
		bin, lookErr = lookChromiumBinary()
		if lookErr != nil {
			return nil, lookErr
		}
	}

	// The two pipes Chromium will see at FD 3 (reads CDP requests
	// from us) and FD 4 (writes CDP responses + events to us).
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("browser: in pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, fmt.Errorf("browser: out pipe: %w", err)
	}

	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--remote-debugging-pipe",
	}
	args = append(args, r.ExtraArgs...)

	cmd := exec.Command(bin, args...)
	// The child receives a process-essential allowlist read as INHERITED
	// (chromiumChildEnv), never the launcher's whole environment: a nil
	// cmd.Env makes Go hand Chromium every variable of the launcher,
	// including the names a project .env planted — and a .env plants only
	// names that were UNSET, which is exactly where LD_PRELOAD,
	// LD_LIBRARY_PATH and CHROME_* live on a normal machine (#2809, the
	// site #2018 deliberately left: the allowlist a headless Chromium
	// needs cannot be proven in CI, so it is named here rather than
	// derived).
	cmd.Env = chromiumChildEnv()
	// FDs 3, 4 — order matters for --remote-debugging-pipe.
	cmd.ExtraFiles = []*os.File{inR, outW}
	// Don't surface Chromium's stderr to the parent's; redirect to
	// /dev/null. Operators who need to debug can override via
	// ExtraArgs (--enable-logging --v=1 + manual stderr hookup).
	cmd.Stderr = io.Discard
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("browser: start chromium: %w", err)
	}

	// Close the child's ends in the parent — only the parent should
	// hold inW (writes to Chromium) and outR (reads from Chromium).
	_ = inR.Close()
	_ = outW.Close()

	return &chromiumPipe{
		runID:  runID,
		nodeID: nodeID,
		read:   outR,
		write:  inW,
		cmd:    cmd,
	}, nil
}

// chromiumPipe is the io.ReadWriteCloser handed to the WS proxy.
// Read forwards from Chromium's FD 4; Write forwards to Chromium's
// FD 3; Close terminates the process and frees the pipes.
type chromiumPipe struct {
	runID  string
	nodeID string
	read   *os.File
	write  *os.File
	cmd    *exec.Cmd
}

func (p *chromiumPipe) Read(b []byte) (int, error)  { return p.read.Read(b) }
func (p *chromiumPipe) Write(b []byte) (int, error) { return p.write.Write(b) }

// Close terminates Chromium gracefully (best-effort SIGTERM, then
// closes the parent ends so any in-flight Read returns EOF). Calling
// twice is safe — file Close is idempotent and Process.Kill returns
// an "already exited" error we swallow.
func (p *chromiumPipe) Close() error {
	var firstErr error
	// Closing the write side signals Chromium that we're done.
	if err := p.write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		// cmd.Wait (NOT Process.Wait): Stdout/Stderr are io.Discard, so
		// os/exec allocated parent-side pipes + copy goroutines that only
		// cmd.Wait closes/reaps. Process.Wait reaps the process but leaks
		// those two fds until their finalizers run — one leak per browser
		// pane open/close.
		_ = p.cmd.Wait()
	}
	if err := p.read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// chromiumAmbientAllowlist is the only ambient environment a host Chromium
// keeps: what a headless browser needs to EXIST on the host, never what it
// loads or trusts — LD_PRELOAD, LD_LIBRARY_PATH and the CHROME_* family are
// the class this closes, and the `all` passthrough of the stdio transport
// has no business here (a browser pane is not a declared server). Unix
// names: the pipe transport (ExtraFiles at FDs 3, 4) is Unix-only already.
var chromiumAmbientAllowlist = []string{
	"PATH", "HOME", "TMPDIR",
	// The XDG dirs Chromium resolves its config, cache and runtime state
	// against, and the session bus a desktop session has it announce
	// itself on — absent on a headless host, where the browser runs
	// without them.
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
}

// chromiumChildEnv composes the environment of the host Chromium child:
// the allowlist above, each name read as INHERITED through the same
// fillInherited the stdio transport's allowlist is read through — a name a
// project .env planted does not reach the browser.
func chromiumChildEnv() []string {
	merged := make(map[string]string, len(chromiumAmbientAllowlist))
	fillInherited(merged, chromiumAmbientAllowlist)
	return renderEnv(merged)
}

// lookChromiumBinary returns the first entry of chromiumCandidates
// found on $PATH, or an error listing what was searched.
func lookChromiumBinary() (string, error) {
	for _, name := range chromiumCandidates {
		// envtrust.LookPath, not exec.LookPath: which browser the launcher
		// runs is an authority question, and a PATH a project .env planted
		// must not answer it.
		if path, err := envtrust.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"browser: no chromium binary found on PATH (tried %v); "+
			"install chromium / google-chrome or set HostChromiumRunner.BinaryPath",
		chromiumCandidates,
	)
}
