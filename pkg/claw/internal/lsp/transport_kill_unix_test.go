//go:build !windows

package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// startReleasedSleep starts a process and releases its handle: the OS still
// runs it, and a Kill through the handle is refused — with an error that is
// not os.ErrProcessDone. The cleanup ends the process by pid.
func startReleasedSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	return cmd
}

// A kill the OS refuses is reported, not taken for an ended server.
func TestLspTransportKillReportsARefusedKill(t *testing.T) {
	transport := &LspTransport{cmd: startReleasedSleep(t)}
	err := transport.kill()
	if err == nil || !strings.Contains(err.Error(), "lsp kill") {
		t.Fatalf("kill on a released handle = %v, want the refusal reported", err)
	}
}

// When the server does not take the shutdown request, Shutdown ends it and
// reports both: the request that failed and a kill that failed too.
func TestLspTransportShutdownCarriesAFailedKill(t *testing.T) {
	r, w := io.Pipe()
	_ = r.Close() // every write to w now fails: the request never reaches a server
	transport := &LspTransport{
		cmd:     startReleasedSleep(t),
		stdin:   w,
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
		cancel:  func() {},
	}
	err := transport.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lsp shutdown") || !strings.Contains(err.Error(), "lsp kill") {
		t.Fatalf("Shutdown = %v, want the failed request and the failed kill", err)
	}
}

// When the server takes the shutdown request but cannot hear "exit",
// Shutdown ends it, and reports a kill that fails.
func TestLspTransportShutdownReportsAFailedKillWhenExitIsLost(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	t.Cleanup(func() { _ = serverToClientW.Close() })
	transport := &LspTransport{
		cmd:     startReleasedSleep(t),
		stdin:   clientToServerW,
		stdout:  bufio.NewReader(serverToClientR),
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
		cancel:  func() {},
	}
	go mockServer(t, bufio.NewReader(clientToServerR), serverToClientW, func(req jsonRPCRequest) interface{} {
		if req.Method == "shutdown" {
			_ = clientToServerR.Close() // "exit" now has nowhere to go
		}
		return nil
	})
	go transport.readLoop()

	err := transport.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lsp kill") {
		t.Fatalf("Shutdown = %v, want the failed kill of a server deaf to exit", err)
	}
}
