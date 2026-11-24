//go:build crossbuildonly

// Only -tags crossbuildonly compiles this file. The tag is named nowhere
// but here and in the test's expectation: the guard reads it off this tree.
package ir

import "example.com/crossbuild/pkg/server"

// Compose crosses from the leaf into the composition root.
func Compose() string { return server.Name() }
