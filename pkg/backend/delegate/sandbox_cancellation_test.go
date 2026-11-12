//go:build !windows

package delegate

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type cancellationOwnedRejectRun struct{ captureSandboxCmdRun }

func (*cancellationOwnedRejectRun) OwnsCommandCancellation() bool { return true }
func (*cancellationOwnedRejectRun) Command(context.Context, []string, sandbox.ExecOpts) *exec.Cmd {
	return exec.Command("/bin/true")
}
func TestClaudeProductionCancellationOwnershipPropagation(t *testing.T) {
	resetClaudeCredEnv(t)
	for _, format := range []bool{false, true} {
		t.Run(map[bool]string{false: "main", true: "format"}[format], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
			task := Task{NodeID: "n", UserPrompt: "x", Sandbox: &cancellationOwnedRejectRun{}, OutputSchema: []byte(schemaOK)}
			var err error
			if format {
				_, _, _, err = b.formatOutput(ctx, task, "sid")
			} else {
				_, err = b.Execute(ctx, task)
			}
			if err == nil || !strings.Contains(err.Error(), "Cancel hook") {
				t.Fatalf("owned contract did not reach SDK: %v", err)
			}
		})
	}
}

type cancellationOwnedWatchdogRun struct {
	captureSandboxCmdRun
	cancels atomic.Int32
}

func (*cancellationOwnedWatchdogRun) OwnsCommandCancellation() bool { return true }
func (r *cancellationOwnedWatchdogRun) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	if !strings.Contains(strings.Join(argv, " "), "--print") {
		return exec.CommandContext(ctx, "true")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sleep", "60")
	proc.TerminateGroupOnCancel(cmd)
	cancel := cmd.Cancel
	cmd.Cancel = func() error { r.cancels.Add(1); return cancel() }
	return cmd
}
func TestFormattingWatchdogHonorsOwnedHook(t *testing.T) {
	resetClaudeCredEnv(t)
	run := &cancellationOwnedWatchdogRun{}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{NodeID: "n", UserPrompt: "x", Sandbox: run, OutputSchema: []byte(schemaOK)}
	_, _, _, err := b.formatOutput(ctx, task, "sid")
	if err == nil || run.cancels.Load() == 0 {
		t.Fatalf("watchdog bypassed owned hook: cancels=%d err=%v", run.cancels.Load(), err)
	}
}
