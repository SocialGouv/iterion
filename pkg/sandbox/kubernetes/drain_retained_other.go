//go:build !unix

package kubernetes

import "os"

func openDrainRootFromRetained(_ *os.Root, _ string) (*drainDir, error) {
	return nil, errDrainNeedsDirDescriptors
}
