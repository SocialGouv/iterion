package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scope's semantics are INTERSECT-ONLY (#1717, review HIGH): on the
// `command` surface every invocation is model-side, so a narrowing the
// model could lift by invoking a plain command would be none at all. A
// declared allowed-tools narrows, a second one can only shrink further,
// and a declared EMPTY list narrows to the always-allowed trio.
// Mutant: restore the vendor's replace/clear and the last assertions
// redden.
func TestCommandScope_IntersectOnlyNarrowing(t *testing.T) {
	s := &CommandScope{}

	// No narrowing: everything passes.
	if err := s.CheckAllowed("bash"); err != nil {
		t.Fatalf("an untouched scope refused bash: %v", err)
	}

	// Pose: declared list narrows, folded from the Claude spelling.
	s.Apply("audit", []string{"Read", "Grep"})
	if err := s.CheckAllowed("read_file"); err != nil {
		t.Errorf("Read folds to read_file, which must pass: %v", err)
	}
	if err := s.CheckAllowed("grep"); err != nil {
		t.Errorf("grep must pass: %v", err)
	}
	if err := s.CheckAllowed("bash"); err == nil ||
		!strings.Contains(err.Error(), `tool "bash" is not in allowed-tools for command "audit"`) {
		t.Errorf("bash must be refused, naming the command — got %v", err)
	}
	// The always-allowed trio survives every narrowing.
	for _, name := range []string{"command", "tool_search", "todo_write"} {
		if err := s.CheckAllowed(name); err != nil {
			t.Errorf("%s is always allowed: %v", name, err)
		}
	}

	// A second command INTERSECTS: what it lists and the first omitted
	// drops out; what it lists beyond the first never comes back.
	s.Apply("grepless", []string{"Read", "Write"})
	if err := s.CheckAllowed("read_file"); err != nil {
		t.Errorf("read_file is in both lists: %v", err)
	}
	if err := s.CheckAllowed("grep"); err == nil {
		t.Error("grep was only in the first list — the intersection drops it")
	}
	if err := s.CheckAllowed("write_file"); err == nil {
		t.Error("write_file was not in the first list — intersect-only must not widen")
	}
}

// A DECLARED empty list is not an absent key: it narrows to the trio alone.
// The vendor's WorkspaceCommand.AllowedTools keeps the two apart precisely
// so this distinction can be honoured.
func TestCommandScope_DeclaredEmptyListNarrowsToTheTrio(t *testing.T) {
	s := &CommandScope{}
	s.Apply("sealed", []string{})
	for _, name := range []string{"command", "tool_search", "todo_write"} {
		if err := s.CheckAllowed(name); err != nil {
			t.Errorf("%s is always allowed: %v", name, err)
		}
	}
	if err := s.CheckAllowed("read_file"); err == nil {
		t.Error("an empty allowed-tools must refuse everything else")
	}
}

// The gate never invents a tool: a spelling that folds to nothing the node
// holds still narrows — narrowing can only ever remove.
func TestCommandScope_UnknownSpellingNarrowsToNothing(t *testing.T) {
	s := &CommandScope{}
	s.Apply("probe", []string{"NoSuchTool"})
	if err := s.CheckAllowed("nosuchtool"); err != nil {
		t.Errorf("the folded spelling is in the list: %v", err)
	}
	if err := s.CheckAllowed("read_file"); err == nil {
		t.Error("an unknown spelling must not smuggle read_file in")
	}
}

// The published shapes that are not bare names (review LOW-5): the prefixed
// form grants the whole tool (iterion has no per-prefix granularity), the
// MCP FQN folds to the sanitized spelling and to the server family, and the
// bare `*` is an explicit no-restriction.
func TestCommandScope_PrefixedMCPAndWildcardShapes(t *testing.T) {
	s := &CommandScope{}
	s.Apply("c", []string{"Bash(git:*)", "mcp__github__create_issue"})
	if err := s.CheckAllowed("bash"); err != nil {
		t.Errorf("Bash(git:*) grants the whole bash tool: %v", err)
	}
	if err := s.CheckAllowed("mcp_github_create_issue"); err != nil {
		t.Errorf("the FQN folds to the sanitized spelling: %v", err)
	}
	if err := s.CheckAllowed("mcp_github_list_issues"); err == nil {
		t.Error("one FQN grants one tool, not the server")
	}

	fam := &CommandScope{}
	fam.Apply("c", []string{"mcp__github__*"})
	if err := fam.CheckAllowed("mcp_github_list_issues"); err != nil {
		t.Errorf("mcp__github__* grants the family: %v", err)
	}
	if err := fam.CheckAllowed("mcp_gitlab_list_issues"); err == nil {
		t.Error("the family does not cross servers")
	}

	all := &CommandScope{}
	all.Apply("c", []string{"*"})
	if err := all.CheckAllowed("bash"); err != nil {
		t.Errorf("a bare * is an explicit no-restriction: %v", err)
	}
}

// The pause checkpoint carries the narrowing (review MED-1): Snapshot and
// Restore round-trip it, and a restored scope binds the fresh generation.
func TestCommandScope_SnapshotRestoreRoundTrip(t *testing.T) {
	s := &CommandScope{}
	if s.Snapshot() != nil {
		t.Fatal("an inactive scope snapshots to nil")
	}
	s.Apply("audit", []string{"Read", "mcp__github__*"})
	st := s.Snapshot()
	if st == nil || st.Command != "audit" {
		t.Fatalf("snapshot = %+v", st)
	}

	fresh := &CommandScope{}
	fresh.Restore(st)
	if err := fresh.CheckAllowed("read_file"); err != nil {
		t.Errorf("restored scope must allow read_file: %v", err)
	}
	if err := fresh.CheckAllowed("mcp_github_list_issues"); err != nil {
		t.Errorf("restored scope must keep the family: %v", err)
	}
	if err := fresh.CheckAllowed("bash"); err == nil ||
		!strings.Contains(err.Error(), `"audit"`) {
		t.Errorf("restored scope must refuse bash, naming the command: %v", err)
	}
	fresh.Restore(nil) // a nil state restores nothing
	if err := fresh.CheckAllowed("bash"); err == nil {
		t.Error("restoring nil must not clear the narrowing")
	}
}

