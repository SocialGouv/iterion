package toolcatalog

import "strings"

// CanonicalToolName collapses every spelling of the SAME tool onto one key.
//
// It is the ONE table two different questions read, and the reason they
// cannot disagree about a word:
//
//   - what a `tools:` entry GRANTS — `delegate.claudeNativeToolsForAllowed`
//     projects this key onto Claude Code's native roster;
//   - what an `allow:`/`ask:`/`deny:` rule BOUNDS — `permission` matches a
//     rule against the key a call canonicalises to.
//
// Before the two shared a table they drifted: `run_command` granted native
// Bash through the first and matched nothing in the second, so a node could
// grant a tool by a spelling the policy could not bound (#1579).
//
// # What belongs here, and what must never
//
// A row asserts that two spellings ARE the same tool. Nothing weaker: an
// `allow:` rule WIDENS, so mapping a narrower intent onto a broader tool
// grants more than the author wrote — `allow: ["git_diff"]` collapsed onto
// `bash` would hand over the whole shell. The advisory map that turns a
// phantom name into a suggestion is `legacyNames`, and it is deliberately not
// imported here.
//
// Two tools that merely resemble each other are also not one: `workspace_grep`
// is not `grep` and `diagnostic_shell` is not `bash` — the catalogue's own
// `bots/copilot` allows the first of each pair while denying the second, so
// merging them would deny a shipped bot its own allowed tool.
//
// An unknown name is lower-cased and returned as itself, so a rule still
// matches its own spelling.
//
// A backend whose gate is REFUSED needs no rows: opencode spells its verbs
// `bash`/`read`/`edit`/`glob`/`grep`/`list`/`webfetch`/`task`, and `list` is
// not this table's `ls` — but a gated opencode node is refused at compile
// time (C176) and at dispatch, so no policy is ever evaluated against those
// names. Adding rows for them would assert a synonymy nothing exercises, and
// `allow:` widens. The divergence is recorded on #1640.
func CanonicalToolName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	// MCP FQNs (mcp__server__tool / mcp_server_tool) are kept verbatim so
	// server-scoped globs match, and are checked BEFORE the table rather
	// than after: a row whose key carried an mcp prefix would be read by one
	// consumer and skipped by the other, which is #1579 again.
	if strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, "mcp_") {
		return n
	}
	if c, ok := canonicalToolNames[n]; ok {
		return c
	}
	return n
}

// CanonicalRows is the table itself, copied. Its only consumer is the test
// that pins every row: a row is an assertion that two spellings are the same
// tool, and `allow:` widens, so a new one has to be argued in the test before
// it can grant anything.
func CanonicalRows() map[string]string {
	out := make(map[string]string, len(canonicalToolNames))
	for k, v := range canonicalToolNames {
		out[k] = v
	}
	return out
}

var canonicalToolNames = map[string]string{
	// shell. `run_command` is accepted as a Bash grant by the claude_code
	// projection, so a rule spelled that way has to bound the same tool.
	"bash": "bash", "shell": "bash", "sh": "bash",
	"run_terminal_command": "bash", "run_command": "bash",
	// read
	"read": "read", "read_file": "read", "readfile": "read", "cat": "read",
	// write
	"write": "write", "write_file": "write", "writefile": "write", "file_write": "write",
	// edit. `apply_patch` is codex's own edit tool, not a phantom name.
	"edit": "edit", "edit_file": "edit", "file_edit": "edit", "apply_patch": "edit",
	"multiedit": "edit", "edit_mode": "edit", "str_replace": "edit", "search_replace": "edit",
	// notebook
	"notebookedit": "notebookedit", "notebook_edit": "notebookedit",
	// search. pi names its glob tool `find` (it takes a `pattern`, not a
	// directory to walk), so without the alias a `Glob(**)` rule silently
	// fails to match it and the gate reaches a different verdict on pi than
	// on the other backends.
	"glob": "glob", "find": "glob", "grep": "grep",
	// web
	"webfetch": "webfetch", "web_fetch": "webfetch", "fetchurl": "webfetch", "web": "webfetch",
	"websearch": "websearch", "web_search": "websearch",
	// schema loader for deferred tools: claw spells it `tool_search`, Claude
	// Code `ToolSearch`, and the two used to canonicalise apart.
	"toolsearch": "toolsearch", "tool_search": "toolsearch",
	// the background-task pair. claw registers `task_output`/`task_stop`;
	// claude_code's orchestration surface spells them `TaskOutput`/`TaskStop`
	// — and docs/permissions.md sends authors to a `deny:` rule for exactly
	// these, so the two spellings have to land on one key.
	"taskoutput": "taskoutput", "task_output": "taskoutput",
	"taskstop": "taskstop", "task_stop": "taskstop",
	// misc
	"ls": "ls", "list_dir": "ls", "todowrite": "todowrite", "todo_write": "todowrite",
	"agent": "agent", "task": "agent", "spawn_subagent": "agent",
	"use_tool": "use_tool",
}
