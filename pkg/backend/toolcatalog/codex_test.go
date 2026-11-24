package toolcatalog

import "testing"

// The table answers for the AUTHOR's declaration: an undeclared list is no
// bound, a declared-empty one names nothing that writes, codex's own read-only
// names stay read-only under every spelling, and any other name — the ones
// other backends read as reads included — buys workspace-write.
func TestCodexDeclaredListIsReadOnlyIsTheAuthorsDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tools    []string
		declared bool
		want     bool
	}{
		{"undeclared", nil, false, false},
		{"declared empty", []string{}, true, true},
		{"codex reads, by alias", []string{"read_file", "Read", "grep", "glob", "ls", "list_dir", "toolsearch", "todo_write", "skill"}, true, true},
		{"web search, every spelling", []string{"web_search", "websearch", "web-search", "WebSearch"}, true, true},
		{"a writer", []string{"read_file", "bash"}, true, false},
		{"a read elsewhere is a write here", []string{"git_diff", "tree", "web_fetch", "list_files"}, true, false},
		{"an unknown name", []string{"my_custom_tool"}, true, false},
	} {
		if got := CodexDeclaredListIsReadOnly(tc.tools, tc.declared); got != tc.want {
			t.Errorf("%s: CodexDeclaredListIsReadOnly(%v, %v) = %v, want %v", tc.name, tc.tools, tc.declared, got, tc.want)
		}
	}
}
