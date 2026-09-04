package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/sandbox/registry"
)

// errStrictSandboxChecks is returned by the strict doctor when any check
// has status fail. It is a plain (non-[ErrUserInput]) error so the CLI
// exits 1 — a misconfigured host/spec, distinct from a usage error
// (bad flag / missing file → exit 2 via [UserInputError]).
var errStrictSandboxChecks = errors.New("sandbox doctor: strict checks failed")

// CheckStatus is the outcome of a single strict-doctor check.
type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// SandboxCheck is one line of the strict report: a named check, its
// status, an optional human detail, and a remediation hint shown only
// for non-pass results.
type SandboxCheck struct {
	Name        string      `json:"name"`
	Status      CheckStatus `json:"status"`
	Detail      string      `json:"detail,omitempty"`
	Remediation string      `json:"remediation,omitempty"`
}

// SandboxStrictReport is the full result of `iterion sandbox doctor
// --strict`. It is rendered human-readably or as JSON, and drives the
// process exit code via [SandboxStrictReport.Failed].
type SandboxStrictReport struct {
	Host   string         `json:"host"`
	Target string         `json:"target,omitempty"`
	Driver string         `json:"driver,omitempty"`
	Mode   string         `json:"mode"`
	Source string         `json:"source,omitempty"`
	Image  string         `json:"image,omitempty"`
	Checks []SandboxCheck `json:"checks"`
}

// Failed reports whether any check has status fail.
func (r *SandboxStrictReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return true
		}
	}
	return false
}

func (r *SandboxStrictReport) add(name string, status CheckStatus, detail, remediation string) {
	r.Checks = append(r.Checks, SandboxCheck{Name: name, Status: status, Detail: detail, Remediation: remediation})
}

// runSandboxDoctorStrict resolves the effective sandbox spec (host +
// optional workflow file + CLI/env overrides) and runs every applicable
// pre-flight check, returning [errStrictSandboxChecks] when any fails so
// the process exits non-zero. See [SandboxDoctorOptions].
func runSandboxDoctorStrict(ctx context.Context, p *Printer, opts SandboxDoctorOptions) error {
	// 1. Optional workflow → effective spec.
	var wf *ir.Workflow
	if opts.File != "" {
		loaded, err := loadWorkflowForDoctor(opts.File)
		if err != nil {
			return UserInputError(fmt.Errorf("sandbox doctor: %w", err))
		}
		wf = loaded
	}
	return renderStrict(p, buildStrictReport(ctx, wf, opts))
}

// buildStrictReport runs the full pre-flight battery for an
// already-loaded workflow (nil = host-only checks) and the resolved run
// options, returning the report WITHOUT rendering or deciding the exit
// code. Shared by the `--strict` doctor and the opt-in run/dispatch
// pre-flight hook (see [PreflightSandbox]) so both apply identical
// checks.
func buildStrictReport(ctx context.Context, wf *ir.Workflow, opts SandboxDoctorOptions) *SandboxStrictReport {
	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: registry.Default(),
	})
	report := &SandboxStrictReport{Host: string(factory.Host())}

	spec, source, err := runtime.ResolveSandboxSpecForDoctor(
		wf, doctorRepoRoot(opts.File), opts.Sandbox,
		runtime.ResolveGlobalSandboxDefault(),
		opts.SandboxDefaultImage, opts.SandboxHostState,
		strings.ToLower(os.Getenv("ITERION_SANDBOX_HOST_STATE")),
	)
	if err != nil {
		report.add("spec resolution", CheckFail, err.Error(),
			"fix the sandbox: block or --sandbox flag, then re-run the doctor")
		return report
	}
	if spec == nil || !spec.Mode.IsActive() {
		report.Mode = "none"
		report.add("sandbox configured", CheckWarn,
			"no active sandbox (mode none/inherit) — nothing to validate",
			"declare sandbox: auto|inline on the workflow, or pass --sandbox auto, to enable container isolation")
		return report
	}
	report.Mode = string(spec.Mode)
	report.Source = source
	report.Image = spec.Image

	// 2. Driver selection (active spec on a runtime-less host → fail).
	driver, driverErr := factory.DriverForSpec(spec)
	driverName := "<none>"
	if driver != nil {
		driverName = driver.Name()
	}
	report.Driver = driverName
	if driverErr != nil {
		// A runtime-less host (or one whose selected driver cannot serve
		// the forced target battery) fails driver selection. When the
		// operator explicitly asked to validate a spec for a different
		// host class via --target (e.g. `--target cloud` from a laptop),
		// local driver availability is not the thing being validated —
		// downgrade to a warning so a valid cross-host spec still exits 0.
		// See docs/adr/006-sandbox-strict-doctor.md.
		if crossHostDoctorValidation(opts.Target, driverName) {
			report.add("driver available", CheckWarn, driverErr.Error(),
				"no local container runtime; --target "+strings.ToLower(strings.TrimSpace(opts.Target))+
					" validates the spec for another host, so a local driver is not required here")
		} else {
			report.add("driver available", CheckFail, driverErr.Error(),
				"install Docker or Podman, or pass --sandbox-driver=noop to bypass (the run will NOT be isolated)")
		}
	} else {
		report.add("driver available", CheckPass, "selected driver: "+driverName, "")
	}
	runSchedulingPolicyStrictCheck(report, driver)

	// 3. Spec internal validity (pure, driver-agnostic).
	if vErr := spec.Validate(); vErr != nil {
		report.add("spec valid", CheckFail, vErr.Error(), "fix the sandbox: block; see docs/sandbox.md")
	} else {
		report.add("spec valid", CheckPass, "", "")
	}

	// 4–5. Driver-targeted checks.
	target := resolveDoctorTarget(opts.Target, driverName)
	report.Target = target
	switch target {
	case "docker":
		runDockerStrictChecks(ctx, report, spec)
	case "kubernetes":
		runK8sStrictChecks(ctx, report, spec, driverName, driver)
	}

	// 6. Network allowlist syntax (driver-agnostic).
	runNetworkStrictChecks(report, spec)

	// 7. Capability mismatches against the selected driver.
	if driver != nil && driver.Name() != "noop" {
		runCapabilityStrictChecks(report, spec, driver)
	}

	return report
}

