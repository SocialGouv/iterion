// Package runtime — the engine-owned copy of a bundle's skills.
package runtime

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
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
// workspace. It answers "" — never a relative path — when workDir is empty or
// itself relative.
//
// An ABSOLUTE answer or none is the whole contract, and the reason is what a
// reader does with the value: `os.path.join(dir, "lang-python.md")` on an
// empty or relative dir yields a path resolved against the reader's cwd, which
// IS the checkout. A value that degrades quietly into the untrusted tree is
// the defect this directory exists to remove, so the degraded value is not
// produced at all and materializeOwnedSkills refuses the run instead.
func OwnedSkillsDir(workDir string) string {
	if workDir == "" || !filepath.IsAbs(workDir) {
		return ""
	}
	return filepath.Join(workDir, ".claude", ownedSkillsDirName)
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

// pruneOwnedSkillsInSharedSandbox empties the engine-owned skills copy inside
// a sandbox this run is ADOPTING from its parent, so the parent's names cannot
// answer for the child.
//
// Only copy-based drivers need it, and only they call it: a bind-mount driver
// shares the host inode the host-side reset already emptied, while a copied
// workspace keeps whatever the parent wrote and the write-through seam adds
// files without ever removing one.
//
// It fails CLOSED. A prune that did not happen leaves a child reading another
// bundle's data blocks and reporting the languages they cover as covered — a
// wrong verdict is worse here than a refused run, and the message says which.
func pruneOwnedSkillsInSharedSandbox(ctx context.Context, run sandbox.Run, containerWorkspace string) error {
	dir := ownedSkillsContainerDir(containerWorkspace)
	if dir == "" {
		return fmt.Errorf("runtime/bundle: the shared sandbox reports workspace %q, which is not an absolute path, so the parent's engine-owned skills copy cannot be located and emptied", containerWorkspace)
	}
	pruneCtx, cancel := context.WithTimeout(ctx, ownedSkillsPruneTimeout)
	defer cancel()
	res, err := run.Exec(pruneCtx, []string{"rm", "-rf", "--", dir}, sandbox.ExecOpts{})
	if err != nil {
		return fmt.Errorf("runtime/bundle: empty the parent's engine-owned skills copy at %s: %w", dir, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("runtime/bundle: empty the parent's engine-owned skills copy at %s: exited %d: %s",
			dir, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// ownedSkillsPruneTimeout bounds the one exec above: a single rm in a live
// container, not a workload.
const ownedSkillsPruneTimeout = 30 * time.Second

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
// workspace that is not an absolute path: ${BUNDLE_SKILLS_DIR} would then
// expand to nothing, every reader would resolve its skill name against its own
// cwd — the checkout — and the run would audit the repository using whatever
// the repository put there. A run that cannot name the directory is refused
// here rather than allowed to read the wrong one.
func materializeOwnedSkills(workDir string, b *bundle.Bundle, logger *iterlog.Logger) error {
	dir := OwnedSkillsDir(workDir)
	if dir == "" {
		return fmt.Errorf("runtime/bundle: the run's workspace %q is not an absolute path, so the engine-owned skills copy has no home and ${BUNDLE_SKILLS_DIR} would expand to nothing", workDir)
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
