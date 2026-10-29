// `go list ./pkg/...` names no package in a directory whose name starts
// with _, so no context of the listing compiles this file and none ever
// can: a guard that demands one prints a remedy nobody can carry out. The
// package is importable all the same — package a imports it, `go build
// ./...` ships it, and unjudgedImports is what names that edge. This file
// carries no build constraint, so the coverage half of the fixture's guard
// is the only one it can make red.
package old

// Name answers something, so the package has a body.
func Name() string { return "old" }
