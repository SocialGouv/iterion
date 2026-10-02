package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestStart_readsWhetherTheContainerHasItsOwnProcessNamespace: a signal to
// every process from a command of the container reaches only its own when
// the container runs in a process namespace of its own. The runtime's
// defaults decide that — podman's containers.conf may put every container
// in the host's — so the Run reports what the started container runs in,
// never a constant.
func TestStart_readsWhetherTheContainerHasItsOwnProcessNamespace(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	for _, tc := range []struct {
		name, inspect string
		isolated      bool
	}{
		{"docker's default", `echo ""`, true},
		{"podman's private", `echo private`, true},
		{"the host's", `echo host`, false},
		{"another container's", `echo container:3f2a`, false},
		{"not readable", `exit 3`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := Runtime("iterion-test-pidns-runtime")
			shimDir := t.TempDir()
			shim := "#!/bin/sh\n" +
				"case \"$1 $2 $3\" in\n" +
				"  \"inspect --format {{.HostConfig.PidMode}}\") " + tc.inspect + " ;;\n" +
				"  \"image inspect --format\") echo sha256:0123 ;;\n" +
				"  run*) echo 0123456789abcdef ;;\n" +
				"esac\n" +
				"exit 0\n"
			if err := os.WriteFile(filepath.Join(shimDir, string(rt)), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			d := &Driver{rt: rt, logger: iterlog.Nop()}
			ctx := context.Background()
			prepared, err := d.Prepare(ctx, sandbox.Spec{Mode: sandbox.ModeInline, Image: "example.invalid/sandbox:test"})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			run, err := d.Start(ctx, prepared, sandbox.RunInfo{RunID: "run-pidns", WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = run.Cleanup(ctx) }()
			pi, ok := run.(sandbox.ProcessIsolated)
			if !ok || pi.ProcessIsolated() != tc.isolated {
				t.Fatalf("the started container reports isolated=%v, want %v", ok && pi.ProcessIsolated(), tc.isolated)
			}
		})
	}
}
