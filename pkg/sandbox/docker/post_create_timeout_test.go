package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The post_create snippet is a SETUP phase, and every setup phase carries
// its own bound: an unbounded one does not fail, it waits, and a run
// waiting in setup emits no `sandbox_started`, holds whatever lease it
// took, and only dies when the run's own max_duration fires. The
// kubernetes driver bounds it; the docker driver must land on the same
// contract and the same typed park, through the same helper.
//
// Driven through the REAL runPostCreate with a container-runtime shim
// that never exits — the "package install waiting on a dead mirror"
// shape, seen from the host side.
func TestRunPostCreate_DockerStallIsBoundedAndTyped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	rt := Runtime("iterion-test-wedged-runtime")
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, string(rt)), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(sandbox.PostCreateTimeoutEnv, "300ms")

	r := &Run{
		driver:      &Driver{rt: rt, logger: iterlog.Nop()},
		containerID: "deadbeef",
		prepared:    &Prepared{},
	}

	done := make(chan error, 1)
	go func() { done <- r.runPostCreate(context.Background(), "apt-get install -y the-world") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged post_create returned no error")
		}
		if !errors.Is(err, sandbox.ErrPhaseTimeout) {
			t.Fatalf("post_create stall does not carry sandbox.ErrPhaseTimeout: %v — the setup classifier cannot park it resumable", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("post_create stall does not carry context.DeadlineExceeded: %v", err)
		}
		if !strings.Contains(err.Error(), "post_create phase timed out") {
			t.Fatalf("timeout error must name the phase, got %q", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runPostCreate never returned — the snippet runs on the bare ctx, so the run sits in sandbox setup until the outer max_duration fires")
	}
}

// The docker post_create reads the SAME knob as the kubernetes one: the
// budget belongs to the phase, not to the driver, so an operator raising
// it for a slow toolchain install raises it everywhere the phase runs.
func TestRunPostCreate_DockerReadsTheSharedPhaseKnob(t *testing.T) {
	t.Setenv(sandbox.PostCreateTimeoutEnv, "45m")
	if got, want := sandbox.ResolvePostCreateTimeout(), 45*time.Minute; got != want {
		t.Fatalf("ResolvePostCreateTimeout() = %s, want %s", got, want)
	}
	if sandbox.PostCreateTimeoutEnv != "ITERION_SANDBOX_POST_CREATE_TIMEOUT" {
		t.Fatalf("post_create knob = %q, want the documented ITERION_SANDBOX_POST_CREATE_TIMEOUT", sandbox.PostCreateTimeoutEnv)
	}
}
