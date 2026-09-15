package bundle

// ToolAliasesSince is the first release containing the runtime alias resolver.
// A floor below that release would admit a runner that still treats Read as MCP
// only. See docs/tool-name-aliases.md for the release/rollback checklist.
//
// UNSET ON PURPOSE, and it FAILS CLOSED: no manifest can declare a floor at or
// above this, so AllowsToolAliases answers false everywhere and the feature
// stays inert until someone writes the real number at merge.
//
// It is a sentinel rather than a plausible version because every plausible
// version rots. This constant named 3.144.0, then 3.146.0; 3.144.0, 3.144.1,
// 3.145.0 and 3.146.x have all shipped while the resolver sat on its branch,
// each one turning the floor into a claim that a released runner carries a
// capability it does not. Releases move faster than the branch, so chasing the
// number is a race that cannot be won — and a comment saying "provisional" did
// not stop it rotting twice.
//
// Replacing it is the release step, not an afterthought: set it to the version
// that actually ships this resolver, re-run the compatibility probe, and only
// then take the PR out of draft.
const ToolAliasesSince = "9999.0.0"

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
