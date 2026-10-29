//go:build unix

package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// A crossing that aborts between cmd.Start and the task envelope has already
// collected whatever the runner wrote on its stderr — and when the runner died
// before the launcher finished seeding it (an image shipping no `iterion`, an
// OOM-killed container), that buffer holds the only account of why. None of
// those aborts folds it into the node's error, which then says nothing but
// that a pipe is closed. The report therefore belongs to every outcome of a
// started crossing, not only to the outcomes that reach the end of the
// dispatch.
func TestSandboxDispatch_ReportsTheRunnersStderrWhenTheCrossingAborts(t *testing.T) {
	t.Parallel()
	const diagnostic = `iterion-claw-runner: exec: "iterion": executable file not found in $PATH`
	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	done := make(chan error, 1)
	go func() {
		// The order is a fact, not a race. The runner writes its stderr, then
		// closes its stdin and waits to be killed. The task envelope is under
		// the IPC's line cap, so it passes the pre-spawn size check, and over a
		// pipe's buffer, so the launcher's write blocks until that close and
		// then fails on a broken pipe: the Kill+Wait abort every pre-task send
		// failure shares, with the diagnostic already written.
		_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: t.TempDir(),
			UserContent: []delegate.ContentBlock{{Type: "text", Text: strings.Repeat("u", 1<<20)}},
			Sandbox: &scriptedSandbox{
				script: "printf '%s\\n' \"$1\" >&2\nexec 0<&-\nexec sleep 30",
				args:   []string{diagnostic},
			},
		})
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s", sandboxDrainBound)
	}
	if err == nil {
		t.Fatal("the task envelope crossed a runner that closed its stdin — this witness no longer drives a pre-task abort")
	}
	if !strings.Contains(err.Error(), "send task envelope") {
		t.Fatalf("the dispatch failed elsewhere than the pre-task abort this witness drives: %v", err)
	}
	if !strings.Contains(logs.String(), diagnostic) {
		t.Errorf("the runner's stderr is nowhere in the operator-facing log, so an abort reports a closed pipe and drops the reason: %q", logs.String())
	}
}
