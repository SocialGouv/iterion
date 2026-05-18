package docker

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// DefaultWorkspace is the in-container path where the host worktree
// is bind-mounted. Devcontainer convention. Workflows can override
// via [sandbox.Spec.WorkspaceFolder].
const DefaultWorkspace = "/workspace"

// New returns a Docker driver bound to the given runtime, or an error
// when neither docker nor podman are on PATH. The constructor itself
// is cheap — no images are pulled, no containers created.
//
// The driver starts with a discard logger; callers (engine, doctor)
// install a real logger via [Driver.WithLogger] so sandbox events are
// interleaved with the rest of the run.
func New() (sandbox.Driver, error) {
	rt, err := Detect()
	if err != nil {
		return nil, &sandbox.ErrUnavailable{Driver: "docker", Reason: err.Error()}
	}
	return &Driver{rt: rt, logger: iterlog.New(iterlog.LevelInfo, io.Discard)}, nil
}

// Constructor is the [sandbox.DriverConstructor] hook for registration
// in [sandbox.Factory]. The factory falls back to the next candidate
// (podman, then noop) when this returns ErrUnavailable, so callers do
// not need to special-case Docker absence.
func Constructor() (sandbox.Driver, error) { return New() }

// Driver implements [sandbox.Driver] for the Docker / Podman runtimes.
type Driver struct {
	rt     Runtime
	logger *iterlog.Logger
}

// WithLogger returns a copy of the driver bound to a specific logger.
// The default logger discards output; engine integration installs the
// run's logger so sandbox events are interleaved with the rest of the
// run.
func (d *Driver) WithLogger(l *iterlog.Logger) *Driver {
	cp := *d
	if l != nil {
		cp.logger = l
	}
	return &cp
}

// Name returns the underlying runtime ("docker" or "podman").
func (d *Driver) Name() string { return string(d.rt) }

// Compile-time guard: Driver satisfies the optional ProxyConfigurer
// interface so the engine routes proxyAddressesForDriver to ProxyConfig
// instead of the loopback-only fallback (which is unreachable from a
// container via host.docker.internal).
var _ sandbox.ProxyConfigurer = (*Driver)(nil)

// ProxyConfig binds the network proxy on all interfaces and advertises
// the canonical "host.docker.internal" alias (which we wire via
// --add-host host-gateway in Start). The default bind 127.0.0.1 only
// listens on the host's loopback, but Docker's host-gateway resolves to
// the bridge IP (e.g. 172.17.0.1 on Linux), so the container's outbound
// CONNECTs land on a different interface and get ECONNREFUSED. Binding
// 0.0.0.0 covers both. The proxy enforces per-run bearer-token auth on
// every request, so this doesn't open it to unauthenticated host
// processes — only the sibling sandbox container that received the
// token in HTTPS_PROXY can use it.
func (d *Driver) ProxyConfig() (string, string, error) {
	return "0.0.0.0:0", "host.docker.internal", nil
}

// Capabilities advertises the features the driver supports today.
// Phase 1 implements image, mounts, env, remote user, and post-create.
// V2-6 adds Build (Dockerfile-at-run-start) via `docker buildx build`
// — BuildKit is already wired into the Docker daemon, no separate
// service needed.
func (d *Driver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:         true,
		SupportsBuild:         true, // V2-6 — docker buildx build
		SupportsMounts:        true,
		SupportsNetworkPolicy: false, // Phase 3
		SupportsPostCreate:    true,
		SupportsRemoteUser:    true,
	}
}

// Prepare validates the spec, ensures the requested image is present
// on the host (pulling it if missing), and returns an opaque
// [PreparedSpec] consumed by [Driver.Start]. It is the ctx-aware
// "do all the slow IO before allocating a container" hook.
func (d *Driver) Prepare(ctx context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Image == "" && spec.Build == nil {
		return nil, fmt.Errorf("docker: sandbox.image is required when mode=inline (or declare a build: block); use mode=auto with a .devcontainer/devcontainer.json to read it from there")
	}
	workspace := spec.WorkspaceFolder
	if workspace == "" {
		workspace = DefaultWorkspace
	}

	// Pull is only meaningful when we have a pre-built image ref.
	// Build-driven specs materialize the image at Build() time and
	// always end up local — no pull required.
	if spec.Image != "" && !imageExists(ctx, d.rt, spec.Image) {
		d.logger.Info("sandbox: pulling image %s via %s", spec.Image, d.rt)
		if err := pullImage(ctx, d.rt, spec.Image); err != nil {
			return nil, err
		}
	}
	return &Prepared{
		spec:      spec,
		workspace: workspace,
		runtime:   d.rt,
	}, nil
}

