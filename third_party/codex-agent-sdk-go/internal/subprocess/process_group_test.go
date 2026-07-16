//go:build !windows

package subprocess

import (
	"bufio"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKillProcessTree_KillsChildProcesses(t *testing.T) {
	cmd, childPID := startProcessGroupHelper(t)

	require.Eventually(t, func() bool {
		return processExists(childPID)
	}, 2*time.Second, 25*time.Millisecond)

	require.NoError(t, killProcessTree(cmd))

	require.Eventually(t, func() bool {
		return !processExists(childPID)
	}, 2*time.Second, 25*time.Millisecond)

	waitErr := cmd.Wait()
	require.Error(t, waitErr)
}

func TestCLITransport_ContextCancellationKillsProcessTree(t *testing.T) {
	cmd, childPID := startProcessGroupHelper(t)

	ctx, cancel := context.WithCancel(context.Background())
	transport := &CLITransport{
		log:     slog.Default(),
		cmd:     cmd,
		closeCh: make(chan struct{}),
	}
	transport.watchContextCancellation(ctx)

	cancel()

	require.Eventually(t, func() bool {
		return !processExists(childPID)
	}, 2*time.Second, 25*time.Millisecond)

	waitErr := cmd.Wait()
	require.Error(t, waitErr)
}

func TestAppServerTransport_ContextCancellationKillsProcessTree(t *testing.T) {
	cmd, childPID := startProcessGroupHelper(t)

	ctx, cancel := context.WithCancel(context.Background())
	transport := &AppServerTransport{
		log:     slog.Default(),
		cmd:     cmd,
		closeCh: make(chan struct{}),
	}
	transport.watchContextCancellation(ctx)

	cancel()

	require.Eventually(t, func() bool {
		return !processExists(childPID)
	}, 2*time.Second, 25*time.Millisecond)
}

func startProcessGroupHelper(t *testing.T) (*exec.Cmd, int) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestProcessGroupHelper", "--", "spawn-child")

	cmd.Env = append(os.Environ(), "GO_WANT_PROCESS_GROUP_HELPER=1")
	configureProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)

	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)

	return cmd, childPID
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || !stderrors.Is(err, syscall.ESRCH)
}

func TestProcessGroupHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_GROUP_HELPER") != "1" {
		return
	}

	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "spawn-child" {
		os.Exit(2)
	}

	child := exec.Command("sleep", "30")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "start child: %v\n", err)

		os.Exit(2)
	}

	fmt.Println(child.Process.Pid)

	for {
		time.Sleep(time.Second)
	}
}
