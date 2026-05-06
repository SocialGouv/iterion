package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// HostKind identifies the iterion deployment topology.
//
// The factory uses this to pick a default driver when the caller does
// not request one explicitly. The mapping is documented in the design
// note (.plans/on-va-tudier-la-snappy-lemon.md, §0bis).
type HostKind string

const (
	// HostLocal: `iterion run` from a developer machine. Default to
	// docker driver if available; noop otherwise.
	HostLocal HostKind = "local"

	// HostDesktop: Wails desktop app (cmd/iterion-desktop). Same
	// driver preference as HostLocal but failure modes surface in
	// the UI rather than the CLI.
	HostDesktop HostKind = "desktop"

	// HostCloud: `iterion server` / `iterion runner` running inside
	// a kubernetes pod. Default to kubernetes driver if available;
	// noop otherwise (Phase 5 wires KubernetesDriver; Phase 0 uses
	// noop everywhere).
	HostCloud HostKind = "cloud"
)

// FactoryOptions tunes [NewFactory]. Zero values are sensible.
type FactoryOptions struct {
	// Host overrides the auto-detected host kind. Defaults derive
	// from ITERION_MODE (local|cloud) and the presence of the
	// iterion-desktop wrapper.
	Host HostKind

	// PreferredDriver, if non-empty, forces the factory to pick this
	// driver name. An unrecognised name returns an error from
	// [Factory.Driver]. Useful for tests and for users who want to
	// override auto-detection ("driver: docker" in a config file).
	PreferredDriver string

	// AvailableDrivers is the set of registered driver constructors,
	// keyed by Driver.Name(). Populated by side-effect imports from
	// driver sub-packages (pkg/sandbox/noop, pkg/sandbox/docker, ...).
	// Tests can pass a synthetic map here.
	AvailableDrivers map[string]DriverConstructor
}

// DriverConstructor returns a [Driver] instance. The constructor may
// inspect the host (`exec.LookPath`, env vars, kubeconfig) to decide
// whether it is usable; if not, it returns (nil, ErrUnavailable) and
// the factory falls back to the next candidate.
type DriverConstructor func() (Driver, error)

// ErrUnavailable signals that a driver constructor cannot satisfy the
// host's environment (e.g. docker not installed, not in a k8s pod).
// The factory treats it as "skip this candidate" rather than a hard
// failure.
type ErrUnavailable struct {
	Driver string
	Reason string
}

// Error implements error.
func (e *ErrUnavailable) Error() string {
	return fmt.Sprintf("sandbox driver %q unavailable: %s", e.Driver, e.Reason)
}

// Factory selects and instantiates a [Driver] for a run.
type Factory struct {
	host       HostKind
	preferred  string
	registry   map[string]DriverConstructor
	cached     Driver // memoised result of [Factory.Driver]
	cachedErr  error  // memoised error
	cacheValid bool
}

// NewFactory builds a Factory. The factory inspects the environment
// lazily — no driver is constructed until [Factory.Driver] is called.
func NewFactory(opts FactoryOptions) *Factory {
	host := opts.Host
	if host == "" {
		host = detectHost()
	}
	registry := opts.AvailableDrivers
	if registry == nil {
		registry = map[string]DriverConstructor{}
	}
	return &Factory{
		host:      host,
		preferred: opts.PreferredDriver,
		registry:  registry,
	}
}

// Host returns the resolved host kind.
func (f *Factory) Host() HostKind { return f.host }

// Register adds (or replaces) a driver constructor. Driver sub-packages
// register themselves via init() side-effects in package main / cmd
// init code. Registration is not thread-safe and should happen during
// process bootstrap.
func (f *Factory) Register(name string, ctor DriverConstructor) {
	f.registry[name] = ctor
	f.cacheValid = false
}

// Available returns the names of registered drivers in stable order.
// Useful for `iterion sandbox doctor` output.
func (f *Factory) Available() []string {
	out := make([]string, 0, len(f.registry))
	for k := range f.registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Driver returns the driver to use for this host, instantiating it on
// first call. The selection rule is:
//
//  1. PreferredDriver, if set, must succeed — its failure is a hard error.
//  2. Otherwise, walk the host's preference order and return the first
//     constructor that succeeds.
//  3. If none succeed, return the noop driver (which is always
//     constructible — see pkg/sandbox/noop).
//
// The result is cached for the lifetime of the Factory.
func (f *Factory) Driver() (Driver, error) {
	if f.cacheValid {
		return f.cached, f.cachedErr
	}

	var d Driver
	var err error

	if f.preferred != "" {
		d, err = f.tryDriver(f.preferred)
		if err != nil {
			f.cached, f.cachedErr, f.cacheValid = nil, fmt.Errorf("preferred driver %q: %w", f.preferred, err), true
			return f.cached, f.cachedErr
		}
		f.cached, f.cachedErr, f.cacheValid = d, nil, true
		return d, nil
	}

	for _, name := range preferenceOrder(f.host) {
		d, err = f.tryDriver(name)
		if err == nil {
			f.cached, f.cachedErr, f.cacheValid = d, nil, true
			return d, nil
		}
	}

	f.cached, f.cachedErr, f.cacheValid = nil, fmt.Errorf("no usable sandbox driver found (registry: %v)", f.Available()), true
	return f.cached, f.cachedErr
}

func (f *Factory) tryDriver(name string) (Driver, error) {
	ctor, ok := f.registry[name]
	if !ok {
		return nil, fmt.Errorf("driver %q not registered", name)
	}
	return ctor()
}

// preferenceOrder returns the driver-name precedence for a host.
// Phase 0 only wires the noop driver, so the lists are
// forward-looking; the unregistered names are silently skipped by
// [Factory.Driver].
func preferenceOrder(h HostKind) []string {
	switch h {
	case HostCloud:
		return []string{"kubernetes", "noop"}
	case HostDesktop, HostLocal:
		return []string{"docker", "podman", "noop"}
	}
	return []string{"noop"}
}

// detectHost picks a [HostKind] from the environment without any
// caller-side input. ITERION_MODE wins over heuristics so explicit
// configuration is always honoured.
func detectHost() HostKind {
	switch strings.ToLower(os.Getenv("ITERION_MODE")) {
	case "cloud":
		return HostCloud
	case "local":
		return HostLocal
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		// Likely running inside a pod even without ITERION_MODE set
		// (e.g. someone shelled in for debugging).
		return HostCloud
	}
	if isDesktopBinary() {
		return HostDesktop
	}
	return HostLocal
}

// isDesktopBinary heuristically identifies the iterion-desktop Wails
// wrapper. The desktop binary sets ITERION_DESKTOP=1 at startup
// (see cmd/iterion-desktop). Falls back to argv[0] inspection so the
// detection still works when the env var is missing (e.g. the binary
// was renamed).
func isDesktopBinary() bool {
	if os.Getenv("ITERION_DESKTOP") != "" {
		return true
	}
	if len(os.Args) == 0 {
		return false
	}
	return strings.Contains(strings.ToLower(os.Args[0]), "iterion-desktop")
}

// HostHasDocker is a small probe used by `iterion sandbox doctor` to
// surface the docker/podman state independently of which driver got
// selected. Returns the binary name found ("docker" or "podman") or
// the empty string.
func HostHasDocker() string {
	for _, bin := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(bin); err == nil {
			return bin
		}
	}
	return ""
}
