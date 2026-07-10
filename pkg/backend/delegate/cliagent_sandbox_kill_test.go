package delegate

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// recordingSandboxRun captures every Command argv; the returned cmd is
// a host no-op so runOnce completes without a container.
type recordingSandboxRun struct {
	mu    sync.Mutex
	argvs [][]string
}

func (r *recordingSandboxRun) Driver() string { return "fake" }

func (r *recordingSandboxRun) Command(ctx context.Context, cmd []string, _ sandbox.ExecOpts) *exec.Cmd {
	r.mu.Lock()
	r.argvs = append(r.argvs, append([]string(nil), cmd...))
	r.mu.Unlock()
	return exec.CommandContext(ctx, "true")
}

func (r *recordingSandboxRun) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := r.Command(ctx, cmd, opts)
	err := c.Run()
	return sandbox.ExecResult{}, err
}

func (r *recordingSandboxRun) Cleanup(context.Context) error { return nil }

// TestCLIAgentSandboxRunOnceWrapsAndKills pins the leak fix on the
// cliagent sandbox path: the delegated argv must run under the pidfile
// wrapper (so the in-container PID is recorded — killing the host-side
// docker exec client alone has no signal path to the process), and the
// deferred cleanup must issue the in-container kill script. Same leak
// class as native:221edac8 on the claude_code path.
func TestCLIAgentSandboxRunOnceWrapsAndKills(t *testing.T) {
	run := &recordingSandboxRun{}
	b := &CLIAgentBackend{
		Protocol: CLIAgentProtocol{Name: "testcli"},
		Logger:   iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
	}
	task := Task{NodeID: "campaign", Iteration: 0, Sandbox: run}
	if _, _, _, err := b.runOnce(context.Background(), task, "agent-bin", []string{"--flag"}, "", nil, time.Minute); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.argvs) != 2 {
		t.Fatalf("Command calls = %d (%v), want 2 (wrapped agent + cleanup kill)", len(run.argvs), run.argvs)
	}
	wrapped := run.argvs[0]
	if wrapped[0] != "sh" || wrapped[1] != "-c" {
		t.Fatalf("agent argv not wrapped through sh -c: %v", wrapped)
	}
	if !strings.Contains(wrapped[2], "echo $$ >") || !strings.Contains(wrapped[2], `exec "$@"`) {
		t.Fatalf("wrapper script %q must record $$ then exec the agent", wrapped[2])
	}
	if got := wrapped[len(wrapped)-2:]; got[0] != "agent-bin" || got[1] != "--flag" {
		t.Fatalf("original argv not preserved at tail: %v", wrapped)
	}
	cleanup := run.argvs[1]
	if cleanup[0] != "sh" || !strings.Contains(cleanup[2], "kill -TERM") {
		t.Fatalf("deferred cleanup did not issue the in-container kill: %v", cleanup)
	}
}
