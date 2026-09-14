package bundle

// ToolAliasesSince is PROVISIONAL until the change is released. Before merging,
// align it with the first release containing the runtime alias resolver. A floor
// below that release would admit a runner that still treats Read as MCP only.
// See docs/tool-name-aliases.md for the release/rollback checklist.
const ToolAliasesSince = "3.144.0"

// AllowsToolAliases makes requires.iterion the explicit opt-in to the newer
// resolution semantics. Old files without this floor retain MCP-only shorthand
// resolution. In particular, adding support never steals their Read MCP tool.
func AllowsToolAliases(m *Manifest) bool {
	if m == nil || m.Requires == nil {
		return false
	}
	c, err := ParseEngineConstraint(m.Requires.Iterion)
	if err != nil {
		return false
	}
	minimum, ok := numericVersionParts(ToolAliasesSince)
	return ok && compareVersionParts(c.Min, minimum) >= 0
}
