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

// TestBuildSpec_PerTicketBotSetsRouteKey confirms a ticket with iss.Bot
// set drives the routing key (spec.Assignee) — the RoutingRunner selects
// the pre-compiled workflow by that key. buildSpec no longer resolves a
// workflow file (the engine runs its pre-compiled IR; the bot FILE is
// resolved + route-checked by the dispatch() guard).
func TestBuildSpec_PerTicketBotSetsRouteKey(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{Workflow: "/tmp/default.bot"}
	iss := tracker.Issue{ID: "i-1", Title: "x", Bot: "feature_dev"}
	spec := d.buildSpec(cfg, iss, "run-1", "/tmp/ws", 0, nil)
	if spec.Assignee != "feature_dev" {
		t.Fatalf("route key (spec.Assignee) = %q, want %q", spec.Assignee, "feature_dev")
	}
}

// TestBuildSpec_BotWinsOverAssignee pins the routing-key precedence:
// Bot wins when both are set; otherwise Assignee is the key. Resolution
// / route-checking of an unknown bot is the dispatch() guard's job, not
// buildSpec's — buildSpec sets the key unconditionally.
func TestBuildSpec_BotWinsOverAssignee(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{Workflow: "/tmp/default.bot"}

	both := tracker.Issue{ID: "i-2", Title: "x", Bot: "feature_dev", Assignee: "alice"}
	if got := d.buildSpec(cfg, both, "run-2a", "/tmp/ws", 0, nil).Assignee; got != "feature_dev" {
		t.Fatalf("bot+assignee: route key = %q, want bot %q", got, "feature_dev")
	}

	assigneeOnly := tracker.Issue{ID: "i-2b", Title: "x", Assignee: "whole_improve_loop"}
	if got := d.buildSpec(cfg, assigneeOnly, "run-2b", "/tmp/ws", 0, nil).Assignee; got != "whole_improve_loop" {
		t.Fatalf("assignee-only: route key = %q, want %q", got, "whole_improve_loop")
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

// TestBuildSpec_EmptyBotEmptyRouteKey — neither Assignee nor Bot set
// yields an empty routing key, so the RoutingRunner falls through to the
// default workflow.
func TestBuildSpec_EmptyBotEmptyRouteKey(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{Workflow: "/tmp/default.bot"}
	iss := tracker.Issue{ID: "i-4", Title: "x"}
	spec := d.buildSpec(cfg, iss, "run-4", "/tmp/ws", 0, nil)
	if spec.Assignee != "" {
		t.Fatalf("route key (spec.Assignee) = %q, want empty", spec.Assignee)
	}
	if len(spec.Vars) != 0 {
		t.Fatalf("Vars should be empty: %v", spec.Vars)
	}
}

// TestBuildSpec_FeatureDevPromptFallback pins the fix for the dispatched
// feature_dev garbage-run bug: a ticket routed to feature_dev (incl. the
// "feature-dev" hyphen alias) without bot_args.feature_prompt gets the
// required var defaulted from the issue's own title+body, so the prompt
// never renders the literal "{{vars.feature_prompt}}". An explicit
// bot_args value still wins, and non-feature_dev bots are untouched.
func TestBuildSpec_FeatureDevPromptFallback(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{Workflow: "/tmp/default.bot"}

	// no bot_args → fallback from title+body (note the hyphen alias)
	iss := tracker.Issue{ID: "i-fp", Title: "Add X", Body: "Do the thing.", Bot: "feature-dev"}
	if got := d.buildSpec(cfg, iss, "r-fp", "/tmp/ws", 0, nil).Vars["feature_prompt"]; got != "Add X\n\nDo the thing." {
		t.Fatalf("feature_prompt fallback = %q, want title+body", got)
	}

	// explicit bot_args.feature_prompt wins (not overwritten)
	iss2 := tracker.Issue{ID: "i-fp2", Title: "T", Body: "B", Bot: "feature_dev",
		BotArgs: map[string]string{"feature_prompt": "EXPLICIT"}}
	if got := d.buildSpec(cfg, iss2, "r-fp2", "/tmp/ws", 0, nil).Vars["feature_prompt"]; got != "EXPLICIT" {
		t.Fatalf("explicit feature_prompt overwritten: got %q", got)
	}

	// non-feature_dev bot → no feature_prompt injected
	iss3 := tracker.Issue{ID: "i-fp3", Title: "T", Body: "B", Bot: "doc-align"}
	if _, ok := d.buildSpec(cfg, iss3, "r-fp3", "/tmp/ws", 0, nil).Vars["feature_prompt"]; ok {
		t.Fatalf("feature_prompt must not be set for non-feature_dev bots")
	}
}
