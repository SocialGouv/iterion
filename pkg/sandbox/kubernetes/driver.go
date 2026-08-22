package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Compile-time interface checks.
var (
	_ sandbox.Driver                 = (*Driver)(nil)
	_ sandbox.Run                    = (*Run)(nil)
	_ sandbox.PreparedSpec           = (*Prepared)(nil)
	_ sandbox.ProxyConfigurer        = (*Driver)(nil)
	_ sandbox.SecretFileRefresher    = (*Run)(nil)
	_ sandbox.WorkspaceFileRefresher = (*Run)(nil)
	_ sandbox.WorkspaceExporter      = (*Run)(nil)
)

// NOTE: the kubernetes driver intentionally does NOT implement
// [sandbox.Builder]. `sandbox.build:` (Dockerfile-at-run-start) is a
// local-docker-only feature in V2-6: cloud workflows must reference
// pre-built image refs via `sandbox.image:` (typically built by CI
// and pinned by digest). Reasoning is documented in
// docs/sandbox.md § "BuildKit (local docker only)".

// PodIPEnvVar is the downward-API env var the runner pod must inject so
// the kubernetes driver can advertise a routable proxy address to
// sibling sandbox pods (the default "host.docker.internal" alias does
// not exist in pure k8s pod networking).
const PodIPEnvVar = "ITERION_POD_IP"

// DefaultPodReadyTimeoutSecs caps how long the driver waits for a
// freshly-applied pod to reach Ready. Image pulls dominate this in
// practice (cluster-cached images go Ready in <2s; cold pulls of
// multi-GB images take 30-60s).
const DefaultPodReadyTimeoutSecs = 180

// Downward-API env vars the runner pod's Helm chart should inject so the
// driver can set an ownerReference on every per-run resource (sandbox
// pod, Secrets, NetworkPolicy) pointing back at the runner pod. When a
// runner pod is deleted (deployment rollout, node drain, scale-down) the
// cluster then cascade-GCs its whole sandbox footprint — including the
// plaintext-credential Secret — without waiting for the label reaper.
// Best-effort: unset → no ownerReference, and the run leans on
// activeDeadlineSeconds + the reaper. Wire via:
//
//	env:
//	  - name: ITERION_RUNNER_POD_NAME
//	    valueFrom: {fieldRef: {fieldPath: metadata.name}}
//	  - name: ITERION_RUNNER_POD_UID
//	    valueFrom: {fieldRef: {fieldPath: metadata.uid}}
const (
	RunnerPodNameEnvVar = "ITERION_RUNNER_POD_NAME"
	RunnerPodUIDEnvVar  = "ITERION_RUNNER_POD_UID"
)

// deadlineMarginSecs is added to the run's budgeted max_duration when
// deriving spec.activeDeadlineSeconds, so a run that legitimately uses
// its full budget (plus sandbox setup/teardown slack) is never killed by
// the deadline mid-work — the deadline only reaps genuinely-leaked pods.
const deadlineMarginSecs int64 = 30 * 60 // 30 minutes

// runnerPodOwner reads the runner pod's identity from the downward-API
// env vars and returns an OwnerReference, or nil when the chart didn't
// wire them (best-effort — the reaper + activeDeadlineSeconds remain).
func runnerPodOwner() *OwnerReference {
	name := os.Getenv(RunnerPodNameEnvVar)
	uid := os.Getenv(RunnerPodUIDEnvVar)
	if name == "" || uid == "" {
		return nil
	}
	return &OwnerReference{APIVersion: "v1", Kind: "Pod", Name: name, UID: uid}
}

// activeDeadlineFor derives the sandbox pod's activeDeadlineSeconds from
// the run's budgeted max_duration (seconds). Returns 0 (unbounded) when
// the run has no duration budget — we never invent a cap the operator
// didn't ask for, since the reaper is the backstop for unbounded runs.
func activeDeadlineFor(maxDurationSecs int64) int64 {
	if maxDurationSecs <= 0 {
		return 0
	}
	return maxDurationSecs + deadlineMarginSecs
}

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

