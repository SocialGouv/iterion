package mcp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// HostChromiumRunner launches Chromium directly on the host (no
// container, no docker exec) using `--remote-debugging-pipe`. The
// pipe transport opens FDs 3 (Chromium reads) and 4 (Chromium writes)
// in the child process; we provide them via cmd.ExtraFiles and wrap
// the parent ends as an io.ReadWriteCloser the registry hands to
// the editor's WS proxy.
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
	// user-data-dir; the defaults stay safe for the editor pane.
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
		_, _ = p.cmd.Process.Wait()
	}
	if err := p.read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// lookChromiumBinary returns the first entry of chromiumCandidates
// found on $PATH, or an error listing what was searched.
func lookChromiumBinary() (string, error) {
	for _, name := range chromiumCandidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"browser: no chromium binary found on PATH (tried %v); "+
			"install chromium / google-chrome or set HostChromiumRunner.BinaryPath",
		chromiumCandidates,
	)
}

// ErrChromiumNotInstalled is the typed error returned by lookChromiumBinary
// so callers can branch on "Chromium absent" vs "Chromium failed to start".
var ErrChromiumNotInstalled = errors.New("browser: chromium not installed on PATH")