// validateEnvVar rejects an env var whose name or value contains
// control characters that would break docker's --env arg parser.
// docker's parser tokenises by whitespace, so a value containing
// "\nDOCKER_HOST=tcp://attacker" would inject a second flag.
func validateEnvVar(k, v string) error {
	if strings.ContainsAny(k, "=\n\r\x00") {
		return fmt.Errorf("invalid env var name (contains '=', newline, or NUL): %q", k)
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("env var value contains a newline, carriage return, or NUL — refusing to inject")
	}
	return nil
}

// Start creates and starts a long-lived container holding the run for
// its lifetime. The caller invokes [Run.Command] / [Run.Exec] for each
// delegate (claude_code, tool node) — startup cost is amortised across
// every invocation in the run.
func (d *Driver) Start(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("docker: PreparedSpec from driver %q passed to docker.Start", prepared.DriverName())
	}
	if info.WorkspacePath == "" {
		return nil, fmt.Errorf("docker: RunInfo.WorkspacePath is required (set to the host worktree path)")
	}
	absWorkspace, err := filepath.Abs(info.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("docker: resolve workspace path: %w", err)
	}

	containerName := containerNameFor(info.RunID)
	args := []string{"run", "--detach", "--rm",
		"--name", containerName,
		"--label", "iterion.io/managed=true",
		"--label", "iterion.io/run-id=" + info.RunID,
		"--workdir", p.workspace,
		"--mount", "type=bind,source=" + absWorkspace + ",target=" + p.workspace,
	}
	if info.FriendlyName != "" {
		args = append(args, "--label", "iterion.io/run-name="+info.FriendlyName)
	}
	if p.spec.User != "" {
		if err := validatePlainArg("docker --user", p.spec.User); err != nil {
			return nil, err
		}
		args = append(args, "--user", p.spec.User)
	}
	if err := validatePlainArg("docker image", p.spec.Image); err != nil {
		return nil, err
	}
	if err := validatePlainArg("docker --workdir", p.workspace); err != nil {
		return nil, err
	}
	for _, m := range p.spec.Mounts {
		if err := validateMount(m); err != nil {
			return nil, fmt.Errorf("docker driver: %w", err)
		}
		args = append(args, "--mount", m)
	}
	// Iterate Env in sorted key order so validation errors and the
	// resulting `docker run` argv are deterministic — Go's map
	// iteration randomises order, which produced flaky tests when two
	// env vars were both invalid (the error message would refer to
	// whichever happened to be visited first).
	envKeys := make([]string, 0, len(p.spec.Env))
	for k := range p.spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		v := p.spec.Env[k]
		if err := validateEnvVar(k, v); err != nil {
			return nil, fmt.Errorf("docker driver: env %s: %w", k, err)
		}
		args = append(args, "--env", k+"="+v)
	}
	if info.ProxyEndpoint != "" {
		// Inject proxy URL via standard env vars + a host-gateway alias
		// so the container can reach the host's loopback interface
		// where the iterion proxy is listening. On Docker Desktop the
		// alias is built-in; on Linux we have to opt in via --add-host.
		args = append(args, "--env", "HTTPS_PROXY="+info.ProxyEndpoint)
		args = append(args, "--env", "HTTP_PROXY="+info.ProxyEndpoint)
		// Allow inner clones / installs that talk to localhost-only
		// services (rare, but legal — e.g. an inner devbox cache) to
		// bypass the proxy. The container's own services on its loop-
		// back interface should not be tunneled through the host.
		args = append(args, "--env", "NO_PROXY=localhost,127.0.0.1")
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}

	// PID 1 is `sleep infinity` so the container stays alive while
	// the run streams in N `docker exec` calls. We deliberately do
	// not use the image's CMD/ENTRYPOINT — that would shadow our
	// "container as a long-lived ssh-like target" model.
	args = append(args, p.spec.Image, "sleep", "infinity")

	out, err := runtimeCmdContext(ctx, d.rt, args...).CombinedOutput()
	if err != nil && isContainerNameConflict(out) {
		// A leftover container with the same name (one per run-id) is
		// a recoverable state, not a hard failure. The two common
		// causes are both transient:
		//   - prior daemon was SIGTERM-killed while a sandbox was
		//     up; --rm cleanup raced with shutdown and lost.
		//   - prior run failed without reaching Cleanup (e.g. a panic
		//     between Start return and the engine's defer).
		// Force-remove the stale container and retry once. If the
		// retry also fails, surface the original error chain so the
		// operator sees the underlying cause.
		d.logger.Warn("sandbox: container %q already exists from a prior run — force-removing and retrying", containerName)
		if rmErr := forceRemoveContainer(ctx, d.rt, containerName); rmErr != nil {
			return nil, fmt.Errorf("docker: run: %w\noutput: %s\n(also tried force-remove of stale container: %v)", err, string(out), rmErr)
		}
		out, err = runtimeCmdContext(ctx, d.rt, args...).CombinedOutput()
	}
	if err != nil {
		return nil, fmt.Errorf("docker: run: %w\noutput: %s", err, string(out))
	}
	containerID := strings.TrimSpace(string(out))

	r := &Run{
		driver:        d,
		containerID:   containerID,
		containerName: containerName,
		prepared:      p,
		info:          info,
	}

	if p.spec.PostCreate != "" {
		if err := r.runPostCreate(ctx, p.spec.PostCreate); err != nil {
			// Best-effort cleanup on PostCreate failure. PostCreate
			// often fails *because* ctx is done (timeout, caller
			// cancel) — passing the same ctx to Cleanup would leave it
			// with no budget to actually remove the container we just
			// created. Strip cancellation so Cleanup's internal 10s
			// timeout governs.
			_ = r.Cleanup(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("docker: postCreate failed: %w", err)
		}
	}

	d.logger.Info("sandbox: container %s started (image=%s, workspace=%s)", containerShortID(containerID), p.spec.Image, p.workspace)
	return r, nil
}

