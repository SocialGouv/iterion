//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procfs directory timestamps and CLOCK_REALTIME can differ by a small amount
// around fork/exec. Treat processes within this window before the launcher
// boundary as in-scope; only a process conclusively older may be ignored after
// an inspection denial.
const worktreeProcessStartSkew = 10 * time.Millisecond

// worktreeProcessReferences returns same-user processes whose cwd, root, or an
// open file descriptor or memory mapping points at or below root. After an
// atomic worktree rename, these are the ordinary processes that can still
// write through the old directory inode without learning the random recovery
// path.
//
// We inspect only the effective user's processes; cleanup temporarily removes
// group/other write bits from the recovery root before calling us. A same-user
// process older than the trusted creation boundary captured outside the
// checkout cannot have been launched by that run and is skipped when procfs
// intentionally hides its links. Every newer/in-scope denial fails closed.
func worktreeProcessReferences(root string, authoritySince time.Time) ([]int, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve recovery path: %w", err)
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read procfs: %w", err)
	}
	euid := os.Geteuid()
	seen := make(map[int]struct{})
	var references []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		sameUser, err := procProcessHasEUID(pid, euid)
		if err != nil {
			if processVanished(err) {
				continue
			}
			return nil, fmt.Errorf("inspect uid for pid %d: %w", pid, err)
		}
		if !sameUser {
			continue
		}
		referencesRoot, err := procProcessReferencesPath(pid, absoluteRoot)
		if err != nil {
			if processVanished(err) {
				continue
			}
			// Linux may deny /proc/<pid> links for unrelated same-UID
			// non-dumpable processes (for example credential agents). Those
			// processes are outside Iterion's child-process authority; treating
			// them as globally blocking would make every cleanup leak forever
			// on an otherwise healthy desktop. Only processes provably older
			// than the worktree are unrelated; a newer/non-dumpable child is
			// retained fail-closed.
			if processInspectionDenied(err) {
				startedAt, startErr := procProcessStartedAt(pid)
				if startErr != nil {
					if processVanished(startErr) {
						continue
					}
					return nil, fmt.Errorf(
						"inspect start time for inaccessible pid %d: %w",
						pid,
						startErr,
					)
				}
				if !authoritySince.IsZero() &&
					startedAt.Before(authoritySince.Add(-worktreeProcessStartSkew)) {
					continue
				}
			}
			return nil, fmt.Errorf("inspect path references for pid %d: %w", pid, err)
		}
		if referencesRoot {
			if _, ok := seen[pid]; !ok {
				seen[pid] = struct{}{}
				references = append(references, pid)
			}
		}
	}
	return references, nil
}

func procProcessHasEUID(pid, euid int) (bool, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) < 2 {
			return false, fmt.Errorf("malformed Uid line %q", line)
		}
		effective, err := strconv.Atoi(fields[1])
		if err != nil {
			return false, fmt.Errorf("parse effective uid %q: %w", fields[1], err)
		}
		return effective == euid, nil
	}
	return false, fmt.Errorf("status has no Uid line")
}

func procProcessReferencesPath(pid int, root string) (bool, error) {
	procRoot := filepath.Join("/proc", strconv.Itoa(pid))
	for _, link := range []string{"cwd", "root"} {
		target, err := os.Readlink(filepath.Join(procRoot, link))
		if err != nil {
			if processVanished(err) {
				continue
			}
			return false, err
		}
		if processReferenceWithin(target, root) {
			return true, nil
		}
	}

	fdDir := filepath.Join(procRoot, "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			if processVanished(err) {
				continue
			}
			return false, err
		}
		if processReferenceWithin(target, root) {
			return true, nil
		}
	}

	// A process can close its descriptor after mmap(MAP_SHARED) and still
	// modify the underlying inode. /proc/<pid>/maps retains the mapped path;
	// omitting it would make cwd/fd census falsely quiescent.
	rawMaps, err := os.ReadFile(filepath.Join(procRoot, "maps"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(rawMaps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		mappedPath := strings.Join(fields[5:], " ")
		if processReferenceWithin(mappedPath, root) {
			return true, nil
		}
	}
	return false, nil
}

func procProcessStartedAt(pid int) (time.Time, error) {
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func processReferenceWithin(reference, root string) bool {
	reference = strings.TrimSuffix(reference, " (deleted)")
	if !filepath.IsAbs(reference) {
		return false
	}
	reference = filepath.Clean(reference)
	root = filepath.Clean(root)
	if reference == root {
		return true
	}
	return strings.HasPrefix(reference, root+string(filepath.Separator))
}

func processVanished(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
}

func processInspectionDenied(err error) bool {
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM)
}
