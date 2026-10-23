package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// stalledKubectl replaces the shell instead of spawning descendants. A shell
// trap cannot reap children after CommandContext kills the shell with SIGKILL.
func stalledKubectl(t *testing.T, seconds int) string {
	t.Helper()
	isolateExecEnvironment(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, kubeBinaryName)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep "+strconv.Itoa(seconds)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fake
}

// TestKubectlWaitDelayBoundsAnUnresponsiveProcess exercises both production
// command builders. Cancellation leaves the process alive so WaitDelay itself
// must kill it and close its pipes. The fixture has no orphan-pipe race: the
// test owns and reaps the only process, even when WaitDelay is removed.
func TestKubectlWaitDelayBoundsAnUnresponsiveProcess(t *testing.T) {
	for _, name := range []string{"exec", "runtime"} {
		t.Run(name, func(t *testing.T) {
			fake := stalledKubectl(t, 30)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var cmd *exec.Cmd
			if name == "exec" {
				r := &Run{
					driver:    &Driver{logger: iterlog.Nop(), kubectl: fake},
					podName:   "pod",
					namespace: "ns",
					prepared:  &Prepared{workspace: "/workspace"},
				}
				cmd = r.Command(ctx, []string{"tar", "-czf", "-", "."}, sandbox.ExecOpts{})
			} else {
				cmd = kubectlCmdContext(ctx, "get", "nodes")
			}
			// Exercise WaitDelay's escalation when graceful cancellation cannot
			// stop the process, without creating an unowned pipe-holding child.
			cmd.Cancel = func() error { return nil }
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			cancel()
			timer := time.NewTimer(8 * time.Second)
			defer timer.Stop()
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != -1 {
					t.Fatalf("WaitDelay must kill the stalled kubectl, got %v; stderr: %s", err, &stderr)
				}
			case <-timer.C:
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("WaitDelay watchdog killed kubectl but Wait did not reap it within 2s")
				}
				t.Fatal("production WaitDelay did not bound kubectl within 8s; watchdog killed and reaped it")
			}
		})
	}
}