// Prepared is the docker driver's [sandbox.PreparedSpec] implementation.
type Prepared struct {
	spec      sandbox.Spec
	workspace string
	runtime   Runtime
}

// DriverName implements [sandbox.PreparedSpec].
func (p *Prepared) DriverName() string { return string(p.runtime) }

// Spec returns the spec the prepared was built from. Useful for tests
// and engine-side diagnostics.
func (p *Prepared) Spec() sandbox.Spec { return p.spec }

// Run is the live docker driver sandbox handle.
//
// All [Run] methods are safe to call concurrently — `docker exec` is
// itself concurrent-safe and the cleanup mutex serialises lifecycle
// transitions.
type Run struct {
	driver        *Driver
	containerID   string // full SHA returned by `docker run -d`
	containerName string // human-readable label, also stable across runtime restarts
	prepared      *Prepared
	info          sandbox.RunInfo

	mu      sync.Mutex
	stopped bool
	cleaned bool
}

// Driver returns the runtime name — "docker" or "podman".
func (r *Run) Driver() string { return string(r.driver.rt) }

// Command returns an *exec.Cmd that, when started, runs cmd inside the
// container via `docker exec`. Stdin/Stdout/Stderr on the returned cmd
// are forwarded transparently to the in-container process by docker
// itself, so callers can drive streaming I/O exactly as they would for
// a host subprocess.
//
// Cwd defaults to the workspace folder (the bind-mount target);
// [ExecOpts.WorkDir] overrides per-call.
//
// Env vars are passed via `docker exec --env KEY=VAL` so the inner
// process sees them — setting them on the returned [exec.Cmd.Env]
// would only affect the host-side `docker exec` driver process, not
// the inner program.
func (r *Run) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(cmd) == 0 {
		// Mirror noop's degenerate case: return a cmd that errors on
		// Start.
		return exec.CommandContext(ctx, "")
	}

	args := []string{"exec"}
	if opts.Stdin != nil || opts.KeepStdinOpen {
		args = append(args, "--interactive")
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = r.prepared.workspace
	}
	args = append(args, "--workdir", workDir)
	for k, v := range opts.Env {
		// Validate every per-call env var. Without this a newline (or
		// other control char) embedded in a value — e.g. coming from
		// an artifact field interpolated into a tool command — could
		// split the argument and inject arbitrary `docker exec` flags.
		// Driver.Start applies the same check at container-creation
		// time; the per-exec path here needs it too.
		if err := validateEnvVar(k, v); err != nil {
			r.driver.logger.Warn("docker exec: skipping invalid env %s: %v", k, err)
			continue
		}
		args = append(args, "--env", k+"="+v)
	}
	if r.prepared.spec.User != "" {
		args = append(args, "--user", r.prepared.spec.User)
	}
	args = append(args, r.containerID)
	args = append(args, cmd...)

	c := exec.CommandContext(ctx, string(r.driver.rt), args...)
	if opts.Stdin != nil {
		c.Stdin = opts.Stdin
	}
	proc.DetachProcessGroup(c)
	return c
}