// PreflightSandbox runs the strict pre-flight battery for an
// already-compiled workflow + the run's sandbox options, returning a
// [UserInputError] when any check fails so the caller can abort the run
// before booting the engine. It is the opt-in hook `iterion run` invokes
// (gated by ITERION_SANDBOX_PREFLIGHT) so a misconfigured sandbox
// surfaces in ~1s with an actionable hint instead of 30s into the run
// with a cryptic Docker/K8s error.
//
// Unlike the doctor command it does NOT print the full report — it logs
// each warning at warn level and each failure at error level, then
// returns a single aborting error pointing at `iterion sandbox doctor
// --strict` for the detail. Warnings never abort.
func PreflightSandbox(ctx context.Context, wf *ir.Workflow, opts SandboxDoctorOptions, logf func(status CheckStatus, c SandboxCheck)) error {
	report := buildStrictReport(ctx, wf, opts)
	if logf != nil {
		for _, c := range report.Checks {
			if c.Status != CheckPass {
				logf(c.Status, c)
			}
		}
	}
	if report.Failed() {
		hint := "iterion sandbox doctor --strict"
		if opts.File != "" {
			hint += " " + opts.File
		}
		return UserInputError(fmt.Errorf("sandbox pre-flight failed; run `%s` for the full report and remediation", hint))
	}
	return nil
}

// resolveDoctorTarget maps --target + the selected driver to the check
// battery to run. "cloud" forces the kubernetes (host-independent)
// battery so an operator can validate a cloud workflow from a laptop;
// "local" forces docker; "auto" follows the selected driver.
func resolveDoctorTarget(optTarget, driverName string) string {
	switch strings.ToLower(strings.TrimSpace(optTarget)) {
	case "cloud", "kubernetes", "k8s":
		return "kubernetes"
	case "local", "docker", "podman":
		return "docker"
	}
	switch driverName {
	case "docker", "podman":
		return "docker"
	case "kubernetes":
		return "kubernetes"
	}
	return ""
}

// isExplicitDoctorTarget reports whether --target names a concrete host
// class rather than "" / "auto" — i.e. the operator is deliberately
// validating a spec for a possibly-different host than this one. An
// explicit target is exactly one that resolveDoctorTarget maps to a
// concrete battery regardless of the selected driver, so we defer to it
// rather than re-listing the target name set.
func isExplicitDoctorTarget(optTarget string) bool {
	return resolveDoctorTarget(optTarget, "") != ""
}

