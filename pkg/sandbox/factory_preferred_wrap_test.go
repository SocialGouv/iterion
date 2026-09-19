package sandbox

import (
	"errors"
	"testing"
)

// TestFactoryDriverWrapsErrDriverUnavailable pins the second way this
// factory can fail to produce a driver: a PreferredDriver whose
// constructor fails (docker named, docker not installed). It must wrap
// `ErrDriverUnavailable` like the preference walk does, so
// runtime.resolveAndStartSandbox reads ONE `errors.Is` and produces the
// typed `SANDBOX_DRIVER_UNAVAILABLE` FailureCode — unwrapped, a pinned
// driver's failure parks the run under EXECUTION_FAILED and the
// classifier row never fires.
//
// Mutation: drop the `fmt.Errorf("%w: %w", ErrDriverUnavailable, err)`
// chain in Factory.Driver's preferred branch (or the equivalent
// preference-walk wrap) → this test reddens with `errors.Is` false.
func TestFactoryDriverWrapsErrDriverUnavailable(t *testing.T) {
	registry := map[string]DriverConstructor{
		"docker": func() (Driver, error) {
			return nil, &ErrUnavailable{Driver: "docker", Reason: "not installed"}
		},
	}
	f := NewFactory(FactoryOptions{
		Host: HostLocal, AvailableDrivers: registry, PreferredDriver: "docker",
	})
	_, err := f.Driver()
	if err == nil {
		t.Fatal("Driver() with failing PreferredDriver = nil err, want wrapped ErrDriverUnavailable")
	}
	if !errors.Is(err, ErrDriverUnavailable) {
		t.Fatalf("errors.Is(err, ErrDriverUnavailable) = false — a pinned driver's failure would park the run untyped and #1426's schedule would miss SANDBOX_DRIVER_UNAVAILABLE; err = %v", err)
	}
}
