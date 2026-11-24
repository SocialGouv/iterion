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
// root — or one run family's subtree under its runs/ entry — with the age
// that decides its fate.
type ScratchEntry struct {
	// Name is the entry's path RELATIVE to the scratch root: a top-level
	// name, or runs/<id> for a run family's subtree. Removal resolves it
	// under the root (RemoveScratchEntry), never through Path.
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
//
// The runs/ entry (RunScratchDirName) is not one entry: it holds every run
// family's own subtree (${RUN_SCRATCH_DIR}, #2869), so judged whole its
// newest mtime is the newest of ANY run and a stale subtree would never
// be reclaimed while the repository keeps running bots. Each runs/<id> is
// listed and aged as its own entry instead. A runs/ that is a symlink is
// not descended — it is aged as an ordinary entry, the link itself.
func ScanScratch(root string, retention time.Duration, now time.Time) ([]ScratchEntry, error) {
	if root == "" {
		return nil, nil
	}
	// A root that is a symlink is refused, not followed: the sweep would
	// remove the stale entries wherever it points, outside the store the
	// scratch belongs to.
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("scratch: %s is a symlink — refused", root)
	}
	items, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scratch: read %s: %w", root, err)
	}
	var out []ScratchEntry
	add := func(name string) {
		p := filepath.Join(root, name)
		newest, size, err := treeStat(p)
		if err != nil {
			// An unreadable entry is reported, never swept: deleting what
			// we could not inspect is how a sweep loses real work.
			return
		}
		out = append(out, ScratchEntry{
			Name:   name,
			Path:   p,
			Newest: newest,
			Size:   size,
			Stale:  retention > 0 && now.Sub(newest) >= retention,
		})
	}
	for _, it := range items {
		if it.Name() == "" || it.Name()[0] == '.' {
			continue
		}
		// The DirEntry type is the entry's own (readdir does not follow),
		// so a runs/ planted as a symlink falls through to the plain arm.
		if it.Name() == RunScratchDirName && it.IsDir() {
			runs, err := os.ReadDir(filepath.Join(root, it.Name()))
			if err != nil {
				// Unreadable, so never swept — the plain arm's rule.
				continue
			}
			for _, run := range runs {
				if run.Name() == "" || run.Name()[0] == '.' {
					continue
				}
				add(filepath.Join(RunScratchDirName, run.Name()))
			}
			continue
		}
		add(it.Name())
	}
	return out, nil
}

// RemoveScratchEntry deletes one entry ScanScratch listed, resolved under
// root through an os.Root: a runs/<id> entry crosses runs/, a directory
// sandboxed runs can write through the scratch bind, and a runs/ swapped
// for a link between the scan and the removal must not carry the
// deletion out of the scratch root. A path that escapes the root fails
// instead.
func RemoveScratchEntry(root string, e ScratchEntry) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("scratch: open %s: %w", root, err)
	}
	defer func() { _ = r.Close() }()
	if err := r.RemoveAll(e.Name); err != nil {
		return fmt.Errorf("scratch: remove %s: %w", e.Path, err)
	}
	return nil
}

// ScanNamedPaths ages the paths it is GIVEN, one entry per path, rather than
// the children of a root — for a store whose members are named by the code
// that creates them instead of discovered by listing.
//
// The sandbox's own cache store is that shape: its members are
// `.cache/go-build`, `.npm`, `.local/share/pnpm` … — dot-prefixed, which
// [ScanScratch] skips by design, and nested, which listing one level would
// miss. Age decides the same way and for the same reason: a cache nothing has
// touched within the retention window is one no live run is warming.
//
// A path that is a symlink is refused rather than followed, exactly as a
// symlinked scratch root is: the sweep would otherwise delete wherever a run
// pointed it.
func ScanNamedPaths(paths []string, retention time.Duration, now time.Time) ([]ScratchEntry, error) {
	var out []ScratchEntry
	var firstErr error
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("scan %s: %w", p, err)
			}
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("scan: %s is a symlink — refused", p)
			}
			continue
		}
		newest, size, err := treeStat(p)
		if err != nil {
			// Reported, never swept: deleting what we could not inspect is
			// how a sweep loses real work.
			if firstErr == nil {
				firstErr = fmt.Errorf("scan %s: %w", p, err)
			}
			continue
		}
		out = append(out, ScratchEntry{
			Name:   filepath.ToSlash(filepath.Base(p)),
			Path:   p,
			Newest: newest,
			Size:   size,
			Stale:  retention > 0 && now.Sub(newest) >= retention,
		})
	}
	return out, firstErr
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
			if err := RemoveScratchEntry(root, e); err != nil {
				return swept, freed, err
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
