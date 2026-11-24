package model

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The prompt-invoked surface of #1717: a node whose prompt OPENS with a
// workspace command declaring `allowed-tools:` runs with its ToolDefs
// INTERSECTED with that list (folded, plus the always-allowed trio the node
// carries). Never widened: a tool the node did not declare stays out,
// whatever the file claims. The advertised set is what the sandbox crossing
// enforces, so the intersection holds in-container without a wire field.
//
// Mutant: drop the intersection from buildTask and the node runs with its
// full set — exactly the silent divergence the ticket measures.
func TestBuildTask_PromptCommandAllowedToolsNarrowsTheToolDefs(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	cmdDir := filepath.Join(ws, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(cmdDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("narrow.md", "---\ndescription: n\nallowed-tools: Read\n---\nAudit the diff.\n")
	write("sealed.md", "---\ndescription: s\nallowed-tools: []\n---\nAnswer from what you have.\n")
	write("plain.md", "---\ndescription: p\n---\nDo the thing.\n")

	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{
		"usr":        {Body: "/narrow"},
		"usr-sealed": {Body: "/sealed"},
		"usr-plain":  {Body: "/plain"},
	}}
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "worker"},
		LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, UserPrompt: "usr", Model: "anthropic/claude-sonnet-4-6"},
	}
	f, err := extractBackendFields(node)
	if err != nil {
		t.Fatalf("extractBackendFields: %v", err)
	}
	f.tools = []string{"read_file", "bash", "command"}

	build := func() delegate.Task {
		t.Helper()
		reg := tool.NewRegistry()
		if err := tool.RegisterClawBuiltins(reg, ws); err != nil {
			t.Fatalf("RegisterClawBuiltins: %v", err)
		}
		// buildTask injects todo_write into every tool-bearing node's set;
		// the registry must hold it for the resolution to succeed.
		if err := tool.RegisterClawTodo(reg); err != nil {
			t.Fatalf("RegisterClawTodo: %v", err)
		}
		if err := tool.RegisterClawCommand(reg, ws, 0); err != nil {
			t.Fatalf("RegisterClawCommand: %v", err)
		}
		e := NewClawExecutor(NewRegistry(), wf,
			WithLogger(iterlog.Nop()), WithWorkDir(ws), WithToolRegistry(reg))
		task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, &nodeBuildSession{})
		if err != nil {
			t.Fatalf("buildTask: %v", err)
		}
		return task
	}

	names := func(task delegate.Task) []string {
		out := make([]string, 0, len(task.ToolDefs))
		for _, td := range task.ToolDefs {
			out = append(out, td.Name)
		}
		slices.Sort(out)
		return out
	}

	// The narrowing holds: read_file survives, the always-allowed pair the
	// node carries (command, todo_write — buildTask injects it) survives,
	// bash is gone — and the body was substituted, so this is not a vacuous
	// pass.
	task := build()
	if got := names(task); !slices.Equal(got, []string{"command", "read_file", "todo_write"}) {
		t.Errorf("narrowed ToolDefs = %v, want [command read_file todo_write]", got)
	}
	if task.UserPrompt != "Audit the diff." {
		t.Errorf("UserPrompt = %q, want the expanded body", task.UserPrompt)
	}

	// A DECLARED empty list narrows to the trio — of which the node carries
	// command and todo_write.
	f.userPrompt = "usr-sealed"
	if got := names(build()); !slices.Equal(got, []string{"command", "todo_write"}) {
		t.Errorf("sealed ToolDefs = %v, want [command todo_write]", got)
	}

	// No allowed-tools: the full declared set, unchanged.
	f.userPrompt = "usr-plain"
	if got := names(build()); !slices.Equal(got, []string{"bash", "command", "read_file", "todo_write"}) {
		t.Errorf("plain ToolDefs = %v, want the declared set untouched", got)
	}
}
