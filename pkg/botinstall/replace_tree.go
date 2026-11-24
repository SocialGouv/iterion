package botinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// ErrInstallTargetLocked is the cause of an install that stopped because a
// file of the bundle it replaces is being saved. Nothing of that file was
// written or removed: the save finishes, and the install is run again.
var ErrInstallTargetLocked = errors.New("a file of the bundle is being saved (its authoring lock was not taken)")

// installLockWait bounds how long an install waits for a save of one of the
// files it replaces. The caller's context bounds it too, and whichever ends
// first wins: a request that hangs up stops the wait, and so does an
// operator's SIGINT through cobra's context.
var installLockWait = 15 * time.Second

// holdBounded runs fn under path's authoring lock and answers whether it got
// in. The wait is bounded PER FILE — a bundle of many files must not run out
// of one budget halfway through its replacement, which would leave a tree
// half old and half new — and by the caller's context too, whichever ends
// first. The bool tells a refused TAKE, where nothing was written or
// removed, from a failure under the lock.
func holdBounded(ctx context.Context, path string, fn func() error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, installLockWait)
	defer cancel()
	held := false
	err := authoringlock.Hold(ctx, path, func() error {
		held = true
		return fn()
	})
	return held, err
}

// replaceTree makes target hold exactly what source ships — the same set
// copyTree ever copied: no .git, no .iterion, no author document, no
// irregular file — one file at a time, and every file it writes or removes
// goes under THAT file's authoring lock.
//
// It replaces the os.RemoveAll + copy pair, which took no lock at all and
// left the whole bundle MISSING for the length of the copy. `--dest bots`
// points an install straight at authored bundles (#2689 item 2), and the
// studio's authoring surface opens the materialized ones under .botz too,
// so a save in flight used to be destroyed wholesale or torn in half. Now a
// save and an install take turns, file by file, and a refused take leaves
// the bundle as it stands — not emptied.
//
// The control directory (.iterion) is the one thing the target KEEPS: it is
// the lock's own home, no walker of a bundle's content counts it
// (bundle.IsPackSkipped), and deleting it mid-install would hand the next
// save a brand-new lock file while this install still held the old one —
// which is to say no exclusion at all.
func replaceTree(ctx context.Context, source, target string) error {
	shipped, err := shippedEntries(source)
	if err != nil {
		return err
	}
	if err := pruneUnshipped(ctx, target, shipped); err != nil {
		return err
	}
	return copyShipped(ctx, source, target, shipped)
}

// shippedEntries is what an install copies, keyed by slash-separated
// bundle-relative path: true for a directory, false for a regular file.
func shippedEntries(source string) (map[string]bool, error) {
	shipped := map[string]bool{}
	err := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && isLocalStateName(d.Name()) {
			return filepath.SkipDir
		}
		if bundle.IsDraftEntry(d.Name(), d.IsDir()) {
			return nil
		}
		rel, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		shipped[filepath.ToSlash(rel)] = d.IsDir()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the bundle to install at %s: %w", source, err)
	}
	return shipped, nil
}

// isLocalStateName reports the directory names an install neither copies
// from the source nor removes from the target: the git metadata of whatever
// checkout the source came from, and the recovery directory the authoring
// lock lives in.
func isLocalStateName(name string) bool {
	return name == ".git" || name == authoringlock.RecoveryDirName
}

