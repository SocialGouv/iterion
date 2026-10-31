package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The workspace slash-command capability has two surfaces: the prompt path
// (model.expandWorkspaceSlashCommand), which reads the executor's workDir
// LIVE, and the model-invoked `command`/`skill` tools, registered against a
// workspace while the executor is built. The engine replaces that directory
// at run start when `worktree: auto` produces a per-run worktree
// (ClawExecutor.SetWorkDir). Both surfaces must resolve
// `.claude/commands/` and `.claude/skills/` in the SAME checkout: the run's
// — a command file planted only in the worktree, expanded by the prompt
// path, is one the tool must find too (#2870).
//
// Mutant: the registration capturing the construction-time workspace string
// again → both halves redden on "Unknown command" / "not found".
func TestBuildExecutor_WorkspaceLookupToolsFollowSetWorkDir(t *testing.T) {
	// The checkout the executor is built against: carries neither the
	// command nor the skill.
	launch := t.TempDir()
	// The run's worktree: the only place they exist.
	worktree := t.TempDir()
	commandBody := "Review with the worktree checklist KUMQUAT-2870.\n"
	if err := os.MkdirAll(filepath.Join(worktree, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".claude", "commands", "wt-only.md"), []byte(commandBody), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(worktree, ".claude", "skills", "wt-only-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: wt-only-skill\ndescription: worktree-only skill\n---\nFollow the KUMQUAT-2870 skill body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		Name: "workspace-follow",
		Nodes: map[string]ir.Node{
			"probe": &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "probe"},
				Tools:    []string{"command", "skill"},
			},
		},
	}
	exec, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Store:    st,
		RunID:    "workspace-follow",
		StoreDir: t.TempDir(),
		WorkDir:  launch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	// What the engine does at run start once `worktree: auto` has produced
	// the run's worktree (runtime.runPersistWorkspace).
	exec.SetWorkDir(worktree)

	ctx := context.Background()
	command, err := exec.Execute(ctx, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "invoke-command"},
		Command:  "command",
	}, map[string]any{"name": "wt-only"})
	if err != nil {
		t.Fatalf("command tool node: %v", err)
	}
	if result, _ := command["result"].(string); !strings.Contains(result, "KUMQUAT-2870") {
		t.Fatalf("command tool resolved outside the run worktree: %q", result)
	}

	skill, err := exec.Execute(ctx, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "invoke-skill"},
		Command:  "skill",
	}, map[string]any{"skill": "wt-only-skill"})
	if err != nil {
		t.Fatalf("skill tool node: %v", err)
	}
	// The skill tool answers a JSON object ({"prompt": <body>, ...}), which
	// the tool node parses into keys rather than wrapping under `result`.
	if prompt, _ := skill["prompt"].(string); !strings.Contains(prompt, "KUMQUAT-2870") {
		t.Fatalf("skill tool resolved outside the run worktree: %v", skill)
	}
}
