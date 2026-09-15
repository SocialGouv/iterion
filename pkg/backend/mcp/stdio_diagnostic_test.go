package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const startupTestSecret = "plain-secret-with-no-recognizable-token-format"
const startupTestAuthURL = "https://user:password@private.invalid/auth?token=do-not-publish"

func TestStartupStderrBoundAndClear(t *testing.T) {
	s := &startupStderr{}
	for _, payload := range [][]byte{[]byte("prefix"), bytes.Repeat([]byte(startupTestSecret), 10000), []byte(" Unauthorized: " + startupTestAuthURL)} {
		if n, err := s.Write(payload); n != len(payload) || err != nil {
			t.Fatalf("drain: n=%d err=%v", n, err)
		}
		if s.size > startupStderrLimit {
			t.Fatalf("retained %d bytes", s.size)
		}
	}
	summary := s.finish()
	if !summary.truncated || summary.hint != "authentication" || summary.bytes <= startupStderrLimit {
		t.Fatalf("summary: %+v", summary)
	}
	if s.size != 0 || s.tail != [startupStderrLimit]byte{} {
		t.Fatal("raw stderr retained after initialization")
	}
	_, _ = s.Write([]byte(startupTestSecret))
	if s.size != 0 || s.tail != [startupStderrLimit]byte{} {
		t.Fatal("successful/finished capture must keep draining without retaining bytes")
	}
}

func TestStdioStartupFailureSafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want []string
	}{
		{"huge-secret-exit", []string{"exit_code=23", "stderr_truncated=true", "stderr_hint=authentication"}},
		{"silent-exit", []string{"exit_code=9", "stderr_bytes=0", "stderr_hint=none"}},
		{"secret-protocol-error", []string{"exit_code=0", "reason=protocol_or_startup_failure", "stderr_hint=configuration"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := newSDKClient(startupHelperConfig(tc.mode), clientInfo{Name: "test", Version: "1"})
			t.Cleanup(func() { _ = client.Close() })
			_, err := client.ListTools(ctx)
			assertSafeStartupError(t, err, tc.want...)
			// A permanent start failure remains cached, with safe formatting.
			_, again := client.ListTools(ctx)
			if again != err {
				t.Fatal("permanent failure unexpectedly retried")
			}
		})
	}
}

func TestStdioStartupCommandFailureDoesNotPrintCommand(t *testing.T) {
	client := newSDKClient(&ServerConfig{
		Name: "missing", Transport: TransportStdio,
		Command: filepath.Join(t.TempDir(), startupTestSecret),
		Args:    []string{startupTestAuthURL},
	}, clientInfo{Name: "test", Version: "1"})
	_, err := client.ListTools(context.Background())
	assertSafeStartupError(t, err, "reason=command_not_found", "exit_code=unavailable", "stderr_bytes=0")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost missing-file identity: %v", err)
	}
}

func TestStdioStartupCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		err  error
	}{
		{"cancel", "wait-for-close", context.Canceled},
		{"deadline", "wait-for-close", context.DeadlineExceeded},
		{"forced-stop", "ignore-close", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := startupHelperConfig(tc.mode)
			ready := filepath.Join(t.TempDir(), "ready")
			cfg.Env["ITERION_TEST_MCP_READY"] = ready
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.name == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			client := newSDKClient(cfg, clientInfo{Name: "test", Version: "1"})
			result := make(chan error, 1)
			go func() { _, err := client.ListTools(ctx); result <- err }()
			waitStartupHelperFile(t, ready)
			if tc.name != "deadline" {
				cancel()
			}
			select {
			case err := <-result:
				assertSafeStartupError(t, err, "stderr_hint=network")
				if tc.name == "forced-stop" {
					assertSafeStartupError(t, err, "exit_code=-1")
				}
				if !errors.Is(err, tc.err) {
					t.Fatalf("lost caller cancellation identity: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("startup did not stop after cancellation")
			}
			if err := client.Close(); err != nil {
				t.Fatalf("close failed startup: %v", err)
			}
		})
	}
}

