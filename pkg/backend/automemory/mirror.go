package automemory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// syncBudget bounds a whole sync-back: a floor that covers the ordinary case,
// plus an allowance per document it may have to write or delete.
//
// The ceiling is what an operator can be made to wait through — pressing
// Cancel does not return until the in-flight node has finished persisting —
// so it stays below the bound this path has ever carried. Past it the store is
// broken rather than slow, and waiting longer buys nothing. On a healthy store
// none of this is reached: the budget is a ceiling, not a duration, and an
// untouched space costs no writes at all.
func syncBudget(docs int) time.Duration {
	const (
		floor   = 30 * time.Second
		perDoc  = 2 * time.Second
		ceiling = 90 * time.Second
	)
	d := floor + time.Duration(docs)*perDoc
	if d > ceiling {
		return ceiling
	}
	return d
}

// maxMirrorFileBytes caps a single file the sync-back will read off disk.
// Mirrors knowledge.DefaultMaxDocumentSize, which the store would reject
// anyway — refusing here keeps a runaway agent from pulling a multi-gigabyte
// file into memory just to learn that.
const maxMirrorFileBytes = 2 << 20

// Mirror materialises one memory space onto the filesystem for a node to use
// as its MEMORY.md directory, then folds what the agent left behind back into
// the store.
//
// The round trip exists because the two mechanisms meet at incompatible
// layers: both backends maintain auto-memory with ORDINARY FILE TOOLS against
// a directory, while durable, tenant-isolated, quota-governed storage is a
// knowledge.MemoryStore. On a cloud runner the pod's disk dies with the run,
// so a directory alone persists nothing.
//
// The manifest captured at Hydrate is what makes SyncBack a diff rather than a
// blind re-upload: an untouched space costs zero writes, and a file the agent
// deleted is deleted in the store rather than silently resurrected on the next
// hydrate.
type Mirror struct {
	store knowledge.MemoryStore
	ref   knowledge.SpaceRef
	dir   string
	// updatedBy attributes every write (e.g. "bot:<id>"), so the studio
	// memory panel can tell an agent's edit from an operator's.
	updatedBy string
	// hydrated maps space-relative path → sha256 of the bytes written to
	// disk. nil until Hydrate succeeds.
	hydrated map[string]string
	// skipped holds documents Hydrate could not materialise. They are
	// reported by SyncBack rather than returned by Hydrate, because they are
	// not grounds for giving the node no memory at all — and because leaving
	// them out of `hydrated` is exactly what keeps the deletion loop off
	// them.
	skipped []error
}

// NewMirror binds a mirror to a space and an on-disk directory. dir is the
// absolute path both the backend and this process see (inside a sandbox it is
// the same absolute path on both sides — see delegate.Task.StateDir).
func NewMirror(store knowledge.MemoryStore, ref knowledge.SpaceRef, dir, updatedBy string) *Mirror {
	return &Mirror{store: store, ref: ref, dir: dir, updatedBy: updatedBy}
}

// Dir is the directory the backend should be pointed at.
func (m *Mirror) Dir() string { return m.dir }

// Hydrate writes every document of the space into the mirror directory and
// records what it wrote. It creates the directory even for an empty space, so
// the backend always has a real path to write into on the agent's first
// memory write.
func (m *Mirror) Hydrate(ctx context.Context) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("auto-memory: create %s: %w", m.dir, err)
	}
	index, err := m.store.BuildIndex(ctx, m.ref)
	if err != nil {
		return fmt.Errorf("auto-memory: index: %w", err)
	}
	index, m.skipped = partitionMaterialisable(index)
	bodies, err := m.readAll(ctx, index)
	if err != nil {
		return err
	}
	m.hydrated = make(map[string]string, len(index))
	for path, content := range bodies {
		abs, err := m.resolve(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return fmt.Errorf("auto-memory: create %s: %w", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, content, 0o600); err != nil {
			return fmt.Errorf("auto-memory: write %s: %w", abs, err)
		}
		m.hydrated[path] = knowledge.ChecksumHex(content)
	}
	m.pruneStale()
	return nil
}

