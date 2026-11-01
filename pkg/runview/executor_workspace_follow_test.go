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

// The file and shell builtins are the other half of the same capture
// (#2889, the #2870 class): bash, write_file and read_file resolved
// relative paths and the shell's cwd against the workspace captured when
// the registry was built — the launch checkout — while the engine replaces
// the run's directory at run start (`worktree: auto` → SetWorkDir). They
// must resolve `/x` in the RUN's worktree, through the same live reading
// the `command`/`skill` lookup tools take.
//
// Mutant: RegisterClawBuiltinsWithScope capturing the scope's workspace
// string again, or the scope built from a string instead of the executor's
// workDir provider → every half below reddens (pwd prints the launch dir,
// the write lands there, the worktree-only file reads as missing).
func TestBuildExecutor_WorkspaceFileToolsFollowSetWorkDir(t *testing.T) {
	// The checkout the executor is built against: the write must NOT land
	// here, and the marker file does not exist here.
	launch := t.TempDir()
	// The run's worktree: where the shell starts, where relative paths go.
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "wt-only.txt"), []byte("KUMQUAT-2889\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{
		Name: "workspace-follow-files",
		Nodes: map[string]ir.Node{
			"probe": &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "probe"},
				Tools:    []string{"bash", "write_file", "read_file"},
			},
		},
	}
	exec, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Store:    st,
		RunID:    "workspace-follow-files",
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
	bash, err := exec.Execute(ctx, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "run-bash"},
		Command:  "bash",
	}, map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("bash tool node: %v", err)
	}
	if out, _ := bash["result"].(string); strings.TrimSpace(out) != worktree {
		t.Fatalf("bash ran in the registration workspace, not the run worktree: %q", out)
	}

	if _, err := exec.Execute(ctx, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "run-write"},
		Command:  "write_file",
	}, map[string]any{"path": "written.txt", "content": "from the run\n"}); err != nil {
		t.Fatalf("write_file tool node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "written.txt")); err != nil {
		t.Fatalf("write_file did not land in the run worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(launch, "written.txt")); !os.IsNotExist(err) {
		t.Fatalf("write_file leaked into the launch checkout")
	}

	read, err := exec.Execute(ctx, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "run-read"},
		Command:  "read_file",
	}, map[string]any{"path": "wt-only.txt"})
	if err != nil {
		t.Fatalf("read_file tool node: %v", err)
	}
	if out, _ := read["result"].(string); !strings.Contains(out, "KUMQUAT-2889") {
		t.Fatalf("read_file resolved outside the run worktree: %q", out)
	}
}
