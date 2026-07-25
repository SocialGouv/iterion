//go:build linux

package model

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestConfigureToolNodeProcessGroupKillsDescendants proves the regression that
// motivated the process-group wrapper: cancelling the direct recipe process
// must also stop a child that would otherwise be reparented to PID 1 and keep
// working after the run has terminated.
func TestConfigureToolNodeProcessGroupKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "orphan-finished")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestToolNodeProcessGroupHelper$", "--", marker)
	cmd.Env = append(os.Environ(), "GO_WANT_TOOL_NODE_PROCESS_HELPER=1")
	configureToolNodeProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		if cmd.ProcessState != nil {
			return
		}
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			_ = killToolNodeProcessTree(cmd.Process.Pid)
		}
	})

	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "ready" {
			t.Fatalf("helper readiness = %q, want ready", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not become ready")
	}

	cancel()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled process group did not exit")
	}

	// The descendant writes this marker after one second. If only the helper
	// leader was killed it survives under PID 1 and the marker appears.
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("descendant survived context cancellation and completed its work")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

func TestHostToolNodeCommandsConfigureProcessTreeCancellation(t *testing.T) {
	executor := &ClawExecutor{}
	commands := map[string]*exec.Cmd{
		"shell":  executor.toolNodeCommand(context.Background(), "true", nil),
		"script": executor.toolNodeScriptCommand(context.Background(), "sh", "recipe.sh"),
	}
	for name, cmd := range commands {
		t.Run(name, func(t *testing.T) {
			if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
				t.Fatal("host tool command is not isolated in a process group")
			}
			if cmd.Cancel == nil {
				t.Fatal("host tool command kept leader-only CommandContext cancellation")
			}
		})
	}
}

func TestToolNodeProcessGroupHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TOOL_NODE_PROCESS_HELPER") != "1" {
		return
	}
	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) {
		os.Exit(2)
	}
	marker := args[sep+1]
	child := exec.Command("sh", "-c", "sleep 1; printf done > \"$1\"", "tool-child", marker)
	// Escape the helper's process group exactly like a Python
	// Popen(start_new_session=True). Group-only cancellation would leave this
	// process alive under PID 1; the Linux procfs tree walk must still find it.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Hour)
	}
}
