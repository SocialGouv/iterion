package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mirrorPass is the per-pass tier bookkeeping the skill mirrors share, and the
// carrier of the two facts every write under the workspace needs: who wrote
// the workspace (trust) and the handle writes go through (wr). They live on
// one object rather than travelling as two more parameters because they are
// one fact about one pass: what this workspace is, and therefore what may be
// done to it.
//
// The tier arbitrates a collision between two of iterion's OWN copies (bundle
// > plugin > library) and is meaningful only within one pass — which is
// exactly why the untrusted arbitration does not read it back from a file:
// every on-disk spelling of it lives inside the workspace, where a checkout
// could write one. The in-memory map is what THIS pass wrote; the on-disk
// `.tier` sidecar survives as the pruner's "touched this pass" bookkeeping and
// the trusted path's historical read, and decides nothing on the untrusted
// floor.
type mirrorPass struct {
	mu    sync.Mutex
	tiers map[string]skillTier

	trust store.RunTrust
	wr    *workspaceRoot
}

// newMirrorPass opens the pass for a workspace. The caller closes it.
func newMirrorPass(workDir string, trust store.RunTrust) (*mirrorPass, error) {
	wr, err := openWorkspaceRoot(workDir, trust)
	if err != nil {
		return nil, err
	}
	return &mirrorPass{tiers: map[string]skillTier{}, trust: trust, wr: wr}, nil
}

// Close releases the pass's workspace handle.
func (p *mirrorPass) Close() {
	if p != nil {
		p.wr.Close()
	}
}

// Trusted reports whether this pass's workspace was written by the tenant.
//
// A NIL pass answers FALSE. It answering true — "a caller that tracks no pass
// is an old, in-tree one" — was the defect class the fork-lane review paid
// for: one forgotten argument on one caller became a full bypass of every
// floor below. The zero value of a guarantee is the refusal.
func (p *mirrorPass) Trusted() bool { return p != nil && p.trust.Trusted() }

// root is the handle writes go through. A nil pass yields a REFUSING handle,
// not the os package: see Trusted.
func (p *mirrorPass) root() *workspaceRoot {
	if p == nil {
		return refusingWorkspaceRoot()
	}
	return p.wr
}

// record notes which tier wrote destPath during this pass, keeping the
// highest-ranked one.
func (p *mirrorPass) record(destPath string, tier skillTier) {
	if p == nil || tier == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tiers == nil {
		p.tiers = map[string]skillTier{}
	}
	if tierRank(tier) > tierRank(p.tiers[destPath]) {
		p.tiers[destPath] = tier
	}
}

// tierFor reports the highest tier that wrote destPath during this pass.
func (p *mirrorPass) tierFor(destPath string) skillTier {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tiers[destPath]
}

// tierOwning reports the highest tier that wrote destPath — or the skill
// DIRECTORY containing it — during this pass.
//
// A tree skill is recorded once, at its directory: the files inside it are
// not keyed individually, so a file-level lookup answers "" for every one of
// them. The ancestors are walked to the workspace root, which bounds the walk
// and keeps the answer inside the tree this pass owns.
func (p *mirrorPass) tierOwning(destPath string) skillTier {
	if p == nil {
		return ""
	}
	stop := ""
	if p.wr != nil {
		stop = p.wr.dir
	}
	for cur := destPath; cur != "" && cur != stop; cur = filepath.Dir(cur) {
		if t := p.tierFor(cur); t != "" {
			return t
		}
		if parent := filepath.Dir(cur); parent == cur {
			break
		}
	}
	return ""
}

// skillsDest resolves <workDir>/.claude/skills and hands back the path every
// write under it goes through the pass's handle to reach. See workspaceRoot:
// the guarantee is the object, not a verdict recorded before the writes.
func skillsDest(workDir string, pass *mirrorPass) (string, error) {
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := pass.root().MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

// prepareSkillDir readies a skill's own directory for a mirror write, and on
// an untrusted workspace REPLACES it wholesale first.
//
// All three tiers go through here — bundle, plugin and library. Reconciling a
// skill file by file leaves whatever else the checkout planted beside
// SKILL.md, and a skill's own convention points at relative
// `references/*.md`, so a planted sibling is read as part of the bot's skill.
//
// Once per directory per pass: a second tier writing the same name must not
// delete what the first just wrote. Every operation goes through the pass's
// workspace handle, so none of it can land outside the workspace.
func prepareSkillDir(pass *mirrorPass, skillDir string, tier skillTier) error {
	if !pass.Trusted() && pass.tierFor(skillDir) == "" {
		if rmErr := pass.root().RemoveAll(skillDir); rmErr != nil {
			return fmt.Errorf("runtime/bundle: replace untrusted workspace skill dir %s: %w", skillDir, rmErr)
		}
		// And the flat alias beside it: a tier that writes only the directory
		// form would otherwise leave `<name>.md` exactly where an explicit
		// Read by path resolves it.
		if rmErr := pass.root().RemoveAll(skillDir + ".md"); rmErr != nil {
			return fmt.Errorf("runtime/bundle: replace untrusted workspace skill alias %s.md: %w", skillDir, rmErr)
		}
		pass.record(skillDir, tier)
	}
	if err := pass.root().MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir %s: %w", skillDir, err)
	}
	return nil
}

// overwriteWithSource replaces the destination with the bundle's/library's own
// copy. "Workspace wins on collision" exists for an operator customising a
// checkout they wrote; on a checkout they did not it is exactly inverted — the
// shadowed file is served to the agent UNDER THE BOT'S OWN SKILL NAME, and a
// fork can keep the legitimate `description:` frontmatter so the catalog line
// never changes. Only the body is swapped.
func overwriteWithSource(pass *mirrorPass, srcPath, destPath, markerPath, srcHash string, tier skillTier, destInfo os.FileInfo, logger *iterlog.Logger) (skillReconcileOutcome, error) {
	wr := pass.root()
	if destInfo != nil {
		_ = wr.Chmod(destPath, destInfo.Mode().Perm())
	}
	if err := wr.CopyFrom(srcPath, destPath); err != nil {
		return skillOutcomeShadowed, err
	}
	markIterionWrote(wr, markerPath, destPath)
	if err := writeMarker(wr, markerPath, srcHash, tier); err != nil {
		return skillOutcomeShadowed, err
	}
	recordMirrorWrite(wr, mirrorManifestPathForMarker(markerPath), destPath, srcHash, logger)
	if logger != nil {
		logger.Warn("skill %q: the workspace's own copy at %s was REPLACED by the %s-tier one — a checkout the tenant did not write does not supply the instructions this agent is told to load", filepath.Base(srcPath), destPath, tier)
	}
	return skillOutcomeRefreshed, nil
}
