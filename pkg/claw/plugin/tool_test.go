package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test, skipping on Windows")
	}
}

func testTool(name, command string, args []string, root string) *PluginTool {
	return &PluginTool{
		PluginID:   "test-plugin",
		PluginName: "TestPlugin",
		Definition: PluginToolDefinition{Name: name},
		Command:    command,
		Args:       args,
		Root:       root,
	}
}

func TestExecuteSetsEnvVars(t *testing.T) {
	t.Parallel()

	var cmd, arg string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		arg = "/c set"
	} else {
		cmd = "env"
		arg = ""
	}

	var args []string
	if arg != "" {
		args = []string{arg}
	}

	tool := testTool("echo_tool", cmd, args, t.TempDir())

	input := json.RawMessage(`{"key":"value"}`)
	output, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	for _, want := range []string{
		"CLAWD_PLUGIN_ID=test-plugin",
		"CLAWD_PLUGIN_NAME=TestPlugin",
		"CLAWD_TOOL_NAME=echo_tool",
		`CLAWD_TOOL_INPUT={"key":"value"}`,
		"ITERION_PLUGIN_ID=test-plugin",
		"ITERION_PLUGIN_NAME=TestPlugin",
		"ITERION_TOOL_NAME=echo_tool",
		`ITERION_TOOL_INPUT={"key":"value"}`,
		"CLAWD_PLUGIN_ROOT=",
		"ITERION_PLUGIN_ROOT=",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing env var %q", want)
		}
	}
}

func TestExecuteExitCodeError(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("fail_tool", "bash", []string{"-c", "exit 42"}, t.TempDir())

	_, err := tool.Execute(json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}

	pe, ok := err.(*PluginError)
	if !ok {
		t.Fatalf("expected *PluginError, got %T: %v", err, err)
	}
	if pe.Kind != ErrCommandFailed {
		t.Errorf("expected Kind=ErrCommandFailed, got %q", pe.Kind)
	}
	if !strings.Contains(pe.Message, "exit status 42") {
		t.Errorf("expected 'exit status 42' in message, got %q", pe.Message)
	}
}

func TestExecuteStdinJSON(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("stdin_tool", "cat", nil, t.TempDir())

	input := json.RawMessage(`{"hello":"world","num":42}`)
	output, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if output != `{"hello":"world","num":42}` {
		t.Errorf("expected stdin JSON echoed back, got %q", output)
	}
}

func TestExecuteStderrCaptured(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("stderr_tool", "bash",
		[]string{"-c", "echo 'custom error message' >&2; exit 1"}, t.TempDir())

	_, err := tool.Execute(json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}

	pe := err.(*PluginError)
	if !strings.Contains(pe.Message, "custom error message") {
		t.Errorf("expected stderr content in error message, got %q", pe.Message)
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("hang_tool", "sleep", []string{"60"}, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tool.ExecuteContext(ctx, json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if elapsed > 5*time.Second {
		t.Errorf("context cancellation took too long: %v", elapsed)
	}
}

func TestExecuteWorkingDirectory(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tmpDir := t.TempDir()
	tool := testTool("pwd_tool", "pwd", nil, tmpDir)

	output, err := tool.Execute(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	// Resolve symlinks for comparison (macOS /tmp is a symlink).
	resolvedTmp, _ := filepath.EvalSymlinks(tmpDir)
	resolvedOut, _ := filepath.EvalSymlinks(output)
	if resolvedOut != resolvedTmp {
		t.Errorf("expected working dir %q, got %q", resolvedTmp, resolvedOut)
	}
}

func TestExecuteNoRootNoDir(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("no_root_tool", "echo", []string{"hello"}, "")

	output, err := tool.Execute(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if output != "hello" {
		t.Errorf("expected 'hello', got %q", output)
	}
}

func TestExecuteLargeStdout(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tool := testTool("large_output_tool", "bash",
		[]string{"-c", "dd if=/dev/zero bs=1024 count=100 2>/dev/null | base64"}, t.TempDir())

	output, err := tool.Execute(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if len(output) < 1000 {
		t.Errorf("expected large output (>1000 bytes), got %d bytes", len(output))
	}
}

func TestExecuteTempScript(t *testing.T) {
	t.Parallel()
	skipWindows(t)

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "test-script.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\necho \"plugin output: $CLAWD_TOOL_NAME\"\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	tool := &PluginTool{
		PluginID:   "script-plugin",
		PluginName: "ScriptPlugin",
		Definition: PluginToolDefinition{Name: "my_script_tool"},
		Command:    scriptPath,
		Root:       tmpDir,
	}

	output, err := tool.Execute(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if output != "plugin output: my_script_tool" {
		t.Errorf("expected 'plugin output: my_script_tool', got %q", output)
	}
}
