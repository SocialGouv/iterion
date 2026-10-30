package delegate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// rewriteCapableTask is a task whose compression is on and whose rewriter
// binary is present — the two preconditions installRewriteHook already had —
// so the sandbox signal is the only thing left for the predicate to consult.
func rewriteCapableTask(t *testing.T) Task {
	t.Helper()
	rw := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nprintf 'rtk %s' \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Task{NodeID: "n", CompressMode: "on", Rewriters: []plugin.RewriterSpec{{ID: "fake",
		Locate: plugin.LocateSpec{Paths: []string{rw}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}, ApplyExitCodes: []int{0}}}}}
}

// rewriteSkipsSandboxedTask is the delegate half of the #2007 placement
// decision: it must see the sandbox signal — a real driver, not the noop
// passthrough — and the knob.
func TestRewriteSkipsSandboxedTaskSeesTheSandboxSignal(t *testing.T) {
	for _, c := range []struct {
		name string
		task Task
		env  string
		want bool
	}{
		{"host task, hatch flipped", Task{}, "skip", false},
		{"sandboxed, hatch at default", Task{Sandbox: stubSandboxRun{}}, "", false},
		{"sandboxed, explicit host", Task{Sandbox: stubSandboxRun{}}, "host", false},
		{"sandboxed, hatch flipped", Task{Sandbox: stubSandboxRun{}}, "skip", true},
		{"noop passthrough is not a sandbox", Task{Sandbox: noopLikeRun{}}, "skip", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(rewrite.SandboxEnv, c.env)
			if got := rewriteSkipsSandboxedTask(c.task); got != c.want {
				t.Errorf("rewriteSkipsSandboxedTask = %v, want %v", got, c.want)
			}
		})
	}
}

// With the hatch at its default a sandboxed task's hook installs exactly as
// before (the chain still runs host-side — byte-identical behaviour); with
// the hatch flipped the hook is NOT installed and the stand-down is named on
// the node's log. installRewriteHook appends exactly one option when it
// installs the hook, so the opts length is the installation witness.
func TestInstallRewriteHookUnderTheSandboxHatch(t *testing.T) {
	task := rewriteCapableTask(t)
	base := []claudesdk.Option{claudesdk.WithModel("m")}

	t.Run("default: the chain still installs for a sandboxed task", func(t *testing.T) {
		task := task
		task.Sandbox = stubSandboxRun{}
		var logs bytes.Buffer
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
		if got := b.installRewriteHook(task, base); len(got) != len(base)+1 {
			t.Errorf("hook not installed at the default: %d opts, want %d", len(got), len(base)+1)
		}
		if logs.Len() != 0 {
			t.Errorf("the default path must stay silent, logged: %s", logs.String())
		}
	})

	t.Run("skip: the hook stands down and names the knob", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "skip")
		task := task
		task.Sandbox = stubSandboxRun{}
		var logs bytes.Buffer
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
		if got := b.installRewriteHook(task, base); len(got) != len(base) {
			t.Errorf("hook installed despite %s=skip: %d opts, want %d", rewrite.SandboxEnv, len(got), len(base))
		}
		if out := logs.String(); !bytes.Contains(logs.Bytes(), []byte(rewrite.SandboxEnv)) || !bytes.Contains(logs.Bytes(), []byte("stands down")) {
			t.Errorf("the stand-down must name %s on the node's log, got: %q", rewrite.SandboxEnv, out)
		}
	})

	t.Run("skip: a host task is unaffected", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "skip")
		b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
		if got := b.installRewriteHook(task, base); len(got) != len(base)+1 {
			t.Errorf("a host task lost its hook under %s=skip: %d opts, want %d", rewrite.SandboxEnv, len(got), len(base)+1)
		}
	})
}
