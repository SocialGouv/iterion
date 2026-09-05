package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
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

// DefaultWorkspaceCopyTimeout bounds populateWorkspace (host tar |
// kubectl exec pod tar) and the git fixup that follows, each end-to-end.
// The pod-Ready wait has its own cap; without this one a stuck
// kubectl-exec pipe blocks the run until the outer max_duration fires —
// hours later, with no `sandbox_started` event to warn on (a resumed
// review sat 2h 26m in the sandbox-start phase, was wiped by a runner
// rollout, and its message re-delivered onto a stale `running` status —
// #669 part 1).
//
// Fifteen minutes because no measured copy duration exists to size it
// from: iterion's own checkout is 355 MB streamed through the apiserver,
// and a cold copy on a slow apiserver must not be killed for being slow
// but healthy. The halfway warning in runWithPhaseTimeout makes such a
// copy visible at 7m30s, before the bound strikes. Overridable per host
// via ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT (a Go duration).
const DefaultWorkspaceCopyTimeout = 15 * time.Minute

// workspaceCopyTimeoutEnv is the override key. Read once per call so
// tests can flip it between subcases.
const workspaceCopyTimeoutEnv = "ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT"

// workspaceCopyTimeoutWarnOnce bounds the "unparseable override" warning
// to one stderr line per process (the ITERION_BUDGET_EXIT_GRACE
// convention): an operator input silently replaced by the default is an
// override that never took, with nothing saying so.
var workspaceCopyTimeoutWarnOnce sync.Once

// resolveWorkspaceCopyTimeout returns the effective per-phase timeout,
// honouring the env override with a fail-safe fallback: a garbage or
// non-positive value ("banana", "0", "-5m", or "5" — which Go parses as
// five NANOseconds, not the five minutes the operator meant) falls back
// to the default (a copy phase left unbounded is exactly the bug this
// exists to close) and warns once, naming the value and the default.
func resolveWorkspaceCopyTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(workspaceCopyTimeoutEnv))
	if raw == "" {
		return DefaultWorkspaceCopyTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		workspaceCopyTimeoutWarnOnce.Do(func() {
			// Stderr, not the driver logger: a leaf helper with no
			// logger in reach.
			fmt.Fprintf(os.Stderr,
				"iterion: %s=%q is not a positive Go duration (use e.g. 5m, 15m, 2h) — using the default %s\n",
				workspaceCopyTimeoutEnv, raw, DefaultWorkspaceCopyTimeout)
		})
		return DefaultWorkspaceCopyTimeout
	}
	return d
}

// phaseTimeoutWarnRatio is where the halfway-mark warning fires, as a
// fraction of the phase budget: a slow-but-healthy copy shows up on the
// runner log BEFORE the bound strikes, with enough runway left to tell
// "clogged and finishing" from "wedged". A var so tests can lower it.
var phaseTimeoutWarnRatio = 0.5

// runWithPhaseTimeout runs fn under a bounded child context. When THIS
// phase's deadline strikes it returns an error naming the phase and the
// wall-clock elapsed time, wrapping sandbox.ErrPhaseTimeout,
// context.DeadlineExceeded AND fn's own error through errors.Join, so
// every consumer can classify the shape with errors.Is (the setup
// classifier routes it to failed_resumable, the runner NAKs it) and the
// operator still reads the actual cause. An outer ctx cancellation (run
// cancel, pod SIGTERM) keeps its own shape: a cooperative stop is not a
// stall.
//
// The bound is only as strong as fn's ctx discipline. fn is called
// synchronously and nothing races the deadline: the child ctx expires,
// and whatever fn does with that is the whole enforcement. For the
// workspace copy that is exec.CommandContext killing the LOCAL tar and
// kubectl processes; the in-pod tar behind `kubectl exec` is not reached
// by that signal (Setpgid signals only the leader), it dies with the
// pipe. A callee that ignores its ctx and returns nil after the deadline
// therefore completes — and is warned about, so a phase that burned its
// whole budget and won by a hair is visible before the next occurrence
// trips the bound.
//
// The halfway warning (phaseTimeoutWarnRatio × timeout) runs on a side
// goroutine that fn's return cancels; on an early return it emits
// nothing.
func runWithPhaseTimeout(ctx context.Context, logger *iterlog.Logger, phase string, timeout time.Duration, fn func(context.Context) error) error {
	if logger == nil {
		logger = iterlog.Nop()
	}
	phaseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	// The warn delay is computed here, not in the goroutine, so the
	// ratio is read before the goroutine exists (tests lower it).
	warnAfter := time.Duration(float64(timeout) * phaseTimeoutWarnRatio)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(warnAfter):
			logger.Warn("sandbox: %s phase still running after %s of its %s budget — a stall from here on fails the phase (ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT raises the bound)",
				phase, time.Since(start).Round(time.Second), timeout)
		}
	}()

	err := fn(phaseCtx)
	close(done)

	if err == nil {
		if phaseCtx.Err() != nil {
			logger.Warn("sandbox: %s phase completed at or past its %s budget (elapsed %s) — raise ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT or investigate the delay",
				phase, timeout, time.Since(start).Round(time.Millisecond))
		}
		return nil
	}
	if phaseCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("kubernetes: %s phase timed out after %s (deadline %s exceeded): %w",
			phase, time.Since(start).Round(time.Millisecond), timeout,
			errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, err))
	}
	return err
}

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

