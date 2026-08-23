package toolcatalog

import "testing"

// TestConstrainsToolsIsClawOnly pins the predicate C135 rests on. Widening it
// to a CLI backend would turn the diagnostic into a false positive on most of
// the bot catalog, whose lists are inert there.
func TestConstrainsToolsIsClawOnly(t *testing.T) {
	if !ConstrainsTools("claw") || !ConstrainsTools("  claw  ") {
		t.Error("claw resolves every declared name against the registry — it constrains")
	}
	for _, backend := range []string{"claude_code", "codex", "pi", "kimi", "grok", "", "auto", "${BACKEND}"} {
		if ConstrainsTools(backend) {
			t.Errorf("backend %q does not consume the lowercase tools: list", backend)
		}
	}
}

func TestIsStaticBuiltinRef(t *testing.T) {
	static := []string{"read_file", "bash", " glob "}
	dynamic := []string{
		"",
		"   ",
		"mcp.github.create_issue",     // discovered when the server connects
		"mcp.github.*",                // wildcard, expanded at run time
		"mcp__iterion_board__create",  // the claude_code FQN alias form
		"${EXTRA_TOOL}",               // resolved from the environment
		"mcp.iterion_board.card_move", // board caps register under MCP
	}
	for _, name := range static {
		if !IsStaticBuiltinRef(name) {
			t.Errorf("%q is a bare built-in reference the catalog decides", name)
		}
	}
	for _, name := range dynamic {
		if IsStaticBuiltinRef(name) {
			t.Errorf("%q is resolved at run time — the catalog must have no opinion", name)
		}
	}
}

func TestIsBuiltinCoversConditionalFamilies(t *testing.T) {
	// Registered unconditionally.
	for _, name := range []string{"read_file", "bash", "glob", "grep", "file_edit", "web_fetch", "skill", "ask_user"} {
		if !IsBuiltin(name) {
			t.Errorf("%q is a core claw built-in", name)
		}
	}
	// Behind a host flag: a compile-time check cannot know whether the
	// operator enabled them, and refusing would block a run that works.
	for _, name := range []string{"web_search", "screenshot", "computer_use", "enter_plan_mode", "exit_plan_mode", "privacy_filter", "privacy_unfilter"} {
		if !IsBuiltin(name) {
			t.Errorf("%q is registered behind a runtime flag and must still be accepted", name)
		}
	}
	for _, name := range []string{"list_files", "git_diff", "search_codebase", "", "memory_read"} {
		if IsBuiltin(name) {
			t.Errorf("%q is not registered — accepting it defeats the check", name)
		}
	}
}

func TestSuggest(t *testing.T) {
	cases := map[string]string{
		"list_files":      "glob",      // curated: nowhere near by edit distance
		"search_codebase": "grep",      //
		"run_command":     "bash",      //
		"read_fil":        "read_file", // ordinary typo
		"write_files":     "write_file",
		"zzzzzzzzzzzz":    "", // nothing close: say nothing rather than invent
		"x":               "", // too short to search
	}
	for in, want := range cases {
		if got := Suggest(in); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuiltinsIsSortedAndCopied(t *testing.T) {
	got := Builtins()
	if len(got) == 0 {
		t.Fatal("the catalog is empty")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Builtins() is not sorted: %q before %q", got[i-1], got[i])
		}
	}
	got[0] = "mutated"
	if Builtins()[0] == "mutated" {
		t.Error("Builtins() must hand back a copy")
	}
}
