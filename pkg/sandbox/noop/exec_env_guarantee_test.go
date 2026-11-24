//go:build unix

package noop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The driver has no remote transport: its spec seed is the driver-owned
// addition and per-exec Env is the workload override. Both must survive the
// Command→Exec crossing, without relying on an ambient export.
func TestExecPreservesDriverEnvironment(t *testing.T) {
	const key = "ITERION_TEST_NOOP_DRIVER_ENV"
	const workload = "ITERION_TEST_NOOP_WORKLOAD_ENV"
	for _, name := range []string{key, workload} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	d, _ := New()
	nonce := filepath.Base(t.TempDir())
	p, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeNone, Env: map[string]string{key: nonce, workload: "seed"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Start(context.Background(), p, sandbox.RunInfo{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cleanup(context.Background())
	result, err := r.Exec(context.Background(), []string{"sh", "-c", "printf '%s|%s' \"$" + key + "\" \"$" + workload + "\""}, sandbox.ExecOpts{Env: map[string]string{workload: "workload"}})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("exec: %+v %v", result, err)
	}
	if string(result.Stdout) != nonce+"|workload" {
		t.Fatalf("TRANSPORT_LOST: got %q, want %q", result.Stdout, nonce+"|workload")
	}
}