// Scheduling knobs of the sibling pod, read from the runner's environment
// once at construction and rendered on every pod it creates. They are a
// deployment policy, not a workflow's: the operator sizes what one run may
// claim, and a bot cannot lower it.
//
// A pod that requests nothing scores every node the same, so the scheduler
// packs a campaign's runs onto whichever node already holds the image
// (measured: 5 of 6 run pods on one 8-core worker at 89 % CPU while two
// workers idled, and an oracle's 300 s boot budget blown at 459 s). The
// request is what makes LeastAllocated spread them and what a cluster
// autoscaler sizes the pool on; the spread constraint steers what the
// request leaves equal. Unset → no `resources` (the manifest of a driver
// without the knobs).
const (
	RequestsCPUEnvVar    = "ITERION_SANDBOX_K8S_REQUESTS_CPU"
	RequestsMemoryEnvVar = "ITERION_SANDBOX_K8S_REQUESTS_MEMORY"
	LimitsCPUEnvVar      = "ITERION_SANDBOX_K8S_LIMITS_CPU"
	LimitsMemoryEnvVar   = "ITERION_SANDBOX_K8S_LIMITS_MEMORY"
	// SpreadEnvVar selects the topology key of the soft spread constraint
	// over sandbox-run pods: unset / "none" / "off" → no constraint (the
	// scheduler's own policy decides, as it always did), "hostname" →
	// kubernetes.io/hostname, any other value must be a prefixed label key
	// (e.g. topology.kubernetes.io/zone) that the nodes actually carry —
	// nodes without the label are excluded from scheduling, soft or not.
	SpreadEnvVar = "ITERION_SANDBOX_K8S_SPREAD"
)

// hostnameTopologyKey is the node-level spread, the one key every node
// carries by construction.
const hostnameTopologyKey = "kubernetes.io/hostname"

// quantityRe is the subset of the Kubernetes quantity grammar the driver
// accepts: a decimal (`2`, `.5`, `1.`, `+1`), an optional exponent (`1e3`)
// or one of the SI / binary suffixes operators actually write for CPU and
// memory (`500m`, `4Gi`). The micro/nano suffixes (`u`, `n`) the API also
// admits are left out on purpose — no run is sized in nanocores. The API
// server owns the rest of the semantics; the driver refuses what could never
// be a sane quantity, so a typo fails once at startup instead of on every
// pod apply.
var quantityRe = regexp.MustCompile(`^\+?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+|m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$`)

// suffixScale is the multiplier of each quantity suffix, for the
// request ≤ limit comparison.
var suffixScale = map[string]float64{
	"m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// splitQuantity separates a quantity matched by quantityRe into its numeric
// text and its suffix ("" when none; an exponent counts as part of the
// number).
func splitQuantity(v string) (number, suffix string) {
	v = strings.TrimPrefix(v, "+")
	// Scanned from the right: the number ends at its last digit or dot, and a
	// valid exponent ends in a digit too (`1e3`, `1e+3`), so it stays inside
	// the number; `E` and `Ei` end in a letter and are suffixes.
	for i := len(v) - 1; i >= 0; i-- {
		if c := v[i]; (c >= '0' && c <= '9') || c == '.' {
			return v[:i+1], v[i+1:]
		}
	}
	return "", v
}

// quantityValue converts a quantity matched by quantityRe to a float for the
// zero check and for comparisons (a request against its limit). Precision is
// irrelevant here: the API server re-parses the strings themselves. A value
// it cannot evaluate (an exponent out of float64 range, an unknown suffix)
// is an error, never a silent zero; a value that evaluates to zero (`0`,
// `0.0`, `0Gi`, an underflowing `1e-400`) is the caller's to refuse.
func quantityValue(v string) (float64, error) {
	number, suffix := splitQuantity(v)
	f, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity %q: %w", v, err)
	}
	if suffix != "" {
		scale, ok := suffixScale[suffix]
		if !ok {
			return 0, fmt.Errorf("quantity %q: unknown suffix %q", v, suffix)
		}
		f *= scale
	}
	if math.IsInf(f, 0) {
		return 0, fmt.Errorf("quantity %q: out of range", v)
	}
	return f, nil
}