// partitionMaterialisable splits an index into the documents that can be
// written to disk and the ones that cannot, with a reason for each.
//
// The only documents in the second group are rows an OLDER binary stored
// under a path this build now refuses — non-canonical spellings like
// "./MEMORY.md", which knowledge.ValidateDocPath rejects since a store that
// keeps several keys for one document is what let a mirror destroy an
// untouched note. A rolling cloud deploy is exactly where such a row meets
// this code.
//
// They are skipped rather than fatal, and that asymmetry is the point: one
// unusable row must not cost the node its whole memory, and skipping keeps
// the row OUT of `hydrated`, which is what stops SyncBack's deletion loop
// from inferring the agent deleted it. Left alone, it stays readable to
// whoever can address it; deleted here, it would be gone.
func partitionMaterialisable(index []knowledge.IndexEntry) (keep []knowledge.IndexEntry, skipped []error) {
	for _, e := range index {
		if err := knowledge.ValidateDocPath(e.Path); err != nil {
			skipped = append(skipped, fmt.Errorf("skipped %q: %w", e.Path, err))
			continue
		}
		keep = append(keep, e)
	}
	return keep, skipped
}

// readAll fetches the bodies of every indexed document, preferring the
// store's BULK path.
//
// Autoload is that path: on the cloud adapter it is a single query for the
// whole space, where one ReadDocument per document would be one round trip
// each — on a 20-document memory across a 5-node bot, 100 queries instead of
// 5. Passing the index paths as patterns is exact-matching (they carry no
// metacharacters in practice), so the result is the same set.
//
// "In practice" is not "always" — a path containing a glob metacharacter
// would not match itself — so anything Autoload did not return is fetched
// individually rather than silently dropped. A document the index lists but
// neither path can read is a real store fault: surfacing it beats handing the
// agent a truncated memory it would then "correct" by rewriting.
func (m *Mirror) readAll(ctx context.Context, index []knowledge.IndexEntry) (map[string][]byte, error) {
	out := make(map[string][]byte, len(index))
	if len(index) == 0 {
		return out, nil
	}
	paths := make([]string, len(index))
	for i, e := range index {
		paths[i] = e.Path
	}
	// A bulk failure is not fatal on its own — fall through to the per-document
	// path, which reports the real error against a concrete document.
	if entries, err := m.store.Autoload(ctx, m.ref, paths); err == nil {
		for _, e := range entries {
			out[e.Path] = e.Content
		}
	}
	for _, path := range paths {
		if _, ok := out[path]; ok {
			continue
		}
		doc, err := m.store.ReadDocument(ctx, m.ref, path)
		if err != nil {
			return nil, fmt.Errorf("auto-memory: read %q: %w", path, err)
		}
		out[path] = doc.Content
	}
	return out, nil
}

// pruneStale removes mirrored documents the space no longer has.
//
// This enforces the ownership contract NewMirror states — after Hydrate, the
// directory IS the space — for any directory a caller supplies. Whatever the
// space no longer has must not survive on disk: the agent would read it, and
// SyncBack would see an untracked file and upload it straight back, so a
// deletion would never stick and the store would quietly stop being the source
// of truth.
//
// In this tree it finds nothing, and that is by construction rather than luck:
// the only production caller passes a directory NewNodeDir has just created,
// so it is empty. It is kept because NewMirror is exported and takes an
// arbitrary directory — the invariant is the function's, not the caller's.
//
// Only files this mirror manages are touched: regular `.md` files. A symlink
// or anything else is left exactly where it is — pruning is not the place to
// act on a path we already refuse to read through.
func (m *Mirror) pruneStale() {
	paths, _, walked := m.walkPaths(nil)
	if !walked {
		// Same rule as SyncBack: a partial view is not grounds for deleting.
		return
	}
	for _, path := range paths {
		if _, kept := m.hydrated[path]; kept {
			continue
		}
		_ = os.Remove(filepath.Join(m.dir, filepath.FromSlash(path)))
	}
}

