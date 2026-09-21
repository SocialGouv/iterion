package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// skipped, never failing the run — the DSL reference is soft. It is only
// WARNED about when nothing else already satisfied it: a bundle that ships its
// own skills is the normal case, and warning that each of them is "not in the
// skill library" is true, useless, and reads as a broken run.
//
// The library layers a machine-global store (~/.iterion/skills) with an
// optional per-project override (<projectStoreDir>/skills). No-op (nil map)
// when the workflow references no skills.
// When inj is non-nil the payload is AUTHORITATIVE: its skills are mirrored and
// the local library store is never consulted (the cloud path — a runner pod has
// no library on disk, so the launching instance resolved the workflow's refs
// for it; see Contributions).
func mirrorLibrarySkills(workDir, projectStoreDir string, wf *ir.Workflow, extra []string, inj *Contributions, logger *iterlog.Logger) (hints map[string]string, owned []string, complete bool, err error) {
	if workDir == "" || wf == nil {
		return nil, nil, true, nil
	}
	if inj != nil {
		// The cloud path: the launching instance already resolved BOTH the
		// workflow's refs and the operator's extras into the payload, so the
		// union is upstream of here and the pod just mirrors what it was sent.
		injHints, injOwned, injErr := mirrorInjectedLibrarySkills(workDir, inj.Library, logger)
		// The payload is authoritative for WHAT to mirror, but the workflow's
		// declaration is checkable right here: every declared ref must have
		// been produced this pass — by the payload, or by the bundle/plugin
		// mirrors that outrank the library and ran earlier in the sequence —
		// or be provably untouchable by the pruner. A ref that is neither was
		// dropped upstream of the payload; blessing the pass complete would
		// let the pruner delete the still-declared skill's prior copy
		// (#1500 R6 high).
		dest := filepath.Join(workDir, ".claude", "skills")
		markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
		complete := true
		for _, name := range unionSkillRefs(collectSkillRefs(wf), extra) {
			if skillCoveredThisPass(dest, markerDir, name) {
				continue
			}
			complete = false
			if logger != nil {
				logger.Warn("library: skill %q is declared by the workflow but was produced by no mirror this pass (dropped from the contributions payload, or unresolved upstream) — the orphan pruner is skipped for this pass", name)
			}
		}
		return injHints, injOwned, complete, injErr
	}
	refs := unionSkillRefs(collectSkillRefs(wf), extra)
	if len(refs) == 0 {
		return nil, nil, true, nil
	}
	store := skilllib.LocalStoreForProject(projectStoreDir)

	dest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)

	hints = make(map[string]string)
	complete = true
	dirsReady := false
	for _, name := range refs {
		if verr := skilllib.ValidName(name); verr != nil {
			if logger != nil {
				logger.Warn("skill %q skipped: %v", name, verr)
			}
			// The DSL declared this name; iterion refused it. From the
			// pruner's perspective the pass did not mirror it — mark
			// incomplete so the pruner does not treat last pass's file
			// (if any) as an orphan.
			complete = false
			continue
		}
		srcPath, ok := store.Resolve(name)
		if !ok {
			// The reference may already be satisfied: this mirror runs AFTER the
			// bundle and plugin ones, which take precedence (ADR-059). Warning
			// that a bundle skill is "not in the skill library" is true and
			// useless — it reads as a broken run to anyone watching the log, and
			// every bundle that declares its own skills (or an operator's
			// hand-authored file, or a marker-less bundle directory skill) owns
			// the name. A PREVIOUS pass's library copy does not satisfy the ref:
			// its sidecar is stale, and that file is exactly what the pruner
			// would delete on a transient store outage (#1500 R17fd85).
			if skillCoveredThisPass(dest, markerDir, name) {
				if logger != nil {
					logger.Debug("skill %q resolved from the bundle/plugin mirror, not the library", name)
				}
				continue
			}
			if logger != nil {
				logger.Warn("skill %q referenced by the workflow is not in the skill library (~/.iterion/skills or <project>/.iterion/skills) — not mirrored", name)
			}
			// Same rationale as the ValidName branch: the DSL declared
			// this skill, iterion failed to produce it. Whatever a prior
			// pass wrote for this name is now un-verifiable — the
			// pruner MUST NOT run, or it will delete a skill whose
			// declaration was TEMPORARILY unresolvable (a store outage,
			// a per-project override missing).
			complete = false
			continue
		}
		if !dirsReady {
			for _, d := range []string{dest, markerDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return nil, nil, false, fmt.Errorf("runtime/library: mkdir %s: %w", d, err)
				}
			}
			dirsReady = true
		}
		// Always mirror as <dest>/<name>/SKILL.md (directory form): a flat
		// <name>.md is NOT discovered as a skill by claude_code's Skill tool
		// (only the directory form is — Agent Skills spec), and claw discovers
		// both, so the directory form is the one that satisfies both backends.
		// A source that is already <name>/SKILL.md maps to the same dest.
		skillDir := filepath.Join(dest, name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			// I/O errors on a declared library skill (ENOSPC, EACCES,
			// ENOTDIR when the checkout planted a plain file where the
			// skill dir goes) stay FATAL: a run whose `.bot` explicitly
			// declares `skills: [x]` must not proceed and report success
			// without x — the doctrine is no silent fallback. skilllib
			// name validation is already soft above (line ~61); anything
			// reaching here is a filesystem issue.
			return nil, nil, false, fmt.Errorf("runtime/library: mirror skill %q: mkdir %s: %w", name, skillDir, err)
		}
		destPath := filepath.Join(skillDir, "SKILL.md")
		markerPath := filepath.Join(markerDir, name+".SKILL.md.sha256")
		outcome, err := reconcileSkillFile(srcPath, destPath, markerPath, skillTierLibrary, logger)
		if err != nil {
			// Same rationale as the mkdir branch: an I/O failure on a
			// declared library skill is fatal.
			return nil, nil, false, fmt.Errorf("runtime/library: mirror skill %q: %w", name, err)
		}
		// The hint is recorded either way — claude_code and claw read the
		// directory natively, so the agent sees the skill whoever wrote it. The
		// OWNED list is what a backend passes explicitly, so a shadowed entry
		// stays out of it: that content is the target repository's.
		// The FILE, not skillDir: MkdirAll succeeds on a directory the checkout
		// already shipped, so claiming the directory would hand over whatever
		// else the target repo planted in it.
		if outcome != skillOutcomeShadowed {
			owned = append(owned, destPath)
		}
		hints[name] = skillDescription(srcPath)
	}
	if logger != nil && len(hints) > 0 {
		logger.Info("library: %d skill(s) mirrored into %s", len(hints), dest)
	}
	return hints, owned, complete, nil
}

