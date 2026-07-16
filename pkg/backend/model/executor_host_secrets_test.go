package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestExecutorHostFileSecret_MaterialisesAndReadsBack drives the fix
// end-to-end: on a HOST (no-sandbox) run, `{{secrets.X.path}}` must
// resolve to a real host file whose content is the plaintext value —
// the exact scenario the veille bot hit before the fix
// (path_ref='/run/iterion/secrets/webhooks' MISSING).
func TestExecutorHostFileSecret_MaterialisesAndReadsBack(t *testing.T) {
	const payload = "https://hook.example/w1"
	guard := secretguard.New([]secretguard.Secret{{
		Name:     "webhooks",
		Value:    payload,
		FilePath: "/run/iterion/secrets/webhooks",
	}}, secretguard.DefaultConfig())

	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })

	// A tool node whose shell script reads the host path and echoes the
	// content — proves the template resolves to a real file, and the file
	// contains the expected plaintext.
	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "read_webhooks"},
		Command:  "cat {{secrets.webhooks.path}}",
		CommandRefs: []*ir.Ref{{
			Kind: ir.RefSecrets, Path: []string{"webhooks", "path"},
			Raw: "{{secrets.webhooks.path}}",
		}},
	}
	out, err := exec.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, _ := out["result"].(string); !strings.Contains(got, payload) {
		t.Fatalf("tool node did not read materialised host file (got %q, want to contain %q)", got, payload)
	}

	// The guard's file path should now be a host path (not the sandbox mount).
	hostPath := guard.ResolveSecretRef("webhooks")
	if hostPath == "/run/iterion/secrets/webhooks" {
		t.Fatal("host materialisation did not rewrite the guard path")
	}
	if !filepath.IsAbs(hostPath) {
		t.Fatalf("host path is not absolute: %q", hostPath)
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		t.Fatalf("stat host secret file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("host secret perms = %v, want 0600", info.Mode().Perm())
	}

	// Close removes the tempdir.
	dir := filepath.Dir(hostPath)
	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Close did not remove host secret tempdir %q: %v", dir, err)
	}
}

// TestExecutorHostFileSecret_OnceAcrossExecutions verifies the sync.Once
// gate: two Execute calls reuse the same host tempfile, so the guard is
// rewritten exactly once.
func TestExecutorHostFileSecret_OnceAcrossExecutions(t *testing.T) {
	guard := secretguard.New([]secretguard.Secret{{
		Name:     "tok",
		Value:    "v-abcdef",
		FilePath: "/run/iterion/secrets/tok",
	}}, secretguard.DefaultConfig())
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n"},
		Command:  "echo {{secrets.tok.path}}",
		CommandRefs: []*ir.Ref{{
			Kind: ir.RefSecrets, Path: []string{"tok", "path"},
			Raw: "{{secrets.tok.path}}",
		}},
	}
	if _, err := exec.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("Execute #1: %v", err)
	}
	first := guard.ResolveSecretRef("tok")
	if _, err := exec.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("Execute #2: %v", err)
	}
	if second := guard.ResolveSecretRef("tok"); second != first {
		t.Fatalf("host path changed between calls (%q vs %q); sync.Once should hold", first, second)
	}
}

// TestExecutorHostFileSecret_SandboxPathUnchanged is the byte-for-byte
// safety net for the sandbox path: with a live sandbox handle,
// ensureHostSecretFiles is a strict no-op — the guard's file path stays
// at the sandbox mount, no tempdir is created, and Close has nothing to
// clean up.
func TestExecutorHostFileSecret_SandboxPathUnchanged(t *testing.T) {
	guard := secretguard.New([]secretguard.Secret{{
		Name:     "webhooks",
		Value:    "sandbox-owned",
		FilePath: "/run/iterion/secrets/webhooks",
	}}, secretguard.DefaultConfig())
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithSecretGuard(guard))
	exec.SetSandbox(fakeSandboxRun{})
	t.Cleanup(func() { _ = exec.Close() })

	if err := exec.ensureHostSecretFiles(); err != nil {
		t.Fatalf("ensureHostSecretFiles (sandbox): %v", err)
	}
	if got := guard.ResolveSecretRef("webhooks"); got != "/run/iterion/secrets/webhooks" {
		t.Errorf("sandbox path mutated (got %q); host materialisation must not touch sandbox runs", got)
	}
	hints := guard.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != "/run/iterion/secrets/webhooks" {
		t.Errorf("sandbox hints mutated: %+v", hints)
	}
	if exec.hostSecretCleanup != nil {
		t.Error("no host cleanup should be installed on the sandbox path")
	}
}

// TestExecutorHostFileSecret_NoGuardNoOp checks the cheap fast-path: an
// executor with no secret guard sees ensureHostSecretFiles as a no-op
// (nil-safe) and installs no cleanup.
func TestExecutorHostFileSecret_NoGuardNoOp(t *testing.T) {
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{})
	t.Cleanup(func() { _ = exec.Close() })
	if err := exec.ensureHostSecretFiles(); err != nil {
		t.Fatalf("ensureHostSecretFiles: %v", err)
	}
	if exec.hostSecretCleanup != nil {
		t.Error("no cleanup expected on the guardless path")
	}
}

// fakeSandboxRun is the minimum satisfying sandbox.Run implementation
// for the "sandbox path unchanged" test — it never has to actually run
// anything because ensureHostSecretFiles bails out on `e.sandbox != nil`.
type fakeSandboxRun struct{}

func (fakeSandboxRun) Driver() string { return "fake" }
func (fakeSandboxRun) Command(ctx context.Context, _ []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}
func (fakeSandboxRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (fakeSandboxRun) Cleanup(context.Context) error { return nil }
