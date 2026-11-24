//go:build !unix

package kubernetes

import (
	"errors"
	"fmt"
	"os"
)

// errDrainNeedsDirDescriptors is the drain's refusal on a host whose OS
// offers no directory-relative calls: the drain writes every member
// beneath its root one component at a time, never through a link
// (drainDir), and a path-based write cannot promise that.
var errDrainNeedsDirDescriptors = errors.New("the run-files drain lands every member through directory descriptors (openat), which this host's OS does not offer")

// drainDir stands for the unix one; openDrainRoot never returns it.
type drainDir struct {
	path string
}

// drainPathMax is never reached: openDrainRoot refuses first.
const drainPathMax = 260

func openDrainRoot(path string) (*drainDir, error) {
	return nil, fmt.Errorf("%s: %w", path, errDrainNeedsDirDescriptors)
}

func (d *drainDir) close() {}

func (d *drainDir) child(string) (*drainDir, error) { return nil, errDrainNeedsDirDescriptors }

func (d *drainDir) kind(string) (entryKind, error) { return entryNone, errDrainNeedsDirDescriptors }

func (d *drainDir) readlink(string) (string, error) { return "", errDrainNeedsDirDescriptors }

func (d *drainDir) mkdir(string) error { return errDrainNeedsDirDescriptors }

func (d *drainDir) remove(string, bool) error { return errDrainNeedsDirDescriptors }

func (d *drainDir) rename(string, string) error { return errDrainNeedsDirDescriptors }

func (d *drainDir) createStaged() (*os.File, string, error) {
	return nil, "", errDrainNeedsDirDescriptors
}

func (d *drainDir) symlinkStaged(string) (string, error) { return "", errDrainNeedsDirDescriptors }

func (d *drainDir) openFile(string) (*os.File, error) { return nil, errDrainNeedsDirDescriptors }

func (d *drainDir) names() ([]string, error) { return nil, errDrainNeedsDirDescriptors }
