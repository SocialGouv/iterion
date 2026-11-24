//go:build !linux

package scripthost

import "syscall"

// applyParentDeathSignal is a no-op off Linux: no other platform this tree
// builds for offers a parent-death signal. Nothing is lost that matters, since
// [Confine] refuses off Linux anyway.
func applyParentDeathSignal(*syscall.SysProcAttr) {}
