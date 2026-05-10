package claudesdk

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// When a CommandBuilder is configured (sandbox-routed), the explicit cliPath
// must pass through unchanged: the binary is resolved by the builder's
// runtime (e.g. PATH inside a docker container), not by host-side os.Stat.
// This guards against the regression where bb95150's WithCLIPath("claude")
// was rejected because the host had no ./claude file.
func TestResolveCLIPath_SandboxRoutedSkipsHostValidation(t *testing.T) {
	cb := func(ctx context.Context, path string, args []string, cwd string, env map[string]string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	cfg := applyOptions([]Option{
		WithCLIPath("claude"),
		WithCommandBuilder(cb),
	})

	got, err := resolveCLIPath(cfg)
	if err != nil {
		t.Fatalf("resolveCLIPath returned error: %v", err)
	}
	if got != "claude" {
		t.Errorf("cliPath = %q, want %q (bare name to be resolved in container)", got, "claude")
	}
}

// Same path with no explicit cliPath: defaults to "claude" so the container
// PATH lookup wins. Host-side findCLI must NOT be consulted.
func TestResolveCLIPath_SandboxRoutedDefaultsToClaude(t *testing.T) {
	cb := func(ctx context.Context, path string, args []string, cwd string, env map[string]string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	cfg := applyOptions([]Option{WithCommandBuilder(cb)})

	got, err := resolveCLIPath(cfg)
	if err != nil {
		t.Fatalf("resolveCLIPath returned error: %v", err)
	}
	if got != "claude" {
		t.Errorf("cliPath = %q, want %q", got, "claude")
	}
}

// Without a CommandBuilder, the explicit path is host-validated. A bare name
// that doesn't exist in cwd nor on PATH must surface the not-found error so
// host-execution callers fail loudly instead of silently spawning a missing
// binary.
func TestResolveCLIPath_HostExecValidatesExplicit(t *testing.T) {
	cfg := applyOptions([]Option{WithCLIPath("definitely-not-a-real-binary-12345")})

	_, err := resolveCLIPath(cfg)
	if err == nil {
		t.Fatal("expected error for non-existent host cliPath, got nil")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-binary-12345") {
		t.Errorf("error %q should mention the searched path", err.Error())
	}
}