func TestStdioStartupInheritedStderrIsBounded(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	cfg := startupHelperConfig("spawn-stderr-holder")
	cfg.Env["ITERION_TEST_MCP_CHILD_PID"] = pidFile
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ := strconv.Atoi(string(data))
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
				_ = process.Release()
			}
		}
	})
	client := newSDKClient(cfg, clientInfo{Name: "test", Version: "1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	_, err := client.ListTools(ctx)
	assertSafeStartupError(t, err, "exit_code=7", "stderr_hint=dependency")
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("stderr copier blocked shutdown for %s", elapsed)
	}
}

func TestStdioStartupSuccessRetainsProtocolAndCloses(t *testing.T) {
	cfg := startupHelperConfig("success")
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(os.Environ(), "ITERION_TEST_MCP_STARTUP_MODE=success")
	transport := newDiagnosticCommandTransport(cmd)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(transport.startupError(err))
	}
	t.Cleanup(func() { _ = session.Close() })
	transport.stderr.finish()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "still connected"}})
	if err != nil || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "still connected" {
		t.Fatalf("tool call after initialization: result=%+v err=%v", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || !cmd.ProcessState.Success() {
		t.Fatal("SDK did not reap the successful process")
	}
	if transport.stderr.size != 0 || transport.stderr.tail != [startupStderrLimit]byte{} {
		t.Fatal("successful process retained startup stderr")
	}
}

func startupHelperConfig(mode string) *ServerConfig {
	return &ServerConfig{
		Name: "diagnostic-test", Transport: TransportStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPStartupHelperProcess$", "--", startupTestAuthURL},
		Env:  map[string]string{"ITERION_TEST_MCP_STARTUP_MODE": mode, "ITERION_TEST_MCP_SECRET": startupTestSecret},
	}
}

func assertSafeStartupError(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected startup failure")
	}
	message := fmt.Sprintf("%+v", err)
	for _, secret := range []string{startupTestSecret, startupTestAuthURL, "private.invalid", "password", "do-not-publish"} {
		if strings.Contains(message, secret) {
			t.Fatal("startup diagnostic leaked subprocess data")
		}
	}
	for _, marker := range append(want, "raw diagnostics withheld") {
		if !strings.Contains(message, marker) {
			t.Fatalf("missing %q in %s", marker, message)
		}
	}
}

func waitStartupHelperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not become ready")
}

// TestMCPStartupHelperProcess is only a subprocess entry point. No external
// server, credential, package manager or network is used by these tests.
func TestMCPStartupHelperProcess(t *testing.T) {
	mode := os.Getenv("ITERION_TEST_MCP_STARTUP_MODE")
	if mode == "" {
		return
	}
	secretLine := startupTestSecret + " " + startupTestAuthURL
	switch mode {
	case "huge-secret-exit":
		for range 256 {
			_, _ = os.Stderr.Write(bytes.Repeat([]byte(secretLine), 256))
		}
		_, _ = fmt.Fprintln(os.Stderr, "Unauthorized "+secretLine)
		os.Exit(23)
	case "silent-exit":
		os.Exit(9)
	case "wait-for-close", "ignore-close":
		if mode == "ignore-close" {
			signal.Ignore(syscall.SIGTERM)
		}
		_, _ = fmt.Fprintln(os.Stderr, "ETIMEDOUT "+secretLine)
		_ = os.WriteFile(os.Getenv("ITERION_TEST_MCP_READY"), []byte("ready"), 0o600)
		if mode == "ignore-close" {
			time.Sleep(30 * time.Second)
			os.Exit(97)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "secret-protocol-error":
		_, _ = fmt.Fprintln(os.Stderr, "unknown option "+secretLine)
		decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if decoder.Decode(&request) != nil {
				os.Exit(0)
			}
			if len(request.ID) > 0 {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32000, "message": secretLine}})
			}
		}
	case "spawn-stderr-holder":
		child := exec.Command(os.Args[0], "-test.run=^TestMCPStartupHelperProcess$")
		child.Env = []string{"ITERION_TEST_MCP_STARTUP_MODE=hold-stderr"}
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(99)
		}
		_ = os.WriteFile(os.Getenv("ITERION_TEST_MCP_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		_, _ = fmt.Fprintln(os.Stderr, "Cannot find module "+secretLine)
		os.Exit(7)
	case "hold-stderr":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "success":
		_, _ = fmt.Fprintln(os.Stderr, secretLine)
		runStdioHelperProcess()
	default:
		os.Exit(98)
	}
}