// crossHostDoctorValidation reports whether the strict "driver available"
// failure should be downgraded to a warning: an explicit --target points
// at a host class this host's selected driver does not naturally serve
// (or this host has no driver at all), so local runtime availability is
// not what is being validated. Without an explicit --target, a
// runtime-less host is a genuine local misconfiguration and still fails.
func crossHostDoctorValidation(optTarget, driverName string) bool {
	if !isExplicitDoctorTarget(optTarget) {
		return false
	}
	if driverName == "" || driverName == "<none>" {
		// Runtime-less host: any explicit target is cross-host by definition.
		return true
	}
	return resolveDoctorTarget(optTarget, driverName) != resolveDoctorTarget("", driverName)
}

func runDockerStrictChecks(ctx context.Context, report *SandboxStrictReport, spec *sandbox.Spec) {
	pingCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout())
	defer cancel()
	if ver, err := docker.PingDaemon(pingCtx); err != nil {
		report.add("docker daemon", CheckFail, err.Error(),
			"start the daemon (Docker Desktop, `sudo systemctl start docker`, colima/orbstack) then re-run")
	} else {
		report.add("docker daemon", CheckPass, "server version "+ver, "")
	}

	if mErr := docker.ValidateSpecMounts(*spec); mErr != nil {
		report.add("spec safety", CheckFail, mErr.Error(),
			"remove the offending bind (docker.sock, host credentials, /proc, /sys), fix the mount string, or sanitise the env var")
	} else {
		report.add("spec safety", CheckPass, "", "")
	}

	switch {
	case spec.Build != nil:
		report.add("image resolvable", CheckPass,
			"build mode: image is built at run start (no registry ref to resolve)", "")
	case spec.Image == "":
		// spec.Validate already flagged the missing image; nothing to do.
	default:
		imgCtx, cancel2 := context.WithTimeout(ctx, doctorProbeTimeout())
		defer cancel2()
		if rErr := docker.ResolveImageRef(imgCtx, spec.Image); rErr != nil {
			var ire *docker.ImageResolveError
			if errors.As(rErr, &ire) && ire.Transient {
				report.add("image resolvable", CheckWarn, rErr.Error(),
					"could not verify offline (registry auth/network); the run may still pull with daemon-side credentials")
			} else {
				report.add("image resolvable", CheckFail, rErr.Error(),
					"check the ref/tag exists and is reachable (try: docker manifest inspect "+spec.Image+")")
			}
		} else {
			report.add("image resolvable", CheckPass, spec.Image, "")
		}
	}
}

// runSchedulingPolicyStrictCheck reports the scheduling policy a driver
// stamps on its sandboxes. The driver parses it from the environment at
// construction and, when it is malformed, fails every run at sandbox start
// — never the constructor, which the factory would skip in silence. The
// doctor is where that error must show before the first run pays for it.
func runSchedulingPolicyStrictCheck(report *SandboxStrictReport, driver sandbox.Driver) {
	r, ok := driver.(sandbox.SchedulingPolicyReporter)
	if !ok {
		return
	}
	policy, err := r.SchedulingPolicy()
	if err != nil {
		report.add("sandbox scheduling policy", CheckFail, err.Error(),
			"fix the ITERION_SANDBOX_K8S_* value in the runner environment (chart runner.sandbox.scheduling — not config.extraEnv: its ConfigMap value loses to the literal PodTemplate env) — every run would fail at sandbox start")
		return
	}
	report.add("sandbox scheduling policy", CheckPass, policy, "")
}

func runK8sStrictChecks(ctx context.Context, report *SandboxStrictReport, spec *sandbox.Spec, driverName string, driver sandbox.Driver) {
	if vErr := kubernetes.ValidateSpec(*spec); vErr != nil {
		report.add("k8s spec compatible", CheckFail, vErr.Error(),
			"for cloud: pin sandbox.image (no build:), set host_state: none, set a numeric sandbox.user")
	} else {
		report.add("k8s spec compatible", CheckPass, "", "")
	}

	pingCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout())
	defer cancel()
	kctx, ns, err := kubernetes.PingContext(pingCtx)
	if err != nil {
		status, remediation := CheckWarn, "k8s context is only needed where the cloud runner executes; this host is not a runner"
		if driverName == "kubernetes" {
			// The selected driver IS kubernetes (in-cluster /
			// ITERION_MODE=cloud) → a missing/unreachable context is fatal.
			status = CheckFail
			remediation = "select a context (`kubectl config use-context <name>`) or verify the in-cluster service account + API reachability"
		}
		report.add("k8s context", status, err.Error(), remediation)
		return
	}
	detail := "context: " + kctx
	if ns != "" {
		detail += " (namespace: " + ns + ")"
	}
	report.add("k8s context", CheckPass, detail, "")

	// A spread over a topology key the nodes do not carry excludes those
	// nodes from scheduling — a soft constraint does not waive the label —
	// and a key no node carries leaves every run Pending. Only a reachable
	// cluster can answer, hence after the ping.
	if kd, ok := driver.(*kubernetes.Driver); ok {
		runSpreadCoverageStrictCheck(ctx, report, kd.SpreadTopologyKey())
	}
}

