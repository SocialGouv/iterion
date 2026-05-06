package sandbox

import (
	"context"
	"os/exec"
)

// testDriver is a driver stub used by factory tests to satisfy the
// Driver interface without pulling docker/k8s deps. It is build-tagged
// out of production binaries via its name (no init registration). The
// file lives under pkg/sandbox/ rather than pkg/sandbox/internal so
// the test file (factory_test.go) can refer to it without an extra
// helper package.
type testDriver struct {
	name string
	caps Capabilities
}

// Name implements Driver.
func (d *testDriver) Name() string { return d.name }

// Capabilities implements Driver.
func (d *testDriver) Capabilities() Capabilities { return d.caps }

// Prepare implements Driver — returns a stub PreparedSpec.
func (d *testDriver) Prepare(_ context.Context, spec Spec) (PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &testPrepared{driver: d.name}, nil
}

// Start implements Driver — returns a stub Run.
func (d *testDriver) Start(_ context.Context, _ PreparedSpec, _ RunInfo) (Run, error) {
	return &testRun{driver: d.name}, nil
}

type testPrepared struct{ driver string }

func (t *testPrepared) DriverName() string { return t.driver }

type testRun struct{ driver string }

func (r *testRun) Driver() string { return r.driver }
func (r *testRun) Command(ctx context.Context, _ []string, _ ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}
func (r *testRun) Exec(_ context.Context, _ []string, _ ExecOpts) (ExecResult, error) {
	return ExecResult{}, nil
}
func (r *testRun) Stop(_ context.Context) error    { return nil }
func (r *testRun) Cleanup(_ context.Context) error { return nil }