// topologyKeyRe is a prefixed Kubernetes label key (DNS-subdomain prefix,
// "/", then the name). A bare word is refused on purpose: every topology
// key a cluster actually carries is prefixed (kubernetes.io/hostname,
// topology.kubernetes.io/zone, …), and a bare word is almost always a typo
// of the keywords — which the API server would accept as a label no node
// has, turning the spread into a silent no-op.
var topologyKeyRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// podScheduling is the parsed form of the scheduling env vars.
type podScheduling struct {
	resources PodResources
	spreadKey string // "" = no spread constraint
}

// String renders the policy for logs, events and the doctor report.
func (s podScheduling) String() string {
	var parts []string
	side := func(label string, l ResourceList) {
		var kv []string
		if l.CPU != "" {
			kv = append(kv, "cpu="+l.CPU)
		}
		if l.Memory != "" {
			kv = append(kv, "memory="+l.Memory)
		}
		if len(kv) > 0 {
			parts = append(parts, label+" "+strings.Join(kv, " "))
		}
	}
	side("requests", s.resources.Requests)
	side("limits", s.resources.Limits)
	if len(parts) == 0 {
		parts = append(parts, "no resources")
	}
	if s.spreadKey == "" {
		parts = append(parts, "no spread")
	} else {
		parts = append(parts, "spread="+s.spreadKey)
	}
	return strings.Join(parts, ", ")
}

// ValidateSchedulingEnv parses the scheduling policy from the process
// environment and returns the error every sandbox Start would return. The
// runner calls it at bootstrap so a misconfigured pod never becomes ready —
// the driver factory skips constructor errors, so this is the only place a
// bad value can stop a rollout instead of failing runs one by one.
func ValidateSchedulingEnv() error {
	_, err := schedulingFromEnv(os.Getenv)
	return err
}

// schedulingFromEnv parses the scheduling env vars through getenv (injected
// so tests never touch the process environment). Every set quantity must
// match quantityRe, be non-zero and use a suffix that makes sense for its
// resource; a limit needs its request (the API server would otherwise copy
// the limit into the request at admission) and must not be below it; the
// spread keywords are matched case-insensitively and anything else must be
// a prefixed label key. Each error names the variable and the value.
func schedulingFromEnv(getenv func(string) string) (podScheduling, error) {
	var s podScheduling
	quantities := []struct {
		env    string
		dst    *string
		memory bool
	}{
		{RequestsCPUEnvVar, &s.resources.Requests.CPU, false},
		{RequestsMemoryEnvVar, &s.resources.Requests.Memory, true},
		{LimitsCPUEnvVar, &s.resources.Limits.CPU, false},
		{LimitsMemoryEnvVar, &s.resources.Limits.Memory, true},
	}
	for _, q := range quantities {
		v := strings.TrimSpace(getenv(q.env))
		if v == "" {
			continue
		}
		if !quantityRe.MatchString(v) {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is not a Kubernetes quantity (want e.g. 500m, 2, 4Gi)", q.env, v)
		}
		// Evaluated for every set variable, not only when a limit exists: a
		// zero (`0`, `0Gi`, an underflowing `1e-400`) renders a resources block
		// that schedules exactly like no resources block, which is the one
		// thing the policy exists to prevent — an operator reading the pod
		// would believe the floor is set.
		value, err := quantityValue(v)
		if err != nil {
			return podScheduling{}, fmt.Errorf("kubernetes: %s: %w", q.env, err)
		}
		if value == 0 {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is a zero quantity — it schedules like no request at all; unset the variable instead", q.env, v)
		}
		_, suffix := splitQuantity(v)
		if q.memory && suffix == "m" {
			// `400m` of memory is 0.4 bytes — always the CPU suffix on the
			// wrong variable, never a size anyone meant.
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is a milli-byte quantity (m is the CPU suffix; want e.g. 512Mi, 4Gi)", q.env, v)
		}
		if !q.memory && len(suffix) == 2 {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q uses a byte suffix on a CPU quantity (want e.g. 500m, 2)", q.env, v)
		}
		// Below Kubernetes' own precision (1m of CPU, 1 byte of memory) the
		// API server rounds up at admission: a "floor" of 5e-324 is a zero
		// wearing a number.
		if floor := 1.0; (!q.memory && value < 1e-3) || (q.memory && value < floor) {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is below the quantity precision (1m of CPU, 1 byte of memory) — it schedules like no request at all", q.env, v)
		}
		*q.dst = v
	}
	for _, pair := range []struct {
		name, reqEnv, limEnv, req, lim string
	}{
		{"cpu", RequestsCPUEnvVar, LimitsCPUEnvVar, s.resources.Requests.CPU, s.resources.Limits.CPU},
		{"memory", RequestsMemoryEnvVar, LimitsMemoryEnvVar, s.resources.Requests.Memory, s.resources.Limits.Memory},
	} {
		if pair.lim == "" {
			continue
		}
		if pair.req == "" {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q without %s — the API server would copy the limit into the request at admission; set the request explicitly", pair.limEnv, pair.lim, pair.reqEnv)
		}
		lim, err := quantityValue(pair.lim)
		if err != nil {
			return podScheduling{}, fmt.Errorf("kubernetes: %s: %w", pair.limEnv, err)
		}
		req, err := quantityValue(pair.req)
		if err != nil {
			return podScheduling{}, fmt.Errorf("kubernetes: %s: %w", pair.reqEnv, err)
		}
		if lim < req {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is below %s=%q — a %s limit cannot be lower than its request", pair.limEnv, pair.lim, pair.reqEnv, pair.req, pair.name)
		}
	}
	v := strings.TrimSpace(getenv(SpreadEnvVar))
	switch strings.ToLower(v) {
	case "", "none", "off":
		s.spreadKey = ""
	case "hostname":
		s.spreadKey = hostnameTopologyKey
	default:
		if !topologyKeyRe.MatchString(v) {
			return podScheduling{}, fmt.Errorf("kubernetes: %s=%q is not a topology key (want hostname, none, or a prefixed label key such as topology.kubernetes.io/zone)", SpreadEnvVar, v)
		}
		s.spreadKey = v
	}
	return s, nil
}