// runSpreadCoverageStrictCheck compares the nodes carrying the spread key to
// the cluster's nodes. kubernetes.io/hostname needs no check: every node
// carries it by construction. Listing nodes is cluster-scoped and the
// chart's runner Role is namespace-scoped on purpose, so in-cluster the
// check cannot answer: it warns with kubectl's reason and hands the
// operator the command to run from a context that can.
func runSpreadCoverageStrictCheck(ctx context.Context, report *SandboxStrictReport, key string) {
	if key == "" || key == "kubernetes.io/hostname" {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout())
	defer cancel()
	labelled, total, err := kubernetes.NodeLabelCoverage(probeCtx, key)
	switch {
	case err != nil:
		report.add("k8s spread key coverage", CheckWarn, err.Error(),
			"could not list nodes (the chart's runner Role is namespace-scoped and grants no nodes/list); from an operator context run `kubectl get nodes -L "+key+"` and check every schedulable node carries the label")
	case total == 0:
		report.add("k8s spread key coverage", CheckWarn, "no node visible", "verify RBAC on nodes/list, then that every schedulable node carries the label "+key)
	case labelled == 0:
		report.add("k8s spread key coverage", CheckFail, fmt.Sprintf("no node carries %s (%d nodes)", key, total),
			"every run pod would stay Pending — set ITERION_SANDBOX_K8S_SPREAD to hostname or to a label the nodes carry")
	case labelled < total:
		report.add("k8s spread key coverage", CheckWarn, fmt.Sprintf("%d of %d nodes carry %s", labelled, total, key),
			"nodes without the label are excluded from run pod scheduling; label them or use hostname")
	default:
		report.add("k8s spread key coverage", CheckPass, fmt.Sprintf("%d of %d nodes carry %s", labelled, total, key), "")
	}
}

func runNetworkStrictChecks(report *SandboxStrictReport, spec *sandbox.Spec) {
	if spec.Network != nil && spec.Network.Preset != "" {
		if _, ok := netproxy.PresetRules(spec.Network.Preset); !ok {
			report.add("network preset", CheckFail,
				fmt.Sprintf("unknown network preset %q", spec.Network.Preset),
				"use a known preset (iterion-default) or drop preset: and list rules: explicitly")
		} else {
			report.add("network preset", CheckPass, spec.Network.Preset, "")
		}
	}

	mode, rules := runtime.ResolveNetworkPolicy(spec)
	if _, err := netproxy.Compile(mode, rules); err != nil {
		report.add("network allowlist syntax", CheckFail, err.Error(),
			"fix the offending rule: wildcards must lead a label (**.example.com), CIDRs must parse, one wildcard segment per rule")
	} else {
		report.add("network allowlist syntax", CheckPass,
			fmt.Sprintf("mode=%s, %d rule(s)", mode, len(rules)), "")
	}
}

func runCapabilityStrictChecks(report *SandboxStrictReport, spec *sandbox.Spec, driver sandbox.Driver) {
	caps := driver.Capabilities()
	var problems []string
	if spec.Build != nil && !caps.SupportsBuild {
		problems = append(problems, fmt.Sprintf("driver %q does not support sandbox.build", driver.Name()))
	}
	if len(spec.Mounts) > 0 && !caps.SupportsMounts {
		problems = append(problems, fmt.Sprintf("driver %q does not support extra mounts", driver.Name()))
	}
	if spec.User != "" && !caps.SupportsRemoteUser {
		problems = append(problems, fmt.Sprintf("driver %q does not support a remote user", driver.Name()))
	}
	if spec.PostCreate != "" && !caps.SupportsPostCreate {
		problems = append(problems, fmt.Sprintf("driver %q does not support postCreate", driver.Name()))
	}
	if len(problems) > 0 {
		report.add("driver capabilities", CheckFail, strings.Join(problems, "; "),
			"choose a driver that supports the requested features, or drop them from the spec")
	} else {
		report.add("driver capabilities", CheckPass, "", "")
	}
}