// pruneUnshipped removes everything under target that the source does not
// ship, deepest entry first so a directory is empty by the time its turn
// comes. A file goes under its own authoring lock; a directory the target
// keeps only because local state sits inside it stays.
func pruneUnshipped(ctx context.Context, target string, shipped map[string]bool) error {
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !info.IsDir():
		// A plain file where the bundle goes is not a tree to prune: it is
		// removed whole, under the lock of its own name.
		return removeUnderLock(ctx, target, target)
	}
	var entries []string
	dirs := map[string]bool{}
	err = filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && isLocalStateName(d.Name()) {
			return filepath.SkipDir
		}
		rel, relErr := filepath.Rel(target, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		entries = append(entries, rel)
		dirs[rel] = d.IsDir()
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the bundle at %s: %w", target, err)
	}
	// Deepest first: sorting the paths and walking them backwards puts every
	// child before its parent.
	sort.Strings(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		rel := entries[i]
		isDir, shippedAs := dirs[rel], shipped[rel]
		_, isShipped := shipped[rel]
		if isShipped && shippedAs == isDir {
			continue
		}
		path := filepath.Join(target, filepath.FromSlash(rel))
		if isDir {
			if err := removeEmptyDir(path, target); err != nil {
				return err
			}
			continue
		}
		if err := removeUnderLock(ctx, path, target); err != nil {
			return err
		}
	}
	return nil
}

// removeEmptyDir removes a directory the source does not ship. Its members
// were removed before it, so what can still hold it is local state the
// install keeps on purpose — and then the directory stays, holding nothing
// else.
func removeEmptyDir(path, target string) error {
	if err := ownedUnder(path, target); err != nil {
		return err
	}
	// Asked before the removal rather than read out of an errno: "the
	// directory is not empty" is spelled differently on every platform,
	// and what is left in it decides whether this is normal.
	left, err := os.ReadDir(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	for _, e := range left {
		if !isLocalStateName(e.Name()) {
			return fmt.Errorf("remove %s: it still holds %s, which the install neither ships nor keeps", path, e.Name())
		}
	}
	if len(left) > 0 {
		// Only the lock's own home is in there: the directory stays, so the
		// next save of a file that lands here keeps the same lock.
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// removeUnderLock removes one file the source does not ship, under the
// authoring lock of that file: a save in flight stops the install instead
// of losing its bytes.
func removeUnderLock(ctx context.Context, path, target string) error {
	if err := ownedUnder(path, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	held, err := holdBounded(ctx, path, func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	})
	if err != nil && !held {
		return fmt.Errorf("%s: %w: %w", path, ErrInstallTargetLocked, err)
	}
	return err
}

// copyShipped writes every file the source ships into target, each under
// its own authoring lock and through a staged file renamed into place: a
// reader that takes no lock (a run mirroring a bundle's skills) never sees
// half a file, and a copy that fails leaves the one that was there.
func copyShipped(ctx context.Context, source, target string, shipped map[string]bool) error {
	rels := make([]string, 0, len(shipped))
	for rel := range shipped {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	// Sorted, so a directory is created before anything inside it.
	for _, rel := range rels {
		path := filepath.Join(target, filepath.FromSlash(rel))
		if err := ownedUnder(path, target); err != nil {
			return err
		}
		if shipped[rel] {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		src := filepath.Join(source, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		held, err := holdBounded(ctx, path, func() error {
			return stageFileInto(src, path)
		})
		if err != nil && !held {
			return fmt.Errorf("%s: %w: %w", path, ErrInstallTargetLocked, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// stageFileInto copies src beside dst and renames it onto dst, keeping the
// source file's permission bits so an executable shipped in the bundle (a
// tool script under skills/ or attachments/) stays runnable.
func stageFileInto(src, dst string) (err error) {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	staged, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".install-*")
	if err != nil {
		return err
	}
	name := staged.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = io.Copy(staged, in); err != nil {
		_ = staged.Close()
		return err
	}
	if err = staged.Chmod(info.Mode().Perm()); err != nil {
		_ = staged.Close()
		return err
	}
	// Close explicitly and surface its error: on NFS / full disks the
	// buffered write can fail only at Close, so a deferred-and-ignored
	// Close would hide a real write failure.
	if err = staged.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// ownedUnder refuses a path that is not strictly inside the bundle
// directory this install owns. Every path here is built from a walk of that
// directory, so a refusal means something moved under the walk — and a
// removal is not a gesture to make on a guess.
func ownedUnder(path, target string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if absPath == absTarget {
		return nil
	}
	rel, err := filepath.Rel(absTarget, absPath)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to touch %s: it is not inside the bundle directory %s this install owns", absPath, absTarget)
	}
	return nil
}
