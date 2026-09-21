package toolcatalog

// BuiltinAlias returns the canonical Claw builtin for an exact Claude Code
// spelling. This is the LAST resolution tier: a registered exact name or MCP
// shorthand always wins, and an ambiguous MCP shorthand must never fall back.
// No case folding, qualified-name rewriting or wildcard expansion happens here.
func BuiltinAlias(name string) string {
	switch name {
	case "Read":
		return "read_file"
	case "Bash":
		return "bash"
	case "Grep":
		return "grep"
	default:
		return ""
	}
}
