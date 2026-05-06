package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Compile-time interface checks.
var (
	_ sandbox.Driver       = (*Driver)(nil)
	_ sandbox.Run          = (*Run)(nil)
	_ sandbox.PreparedSpec = (*Prepared)(nil)
)

// Ensure strings is referenced so goimports doesn't drop the import
// when this file is auto-formatted (used inside Command for the
// custom-workdir branch via buildShellChdirExec — but the import is
// here so future extensions can add string ops without flapping).
var _ = strings.ReplaceAll

// DefaultPodReadyTimeoutSecs caps how long the driver waits for a
// freshly-applied pod to reach Ready. Image pulls dominate this in
// practice (cluster-cached images go Ready in <2s; cold pulls of
// multi-GB images take 30-60s).
const DefaultPodReadyTimeoutSecs = 180

// New returns a kubernetes driver bound to the in-cluster service
// account, or [sandbox.ErrUnavailable] when the host doesn't qualify
// (no kubectl, no in-cluster token). Cheap — no API calls.
func New() (sandbox.Driver, error) {
	binPath, namespace, err := Detect()
	if err != nil {
		return nil, &sandbox.ErrUnavailable{Driver: "kubernetes", Reason: err.Error()}
	}
	return &Driver{
		kubectl:   binPath,
		namespace: namespace,
		logger:    iterlog.New(iterlog.LevelInfo, io.Discard),
	}, nil
}

// Constructor is the [sandbox.DriverConstructor] hook the factory
// calls. Returning ErrUnavailable lets the factory fall through to
// the noop driver on hosts that aren't in-cluster — same fallback
// shape as the docker driver.
func Constructor() (sandbox.Driver, error) { return New() }

// Driver implements [sandbox.Driver] for in-cluster runs.
//
// State is intentionally minimal: kubectl path, namespace, logger.
// Per-run state lives on the [Run] handle returned by [Driver.Start].
type Driver struct {
	kubectl   string
	namespace string
	logger    *iterlog.Logger
}

// WithLogger returns a copy of the driver bound to a real logger.
// The default discards output; engine integration installs the
// run's logger so sandbox messages appear interleaved with the rest.
func (d *Driver) WithLogger(l *iterlog.Logger) *Driver {
	cp := *d
	if l != nil {
		cp.logger = l
	}
	return &cp
}

// Name returns "kubernetes".
func (d *Driver) Name() string { return "kubernetes" }

// Capabilities advertises the feature set the V1 driver supports.
// Per-run NetworkPolicy synthesis is deferred to V2; today the
// engine's CONNECT proxy provides egress filtering for both docker
// and kubernetes drivers.
func (d *Driver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:         true,
		SupportsBuild:         false, // Phase 2 of the broader plan; would need Kaniko in-cluster
		SupportsMounts:        false, // V2 — needs PVC/CSI plumbing
		SupportsNetworkPolicy: false, // V2 — proxy handles egress today
		SupportsPostCreate:    true,
		SupportsRemoteUser:    true,
	}
}

// Prepare validates the spec. Unlike the docker driver, the
// kubernetes driver does not pre-pull the image — kubelet handles
// the pull when the pod is admitted, with image-pull policies
// already configured at the cluster level.
func (d *Driver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Build != nil {
		return nil, fmt.Errorf("kubernetes: sandbox.build is not supported in-cluster (V2 will wire Kaniko); use a pre-built image: instead")
	}
	if spec.Image == "" {
		return nil, fmt.Errorf("kubernetes: sandbox.image is required; declare an image: field or use mode=auto with a .devcontainer/devcontainer.json")
	}
	if len(spec.Mounts) > 0 {
		return nil, fmt.Errorf("kubernetes: sandbox.mounts is not supported in V1 (V2 will wire PVCs); the workspace is provided via emptyDir")
	}
	workspace := spec.WorkspaceFolder
	if workspace == "" {
		workspace = "/workspace"
	}
	return &Prepared{spec: spec, workspace: workspace}, nil
}