// SpreadTopologyKey is the topology key of the spread constraint the driver
// renders, "" when it renders none. The doctor uses it to check that the
// cluster's nodes carry the label — a node without it is excluded from
// scheduling, which a soft constraint does not waive.
func (d *Driver) SpreadTopologyKey() string { return d.sched.spreadKey }

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
	// A malformed scheduling value is kept on the driver and fails every
	// Start, not the constructor: the factory's preference walk skips ANY
	// constructor error and ends on the always-constructible noop driver,
	// so returning it here would degrade cloud runs to unsandboxed with a
	// warning event as the only trace. Failing each run with the variable
	// named is the loud path; `iterion sandbox doctor` reads the same
	// error through SchedulingPolicy.
	sched, schedErr := schedulingFromEnv(os.Getenv)
	return &Driver{
		kubectl:   binPath,
		namespace: namespace,
		logger:    iterlog.New(iterlog.LevelInfo, io.Discard),
		sched:     sched,
		schedErr:  schedErr,
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

	// sched is the deployment's pod scheduling policy (requests, limits,
	// spread), parsed once from the environment; schedErr is the parse
	// failure it carries, surfaced by Start on every run.
	sched    podScheduling
	schedErr error
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

// SchedulingPolicy implements [sandbox.SchedulingPolicyReporter]: the
// deployment's pod scheduling policy as rendered on every sibling pod, or
// the parse error every Start will return.
func (d *Driver) SchedulingPolicy() (string, error) {
	if d.schedErr != nil {
		return "", d.schedErr
	}
	return d.sched.String(), nil
}

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
	if d.schedErr != nil {
		return nil, d.schedErr
	}
	d.logger.Info("kubernetes: pod scheduling policy: %s", d.sched)

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
		Resources:             d.sched.resources,
		SpreadTopologyKey:     d.sched.spreadKey,
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
	//
	// Both phases are BOUNDED (the pod-Ready wait has its own cap,
	// DefaultPodReadyTimeoutSecs): a stuck kubectl-exec pipe must fail the
	// phase as a typed error, not block the run until the outer
	// max_duration fires. resolveWorkspaceCopyTimeout is the budget
	// (DefaultWorkspaceCopyTimeout; ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT
	// overrides).
	if info.WorkspacePath != "" {
		copyTimeout := resolveWorkspaceCopyTimeout()
		if err := runWithPhaseTimeout(ctx, d.logger, "workspace copy", copyTimeout, func(ctx context.Context) error {
			return r.populateWorkspace(ctx, info.WorkspacePath, p.workspace)
		}); err != nil {
			_ = r.Cleanup(ctx)
			return nil, fmt.Errorf("kubernetes: populate workspace: %w", err)
		}
		if err := runWithPhaseTimeout(ctx, d.logger, "workspace git fixup", copyTimeout, func(ctx context.Context) error {
			return r.fixupWorkspaceGit(ctx, p.workspace)
		}); err != nil {
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
