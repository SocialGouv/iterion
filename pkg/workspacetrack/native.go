package workspacetrack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxFileBytes caps the size of a single captured file. Above it
// the path is recorded in Snapshot.Skipped instead: a multi-hundred-MB
// artefact is not what a rewind is for, and hashing it on every node
// boundary would dominate the run. Skipped is reported, never silent.
const DefaultMaxFileBytes = 32 << 20 // 32 MiB

// DefaultMaxFiles bounds a capture. Beyond it the tracker gives up rather
// than grind through a workspace it was never meant to version (a home
// directory, a mounted dataset). Also reported.
const DefaultMaxFiles = 50_000

// Native is a filesystem, content-addressed Tracker. It stores everything
// under the run's own directory in the iterion store, so a workspace's
// history is GC'd, exported and pruned with the run it belongs to — and
// nothing is ever written into the project being worked on.
type Native struct {
	root string // the iterion store root

	mu    sync.Mutex
	stats map[string]*statCache // runID → cache, memoised across captures

	MaxFileBytes int64
	MaxFiles     int
}

// NewNative builds a tracker rooted at an iterion store directory.
func NewNative(storeRoot string) *Native {
	return &Native{
		root:         storeRoot,
		stats:        map[string]*statCache{},
		MaxFileBytes: resolveMaxFileBytes(),
		MaxFiles:     DefaultMaxFiles,
	}
}

// resolveMaxFileBytes reads ITERION_WORKSPACE_MAX_FILE_MB.
//
// 32 MiB is a sane default for a source tree, and a bad one for a media
// pipeline: measured on a real video project, the delivered final.mp4 of
// each episode is 48-55 MiB — the artefact the whole run exists to
// produce, and the one a rewind most needs to restore. A skipped file is
// reported rather than silently lost, but reporting is not restoring.
func resolveMaxFileBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("ITERION_WORKSPACE_MAX_FILE_MB"))
	if raw == "" {
		return DefaultMaxFileBytes
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb <= 0 {
		return DefaultMaxFileBytes
	}
	return mb << 20
}

func (n *Native) runDir(runID string) string {
	return filepath.Join(n.root, "runs", runID, "workspace")
}

// objectsDir is the store-global content-addressed pool, relative to the
// store root. Named once so objectPath, storeObject's temp-file staging
// and PruneObjects cannot drift apart.
const objectsDir = "workspace-objects"

// objectPath is deliberately NOT keyed by run: content is content, and a
// per-run pool means every run of the same bot on the same repo rewrites
// the entire workspace from scratch (measured: 318 MiB per run on this
// repo, dominated by vendor/). git has one object store per repository,
// not one per checkout, for exactly this reason.
//
// The trade-off is that pruning a run can no longer blind-delete its
// objects — see PruneObjects, which sweeps against the manifests that
// remain.
func (n *Native) objectPath(hash string) string {
	// A manifest is untrusted data — Load is a bare json.Unmarshal over a
	// file in a store that may have been hand-copied, partially restored
	// or written by another version — so the hash gets the same treatment
	// as safeRelPath and validSnapshotID. Unvalidated, `hash[:2]` panicked
	// on an empty or 1-char hash, and a 2-char one resolved to a shard
	// DIRECTORY: the pre-delete os.Stat then succeeded, the irreversible
	// deletion pass ran, and the write-back failed with "is a directory" —
	// exactly the half-destroyed workspace that pre-flight exists to rule
	// out. "" is unopenable, so every caller fails closed.
	if !validHash(hash) {
		return ""
	}
	return filepath.Join(n.root, objectsDir, hash[:2], hash[2:])
}

// validHash accepts only a full lowercase sha256 hex digest.
func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (n *Native) snapshotPath(runID, id string) string {
	return filepath.Join(n.runDir(runID), "snapshots", id+".json")
}

// statCache is the equivalent of git's index: it lets a capture skip
// re-hashing a file whose size and mtime have not moved. Without it every
// node boundary would re-read the whole workspace, which is the single
// thing that would make this too slow to leave on.
type statCache struct {
	Entries map[string]statEntry `json:"entries"`
	Head    string               `json:"head,omitempty"`
	Labels  map[string]string    `json:"labels,omitempty"`
	// racyFrom is the mtime of index.json itself, filled in at load — NOT
	// serialised. It is git's racy-index stamp.
	//
	// (size, mtime) alone is not sufficient to call a file unchanged: a
	// file can be rewritten AFTER a capture read it and still land in the
	// same filesystem timestamp tick, and the change is then permanently
	// invisible. Measured on overlayfs — the shape container and CI
	// runners use, including iterion's own sandbox images — mtime
	// granularity is 4 ms, and five writes in a tight loop share one
	// ModTime. Any entry whose mtime is at or after this stamp was touched
	// during the capture window and must be re-hashed rather than trusted.
	//
	// On the runtime path a snapshot that silently records pre-write
	// content is the one thing this must never do: revertViaTracker banks
	// through Capture before restoring, so a stale bank means the restore
	// destroys bytes the operator was told were recoverable.
	racyFrom int64
	// Overflowed latches once the workspace was found to exceed MaxFiles,
	// so later boundaries short-circuit instead of re-walking a workspace
	// that will fail identically. Persisted, so it survives a resume.
	Overflowed bool `json:"overflowed,omitempty"`
}

type statEntry struct {
	Size    int64 `json:"size"`
	ModNano int64 `json:"mod_nano"`
	Hash    string
}

