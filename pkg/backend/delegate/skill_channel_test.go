package delegate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The claude_code half of the pinned skill channel (iterion#1719): the
// engine-owned root reaches the CLI as --add-dir — its documented discovery
// for skills outside the working tree — on the host, and stays OUT of a
// container spawn, whose workspace is not this process's filesystem (the
// run-owned bind mount is #1730's admission-half design).
func TestSkillChannelAddDir(t *testing.T) {
	root := t.TempDir()
	if got, ok := skillChannelAddDir(Task{}); ok || got != "" {
		t.Errorf("no channel root = (%q, %v), want none", got, ok)
	}
	if got, ok := skillChannelAddDir(Task{SkillChannelRoot: root}); !ok || got != root {
		t.Errorf("a host run = (%q, %v), want the root", got, ok)
	}
	// The noop driver is a host passthrough: the channel reaches it, through
	// the same Task.Hostless() reading every other caller makes.
	if got, ok := skillChannelAddDir(Task{SkillChannelRoot: root, Sandbox: fakeSandboxRun{driver: "noop"}}); !ok || got != root {
		t.Errorf("a noop-driver run = (%q, %v), want the root", got, ok)
	}
	if got, ok := skillChannelAddDir(Task{SkillChannelRoot: root, Sandbox: fakeSandboxRun{driver: "docker"}}); ok || got != "" {
		t.Errorf("a container run = (%q, %v), want the channel withheld", got, ok)
	}
}

// The wiring witness: an armed task's spawn really carries --add-dir, and a
// disarmed one does not. The scripted fake CLI records its argv.
func TestClaudeCodeSpawnCarriesTheSkillChannelRoot(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("POSIX shell fixture")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(fake, []byte(fakeClaudeScripted), 0o755); err != nil {
		t.Fatal(err)
	}
	script := []string{
		lnInit("2.1.280"),
		lnAssistant("m1", "", cText("done")),
		lnResult(resultSpec{text: "done", turns: 1, cost: 0.01, in: 1, out: 1}),
	}
	scriptPath := filepath.Join(dir, "script")
	if err := os.WriteFile(scriptPath, []byte(strings.Join(script, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_SCRIPT", scriptPath)

	channelRoot := filepath.Join(dir, "channel")
	for i, tc := range []struct {
		name string
		root string
		want bool
	}{
		{"an armed task", channelRoot, true},
		{"a disarmed task", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawnLog := filepath.Join(dir, fmt.Sprintf("spawn-%d.log", i))
			t.Setenv("FAKE_CLAUDE_SPAWN_LOG", spawnLog)
			b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := b.Execute(ctx, Task{NodeID: "n", Command: fake, WorkDir: dir, UserPrompt: "say ok", SkillChannelRoot: tc.root})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			raw, err := os.ReadFile(spawnLog)
			if err != nil {
				t.Fatalf("the fake CLI recorded nothing: %v", err)
			}
			got := strings.Contains(string(raw), "--add-dir "+tc.root)
			if tc.want && !got {
				t.Errorf("the spawn argv lacks --add-dir %s:\n%s", tc.root, raw)
			}
			if !tc.want && strings.Contains(string(raw), "--add-dir") {
				t.Errorf("a disarmed task still spawned with --add-dir:\n%s", raw)
			}
		})
	}
}

// fakeSandboxRun is the sandbox.Run surface skillChannelAddDir reads: the
// driver name.
type fakeSandboxRun struct{ driver string }

func (f fakeSandboxRun) Driver() string { return f.driver }

func (f fakeSandboxRun) Command(context.Context, []string, sandbox.ExecOpts) *exec.Cmd {
	return nil
}

func (f fakeSandboxRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (f fakeSandboxRun) Cleanup(context.Context) error { return nil }
