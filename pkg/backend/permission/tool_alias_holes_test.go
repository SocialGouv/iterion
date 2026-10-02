package permission

import "testing"

// A rule and a call that name the SAME tool must reach the same verdict
// whichever spelling each uses. Measured against the production Policy
// (mode ask, one deny rule), so a row dropped from the shared table reddens
// here rather than in a run.
//
// The four rows at the top were holes: `deny: ["tool_search"]` did not match
// a `ToolSearch` call and `deny: ["run_command"]` did not match `Bash`, while
// `run_command` WAS accepted as a Bash grant by a node's `tools:` — the two
// fields disagreed about one word (#1579).
func TestARuleMatchesItsToolWhicheverSpellingEachSideUses(t *testing.T) {
	pairs := [][2]string{
		// the measured holes
		{"tool_search", "ToolSearch"},
		{"run_command", "Bash"},
		{"file_write", "Write"},
		{"apply_patch", "Edit"},
		// the rows that already agreed — a fixture with only the broken rows
		// would go green under a table that mapped everything onto one key
		{"read_file", "Read"},
		{"web_fetch", "WebFetch"},
		{"notebook_edit", "NotebookEdit"},
		{"todo_write", "TodoWrite"},
		{"shell", "Bash"},
		{"find", "Glob"},
		{"task", "Task"},
		{"skill", "Skill"},
		{"task_output", "TaskOutput"},
		{"task_stop", "TaskStop"},
	}
	for _, p := range pairs {
		for _, d := range [][2]string{{p[0], p[1]}, {p[1], p[0]}} {
			pol, err := NewPolicy(ModeAsk, nil, nil, []string{d[0]})
			if err != nil {
				t.Fatalf("deny %q: %v", d[0], err)
			}
			if got, _ := pol.Evaluate(d[1], map[string]any{}); got != Deny {
				t.Errorf("deny %q vs call %q = %v, want deny (canonical %q vs %q)",
					d[0], d[1], got, canonicalToolName(d[0]), canonicalToolName(d[1]))
			}
		}
	}
}

// Two tools that merely resemble each other are not one. `bots/copilot`
// allows `workspace_grep` while denying `Grep`, and asks for
// `diagnostic_shell` while the Bash bridge is bounded separately: a table
// that collapsed either pair would deny a shipped bot its own allowed tool.
func TestResemblingToolsAreNotCollapsedOntoOneKey(t *testing.T) {
	for _, p := range [][2]string{
		{"Grep", "workspace_grep"},
		{"Bash", "diagnostic_shell"},
		{"Read", "Glob"},
	} {
		pol, err := NewPolicy(ModeAsk, nil, nil, []string{p[0]})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := pol.Evaluate(p[1], map[string]any{}); got == Deny {
			t.Errorf("deny %q blocked the distinct tool %q", p[0], p[1])
		}
	}
}
