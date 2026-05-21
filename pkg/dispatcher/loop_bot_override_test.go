package dispatcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func writeBotFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const botSrcWithVars = `## ---
## name: feature_dev
## ---

workflow w:
  vars:
    workspace_dir: string = ""
    loop_cap: int = 5
  agent a:
    model: "test"
  a -> done

agent a:
  model: "test"
`

// TestBuildSpec_PerTicketBotResolvesWorkflowPath confirms a ticket
// with iss.Bot set picks up the resolved bot's main file instead of
// cfg.Workflow.
func TestBuildSpec_PerTicketBotResolvesWorkflowPath(t *testing.T) {
	botregistry.ClearSchemaCache()
	d := newMinimalDispatcher(t)
	dir := t.TempDir()
	botPath := filepath.Join(dir, "feature_dev.bot")
	writeBotFile(t, botPath, botSrcWithVars)

	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Bots:     botregistry.Config{Paths: []string{dir}},
	}
	iss := tracker.Issue{ID: "i-1", Title: "x", Bot: "feature_dev"}
	spec := d.buildSpec(cfg, iss, "run-1", "/tmp/ws", 0, nil)
	if spec.WorkflowPath != botPath {
		t.Fatalf("WorkflowPath = %q, want %q", spec.WorkflowPath, botPath)
	}
}

// TestBuildSpec_UnknownBotFallsBackToConfigWorkflow — a stale name on
// a ticket should not silently halt dispatch; we fall back to the
// config workflow and log a warning.
func TestBuildSpec_UnknownBotFallsBackToConfigWorkflow(t *testing.T) {
	botregistry.ClearSchemaCache()
	d := newMinimalDispatcher(t)
	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Bots:     botregistry.Config{Paths: []string{t.TempDir()}},
	}
	iss := tracker.Issue{ID: "i-2", Title: "x", Bot: "ghost"}
	spec := d.buildSpec(cfg, iss, "run-2", "/tmp/ws", 0, nil)
	if spec.WorkflowPath != "/tmp/default.bot" {
		t.Fatalf("expected fallback to default, got %q", spec.WorkflowPath)
	}
}

// TestBuildSpec_BotArgsMergeOverConfigVars verifies the merge order:
// rendered config vars first, then per-ticket BotArgs win on shared
// keys; orphan keys flow through.
func TestBuildSpec_BotArgsMergeOverConfigVars(t *testing.T) {
	botregistry.ClearSchemaCache()
	d := newMinimalDispatcher(t)
	dir := t.TempDir()
	writeBotFile(t, filepath.Join(dir, "feature_dev.bot"), botSrcWithVars)

	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Bots:     botregistry.Config{Paths: []string{dir}},
		Dispatch: DispatchConfig{Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"loop_cap":      "10",
		}},
	}
	iss := tracker.Issue{
		ID:    "i-3",
		Title: "x",
		Bot:   "feature_dev",
		BotArgs: map[string]string{
			"loop_cap":     "3",        // override config template
			"extra_orphan": "passthru", // not in config vars
		},
	}
	spec := d.buildSpec(cfg, iss, "run-3", "/tmp/ws/i-3", 0, nil)
	if spec.Vars["workspace_dir"] != "/tmp/ws/i-3" {
		t.Errorf("workspace_dir = %v (config template should still apply)", spec.Vars["workspace_dir"])
	}
	if spec.Vars["loop_cap"] != "3" {
		t.Errorf("loop_cap = %v (BotArgs should win)", spec.Vars["loop_cap"])
	}
	if spec.Vars["extra_orphan"] != "passthru" {
		t.Errorf("extra_orphan dropped: %v", spec.Vars)
	}
}

// TestBuildSpec_EmptyBotKeepsConfigWorkflow — the legacy code path
// where neither Assignee nor Bot points anywhere still produces the
// default cfg.Workflow.
func TestBuildSpec_EmptyBotKeepsConfigWorkflow(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{Workflow: "/tmp/default.bot"}
	iss := tracker.Issue{ID: "i-4", Title: "x"}
	spec := d.buildSpec(cfg, iss, "run-4", "/tmp/ws", 0, nil)
	if spec.WorkflowPath != "/tmp/default.bot" {
		t.Fatalf("WorkflowPath = %q", spec.WorkflowPath)
	}
	if len(spec.Vars) != 0 {
		t.Fatalf("Vars should be empty: %v", spec.Vars)
	}
}
