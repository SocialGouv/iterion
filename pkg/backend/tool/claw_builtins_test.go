package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
	clawlsp "github.com/SocialGouv/claw-code-go/pkg/api/lsp"
	clawmcp "github.com/SocialGouv/claw-code-go/pkg/api/mcp"
	clawtask "github.com/SocialGouv/claw-code-go/pkg/api/task"
	clawteam "github.com/SocialGouv/claw-code-go/pkg/api/team"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
	clawworker "github.com/SocialGouv/claw-code-go/pkg/api/worker"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
)

func hasTool(r *Registry, name string) bool {
	_, err := r.Resolve(name)
	return err == nil
}

// skipIfComputerUseAvailable skips the test when the host actually has
// the X11 stack the *Unavailable tests assume is missing. CI in stripped
// containers genuinely is headless; a developer machine with xorg +
// xdotool + ImageMagick (`import`) on PATH is not — letting these
// tests run there asserts an error that legitimately won't fire.
func skipIfComputerUseAvailable(t *testing.T) {
	t.Helper()
	if os.Getenv("DISPLAY") == "" {
		return
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return
	}
	if _, err := exec.LookPath("import"); err != nil {
		return
	}
	t.Skip("computer-use stack is available on this host (DISPLAY + xdotool + import) — skipping headless-only assertion")
}

func TestRegisterClawBuiltins_RegistersStandardSet(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, ""); err != nil {
		t.Fatalf("RegisterClawBuiltins: %v", err)
	}
	want := []string{"read_file", "write_file", "glob", "grep", "file_edit", "web_fetch", "bash"}
	for _, name := range want {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawBuiltins_DoesNotRegisterComputerUse(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, ""); err != nil {
		t.Fatalf("RegisterClawBuiltins: %v", err)
	}
	for _, name := range []string{"read_image", "screenshot", "computer_use"} {
		if hasTool(r, name) {
			t.Errorf("expected %q NOT registered by default; vision tools are opt-in", name)
		}
	}
}

func TestRegisterClawWorkspaceDiagnostics_IsExplicitOptIn(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workspace_grep", "diagnostic_shell"} {
		if hasTool(r, name) {
			t.Fatalf("%q registered without diagnostic opt-in", name)
		}
	}
	if err := RegisterClawWorkspaceDiagnostics(r, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workspace_grep", "diagnostic_shell"} {
		if !hasTool(r, name) {
			t.Errorf("%q missing after diagnostic opt-in", name)
		}
	}
}

func TestClawShellToolsRunInTheirRegisteredWorkspace(t *testing.T) {
	workspace := t.TempDir()
	otherDir := t.TempDir()
	t.Chdir(otherDir)

	r := NewRegistry()
	if err := RegisterClawBuiltins(r, workspace); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(r, workspace, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bash", "diagnostic_shell"} {
		t.Run(name, func(t *testing.T) {
			td, err := r.Resolve(name)
			if err != nil {
				t.Fatal(err)
			}
			out, err := td.Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := filepath.Clean(strings.TrimSpace(out)); got != workspace {
				t.Fatalf("%s cwd = %q, want registered workspace %q", name, got, workspace)
			}
		})
	}
}

