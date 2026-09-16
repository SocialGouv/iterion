// Package docsguard holds tests that keep this repository's documentation
// honest from inside the test suite, where a required check can see them.
//
// Documentation is prose, so nothing type-checks it and nothing fails when a
// claim it makes stops being true. The guards here are the substitute, and
// each one asserts a property against the files themselves rather than
// against a copy of them — the sibling of internal/ciguard, which does the
// same for CI configuration.
package docsguard
