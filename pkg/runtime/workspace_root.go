package runtime

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrSkillsDestEscapesWorkspace is the typed refusal when a mirror write would
// land outside the run's workspace.
var ErrSkillsDestEscapesWorkspace = errors.New("runtime/bundle: a workspace mirror path does not resolve inside the workspace")

// workspaceRoot is the HANDLE every mirror create, remove, write and copy
// goes through. It is not a check performed once and discarded — that was the
// defect it replaces.
//
// git checks out a symlink with whatever target it was committed with, so a
// fork can ship `.claude`, `.claude/skills`, or any directory one level deeper,
// as a link into the operator's own tree. The untrusted branches of the mirror
// REMOVE and REPLACE what they find, which turns such a link into a delete and
// a rewrite outside the workspace, under file names the public bundle catalog
// already gives the attacker.
//
// The first attempt at this proved the destination once and returned a STRING;
// every later operation re-resolved that string through the OS, so a symlink
// one component deeper escaped past a passing proof. `os.Root` refuses any
// component that leaves the root, and forbids a symlink inside it from
// pointing out — so the guarantee has to BE the object writes go through, not
// a verdict recorded before them.
//
// On a TRUSTED workspace the handle is a passthrough: the operator's own
// arrangement (a symlinked skills dir, say) works today and keeps working, and
// the mirror there never removes.
//
// Bounded residue, named so the claim above stays exact: a handful of READS
// stay on the os package — the content hashes of reconcileSkillFile/hashFile
// and readMarker, the marker→destination mapping of destPathFromMarkerName,
// and the mirror-manifest probes of mirrorMayClaimSettings/loadMirrorManifest.
// They answer "same bytes?" or "which name?", never "may I write?" — the worst
// a planted link buys there is a one-bit equality oracle against content the
// plant already carries, and hashFile doubles as the reader of the bundle's
// OWN sources, which live outside the workspace by construction. Every write,
// remove, copy and the freshness/presence reads that DRIVE a write go through
// the handle.
type workspaceRoot struct {
	dir  string
	root *os.Root // nil on a trusted workspace: operate through the os package

	// refuse marks a handle that stands for a MISSING one — a caller that
	// forgot to carry the pass. Every operation on it fails, typed.
	//
	// This is the rule the series paid for: the zero value of a guarantee
	// object is the REFUSAL, never the passthrough. A nil pass answering
	// "trusted, use the os package" turned one forgotten argument into a
	// full bypass; a handle whose absence fails closed makes that defect
	// unwritable.
	refuse bool
}

// refusingWorkspaceRoot is the zero value of the guarantee: it grants nothing.
func refusingWorkspaceRoot() *workspaceRoot { return &workspaceRoot{refuse: true} }

// guard is the first thing every operation calls.
func (w *workspaceRoot) guard(abs string) error {
	if w == nil || w.refuse {
		return fmt.Errorf("%w: no workspace handle was carried to this write (%s)", ErrSkillsDestEscapesWorkspace, abs)
	}
	return nil
}

// openWorkspaceRoot opens the handle for a workspace.
func openWorkspaceRoot(workDir string, trust store.RunTrust) (*workspaceRoot, error) {
	if workDir == "" {
		// An untrusted run with no workspace has nothing legitimate to write;
		// a trusted one with none is an embedder that writes nowhere either.
		// Refusing both keeps the rule the file states three times: the zero
		// value of a guarantee object is the refusal.
		if !trust.Trusted() {
			return refusingWorkspaceRoot(), nil
		}
		return &workspaceRoot{}, nil
	}
	wr := &workspaceRoot{dir: workDir}
	if trust.Trusted() {
		return wr, nil
	}
	r, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/bundle: open workspace root %s: %w", workDir, err)
	}
	wr.root = r
	return wr, nil
}

// Close releases the handle. Safe on a passthrough.
func (w *workspaceRoot) Close() {
	if w != nil && w.root != nil {
		_ = w.root.Close()
	}
}

// rel converts an absolute path under the workspace into the root-relative one
// the handle accepts.
//
// It does NOT itself judge whether the path escapes: os.Root does that, at
// every component and through every symlink, and a second check here would be
// a guard no test could redden — the kind that looks like it works. A `..`
// prefix simply travels to the handle, which refuses it.
func (w *workspaceRoot) rel(abs string) (string, error) {
	r, err := filepath.Rel(w.dir, abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrSkillsDestEscapesWorkspace, abs)
	}
	return r, nil
}

