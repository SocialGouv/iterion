// Package runtime — the engine-owned copy of a bundle's skills.
package runtime

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

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
const ownedSkillsDirName = "iterion-skills"

// OwnedSkillsDir returns the engine-owned bundle-skills directory for a
// workspace, or "" when no workspace is known.
func OwnedSkillsDir(workDir string) string {
	if workDir == "" {
		return ""
	}
	return filepath.Join(workDir, ".claude", ownedSkillsDirName)
}

// ownedSkillsContainerDir returns the same directory as seen from inside a
// sandbox, where the workspace is bound (or copied) at another pathname.
func ownedSkillsContainerDir(containerWorkspace string) string {
	if containerWorkspace == "" {
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
// skills it could not lay down must not report success without them.
func materializeOwnedSkills(workDir string, b *bundle.Bundle, logger *iterlog.Logger) error {
	dir := OwnedSkillsDir(workDir)
	if dir == "" {
		return nil
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
