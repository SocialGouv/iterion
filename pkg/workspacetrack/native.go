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
	return filepath.Join(n.root, objectsDir, hash[:2], hash[2:])
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
	if b, err := os.ReadFile(filepath.Join(n.runDir(runID), "index.json")); err == nil {
		_ = json.Unmarshal(b, c)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "index.json"), b)
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

	snap := &Snapshot{
		ID:        newSnapshotID(),
		Parent:    c.Head,
		Label:     label,
		CreatedAt: time.Now().UTC(),
	}
	fresh := map[string]statEntry{}

	err := filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable directory is skipped, not fatal: a run should
			// not die because one path lost its permissions.
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
		if prev, ok := c.Entries[rel]; ok && prev.Size == info.Size() && prev.ModNano == modNano && prev.Hash != "" {
			snap.Entries = append(snap.Entries, Entry{Path: rel, Hash: prev.Hash, Mode: uint32(info.Mode().Perm()), Size: info.Size()})
			fresh[rel] = prev
			return nil
		}
		hash, herr := n.storeObject(path)
		if herr != nil {
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
	if err := os.MkdirAll(pool, 0o755); err != nil {
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
		return hash, nil // already stored
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
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

// Labels returns a copy of the run's label map.
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
	if len(hash) < 3 {
		return nil, fmt.Errorf("%w: object %q", ErrSnapshotNotFound, hash)
	}
	f, err := os.Open(n.objectPath(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: object %s", ErrSnapshotNotFound, hash)
		}
		return nil, err
	}
	return f, nil
}

// Object reads a content-addressed blob from the shared object pool.
func (n *Native) Object(hash string) ([]byte, error) {
	if len(hash) < 3 {
		return nil, fmt.Errorf("%w: object %q", ErrSnapshotNotFound, hash)
	}
	b, err := os.ReadFile(n.objectPath(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: object %s", ErrSnapshotNotFound, hash)
		}
		return nil, err
	}
	return b, nil
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
	snap, err := n.Load(runID, snapshotID)
	if err != nil {
		return nil, err
	}
	ig := NewIgnorer(workspaceDir)
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
	report := &RestoreReport{Skipped: snap.Skipped}

	// Remove what the snapshot does not have.
	var toDelete, oversized []string
	err = filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
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
		if !safeRelPath(e.Path) || ig.Match(e.Path, false) {
			continue // not restored anyway — see the write-back loop
		}
		if _, serr := os.Stat(n.objectPath(e.Hash)); serr != nil {
			return nil, fmt.Errorf("restore: object %s for %s is unavailable, refusing to delete anything: %w",
				e.Hash, e.Path, serr)
		}
	}
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("restore: remove %s: %w", p, err)
		}
		report.Deleted++
	}
	if len(oversized) > 0 {
		report.Skipped = append(append([]string(nil), report.Skipped...), oversized...)
		sort.Strings(report.Skipped)
	}

	// Write back what differs.
	for _, e := range snap.Entries {
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
			continue
		}
		dest := filepath.Join(workspaceDir, filepath.FromSlash(e.Path))
		if sameContent(dest, e) {
			report.Unchanged++
			continue
		}
		blob, rerr := os.ReadFile(n.objectPath(e.Hash))
		if rerr != nil {
			return nil, fmt.Errorf("restore %s: read object %s: %w", e.Path, e.Hash, rerr)
		}
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
	}

	// Prune directories the deletions emptied, so an "undo" of a node that
	// created docs/generated/ leaves no hollow tree behind.
	pruneEmptyDirs(workspaceDir, ig)

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

// pruneEmptyDirs removes directories left empty by a restore, deepest
// first. Ignored directories are never touched.
func pruneEmptyDirs(root string, ig *Ignorer) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if ig.Match(filepath.ToSlash(rel), true) {
			if ig.CanPruneDir(filepath.ToSlash(rel)) {
				return fs.SkipDir
			}
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(d)
		}
	}
}

func writeAtomic(path string, b []byte) error { return writeAtomicMode(path, b, 0o644) }

func writeAtomicMode(path string, b []byte, mode fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
// Safe to run while nothing is capturing. A concurrent capture could
// write an object between the mark and the sweep, so callers should run
// this when the store is idle — the same contract `runs prune` already
// has.
func (n *Native) PruneObjects() (objects int, bytes int64, err error) {
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
			continue
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
			return nil
		}
		if live[parts[0]+parts[1]] {
			return nil
		}
		info, ierr := d.Info()
		if ierr == nil {
			bytes += info.Size()
		}
		if rmErr := os.Remove(path); rmErr == nil {
			objects++
		}
		return nil
	})
	if werr != nil && !os.IsNotExist(werr) {
		return objects, bytes, werr
	}
	return objects, bytes, nil
}
