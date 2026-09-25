// Package runtime — the engine-owned copy of a bundle's skills.
package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ownedSkillsDirName is the directory under <workDir>/.claude/ holding the
// engine's own copy of the bundle's skills — the source a tool node reads
// when it parses a machine-readable `iterion:` block out of a skill.
//
// It is NOT <workDir>/.claude/skills/: that directory carries the
// workspace-wins collision policy (mirrorBundleSkills), which exists so an
// operator's own customisation of a skill survives the mirror. The workspace
// is a checkout of an untrusted repository, so a file found there may be the
// bundle's or the repository's and nothing read back can tell the two apart.
// Here the engine is the only writer: the directory is removed and rewritten
// from the bundle on every mirror pass, so a name the bundle does not ship
// does not exist and a name it does ship carries the bundle's bytes.
//
// It sits under `.claude/` rather than beside it because `.claude/` is the
// canonical tree noise (pkg/treenoise): every staging gesture, cleanliness
// probe and `.git/info/exclude` seed already keeps it out of the run's diff.
// A sibling at the workspace root would need each of those taught a second
// path.
//
// It sits INSIDE the workspace rather than out of the git tree
// (${PROJECT_SCRATCH_DIR}) because the kubernetes driver reports
// SupportsHostBindMounts=false: no host path reaches the pod, applyScratchMount
// skips, and ${PROJECT_SCRATCH_DIR} resolves to a container-local directory
// nothing populates. The workspace is the one tree that travels — the mirror
// runs before startSandbox on all three entry paths (Run, resumeRebuildState,
// resumeFromFailure), so the pod's tar copy carries this directory already
// written.
//
// Named residue: living in the workspace means a node that runs a shell
// EARLIER in the same run can still rewrite it. What this removes is the
// repository's own ability to supply the content — the path that needs no
// node to misbehave and that travels in git. Closing the mid-run rewrite as
// well takes one of two things, neither free: re-materialising the directory
// before every tool node (on a copy-based driver that is one write-through
// exec per skill file per node), or a read-only projection the kubernetes
// driver cannot give (SupportsHostBindMounts=false).
const ownedSkillsDirName = "iterion-skills"

// OwnedSkillsDir returns the engine-owned bundle-skills directory for a
// workspace: an ABSOLUTE path, or "".
//
// That is the whole contract, and the reason is what a reader does with the
// value: `os.path.join(dir, "lang-python.md")` on an empty or relative dir
// yields a path resolved against the reader's own working directory, which IS
// the checkout. A value that degrades quietly into the untrusted tree is the
// defect this directory exists to remove.
//
// A relative workDir is RESOLVED rather than refused — every caller that hands
// the engine one (a dispatcher spec, a dry run, a subbot request, an external
// embedder of WithWorkDir) means it against the process's own directory, which
// is what filepath.Abs reads. Refusing it would turn a working run into a hard
// stop for no gain: the contract is that the answer is absolute, not that the
// caller spelled it that way.
func OwnedSkillsDir(workDir string) string {
	if workDir == "" {
		return ""
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	return filepath.Join(abs, ".claude", ownedSkillsDirName)
}

// refuseAnOwnedCopyOutsideTheWorkspace refuses a reset that would leave the
// run's own tree.
//
// The directory is removed recursively on every mirror pass, and `.claude` is a
// path the CHECKOUT supplies: committed as a symlink to somewhere else, it aims
// that removal at a directory the engine never created, at a pathname the
// repository under audit chose. Measured before this guard existed: a `.claude`
// linked out of the workspace had `<target>/iterion-skills` removed with the
// mirror reporting success.
//
// So the rule every destructive step here follows — resolve the symlinks,
// require the target STRICTLY under the tree we own, then act. A workspace
// REACHED through a symlink is fine: both sides resolve to the same tree. What
// is refused is a `.claude` resolving out of it, and the run stops rather than
// reading an owned copy it may not reset.
//
// "Strictly" includes the workspace root itself, which is not a hair split:
// `.claude` linked to the root puts the owned copy at `<workDir>/iterion-skills`.
// Measured — the reset removed a file the REPOSITORY had committed there, and
// the bundle's copy landed outside `.claude/`, the one prefix every staging
// gesture and cleanliness probe excludes (pkg/treenoise).
func refuseAnOwnedCopyOutsideTheWorkspace(dir string) error {
	claudeDir := filepath.Dir(dir)
	resolved, err := filepath.EvalSymlinks(claudeDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing there yet; the mirror creates a real directory.
			return nil
		}
		return fmt.Errorf("runtime/bundle: resolve %s before resetting the engine-owned skills copy: %w", claudeDir, err)
	}
	workDir := filepath.Dir(claudeDir)
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("runtime/bundle: resolve the run workspace %s before resetting the engine-owned skills copy: %w", workDir, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("runtime/bundle: %s resolves to %s, which is not strictly under the run workspace %s — the engine-owned skills copy is removed and rewritten on every pass, and the engine neither removes a directory its workspace only points at nor writes its own copy outside `.claude/` (a checkout can commit that link)", claudeDir, resolved, root)
	}
	return nil
}

