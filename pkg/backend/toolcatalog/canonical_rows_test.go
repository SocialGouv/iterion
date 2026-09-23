package toolcatalog

import "testing"

// Every row of the shared table, pinned.
//
// A row asserts that two spellings ARE the same tool, and it is read by a
// security gate where `allow:` WIDENS — so the one row that must never exist
// (`git_diff` → `bash`, granting the whole shell to an author who wrote a
// diff) is indistinguishable, mechanically, from the rows that must. The only
// guard that separates them is a human argument, so a new row has to be typed
// here before it can grant anything, and a deleted one reddens instead of
// silently re-opening a gate hole.
//
// Measured on three mutations that every other test in the repo passed:
// adding `git_diff → bash` (allow:[git_diff] then permits `rm -rf /`),
// deleting `write_file → write`, and deleting `web_search → websearch`.
func TestEveryRowOfTheSharedTableIsPinned(t *testing.T) {
	want := map[string]string{
		// shell — `run_command` is accepted as a Bash grant by the
		// claude_code projection; `run_terminal_command` is Grok's spelling.
		"bash": "bash", "shell": "bash", "sh": "bash",
		"run_terminal_command": "bash", "run_command": "bash",
		// read
		"read": "read", "read_file": "read", "readfile": "read", "cat": "read",
		// write
		"write": "write", "write_file": "write", "writefile": "write", "file_write": "write",
		// edit — `apply_patch` is codex's own edit tool, not a phantom name.
		"edit": "edit", "edit_file": "edit", "file_edit": "edit", "apply_patch": "edit",
		"multiedit": "edit", "edit_mode": "edit", "str_replace": "edit", "search_replace": "edit",
		// notebook
		"notebookedit": "notebookedit", "notebook_edit": "notebookedit",
		// search — pi names its glob tool `find`.
		"glob": "glob", "find": "glob", "grep": "grep",
		// web
		"webfetch": "webfetch", "web_fetch": "webfetch", "fetchurl": "webfetch", "web": "webfetch",
		"websearch": "websearch", "web_search": "websearch",
		// the schema loader for deferred tools
		"toolsearch": "toolsearch", "tool_search": "toolsearch",
		// the background-task pair, one key per tool
		"taskoutput": "taskoutput", "task_output": "taskoutput",
		"taskstop": "taskstop", "task_stop": "taskstop",
		// misc
		"ls": "ls", "list_dir": "ls", "todowrite": "todowrite", "todo_write": "todowrite",
		"agent": "agent", "task": "agent", "spawn_subagent": "agent",
		"use_tool": "use_tool",
	}
	got := CanonicalRows()
	for k, v := range got {
		w, ok := want[k]
		if !ok {
			t.Errorf("row %q → %q is not pinned: a row grants through `tools:` AND bounds through `allow:/ask:/deny:`, and `allow:` widens — argue it here first", k, v)
			continue
		}
		if w != v {
			t.Errorf("row %q → %q, pinned as %q", k, v, w)
		}
	}
	for k, v := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("row %q → %q was deleted: a rule spelled %q stops bounding the tool it names", k, v, k)
		}
	}
	// Every canonical VALUE is a fixed point, or no name can ever reach it.
	for _, v := range got {
		if c := CanonicalToolName(v); c != v {
			t.Errorf("canonical %q is not a fixed point (→ %q)", v, c)
		}
	}
	// No key may carry an MCP prefix: those are returned verbatim before the
	// table is consulted, so such a row would be dead on one side only.
	for k := range got {
		if len(k) >= 4 && k[:4] == "mcp_" {
			t.Errorf("row %q carries an MCP prefix: CanonicalToolName returns those verbatim, so the row would be read by one consumer and skipped by the other", k)
		}
	}
}

// Which backends RECEIVE a node's `tools:` list, pinned. The default answer
// is the safe one — a backend does not receive it until someone proves it
// does — so this test is what makes adding a backend to the set a deliberate
// act rather than a side effect.
//
// `opencode` is the worked example (#1640): it drives `opencode --format json
// run` with the prompt on stdin and reads no tool list, so `tools: [Bash,
// Read]` on such a node validated with zero diagnostics until C270.
func TestOnlyTheBackendsThatReceiveTheToolListAreNamed(t *testing.T) {
	for _, backend := range []string{"claw", "claude_code", "codex"} {
		if !ReceivesToolList(backend) {
			t.Errorf("%q narrows on the tools: list but is not named as receiving it", backend)
		}
	}
	for _, backend := range []string{"pi", "kimi", "grok", "opencode", "", "auto", "something_new"} {
		if ReceivesToolList(backend) {
			t.Errorf("%q does not receive the tools: list — C270's silence there would be a lie", backend)
		}
	}
}
