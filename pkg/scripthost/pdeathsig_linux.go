//go:build linux

package scripthost

import "syscall"

// applyParentDeathSignal makes the kernel SIGKILL the child when its parent
// dies. The signal is delivered when the forking THREAD exits, not the
// process, which is why [Host.Run] treats it as a backstop and not as the
// orphan bound.
func applyParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