// Exec is a buffered convenience wrapper around [Run.Command]. See
// [sandbox.Run.Exec] for semantics.
func (r *Run) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if len(cmd) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("docker.Exec: empty cmd")
	}
	return sandbox.ExecCmd(r.Command(ctx, cmd, opts), opts)
}

// Stop sends SIGTERM via `docker stop` with a short grace period.
// Idempotent — calling Stop on an already-stopped container is a
// no-op.
func (r *Run) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	// Give the container 5s to exit gracefully, then docker SIGKILLs.
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runtimeCmdContext(stopCtx, r.driver.rt, "stop", "--time", "5", r.containerID).CombinedOutput()
	if err != nil {
		// "no such container" means it already exited — treat as success.
		if strings.Contains(string(out), "No such container") || strings.Contains(string(out), "no such container") {
			return nil
		}
		return fmt.Errorf("docker: stop %s: %w\noutput: %s", containerShortID(r.containerID), err, string(out))
	}
	return nil
}

// Cleanup ensures the container is gone. Containers were created with
// `--rm` so a graceful Stop already removes them; Cleanup is the
// fallback for the failure-mode where the container is alive but
// orphaned (engine crash mid-run, etc.).
func (r *Run) Cleanup(ctx context.Context) error {
	r.mu.Lock()
	if r.cleaned {
		r.mu.Unlock()
		return nil
	}
	r.cleaned = true
	r.mu.Unlock()

	// Best-effort stop first to give --rm a chance to fire normally.
	_ = r.Stop(ctx)

	// Then force-remove any lingering container with our run-id label,
	// independent of the captured containerID (covers the case where
	// the container died but didn't get auto-removed). Use the
	// caller's ctx as parent so a cancelled run aborts cleanup quickly
	// (10s is a cap on the cleanup itself, not an extension past the
	// caller's deadline). Falling back to context.Background when ctx
	// is nil keeps the prior behaviour for niche callers.
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	out, err := runtimeCmdContext(cleanupCtx, r.driver.rt, "rm", "--force", r.containerID).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such container") || strings.Contains(string(out), "no such container") {
			return nil
		}
		// Don't propagate cleanup errors to the run — they are usually
		// "container already gone" from --rm racing with us. Log at
		// debug level for forensics.
		r.driver.logger.Debug("sandbox: cleanup of %s reported: %v (output: %s)", containerShortID(r.containerID), err, string(out))
	}
	return nil
}

// runPostCreate runs the spec's post-create command inside the
// freshly started container. Stdout/stderr are streamed to the
// driver's logger so users see install progress.
func (r *Run) runPostCreate(ctx context.Context, snippet string) error {
	r.driver.logger.Info("sandbox: running postCreateCommand")
	return sandbox.RunPostCreate(ctx, r, snippet, r.driver.logger)
}

// containerNameFor maps a run ID to a deterministic container name.
// Stable across iterion server restarts so a `docker ps` listing keeps
// the same identifiers visible to the operator.
func containerNameFor(runID string) string {
	// Run IDs are already filesystem-safe (e.g. "run_1777989944581").
	// Length cap: docker truncates names beyond 64 chars.
	name := "iterion-" + runID
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// containerShortID truncates a SHA to its first 12 chars, matching the
// convention `docker ps` uses for compact display.
func containerShortID(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// isContainerNameConflict matches the docker-daemon error message that
// `docker run --name X` produces when a container with name X already
// exists (running, stopped, or paused). The message is stable enough
// across docker / podman / runc versions to substring-match without
// over-fitting:
//
//	docker: Error response from daemon: Conflict. The container name
//	"/iterion-run_xxx" is already in use by container "<sha>". You have
//	to remove (or rename) that container to be able to reuse that name.
//
// `podman` produces the slightly different "the container name … is
// already in use by …" — covering both with a shared substring.
func isContainerNameConflict(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "is already in use")
}

// forceRemoveContainer issues `docker rm -f <name>` to evict a leftover
// container that's blocking a name reuse. Best-effort: returns the
// underlying error so the caller can decide whether to surface a
// combined diagnostic. Idempotent — removing a missing container is
// not treated as a hard error by the caller path.
func forceRemoveContainer(ctx context.Context, rt Runtime, name string) error {
	out, err := runtimeCmdContext(ctx, rt, "rm", "-f", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rm -f %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Compile-time interface checks.
var (
	_ sandbox.Driver       = (*Driver)(nil)
	_ sandbox.Run          = (*Run)(nil)
	_ sandbox.PreparedSpec = (*Prepared)(nil)
)
