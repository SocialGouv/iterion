// Package runtimedocs holds the pkg/runtime guards whose subject is an
// operator page: docs/sandbox.md's bounded promise about the image-ENV
// screen, the residue it states and the engine guard lifts it enumerates on
// the side the screen puts them, and the default identical-failure limit that
// docs/environment-variables.md and docs/resume.md state once each. Each
// reads the page against the production answer, so a page and an engine that
// disagree go red here.
//
// They live outside pkg/runtime because they read pages, not engine code:
// kept there, an edit to one of those pages would select the engine's whole
// suite. pkg/runtime and pkg/sandbox/devcontainer reach this package through
// its imports, so a change to the answers still selects these guards.
package runtimedocs