// TestDiagnosticShellCharacterizesFailureOutput pins the dependency contract
// Copi relies on for diagnostics: the public claw executor returns combined
// output alongside a non-nil error when a command exits non-zero. The model
// layer decides how to present that pair; this test must fail if a dependency
// upgrade changes either half of the contract.
func TestDiagnosticShellCharacterizesFailureOutput(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(r, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	td, err := r.Resolve("diagnostic_shell")
	if err != nil {
		t.Fatal(err)
	}

	out, err := td.Execute(context.Background(), json.RawMessage(`{"command":"printf stdout; printf stderr >&2; exit 7"}`))
	if !strings.Contains(out, "stdout") || !strings.Contains(out, "stderr") {
		t.Fatalf("non-zero diagnostic output = %q, want combined stdout and stderr", out)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "command exited with error:") {
		t.Fatalf("non-zero diagnostic error = %v, want documented exit error", err)
	}
}

func TestDiagnosticShellCharacterizesInvalidInput(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(r, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	td, err := r.Resolve("diagnostic_shell")
	if err != nil {
		t.Fatal(err)
	}

	out, err := td.Execute(context.Background(), json.RawMessage(`{}`))
	if out != "" {
		t.Fatalf("invalid diagnostic output = %q, want empty", out)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "bash:") {
		t.Fatalf("invalid diagnostic error = %v, want validation error", err)
	}
}

func TestClawReadFile_LargeFileHasExplicitContinuation(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	for i := 1; i <= 12_000; i++ {
		fmt.Fprintf(&body, "line-%05d: payload payload payload\n", i)
	}
	path := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, dir); err != nil {
		t.Fatal(err)
	}
	td, err := r.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{"path":"large.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 256*1024 {
		t.Fatalf("read output crossed downstream safety boundary: %d bytes", len(out))
	}
	if !strings.Contains(out, "[read_file partial:") || !strings.Contains(out, "continue with path \"large.txt\" and start_line") {
		t.Fatalf("large read did not advertise continuation: tail=%q", out[len(out)-300:])
	}
	if strings.Contains(out, "line-12000") {
		t.Fatal("first chunk unexpectedly contained the file tail")
	}

	out, err = td.Execute(context.Background(), json.RawMessage(`{"path":"large.txt","start_line":11995}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line-12000") || strings.Contains(out, "[read_file partial:") {
		t.Fatalf("continuation did not return the complete tail: %q", out)
	}
}

func TestClawGrep_SearchesWorkspaceButSkipsCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state", "films"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "films", "timing.json"), []byte("diagnostic-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env.bak"), []byte("PASSWORD=diagnostic-needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ssh", "id_rsa"), []byte("diagnostic-needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	if err := RegisterClawBuiltins(r, dir); err != nil {
		t.Fatal(err)
	}
	td, err := r.Resolve("grep")
	if err != nil {
		t.Fatal(err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{"pattern":"diagnostic-needle","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timing.json:1:diagnostic-needle") {
		t.Fatalf("workspace artifact match missing: %q", out)
	}
	if strings.Contains(out, ".env") || strings.Contains(out, ".ssh") || strings.Contains(out, "PASSWORD") {
		t.Fatalf("credential file leaked through grep: %q", out)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("diagnostic-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"pattern": "diagnostic-needle", "path": outside})
	if _, err := td.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("outside-workspace grep = %v, want refusal", err)
	}
}

func TestClawWorkspaceGrep_BoundsOutputDuringScan(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	for i := range 40 {
		fmt.Fprintf(&body, "needle-%02d %s\n", i, strings.Repeat("x", 48))
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	const maxOutput = 512
	out, err := executeWorkspaceGrepWithLimits(
		map[string]any{"pattern": "needle-", "path": "."},
		dir,
		workspaceGrepLimits{maxResults: 100, maxOutputBytes: maxOutput},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutput {
		t.Fatalf("grep output crossed byte limit: got %d, max %d", len(out), maxOutput)
	}
	if !strings.Contains(out, "needle-00") || strings.Contains(out, "needle-39") {
		t.Fatalf("grep did not preserve the bounded prefix: %q", out)
	}
	if !strings.Contains(out, "[grep partial: stopped at output limit (512 bytes)") {
		t.Fatalf("grep did not advertise byte truncation: %q", out)
	}
}

func TestClawWorkspaceGrep_OversizedFirstMatchReportsPartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "single.txt"),
		[]byte("needle "+strings.Repeat("x", 2_000)+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	const maxOutput = 256
	out, err := executeWorkspaceGrepWithLimits(
		map[string]any{"pattern": "needle", "path": "."},
		dir,
		workspaceGrepLimits{maxResults: 100, maxOutputBytes: maxOutput},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutput {
		t.Fatalf("grep output crossed byte limit: got %d, max %d", len(out), maxOutput)
	}
	if strings.Contains(out, "No matches found") || !strings.Contains(out, "[grep partial: stopped at output limit (256 bytes)") {
		t.Fatalf("oversized first match was reported incorrectly: %q", out)
	}
}

func TestClawWorkspaceGrep_ReportsInjectedResultLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "many.txt"),
		[]byte("needle-0\nneedle-1\nneedle-2\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	out, err := executeWorkspaceGrepWithLimits(
		map[string]any{"pattern": "needle-", "path": "."},
		dir,
		workspaceGrepLimits{maxResults: 2, maxOutputBytes: 512},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "needle-0") || !strings.Contains(out, "needle-1") || strings.Contains(out, "needle-2") {
		t.Fatalf("grep result limit kept the wrong matches: %q", out)
	}
	if !strings.Contains(out, "[grep partial: stopped at result limit (2)") {
		t.Fatalf("grep did not advertise the injected result limit: %q", out)
	}
}

func TestClawGlob_IsWorkspaceBoundedAndProtectsSensitivePaths(t *testing.T) {
	workspace := t.TempDir()
	for path, contents := range map[string]string{
		"bots/planner/manifest.yaml":     "planner",
		"node_modules/pkg/manifest.yaml": "dependency",
		".ssh/manifest.yaml":             "secret",
		".env/manifest.yaml":             "secret",
	} {
		fullPath := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	r := NewRegistry()
	if err := RegisterClawBuiltins(r, workspace); err != nil {
		t.Fatal(err)
	}
	td, err := r.Resolve("glob")
	if err != nil {
		t.Fatal(err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{"path":".","pattern":"**/manifest.yaml"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bots/planner/manifest.yaml") {
		t.Fatalf("workspace match missing: %q", out)
	}
	if strings.Contains(out, "node_modules") || strings.Contains(out, ".ssh") || strings.Contains(out, ".env") {
		t.Fatalf("ignored or sensitive path leaked: %q", out)
	}
	out, err = td.Execute(context.Background(), json.RawMessage(`{"path":".","pattern":"bots/*/manifest.yaml"}`))
	if err != nil || out != "bots/planner/manifest.yaml" {
		t.Fatalf("non-recursive workspace glob = %q, %v", out, err)
	}

	if _, err := td.Execute(context.Background(), json.RawMessage(`{"path":"../","pattern":"**/manifest.yaml"}`)); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("outside-workspace glob = %v, want refusal", err)
	}
	if _, err := td.Execute(context.Background(), json.RawMessage(`{"path":".","pattern":"../manifest.yaml"}`)); err == nil || !strings.Contains(err.Error(), "must not traverse") {
		t.Fatalf("traversing pattern = %v, want refusal", err)
	}
}

func TestWorkspaceGlob_StopsOnCancellationAndTraversalBudget(t *testing.T) {
	workspace := t.TempDir()
	for i := 0; i < 6; i++ {
		path := filepath.Join(workspace, fmt.Sprintf("dir-%d", i), "marker.txt")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	limits := workspaceGlobLimits{maxResults: 100, maxOutputBytes: 10_000, maxVisitedEntries: 3}
	out, err := executeWorkspaceGlobWithLimits(context.Background(), map[string]any{"path": ".", "pattern": "**/absent.txt"}, workspace, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "visited-entry limit (3)") {
		t.Fatalf("walk did not advertise traversal cutoff: %q", out)
	}
	limits.maxVisitedEntries = 100
	limits.maxResults = 1
	out, err = executeWorkspaceGlobWithLimits(context.Background(), map[string]any{"path": ".", "pattern": "**/*.txt"}, workspace, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "result limit (1)") {
		t.Fatalf("walk did not advertise result cutoff: %q", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = executeWorkspaceGlobWithLimits(ctx, map[string]any{"path": ".", "pattern": "**/*.txt"}, workspace, defaultWorkspaceGlobLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled glob error = %v, want context.Canceled", err)
	}
}

func TestRegisterClawComputerUse_RegistersAll(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatalf("RegisterClawReadImage: %v", err)
	}
	for _, name := range []string{"read_image", "screenshot", "computer_use"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered after opt-in", name)
		}
	}
}

func TestRegisterClawComputerUse_ReadImageRoundTrip(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatalf("RegisterClawReadImage: %v", err)
	}

	// Tiny 1x1 transparent PNG.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tool, err := r.Resolve("read_image")
	if err != nil {
		t.Fatalf("read_image not in registry: %v", err)
	}
	input, _ := json.Marshal(map[string]any{"path": path})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute read_image: %v", err)
	}

	// Output is JSON-encoded ReadImageResult.
	var decoded struct {
		Description string                 `json:"description"`
		Blocks      []clawapi.ContentBlock `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(decoded.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(decoded.Blocks))
	}
	block := decoded.Blocks[0]
	if block.Type != "image" || block.Source == nil || block.Source.Type != "base64" {
		t.Errorf("expected base64 image block, got %+v", block)
	}
	if block.Source.MediaType != "image/png" {
		t.Errorf("expected image/png, got %q", block.Source.MediaType)
	}
	if block.Source.Data == "" {
		t.Errorf("expected non-empty base64 data")
	}
}

func TestRegisterClawComputerUse_ReadImagePropagatesError(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatalf("RegisterClawReadImage: %v", err)
	}
	tool, _ := r.Resolve("read_image")

	// Missing both path and url → underlying tool errors. The
	// adapter must surface that, not swallow it.
	input, _ := json.Marshal(map[string]any{})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Fatal("expected error when neither path nor url given")
	}
}

