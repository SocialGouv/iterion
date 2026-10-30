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
		{"host task, knob unset", Task{}, "", false},
		{"sandboxed, the default skips", Task{Sandbox: stubSandboxRun{}}, "", true},
		{"sandboxed, explicit host", Task{Sandbox: stubSandboxRun{}}, "host", false},
		{"sandboxed, explicit skip", Task{Sandbox: stubSandboxRun{}}, "skip", true},
		{"noop passthrough is not a sandbox", Task{Sandbox: noopLikeRun{}}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(rewrite.SandboxEnv, c.env)
			if got := rewriteSkipsSandboxedTask(c.task); got != c.want {
				t.Errorf("rewriteSkipsSandboxedTask = %v, want %v", got, c.want)
			}
		})
	}
}

// Under a sandbox the hook stands down by default: the chain no longer runs
// host-side, the stand-down is named on the node's log, and the knob's "host"
// value is the greppable escape hatch restoring the previous byte-identical
// placement. installRewriteHook appends exactly one option when it installs
// the hook, so the opts length is the installation witness.
func TestInstallRewriteHookUnderTheSandboxHatch(t *testing.T) {
	task := rewriteCapableTask(t)
	base := []claudesdk.Option{claudesdk.WithModel("m")}

	t.Run("default: the hook stands down for a sandboxed task and names the knob", func(t *testing.T) {
		task := task
		task.Sandbox = stubSandboxRun{}
		var logs bytes.Buffer
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
		if got := b.installRewriteHook(task, base); len(got) != len(base) {
			t.Errorf("hook installed at the default under a sandbox: %d opts, want %d", len(got), len(base))
		}
		if out := logs.String(); !bytes.Contains(logs.Bytes(), []byte(rewrite.SandboxEnv)) || !bytes.Contains(logs.Bytes(), []byte("stands down")) {
			t.Errorf("the stand-down must name %s on the node's log, got: %q", rewrite.SandboxEnv, out)
		}
	})

	t.Run("host: the chain still installs for a sandboxed task", func(t *testing.T) {
		t.Setenv(rewrite.SandboxEnv, "host")
		task := task
		task.Sandbox = stubSandboxRun{}
		var logs bytes.Buffer
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
		if got := b.installRewriteHook(task, base); len(got) != len(base)+1 {
			t.Errorf("hook not installed under %s=host: %d opts, want %d", rewrite.SandboxEnv, len(got), len(base)+1)
		}
		if logs.Len() != 0 {
			t.Errorf("the host escape hatch must stay silent, logged: %s", logs.String())
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
