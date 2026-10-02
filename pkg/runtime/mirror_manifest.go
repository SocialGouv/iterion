package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// mirrorManifestName is the bookkeeping file the `.claude` mirror maintains
// of what it wrote: <workDir>/.claude/.iterion-managed/mirror-manifest.json.
// It is the answer to the question the porcelain cannot answer (#1571):
// `git status` sees a tracked file under `.claude/` as modified whoever
// changed it — the engine's own run-start mirror or the run's agent — and
// the path-only noise rules (pkg/treenoise) cannot tell the two apart. The
// mirror CAN: it is the writer. So it records each destination with the
// content hash it laid there, and the dirtiness probes (runOutputPaths,
// commitWorkPaths, reclaimEarlyRefusalWorktree) treat as noise, among
// TRACKED paths under `.claude/`, only what the mirror wrote AND left
// unchanged since. An agent's edit to a tracked `.claude/**` file — or to a
// mirrored file after the mirror laid it — is work, and is banked instead
// of being destroyed with the worktree.
//
// The distinction is deliberately one-directional in its failure mode: a
// missing or unreadable manifest proves nothing, so a tracked mirror path
// then reads as WORK. The cost of a lost manifest is a converged run
// wip-banked once (the operator reviews and discards it); the cost of the
// opposite default is the run's deliverable silently destroyed — the bug
// this file exists to close.
//
// Untracked paths under `.claude/` keep the path-only classification: they
// cannot be a repository's own committed file, so the mirror's writes and
// any scratch the agent dropped there stay noise either way.
//
// An edit that lands byte-identical to what the mirror would write is
// indistinguishable from the mirror's own write by construction — content
// is the only oracle both sides share — and reads as noise. That is the
// same "owned by content" doctrine mirrorBundleSkills already applies to
// directory skills.
//
// The manifest lives in the agent's writable workspace, so it is NOT an
// integrity boundary: an agent can forge an entry exactly as it can `rm`
// the file. It is the honest writer's oracle — it exists so the ENGINE's
// own rewrites are not mistaken for the run's work, not to prove anything
// about an adversary.
const mirrorManifestName = "mirror-manifest.json"

// mirrorManifestDeleted is the tombstone a manifest entry carries when the
// mirror itself REMOVED the path (the orphan pruner, the owned-copy reset):
// the deletion is the engine's own act, so a ` D` porcelain record for it
// is noise. A path that comes BACK after its tombstone is somebody else's
// write and reads as work.
const mirrorManifestDeleted = "-"

// mirrorManifest maps an absolute destination path to the hex sha256 the
// mirror wrote there, or mirrorManifestDeleted for a path the mirror
// removed.
type mirrorManifest struct {
	Files map[string]string `json:"files"`
}

// mirrorManifestMu serializes read-modify-write cycles: a child subbot
// mirrors into its parent's workspace, and a mid-run skill attach
// (MirrorSingleSkill) can land while another pass of the same workspace
// records.
var mirrorManifestMu sync.Mutex

// mirrorManifestPath is the manifest's canonical location, beside the
// plugin-hooks sidecar: under `.claude/` it is itself tree noise, and
// `.iterion-managed/` is the directory every consumer already reads as
// iterion's own bookkeeping.
func mirrorManifestPath(workDir string) string {
	return filepath.Join(workDir, treenoise.MirrorPath, bundleMirrorMarkerDir, mirrorManifestName)
}

// mirrorManifestPathForMarker derives the manifest location from a skill
// marker path, whose layout every mirror site shares by construction:
// <workDir>/.claude/<kind>/.iterion-managed/<name>.sha256. Deriving it here
// — rather than threading workDir through the five reconcileSkillFile call
// sites — is what keeps a future mirror writer incapable of forgetting to
// record: the funnel records, every caller is covered.
func mirrorManifestPathForMarker(markerPath string) string {
	claudeDir := filepath.Dir(filepath.Dir(filepath.Dir(markerPath)))
	return filepath.Join(claudeDir, bundleMirrorMarkerDir, mirrorManifestName)
}

// updateMirrorManifest applies fn to the manifest at path and persists it
// when fn reports a change. Best-effort throughout: the mirror's own write
// already landed, and a lost manifest downgrades to the safe direction
// (tracked mirror paths read as work). WriteFileAtomic keeps a concurrent
// reader from ever seeing a torn file.
func updateMirrorManifest(path string, logger *iterlog.Logger, fn func(m *mirrorManifest) (dirty bool)) {
	mirrorManifestMu.Lock()
	defer mirrorManifestMu.Unlock()
	m := &mirrorManifest{Files: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, m) != nil || m.Files == nil {
			m = &mirrorManifest{Files: map[string]string{}}
		}
	}
	if !fn(m) {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		if logger != nil {
			logger.Warn("runtime/mirror: manifest dir %s: %v — tracked mirror files will read as run work", filepath.Dir(path), err)
		}
		return
	}
	if err := store.WriteFileAtomic(path, data, 0o644); err != nil && logger != nil {
		logger.Warn("runtime/mirror: manifest %s: %v — tracked mirror files will read as run work", path, err)
	}
}