func TestRegisterClawComputerUse_ScreenshotPropagatesUnavailable(t *testing.T) {
	skipIfComputerUseAvailable(t)
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatalf("RegisterClawReadImage: %v", err)
	}
	tool, _ := r.Resolve("screenshot")

	input, _ := json.Marshal(map[string]any{})
	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error from screenshot in headless test env")
	}
	if !errors.Is(err, clawtools.ErrComputerUseUnavailable) {
		t.Fatalf("expected ErrComputerUseUnavailable to propagate through adapter, got %T: %v", err, err)
	}
}

// TestRegisterClawComputerUse_ComputerUsePropagatesUnavailable verifies
// the unified action dispatcher reaches the same gating: in a headless
// CI env, every action verb (left_click, type, key, …) bottoms out in
// xdotool / ImageMagick which we don't ship in the test container, so
// the adapter must surface ErrComputerUseUnavailable instead of
// silently returning a stub success.
func TestRegisterClawComputerUse_ComputerUsePropagatesUnavailable(t *testing.T) {
	skipIfComputerUseAvailable(t)
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatalf("RegisterClawReadImage: %v", err)
	}
	tool, err := r.Resolve("computer_use")
	if err != nil {
		t.Fatalf("computer_use not in registry: %v", err)
	}
	for _, action := range []string{"screenshot", "left_click", "type", "key", "mouse_move"} {
		input := map[string]any{"action": action}
		switch action {
		case "type":
			input["text"] = "hello"
		case "key":
			input["text"] = "Return"
		case "left_click", "mouse_move":
			input["coordinate"] = []any{10, 10}
		}
		raw, _ := json.Marshal(input)
		_, err := tool.Execute(context.Background(), raw)
		if err == nil {
			t.Errorf("action %q: expected error in headless test env", action)
			continue
		}
		if !errors.Is(err, clawtools.ErrComputerUseUnavailable) {
			t.Errorf("action %q: expected ErrComputerUseUnavailable, got %T: %v", action, err, err)
		}
	}
}