// ProxyConfig binds the network proxy on all interfaces (so sibling
// sandbox pods can reach it across the cluster network) and advertises
// the runner pod's own IP, read from the [PodIPEnvVar] env var. The
// runner pod manifest must inject this via the downward API:
//
//	env:
//	  - name: ITERION_POD_IP
//	    valueFrom:
//	      fieldRef:
//	        fieldPath: status.podIP
//
// The proxy enforces a per-run bearer token in every CONNECT request,
// so binding 0.0.0.0 doesn't open the proxy to unauthenticated
// in-cluster traffic — only callers that received the token (i.e. the
// sibling pods this driver creates) can use it.
func (d *Driver) ProxyConfig() (string, string, error) {
	podIP := os.Getenv(PodIPEnvVar)
	if podIP == "" {
		return "", "", fmt.Errorf("kubernetes: %s env var is empty; the runner pod manifest must inject it via downward API (status.podIP) so the network proxy can advertise a routable address to sibling sandbox pods", PodIPEnvVar)
	}
	return "0.0.0.0:0", podIP, nil
}

// Capabilities advertises the feature set the V1 driver supports.
// NetworkPolicy synthesis is on (V2-5). `sandbox.build:` is local-only
// (docker driver, V2-6) — cloud workflows reference pre-built images
// instead. `sandbox.mounts:` honours PVC/ConfigMap/Secret entries
// (V2-7); bind mounts are rejected because cloud pods have no host
// filesystem. Enforcement of NetworkPolicy still requires a CNI that
// honours the resource (Calico, Cilium, …).
func (d *Driver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsBuild:          false, // local-only feature; cloud users build via CI + sandbox.image:
		SupportsMounts:         true,  // V2-7 — type=pvc / type=configmap / type=secret
		SupportsHostBindMounts: false, // pods have no host filesystem — type=bind is rejected; the iterion/rtk binaries are baked into the sandbox image
		SupportsNetworkPolicy:  true,  // V2-5 — synthesised per-run; CNI must enforce
		SupportsPostCreate:     true,
		SupportsRemoteUser:     true,
		SupportsTLSInspection:  true, // per-run CA Secret mounted into the pod (not cluster-validated)
	}
}

// Prepare validates the spec. Unlike the docker driver, the
// kubernetes driver does not pre-pull the image — kubelet handles
// the pull when the pod is admitted, with image-pull policies
// already configured at the cluster level.
//
// `sandbox.build:` is rejected here. Building images at run-start
// is a local-only feature (V2-6, docker driver via `docker buildx`).
// Cloud workflows must reference pre-built image refs via
// `sandbox.image:` — build via CI, pin by digest, point iterion at
// the result. See docs/sandbox.md.
func (d *Driver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	// Spec validity + the cloud-driver hard constraints (no build, image
	// required, host_state!=auto, numeric user) live in ValidateSpec so
	// `iterion sandbox doctor --strict` can run the identical battery
	// without an in-cluster kubectl. V2-7: sandbox.mounts entries are
	// validated lazily — translateMounts at manifest-render time produces
	// a clear error pointing at the offending entry, so authors see the
	// offending mount string verbatim rather than a generic rejection here.
	// Default the numeric user before validation: under sandbox-by-default
	// most specs are the platform's synthetic default-image spec (published
	// iterion-sandbox-* images, which all run as devbox uid 1000) and carry
	// no user: field — hard-requiring one made every ambient cloud sandbox
	// fail at boot (observed live, run 019f8a37). An explicit user: still
	// wins; for a foreign image whose filesystem expects another uid, the
	// kubelet's runAsNonRoot/permission failure stays the visible guard.
	if spec.User == "" {
		spec.User = defaultPodUser
		d.logger.Info("kubernetes: sandbox.user not set — defaulting to %s (published iterion sandbox images run as devbox uid 1000); declare user: to override", defaultPodUser)
	}
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	workspace := spec.WorkspaceFolder
	if workspace == "" {
		workspace = "/workspace"
	}
	return &Prepared{spec: spec, workspace: workspace}, nil
}

// defaultPodUser is the uid:gid a spec without user: runs as on the
// kubernetes driver — the devbox user every published iterion-sandbox-*
// image is built with.
const defaultPodUser = "1000:1000"

