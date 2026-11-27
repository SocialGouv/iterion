// Package itertoken holds the zero-trace guard for the deprecated workflow
// extension that `.bot` replaced: TestNoIterExtensionAnywhere greps every
// tracked file of the repository for its token and fails when it creeps back
// into a source, a fixture, a doc or a config.
//
// The guard reads the whole tree and imports nothing it judges, so it lives
// in a package of its own rather than beside the CLI: a test selector that
// picks a package by what its tests can reach selects this one — a fraction
// of a second — on every change, and no longer selects pkg/cli for it.
//
// This comment names the extension only by description: the guard's own
// file is the one carve-out it makes, and every other file here is judged.
package itertoken
