//go:build !linux && !darwin && !windows

package runtime

import (
	"fmt"
	"time"
)

func worktreeProcessReferences(root string, _ time.Time) ([]int, error) {
	return nil, fmt.Errorf("process-reference proof for %s is unsupported on this platform", root)
}
