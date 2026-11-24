//go:build linux

package execenv

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func hostSupported() error { return nil }
func configureHostCommand(cmd *exec.Cmd) {
	proc.DetachProcessGroup(cmd)
	// A controlled cancellation lets the host kill its client's separate group.
	// WaitDelay remains the caller-resolved bound if the host fails to respond.
	// Direct Process.Kill is different: Pdeathsig protects the client itself,
	// not all credential-plugin descendants and never the remote workload.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func runHost(l wireLaunch, environment []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// Linux Pdeathsig follows the creating OS thread, not merely the process.
	// Keep that thread alive through the child's Wait and group cleanup.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.ClientPath, l.ClientArgs...)
	cmd.Env = environment
	cmd.Stderr = stderr
	cmd.WaitDelay = l.WaitDelay
	proc.TerminateGroupOnCancel(cmd)
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	prefix := newPrefixWriter(stdout, cancel, l.Invocation)
	cmd.Stdout = prefix
	input, err := cmd.StdinPipe()
	if err != nil {
		return 125, refusal("host", "stdin-pipe", err)
	}
	defer input.Close()
	if err = cmd.Start(); err != nil {
		return 125, refusal("host", "client-start", err)
	}
	defer func() { _, _ = proc.KillProcessGroup(cmd) }()
	// Dedicated host process: do not await an SDK stdin reader after client exit.
	// A completed frame is required; errors before it are separately observable.
	frameDone := make(chan error, 1)
	go func() {
		defer input.Close()
		err := protocol.WriteInvocation(input, l.Invocation)
		frameDone <- err
		if err != nil {
			cancel()
			return
		}
		if stdin != nil {
			_, _ = io.Copy(input, stdin)
		}
	}()
	startupDone := make(chan struct{})
	startupResult := make(chan bool, 1)
	go func() {
		var timer *time.Timer
		var deadline <-chan time.Time
		if l.StartTimeout > 0 {
			timer = time.NewTimer(l.StartTimeout)
			deadline = timer.C
			defer timer.Stop()
		}
		select {
		case <-prefix.ready:
			startupResult <- false
		case <-startupDone:
			startupResult <- false
		case <-deadline:
			// A readiness which won the race ends startup, not the workload.
			select {
			case <-prefix.ready:
				startupResult <- false
			default:
				cancel()
				startupResult <- true
			}
		}
	}()
	waitErr := cmd.Wait()
	_ = input.Close()
	close(startupDone)
	timedOut := <-startupResult
	if timedOut {
		return 125, refusal("host", "startup-timeout", context.DeadlineExceeded)
	}
	if err = prefix.result(); err != nil {
		return 125, err
	}
	if err = <-frameDone; err != nil {
		return 125, err
	}
	if waitErr == nil {
		return 0, nil
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return 125, refusal("host", "drain", waitErr)
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 125, refusal("host", "client-wait", waitErr)
}
