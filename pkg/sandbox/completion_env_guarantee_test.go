//go:build unix

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
)

const completionDriverEnvKey = "ITERION_TEST_SWEEP_DRIVER_ENV"

type completionDriverEnvRun struct {
	Run
	t     *testing.T
	nonce string
	calls int
}

func (r *completionDriverEnvRun) Command(ctx context.Context, argv []string, opts ExecOpts) *exec.Cmd {
	r.calls++
	if len(opts.Env) != 0 {
		r.t.Fatal("sweep must not move host transport into workload Env")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), completionDriverEnvKey+"="+r.nonce)
	return cmd
}

func TestCompletionSweepPreservesDriverEnvironment(t *testing.T) {
	t.Setenv(completionDriverEnvKey, "")
	if err := os.Unsetenv(completionDriverEnvKey); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	run := &completionDriverEnvRun{t: t, nonce: filepath.Base(t.TempDir())}
	capture := filepath.Join(t.TempDir(), "observed")
	// Execute a harmless remote-script stand-in through the real sweep caller;
	// never run a /proc sweep on the test host.
	script := "printf '%s' \"$" + completionDriverEnvKey + "\" > " + shellquote.Quote(capture) + "\nprintf 'iterion-node-session done\\n'\n"
	outcome, reached := NewCompletion("env-witness").sweepOnce(run, script, time.Now().Add(5*time.Second))
	if outcome.Err != nil || !outcome.Ran || !reached {
		t.Fatalf("sweep did not finish: %+v reached=%v", outcome, reached)
	}
	out, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != run.nonce {
		t.Fatalf("TRANSPORT_LOST: got %q, want private driver nonce %q", out, run.nonce)
	}
	if run.calls != 1 {
		t.Fatalf("Command calls=%d", run.calls)
	}
}
