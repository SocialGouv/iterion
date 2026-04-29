package tool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
)

func hasTool(r *Registry, name string) bool {
	_, err := r.Resolve(name)
	return err == nil
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
	for _, name := range []string{"read_image", "screenshot"} {
		if hasTool(r, name) {
			t.Errorf("expected %q NOT registered by default; vision tools are opt-in", name)
		}
	}
}

func TestRegisterClawComputerUse_RegistersBoth(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	for _, name := range []string{"read_image", "screenshot"} {
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
	tool, _ := r.Resolve("read_image")

	// Missing both path and url → underlying tool errors. The
	// adapter must surface that, not swallow it.
	input, _ := json.Marshal(map[string]any{})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Fatal("expected error when neither path nor url given")
	}
}

func TestRegisterClawComputerUse_ScreenshotReturnsAPIError(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	tool, _ := r.Resolve("screenshot")

	input, _ := json.Marshal(map[string]any{})
	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected stub error from screenshot")
	}
	var apiErr *clawapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.APIError to propagate through adapter, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 501 {
		t.Errorf("expected status 501, got %d", apiErr.StatusCode)
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
	if err := RegisterClawComputerUse(r); err != nil {
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
	if err := RegisterClawSubagents(r); err != nil {
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
	if err := RegisterClawMCPResources(r, clawmcp.NewRegistry(), clawmcp.NewAuthState()); err != nil {
		t.Fatalf("RegisterClawMCPResources: %v", err)
	}
	for _, name := range []string{"list_mcp_resources", "read_mcp_resource", "mcp_auth"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawMCPResources_NilAuthFallsBack(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawMCPResources(r, clawmcp.NewRegistry(), nil); err != nil {
		t.Errorf("expected nil auth to fallback to fresh AuthState; got %v", err)
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
		"todo_write", "agent", "skill",
		"send_user_message", "remote_trigger", "sleep", "notebook_edit", "repl", "structured_output",
		"task_create", "worker_create", "team_create", "cron_create",
		"list_mcp_resources", "lsp", "tool_search",
	}
	for _, name := range expected {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered by RegisterClawAll", name)
		}
	}
	// Opt-in flags off by default.
	for _, name := range []string{"web_search", "read_image", "screenshot"} {
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
	for _, name := range []string{"web_search", "read_image", "screenshot", "enter_plan_mode", "exit_plan_mode"} {
		if !hasTool(r, name) {
			t.Errorf("expected opt-in %q registered", name)
		}
	}
}
