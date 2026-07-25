//go:build windows

package runtime

import (
	"fmt"
	"time"
)

// A Windows handle opened with FILE_SHARE_DELETE can remain writable while a
// delete is pending, so successful Git removal is not a quiescence proof.
// Preserve recovery worktrees until a Job Object/handle census is implemented.
func worktreeProcessReferences(root string, _ time.Time) ([]int, error) {
	return nil, fmt.Errorf("Windows process-reference proof is not implemented for %s", root)
}