// Start applies the pod manifest, waits for Ready, optionally runs
// post-create, and returns a live [Run] handle.
func (d *Driver) Start(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("kubernetes: PreparedSpec from driver %q passed to kubernetes.Start", prepared.DriverName())
	}

	podName := podNameFor(info.RunID)
	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace:      d.namespace,
		Name:           podName,
		RunID:          info.RunID,
		FriendlyName:   info.FriendlyName,
		Spec:           p.spec,
		WorkspaceMount: p.workspace,
		ProxyEndpoint:  info.ProxyEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build manifest: %w", err)
	}

	if err := applyManifest(ctx, d.namespace, manifest); err != nil {
		return nil, fmt.Errorf("kubernetes: apply pod: %w", err)
	}

	r := &Run{
		driver:    d,
		podName:   podName,
		namespace: d.namespace,
		prepared:  p,
		info:      info,
	}

	if err := waitForPodRunning(ctx, d.namespace, podName, DefaultPodReadyTimeoutSecs); err != nil {
		_ = r.Cleanup(ctx)
		return nil, fmt.Errorf("kubernetes: wait for pod ready: %w", err)
	}

	if p.spec.PostCreate != "" {
		if err := r.runPostCreate(ctx, p.spec.PostCreate); err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: postCreate: %w", err)
		}
	}

	d.logger.Info("sandbox: kubernetes pod %s/%s started (image=%s)", d.namespace, podName, p.spec.Image)
	return r, nil
}

// Prepared is the kubernetes driver's [sandbox.PreparedSpec].
type Prepared struct {
	spec      sandbox.Spec
	workspace string
}

// DriverName implements [sandbox.PreparedSpec].
func (p *Prepared) DriverName() string { return "kubernetes" }

// Spec returns the spec the prepared was built from.
func (p *Prepared) Spec() sandbox.Spec { return p.spec }

// Run is the kubernetes-driver [sandbox.Run] handle. All operations
// are concurrent-safe: kubectl is itself concurrent-safe, and the
// cleanup mutex serialises the lifecycle transitions.
type Run struct {
	driver    *Driver
	podName   string
	namespace string
	prepared  *Prepared
	info      sandbox.RunInfo

	mu      sync.Mutex
	stopped bool
	cleaned bool
}

// Driver returns "kubernetes".
func (r *Run) Driver() string { return "kubernetes" }

// Command returns an *exec.Cmd that, when started, runs cmd inside
// the sandbox pod via `kubectl exec`. Stdin/Stdout/Stderr on the
// returned cmd are forwarded transparently by kubectl.
//
// Cwd defaults to the prepared workspace; [ExecOpts.WorkDir]
// overrides per-call. Env vars are passed via env-prefixed argv
// (`env KEY=val cmd ...`) because `kubectl exec` doesn't expose a
// `--env` flag — the sandbox env established at pod creation time
// is the base, and per-call envs are layered on top via the env
// command.
func (r *Run) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(cmd) == 0 {
		return exec.CommandContext(ctx, "")
	}

	args := []string{"--namespace", r.namespace, "exec"}
	if opts.Stdin != nil {
		args = append(args, "--stdin")
	}
	args = append(args, r.podName, "--container", "workload", "--")

	// Per-call cwd is realised by `cd <dir> && exec ...` — kubectl
	// exec doesn't take a --workdir flag. We avoid quoting issues
	// by exec'ing through `sh -c` only when WorkDir is non-default;
	// otherwise the pod's container.workingDir already applies.
	workDir := opts.WorkDir
	if workDir == "" || workDir == r.prepared.workspace {
		// Default workingDir already set on the container; use direct
		// argv form to avoid an extra shell layer (preserves signal
		// semantics and exit codes).
		args = appendEnvPrefix(args, opts.Env)
		args = append(args, cmd...)
		return r.cmdContext(ctx, args, opts)
	}

	// Custom workdir — wrap in `sh -c "cd <dir> && exec <cmd...>"`.
	wrapped := buildShellChdirExec(workDir, cmd, opts.Env)
	args = append(args, "sh", "-c", wrapped)
	return r.cmdContext(ctx, args, opts)
}

