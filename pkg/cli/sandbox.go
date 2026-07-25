package cli

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterruntime "github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/registry"
)

// SandboxDoctorOptions tunes `iterion sandbox doctor`. The zero value
// reproduces the historical host-only report.
type SandboxDoctorOptions struct {
	// Strict turns on the pre-run validation battery (driver liveness,
	// image resolvability, k8s compatibility, network syntax, …) and
	// makes the command exit non-zero on any failure.
	Strict bool
	// File is an optional workflow (.bot/.botz/dir) whose sandbox:
	// block is resolved and validated. Empty runs host-level checks only.
	File string
	// Sandbox / SandboxDefaultImage / SandboxHostState mirror the
	// `iterion run` flags so the doctor validates the EXACT spec a run
	// with the same flags would use.
	Sandbox             string
	SandboxDefaultImage string
	SandboxHostState    string
	// Target selects the check battery: "auto" (follow the selected
	// driver, default), "cloud" (force the kubernetes/host-independent
	// battery — validate a cloud workflow from a laptop), or "local"
	// (force docker).
	Target string
}

// RunSandboxDoctor implements `iterion sandbox doctor`. Without
// [SandboxDoctorOptions.Strict] it prints the host-only report
// (unchanged from prior releases). With Strict it resolves the effective
// sandbox spec and runs the full pre-flight battery, returning a non-nil
// error (non-zero exit) when any check fails.
func RunSandboxDoctor(ctx context.Context, p *Printer, opts SandboxDoctorOptions) error {
	if opts.Strict {
		return runSandboxDoctorStrict(ctx, p, opts)
	}
	return runSandboxDoctorBasic(p)
}

// runSandboxDoctorBasic prints diagnostics about the sandbox subsystem.
//
// The output covers:
//
//   - the detected host kind (local / desktop / cloud) and how it was
//     determined (env var vs heuristic);
//   - which container runtime, if any, is on the user's PATH;
//   - the registered sandbox drivers in preference order;
//   - the driver the factory would pick for a run started from this
//     environment, plus its advertised capabilities;
//   - the global sandbox default (ITERION_SANDBOX_DEFAULT) — the lowest
//     precedence layer that all workflows inherit from.
//
// Output is human-readable by default; pass --json on the parent
// command to switch to JSON. Phase 0 prints stable string keys; Phase
// 1+ may extend the JSON shape — keys present today will not change.
func runSandboxDoctorBasic(p *Printer) error {
	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: registry.Default(),
	})

	driver, driverErr := factory.Driver()
	caps := sandbox.Capabilities{}
	driverName := "<none>"
	if driver != nil {
		driverName = driver.Name()
		caps = driver.Capabilities()
	}

	containerRuntime := sandbox.HostHasDocker()
	if containerRuntime == "" {
		containerRuntime = "<none>"
	}

	defaultMode := iterruntime.ResolveGlobalSandboxDefault()
	if strings.TrimSpace(os.Getenv("ITERION_SANDBOX_DEFAULT")) == "" {
		defaultMode += " (built-in sandbox-by-default)"
	}

	hostBin := proc.LocateIterionBinary()
	binLink := detectBinaryLinkage(hostBin)
	binPath := hostBin
	if binPath == "" {
		binPath = "<none>"
	}

	report := map[string]any{
		"host":                string(factory.Host()),
		"os":                  runtime.GOOS,
		"arch":                runtime.GOARCH,
		"container_runtime":   containerRuntime,
		"sandbox_default":     defaultMode,
		"selected_driver":     driverName,
		"available_drivers":   factory.Available(),
		"iterion_binary":      binPath,
		"iterion_binary_link": binLink.String(),
		"capabilities": map[string]bool{
			"image":          caps.SupportsImage,
			"build":          caps.SupportsBuild,
			"mounts":         caps.SupportsMounts,
			"network_policy": caps.SupportsNetworkPolicy,
			"post_create":    caps.SupportsPostCreate,
			"remote_user":    caps.SupportsRemoteUser,
		},
	}
	if driverErr != nil {
		report["driver_error"] = driverErr.Error()
	}

	if p.Format == OutputJSON {
		p.JSON(report)
		return nil
	}

	fmt.Fprintln(p.W, "iterion sandbox — doctor report")
	fmt.Fprintln(p.W, "===============================")
	fmt.Fprintf(p.W, "  host                : %s\n", report["host"])
	fmt.Fprintf(p.W, "  platform            : %s/%s\n", report["os"], report["arch"])
	fmt.Fprintf(p.W, "  container runtime   : %s\n", report["container_runtime"])
	fmt.Fprintf(p.W, "  ITERION_SANDBOX_DEFAULT : %s\n", report["sandbox_default"])
	fmt.Fprintf(p.W, "  available drivers   : %s\n", strings.Join(factory.Available(), ", "))
	fmt.Fprintf(p.W, "  selected driver     : %s\n", driverName)
	if driverErr != nil {
		fmt.Fprintf(p.W, "  driver error        : %v\n", driverErr)
	}
	fmt.Fprintln(p.W, "  capabilities:")
	fmt.Fprintf(p.W, "    image          : %v\n", caps.SupportsImage)
	fmt.Fprintf(p.W, "    build          : %v\n", caps.SupportsBuild)
	fmt.Fprintf(p.W, "    mounts         : %v\n", caps.SupportsMounts)
	fmt.Fprintf(p.W, "    network policy : %v\n", caps.SupportsNetworkPolicy)
	fmt.Fprintf(p.W, "    post create    : %v\n", caps.SupportsPostCreate)
	fmt.Fprintf(p.W, "    remote user    : %v\n", caps.SupportsRemoteUser)
	fmt.Fprintf(p.W, "  iterion binary      : %s (%s)\n", binPath, binLink)
	if w := staticBinaryWarning(binPath, binLink); w != "" {
		fmt.Fprintln(p.W)
		fmt.Fprint(p.W, w)
	}
	fmt.Fprintln(p.W)
	if driverName == "noop" {
		fmt.Fprintln(p.W, "Note: the noop driver is the safe fallback. Workflows that declare")
		fmt.Fprintln(p.W, "an active sandbox mode will run on the host with a sandbox_skipped event")
		fmt.Fprintln(p.W, "in events.jsonl. Install Docker or Podman locally to enable container")
		fmt.Fprintln(p.W, "isolation, or run iterion in cloud mode for k8s-native isolation.")
	}
	return nil
}

