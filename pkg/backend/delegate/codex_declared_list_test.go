package delegate

import "testing"

// A codex node's `tools:` list is neither dropped nor an allowlist: it picks a
// sandbox mode, and that mode is the whole bound, since codex runs its
// built-in shell without routing tool_use through any SDK callback. So the
// read-only set is CODEX's own, and a reader that assumed the wider read-only
// vocabulary of a declaration (`git_diff`, `tree`, `web_fetch` …) would read a
// workspace-write node as a reader.
//
// The modes are the measured facts, not a second derivation: each row asserts
// the mode codexSandboxForTask gives the task AND that the exported predicate
// says the same thing, so a name moving between the classifier's cases reddens
// here rather than silently widening a boundary class in pkg/runtime.
func TestCodexDeclaredListIsReadOnlyAnswersFromTheNamesCodexReads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tools    []string
		declared bool
		wantMode string
	}{
		{"the names codex reads as reads", []string{"read_file", "glob", "grep"}, true, "read-only"},
		// The rest of codex's read-only vocabulary, name by name. A reader in
		// pkg/runtime bounds a node on this predicate over the list the route
		// RECEIVES, which carries the runtime's own appends: `todo_write`
		// (canonical `todowrite`) is appended for the CLAW route alone, so a
		// codex node with a `fallbacks:` route onto claw is asked about a name
		// its codex route never gets. It stays eligible for a shared worktree
		// because this row holds.
		{"ls is a name codex reads", []string{"ls"}, true, "read-only"},
		{"websearch is a name codex reads", []string{"websearch"}, true, "read-only"},
		{"toolsearch is a name codex reads", []string{"toolsearch"}, true, "read-only"},
		{"the claw-only plumbing append codex must read as a read", []string{"todo_write"}, true, "read-only"},
		{"skill is a name codex reads", []string{"skill"}, true, "read-only"},
		{"a list declared empty names nothing that writes", []string{}, true, "read-only"},
		{"no `tools:` list at all is the full native toolset", nil, false, "workspace-write"},
		{"git_diff is not a name codex reads", []string{"git_diff"}, true, "workspace-write"},
		{"git_status is not a name codex reads", []string{"git_status"}, true, "workspace-write"},
		{"list_files is not a name codex reads", []string{"list_files"}, true, "workspace-write"},
		{"search_codebase is not a name codex reads", []string{"search_codebase"}, true, "workspace-write"},
		{"tree is not a name codex reads", []string{"tree"}, true, "workspace-write"},
		{"web_fetch is not a name codex reads", []string{"web_fetch"}, true, "workspace-write"},
		{"one unread name carries the whole list", []string{"read_file", "git_diff"}, true, "workspace-write"},
		// The runtime's own `interaction:` append lands on a non-empty list
		// (model.buildTask), and `ask_user` buys workspace-write — which is
		// why a pre-run reader of the DECLARATION is not a reading of what the
		// node will hold.
		{"the interaction append widens the mode", []string{"read_file", "ask_user"}, true, "workspace-write"},
		{"a declared shell", []string{"bash"}, true, "workspace-write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ToolsDeclared: tc.declared, AllowedTools: tc.tools}
			if got := codexSandboxForTask(task); got != tc.wantMode {
				t.Errorf("codexSandboxForTask(%v, declared=%v) = %q, want %q", tc.tools, tc.declared, got, tc.wantMode)
			}
			want := tc.wantMode == codexSandboxReadOnly
			if got := CodexDeclaredListIsReadOnly(tc.tools, tc.declared); got != want {
				t.Errorf("CodexDeclaredListIsReadOnly(%v, %v) = %v, want %v — a reader of this predicate bounds a node on it", tc.tools, tc.declared, got, want)
			}
		})
	}
	// The author's own locks are answered before a declared list is
	// classified, so the predicate must not be read as "this node cannot
	// write": it answers for the LIST alone.
	if got := codexSandboxForTask(Task{Readonly: true, ToolsDeclared: true, AllowedTools: []string{"git_diff"}}); got != "read-only" {
		t.Errorf("readonly: is the author's lock and wins over the names: got %q", got)
	}
	if CodexDeclaredListIsReadOnly([]string{"git_diff"}, true) {
		t.Error("the predicate reads the list, never `readonly:`: a caller that must honour the flag reads it itself")
	}
}