// recordMirrorWrite notes that the mirror laid destPath with content
// hashing to hash. destPath is the absolute destination the mirror wrote.
func recordMirrorWrite(manifestPath, destPath, hash string, logger *iterlog.Logger) {
	updateMirrorManifest(manifestPath, logger, func(m *mirrorManifest) bool {
		if m.Files[destPath] == hash {
			return false
		}
		m.Files[destPath] = hash
		return true
	})
}

// tombstoneMirrorWrite notes that the mirror REMOVED destPath (the orphan
// pruner, the owned-copy reset): the deletion is the engine's own act, not
// the run's. Called only where the engine itself deletes — never where a
// missing file is merely observed, which can equally be the agent's
// deletion and must stay work.
func tombstoneMirrorWrite(manifestPath, destPath string, logger *iterlog.Logger) {
	updateMirrorManifest(manifestPath, logger, func(m *mirrorManifest) bool {
		if m.Files[destPath] == mirrorManifestDeleted {
			return false
		}
		m.Files[destPath] = mirrorManifestDeleted
		return true
	})
}

// syncMirrorManifestTree reconciles the manifest with a tree the mirror
// (re)wrote wholesale — a directory skill copyDir landed, the engine-owned
// skills copy reset: every regular file under root is recorded with its
// current hash, and every prior entry under root the tree no longer carries
// is tombstoned (the reset removed it, so its ` D` is the engine's own). A
// missing root is an empty tree: the reset ran, the refill did not.
func syncMirrorManifestTree(manifestPath, root string, logger *iterlog.Logger) {
	onDisk := map[string]string{}
	files, err := treeFiles(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if logger != nil {
			logger.Warn("runtime/mirror: walk %s for the manifest: %v — tracked mirror files under it will read as run work", root, err)
		}
	}
	for _, abs := range files {
		if abs == "" { // an irregular entry (treeFiles) is not something the mirror writes
			continue
		}
		onDisk[abs] = ""
	}
	updateMirrorManifest(manifestPath, logger, func(m *mirrorManifest) bool {
		dirty := false
		prefix := root + string(filepath.Separator)
		for k, v := range m.Files {
			if v != mirrorManifestDeleted && strings.HasPrefix(k, prefix) {
				if _, ok := onDisk[k]; !ok {
					m.Files[k] = mirrorManifestDeleted
					dirty = true
				}
			}
		}
		for abs := range onDisk {
			h, herr := hashFile(abs)
			if herr != nil {
				continue
			}
			if m.Files[abs] != h {
				m.Files[abs] = h
				dirty = true
			}
		}
		return dirty
	})
}

// loadMirrorManifest reads the manifest of dir's mirror. nil when dir is
// empty, the file is absent, or it cannot be parsed — the mirror then
// proves nothing, and the caller takes its conservative branch.
func loadMirrorManifest(dir string) *mirrorManifest {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(mirrorManifestPath(dir))
	if err != nil {
		return nil
	}
	m := &mirrorManifest{}
	if json.Unmarshal(data, m) != nil || m.Files == nil {
		return nil
	}
	return m
}

// mirrorTrackedEditIsWork reports whether a TRACKED porcelain path under
// the `.claude` mirror is the run's work rather than the mirror's own
// rewrite (#1571). The rules, in order:
//
//   - no manifest → work. The mirror proves nothing; the safe direction is
//     to bank, never to drop.
//   - a path the manifest never recorded → work. The mirror did not write
//     it, so the modification is the run's (the ticket's data-loss case: a
//     bot editing the repository's own tracked `.claude/settings.json`).
//   - a tombstoned path → noise while it stays deleted (the mirror removed
//     it), work the moment it exists again (somebody rewrote it).
//   - a recorded path whose current content still hashes to what the mirror
//     laid → noise: the mirror's write, untouched since. Anything else — an
//     agent's edit, a deletion the mirror did not make — is work.
func mirrorTrackedEditIsWork(dir, path string) bool {
	m := loadMirrorManifest(dir)
	if m == nil {
		return true
	}
	abs := filepath.Join(dir, path)
	hash, ok := m.Files[abs]
	if !ok {
		return true
	}
	if hash == mirrorManifestDeleted {
		_, err := os.Lstat(abs)
		return err == nil
	}
	cur, err := hashFile(abs)
	if err != nil {
		return true
	}
	return cur != hash
}
