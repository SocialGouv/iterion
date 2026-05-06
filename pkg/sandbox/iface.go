// Package sandbox defines the iterion sandboxing abstraction.
//
// A sandbox isolates a coding agent (claude_code, codex, claw) and the
// shell commands it runs from the host iterion process. The package
// provides a driver-agnostic interface — concrete drivers (docker,
// kubernetes, noop) live in sub-packages and are selected at runtime by
// [Factory] based on ITERION_MODE and host capabilities.
//
// The sandbox is opt-in: workflows that don't declare a `sandbox:` block
// (and runs that don't pass --sandbox) get the noop driver, which is a
// passthrough — no container, no isolation, behaves exactly as if the
// package didn't exist. This guarantees zero regression for existing
// users.
//
// Three drivers are wired (each in its own phase):
//   - noop (Phase 0): passthrough, emits a skipped event for observability
//   - docker (Phase 1): per-run container via shell-out to docker/podman
//   - kubernetes (Phase 5): per-run Pod sibling via the k8s API
package sandbox

import (
	"context"
	"io"
	"os/exec"
)

// Driver is the abstraction over a concrete sandbox runtime.
//
// A driver advertises its [Capabilities] (which spec features it
// supports), validates a [Spec] in [Driver.Prepare], and starts a
// per-run sandbox via [Driver.Start]. The returned [Run] is the handle
// callers use to execute commands inside the sandbox.
//
// Implementations must be safe to call concurrently — the engine may
// spawn multiple sandboxed runs in parallel.
type Driver interface {
	// Name returns a stable identifier ("docker", "podman",
	// "kubernetes", "noop") used in events and diagnostics.
	Name() string

	// Capabilities returns the features this driver supports. The
	// engine consults these before validating a [Spec] so workflows
	// that demand unsupported features fail fast with a clear error
	// rather than mid-run.
	Capabilities() Capabilities

	// Prepare resolves and validates a [Spec] against this driver's
	// capabilities. For drivers that pull/build images, it is also the
	// hook to perform that work. Returns an opaque [PreparedSpec] that
	// [Driver.Start] consumes.
	//
	// Prepare must not start a sandbox — it is a pure validation and
	// resource-acquisition step. Callers may discard the
	// [PreparedSpec] without leaking resources beyond image cache
	// state.
	Prepare(ctx context.Context, spec Spec) (PreparedSpec, error)

	// Start creates the sandbox and returns a [Run] handle. The
	// caller is responsible for invoking [Run.Cleanup] when done — a
	// returned [Run] always represents an allocated resource (even
	// for noop, where Cleanup is a nop).
	Start(ctx context.Context, prepared PreparedSpec, info RunInfo) (Run, error)
}

// Run is a live sandbox handle.
//
// Each Run corresponds to one iterion run; the engine creates it once
// and reuses it for every delegate invocation (claude_code, codex,
// tool node) to amortise container startup over the run's lifetime.
type Run interface {
	// Driver returns the driver name that created this Run. Used for
	// telemetry and to short-circuit driver-specific code paths.
	Driver() string

	// Command returns an *exec.Cmd that, when [exec.Cmd.Start] is
	// called, runs cmd inside the sandbox. Callers wire stdin/stdout/
	// stderr on the returned cmd exactly as they would for a plain
	// exec.CommandContext call.
	//
	// This is the integration surface for backends that already
	// construct their own exec.Cmd (claudesdk, codexsdk): instead of
	// `exec.CommandContext(ctx, "claude", args...)` they call
	// `run.Command(ctx, append([]string{"claude"}, args...), opts)`
	// and use the returned cmd identically.
	//
	// For the noop driver, Command is a transparent passthrough —
	// the returned cmd runs on the host with the host's environment,
	// preserving the pre-sandbox behaviour for unconfigured runs.
	//
	// For container drivers, Command rewrites the invocation to a
	// `docker exec` / `kubectl exec` form and forwards Cwd/Env via
	// the runtime-native flags so the inner process sees them.
	Command(ctx context.Context, cmd []string, opts ExecOpts) *exec.Cmd

	// Exec is a convenience wrapper around [Run.Command] +
	// [exec.Cmd.Run] that captures stdout/stderr buffers (when not
	// supplied via [ExecOpts]) and reports the exit code in
	// [ExecResult]. Use [Run.Command] when you need streaming I/O or
	// finer control over the subprocess lifecycle.
	Exec(ctx context.Context, cmd []string, opts ExecOpts) (ExecResult, error)

	// Stop initiates graceful shutdown. Implementations should send a
	// SIGTERM-equivalent and wait briefly for cleanup. [Run.Cleanup]
	// is the hard-kill path.
	Stop(ctx context.Context) error

	// Cleanup releases all resources (containers, networks,
	// volumes...). Safe to call multiple times. Always called by the
	// engine via defer; implementations must be idempotent.
	Cleanup(ctx context.Context) error
}

