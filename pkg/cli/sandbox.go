package cli

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
)

// RunSandboxDoctor prints diagnostics about the sandbox subsystem.
// It is the implementation of `iterion sandbox doctor`.
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
func RunSandboxDoctor(p *Printer) error {
	factory := sandbox.NewFactory(sandbox.FactoryOptions{
		AvailableDrivers: defaultDriverRegistry(),
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

	defaultMode := strings.ToLower(os.Getenv("ITERION_SANDBOX_DEFAULT"))
	if defaultMode == "" {
		defaultMode = "<unset>"
	}

	report := map[string]any{
		"host":              string(factory.Host()),
		"os":                runtime.GOOS,
		"arch":              runtime.GOARCH,
		"container_runtime": containerRuntime,
		"sandbox_default":   defaultMode,
		"selected_driver":   driverName,
		"available_drivers": factory.Available(),
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
	fmt.Fprintln(p.W)
	if driverName == "noop" {
		fmt.Fprintln(p.W, "Note: the noop driver is the safe fallback. Workflows that declare")
		fmt.Fprintln(p.W, "an active sandbox mode will run on the host with a sandbox_skipped event")
		fmt.Fprintln(p.W, "in events.jsonl. Install Docker or Podman locally to enable container")
		fmt.Fprintln(p.W, "isolation, or run iterion in cloud mode for k8s-native isolation.")
	}
	return nil
}

// defaultDriverRegistry returns the side-effect-free registry of
// driver constructors known to the CLI. Phase 1 ships docker + noop;
// Phase 5 adds kubernetes.
//
// The factory walks this map in [sandbox.preferenceOrder] for the
// detected host kind. On a host without docker/podman, the docker
// constructor returns ErrUnavailable and the factory falls through
// to noop.
func defaultDriverRegistry() map[string]sandbox.DriverConstructor {
	return map[string]sandbox.DriverConstructor{
		"docker": docker.Constructor,
		"podman": docker.Constructor, // same code path; runtime detection picks
		"noop":   noop.Constructor,
	}
}
