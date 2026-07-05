package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// mirrorLibrarySkills resolves the union of skill-library references across the
// workflow (the workflow-level `skills:` default plus every agent/judge node's
// `skills:` list) against the local skill library, mirrors each resolved skill
// into <workDir>/.claude/skills/ using the shared 4-branch collision policy
// (reconcileSkillFile), and returns a name→description map for the resolved
// skills so the executor can render the "## Skills" prompt hint.
//
// It runs at run start (and on resume) AFTER the bundle and plugin skill
// mirrors, so a library skill shadow-defers to a same-named bundle/plugin or
// hand-authored workspace file (precedence: bundle > plugin > library >
// hand-authored — ADR-059). A referenced skill absent from the library is
// logged (Warn) and skipped, never failing the run — the DSL reference is soft.
//
// The library layers a machine-global store (~/.iterion/skills) with an
// optional per-project override (<projectStoreDir>/skills). No-op (nil map)
// when the workflow references no skills.
func mirrorLibrarySkills(workDir, projectStoreDir string, wf *ir.Workflow, logger *iterlog.Logger) (map[string]string, error) {
	if workDir == "" || wf == nil {
		return nil, nil
	}
	refs := collectSkillRefs(wf)
	if len(refs) == 0 {
		return nil, nil
	}
	store := skilllib.LocalStoreForProject(projectStoreDir)

	dest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)

	hints := make(map[string]string)
	dirsReady := false
	for _, name := range refs {
		srcPath, ok := store.Resolve(name)
		if !ok {
			if logger != nil {
				logger.Warn("skill %q referenced by the workflow is not in the skill library (~/.iterion/skills or <project>/.iterion/skills) — not mirrored", name)
			}
			continue
		}
		if !dirsReady {
			for _, d := range []string{dest, markerDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return nil, fmt.Errorf("runtime/library: mkdir %s: %w", d, err)
				}
			}
			dirsReady = true
		}
		// Mirror as <dest>/<name>/SKILL.md (directory form) or <dest>/<name>.md
		// (flat form), matching how the skill is stored on disk so both
		// claude_code's native lookup and the claw skill tool discover it.
		var destPath, markerPath string
		if filepath.Base(srcPath) == "SKILL.md" {
			skillDir := filepath.Join(dest, name)
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				return nil, fmt.Errorf("runtime/library: mkdir %s: %w", skillDir, err)
			}
			destPath = filepath.Join(skillDir, "SKILL.md")
			markerPath = filepath.Join(markerDir, name+".SKILL.md.sha256")
		} else {
			destPath = filepath.Join(dest, name+".md")
			markerPath = filepath.Join(markerDir, name+".md.sha256")
		}
		if _, err := reconcileSkillFile(srcPath, destPath, markerPath, logger); err != nil {
			return nil, fmt.Errorf("runtime/library: mirror skill %q: %w", name, err)
		}
		hints[name] = skillDescription(srcPath)
	}
	if logger != nil && len(hints) > 0 {
		logger.Info("library: %d skill(s) mirrored into %s", len(hints), dest)
	}
	return hints, nil
}

// collectSkillRefs returns the deduplicated union of the workflow-level
// `skills:` default and every LLM node's `skills:` list, in a stable order
// (workflow defaults first, then node refs in node-map iteration order,
// deduped). Order does not affect correctness — the mirror is idempotent and
// the hint list is re-sorted per node.
func collectSkillRefs(wf *ir.Workflow) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(wf.Skills)
	for _, node := range wf.Nodes {
		if ln, ok := node.(ir.LLMNode); ok {
			add(ln.GetSkills())
		}
	}
	return out
}

// skillDescription parses the `description:` frontmatter of a skill file for the
// prompt hint. Returns "" on any read error (the hint still lists the name).
func skillDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	_, desc := skilllib.ScanFrontmatter(f)
	return desc
}