// Capabilities advertises a driver's supported feature set.
//
// The engine compares a [Spec] against capabilities at Prepare time:
// a workflow demanding `sandbox.build.dockerfile:` against a noop
// driver fails fast with a clear error rather than silently ignoring
// the build.
type Capabilities struct {
	// SupportsImage means the driver honours [Spec.Image].
	SupportsImage bool

	// SupportsBuild means the driver honours [Spec.Build] (Dockerfile
	// build at run start).
	SupportsBuild bool

	// SupportsMounts means the driver honours [Spec.Mounts] beyond
	// the implicit workspace mount.
	SupportsMounts bool

	// SupportsNetworkPolicy means the driver enforces
	// [Spec.Network] rules. False on noop and on cloud V1 (where
	// the runner pod is the sandbox and per-run policy isn't applied).
	SupportsNetworkPolicy bool

	// SupportsPostCreate means the driver runs [Spec.PostCreate]
	// after container start (typically `npm install` /
	// `devbox install`).
	SupportsPostCreate bool

	// SupportsRemoteUser means the driver honours [Spec.User] (the
	// devcontainer `remoteUser` field).
	SupportsRemoteUser bool
}

// PreparedSpec is the opaque result of [Driver.Prepare]. Drivers embed
// driver-specific resolution data (image SHA, build cache key, k8s
// PodSpec) and the engine treats it as a pass-through token to
// [Driver.Start].
type PreparedSpec interface {
	// DriverName returns the driver that produced this PreparedSpec.
	// The engine uses this to detect mismatches (a PreparedSpec from
	// driver A passed to Start of driver B → programmer error).
	DriverName() string
}

// ProxyConfigurer is an optional interface a [Driver] may implement to
// override the network proxy's bind address and the hostname injected
// into sandboxed containers as HTTPS_PROXY.
//
// Drivers that don't implement this fall back to the engine defaults:
// bind on 127.0.0.1:0, advertise "host.docker.internal" (the alias the
// docker driver wires via --add-host on Linux and that Docker Desktop
// resolves natively on macOS/Windows). The kubernetes driver implements
// this to bind on 0.0.0.0:0 and advertise the runner pod's IP (read from
// the ITERION_POD_IP env var injected via downward API), since the
// "host.docker.internal" alias does not exist in a pure k8s pod network.
type ProxyConfigurer interface {
	// ProxyConfig returns (bindAddr, advertiseHost, err). bindAddr is
	// passed verbatim to net.Listen — use ":0" / "0.0.0.0:0" to bind
	// all interfaces, "127.0.0.1:0" for loopback. advertiseHost is
	// the hostname or IP injected into HTTPS_PROXY/HTTP_PROXY env vars
	// of sandboxed containers; an empty string falls back to the
	// listener IP.
	ProxyConfig() (bindAddr, advertiseHost string, err error)
}

// RunInfo carries per-run metadata that drivers may need to label
// containers/pods, scope mounts, or compute run-specific paths.
type RunInfo struct {
	// RunID is the iterion run identifier. Drivers MUST include it in
	// container/pod labels for `kubectl get` / `docker ps`
	// observability.
	RunID string

	// FriendlyName is the human-readable run label
	// (e.g. "ready-slate-94f3"). Used in container names and merge
	// branch defaults; drivers may surface it but should not depend
	// on it for correctness — RunID is canonical.
	FriendlyName string

	// WorkspacePath is the host path that becomes the sandbox
	// workspace (typically a git worktree). The driver bind-mounts
	// or copies this into the sandbox at [Spec.WorkspaceFolder]
	// (default `/workspace`).
	WorkspacePath string

	// ProxyEndpoint, if non-empty, is the URL of the iterion network
	// proxy (HTTPS_PROXY value) injected into the sandbox by drivers
	// that support [Capabilities.SupportsNetworkPolicy]. Phase 3
	// populates this; Phase 0 leaves it empty.
	ProxyEndpoint string
}

// ExecOpts controls a single [Run.Exec] invocation.
type ExecOpts struct {
	// Env is appended to the sandbox's environment for this exec.
	// Empty values are honoured (used to unset).
	Env map[string]string

	// WorkDir overrides the cwd inside the sandbox. Defaults to the
	// sandbox's workspace folder when empty.
	WorkDir string

	// Stdin, Stdout, Stderr are wired directly to the process when
	// non-nil. A nil Stdin closes the child's stdin; nil Stdout/Stderr
	// captures output into the returned [ExecResult].
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ExecResult captures the outcome of [Run.Exec].
type ExecResult struct {
	// ExitCode is the process exit status (0 for success). Populated
	// even on graceful failure.
	ExitCode int

	// Stdout and Stderr are populated only when [ExecOpts.Stdout] /
	// [ExecOpts.Stderr] were nil (i.e. the driver buffered them).
	Stdout []byte
	Stderr []byte
}
