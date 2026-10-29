package toolcatalog

import (
	"slices"
	"strings"
)

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

// CanonicalRows is the table itself, copied. Read by the test that pins every
// row — a row is an assertion that two spellings are the same tool, and
// `allow:` widens, so a new one has to be argued there before it can grant
// anything — and by the delegate's own vocabulary derivation, which folds
// every spelling onto WritingCanonicals to learn what an author may write a
// file with.
func CanonicalRows() map[string]string {
	out := make(map[string]string, len(canonicalToolNames))
	for k, v := range canonicalToolNames {
		out[k] = v
	}
	return out
}

// WritingCanonicals are the canonical concepts of this table whose tool
// WRITES A FILE, in the order the roster projections read them. Copied, so a
// consumer cannot grow the set for everyone else.
//
// They are concepts, not spellings: every accepted synonym folds onto one of
// them through CanonicalToolName, so `write_file`, `apply_patch` and
// `str_replace` are the same three members as `write` and `edit`.
//
// The set lives beside the spelling table for the table's own reason: every
// reader of it answers a question about the same word, and they may not
// disagree. The enumeration carries no count — a numeral is the part of an
// enumeration that goes stale — and it is held by a run,
// TestTheWritingSetsDocNamesEveryReaderOfIt, which reads the callers out of
// the tree:
//
//   - what `readonly: true` withholds from every claude_code spawn
//     (delegate.claudeReadonlyWithheld, through claudeNativeForCanonical);
//   - which of those tools the node's own `tools:` list asked for, so the
//     operator notice can name the declaration the flag deleted
//     (delegate.claudeWritersTheDeclarationNames);
//   - the two corpus rules that keep the flag out of the tree beside a
//     declared writer, #1816:
//     bots.TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool (through
//     declaredWriters) and
//     botscaffold.TestNoScaffoldedNodeDeclaresReadonlyWithAWritingTool;
//   - the writing vocabulary every readonly argv row is asserted over
//     (delegate.claudeWritingDeclarations, floored by
//     delegate.TestTheWritingVocabularyIsReadFromTheTable);
//   - the durable-memory question's own reading of the set, which ADDS the
//     shell and claw's `memory_write` (DurableWriteCanonicals below, read by
//     bots.TestABundleSellsADurableBriefOnlyWhereItCanWriteOne).
//
// Each held its own literal, and nothing held them equal: growing the
// delegate's left both corpus rules silently narrower than the rule they
// enforce.
//
// `bash` is NOT a member, and the omission is the contract, not an oversight:
// a shell writes any file the process can reach, but `readonly:` KEEPS it on
// claude_code by design (the flag withholds tools, it is not a sandbox —
// #1651), so a corpus rule that read the shell as a declared writer would
// refuse the 26 shipped read-only nodes that declare the tool the flag leaves
// them (measured: the set added to this list, then
// bots.TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool). codex counts the shell for a different verdict — which sandbox mode
// its declaration needs (codexSandboxForAllowedTools) — and that question is
// its own.
func WritingCanonicals() []string {
	return slices.Clone(writingCanonicals)
}

var writingCanonicals = []string{"write", "edit", "notebookedit"}

// DurableWriteCanonicals are the canonical concepts whose tool can put bytes
// in a bot's workspace memory tree — the question "could this node keep a
// durable file across sessions at all". Copied, like the set it derives from.
//
// It is WritingCanonicals plus two members, and both additions are the whole
// point of asking separately rather than reusing that set:
//
//   - **the shell.** The memory tree is a directory under
//     `$ITERION_HOME/projects/<key>/memory/`, so a node that holds `bash`
//     writes it — that is exactly what a claude_code recipe used to tell a
//     bot to do (iterion#2773). WritingCanonicals excludes the shell by
//     CONTRACT, because `readonly:` KEEPS it on claude_code and a corpus rule
//     reading it as a declared writer would refuse 26 shipped nodes. That
//     contract is about what a tool-withholding FLAG removes; this question is
//     about what a process can write, and codex's sandbox reading is the
//     precedent for a second reading of the shell living beside the first.
//   - **`memory_write`.** Claw's own durable-write builtin is on no roster the
//     spelling table projects, so it has no canonical row and folds onto
//     itself; a question about durable memory that omitted it would miss the
//     only tool named for it.
//
// Members are canonical concepts: fold a declaration through
// CanonicalToolName before testing membership, never compare raw spellings.
func DurableWriteCanonicals() []string {
	return append(slices.Clone(writingCanonicals), "bash", "memory_write")
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