// Start applies the pod manifest, waits for Ready, optionally runs
// post-create, and returns a live [Run] handle.
func (d *Driver) Start(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("kubernetes: PreparedSpec from driver %q passed to kubernetes.Start", prepared.DriverName())
	}

	podName := podNameFor(info.RunID)

	// Best-effort cascade-GC owner (the runner pod) + a bounded lifetime,
	// so a runner killed mid-run before Cleanup fires doesn't leak the pod
	// or its plaintext-credential Secret indefinitely (ADR-070). Both
	// degrade gracefully: owner nil when the downward API isn't wired,
	// deadline 0 when the run has no duration budget — the label reaper is
	// the backstop for either.
	owner := runnerPodOwner()
	activeDeadline := activeDeadlineFor(info.MaxDurationSeconds)

	// The in-pod workspace must live at the SAME absolute path the bot's
	// tool/agent nodes use — RunInfo.WorkspacePath, the runner's worktree
	// (= {{run.worktree}} / PROJECT_DIR) — exactly as the docker driver
	// bind-mounts the worktree at its host absolute path. Otherwise the
	// emptyDir mounts at /workspace while a tool node's `git -C <worktree>`
	// hits a path that doesn't exist in the pod and fails (exit 128). This
	// also keeps the container workingDir, the populate target, and every
	// node's cwd aligned on the one path.
	if info.WorkspacePath != "" {
		p.workspace = info.WorkspacePath
	}

	// File secrets: create a per-run opaque Secret BEFORE the pod so the
	// workload can mount each key as a read-only file. The Secret is
	// deleted in Cleanup together with the pod.
	secretFilesSecretName := ""
	if len(p.spec.SecretFiles) > 0 {
		secretFilesSecretName = podName + "-secret-files"
		secretManifest, err := BuildSecretFilesSecret(d.namespace, secretFilesSecretName, info.RunID, info.FriendlyName, p.spec.SecretFiles, owner)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: build file secrets secret: %w", err)
		}
		if err := applyManifest(ctx, d.namespace, secretManifest); err != nil {
			return nil, fmt.Errorf("kubernetes: apply file secrets secret: %w", err)
		}
	}

	// Egress TLS-inspection CA (Layer 2): create the per-run CA Secret
	// BEFORE the pod so the pod can mount it. The Secret holds only the
	// public CA cert; the private key never leaves the runner.
	caSecretName := ""
	if len(info.ProxyCACert) > 0 {
		caSecretName = podName + "-ca"
		caSecret, err := BuildCASecret(d.namespace, caSecretName, info.RunID, info.FriendlyName, info.ProxyCACert, owner)
		if err != nil {
			if secretFilesSecretName != "" {
				_ = deleteResource(ctx, d.namespace, "secret", secretFilesSecretName)
			}
			return nil, fmt.Errorf("kubernetes: build CA secret: %w", err)
		}
		if err := applyManifest(ctx, d.namespace, caSecret); err != nil {
			if secretFilesSecretName != "" {
				_ = deleteResource(ctx, d.namespace, "secret", secretFilesSecretName)
			}
			return nil, fmt.Errorf("kubernetes: apply CA secret: %w", err)
		}
	}

	manifest, err := BuildPodManifest(PodManifestInput{
		Namespace:             d.namespace,
		Name:                  podName,
		RunID:                 info.RunID,
		FriendlyName:          info.FriendlyName,
		Spec:                  p.spec,
		WorkspaceMount:        p.workspace,
		ProxyEndpoint:         info.ProxyEndpoint,
		CASecretName:          caSecretName,
		SecretFilesSecretName: secretFilesSecretName,
		Owner:                 owner,
		ActiveDeadlineSeconds: activeDeadline,
	})
	if err != nil {
		if caSecretName != "" {
			_ = deleteResource(ctx, d.namespace, "secret", caSecretName)
		}
		if secretFilesSecretName != "" {
			_ = deleteResource(ctx, d.namespace, "secret", secretFilesSecretName)
		}
		return nil, fmt.Errorf("kubernetes: build manifest: %w", err)
	}

	// Resume idempotency: a prior attempt's pod may still exist (same run-id
	// → same name). `kubectl apply` would then PATCH it, but pods are largely
	// immutable and the runner SA intentionally lacks pods/patch → Forbidden,
	// parking the run on the DLQ. Force-delete any stale pod so apply always
	// CREATEs fresh; the resume re-populates the workspace from the checkpoint.
	delStale := kubectlCmdContext(ctx, "--namespace", d.namespace, "delete", "pod", podName,
		"--ignore-not-found=true", "--grace-period=0", "--force")
	_, _ = delStale.CombinedOutput()

	if err := applyManifest(ctx, d.namespace, manifest); err != nil {
		if caSecretName != "" {
			_ = deleteResource(ctx, d.namespace, "secret", caSecretName)
		}
		if secretFilesSecretName != "" {
			_ = deleteResource(ctx, d.namespace, "secret", secretFilesSecretName)
		}
		return nil, fmt.Errorf("kubernetes: apply pod: %w", err)
	}

	r := &Run{
		driver:                d,
		podName:               podName,
		namespace:             d.namespace,
		prepared:              p,
		info:                  info,
		caSecretName:          caSecretName,
		secretFilesSecretName: secretFilesSecretName,
		secretFiles:           append([]sandbox.SecretFileMount(nil), p.spec.SecretFiles...),
	}

	// V2-5: synthesise a per-run NetworkPolicy when the proxy is
	// active. The policy locks the sibling pod's egress to the runner
	// pod (proxy) + kube-dns. Skipped silently when the proxy isn't
	// in play (network mode=open) or the runner can't introspect its
	// own IP. Enforcement requires a NetworkPolicy-aware CNI (Calico,
	// Cilium, ...) — see docs/sandbox.md § cloud.
	if info.ProxyEndpoint != "" {
		runnerIP := os.Getenv(PodIPEnvVar)
		if runnerIP == "" {
			// Fail-closed: the workflow requested isolated network
			// (sandbox.network with an allowlist), the proxy alone
			// cannot enforce it (anything bypassing HTTPS_PROXY hits
			// the kube API or whatever in-cluster service it can
			// reach), and the runner Helm chart didn't wire the
			// downward API to give us this pod's IP. Refuse to start
			// — better a loud failure the operator fixes than a
			// silent isolation downgrade. The previous behaviour
			// (Warn + continue) was the F-SB-6 footgun.
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: %s env is required when sandbox.network is active (Helm chart must wire the downward API)", PodIPEnvVar)
		}
		netpolicy, err := BuildNetworkPolicy(NetworkPolicyInput{
			Namespace:    d.namespace,
			Name:         podName,
			RunID:        info.RunID,
			FriendlyName: info.FriendlyName,
			RunnerPodIP:  runnerIP,
			Owner:        owner,
		})
		if err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: build netpolicy: %w", err)
		}
		if err := applyManifest(ctx, d.namespace, netpolicy); err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: apply netpolicy: %w", err)
		}
		r.networkPolicyApplied = true
	}

	if err := waitForPodRunning(ctx, d.namespace, podName, DefaultPodReadyTimeoutSecs); err != nil {
		_ = r.Cleanup(ctx)
		return nil, fmt.Errorf("kubernetes: wait for pod ready: %w", err)
	}

	// Phase 5 V2: the kubernetes driver has no host filesystem to
	// bind-mount, so the run's workspace (RunInfo.WorkspacePath — the
	// runner's clone/worktree) is COPIED into the pod's emptyDir /workspace
	// via a tar stream. Without this the sandbox starts with an empty
	// workspace (the V1 limitation, docs/sandbox.md) and a repo-bound bot
	// has nothing to work on. Skipped for workspace-less runs.
	if info.WorkspacePath != "" {
		if err := r.populateWorkspace(ctx, info.WorkspacePath, p.workspace); err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: populate workspace: %w", err)
		}
		if err := r.fixupWorkspaceGit(ctx, p.workspace); err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: fixup workspace git: %w", err)
		}
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

	// networkPolicyApplied tracks whether a per-run NetworkPolicy was
	// created in [Driver.Start] so [Run.Cleanup] knows to delete it.
	networkPolicyApplied bool

	// caSecretName is the per-run egress-CA Secret created in
	// [Driver.Start] (Layer 2 TLS inspection), deleted in [Run.Cleanup].
	// Empty when inspection is off.
	caSecretName string

	// secretFilesSecretName is the per-run Secret containing mounted file
	// secrets. Empty when the workflow declares no file secrets.
	secretFilesSecretName string

	// secretFiles is the current (post-refresh) ordered snapshot of the
	// mounted file secrets. RefreshSecretFile re-applies the whole Secret
	// with one value updated; keeping the snapshot means a later refresh
	// of a DIFFERENT key doesn't revert this one to its launch value. The
	// slice order is preserved so the indexed Secret keys (secret-<i>-...)
	// stay stable and the projected-volume item mapping still resolves.
	// Guarded by mu.
	secretFiles []sandbox.SecretFileMount

	mu      sync.Mutex
	cleaned bool
}

