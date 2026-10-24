package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactoryRegistration(t *testing.T) {
	registry := map[string]DriverConstructor{
		"alpha": func() (Driver, error) { return mkDriver("alpha"), nil },
		"beta":  func() (Driver, error) { return mkDriver("beta"), nil },
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		AvailableDrivers: registry,
	})

	avail := f.Available()
	if len(avail) != 2 || avail[0] != "alpha" || avail[1] != "beta" {
		t.Errorf("Available() = %v, want [alpha beta]", avail)
	}
}

func TestFactoryPreferredDriver(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) { return mkDriver("docker"), nil },
		"noop":   func() (Driver, error) { return mkDriver("noop"), nil },
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		PreferredDriver:  "docker",
		AvailableDrivers: registry,
	})
	d, err := f.Driver()
	if err != nil {
		t.Fatalf("Driver() err = %v", err)
	}
	if d.Name() != "docker" {
		t.Errorf("Driver().Name() = %q, want docker", d.Name())
	}
}

func TestFactoryPreferredDriverFailsHard(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) {
			return nil, &ErrUnavailable{Driver: "docker", Reason: "not installed"}
		},
		"noop": func() (Driver, error) { return mkDriver("noop"), nil },
	}
	f := NewFactory(FactoryOptions{
		PreferredDriver:  "docker",
		AvailableDrivers: registry,
	})
	_, err := f.Driver()
	if err == nil {
		t.Fatal("expected error when preferred driver unavailable")
	}
	var unavail *ErrUnavailable
	if !errors.As(err, &unavail) {
		t.Errorf("wrapped err = %T, want *ErrUnavailable", err)
	}
}

func TestFactoryFallsBackToNoop(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) {
			return nil, &ErrUnavailable{Driver: "docker", Reason: "not installed"}
		},
		"noop": func() (Driver, error) { return mkDriver("noop"), nil },
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		AvailableDrivers: registry,
	})
	d, err := f.Driver()
	if err != nil {
		t.Fatalf("Driver() err = %v", err)
	}
	if d.Name() != "noop" {
		t.Errorf("Driver().Name() = %q, want noop", d.Name())
	}
}

func TestFactoryCachesDriver(t *testing.T) {
	calls := 0
	registry := map[string]DriverConstructor{
		"noop": func() (Driver, error) {
			calls++
			return mkDriver("noop"), nil
		},
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		AvailableDrivers: registry,
	})
	for i := 0; i < 5; i++ {
		if _, err := f.Driver(); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("constructor called %d times, want 1", calls)
	}
}

// TestDriverForSpecReportsHostCapabilityNotRunPolicy pins where the
// #1425 split does NOT live. The factory answers one question — can
// this host honour an active mode? — and answers it the same for auto
// and inline, with the typed [ErrDriverUnavailable] every reader keys
// on. Degrade-vs-refuse is the runtime's call
// (runtime.resolveAndStartSandbox), because only there is there a run
// to degrade, an event stream to say so on, and a FailureCode to park
// with. Splitting here too would give `iterion sandbox doctor
// --strict` and the launch pre-flight — which read this same answer
// and cannot degrade anything — a driver named "noop" to report as
// available.
//
// Mutation: return the noop driver for ModeAuto (the mode split back
// in the factory) → the auto sub-case reddens on err == nil.
func TestDriverForSpecReportsHostCapabilityNotRunPolicy(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) {
			return nil, &ErrUnavailable{Driver: "docker", Reason: "not installed"}
		},
		"noop": func() (Driver, error) { return mkDriver("noop"), nil },
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		AvailableDrivers: registry,
	})
	// Both active modes get the same capability answer, typed.
	for _, spec := range []*Spec{{Mode: ModeAuto}, {Mode: ModeInline, Image: "alpine"}} {
		d, err := f.DriverForSpec(spec)
		if err == nil {
			t.Fatalf("DriverForSpec(%s) = %v, want a refusal: noop starts no container, so reporting it as the driver for an active mode is a false capability", spec.Mode, d)
		}
		if !errors.Is(err, ErrDriverUnavailable) {
			t.Fatalf("DriverForSpec(%s) err = %v, want errors.Is(..., ErrDriverUnavailable) — the runtime keys the mode split on it, and #1426's schedule reads the code it produces", spec.Mode, err)
		}
	}
	// Inactive modes degrade silently as before.
	if _, err := f.DriverForSpec(&Spec{Mode: ModeNone}); err != nil {
		t.Errorf("DriverForSpec(ModeNone) unexpected err = %v", err)
	}
	if _, err := f.DriverForSpec(nil); err != nil {
		t.Errorf("DriverForSpec(nil) unexpected err = %v", err)
	}
}