func TestRegisterClawComputerUse_ReadImageRejectsHTTPRedirect(t *testing.T) {
	// Defense-in-depth: the iterion-level adapter must inherit the
	// underlying tool's HTTPS-only check, including redirects.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("plain http should never be reached; redirect must abort")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/img.png", http.StatusFound)
	}))
	defer tls.Close()

	prev := http.DefaultClient
	http.DefaultClient = tls.Client()
	t.Cleanup(func() { http.DefaultClient = prev })

	r := NewRegistry()
	if err := RegisterClawReadImage(r); err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Resolve("read_image")
	input, _ := json.Marshal(map[string]any{"url": tls.URL + "/start"})
	_, err := tool.Execute(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("expected redirect-to-non-https error, got %v", err)
	}
}

func TestRegisterClawSimple_RegistersAll(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawSimple(r); err != nil {
		t.Fatalf("RegisterClawSimple: %v", err)
	}
	for _, name := range []string{"send_user_message", "remote_trigger", "sleep", "notebook_edit", "repl", "structured_output"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawTodo_Registered(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawTodo(r); err != nil {
		t.Fatalf("RegisterClawTodo: %v", err)
	}
	if !hasTool(r, "todo_write") {
		t.Errorf("todo_write not registered")
	}
}

func TestRegisterClawSubagents_Registered(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawSubagents(r, nil); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	if !hasTool(r, "agent") {
		t.Errorf("agent not registered")
	}
}

func TestRegisterClawWebSearch_Registered(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawWebSearch(r); err != nil {
		t.Fatalf("RegisterClawWebSearch: %v", err)
	}
	if !hasTool(r, "web_search") {
		t.Errorf("web_search not registered")
	}
}

func TestRegisterClawSkill_Registered(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawSkill(r, ""); err != nil {
		t.Fatalf("RegisterClawSkill: %v", err)
	}
	if !hasTool(r, "skill") {
		t.Errorf("skill not registered")
	}
}

func TestRegisterClawToolSearch_RegisteredAndQueriesSnapshot(t *testing.T) {
	r := NewRegistry()
	called := false
	snapshot := func() []clawapi.Tool {
		called = true
		return nil
	}
	if err := RegisterClawToolSearch(r, snapshot); err != nil {
		t.Fatalf("RegisterClawToolSearch: %v", err)
	}
	if !hasTool(r, "tool_search") {
		t.Fatalf("tool_search not registered")
	}
	td, _ := r.Resolve("tool_search")
	in, _ := json.Marshal(map[string]any{"query": "anything"})
	if _, err := td.Execute(context.Background(), in); err != nil {
		// The internal tool may complain about empty haystack; we
		// only care that the snapshot closure was invoked.
		_ = err
	}
	if !called {
		t.Errorf("snapshot closure was not invoked")
	}
}

func TestRegisterClawPlanMode_BothRegistered(t *testing.T) {
	r := NewRegistry()
	active := false
	state := &clawtools.PlanModeState{Active: &active, Dir: t.TempDir()}
	if err := RegisterClawPlanMode(r, state); err != nil {
		t.Fatalf("RegisterClawPlanMode: %v", err)
	}
	for _, name := range []string{"enter_plan_mode", "exit_plan_mode"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawTasks_All(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawTasks(r, clawtask.NewRegistry()); err != nil {
		t.Fatalf("RegisterClawTasks: %v", err)
	}
	for _, name := range []string{"task_create", "task_get", "task_list", "task_output", "task_stop", "task_update", "run_task_packet"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawTasks_NilRegistryFails(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawTasks(r, nil); err == nil {
		t.Errorf("expected error on nil task registry")
	}
}

func TestRegisterClawWorkers_All(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawWorkers(r, clawworker.NewWorkerRegistry()); err != nil {
		t.Fatalf("RegisterClawWorkers: %v", err)
	}
	for _, name := range []string{"worker_create", "worker_get", "worker_observe", "worker_resolve_trust", "worker_await_ready", "worker_send_prompt", "worker_restart", "worker_terminate", "worker_observe_completion"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawWorkers_NilRegistryFails(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawWorkers(r, nil); err == nil {
		t.Errorf("expected error on nil worker registry")
	}
}

func TestRegisterClawTeams_All(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawTeams(r, clawteam.NewTeamRegistry()); err != nil {
		t.Fatalf("RegisterClawTeams: %v", err)
	}
	for _, name := range []string{"team_create", "team_get", "team_list", "team_delete"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawCron_All(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawCron(r, clawteam.NewCronRegistry()); err != nil {
		t.Fatalf("RegisterClawCron: %v", err)
	}
	for _, name := range []string{"cron_create", "cron_get", "cron_list", "cron_delete"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawMCPResources_All(t *testing.T) {
	r := NewRegistry()
	provider := clawmcp.NewRegistryProvider(clawmcp.NewRegistry(), clawmcp.NewAuthState())
	if err := RegisterClawMCPResources(r, provider); err != nil {
		t.Fatalf("RegisterClawMCPResources: %v", err)
	}
	for _, name := range []string{"list_mcp_resources", "read_mcp_resource", "mcp_auth"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawMCPResources_NilProviderRejected(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawMCPResources(r, nil); err == nil {
		t.Errorf("expected nil provider to fail; got nil")
	}
}

func TestRegisterClawLSP_Registered(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawLSP(r, clawlsp.NewRegistry()); err != nil {
		t.Fatalf("RegisterClawLSP: %v", err)
	}
	if !hasTool(r, "lsp") {
		t.Errorf("lsp not registered")
	}
}

func TestRegisterClawAll_RegistersFullSet(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawAll(r, ClawDefaults{Workspace: t.TempDir()}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	// Spot-check one tool from each family.
	expected := []string{
		"read_file", "write_file", "bash", "glob", "grep", "file_edit", "web_fetch",
		"todo_write", "agent", "skill", "config",
		"send_user_message", "remote_trigger", "sleep", "notebook_edit", "repl", "structured_output",
		"task_create", "worker_create", "team_create", "cron_create",
		"list_mcp_resources", "lsp", "tool_search",
	}
	for _, name := range expected {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered by RegisterClawAll", name)
		}
	}
	// read_image is now always-on (no display needed); only screenshot
	// and computer_use stay opt-in via IncludeComputerUse.
	if !hasTool(r, "read_image") {
		t.Errorf("expected %q registered by RegisterClawAll (always-on, no display required)", "read_image")
	}
	// Display-bound + Brave/DDG opt-ins remain off by default.
	for _, name := range []string{"web_search", "screenshot", "computer_use"} {
		if hasTool(r, name) {
			t.Errorf("%q should be opt-in, but was registered", name)
		}
	}
	// Plan mode disabled when not provided.
	for _, name := range []string{"enter_plan_mode", "exit_plan_mode"} {
		if hasTool(r, name) {
			t.Errorf("%q should require explicit PlanModeState; got registered without one", name)
		}
	}
}

func TestRegisterClawAll_NoPrivacyByDefault(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawAll(r, ClawDefaults{Workspace: t.TempDir()}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range []string{"privacy_filter", "privacy_unfilter"} {
		if hasTool(r, name) {
			t.Errorf("%q should require explicit Privacy config; got registered without one", name)
		}
	}
}

func TestRegisterClawAll_RegistersPrivacyWhenConfigured(t *testing.T) {
	r := NewRegistry()
	cfg := &privacy.Config{
		StoreDir:     t.TempDir(),
		Detector:     detector.New(),
		RunIDFromCtx: func(ctx context.Context) string { return "test-run" },
	}
	if err := RegisterClawAll(r, ClawDefaults{
		Workspace: t.TempDir(),
		Privacy:   cfg,
	}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range []string{"privacy_filter", "privacy_unfilter"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered when Privacy config is set", name)
		}
	}
}

func TestRegisterClawAll_OptInWebSearchAndComputerUse(t *testing.T) {
	r := NewRegistry()
	active := false
	if err := RegisterClawAll(r, ClawDefaults{
		Workspace:          t.TempDir(),
		IncludeWebSearch:   true,
		IncludeComputerUse: true,
		PlanMode:           &clawtools.PlanModeState{Active: &active, Dir: t.TempDir()},
	}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range []string{"web_search", "read_image", "screenshot", "computer_use", "enter_plan_mode", "exit_plan_mode"} {
		if !hasTool(r, name) {
			t.Errorf("expected opt-in %q registered", name)
		}
	}
}

func TestRegisterAskUser_ProducesErrAskUserOnInvocation(t *testing.T) {
	r := NewRegistry()
	if err := RegisterAskUser(r, nil); err != nil {
		t.Fatalf("RegisterAskUser: %v", err)
	}
	td, err := r.Resolve("ask_user")
	if err != nil {
		t.Fatalf("Resolve(ask_user): %v", err)
	}
	_, execErr := td.Execute(context.Background(), json.RawMessage(`{"question":"Should we deploy?"}`))
	if execErr == nil {
		t.Fatal("expected ask_user to surface ErrAskUser, got nil")
	}
	var ask *delegate.ErrAskUser
	if !errors.As(execErr, &ask) {
		t.Fatalf("expected *delegate.ErrAskUser, got %T: %v", execErr, execErr)
	}
	if ask.Question != "Should we deploy?" {
		t.Errorf("Question = %q, want %q", ask.Question, "Should we deploy?")
	}
}