// The scope rides the context the tool loop hands every Execute: pinned by
// the backend, read by the gate and by the `command` tool. A context
// without one reads nil, and nil narrows nothing.
func TestCommandScope_ContextRoundTrip(t *testing.T) {
	if CommandScopeFrom(context.Background()) != nil {
		t.Fatal("a bare context must carry no scope")
	}
	s := &CommandScope{}
	ctx := WithCommandScope(context.Background(), s)
	if CommandScopeFrom(ctx) != s {
		t.Fatal("the pinned scope did not come back")
	}
}

// The folding table is shared with the workspace-agent resolver
// (pkg/backend/model): a command and an agent file must never fold the same
// spelling two ways.
func TestFoldCLISpelling(t *testing.T) {
	for spelling, want := range map[string]string{
		"Read":      "read_file",
		"read":      "read_file",
		" Write ":   "write_file",
		"Edit":      "file_edit",
		"MultiEdit": "file_edit",
		"WebFetch":  "web_fetch",
		"WebSearch": "web_search",
		"TodoWrite": "todo_write",
		"Task":      "agent",
		"Grep":      "grep", // no row needed: case is the only difference
		"bash":      "bash",
		"Whatever":  "whatever",
		// The prefixed form grants the whole tool (review LOW-5).
		"Bash(git:*)": "bash",
		// The claude_code MCP FQN folds to the sanitized spelling.
		"mcp__github__create_issue": "mcp_github_create_issue",
	} {
		if got := FoldCLISpelling(spelling); got != want {
			t.Errorf("FoldCLISpelling(%q) = %q, want %q", spelling, got, want)
		}
	}
}

// The `command` tool applies the narrowing itself: invoking a command that
// declares allowed-tools narrows the scope the execution pinned on the
// context, and its answer says so; invoking one WITHOUT the key changes
// nothing — intersect-only, because on this surface every invocation is
// model-side and a self-liftable restriction is none (review HIGH).
// Mutant: drop the Apply call from executeWorkspaceCommand and the refusal
// at the end never happens; reinstate a Clear and /plain lifts the bound.
func TestClawCommand_AllowedToolsNarrowsTheGenerationScope(t *testing.T) {
	ws := writeCommandFile(t, "narrow",
		"---\ndescription: n\nallowed-tools: [Read, Grep]\n---\nAudit.\n")
	writeCommandFileAt(t, ws, "plain", "---\ndescription: p\n---\nPlain.\n")
	// The YAML block-list shape (review LOW-4) parses to the same list.
	writeCommandFileAt(t, ws, "blocked", "---\ndescription: b\nallowed-tools:\n  - Write\n---\nWrite.\n")
	reg := NewRegistry()
	if err := RegisterClawCommand(reg, ws, 0); err != nil {
		t.Fatalf("RegisterClawCommand: %v", err)
	}
	td := mustCommandTool(t, reg)

	scope := &CommandScope{}
	ctx := WithCommandScope(context.Background(), scope)
	exec := func(input map[string]any) string {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out, err := td.Execute(ctx, raw)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return out
	}

	out := exec(map[string]any{"name": "narrow"})
	if err := scope.CheckAllowed("read_file"); err != nil {
		t.Errorf("read_file must be allowed after /narrow: %v", err)
	}
	if err := scope.CheckAllowed("bash"); err == nil {
		t.Error("bash must be narrowed away after /narrow")
	}
	if !strings.Contains(out, "narrowed your tool surface") {
		t.Errorf("the answer must tell the model its surface changed, got %q", out)
	}
	if strings.Contains(out, "lifts it") {
		t.Errorf("the notice must not advertise a self-lift — intersect-only: %q", out)
	}

	// A command without allowed-tools does NOT lift the narrowing.
	out = exec(map[string]any{"name": "plain"})
	if err := scope.CheckAllowed("bash"); err == nil {
		t.Error("/plain declares no allowed-tools — intersect-only keeps the bound")
	}
	if err := scope.CheckAllowed("read_file"); err != nil {
		t.Errorf("the bound stands, unchanged: %v", err)
	}
	if strings.Contains(out, "narrowed your tool surface") {
		t.Errorf("a non-narrowing invocation owes no narrowing notice, got %q", out)
	}

	// The block-list shape narrows further — and only further: [Write]
	// intersected with [read_file, grep] leaves nothing but the trio.
	exec(map[string]any{"name": "blocked"})
	if err := scope.CheckAllowed("read_file"); err == nil {
		t.Error("the intersection must drop what the block list omits")
	}
	if err := scope.CheckAllowed("write_file"); err == nil {
		t.Error("the intersection must not add what the first bound omitted")
	}
}

// writeCommandFileAt plants a second command in an existing workspace (the
// helper writeCommandFile builds a fresh one).
func writeCommandFileAt(t *testing.T, ws, name, body string) {
	t.Helper()
	rel := filepath.Join(strings.Split(name, ":")...) + ".md"
	path := filepath.Join(ws, ".claude", "commands", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write command: %v", err)
	}
}
