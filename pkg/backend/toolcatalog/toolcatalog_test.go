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
		"mcp.iterion_board.card_move", // board caps register under MCP
	}
	// An env/template reference is decidable — and unresolvable: `tools:` is
	// the one field iterion never expands. The dotted form must not slip
	// through the MCP-shape check.
	for _, name := range []string{"${EXTRA_TOOL}", "{{vars.extra}}"} {
		if !IsStaticBuiltinRef(name) || !IsUnexpandedRef(name) {
			t.Errorf("%q is an unexpanded reference the catalog decides", name)
		}
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

func TestResolvesViaShorthand(t *testing.T) {
	// Registered by the runtime for every run under mcp.iterion_board.* /
	// mcp.iterion_watch.*, and reachable by their bare name through
	// Registry.Resolve's shorthand path.
	for _, name := range []string{"create_issue", "list_issues", "set_bot", "subscribe", "unsubscribe"} {
		if !ResolvesViaShorthand(name) {
			t.Errorf("%q resolves by its bare name today — rejecting it would block a workflow that runs", name)
		}
	}
	for _, name := range []string{"read_file", "list_files", "browser_click", ""} {
		if ResolvesViaShorthand(name) {
			t.Errorf("%q is not one of iterion's internal MCP tools", name)
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