// Driver returns "kubernetes".
func (r *Run) Driver() string { return "kubernetes" }

// RefreshSecretFile re-applies the per-run file-secrets Secret with the
// named key's value updated, so a rotated short-lived token reaches the
// mounted projected volume. Implements [sandbox.SecretFileRefresher].
//
// The value is never logged. kubelet propagates a Secret update to a
// projected-volume mount within ~1 minute — well inside the refresh
// cadence and a token's lifetime. NOTE: this covers the DEFAULT
// directory-mounted secrets (`/run/iterion/secrets/*`); a secret with a
// custom absolute mount_path is projected via `subPath`, which kubelet
// does NOT auto-update — such a secret keeps its launch value. The Secret
// itself is still refreshed here; only the subPath projection is stale.
func (r *Run) RefreshSecretFile(ctx context.Context, name string, value []byte) error {
	manifest, err := r.renderRefreshedSecret(name, value)
	if err != nil {
		return err
	}
	if err := applyManifest(ctx, r.namespace, manifest); err != nil {
		return fmt.Errorf("kubernetes: refresh file secret %s: apply: %w", name, err)
	}
	return nil
}

// renderRefreshedSecret updates the in-memory snapshot with the named
// key's new value and returns the re-rendered Secret manifest. Split out
// from RefreshSecretFile (which then applies it) so the snapshot/render
// logic is testable without a live kubectl.
func (r *Run) renderRefreshedSecret(name string, value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("kubernetes: refresh file secret %s: empty value", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secretFilesSecretName == "" {
		return nil, fmt.Errorf("kubernetes: refresh: run has no file-secrets Secret")
	}
	updated := false
	for i := range r.secretFiles {
		if r.secretFiles[i].Name == name {
			r.secretFiles[i].Value = value
			updated = true
			break
		}
	}
	if !updated {
		return nil, fmt.Errorf("kubernetes: refresh: no mounted file secret %q", name)
	}
	// Re-apply with the SAME ownerReference the Secret was created with
	// (runnerPodOwner is idempotent — it reads the downward-API env), or the
	// refresh would strip the ownerReference and defeat the orphan-GC cascade
	// (ADR-070). The two features (mid-run refresh + ownerReference GC) touch
	// the same manifest and must agree.
	manifest, err := BuildSecretFilesSecret(r.namespace, r.secretFilesSecretName, r.info.RunID, r.info.FriendlyName, r.secretFiles, runnerPodOwner())
	if err != nil {
		return nil, fmt.Errorf("kubernetes: refresh file secret %s: build: %w", name, err)
	}
	return manifest, nil
}

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
//
// LIMITATION: a `sh -c <huge-script>` cmd is passed as a single argv
// element to `kubectl exec`, so a hundreds-of-KB interpolated script
// can in principle trip the host's ARG_MAX (E2BIG) the same way the
// docker driver did before its stdin-streaming fallback (see
// [shouldStreamScriptViaStdin] in pkg/sandbox/docker/driver.go). Not
// observed in practice yet — cloud runs interpolate smaller payloads
// — so the kubernetes driver currently relies on the argv path. If a
// real symptom appears, mirror the docker fix here using
// `kubectl exec -i … -- sh -s` with the script wired to Cmd.Stdin.
func (r *Run) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(cmd) == 0 {
		return exec.CommandContext(ctx, "")
	}

	args := []string{"--namespace", r.namespace, "exec"}
	if opts.Stdin != nil || opts.KeepStdinOpen {
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
	proc.DetachProcessGroup(c)
	return c
}

// Exec is the buffered convenience wrapper.
func (r *Run) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if len(cmd) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("kubernetes.Exec: empty cmd")
	}
	return sandbox.ExecCmd(r.Command(ctx, cmd, opts), opts)
}

