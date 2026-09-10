// Package ciguard holds tests that keep this repository's CI configuration
// honest from inside the test suite, where a required check can see them.
//
// A workflow file is configuration, so nothing type-checks it and nothing
// fails when two places that must agree stop agreeing. The guards here are
// the substitute: each one asserts a property the workflow's own comments
// claim, against the file the workflow actually runs from.
package ciguard
