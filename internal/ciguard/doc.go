// Package ciguard holds tests that keep this repository's CI configuration
// honest from inside the test suite, where a required check can see them.
//
// A workflow file is configuration, so nothing type-checks it and nothing
// fails when two places that must agree stop agreeing. The guards here are
// the substitute: each one asserts a property the workflow's own comments
// claim, against the file the workflow actually runs from.
//
// A guard that runs a shipped script hands its path to scriptUnderTest, which
// READS it in the test process first: `go test` keys a cached result on the
// files that process opened, so a script only a child shell reads can be
// replaced under a warm cache and the previous PASS replayed. Register entry:
// docs/agents/testing/flakes.md.
package ciguard
