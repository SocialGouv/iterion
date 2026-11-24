//go:build linux

// A platform constraint the listing contexts already cover: tagsDemandedBy
// must drop "linux" instead of passing it as a project tag.
package x

// Name answers something, so the package has a body.
func Name() string { return "linux" }
