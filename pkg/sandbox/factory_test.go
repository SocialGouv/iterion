package sandbox

import (
	"errors"
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

func TestErrUnavailableMessage(t *testing.T) {
	e := &ErrUnavailable{Driver: "docker", Reason: "binary not in PATH"}
	got := e.Error()
	if got != `sandbox driver "docker" unavailable: binary not in PATH` {
		t.Errorf("Error() = %q", got)
	}
}

// mkDriver returns a Driver stub from the test helper file
// (factory_test_helper.go) so factory tests don't have to import
// context.Context explicitly.
func mkDriver(name string) Driver {
	return &testDriver{name: name}
}
