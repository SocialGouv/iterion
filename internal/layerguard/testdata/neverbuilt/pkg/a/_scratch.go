// The FILE half of the convention, underscore side: go/build drops a file
// whose name starts with _ in every context, and no import can reach a
// file, so no build of package a compiles this one — `go build ./...`
// leaves it out and only an explicit `go run` would read it. Its import is
// here to show what such a file may hold: the walk must demand no context
// for it, or the guard is red over a tree the toolchain builds clean.
package a

import "example.com/neverbuilt/internal/b"

// Scratch is never compiled as part of package a.
func Scratch() string { return b.Name() }