// wrap turns a handle error into the typed refusal, so a caller can tell an
// escape from an ordinary I/O failure. The cause stays unwrappable: a read
// through the handle distinguishes "absent inside the workspace"
// (os.ErrNotExist) from "not inside it at all", and both answers carry the
// refusal's type.
func (w *workspaceRoot) wrap(abs string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrSkillsDestEscapesWorkspace, abs, err)
}

func (w *workspaceRoot) MkdirAll(abs string, perm os.FileMode) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return os.MkdirAll(abs, perm)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.wrap(abs, w.root.MkdirAll(rel, perm))
}

func (w *workspaceRoot) RemoveAll(abs string) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return os.RemoveAll(abs)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.wrap(abs, w.root.RemoveAll(rel))
}

func (w *workspaceRoot) Remove(abs string) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return os.Remove(abs)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.wrap(abs, w.root.Remove(rel))
}

func (w *workspaceRoot) WriteFile(abs string, data []byte, perm os.FileMode) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return os.WriteFile(abs, data, perm)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.wrap(abs, w.root.WriteFile(rel, data, perm))
}

// WriteFileAtomic replaces abs with data atomically on BOTH branches (temp +
// fsync + rename): an interrupted write never leaves a truncated file under a
// marker claiming iterion wrote it, and a hardlink the destination
// participates in is broken rather than written through. The trusted branch
// is store.WriteFileAtomic; the rooted branch stages through a sibling temp
// with a per-call name, both through the handle.
func (w *workspaceRoot) WriteFileAtomic(abs string, data []byte, perm os.FileMode) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return store.WriteFileAtomic(abs, data, perm)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.atomicRename(rel, abs, perm, func(f *os.File) error {
		_, werr := f.Write(data)
		return werr
	})
}

func (w *workspaceRoot) Chmod(abs string, perm os.FileMode) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return os.Chmod(abs, perm)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	return w.wrap(abs, w.root.Chmod(rel, perm))
}

// Stat answers about a path that must resolve INSIDE the workspace, following
// symlinks the way os.Stat does — except that a link leaving the workspace is
// an error, never an answer about the operator's tree.
func (w *workspaceRoot) Stat(abs string) (os.FileInfo, error) {
	if err := w.guard(abs); err != nil {
		return nil, err
	}
	if w.root == nil {
		return os.Stat(abs)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return nil, err
	}
	fi, serr := w.root.Stat(rel)
	if serr != nil {
		return nil, w.wrap(abs, serr)
	}
	return fi, nil
}

// ReadDir lists a directory inside the workspace. The rooted branch refuses a
// path that escapes, so a planted link is an error rather than a listing of
// the operator's tree.
func (w *workspaceRoot) ReadDir(abs string) ([]os.DirEntry, error) {
	if err := w.guard(abs); err != nil {
		return nil, err
	}
	if w.root == nil {
		return os.ReadDir(abs)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return nil, err
	}
	f, oerr := w.root.Open(rel)
	if oerr != nil {
		return nil, w.wrap(abs, oerr)
	}
	defer func() { _ = f.Close() }()
	entries, rerr := f.ReadDir(-1)
	if rerr != nil {
		return nil, w.wrap(abs, rerr)
	}
	return entries, nil
}

// resolveLinkTarget resolves a link's target the way the kernel does: an
// absolute target as itself, a relative one against the link's own directory.
func resolveLinkTarget(absLink, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(absLink), target))
}

