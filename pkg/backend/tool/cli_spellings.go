package tool

import "strings"

// cliToolSpellings folds Claude Code's frontmatter spellings onto the tool
// names a claw node's effective set carries. Lower-cased first, so
// `Glob`/`Grep`/`Bash` need no rows; the rows are the spellings that differ
// past case. toolcatalog.CanonicalToolName is NOT this table: it folds onto
// canonical CONCEPTS (`read`, `webfetch`), which are not the registry's tool
// names (`read_file`, `web_fetch`), and toolcatalog.BuiltinAlias's exact-case
// tier covers three of these. A frontmatter name no row and no effective
// tool claims simply intersects to nothing — narrowing can only ever remove.
//
// The two readers are the surfaces that honour a workspace file's tool list:
// the workspace-agent resolver (pkg/backend/model, which cannot be imported
// from here — model imports tool) and the `command` tool's `allowed-tools:`
// narrowing (#1717). One table, so a command and an agent file never fold
// the same spelling two ways.
var cliToolSpellings = map[string]string{
	"read":         "read_file",
	"write":        "write_file",
	"edit":         "file_edit",
	"multiedit":    "file_edit",
	"notebookedit": "notebook_edit",
	"webfetch":     "web_fetch",
	"websearch":    "web_search",
	"todowrite":    "todo_write",
	"task":         "agent",
}

// FoldCLISpelling folds one Claude Code frontmatter spelling (`Read`,
// `WebFetch`, `TodoWrite`) onto an iterion tool name (`read_file`,
// `web_fetch`, `todo_write`). Three more shapes fold (#1717, review LOW-5):
//
//   - the PREFIXED form `Bash(git:*)` folds to `bash`: iterion has no
//     per-prefix granularity, so the form authorizes the WHOLE tool — the
//     documented approximation, narrower than the node's set and wider than
//     the prefix the file named;
//   - the claude_code MCP FQN `mcp__srv__tool` folds to the sanitized
//     spelling the tools are advertised under here (`mcp_srv_tool`);
//   - a spelling no row and no shape claims lower-cases to itself, so
//     `Grep` answers `grep` and an unknown name stays a name that resolves
//     to nothing — folding narrows, it never invents a tool.
func FoldCLISpelling(spelling string) string {
	n := strings.ToLower(strings.TrimSpace(spelling))
	if i := strings.IndexByte(n, '('); i > 0 {
		n = n[:i]
	}
	if server, name, ok := SplitMCPFQN(n); ok {
		return "mcp_" + server + "_" + name
	}
	if folded, ok := cliToolSpellings[n]; ok {
		return folded
	}
	return n
}