// Cleanup deletes the sandbox pod. Idempotent — kubectl's
// --ignore-not-found handles the second call cleanly. Errors here
// are non-fatal for the engine: a pod leaked because Cleanup never
// fired (runner killed mid-run) is bounded by spec.activeDeadlineSeconds,
// cascade-GC'd when the runner pod is deleted (ownerReference), and swept
// by the label reaper (ReapOrphanResources) — see ADR-070.
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
	if r.networkPolicyApplied {
		if err := deleteResource(deleteCtx, r.namespace, "networkpolicy", r.podName); err != nil {
			r.driver.logger.Debug("sandbox: kubernetes netpolicy cleanup of %s/%s reported: %v", r.namespace, r.podName, err)
		}
	}
	if r.caSecretName != "" {
		if err := deleteResource(deleteCtx, r.namespace, "secret", r.caSecretName); err != nil {
			r.driver.logger.Debug("sandbox: kubernetes CA secret cleanup of %s/%s reported: %v", r.namespace, r.caSecretName, err)
		}
	}
	if r.secretFilesSecretName != "" {
		if err := deleteResource(deleteCtx, r.namespace, "secret", r.secretFilesSecretName); err != nil {
			r.driver.logger.Debug("sandbox: kubernetes file secrets cleanup of %s/%s reported: %v", r.namespace, r.secretFilesSecretName, err)
		}
	}
	return nil
}