// escapes reports whether an absolute path lies outside the workspace.
func (w *workspaceRoot) escapes(abs string) bool {
	rel, err := filepath.Rel(w.dir, filepath.Clean(abs))
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RefuseEscapingLink refuses when abs is a symlink pointing out of the
// workspace — the shape a checkout ships to make a later READ deliver the
// operator's files, before any write is attempted.
//
// A DANGLING link is not an escape and is not refused: its target resolves
// inside, there is simply nothing there. Judging "does it resolve to something
// readable" instead of "does it leave the workspace" is what made an earlier
// version of this park a trusted run over a broken link of the operator's own.
//
// Nothing on a trusted workspace, by construction: the operator's arrangement
// — a `.claude` linked to a shared directory, say — is theirs to make.
func (w *workspaceRoot) RefuseEscapingLink(abs string) error {
	if err := w.guard(abs); err != nil {
		return err
	}
	if w.root == nil {
		return nil
	}
	rel, err := w.rel(abs)
	if err != nil {
		return err
	}
	fi, lerr := w.root.Lstat(rel)
	if errors.Is(lerr, os.ErrNotExist) {
		return nil
	}
	if lerr != nil {
		// An intermediate component already left the root, or the entry cannot
		// be read at all. Both fail closed: this is the boundary check.
		return w.wrap(abs, lerr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, rerr := w.root.Readlink(rel)
	if rerr != nil {
		return w.wrap(abs, rerr)
	}
	if w.escapes(resolveLinkTarget(abs, target)) {
		return fmt.Errorf("%w: %s -> %s", ErrSkillsDestEscapesWorkspace, abs, target)
	}
	return nil
}

// Symlink creates a link INSIDE the workspace, whose TARGET also stays inside.
//
// os.Root does NOT check the target — its own doc says so: "Symlink does not
// validate oldname, which may reference a location outside the root." The
// handle refuses traversal THROUGH an escaping link, which is a different
// guarantee: a link re-created here would be refused on the next write through
// it, but it would still be a link into the operator's tree, and the caller
// that follows it need not be this one. So the target is checked here.
func (w *workspaceRoot) Symlink(target, absLink string) error {
	if err := w.guard(absLink); err != nil {
		return err
	}
	if w.root == nil {
		return os.Symlink(target, absLink)
	}
	if w.escapes(resolveLinkTarget(absLink, target)) {
		return fmt.Errorf("%w: %s -> %s", ErrSkillsDestEscapesWorkspace, absLink, target)
	}
	relLink, err := w.rel(absLink)
	if err != nil {
		return err
	}
	return w.wrap(absLink, w.root.Symlink(target, relLink))
}

// ReadFile reads a file that must resolve INSIDE the workspace. os.Root's own
// open refuses every escaping component, so a symlinked file cannot deliver a
// host path's bytes to the caller.
func (w *workspaceRoot) ReadFile(abs string) ([]byte, error) {
	if err := w.guard(abs); err != nil {
		return nil, err
	}
	if w.root == nil {
		return os.ReadFile(abs)
	}
	rel, err := w.rel(abs)
	if err != nil {
		return nil, err
	}
	b, rerr := w.root.ReadFile(rel)
	if rerr != nil {
		return nil, w.wrap(abs, rerr)
	}
	return b, nil
}

// IsRealDir reports whether abs is a directory inside the workspace and NOT a
// symlink. Lstat, deliberately: a link to a directory answers true to IsDir()
// and is exactly the shape that turned a recursive prune into a delete of the
// operator's own tree.
func (w *workspaceRoot) IsRealDir(abs string) bool {
	if w == nil || w.refuse {
		return false
	}
	if w.root == nil {
		fi, err := os.Lstat(abs)
		return err == nil && fi.IsDir()
	}
	rel, err := w.rel(abs)
	if err != nil {
		return false
	}
	fi, lerr := w.root.Lstat(rel)
	return lerr == nil && fi.IsDir()
}

// Exists reports whether abs names something inside the workspace, without
// following a final symlink out of it.
func (w *workspaceRoot) Exists(abs string) bool {
	if w == nil || w.refuse {
		return false
	}
	if w.root == nil {
		_, err := os.Stat(abs)
		return err == nil
	}
	rel, err := w.rel(abs)
	if err != nil {
		return false
	}
	_, serr := w.root.Stat(rel)
	return serr == nil
}

// atomicRename stages fill into a sibling temp of rel and renames it over rel,
// both through the handle: the temp inherits the directory's containment, and
// the rename is the one os.Root operation that swaps a name atomically. The
// temp name is RANDOM — os.Root has no CreateTemp, and a fixed name races two
// concurrent mirrors of the same destination (one renames the other's
// half-written temp from under it), the failure store.WriteFileAtomic's
// CreateTemp documents on the trusted side.
func (w *workspaceRoot) atomicRename(rel, abs string, perm os.FileMode, fill func(*os.File) error) error {
	tmp := rel + ".atomic-" + randomTempSuffix()
	f, cerr := w.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if cerr != nil {
		return w.wrap(abs, cerr)
	}
	cerr = fill(f)
	if cerr == nil {
		cerr = f.Sync()
	}
	if cerr == nil {
		cerr = f.Close()
	} else {
		_ = f.Close()
	}
	if cerr != nil {
		_ = w.root.Remove(tmp)
		return fmt.Errorf("runtime/bundle: stage %s: %w", abs, cerr)
	}
	if rerr := w.root.Rename(tmp, rel); rerr != nil {
		_ = w.root.Remove(tmp)
		return w.wrap(abs, rerr)
	}
	return nil
}

// CopyFrom writes src to the workspace destination, through the handle. The
// replace is atomic on both branches (temp + fsync + rename): an interrupted
// copy never leaves a truncated file under a marker claiming iterion wrote
// it, and a hardlink the destination participates in is broken rather than
// written through. The destination's mode is the SOURCE's own — a 0600
// helper is not widened, an executable skill helper stays one.
//
// src is read with the plain os package: it names the bundle's or a plugin's
// OWN files (or a staging temp), which live outside the workspace by
// construction. A source that lies INSIDE an untrusted workspace is what
// CopyWithin is for.
func (w *workspaceRoot) CopyFrom(src, dstAbs string) error {
	if err := w.guard(dstAbs); err != nil {
		return err
	}
	in, err := os.Open(src) // #nosec G304 — the mirror's own sources, outside the workspace.
	if err != nil {
		return fmt.Errorf("runtime/bundle: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("runtime/bundle: stat %s: %w", src, err)
	}
	perm := info.Mode().Perm()
	if w.root == nil {
		data, rerr := io.ReadAll(in)
		if rerr != nil {
			return fmt.Errorf("runtime/bundle: read %s: %w", src, rerr)
		}
		return store.WriteFileAtomic(dstAbs, data, perm)
	}
	rel, rerr := w.rel(dstAbs)
	if rerr != nil {
		return rerr
	}
	if cerr := w.atomicRename(rel, dstAbs, perm, func(f *os.File) error {
		_, cerr := io.Copy(f, in)
		return cerr
	}); cerr != nil {
		return fmt.Errorf("runtime/bundle: copy %s -> %s: %w", src, dstAbs, cerr)
	}
	return nil
}

// CopyWithin copies a file whose SOURCE also lives inside the workspace (the
// skill channel re-carrying an owned skill). Both endpoints go through the
// handle on the rooted branch: a planted link where the mirror's file stood
// is a typed refusal, never the operator's bytes re-carried under a skill
// name. The destination gets the source's mode, atomically, like CopyFrom.
func (w *workspaceRoot) CopyWithin(srcAbs, dstAbs string) error {
	if err := w.guard(dstAbs); err != nil {
		return err
	}
	if w.root == nil {
		return copyFile(srcAbs, dstAbs)
	}
	data, rerr := w.ReadFile(srcAbs)
	if rerr != nil {
		return fmt.Errorf("runtime/bundle: read in-workspace source %s: %w", srcAbs, rerr)
	}
	info, serr := w.Stat(srcAbs)
	if serr != nil {
		return fmt.Errorf("runtime/bundle: stat in-workspace source %s: %w", srcAbs, serr)
	}
	rel, rerr := w.rel(dstAbs)
	if rerr != nil {
		return rerr
	}
	return w.atomicRename(rel, dstAbs, info.Mode().Perm(), func(f *os.File) error {
		_, werr := f.Write(data)
		return werr
	})
}

// CopyTreeFrom mirrors a source directory (outside the workspace — the
// bundle's skills dir, a plugin's) into the workspace, every destination
// component through the handle.
func (w *workspaceRoot) CopyTreeFrom(srcDir, dstAbs string) error {
	if err := w.guard(dstAbs); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir) // #nosec G304 — the mirror's own source tree, outside the workspace.
	if err != nil {
		return fmt.Errorf("runtime/bundle: read %s: %w", srcDir, err)
	}
	if err := w.MkdirAll(dstAbs, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dstAbs, e.Name())
		if e.IsDir() {
			if err := w.CopyTreeFrom(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := w.CopyFrom(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// CopyTreeWithin mirrors a tree whose source and destination BOTH live inside
// the workspace (the skill channel). On the rooted branch the reads go through
// the handle like the writes: a link the checkout planted where the mirror's
// own file stood refuses instead of carrying the operator's tree under a skill
// name. A non-regular source entry (a link staying inside, a socket) is
// skipped, never followed: the mirror writes plain files, so anything else is
// not the mirror's.
func (w *workspaceRoot) CopyTreeWithin(srcAbs, dstAbs string) error {
	if err := w.guard(dstAbs); err != nil {
		return err
	}
	if w.root == nil {
		return copyDir(srcAbs, dstAbs)
	}
	entries, err := w.ReadDir(srcAbs)
	if err != nil {
		return fmt.Errorf("runtime/bundle: read in-workspace source %s: %w", srcAbs, err)
	}
	if err := w.MkdirAll(dstAbs, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(srcAbs, e.Name())
		dst := filepath.Join(dstAbs, e.Name())
		if e.IsDir() {
			if err := w.CopyTreeWithin(src, dst); err != nil {
				return err
			}
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		if err := w.CopyWithin(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// randomTempSuffix gives the rooted atomic staging file a per-call name —
// 8 bytes of crypto/rand, hex: collisions between concurrent mirrors of one
// destination stop being a shape, not a race to win.
func randomTempSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a dead process anyway; the timestamp keeps the
		// temp name unique enough to fail the rename loudly rather than share it.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
