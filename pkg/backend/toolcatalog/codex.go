package toolcatalog

import "strings"

// CodexDeclaredListIsReadOnly reports whether a DECLARED `tools:` list alone
// puts codex in its read-only sandbox — the one bound a list buys there: codex
// runs its built-in shell outside the SDK's per-name callbacks, so the list
// picks a sandbox MODE and nothing else. The codex delegate picks the mode
// from this very table, and every pre-run reader of a node's surface — the
// compiler's C316, the shared-worktree admission in pkg/runtime — asks the
// same question here, so no reader can drift from the sandbox the task gets.
//
// The read-only set is CODEX's, and it OVERLAPS a reader's own read-only
// vocabulary rather than nesting inside it, in both directions. It is `read`,
// `glob`, `grep`, `ls`, `websearch`, `toolsearch`, `todowrite` and `skill` by
// CANONICAL name, and nothing else: `web_fetch`, `git_diff`, `git_status`,
// `list_files`, `search_codebase` and `tree` buy workspace-write here while
// pkg/runtime reads them as read-only, and `ls`, `websearch`, `toolsearch`,
// `todowrite` and `skill` are read-only here while that map does not carry
// them at all. The canonicalisation is the other half of the difference: this
// classifier reads `Read` and `read_file` as the same tool, a reader matching
// its own map literally does not.
//
// It answers for what the AUTHOR declared. An undeclared list is no bound at
// all — the native toolset, unrestricted, throughout Iterion — and the
// runtime's own appends are outside the question: `interaction:` puts
// `ask_user` on a non-empty list, and that name buys workspace-write.
// `readonly:` and `full_access:` are the author's own locks, read by the
// delegate before a declared list is ever classified.
func CodexDeclaredListIsReadOnly(tools []string, declared bool) bool {
	if !declared && len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		if codexWebSearchSpelling(t) {
			continue
		}
		switch CanonicalToolName(t) {
		case "read", "glob", "grep", "ls", "websearch", "toolsearch", "todowrite", "skill":
			continue
		default:
			// `bash`, `edit`, `write` and `notebookedit` write; an unknown or
			// custom name cannot prove the task is read-only, so writer
			// semantics are preserved — `readonly:` remains the lock.
			return false
		}
	}
	return true
}

// codexWebSearchSpelling is every spelling the codex delegate accepts for its
// web search, a read under each of them.
func codexWebSearchSpelling(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search", "websearch", "web-search":
		return true
	}
	return false
}
