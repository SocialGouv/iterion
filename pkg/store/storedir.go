package store

import (
	"os"
	"path/filepath"
)

// StoreDirName is the conventional directory name for an iterion run store.
const StoreDirName = ".iterion"

// ResolveStoreDir picks the run-store directory shared by the CLI and the
// editor. An explicit override wins; otherwise it walks up from start looking
// for an existing .iterion directory (git-style discovery), and falls back to
// creating one alongside start.
func ResolveStoreDir(start, override string) string {
	if override != "" {
		return override
	}
	if start == "" {
		return StoreDirName
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		return filepath.Join(start, StoreDirName)
	}

	dir := abs
	for {
		candidate := filepath.Join(dir, StoreDirName)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return filepath.Join(abs, StoreDirName)
}
