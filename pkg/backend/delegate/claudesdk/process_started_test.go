//go:build !windows

package claudesdk

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestStartedPublication(t *testing.T) {
	sentinel := errors.New("original refusal")
	for _, kind := range []string{"default", "custom-owned", "refused", "missing-hook", "start-fails"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			opts := spawnOptions{ProcessStarted: func(cmd *exec.Cmd) {
				calls++
				if cmd.Process == nil {
					t.Error("published before successful Start")
				}
			}}
			switch kind {
			case "custom-owned":
				opts.PreserveCommandCancellation = true
				opts.CommandBuilder = func(ctx context.Context, _ string, _ []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
					return exec.CommandContext(ctx, "/bin/true")
				}
			case "refused":
				opts.PreserveCommandCancellation = true
				opts.CommandBuilder = func(context.Context, string, []string, string, map[string]string, bool) *exec.Cmd {
					return &exec.Cmd{Err: sentinel}
				}
			case "missing-hook":
				opts.PreserveCommandCancellation = true
				opts.CommandBuilder = func(context.Context, string, []string, string, map[string]string, bool) *exec.Cmd {
					return exec.Command("/bin/true")
				}
			case "start-fails":
				opts.CommandBuilder = func(ctx context.Context, _ string, _ []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
					return exec.CommandContext(ctx, "/never-present-started-publication")
				}
			}
			p, err := spawnProcess(t.Context(), "/bin/true", nil, opts)
			success := kind == "default" || kind == "custom-owned"
			if success {
				if err != nil || calls != 1 {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
				_ = p.close()
			} else {
				if err == nil || calls != 0 {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
				if kind == "refused" && !errors.Is(err, sentinel) {
					t.Fatalf("cause=%v", err)
				}
			}
		})
	}
}