// renderStrict prints the report (human or JSON) and returns
// [errStrictSandboxChecks] when any check failed.
func renderStrict(p *Printer, report *SandboxStrictReport) error {
	if p.Format == OutputJSON {
		p.JSON(report)
		if report.Failed() {
			return errStrictSandboxChecks
		}
		return nil
	}

	fmt.Fprintln(p.W, "iterion sandbox — strict doctor")
	fmt.Fprintln(p.W, "===============================")
	fmt.Fprintf(p.W, "  host    : %s\n", report.Host)
	if report.Target != "" {
		fmt.Fprintf(p.W, "  target  : %s\n", report.Target)
	}
	if report.Driver != "" {
		fmt.Fprintf(p.W, "  driver  : %s\n", report.Driver)
	}
	fmt.Fprintf(p.W, "  mode    : %s\n", report.Mode)
	if report.Source != "" {
		fmt.Fprintf(p.W, "  source  : %s\n", report.Source)
	}
	if report.Image != "" {
		fmt.Fprintf(p.W, "  image   : %s\n", report.Image)
	}
	fmt.Fprintln(p.W)

	var pass, warn, fail int
	for _, c := range report.Checks {
		icon := "[pass]"
		switch c.Status {
		case CheckWarn:
			icon, warn = "[warn]", warn+1
		case CheckFail:
			icon, fail = "[FAIL]", fail+1
		default:
			pass++
		}
		fmt.Fprintf(p.W, "  %s %s\n", icon, c.Name)
		if c.Detail != "" {
			fmt.Fprintf(p.W, "         %s\n", c.Detail)
		}
		if c.Status != CheckPass && c.Remediation != "" {
			fmt.Fprintf(p.W, "         hint: %s\n", c.Remediation)
		}
	}
	fmt.Fprintln(p.W)
	fmt.Fprintf(p.W, "  summary: %d passed, %d warning(s), %d failure(s)\n", pass, warn, fail)
	if report.Failed() {
		return errStrictSandboxChecks
	}
	return nil
}

// sandboxPreflightEnabled reports whether the opt-in run/dispatch
// sandbox pre-flight is on, via ITERION_SANDBOX_PREFLIGHT
// (1/true/yes/on). Off by default — the strict battery shells out to the
// daemon/registry, so we don't pay that latency on every run unless the
// operator opts in.
func sandboxPreflightEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_SANDBOX_PREFLIGHT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// doctorProbeTimeout caps each shell-out probe so a hung daemon/registry
// surfaces fast. Override via ITERION_SANDBOX_DOCTOR_TIMEOUT (Go duration).
func doctorProbeTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ITERION_SANDBOX_DOCTOR_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

// doctorRepoRoot resolves the repo root the spec resolver should treat as
// the source of truth (for mode=auto devcontainer lookup + ${localEnv}
// expansion). Mirrors the engine's engineRepoRoot precedence: the main
// repo via the git pointer, else the absolute dir.
func doctorRepoRoot(file string) string {
	dir := "."
	if file != "" {
		dir = filepath.Dir(file)
	}
	if root := git.FindMainRepoRoot(dir); root != "" {
		return root
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// loadWorkflowForDoctor resolves a .bot file or a .botz/dir bundle
// to its compiled IR workflow so the doctor can read its sandbox: block.
// It detects + opens the bundle (the part RunValidate/run.go also do)
// and then delegates to the shared runview compile pipeline, so the
// doctor compiles a workflow IDENTICALLY to how `iterion run` and
// `iterion validate` do — same parse/merge/compile/MCP-prep path, no
// drift.
func loadWorkflowForDoctor(path string) (*ir.Workflow, error) {
	path = ResolveRecipePath(path)

	b, iterPath, kind, cleanup, err := openBundleOrFile(path)
	defer func() { _ = cleanup() }()
	if err != nil {
		switch kind {
		case bundle.KindBundle:
			return nil, fmt.Errorf("cannot open bundle: %w", err)
		case bundle.KindBundleDir:
			return nil, fmt.Errorf("cannot open bundle dir: %w", err)
		default:
			return nil, fmt.Errorf("cannot inspect %s: %w", path, err)
		}
	}
	if b != nil {
		wf, _, cErr := runview.CompileBundleWorkflow(iterPath, b)
		return wf, cErr
	}
	return runview.CompileWorkflow(path)
}
