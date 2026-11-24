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

	"github.com/SocialGouv/iterion/internal/envtrust"
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

// CLI is a container runtime this host has: WHICH CLI it is — the identity
// the driver reports and every message names — and the ABSOLUTE path
// [Detect] resolved for it through envtrust.
//
// The two travel as one value because the probe's answer and the spawn's
// target must be the same binary. Detect used to return the enum alone, so
// every spawn passed the runtime's bare NAME to exec.CommandContext, which
// re-asks the live PATH at Command() time — the question Detect had just
// answered, and the one a project `.env` can answer instead where the seam
// is not consulted. Every spawn in this package now hangs off this value,
// so a new one cannot be written without a binary to run it with.
type CLI struct {
	rt   Runtime
	path string
}

// Runtime reports which container CLI this is, for the callers that branch
// on docker-vs-podman behaviour.
func (c CLI) Runtime() Runtime { return c.rt }

// String is the runtime's name — "docker" or "podman" — which is what every
// message about it says and what [Driver.Name] reports. Never the resolved
// path: that is an implementation detail of where this host keeps it.
func (c CLI) String() string { return string(c.rt) }

// Detect probes the host for an available container runtime, preferring
// docker over podman (matching the convention of most local dev tools).
// Returns the zero CLI and an error when neither binary is on PATH.
//
// envtrust.LookPath, not exec.LookPath: which runtime binary the sandbox
// spawns is an authority question, and a PATH a project .env planted must
// not answer it — the refusal surfaces as the plain "not found" below
// (#2876, the class #2809 closed site by site). The resolved path is kept,
// not discarded: the binary this probe accepted is the one every spawn
// below runs.
func Detect() (CLI, error) {
	for _, rt := range []Runtime{RuntimeDocker, RuntimePodman} {
		if path, err := envtrust.LookPath(string(rt)); err == nil {
			return CLI{rt: rt, path: path}, nil
		}
	}
	return CLI{}, fmt.Errorf("docker: neither 'docker' nor 'podman' found on PATH")
}

// command is the one place this CLI becomes a process: it names the binary
// [Detect] resolved and nothing else. What each caller layers on top — the
// locale scrub, the detached process group — stays the caller's own.
//
// A CLI nobody resolved is a programming error, not a bare-name spawn:
// [Detect] is the only constructor, and a spawn that silently fell back to
// the runtime's name would consult exactly the PATH this type exists to
// stop consulting. It is refused on the Cmd itself, so the caller reports
// it where it ran.
func (c CLI) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.path, args...)
	if c.path == "" {
		cmd.Err = fmt.Errorf("docker: no path resolved for runtime %q — build the invocation from Detect, never from the runtime's bare name", string(c.rt))
	}
	return cmd
}

// cmdContext is the management-command shape: the env scrubbing iterion
// uses on git invocations (LC_ALL=C / LANG=C, so stderr substrings like
// "No such image" and "container not found" are stable across user
// locales), and a detached process group on Unix so a SIGTERM to the
// iterion studio PGID (e.g. a watchexec rebuild) does not propagate and
// kill a container management command mid-flight.
func (c CLI) cmdContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := c.command(ctx, args...)
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
func (c CLI) imageExists(ctx context.Context, ref string) bool {
	cmd := c.cmdContext(ctx, "image", "inspect", ref)
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
func (c CLI) pullImage(ctx context.Context, ref string) error {
	timeout := pullTimeout()
	pullCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := c.cmdContext(pullCtx, "pull", ref)
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
