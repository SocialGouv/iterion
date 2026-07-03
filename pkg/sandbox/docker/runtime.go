// Package docker implements the Docker/Podman sandbox driver.
//
// The driver shells out to the `docker` or `podman` CLI rather than
// embedding a Go SDK — see .plans/on-va-tudier-la-snappy-lemon.md §1a
// for the rationale (binary size, dual-runtime support, surface
// stability). The CLI surface used here is the subset both runtimes
// implement compatibly: `docker run`, `docker create`, `docker start`,
// `docker exec`, `docker stop`, `docker rm`, `docker image inspect`,
// `docker pull`.
//
// The driver is opt-in: workflows that don't activate a sandbox never
// instantiate it, and `iterion sandbox doctor` reports unavailability
// gracefully so users on hosts without Docker can still run iterion
// in noop mode.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// Runtime identifies which container CLI we shell out to.
type Runtime string

const (
	// RuntimeDocker uses the `docker` CLI (Docker Engine, Docker
	// Desktop, Colima, OrbStack — all expose a docker-compatible CLI).
	RuntimeDocker Runtime = "docker"

	// RuntimePodman uses the `podman` CLI. The subset of commands we
	// invoke here is bug-compatible with docker, so callers don't need
	// runtime-specific code paths.
	RuntimePodman Runtime = "podman"
)

// Detect probes the host for an available container runtime, preferring
// docker over podman (matching the convention of most local dev tools).
// Returns ("", error) when neither binary is on PATH.
func Detect() (Runtime, error) {
	if _, err := exec.LookPath(string(RuntimeDocker)); err == nil {
		return RuntimeDocker, nil
	}
	if _, err := exec.LookPath(string(RuntimePodman)); err == nil {
		return RuntimePodman, nil
	}
	return "", fmt.Errorf("docker: neither 'docker' nor 'podman' found on PATH")
}

// runtimeCmd wraps exec.Command(<runtime>, args...) with the env
// scrubbing iterion uses on git invocations: LC_ALL=C / LANG=C so
// stderr substrings ("No such image", "container not found") are
// stable across user locales.
//
// Detached process group on Unix so a SIGTERM to the iterion studio
// PGID (e.g. watchexec rebuild) doesn't propagate and kill the
// container management commands mid-flight.
func runtimeCmd(rt Runtime, args ...string) *exec.Cmd {
	cmd := exec.Command(string(rt), args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	proc.DetachProcessGroup(cmd)
	return cmd
}

// runtimeCmdContext is the ctx-aware sibling of [runtimeCmd] — used
// for long-running operations (pull, run -d) that should respect run
// cancellation.
func runtimeCmdContext(ctx context.Context, rt Runtime, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, string(rt), args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	proc.DetachProcessGroup(cmd)
	return cmd
}

// imageExists reports whether a container image is already present in
// the local image store. It is a fast path that lets the driver skip
// `pull` when the user's image is already cached.
//
// `image inspect` exits 0 when present, non-zero otherwise; we don't
// parse the JSON, only the exit code, so missing image tags read as
// "not present" without ambiguity.
func imageExists(ctx context.Context, rt Runtime, ref string) bool {
	cmd := runtimeCmdContext(ctx, rt, "image", "inspect", ref)
	// Discard output — we only care about exit status.
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// defaultPullTimeout caps `<runtime> pull` so a stalled registry or
// blocked DNS in the sandbox network can't pend the run indefinitely.
// 10 min is enough for cold pulls of large CUDA / language-runtime
// images on slow links; operators can raise it via
// ITERION_SANDBOX_PULL_TIMEOUT (Go duration syntax, e.g. "20m").
const defaultPullTimeout = 10 * time.Minute

// pullTimeout reads ITERION_SANDBOX_PULL_TIMEOUT and falls back to the
// default when the env var is unset, empty, unparseable, or non-positive.
func pullTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("ITERION_SANDBOX_PULL_TIMEOUT"))
	if v == "" {
		return defaultPullTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultPullTimeout
	}
	return d
}

// pullImage runs `<runtime> pull <ref>` and surfaces the runtime's
// error output verbatim on failure. Bounded by pullTimeout (or the
// caller's ctx, whichever fires first) so a hung registry doesn't
// strand the run; a deadline overshoot is reported as a distinct
// "timeout" so the operator can lengthen ITERION_SANDBOX_PULL_TIMEOUT
// without chasing a generic exec failure.
func pullImage(ctx context.Context, rt Runtime, ref string) error {
	timeout := pullTimeout()
	pullCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := runtimeCmdContext(pullCtx, rt, "pull", ref)
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // Docker writes pull progress to stdout; capture both.
	err := cmd.Run()
	if err == nil {
		return nil
	}
	// pullCtx.Err() is the authoritative source for "we tripped the
	// deadline" — cmd.Run() may surface "signal: killed" depending on
	// runtime, so derive the error category from the ctx instead.
	if pullCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("docker: pull %s: timed out after %s (set ITERION_SANDBOX_PULL_TIMEOUT to extend)\noutput:\n%s", ref, timeout, stderr.String())
	}
	return fmt.Errorf("docker: pull %s: %w\noutput:\n%s", ref, err, stderr.String())
}
