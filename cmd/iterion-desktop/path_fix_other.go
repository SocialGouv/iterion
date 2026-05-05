//go:build !darwin

package main

// applyMacOSPathFix is a no-op on non-Darwin platforms — Linux desktop
// activation and Windows process inheritance both pass the user's PATH
// correctly.
func applyMacOSPathFix() error { return nil }