func TestDriverForSpecHonoursPreferredNoop(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) {
			return nil, &ErrUnavailable{Driver: "docker", Reason: "not installed"}
		},
		"noop": func() (Driver, error) { return mkDriver("noop"), nil },
	}
	f := NewFactory(FactoryOptions{
		Host:             HostLocal,
		AvailableDrivers: registry,
		PreferredDriver:  "noop",
	})
	// A caller that pinned noop selected the passthrough on purpose —
	// don't second-guess it. Nothing in the shipped CLI sets this today;
	// it is the embedder/test seam.
	d, err := f.DriverForSpec(&Spec{Mode: ModeAuto})
	if err != nil {
		t.Fatalf("DriverForSpec with PreferredDriver=noop should succeed, got %v", err)
	}
	if d.Name() != "noop" {
		t.Errorf("DriverForSpec().Name() = %q, want noop", d.Name())
	}
}

func TestErrUnavailableMessage(t *testing.T) {
	e := &ErrUnavailable{Driver: "docker", Reason: "binary not in PATH"}
	got := e.Error()
	if got != `sandbox driver "docker" unavailable: binary not in PATH` {
		t.Errorf("Error() = %q", got)
	}
}

// W3: remove f.searchContext from the no-runtime refusal in DriverForSpec:
// PATH and cwd assertions fail. Reading the environment at refusal time instead
// of selection time also fails after the fixture changes PATH and cwd.
func TestDriverForSpecRefusalNamesSearchedPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bin") + ":./relative-bin"
	t.Setenv("PATH", path)
	registry := map[string]DriverConstructor{
		"noop": func() (Driver, error) { return mkDriver("noop"), nil },
	}
	for _, name := range []string{"docker", "podman"} {
		registry[name] = func() (Driver, error) {
			_, err := exec.LookPath(name)
			if err == nil {
				t.Fatalf("owned PATH unexpectedly contains %s", name)
			}
			return nil, &ErrUnavailable{Driver: name, Reason: err.Error()}
		}
	}
	f := NewFactory(FactoryOptions{Host: HostLocal, AvailableDrivers: registry})
	if _, err := f.Driver(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/changed-after-selection")
	t.Chdir(t.TempDir())
	_, err = f.DriverForSpec(&Spec{Mode: ModeAuto})
	if !errors.Is(err, ErrDriverUnavailable) {
		t.Fatalf("want typed refusal, got %v", err)
	}
	for _, want := range []string{fmt.Sprintf("PATH=%q", path), fmt.Sprintf("cwd=%q", cwd), `host="local"`, "host process", "docker", "podman", "--sandbox none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name the actual search context; missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/changed-after-selection") {
		t.Errorf("cached selection reported an environment it never searched: %v", err)
	}
}

func TestDescribeSearchPathBoundsAndQuotes(t *testing.T) {
	path := "/first/bin:" + strings.Repeat("/many/bin:", 300) + "/usr/bin:/bin"
	got := describeSearchPath(path)
	if len(got) > 1150 || !strings.Contains(got, "truncated") || !strings.Contains(got, "/first/bin") || !strings.Contains(got, "/usr/bin:/bin") {
		t.Fatalf("long PATH must retain both ends with bounded output: %s", got)
	}
	if got := describeSearchPath("/bad\nline:/bin"); strings.Contains(got, "\n") || !strings.Contains(got, `\n`) {
		t.Errorf("PATH diagnostic must quote control characters: %q", got)
	}
	if got := describeSearchPath(""); !strings.Contains(got, "empty; no directories searched") {
		t.Errorf("empty PATH must be explicit: %q", got)
	}
}

// mkDriver returns a Driver stub from the test helper file
// (factory_test_helper.go) so factory tests don't have to import
// context.Context explicitly.
func mkDriver(name string) Driver {
	return &testDriver{name: name}
}
