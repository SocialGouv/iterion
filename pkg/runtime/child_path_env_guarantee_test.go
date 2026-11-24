//go:build unix

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

const childDriverEnvKey = "ITERION_TEST_CHILD_DRIVER_ENV"

type childDriverEnvRun struct {
	sandbox.Run
	t     *testing.T
	nonce string
	calls int
}

func (r *childDriverEnvRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.calls++
	if _, ok := opts.Env[childDriverEnvKey]; ok {
		r.t.Fatal("transport nonce was moved into workload Env")
	}
	if opts.Env["WORKLOAD"] != "workload-only" {
		r.t.Fatal("workload options were lost")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), childDriverEnvKey+"="+r.nonce)
	return cmd
}
func (r *childDriverEnvRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecCmd(r.Command(ctx, argv, opts), opts)
}

func TestChildPathCommandPreservesDriverEnvironment(t *testing.T) {
	t.Setenv(childDriverEnvKey, "")
	if err := os.Unsetenv(childDriverEnvKey); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	tool := filepath.Join(bin, "child-env-witness")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$"+childDriverEnvKey+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The executable is absent from the ambient PATH, just as the nonce is
	// absent from the ambient environment. Both wrapper and Cmd must survive.
	for _, mode := range []string{"Command", "Exec"} {
		t.Run(mode, func(t *testing.T) {
			inner := &childDriverEnvRun{t: t, nonce: filepath.Base(t.TempDir())}
			r := &childPathRun{Run: inner, bin: bin}
			opts := sandbox.ExecOpts{Env: map[string]string{"WORKLOAD": "workload-only"}}
			var out []byte
			var err error
			if mode == "Command" {
				out, err = r.Command(context.Background(), []string{"child-env-witness"}, opts).Output()
			} else {
				var result sandbox.ExecResult
				result, err = r.Exec(context.Background(), []string{"child-env-witness"}, opts)
				out = result.Stdout
				if result.ExitCode != 0 {
					t.Fatalf("exec exit=%d stderr=%s", result.ExitCode, result.Stderr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != inner.nonce {
				t.Fatalf("TRANSPORT_LOST: got %q, want private driver nonce %q", out, inner.nonce)
			}
			if inner.calls != 1 {
				t.Fatalf("inner Command calls=%d", inner.calls)
			}
		})
	}
}