// cmdContext finalises the *exec.Cmd: ctx, args, stdin pipe, pgid.
func (r *Run) cmdContext(ctx context.Context, args []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := exec.CommandContext(ctx, r.driver.kubectl, args...)
	if opts.Stdin != nil {
		c.Stdin = opts.Stdin
	}
	detachProcessGroup(c)
	return c
}

// Exec is the buffered convenience wrapper.
func (r *Run) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if len(cmd) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("kubernetes.Exec: empty cmd")
	}
	c := r.Command(ctx, cmd, opts)
	var stdoutBuf, stderrBuf bytes.Buffer
	if opts.Stdout != nil {
		c.Stdout = opts.Stdout
	} else {
		c.Stdout = &stdoutBuf
	}
	if opts.Stderr != nil {
		c.Stderr = opts.Stderr
	} else {
		c.Stderr = &stderrBuf
	}
	err := c.Run()
	res := sandbox.ExecResult{}
	if c.ProcessState != nil {
		res.ExitCode = c.ProcessState.ExitCode()
	}
	if opts.Stdout == nil {
		res.Stdout = stdoutBuf.Bytes()
	}
	if opts.Stderr == nil {
		res.Stderr = stderrBuf.Bytes()
	}
	if _, isExit := err.(*exec.ExitError); isExit {
		err = nil
	}
	return res, err
}

// Stop is a no-op for the kubernetes driver: pod lifecycle is
// controlled via kubectl delete, which Cleanup() invokes. Implemented
// to satisfy the interface and reserve the slot for V2 graceful
// drains (e.g. SIGTERM-then-wait before the hard delete).
func (r *Run) Stop(_ context.Context) error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	return nil
}

// Cleanup deletes the sandbox pod. Idempotent — kubectl's
// --ignore-not-found handles the second call cleanly. Errors here
// are non-fatal for the engine: a leaked pod will be GC'd by a
// cluster-side controller (V2 ships a CronJob for this).
func (r *Run) Cleanup(_ context.Context) error {
	r.mu.Lock()
	if r.cleaned {
		r.mu.Unlock()
		return nil
	}
	r.cleaned = true
	r.mu.Unlock()

	deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := deleteResource(deleteCtx, r.namespace, "pod", r.podName); err != nil {
		// Surface to the logger at debug level — most failures are
		// "AlreadyDeleted" or transient API hiccups.
		r.driver.logger.Debug("sandbox: kubernetes cleanup of %s/%s reported: %v", r.namespace, r.podName, err)
	}
	return nil
}

// runPostCreate executes the spec's post-create command inside the
// freshly started pod. Mirrors the docker driver's helper.
func (r *Run) runPostCreate(ctx context.Context, snippet string) error {
	r.driver.logger.Info("sandbox: running postCreateCommand in pod %s", r.podName)
	res, err := r.Exec(ctx, []string{"sh", "-c", snippet}, sandbox.ExecOpts{})
	if err != nil {
		return fmt.Errorf("postCreateCommand: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("postCreateCommand exited %d:\nstdout:\n%s\nstderr:\n%s", res.ExitCode, string(res.Stdout), string(res.Stderr))
	}
	return nil
}

// podNameFor maps a run ID to a deterministic pod name. The k8s API
// caps name length at 253 chars, but DNS-1123 subdomain rules cap
// label segments at 63. We keep names well under that.
func podNameFor(runID string) string {
	// k8s names must be lowercase alphanumeric + dashes; runIDs
	// from iterion are filesystem-safe ("run_1777..."). Replace
	// underscores with dashes.
	n := toLowerASCII("iterion-run-" + runID)
	n = replaceUnderscores(n)
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}