// collectSkillRefs returns the deduplicated union of the workflow-level
// `skills:` default and every LLM node's `skills:` list, in a stable order
// (workflow defaults first, then node refs in node-map iteration order,
// deduped). Order does not affect correctness — the mirror is idempotent and
// the hint list is re-sorted per node. Delegates to the exported
// CollectSkillRefs, the ONE declaration collector shared with the cloud
// publisher.
func collectSkillRefs(wf *ir.Workflow) []string {
	return CollectSkillRefs(wf)
}

// unionSkillRefs appends the operator's run-level skills to the workflow's own,
// deduped, workflow first.
//
// Union, not replacement: the bot's author declared their set for a reason and
// an operator adding a house standard must not be able to drop one. Order puts
// the workflow's first only for stability — the mirror is idempotent and each
// node re-sorts its own hint list.
func unionSkillRefs(wfRefs, extra []string) []string {
	if len(extra) == 0 {
		return wfRefs
	}
	seen := make(map[string]bool, len(wfRefs)+len(extra))
	out := make([]string, 0, len(wfRefs)+len(extra))
	for _, list := range [][]string{wfRefs, extra} {
		for _, n := range list {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// ResolveExtraSkills checks that every operator-supplied skill name exists in
// the local library, returning an error naming the ones that do not alongside
// what IS available.
//
// Deliberately stricter than the workflow's own `skills:` reference, which is
// soft (a bundle ships its skills, the library is a fallback, and warning per
// missing name would read as a broken run). This list was TYPED by a human who
// expects it to take effect; dropping it with a log line reproduces exactly the
// silent failure this whole seam exists to avoid — a skill mirrored nowhere,
// named nowhere, and an agent that answers from priors instead.
//
// Mirrors the `--preset` precedent, which errors at launch and lists what the
// workflow declares.
func ResolveExtraSkills(projectStoreDir string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	store := skilllib.LocalStoreForProject(projectStoreDir)
	var missing []string
	for _, n := range names {
		if err := skilllib.ValidName(n); err != nil {
			return fmt.Errorf("skill %q: %w", n, err)
		}
		if _, ok := store.Resolve(n); !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	available := "none — add one with `iterion skill add <name> --from <file.md>`"
	if list, err := store.List(); err == nil && len(list) > 0 {
		names := make([]string, 0, len(list))
		for _, s := range list {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		available = strings.Join(names, ", ")
	}
	return fmt.Errorf("skill %s: not in the skill library (available: %s)",
		strings.Join(missing, ", "), available)
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

// alreadyMirrored reports whether <dest>/<name> was already produced by the
// bundle or plugin mirror, which run before this one and outrank the library
// (ADR-059). Both shapes are accepted: the directory form <name>/SKILL.md that
// claude_code's Skill tool discovers, and the flat <name>.md that claw also
// reads.
func alreadyMirrored(dest, name string) bool {
	for _, p := range []string{
		filepath.Join(dest, name, "SKILL.md"),
		filepath.Join(dest, name+".md"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// skillCoveredThisPass reports whether a declared skill reference is covered
// as of THIS mirror pass — satisfied for the orphan pruner's purposes, not
// merely present on disk. One predicate serves both resolution paths:
//
//   - a fresh tier sidecar (<name>.SKILL.md.sha256.tier, rewritten by
//     writeMarker after ClearMirroredTierMarkers wiped every sidecar at pass
//     start) means a mirror produced the file THIS pass — the payload mirror,
//     the local library mirror, or the bundle/plugin mirrors that outrank it;
//   - a discoverable file with NO marker at all means a marker-less producer
//     owns the name (a bundle's directory-form skill, or a hand-authored
//     workspace file) — the pruner never touches a name without a marker, so
//     the ref cannot become an orphan either way;
//   - a marker WITHOUT a fresh sidecar is the dangerous middle: a file a
//     PREVIOUS pass mirrored that this pass did not reproduce — exactly the
//     file the pruner would delete, so the ref is NOT covered.
//
// The injected and local paths both consult it, so "declared but produced by
// no mirror this pass" cannot mean different things on the two paths.
func skillCoveredThisPass(dest, markerDir, name string) bool {
	markerPath := filepath.Join(markerDir, name+".SKILL.md.sha256")
	if _, err := os.Stat(markerPath + tierSidecarSuffix); err == nil {
		return true
	}
	if _, err := os.Stat(markerPath); err == nil {
		return false
	}
	return alreadyMirrored(dest, name)
}
