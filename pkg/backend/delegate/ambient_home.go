package delegate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ambientOverlayDirName holds the per-spawn homes homeWithoutInstructions
// builds inside a CLI's own home. Inside, so that a file the CLI replaces by
// rename (a refreshed auth.json) can be carried back with an atomic rename on
// the same filesystem.
const ambientOverlayDirName = ".iterion-ambient"

// ambientOverlayStaleAfter is the age at which a leftover overlay home counts
// as orphaned by a dead process and gets swept.
const ambientOverlayStaleAfter = 24 * time.Hour

// homeWithoutInstructions returns a directory that serves every entry of a
// CLI's home through a symlink except the operator instruction files named in
// hide — what a policy without the operator origin serves a CLI that reads
// those files from its home whatever the working directory (ADR-119). When
// home holds none of them it returns home itself and builds nothing.
//
// Two of the CLI's writes need care. The directories named in mkdirs are
// created in home first, for a CLI that must find in its real home a
// directory it would otherwise create inside the overlay. And a file the CLI
// replaces by rename (a refreshed auth.json) or creates becomes a regular
// entry of the overlay: release renames it back into home before removing the
// overlay, so a rotated refresh token is never lost.
func homeWithoutInstructions(backend, home string, hide, mkdirs []string) (dir string, release func() error, err error) {
	noop := func() error { return nil }
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return home, noop, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("delegate: %s: read the home %s: %w", backend, home, err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return slices.Contains(hide, e.Name()) }) {
		return home, noop, nil
	}
	if len(mkdirs) > 0 {
		for _, name := range mkdirs {
			if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
				return "", nil, fmt.Errorf("delegate: %s: prepare %s in the home: %w", backend, name, err)
			}
		}
		// Re-read: the listing above predates mkdirs, which must be linked.
		if entries, err = os.ReadDir(home); err != nil {
			return "", nil, fmt.Errorf("delegate: %s: read the home %s: %w", backend, home, err)
		}
	}
	root := filepath.Join(home, ambientOverlayDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("delegate: %s: prepare the overlay root: %w", backend, err)
	}
	// A process killed between the spawn and its release leaves its overlay
	// behind; sweep the stale siblings (older than a day — no live node runs
	// that long) with the same carry-back as a normal release, so a token a
	// crashed node refreshed is not lost with them.
	sweepStaleAmbientOverlays(backend, root)
	dir, err = os.MkdirTemp(root, "home-")
	if err != nil {
		return "", nil, fmt.Errorf("delegate: %s: create the overlay home: %w", backend, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == ambientOverlayDirName || slices.Contains(hide, name) {
			continue
		}
		if err := os.Symlink(filepath.Join(home, name), filepath.Join(dir, name)); err != nil {
			return "", nil, errors.Join(fmt.Errorf("delegate: %s: link %s into the overlay home: %w", backend, name, err), releaseAmbientOverlay(backend, home, dir))
		}
	}
	return dir, func() error { return releaseAmbientOverlay(backend, home, dir) }, nil
}

// sweepStaleAmbientOverlays releases every overlay home under root whose
// directory mtime is older than ambientOverlayStaleAfter — the residue of a
// process killed before its release. Each one goes through the same
// carry-back as a normal release, best-effort: a node that died left nothing
// to report the error to, and the directories it cannot carry stay for a
// human rather than being deleted in silence.
func sweepStaleAmbientOverlays(backend, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < ambientOverlayStaleAfter {
			continue
		}
		_ = releaseAmbientOverlay(backend, filepath.Dir(root), dir)
	}
}

// releaseAmbientOverlay carries back into home every entry the CLI created or
// replaced in the overlay, then removes the overlay, which by then holds only
// symlinks. A directory that already exists in home is left where it is and
// reported, rather than merged or lost in silence.
func releaseAmbientOverlay(backend, home, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("delegate: %s: read the overlay home %s: %w", backend, dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		src, dst := filepath.Join(dir, e.Name()), filepath.Join(home, e.Name())
		if e.IsDir() {
			if _, statErr := os.Lstat(dst); statErr == nil {
				errs = append(errs, fmt.Errorf("delegate: %s: %s was created in the overlay home but already exists in %s; left in %s", backend, e.Name(), home, src))
				continue
			}
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("delegate: %s: carry %s back into %s: %w", backend, e.Name(), home, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delegate: %s: remove the overlay home %s: %w", backend, dir, err)
	}
	_ = os.Remove(filepath.Join(home, ambientOverlayDirName)) // only succeeds once empty
	return nil
}