// runPostCreate executes the spec's post-create command inside the
// freshly started pod.
func (r *Run) runPostCreate(ctx context.Context, snippet string) error {
	r.driver.logger.Info("sandbox: running postCreateCommand in pod %s", r.podName)
	return sandbox.RunPostCreate(ctx, r, snippet, r.driver.logger)
}

// populateWorkspace copies the run's host workspace into the pod's
// /workspace emptyDir by streaming a tar archive through `kubectl exec`.
// The kubernetes driver can't bind-mount (no host filesystem), so this is
// the "or copy" half of RunInfo.WorkspacePath's contract; Phase 5 V1 left
// /workspace empty (docs/sandbox.md).
//
// hostSrc is typically a git worktree whose `.git` is a *file* pointing
// back to the runner's clone — useless once detached from it. We copy the
// clone root instead (resolveCloneRoot), which carries a real `.git`
// (objects + the `origin` remote), so the sandboxed bot can commit and
// push (finalize_mr) against the real remote. The clone's checkout is the
// same base commit as the worktree.
func (r *Run) populateWorkspace(ctx context.Context, hostSrc, podDst string) error {
	src := resolveCloneRoot(ctx, hostSrc)
	r.driver.logger.Info("sandbox: copying workspace %s into pod %s:%s", src, r.podName, podDst)

	// Archive the worktree CONTENTS by name, never the "." root entry. A "./"
	// archive member makes the in-pod tar restore the source dir's mode+mtime
	// onto podDst itself — the pod's /workspace emptyDir, which is root-owned
	// (and setgid via fsGroup) — and the non-root sandbox user can't chmod/utime
	// it, so tar exits 2 ("Cannot change mode to …" / "Cannot utime") even though
	// every file extracted fine. Listing the top-level entries omits the root
	// member, so tar only ever creates files *inside* the existing /workspace.
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read workspace source %s: %w", src, err)
	}
	if len(entries) == 0 {
		return nil // nothing to copy
	}
	tarArgs := []string{"-C", src, "-cf", "-"}
	for _, e := range entries {
		tarArgs = append(tarArgs, e.Name())
	}
	hostTar := exec.CommandContext(ctx, "tar", tarArgs...)
	// --no-overwrite-dir stays as defence in depth for any pre-existing subdir.
	podTar := kubectlCmdContext(ctx, "--namespace", r.namespace,
		"exec", "-i", r.podName, "--", "tar", "-C", podDst, "--no-overwrite-dir", "-xf", "-")

	pipe, err := hostTar.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout pipe: %w", err)
	}
	podTar.Stdin = pipe
	var hostErr, podErr bytes.Buffer
	hostTar.Stderr = &hostErr
	podTar.Stderr = &podErr

	if err := podTar.Start(); err != nil {
		return fmt.Errorf("start in-pod tar: %w", err)
	}
	if err := hostTar.Run(); err != nil {
		_ = podTar.Wait()
		return fmt.Errorf("host tar %s: %w\n%s", src, err, strings.TrimSpace(hostErr.String()))
	}
	if err := podTar.Wait(); err != nil {
		return fmt.Errorf("in-pod tar extract: %w\n%s", err, strings.TrimSpace(podErr.String()))
	}
	return nil
}

// The pod workspace is a COPY of the host clone: it must travel back
// (exporter) and its final state must be verifiable (head capturer) —
// the pair is what lets the runner tell "no commits" from "the export
// lost them".
var (
	_ sandbox.WorkspaceExporter     = (*Run)(nil)
	_ sandbox.WorkspaceHeadCapturer = (*Run)(nil)
)

