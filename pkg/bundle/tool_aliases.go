package bundle

// ToolAliasesSince is the first release containing the runtime alias resolver.
// A floor below that release would admit a runner that still treats Read as MCP
// only. See docs/tool-name-aliases.md for the release/rollback checklist.
//
// The pin is held by TestSyntaxFloorsNameReleasesThatExist, the same release
// test that holds the parser floors, and realigned by the release cut itself
// (internal/floorsalign, release-it's before:git:beforeRelease hook):
// while the release is uncut the constant must be exactly the next minor above the changelog's
// newest release, and once cut, the release's notes must carry the syntax's
// word ("alias") — so the number cannot rot the way 3.144.0 and 3.146.0 did
// while the resolver waited on a branch, and a release taken without the
// resolver turns the test red instead of shipping a floor that admits a
// runner without the feature.
const ToolAliasesSince = "3.179.0"

// AllowsToolAliases makes requires.iterion the explicit opt-in to the newer
// resolution semantics. Old files without this floor retain MCP-only shorthand
// resolution. In particular, adding support never steals their Read MCP tool.
//
// This is the runtime half of the alias floor; the authoring half — a bundle
// whose sources spell an alias in a tool list is asked for this floor by the
// one predicate the push admission, `validate`'s C252, `dsl migrate` and the
// scaffold read — is the syntaxFloors entry (profile.go).
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