// binaryLinkage classifies how the host iterion binary is linked. It
// governs whether the binary can exec inside a sandbox container, where
// it is bind-mounted at /usr/local/bin/iterion (see
// runtime.addClawBinaryMount): a dynamically-linked build resolves its
// loader (e.g. the nix/glibc ld-linux) from the *host* filesystem, which
// is absent in the container, so the in-container `iterion __claw-runner`
// dies with `exec: ... no such file or directory`.
type binaryLinkage int

const (
	// linkUnknown — the binary could not be probed (empty path, unreadable,
	// or not an ELF object such as a macOS Mach-O). No warning is emitted:
	// the dynamic-loader failure mode is Linux/ELF-specific.
	linkUnknown binaryLinkage = iota
	// linkStatic — no PT_INTERP program header; the binary carries no
	// external loader dependency and execs cleanly inside the container.
	linkStatic
	// linkDynamic — a PT_INTERP program header names a host dynamic loader;
	// the binary will fail to exec inside a sandbox container.
	linkDynamic
)

func (l binaryLinkage) String() string {
	switch l {
	case linkStatic:
		return "static"
	case linkDynamic:
		return "dynamic"
	default:
		return "unknown"
	}
}

// detectBinaryLinkage reports whether the ELF binary at path is
// statically or dynamically linked. The signal is the PT_INTERP program
// header — present iff the binary defers to an external dynamic loader,
// which is exactly what determines whether it can exec inside a sandbox
// container. A non-ELF file (macOS Mach-O), an unreadable path, or an
// empty path yields linkUnknown so the doctor stays silent off-Linux
// rather than emitting a false-positive warning.
func detectBinaryLinkage(path string) binaryLinkage {
	if path == "" {
		return linkUnknown
	}
	f, err := elf.Open(path)
	if err != nil {
		return linkUnknown
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return linkDynamic
		}
	}
	return linkStatic
}

// staticBinaryWarning returns an operator-facing WARNING when the host
// iterion binary is dynamically linked, naming the in-container failure
// mode and the fix. It returns "" for static or unknown linkage so the
// message fires only when the failure is positively detected.
func staticBinaryWarning(path string, link binaryLinkage) string {
	if link != linkDynamic {
		return ""
	}
	return fmt.Sprintf(`  ⚠ WARNING: the host iterion binary is dynamically linked.
    %s is bind-mounted into sandbox containers at /usr/local/bin/iterion,
    but its dynamic loader (e.g. the nix/glibc ld-linux) is absent there —
    the in-container `+"`iterion __claw-runner`"+` will fail with
    `+"`exec: /usr/local/bin/iterion: no such file or directory`"+`.
    Fix: rebuild static, then reinstall —
      CGO_ENABLED=0 go build -o ./iterion ./cmd/iterion
    (or `+"`devbox run -- task build`"+`, which pins CGO_ENABLED=0).
`, path)
}
