//go:build darwin

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// macOS exposes cwd/open/mapped files through lsof. The stock OS ships lsof;
// if an installation removes it or denies inspection, cleanup fails closed and
// retains the recovery worktree.
func worktreeProcessReferences(root string, _ time.Time) ([]int, error) {
	lsofPath, err := exec.LookPath("lsof")
	if err != nil {
		return nil, fmt.Errorf("locate lsof for process-reference proof: %w", err)
	}
	cmd := exec.Command(lsofPath, "-a", "-u", strconv.Itoa(os.Geteuid()), "+D", root, "-F", "p")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		// lsof exits 1 when its selection has no matching open files.
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("lsof recovery worktree references: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	seen := make(map[int]struct{})
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(line, "p"))
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}
