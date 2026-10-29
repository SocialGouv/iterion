// Package a is the half of this fixture every context compiles. It imports
// both directories the listing drops, so the fixture holds the edge
// unjudgedImports must name: out of the listing is not out of the build.
package a

import (
	hidden "example.com/neverbuilt/pkg/a/.hidden"
	old "example.com/neverbuilt/pkg/a/_old"
)

// Name answers something, so the package has a body, and reaches both
// packages no pattern of this module names.
func Name() string { return "a" + old.Name() + hidden.Name() }
