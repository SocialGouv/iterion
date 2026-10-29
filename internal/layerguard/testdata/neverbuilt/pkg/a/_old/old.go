// `go list ./pkg/...` skips a directory whose name starts with _, so no
// context compiles this file and no context ever can: a guard that demands
// one prints a remedy nobody can carry out. This file carries no build
// constraint, so the coverage half of the fixture's guard is the only one it
// can make red.
package old

// Name answers something, so the package has a body.
func Name() string { return "old" }
