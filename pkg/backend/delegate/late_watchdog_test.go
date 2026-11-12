//go:build !windows

package delegate

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type lateWatchdogRun struct {
	captureSandboxCmdRun
	entered, release, cancelled chan struct{}
	once                        sync.Once
	pidfile                     string
}

func (*lateWatchdogRun) OwnsCommandCancellation() bool { return true }
func (r *lateWatchdogRun) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	if !strings.Contains(strings.Join(argv, " "), "--print") {
		return exec.CommandContext(ctx, "true")
	}
	close(r.entered)
	<-r.release
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", `printf '%s' "$$" > "$1"; exec sleep 60`, "sh", r.pidfile)
	proc.TerminateGroupOnCancel(cmd)
	cancel := cmd.Cancel
	cmd.Cancel = func() error { err := cancel(); r.once.Do(func() { close(r.cancelled) }); return err }
	return cmd
}
func TestFormattingWatchdogCancelsLateStartedProcess(t *testing.T) {
	resetClaudeCredEnv(t)
	run := &lateWatchdogRun{entered: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{}), pidfile: filepath.Join(t.TempDir(), "pid")}
	var release sync.Once
	defer release.Do(func() { close(run.release) })
	t.Cleanup(func() {
		select {
		case <-run.cancelled:
			return
		default:
		}
		if raw, e := os.ReadFile(run.pidfile); e == nil {
			if pid, e := strconv.Atoi(string(raw)); e == nil {
				if p, e := os.FindProcess(pid); e == nil {
					_ = p.Kill()
				}
			}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
		_, _, _, err := b.formatOutput(ctx, Task{NodeID: "late", UserPrompt: "x", Sandbox: run, OutputSchema: []byte(schemaOK)}, "sid")
		done <- err
	}()
	select {
	case <-run.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("builder never entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled format succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not return before Start")
	}
	release.Do(func() { close(run.release) })
	select {
	case <-run.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("late successful Start escaped the remembered cancellation")
	}
}