// CaptureWorkspaceHead implements [sandbox.WorkspaceHeadCapturer]: it
// reads the git HEAD of the pod-side workspace — the exact tree
// ExportWorkspace archives — so the engine can verify the export
// delivered the run's final state before the pod is destroyed.
func (r *Run) CaptureWorkspaceHead(ctx context.Context) (string, error) {
	if r.info.WorkspacePath == "" {
		return "", nil // workspace-less run — nothing was populated
	}
	res, err := r.Exec(ctx, []string{"git", "-C", r.prepared.workspace, "rev-parse", "HEAD"}, sandbox.ExecOpts{})
	if err != nil {
		return "", fmt.Errorf("pod-side git rev-parse HEAD: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("pod-side git rev-parse HEAD: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	head := strings.TrimSpace(string(res.Stdout))
	if head == "" {
		return "", fmt.Errorf("pod-side git rev-parse HEAD printed nothing")
	}
	return head, nil
}

// exportExcludes are the tar --exclude patterns for [Run.ExportWorkspace]
// (member names start with "./" because the in-pod tar archives "."):
//   - .git/config was re-pointed at POD paths by fixupWorkspaceGit — the
//     host clone's own config (host credential-store path) must survive;
//   - .git/iterion-credentials on the host is maintained LIVE by the
//     runner's rotation refresher — the pod copy may be staler and must
//     never overwrite it.
var exportExcludes = []string{"./.git/config", "./.git/iterion-credentials"}

// clearHostLooseRefs deletes the host clone's loose ref files so the
// pod's ref state arrives authoritative through the export extract.
//
// The tar overlay adds and overwrites but never deletes — and a pod-side
// `git gc` / `git pack-refs --all --prune` MOVES refs from loose files
// into .git/packed-refs. Git resolves loose before packed, so a stale
// host loose ref left in place would shadow the pod's packed value: the
// exported clone then reads a pre-run HEAD even though every object
// arrived, and the run's work is unreachable by ref. The pod copy always
// carries the authoritative refs (loose or packed, it was populated from
// this very clone), so the extract restores them; if the extract fails
// after this, the clone is loudly broken rather than silently stale —
// the banking guard refuses either way.
func clearHostLooseRefs(gitDir string) error {
	refsDir := filepath.Join(gitDir, "refs")
	entries, err := os.ReadDir(refsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(refsDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// ExportWorkspace implements [sandbox.WorkspaceExporter]: it streams the
// pod workspace back onto the host workspace it was populated from — the
// exact reverse of populateWorkspace (in-pod `tar -cf -` piped into a
// host-side extract), targeting the same resolved clone root. Without
// it, commits made inside the pod are destroyed with the pod and the
// host-side consumers (worktree finalization, recordRunGitMeta,
// Commits/Files diff capture) see the launch-time state.
//
// The overlay adds/overwrites — a file DELETED inside the pod's working
// tree keeps its host copy (tar has no delete semantics). For git REFS
// that would be a lie (see clearHostLooseRefs), so loose refs are
// cleared first and the pod's ref state lands authoritative; committed
// deletions are fully represented via the exported `.git`; only an
// uncommitted working-tree deletion is left behind, as an untracked
// leftover.
func (r *Run) ExportWorkspace(ctx context.Context) error {
	if r.info.WorkspacePath == "" {
		return nil // workspace-less run — nothing was populated
	}
	hostDst := resolveCloneRoot(ctx, r.info.WorkspacePath)
	r.driver.logger.Info("sandbox: exporting workspace from pod %s:%s back to %s", r.podName, r.prepared.workspace, hostDst)
	if err := clearHostLooseRefs(filepath.Join(hostDst, ".git")); err != nil {
		return fmt.Errorf("clear host loose refs before export extract: %w", err)
	}

	kubectlArgs := []string{"--namespace", r.namespace,
		"exec", r.podName, "--container", "workload", "--",
		"tar", "-C", r.prepared.workspace}
	for _, ex := range exportExcludes {
		kubectlArgs = append(kubectlArgs, "--exclude="+ex)
	}
	kubectlArgs = append(kubectlArgs, "-cf", "-", ".")
	podTar := kubectlCmdContext(ctx, kubectlArgs...)
	hostTar := exec.CommandContext(ctx, "tar", "-C", hostDst, "-xf", "-")

	pipe, err := podTar.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pod tar stdout pipe: %w", err)
	}
	hostTar.Stdin = pipe
	var podErr, hostErr bytes.Buffer
	podTar.Stderr = &podErr
	hostTar.Stderr = &hostErr

	if err := hostTar.Start(); err != nil {
		return fmt.Errorf("start host tar extract: %w", err)
	}
	if err := podTar.Run(); err != nil {
		_ = hostTar.Wait()
		return fmt.Errorf("in-pod tar %s: %w\n%s", r.prepared.workspace, err, strings.TrimSpace(podErr.String()))
	}
	if err := hostTar.Wait(); err != nil {
		return fmt.Errorf("host tar extract into %s: %w\n%s", hostDst, err, strings.TrimSpace(hostErr.String()))
	}
	return nil
}

// fixupWorkspaceGitScript re-anchors the copied clone's git plumbing on
// the IN-POD workspace path. Run via `sh -c` with the workspace as $1.
//
// Two host-path leftovers travel in with the tar copy and would break
// in-pod git otherwise:
//
//   - credential.helper: installGitCredentialStore (pkg/runner) wires
//     `store --file=<HOST clone abs path>/.git/iterion-credentials`.
//     For a worktree run the pod workspace lives at the WORKTREE path
//     while the copied content is the clone — so the recorded absolute
//     path doesn't exist in the pod and every authenticated push would
//     fail credential-less. Re-point it at the pod-local file.
//   - .git/worktrees/: the engine's per-run worktree registration.
//     Inside the pod the workspace is a standalone clone; the stale
//     registration claims the storage branch is checked out elsewhere
//     and blocks `git checkout` of that branch name. Remove it.
//
// `set -e` propagates a failing `git config` (e.g. no git in the image
// while the workspace carries a credential the run will need) as a hard
// error rather than deferring the breakage to push time.
const fixupWorkspaceGitScript = `set -e
ws=$1
if [ -d "$ws/.git" ]; then
  cred="$ws/.git/iterion-credentials"
  if [ -f "$cred" ]; then
    git -C "$ws" config credential.helper "store --file=$cred"
  fi
  rm -rf "$ws/.git/worktrees"
fi`

// fixupWorkspaceGit runs [fixupWorkspaceGitScript] inside the pod right
// after populateWorkspace. No-op for non-git workspaces.
func (r *Run) fixupWorkspaceGit(ctx context.Context, podWorkspace string) error {
	res, err := r.Exec(ctx, []string{"sh", "-c", fixupWorkspaceGitScript, "sh", podWorkspace}, sandbox.ExecOpts{})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// RefreshWorkspaceFile implements [sandbox.WorkspaceFileRefresher]: it
// atomically rewrites relPath under the pod workspace via the exec seam
// (value streamed over stdin, never argv). The kubernetes workspace is
// a tar COPY of the host workspace, so host-side rewrites — the
// runner's mid-run git-credential rotation — must be written through
// here to reach the pod; bind-mount drivers (docker) share the host
// inode and don't need (or implement) this.
func (r *Run) RefreshWorkspaceFile(ctx context.Context, relPath string, value []byte) error {
	target, err := workspaceFileTarget(r.prepared.workspace, relPath)
	if err != nil {
		return fmt.Errorf("kubernetes: refresh workspace file: %w", err)
	}
	if err := sandbox.WriteFileExec(ctx, r, target, value); err != nil {
		return fmt.Errorf("kubernetes: refresh workspace file %s: %w", relPath, err)
	}
	return nil
}

// workspaceFileTarget joins a workspace-relative path onto the pod
// workspace root, rejecting anything that could escape it.
func workspaceFileTarget(workspace, relPath string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("empty workspace")
	}
	if relPath == "" || path.IsAbs(relPath) || path.Clean(relPath) != relPath ||
		relPath == "." || relPath == ".." || strings.HasPrefix(relPath, "../") {
		return "", fmt.Errorf("relPath %q must be a clean workspace-relative file path", relPath)
	}
	if strings.ContainsAny(relPath, "\n\r\x00") {
		return "", fmt.Errorf("relPath %q contains a control character", relPath)
	}
	return path.Join(workspace, relPath), nil
}

// resolveCloneRoot returns the standalone clone directory for a host
// workspace. For a git worktree (whose `.git` is a pointer file) it
// returns the parent of the shared common git dir — the real clone the
// runner made. For a plain clone or a non-git dir it returns hostSrc
// unchanged. Best-effort: any git failure falls back to hostSrc.
func resolveCloneRoot(ctx context.Context, hostSrc string) string {
	cloneCmd := exec.CommandContext(ctx, "git", "-C", hostSrc,
		"rev-parse", "--path-format=absolute", "--git-common-dir")
	// -C names the workspace; an inherited GIT_DIR or GIT_COMMON_DIR would
	// answer about another repository and resolve the clone root to it.
	cloneCmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cloneCmd.Output()
	if err != nil {
		return hostSrc
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return hostSrc
	}
	// commonDir is "<cloneRoot>/.git"; its parent is the clone root.
	if root := filepath.Dir(commonDir); root != "" && root != "." {
		return root
	}
	return hostSrc
}

// podNameFor maps a run ID to a deterministic pod name. The k8s API
// caps name length at 253 chars, but DNS-1123 subdomain rules cap
// label segments at 63. We keep names well under that.
func podNameFor(runID string) string {
	// k8s names must be lowercase alphanumeric + dashes. New run IDs
	// are UUIDv7 strings (already DNS-1123 safe). Legacy IDs from
	// before the UUID switch had the form "run_<ms>" — lowercase
	// + underscore replacement covers both.
	n := toLowerASCII("iterion-run-" + runID)
	n = replaceUnderscores(n)
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}
