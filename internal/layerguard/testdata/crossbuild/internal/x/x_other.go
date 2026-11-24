//go:build !linux

// The other platforms' half, so internal/x has a file in every context.
package x

// Name answers something, so the package has a body.
func Name() string { return "other" }