// ownedSkillsContainerDir returns the same directory as seen from inside a
// sandbox, where the workspace is bound (or copied) at another pathname. Same
// contract as OwnedSkillsDir: absolute, or nothing.
func ownedSkillsContainerDir(containerWorkspace string) string {
	if containerWorkspace == "" || !path.IsAbs(containerWorkspace) {
		return ""
	}
	return path.Join(containerWorkspace, ".claude", ownedSkillsDirName)
}

// materializeOwnedSkills resets <workDir>/.claude/iterion-skills/ and refills
// it from the bundle's skills directory.
//
// The reset is unconditional — it runs before the bundle is even consulted —
// because a run with no bundle must not leave a directory of that name
// readable: the checkout can commit one, and a tool node reading it would take
// the repository's bytes for the engine's. Removing it is what makes "this
// name is not shipped" observable to the bots as a missing file.
//
// I/O failure is fatal, as it is for the mirror: a run whose bot declares
// skills it could not lay down must not report success without them. So is a
// named workspace this process cannot resolve to an absolute path:
// ${BUNDLE_SKILLS_DIR} would expand to nothing, every reader would resolve its
// skill name against its own working directory — the checkout — and the run
// would audit the repository using whatever the repository put there.
//
// An EMPTY workDir keeps its historical no-op: there is no workspace, so there
// is no directory of that name to reset and nothing to copy into. The
// expansion is empty there too, and what refuses it is the reader's own guard,
// by name.
//
// A child running in place resets its PARENT's copy here. The directory is
// one of childResourcePaths, so the child's scope saves it first and restores
// it on every exit; in an adopted copy-based sandbox, where this host-side
// reset cannot reach, the same list drives the reset in the copy, the refill
// and the restore.
func materializeOwnedSkills(workDir string, b *bundle.Bundle, logger *iterlog.Logger) error {
	if workDir == "" {
		return nil
	}
	dir := OwnedSkillsDir(workDir)
	if dir == "" {
		return fmt.Errorf("runtime/bundle: the run's workspace %q cannot be resolved to an absolute path, so the engine-owned skills copy has no home and ${BUNDLE_SKILLS_DIR} would expand to nothing", workDir)
	}
	if err := refuseAnOwnedCopyOutsideTheWorkspace(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("runtime/bundle: reset owned skills dir %s: %w", dir, err)
	}
	if b == nil || b.SkillsDir == "" {
		return nil
	}
	if _, err := os.Stat(b.SkillsDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("runtime/bundle: stat bundle skills %s: %w", b.SkillsDir, err)
	}
	if err := copyDir(b.SkillsDir, dir); err != nil {
		return fmt.Errorf("runtime/bundle: copy bundle skills into %s: %w", dir, err)
	}
	if logger != nil {
		entries, err := os.ReadDir(dir)
		if err == nil {
			logger.Info("runtime: %d bundle skill entries written to the engine-owned copy at %s", len(entries), dir)
		}
	}
	return nil
}
