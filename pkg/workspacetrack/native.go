package workspacetrack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		MaxFileBytes: DefaultMaxFileBytes,
		MaxFiles:     DefaultMaxFiles,
	}
}

func (n *Native) runDir(runID string) string {
	return filepath.Join(n.root, "runs", runID, "workspace")
}

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
	return filepath.Join(n.root, "workspace-objects", hash[:2], hash[2:])
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
			if d.IsDir() {
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
			return fmt.Errorf("workspacetrack: workspace exceeds %d files — refusing to version it", n.MaxFiles)
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
	h := sha256.New()
	// Hash and buffer in one pass so an unchanged file costs one read.
	var buf strings.Builder
	if _, err := io.Copy(io.MultiWriter(h, &buf), f); err != nil {
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
	if err := writeAtomic(dest, []byte(buf.String())); err != nil {
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
			if d.IsDir() {
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
			return fs.SkipDir
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