// SyncBack folds the mirror directory back into the space: new and modified
// documents are written, documents the agent removed are deleted, untouched
// ones cost nothing.
//
// Errors are ACCUMULATED, not returned on the first failure. A rejected
// document (over quota, secret-shaped, unreadable) must not cost the run the
// other documents the agent wrote — memory is a best-effort side channel, and
// the alternative is losing a whole session's notes to one bad file. The
// returned error is a joined summary the caller surfaces as a warning.
//
// Only Markdown is persisted: the store indexes `.md` and nothing else, so a
// non-markdown file would be written and then be invisible to every reader.
// Those are reported rather than dropped in silence.
func (m *Mirror) SyncBack(ctx context.Context) error {
	if m.hydrated == nil {
		return errors.New("auto-memory: SyncBack before a successful Hydrate")
	}
	// Documents Hydrate could not materialise are reported here, on the one
	// channel the caller already surfaces — the node is over, and a warning
	// the operator never sees is the same as no warning.
	errs := slices.Clone(m.skipped)
	onDisk, seen, walked := m.readMirror(&errs)

	// The deadline is set HERE, not by the caller, because only here is the
	// amount of work known. A fixed budget silently truncates: the caller
	// picks it to keep Cancel responsive — the operator waits through this —
	// and a run with a large memory on a slow store then loses whatever did
	// not fit, with one warning. Scaling by the pending count keeps the common
	// case (a document or two) as short as before while letting a big sync
	// finish.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, syncBudget(len(onDisk)+len(m.hydrated)))
		defer cancel()
	}

	for _, path := range slices.Sorted(maps.Keys(onDisk)) {
		content := onDisk[path]
		if before, ok := m.hydrated[path]; ok && before == knowledge.ChecksumHex(content) {
			continue // untouched
		}
		if reason := knowledge.ScanForSecret(content); reason != "" {
			errs = append(errs, fmt.Errorf("refused %q: it %s — memory is readable by every later run of this bot", path, reason))
			continue
		}
		if _, err := m.store.WriteDocument(ctx, m.ref, knowledge.DocumentInput{
			Path: path, Content: content, UpdatedBy: m.updatedBy,
		}); err != nil {
			errs = append(errs, fmt.Errorf("write %q: %w", path, err))
		}
	}

	// Deletions are inferred from ABSENCE, so they are only safe against what
	// the walk actually SAW — never against what we were able to store.
	//
	// Three failures of that one distinction, each reproduced, each found after
	// fixing the last:
	//   - the mirror directory vanished mid-node (a concurrent sweep, a /tmp
	//     reaper) and read as "the agent deleted everything" — hence `walked`;
	//   - a file the walk listed but could not open (a stray mode bit, a uid
	//     mismatch between host and sandbox) read as "the agent deleted this
	//     one";
	//   - a note the agent grew past the size cap was filtered out before
	//     `seen` was built, so the store's own copy was deleted — an ordinary
	//     end for a long-lived memory.
	//
	// `seen` is therefore every path the walk ENCOUNTERED, not every path we
	// could persist. Whether a file can be stored and whether the agent still
	// has it are different questions.
	if walked {
		for _, path := range slices.Sorted(maps.Keys(m.hydrated)) {
			if _, still := seen[path]; still {
				continue
			}
			if err := m.store.DeleteDocument(ctx, m.ref, path); err != nil {
				errs = append(errs, fmt.Errorf("delete %q: %w", path, err))
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("auto-memory: %w", errors.Join(errs...))
}

// walkPaths lists the space-relative paths of every persistable Markdown file
// under the mirror directory. When errs is non-nil, each skipped entry is
// appended to it with the reason.
//
// Symlinks and other non-regular entries are SKIPPED, never followed: the
// mirror can sit inside the target repository's checkout, so following one
// would copy a host file of the repo's choosing into a store that every later
// run of this bot reads.
//
// Naming is separate from reading because pruning only needs the names, and
// reading every body just to discard it costs up to the whole memory tree per
// node.
//
// The second return value distinguishes "the walk looked and found nothing"
// from "the walk could not look". Callers that infer deletions from absence
// depend on that difference: treating an unreadable directory as an empty one
// deletes the space.
func (m *Mirror) walkPaths(errs *[]error) ([]string, map[string]struct{}, bool) {
	var out []string
	visited := map[string]struct{}{}
	note := func(format string, args ...any) {
		if errs != nil {
			*errs = append(*errs, fmt.Errorf(format, args...))
		}
	}
	err := filepath.WalkDir(m.dir, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			note("skipped %s: not a regular file", abs)
			return nil
		}
		rel, relErr := filepath.Rel(m.dir, abs)
		if relErr != nil {
			note("skipped %s: %w", abs, relErr)
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Visited BEFORE any persistability filter. Whether we can store a
		// file and whether the agent still has it are different questions, and
		// answering the second with the first is what made a note that grew
		// past the size cap delete the store's copy of itself.
		visited[rel] = struct{}{}
		if !strings.EqualFold(filepath.Ext(rel), ".md") {
			note("skipped %q: only Markdown (.md) is persisted", rel)
			return nil
		}
		if err := knowledge.ValidateDocPath(rel); err != nil {
			note("skipped %q: %w", rel, err)
			return nil
		}
		if info, statErr := d.Info(); statErr != nil {
			note("skipped %q: %w", rel, statErr)
			return nil
		} else if info.Size() > maxMirrorFileBytes {
			note("skipped %q: %d bytes exceeds the %d-byte document cap", rel, info.Size(), maxMirrorFileBytes)
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		note("walk %s: %w", m.dir, err)
		return out, visited, false
	}
	return out, visited, true
}

// readMirror reads the body of every persistable file under the mirror
// directory, keyed by space-relative path. Skips and read failures are
// appended to errs — one unreadable file must not cost the agent the rest of
// its notes.
//
// The second return value is every path the walk SAW, whatever became of it —
// unreadable, too large to persist, not Markdown. Callers that infer deletions
// need that set rather than the contents: a file we could not store is still a
// file the agent did not delete.
func (m *Mirror) readMirror(errs *[]error) (map[string][]byte, map[string]struct{}, bool) {
	paths, visited, walked := m.walkPaths(errs)
	out := make(map[string][]byte, len(paths))
	for _, rel := range paths {
		content, err := os.ReadFile(filepath.Join(m.dir, filepath.FromSlash(rel))) // #nosec G304 — verified regular, .md, and inside the mirror dir.
		if err != nil {
			*errs = append(*errs, fmt.Errorf("skipped %q: %w", rel, err))
			continue
		}
		out[rel] = content
	}
	return out, visited, walked
}

// resolve maps a space-relative document path to its absolute mirror path.
//
// ValidateDocPath is the containment check — it is the same clamp the store
// applies on write, and rejecting "..", absolute paths and NUL here means a
// legacy or hand-edited row cannot make Hydrate write outside the mirror. It
// runs on this side too because Hydrate touches the FILESYSTEM before any
// store call would see the path.
func (m *Mirror) resolve(rel string) (string, error) {
	if err := knowledge.ValidateDocPath(rel); err != nil {
		return "", fmt.Errorf("auto-memory: %w", err)
	}
	return filepath.Join(m.dir, filepath.FromSlash(rel)), nil
}

// nodeDirPrefix names the per-node materialisation directories, so a sweep can
// tell them from anything else under the space root.
const nodeDirPrefix = "node-"

// staleNodeDirAge is how old a node directory must be before the sweep takes
// it.
//
// Very generous on purpose, for a reason the obvious value misses: a
// directory's mtime does not move when a file inside it is OVERWRITTEN, only
// when an entry is added or removed. A long node that keeps rewriting the same
// MEMORY.md therefore looks untouched since Hydrate, and a bot with
// `max_duration: 12h` is a real thing in this catalog. Reaping a live node's
// mirror costs that node its notes, so the threshold sits well beyond any
// plausible node rather than close to it.
const staleNodeDirAge = 48 * time.Hour

// nodeLockSuffix names the lock file marking a node directory as in use. It is
// a SIBLING of the directory, not a file inside it: the agent is handed the
// directory and writes freely in it, so a lock living there is a lock the
// agent can overwrite — enough, on the Windows implementation, to make a live
// node look abandoned. Keeping it outside also spares the walk a special case.
const nodeLockSuffix = ".lock"

func nodeLockPath(dir string) string { return dir + nodeLockSuffix }

// NewNodeDir creates a fresh materialisation directory for one node under the
// space root, and returns it with the lock that marks it live.
//
// Each node gets its own directory because a Mirror OWNS its directory:
// Hydrate makes the directory match the space, which means deleting whatever
// else is in it — so two nodes sharing one would delete each other's work.
//
// The lock is what tells the sweep apart from a crash. Age alone cannot: a
// directory's mtime does not move when a file inside it is overwritten, so a
// long node that keeps rewriting MEMORY.md looks untouched since Hydrate. The
// OS drops the lock when the holder exits, which is exactly the signal a
// crashed run leaves behind and a live one does not — and it is why this is a
// lock rather than a recorded pid, which only says a pid once existed.
//
// Release must be called when the node is done; a nil release is never
// returned alongside a nil error.
func NewNodeDir(spaceRoot string) (dir string, release func(), err error) {
	dir, err = os.MkdirTemp(spaceRoot, nodeDirPrefix)
	if err != nil {
		return "", nil, err
	}
	lock, err := store.AcquireFileLock(nodeLockPath(dir), "auto-memory node mirror")
	if err != nil {
		// A brand-new directory cannot be contended; this is a real failure
		// (read-only mount, no inodes). Leaving an unlocked directory behind
		// would make it look crashed to the very next sweep.
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	var once sync.Once
	return dir, func() {
		// Idempotent: releasing twice would race on the lock's file handle,
		// and a closure handed to a defer is exactly the shape that gets
		// called twice by a later refactor.
		once.Do(func() {
			_ = os.RemoveAll(dir)
			_ = lock.Unlock()
			_ = os.Remove(nodeLockPath(dir))
		})
	}, nil
}

// SweepStaleNodeDirs removes node directories a crashed run left behind. The
// defer that normally removes one does not run on SIGKILL or an OOM kill, and
// nothing else reaps them, so they would accumulate for the life of the store.
//
// Liveness comes from the directory's LOCK, not its age. Age was the first
// answer and it is not sound: a directory's mtime does not move when a file
// inside it is overwritten, so a node that spends hours rewriting MEMORY.md
// looks abandoned, and reaping it destroys the notes it is about to sync. The
// age gate stays as a second condition — a crashed run's lock is already gone,
// so waiting costs nothing, and it keeps the sweep away from anything recent
// on a platform where locking degrades.
//
// Best-effort throughout: failures are warned about, never fatal.
//
// Caveat inherited from the lock primitive (see pkg/store/lock.go): on a
// filesystem that does not honour locks across hosts — NFS without client-side
// lock emulation — two machines sharing a state root could each believe the
// other's node is finished. Neither the cloud (per-pod disk) nor a desktop
// store is such a filesystem.
func SweepStaleNodeDirs(spaceRoot string, logger interface{ Warn(string, ...any) }) {
	entries, err := os.ReadDir(spaceRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleNodeDirAge)
	for _, e := range entries {
		// IsDir is false for a symlink here (ReadDir does not follow), so a
		// planted `node-evil -> /somewhere/important` is skipped, not reaped.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), nodeDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(spaceRoot, e.Name())
		lock, err := store.AcquireFileLock(nodeLockPath(path), "auto-memory node mirror")
		if err != nil {
			continue // still held: a live node, however old it looks
		}
		// Remove while still HOLDING the lock, then release. Releasing first
		// lets a concurrent sweep claim the directory and start removing it
		// under this one — harmless (RemoveAll is idempotent) but it produces
		// a confusing "directory not empty" warning about a directory that is
		// being reaped correctly.
		rmErr := os.RemoveAll(path)
		_ = lock.Unlock()
		_ = os.Remove(nodeLockPath(path))
		if rmErr != nil && logger != nil {
			logger.Warn("auto-memory: cannot reap stale mirror %s: %v", path, rmErr)
		}
	}
}