// MarshalJSON keeps the hash in the serialised form (the struct tag is
// omitted above only to keep the literal readable).
func (s statEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Size    int64  `json:"size"`
		ModNano int64  `json:"mod_nano"`
		Hash    string `json:"hash"`
	}{s.Size, s.ModNano, s.Hash})
}

func (s *statEntry) UnmarshalJSON(b []byte) error {
	var raw struct {
		Size    int64  `json:"size"`
		ModNano int64  `json:"mod_nano"`
		Hash    string `json:"hash"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Size, s.ModNano, s.Hash = raw.Size, raw.ModNano, raw.Hash
	return nil
}

func (n *Native) cache(runID string) *statCache {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.stats[runID]; ok {
		return c
	}
	c := &statCache{Entries: map[string]statEntry{}, Labels: map[string]string{}}
	idx := filepath.Join(n.runDir(runID), "index.json")
	if b, err := os.ReadFile(idx); err == nil {
		_ = json.Unmarshal(b, c)
		// The racy stamp is the index file's OWN mtime, deliberately not a
		// value we compute: it then comes from the same clock and the same
		// granularity as the file mtimes it is compared against, which is
		// what makes the rule self-calibrating. On a coarse filesystem
		// (overlayfs: 4 ms) a file written in the same tick as the index
		// compares equal and is re-read; on ns-granularity ext4 it
		// compares strictly older and keeps its cache hit.
		if info, serr := os.Stat(idx); serr == nil {
			c.racyFrom = info.ModTime().UnixNano()
		}
	}
	if c.Entries == nil {
		c.Entries = map[string]statEntry{}
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	n.stats[runID] = c
	return c
}

func (n *Native) saveCache(runID string, c *statCache) error {
	dir := n.runDir(runID)
	if err := os.MkdirAll(dir, storeDirPerm); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	idx := filepath.Join(dir, "index.json")
	if err := writeAtomic(idx, b); err != nil {
		return err
	}
	// Re-stamp from the file we just wrote, so a long-lived tracker keeps
	// the same self-calibrating comparison as a fresh load.
	if info, serr := os.Stat(idx); serr == nil {
		c.racyFrom = info.ModTime().UnixNano()
	}
	return nil
}

// Capture walks the workspace, stores any content it has not seen, and
// writes a manifest chained onto the run's current head.
func (n *Native) Capture(runID, workspaceDir, label string) (*Snapshot, error) {
	if runID == "" || workspaceDir == "" {
		return nil, fmt.Errorf("workspacetrack: capture needs a run id and a workspace dir")
	}
	c := n.cache(runID)
	if c.Overflowed {
		// Latched by an earlier boundary: re-walking would re-read and
		// re-hash up to MaxFiles files to reach the same verdict.
		return nil, fmt.Errorf("%w (over %d files)", ErrWorkspaceTooLarge, n.MaxFiles)
	}
	ig := NewIgnorer(workspaceDir)
	// The store excludes itself STRUCTURALLY, not by name: an explicit
	// --store-dir is returned verbatim by store.ResolveStoreDir, so a
	// store inside the workspace under any name other than `.iterion` is
	// invisible to the name-based rule — and then the tracker captures its
	// own pool (compounding per boundary) and a restore deletes objects
	// other snapshots still reference.
	ig.ExcludeRoot(workspaceDir, n.root)

	snap := &Snapshot{
		ID:        newSnapshotID(),
		Parent:    c.Head,
		Label:     label,
		CreatedAt: time.Now().UTC(),
	}
	fresh := map[string]statEntry{}
	racyFrom := c.racyFrom

	err := filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// The ROOT failing is a different animal from a sub-directory
			// failing, and must be fatal. WalkDir reports it as a single
			// call with (path == root, d == nil), which the tolerant branch
			// below swallowed — so a workspace that was momentarily
			// unreadable (permissions, an unmounted volume, a removed
			// worktree) produced a snapshot with ZERO entries and no error.
			// Restoring that snapshot then deletes every file absent from
			// it, i.e. the whole workspace; and revertViaTracker banks
			// through this same call, so the "nothing is destroyed" backup
			// would record nothing while still handing the operator a ref
			// they are told to trust. A workspace that could not be read
			// has not been observed to be empty.
			if path == workspaceDir {
				return fmt.Errorf("workspacetrack: read workspace %s: %w", workspaceDir, walkErr)
			}
			// A single unreadable sub-directory stays non-fatal: a run
			// should not die because one path lost its permissions.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(workspaceDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() && ig.CanPruneDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Symlinks are recorded as absent: following them can escape the
		// workspace, and rewriting one on restore would silently replace a
		// link with a regular file.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if len(snap.Entries) >= n.MaxFiles {
			return fmt.Errorf("%w (over %d files)", ErrWorkspaceTooLarge, n.MaxFiles)
		}
		if info.Size() > n.MaxFileBytes {
			snap.Skipped = append(snap.Skipped, rel)
			return nil
		}

		// Stat-cache hit: same size and mtime as last time, reuse the hash
		// and skip the read entirely.
		modNano := info.ModTime().UnixNano()
		// Racy-clean guard (git's racy-index rule): an entry whose mtime
		// sits at or after the previous capture's start may have been
		// rewritten inside that tick, so its recorded hash cannot be
		// trusted — re-read it.
		racy := racyFrom > 0 && prevModAtOrAfter(c.Entries[rel], racyFrom)
		if prev, ok := c.Entries[rel]; ok && !racy && prev.Size == info.Size() && prev.ModNano == modNano && prev.Hash != "" {
			snap.Entries = append(snap.Entries, Entry{Path: rel, Hash: prev.Hash, Mode: uint32(info.Mode().Perm()), Size: info.Size()})
			fresh[rel] = prev
			return nil
		}
		hash, herr := n.storeObject(path)
		if herr != nil {
			// ENOENT (vanished between readdir and open) and EACCES (a
			// path this process may not read) are both "we cannot read
			// THIS file" — neither is a reason to lose the boundary. Any
			// other error (ENOSPC, EIO) still aborts, because it says
			// something is wrong with the store rather than with one path.
			if os.IsNotExist(herr) || os.IsPermission(herr) {
				// The default shape is
				// an IN-PLACE run whose workspace is the operator's live
				// checkout, so this is ordinary: an editor save that
				// replaces a file, or a watcher/dev-server a bot node left
				// running. Failing here aborts the WHOLE capture before
				// writeSnapshot and saveCache, so the boundary gets no
				// snapshot at all AND the next one re-hashes everything —
				// visible to the operator only much later, as a rewind
				// reporting "no snapshot recorded". An unreadable directory
				// and a failed d.Info() above are already tolerated the
				// same way, as git's index is.
				//
				// Recorded in Skipped, NOT merely omitted: Restore keeps a
				// path only when it is in Entries or Skipped, so dropping
				// it silently means the next rewind to this snapshot
				// DELETES it — a file the tracker never held a copy of and
				// the run never created, on the operator's live checkout.
				// Same protection the oversized branch already gets.
				snap.Skipped = append(snap.Skipped, rel)
				return nil
			}
			return fmt.Errorf("capture %s: %w", rel, herr)
		}
		snap.Entries = append(snap.Entries, Entry{Path: rel, Hash: hash, Mode: uint32(info.Mode().Perm()), Size: info.Size()})
		fresh[rel] = statEntry{Size: info.Size(), ModNano: modNano, Hash: hash}
		return nil
	})
	if err != nil {
		// Latch an overflow so the next boundary short-circuits. The abort
		// happens from inside the walk, BEFORE saveCache, so without this
		// the run re-reads and re-hashes up to MaxFiles files at every
		// single node boundary, forever, producing no snapshots at all.
		if errors.Is(err, ErrWorkspaceTooLarge) {
			n.mu.Lock()
			c.Overflowed = true
			n.mu.Unlock()
			if serr := n.saveCache(runID, c); serr != nil {
				return nil, fmt.Errorf("%w (and the overflow latch could not be persisted: %v)", err, serr)
			}
		}
		return nil, err
	}
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	sort.Strings(snap.Skipped)

	// An UNCHANGED workspace writes no new manifest — the label is aliased
	// onto the parent instead.
	//
	// writeSnapshot marshals the complete entry list every time, and
	// Capture runs at every post-node boundary: measured at 1.67 MB per
	// manifest on this repo, so a 100-boundary run left ~167 MB behind.
	// Unlike content, manifests are per-run by construction, so a second
	// run of the same bot on the same tree paid it again. Most nodes of a
	// long loop touch no files at all, which is exactly where this bites —
	// and the resulting snapshot is byte-identical to its parent, so
	// pointing at it loses nothing.
	if parent := n.identicalParent(runID, c.Head, snap); parent != "" {
		n.mu.Lock()
		// Refresh the stat cache even though no manifest is written. The
		// files re-hashed this round (their mtime moved, their content did
		// not — `gofmt -w`, `prettier --write`, codegen) would otherwise
		// keep their OLD stat entries and miss again at every later
		// boundary, so a run in that state pays the cold full-hash cost
		// forever instead of the warm one the default-ON case rests on.
		// `fresh` is already computed; it was simply being dropped.
		c.Entries = fresh
		if label != "" {
			c.Labels[label] = parent
		}
		n.mu.Unlock()
		if err := n.saveCache(runID, c); err != nil {
			return nil, err
		}
		reused, lerr := n.Load(runID, parent)
		if lerr != nil {
			return nil, lerr
		}
		return reused, nil
	}

	if err := n.writeSnapshot(runID, snap); err != nil {
		return nil, err
	}
	n.mu.Lock()
	c.Entries = fresh
	c.Head = snap.ID
	if label != "" {
		c.Labels[label] = snap.ID
	}
	n.mu.Unlock()
	if err := n.saveCache(runID, c); err != nil {
		return nil, err
	}
	return snap, nil
}

// identicalParent returns the parent snapshot id when `snap` records
// exactly the same files, so the caller can alias onto it rather than
// write a duplicate manifest. "" when they differ or the parent cannot be
// read — the safe answer is always to write.
func (n *Native) identicalParent(runID, parentID string, snap *Snapshot) string {
	if parentID == "" {
		return ""
	}
	parent, err := n.Load(runID, parentID)
	if err != nil || len(parent.Entries) != len(snap.Entries) || len(parent.Skipped) != len(snap.Skipped) {
		return ""
	}
	// Both sides are sorted by path, so one pass decides it.
	for i := range snap.Entries {
		a, b := parent.Entries[i], snap.Entries[i]
		if a.Path != b.Path || a.Hash != b.Hash || a.Mode != b.Mode {
			return ""
		}
	}
	for i := range snap.Skipped {
		if parent.Skipped[i] != snap.Skipped[i] {
			return ""
		}
	}
	return parentID
}

// prevModAtOrAfter reports whether a cached entry's recorded mtime sits
// at or after the given stamp — the racy window.
func prevModAtOrAfter(e statEntry, stamp int64) bool {
	return e.ModNano >= stamp
}

// storeObject hashes a file and writes it into the content-addressed
// store when that content is new. Same content from any path, any run
// boundary, is stored once.
func (n *Native) storeObject(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	pool := filepath.Join(n.root, objectsDir)
	if err := os.MkdirAll(pool, storeDirPerm); err != nil {
		return "", err
	}
	// A UNIQUE temp name, and streamed rather than buffered whole.
	//
	// The pool is store-global by design, so two runs capturing the same
	// new content at the same moment would otherwise collide on one
	// "<dest>.tmp": the loser's Rename fails with ENOENT, storeObject
	// errors, and the error propagates out of the WalkDir callback —
	// aborting the ENTIRE boundary capture before saveCache runs. The
	// engine only logs a warn, so the boundary silently has no snapshot
	// and a later rewind reports "no snapshot recorded". The studio runs
	// several runs against one store, which is exactly that shape.
	//
	// Streaming also matters on its own: buffering into a strings.Builder
	// and copying again cost 2x file size on the heap, which the media
	// workspaces this feature targets (mp4s, with
	// ITERION_WORKSPACE_MAX_FILE_MB raised) make material.
	tmp, err := os.CreateTemp(pool, ".obj-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, tmp), f); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	dest := n.objectPath(hash)
	if _, err := os.Stat(dest); err == nil {
		// Already stored — but TOUCH it, because PruneObjects' safety
		// argument ("objects are written before the manifest, so anything
		// younger than the grace window may be alive") only covers objects
		// this capture actually wrote. On a dedup hit the file keeps the
		// mtime of whichever run first stored it, possibly days old and
		// possibly since pruned. A sweep whose mark phase ran before this
		// run's first manifest landed would then find the hash
		// unreferenced AND old enough, and delete content this run is
		// about to name — after which the stat cache keeps reusing the
		// same hash, so every later manifest of the run names dead content
		// too. On a store where a bot runs repeatedly against one repo,
		// deduped content is most of the workspace.
		//
		// Best-effort: failing to touch costs the grace window, not the
		// capture.
		now := time.Now()
		_ = os.Chtimes(dest, now, now)
		return hash, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), storeDirPerm); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, storeFilePerm); err != nil {
		return "", err
	}
	// Rename onto the final name. Content-addressed, so a concurrent
	// writer racing us here lands byte-identical content — last writer
	// wins harmlessly.
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	return hash, nil
}

func (n *Native) writeSnapshot(runID string, snap *Snapshot) error {
	p := n.snapshotPath(runID, snap.ID)
	if err := os.MkdirAll(filepath.Dir(p), storeDirPerm); err != nil {
		return err
	}
	// Compact: a manifest of a real repo is ~2 MB indented and is written
	// at every node boundary.
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(p, b)
}

// Alias points a label at an existing snapshot without re-walking the
// workspace.
func (n *Native) Alias(runID, label, snapshotID string) error {
	if label == "" || snapshotID == "" {
		return nil
	}
	c := n.cache(runID)
	n.mu.Lock()
	c.Labels[label] = snapshotID
	n.mu.Unlock()
	return n.saveCache(runID, c)
}

// Resolve returns the snapshot a label points at.
func (n *Native) Resolve(runID, label string) (string, bool) {
	c := n.cache(runID)
	n.mu.Lock()
	defer n.mu.Unlock()
	id, ok := c.Labels[label]
	return id, ok
}

// OpenObject returns the blob as a seekable reader instead of a byte
// slice, so a caller serving media can stream it and honour Range.
//
// Object buffers the whole file, which is fine for a diff of a source
// file and wrong for the artefact a media pipeline exists to produce: a
// multi-hundred-MB export would sit in the server's heap once per
// concurrent request, and every seek in the player would re-read it.
// Callers reach this through an optional interface assertion, so the
// Tracker contract stays minimal.
func (n *Native) OpenObject(hash string) (*os.File, error) {
	p := n.objectPath(hash)
	if p == "" {
		return nil, fmt.Errorf("%w: object %q", ErrSnapshotNotFound, hash)
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: object %s", ErrSnapshotNotFound, hash)
		}
		return nil, err
	}
	return f, nil
}

// Object reads a content-addressed blob from the shared object pool.
//
// Buffers the whole file — fine for a diff of a source file. A caller
// serving media should reach for OpenObject instead.
func (n *Native) Object(hash string) ([]byte, error) {
	p := n.objectPath(hash)
	if p == "" {
		return nil, fmt.Errorf("%w: object %q", ErrSnapshotNotFound, hash)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: object %s", ErrSnapshotNotFound, hash)
		}
		return nil, err
	}
	return b, nil
}

// Labels returns a copy of the run's label → snapshot-id map. Used by
// the review-scope panel to list gate anchors and attribute files to
// nodes without re-walking the snapshot chain.
func (n *Native) Labels(runID string) map[string]string {
	c := n.cache(runID)
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]string, len(c.Labels))
	for k, v := range c.Labels {
		out[k] = v
	}
	return out
}

// Head returns the run's latest snapshot id.
func (n *Native) Head(runID string) string {
	c := n.cache(runID)
	n.mu.Lock()
	defer n.mu.Unlock()
	return c.Head
}

// Overflowed reports whether this run's workspace was found to exceed
// MaxFiles, which latches versioning off for it.
//
// It exists so a rewind can tell the two "no snapshots" causes apart:
// versioning genuinely off, versus a workspace too large to version.
// They have different fixes, and reporting the second as the first sends
// the operator hunting for the wrong problem. Consumed through an
// optional interface assertion, so the Tracker contract stays minimal.
func (n *Native) Overflowed(runID string) bool {
	c := n.cache(runID)
	n.mu.Lock()
	defer n.mu.Unlock()
	return c.Overflowed
}

// Forget drops a run's in-memory stat cache. Measured at ~2 MiB per run
// id on this repository; without it a studio process accumulates that for
// every run it has ever executed. index.json on disk keeps the cache
// warm, so a later capture for the same run rebuilds it.
func (n *Native) Forget(runID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.stats, runID)
}

// Load reads a snapshot manifest.
func (n *Native) Load(runID, snapshotID string) (*Snapshot, error) {
	// The id reaches here straight from `iterion rewind --restore-snapshot`,
	// and snapshotPath joins it onto the run's snapshots dir — so without
	// this an id like "../../../elsewhere/manifest" reads an arbitrary
	// .json and, if it decodes as a Snapshot, drives a delete+write pass
	// over the workspace. Same containment posture the manifest entries
	// already get through safeRelPath.
	if !validSnapshotID(snapshotID) {
		return nil, fmt.Errorf("%w: %q is not a snapshot id", ErrSnapshotNotFound, snapshotID)
	}
	b, err := os.ReadFile(n.snapshotPath(runID, snapshotID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
		}
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Restore puts the workspace back to a snapshot.
//
// Files whose content already matches are left untouched (so mtimes stay
// put and the stat cache keeps working), differing ones are rewritten,
// and files present in the workspace but absent from the snapshot are
// removed — that last part is what makes "undo what this node created"
// work. Ignored paths are never read, rewritten or deleted: build output
// and dependencies survive a restore exactly as they survive a checkout.
func (n *Native) Restore(runID, workspaceDir, snapshotID string, protected ...string) (*RestoreReport, error) {
	return n.restore(runID, workspaceDir, snapshotID, nil, protected)
}

// RestoreOnly is Restore narrowed to `only` — see the Tracker interface.
//
// Scoping is implemented as a FILTER inside the one restore body rather
// than as a second, leaner write-back, and that is not a style choice.
// Everything Restore refuses to destroy it refuses for a reason it
// learned the hard way: paths the capture skipped, files that have since
// grown past the size cap, directories and symlinks the walk never
// records, manifest entries that escape the workspace, an object pool
// missing a blob the write-back will need. A parallel implementation
// keyed off the manifest would inherit none of them, and would delete
// bytes no bank holds.
func (n *Native) RestoreOnly(runID, workspaceDir, snapshotID string, only []string, protected ...string) (*RestoreReport, error) {
	// Literal, empty included — nil is NOT "everything". Overloading it
	// would mean a caller whose scope computation legitimately came back
	// empty gets the full-tree restore instead of the no-op it asked for,
	// which is precisely the blast radius scoping exists to remove.
	set := make(map[string]bool, len(only))
	for _, p := range only {
		set[p] = true
	}
	return n.restore(runID, workspaceDir, snapshotID, set, protected)
}

// restore is the shared body. `only == nil` means the whole workspace;
// a non-nil set — empty included — restricts every pass to its members.
func (n *Native) restore(runID, workspaceDir, snapshotID string, only map[string]bool, protected []string) (*RestoreReport, error) {
	inScope := func(rel string) bool { return only == nil || only[rel] }
	snap, err := n.Load(runID, snapshotID)
	if err != nil {
		return nil, err
	}
	ig := NewIgnorer(workspaceDir)
	// The store excludes itself STRUCTURALLY, not by name: an explicit
	// --store-dir is returned verbatim by store.ResolveStoreDir, so a
	// store inside the workspace under any name other than `.iterion` is
	// invisible to the name-based rule — and then the tracker captures its
	// own pool (compounding per boundary) and a restore deletes objects
	// other snapshots still reference.
	ig.ExcludeRoot(workspaceDir, n.root)
	ig.Protect(workspaceDir, protected...)
	want := make(map[string]Entry, len(snap.Entries))
	for _, e := range snap.Entries {
		want[e.Path] = e
	}
	// A path the capture SKIPPED is not a path the node created: it was
	// there and we chose not to store it. Deleting it as "absent from the
	// snapshot" would destroy data the tracker never had a copy of, which
	// is strictly worse than leaving a coverage gap.
	preserve := make(map[string]bool, len(snap.Skipped))
	for _, p := range snap.Skipped {
		preserve[p] = true
	}
	// A scoped restore reports the coverage gaps of ITS OWN scope. Seeding
	// the report with the whole snapshot's Skipped list would make every
	// scoped rewind on a repo holding one oversized file print "N path(s)
	// were never captured" about paths it never intended to touch — and
	// that warning is the loudest line the CLI prints. A warning that
	// fires every time is a warning nobody reads.
	skipped := snap.Skipped
	if only != nil {
		skipped = nil
		for _, p := range snap.Skipped {
			if only[p] {
				skipped = append(skipped, p)
			}
		}
	}
	report := &RestoreReport{Skipped: skipped}

	// Remove what the snapshot does not have.
	var toDelete, oversized []string
	err = filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Same rule as Capture's walk, and for a sharper reason here.
			// WalkDir reports a ROOT failure as one call with (path ==
			// root, d == nil); tolerating it leaves toDelete empty and
			// falls straight into the write-back, which MkdirAll's the
			// whole tree back into existence and materialises every entry.
			// So a workspace that is gone — a removed worktree, an
			// unmounted volume whose mountpoint is now an empty dir —
			// gets silently re-created, shadowing the real data when the
			// volume comes back, and the report says Written=N, no error.
			if path == workspaceDir {
				return fmt.Errorf("workspacetrack: read workspace %s: %w", workspaceDir, walkErr)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(workspaceDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() && ig.CanPruneDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if _, keep := want[rel]; keep || preserve[rel] {
			return nil
		}
		// Out of scope: this path is not one the run is recorded to have
		// touched, so removing it would be the restore reaching past its
		// own evidence. Checked BEFORE the oversized branch so an
		// out-of-scope path is not reported as a coverage gap either.
		//
		// Note the candidates still come from the WALK, never from a
		// manifest: that is what keeps every deletion physically inside
		// the workspace and restricted to regular files, which a
		// manifest-driven delete list would have to re-establish.
		if !inScope(rel) {
			return nil
		}
		// A file too large to capture is a file we hold no copy of.
		// Deleting it as "absent from the snapshot" would destroy data
		// that no backup can bring back — the target snapshot's Skipped
		// list only covers paths that existed AT capture time, so a file
		// that grew, or appeared, after it is not in `preserve`.
		if info, ierr := d.Info(); ierr == nil && info.Size() > n.MaxFileBytes {
			oversized = append(oversized, rel)
			return nil
		}
		toDelete = append(toDelete, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Verify every object the write-back will need is present BEFORE
	// deleting anything. The deletion pass is irreversible, and the
	// write-back below bails on the first unreadable object — so an
	// incomplete pool (a store copied without workspace-objects/, a
	// partial disk, a manual cleanup, a prune racing a live capture) left
	// the operator with a workspace holding NEITHER the node's work nor
	// the state it was being reverted to. Failing before the first
	// os.Remove keeps the workspace exactly as it was.
	for _, e := range snap.Entries {
		if !inScope(e.Path) {
			continue // not restored anyway — see the write-back loop
		}
		if !safeRelPath(e.Path) || ig.Match(e.Path, false) {
			continue // not restored anyway — see the write-back loop
		}
		if _, serr := os.Stat(n.objectPath(e.Hash)); serr != nil {
			return nil, fmt.Errorf("restore: object %s for %s is unavailable, refusing to delete anything: %w",
				e.Hash, e.Path, serr)
		}
		// A DIRECTORY where the snapshot holds a file. The node replaced
		// README.md with README.md/, say — a doc split into a folder, a
		// config file turned into a config dir. The deletion walk skips
		// directories, so it survives with its children removed, and the
		// write-back then fails on os.Rename onto an existing directory
		// (EISDIR/ENOTDIR) — after the irreversible deletion pass, which
		// is the half-destroyed workspace this pre-flight exists to
		// prevent. The object check alone did not cover it.
		dest := filepath.Join(workspaceDir, filepath.FromSlash(e.Path))
		if info, derr := os.Lstat(dest); derr == nil && info.IsDir() {
			return nil, fmt.Errorf(
				"restore: %s is a directory in the workspace but a file in the snapshot — "+
					"remove or rename it and retry; nothing was deleted", e.Path)
		}
		// And the MIRROR: a regular file sitting where the snapshot needs a
		// DIRECTORY. When that file is ignored, protected or oversized it
		// survives the deletion pass, and MkdirAll then fails with ENOTDIR
		// — again after the irreversible deletions.
		for parent := filepath.Dir(dest); len(parent) > len(workspaceDir); parent = filepath.Dir(parent) {
			info, derr := os.Lstat(parent)
			if derr != nil {
				break // does not exist yet: MkdirAll will create it
			}
			if !info.IsDir() {
				rel, _ := filepath.Rel(workspaceDir, parent)
				return nil, fmt.Errorf(
					"restore: %s is a file in the workspace but a directory in the snapshot "+
						"(it holds %s) — remove or rename it and retry; nothing was deleted",
					filepath.ToSlash(rel), e.Path)
			}
			break // nearest existing ancestor is a directory: fine
		}
	}
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("restore: remove %s: %w", p, err)
		}
		report.Deleted++
		if rel, rerr := filepath.Rel(workspaceDir, p); rerr == nil {
			report.DeletedPaths = appendCapped(report.DeletedPaths, filepath.ToSlash(rel))
		}
	}
	if len(oversized) > 0 {
		report.Skipped = append(append([]string(nil), report.Skipped...), oversized...)
		sort.Strings(report.Skipped)
	}

	// Write back what differs.
	for _, e := range snap.Entries {
		if !inScope(e.Path) {
			// Silently, and deliberately: an out-of-scope path is not a
			// coverage gap, it is a path this restore was never asked
			// about. Reporting it would bury the real gaps.
			continue
		}
		if !safeRelPath(e.Path) {
			// A manifest entry is data; treat it as untrusted. Capture
			// cannot emit an escaping path today, but Load is a bare
			// unmarshal and filepath.Join Cleans, so "../.." would write
			// outside the workspace. The repo standardised on containment
			// checks after its own audit; this is the same posture.
			report.Skipped = append(report.Skipped, e.Path)
			continue
		}
		// A protected path is not restored either. Skipping it only in
		// the deletion pass would leave the workflow source reverted —
		// the exact failure this exists to prevent, since a rewind is
		// launched to test an edit to that file.
		if ig.Match(e.Path, false) {
			// REPORT it unless it is protected. The Ignorer is built from
			// the ignore files as they stand NOW, not as they stood at
			// capture time — so a node that edited .gitignore/.iterionignore
			// (routine for a scaffold or build-config bot) makes every path
			// it newly excludes skipped by BOTH passes: the node's own
			// production survives the rewind untouched, which is precisely
			// the "replaying a node on top of its own previous production"
			// failure this package exists to prevent. Silently, until now —
			// contradicting the documented "coverage gaps are always
			// reported, never silent". A protected path is a deliberate
			// exclusion the operator asked for, so it stays quiet.
			if !ig.IsProtected(e.Path) {
				report.Skipped = append(report.Skipped, e.Path)
			}
			continue
		}
		dest := filepath.Join(workspaceDir, filepath.FromSlash(e.Path))
		if sameContent(dest, e) {
			// Content matches, but the MODE may not: sameContent compares
			// size and sha256 only, so a node that ran `chmod +x deploy.sh`
			// (or `chmod 600` on a config) without touching the bytes would
			// otherwise leave that mode in place across a rewind — the
			// replayed node meeting its own previous production, which is
			// the failure this feature exists to prevent. Executable-bit
			// flips are a routine product of build and scaffold nodes.
			if mode := fs.FileMode(e.Mode).Perm(); mode != 0 {
				if err := os.Chmod(dest, mode); err != nil {
					return nil, fmt.Errorf("restore %s: chmod: %w", e.Path, err)
				}
			}
			report.Unchanged++
			continue
		}
		blob, rerr := os.ReadFile(n.objectPath(e.Hash))
		if rerr != nil {
			return nil, fmt.Errorf("restore %s: read object %s: %w", e.Path, e.Hash, rerr)
		}
		// 0755, NOT the store's 0700: this directory is created inside the
		// operator's WORKSPACE, and a restore must not silently narrow the
		// permissions of their own checkout. The store-side tightening
		// applies only to what iterion writes under its own root.
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		mode := fs.FileMode(e.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := writeAtomicMode(dest, blob, mode); err != nil {
			return nil, fmt.Errorf("restore %s: %w", e.Path, err)
		}
		report.Written++
		report.WrittenPaths = appendCapped(report.WrittenPaths, e.Path)
	}

	// Prune directories the deletions emptied, so an "undo" of a node that
	// created docs/generated/ leaves no hollow tree behind.
	pruneEmptyDirs(workspaceDir, ig, toDelete)

	// The workspace no longer matches the cache: drop it so the next
	// capture re-stats rather than trusting stale (size, mtime) pairs.
	n.mu.Lock()
	if c, ok := n.stats[runID]; ok {
		c.Entries = map[string]statEntry{}
	}
	n.mu.Unlock()
	return report, nil
}

// safeRelPath rejects anything that could resolve outside the workspace.
//
// filepath.IsLocal covers absolute paths, every ".." component, and — on
// Windows — backslash separators, drive-relative paths and reserved
// names. The hand-rolled "/"-only segment scan this replaces let
// `..\..\evil.txt` through on Windows, where the entry has no "/" at all:
// filepath.FromSlash then leaves the backslashes intact and filepath.Join
// resolves outside the workspace. A manifest is untrusted data and
// iterion ships windows/amd64 builds, so the guard has to hold there too.
func safeRelPath(p string) bool {
	if p == "" {
		return false
	}
	return filepath.IsLocal(filepath.FromSlash(p))
}

func sameContent(path string, e Entry) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != e.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == e.Hash
}

// pruneEmptyDirs removes the directories THIS restore emptied, deepest
// first, walking upward from each deleted file while its parent is empty.
//
// Scoped to `deleted` rather than re-walking the workspace, because
// neither git nor this tracker records empty directories: a blanket sweep
// removed any directory that happened to be empty at that moment, whether
// or not the restore touched it. On an in-place run — the workspace being
// the operator's live checkout — that quietly deleted their scratch dirs,
// mount points and pre-created output dirs. A restore must not destroy
// something it never had a copy of and never created.
func pruneEmptyDirs(root string, ig *Ignorer, deleted []string) {
	seen := map[string]bool{}
	for _, p := range deleted {
		dir := filepath.Dir(p)
		for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
			if seen[dir] {
				break
			}
			seen[dir] = true
			rel, rerr := filepath.Rel(root, dir)
			if rerr != nil {
				break
			}
			if ig.Match(filepath.ToSlash(rel), true) {
				break // never touch an ignored directory
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				break
			}
			if err := os.Remove(dir); err != nil {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
}

// storeDirPerm / storeFilePerm mirror pkg/store's 0700/0600.
//
// Everything this package writes lands in the SAME store root, and its
// content is strictly more sensitive than the artifacts the store already
// protects: the full bytes of every non-ignored file in the operator's
// checkout, plus manifests listing every path. Writing that 0755/0644 let
// every local user on a shared host, dev box or CI runner read the
// workspace of every run.
//
// Files written INTO the workspace by a restore are unaffected — those
// carry the snapshot's recorded Entry.Mode.
const (
	storeDirPerm  fs.FileMode = 0o700
	storeFilePerm fs.FileMode = 0o600
)

func writeAtomic(path string, b []byte) error { return writeAtomicMode(path, b, storeFilePerm) }

// writeAtomicMode stages through a UNIQUE temp name in the destination
// directory.
//
// The fixed "<path>.tmp" it replaces was destructive during a restore:
// restoring `build` truncated a real sibling `build.tmp`. When that file
// is itself in the snapshot the damage is transient (entries are sorted,
// so "build" < "build.tmp" and it is rewritten after), but when it is
// IGNORED or PROTECTED it is never restored and its content is gone —
// contradicting the documented contract that an ignored path is never
// rewritten and a protected one never touched.
func writeAtomicMode(path string, b []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".iterion-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

var snapshotCounter struct {
	sync.Mutex
	n int
}

// newSnapshotID is time-ordered so a run's snapshots sort chronologically
// by name, with a counter so two captures in the same nanosecond cannot
// collide.
func newSnapshotID() string {
	snapshotCounter.Lock()
	snapshotCounter.n++
	c := snapshotCounter.n
	snapshotCounter.Unlock()
	return fmt.Sprintf("%d-%04d", time.Now().UTC().UnixNano(), c)
}

// validSnapshotID pins an id to the form newSnapshotID generates
// (`<unixnano>-<counter>`), which contains no separator and no dot, so it
// cannot traverse out of the run's snapshots directory.
func validSnapshotID(id string) bool {
	dash := strings.IndexByte(id, '-')
	if dash <= 0 || dash == len(id)-1 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if i == dash {
			continue
		}
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// PruneObjects deletes content in the store-global pool that no surviving
// snapshot manifest references, and reports how many objects and bytes it
// reclaimed.
//
// The pool is shared across runs so a second run of the same bot on the
// same tree costs almost nothing — but that is exactly why deleting a run
// can no longer blind-delete its objects, and why `iterion runs prune`
// would otherwise leave them behind forever. This is the sweep that
// closes it: mark every hash still named by a manifest under
// <root>/runs/*/workspace/snapshots/, delete the rest.
//
// minAge is the grace period that makes the sweep safe to run while runs
// are executing — git's `gc --prune=<date>` guard, and it is load-bearing
// here rather than belt-and-braces.
//
// A Capture writes its objects FIRST and its manifest LAST. An object
// written after the mark phase read the snapshots directory is therefore
// unreferenced at mark time, and deleting it does not merely lose that
// object: the stat cache goes on reusing its hash for the rest of the
// run, so every later snapshot of that run names dead content too, and
// the run's rewind capability is gone for good. Only the restore refuses
// loudly, long after the fact.
//
// `runs prune` is documented as a crontab entry (docs/scheduling.md),
// which is exactly when it would overlap a scheduled bot, so "callers
// should run it when the store is idle" was not a contract that could
// hold. Skipping anything younger than minAge covers the whole window
// with room to spare — a capture is seconds — and only defers reclaim to
// the next sweep. Pass 0 to disable (tests).
func (n *Native) PruneObjects(minAge time.Duration) (objects int, bytes int64, err error) {
	cutoff := time.Now().Add(-minAge)
	live := map[string]bool{}
	runsDir := filepath.Join(n.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		snapDir := filepath.Join(runsDir, e.Name(), "workspace", "snapshots")
		snaps, rerr := os.ReadDir(snapDir)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // this run never versioned its workspace
			}
			// Any OTHER error (EACCES on a tightened run dir, ENOTDIR,
			// EIO) must not read as "this run references nothing" — the
			// sweep below would then delete every object only this run
			// names, destroying its whole workspace history. Same
			// fail-closed posture as the unreadable manifest just below:
			// a partial mark set is exactly how live content gets deleted.
			return 0, 0, fmt.Errorf("workspacetrack: prune: read %s: %w", snapDir, rerr)
		}
		for _, s := range snaps {
			if s.IsDir() || !strings.HasSuffix(s.Name(), ".json") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(snapDir, s.Name()))
			if rerr != nil {
				// An unreadable manifest is treated as LIVE by refusing to
				// prune at all: deleting content a snapshot might still
				// name is unrecoverable, and a partial mark set is exactly
				// how that happens.
				return 0, 0, fmt.Errorf("workspacetrack: prune: read %s: %w", s.Name(), rerr)
			}
			var snap Snapshot
			if uerr := json.Unmarshal(b, &snap); uerr != nil {
				return 0, 0, fmt.Errorf("workspacetrack: prune: decode %s: %w", s.Name(), uerr)
			}
			for _, ent := range snap.Entries {
				live[ent.Hash] = true
			}
		}
	}

	pool := filepath.Join(n.root, objectsDir)
	werr := filepath.WalkDir(pool, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		// <pool>/<aa>/<rest> → hash
		rel, rerr := filepath.Rel(pool, path)
		if rerr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 {
			// Not <aa>/<rest> — the only other thing that lands at the pool
			// root is a `.obj-*` staging file from storeObject, orphaned by
			// a killed or crashed process. It is structurally invisible to
			// the mark phase (no manifest names it), so nothing else would
			// ever reclaim it. Sweep it under the same age guard.
			if len(parts) == 1 && strings.HasPrefix(parts[0], ".obj-") {
				if info, ierr := d.Info(); ierr == nil && !info.ModTime().After(cutoff) {
					if os.Remove(path) == nil {
						objects++
						bytes += info.Size()
					}
				}
			}
			return nil
		}
		if live[parts[0]+parts[1]] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			// Cannot tell how old it is — leave it. Reclaim is a
			// best-effort optimisation; deleting live content is not
			// recoverable.
			return nil
		}
		// The grace window: an object written by a capture whose manifest
		// had not landed when the mark phase ran is unreferenced but very
		// much alive.
		if info.ModTime().After(cutoff) {
			return nil
		}
		// Count only what actually went: a reported reclaim the operator
		// cannot find on disk is worse than no report.
		if rmErr := os.Remove(path); rmErr == nil {
			objects++
			bytes += info.Size()
		}
		return nil
	})
	if werr != nil && !os.IsNotExist(werr) {
		return objects, bytes, werr
	}
	return objects, bytes, nil
}
