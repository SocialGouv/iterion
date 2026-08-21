package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultScratchRetention is how long an untouched scratch entry is kept.
// It matches `iterion clean`'s own --older-than default: the two sweep the
// same directories and disagreeing would make the automatic one delete
// what the manual one had just reported as spared.
const DefaultScratchRetention = 168 * time.Hour

// EnvScratchRetention overrides that retention. A duration widens or
// narrows it; "off" (or "0") disables the automatic sweep entirely.
const EnvScratchRetention = "ITERION_SCRATCH_RETENTION"

// ScratchEntry is one top-level directory under a workspace's scratch
// root, with the age that decides its fate.
type ScratchEntry struct {
	Name string
	Path string
	// Newest is the most recent mtime anywhere in the entry's subtree.
	// The root directory's own mtime is not enough: writing INTO a
	// subdirectory does not touch the ancestors, so a run actively
	// filling scratch/foo/bar/ leaves scratch/foo looking untouched.
	Newest time.Time
	Size   int64
	// Stale reports that nothing in the entry has changed within the
	// retention window — which is what makes it unreachable by any live
	// run, since a run that is still working writes as it goes.
	Stale bool
}

// ScanScratch lists the top-level entries of a workspace scratch root and
// marks the ones untouched for at least retention.
//
// Age IS the concurrency guard, and it has to be: scratch is deliberately
// SHARED across runs — a subbot child writes into its parent's scratch,
// which is how fan-in works — so nothing can be attributed to one run and
// deleted on its behalf. What can be said is that an entry no process has
// touched in days is one no live run is using.
//
// Dot-prefixed entries are left alone, mirroring `iterion clean`'s rule
// for the worktree root: they hold state shared across runs rather than
// one run's working files.
func ScanScratch(root string, retention time.Duration, now time.Time) ([]ScratchEntry, error) {
	if root == "" {
		return nil, nil
	}
	items, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scratch: read %s: %w", root, err)
	}
	var out []ScratchEntry
	for _, it := range items {
		if it.Name() == "" || it.Name()[0] == '.' {
			continue
		}
		p := filepath.Join(root, it.Name())
		newest, size, err := treeStat(p)
		if err != nil {
			// An unreadable entry is reported, never swept: deleting what
			// we could not inspect is how a sweep loses real work.
			continue
		}
		out = append(out, ScratchEntry{
			Name:   it.Name(),
			Path:   p,
			Newest: newest,
			Size:   size,
			Stale:  retention > 0 && now.Sub(newest) >= retention,
		})
	}
	return out, nil
}

// treeStat returns the newest mtime and the total size under path.
func treeStat(path string) (time.Time, int64, error) {
	var newest time.Time
	var size int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
		if !d.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return newest, size, err
}

// SweepScratch deletes the stale entries of a workspace scratch root and
// returns what it removed (or, when dryRun, what it would remove) plus
// the bytes reclaimed. A missing root is not an error — most workspaces
// never write scratch at all.
func SweepScratch(root string, retention time.Duration, now time.Time, dryRun bool) ([]ScratchEntry, int64, error) {
	entries, err := ScanScratch(root, retention, now)
	if err != nil {
		return nil, 0, err
	}
	var swept []ScratchEntry
	var freed int64
	for _, e := range entries {
		if !e.Stale {
			continue
		}
		if !dryRun {
			if err := os.RemoveAll(e.Path); err != nil {
				return swept, freed, fmt.Errorf("scratch: remove %s: %w", e.Path, err)
			}
		}
		swept = append(swept, e)
		freed += e.Size
	}
	return swept, freed, nil
}

// ScratchRetentionFromEnv resolves EnvScratchRetention. "off"/"0"/"none"
// disable the sweep (returned as 0); anything unparseable is an error
// rather than a silent fallback, so a typo cannot quietly turn the sweep
// off — or, worse, quietly shorten it.
func ScratchRetentionFromEnv(def time.Duration) (time.Duration, error) {
	raw := os.Getenv(EnvScratchRetention)
	switch raw {
	case "":
		return def, nil
	case "off", "none", "0":
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q invalid (want a duration like 168h, or \"off\"): %w", EnvScratchRetention, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s=%q invalid (want >= 0)", EnvScratchRetention, raw)
	}
	return d, nil
}
