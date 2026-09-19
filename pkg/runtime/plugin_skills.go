package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// contribClaimTracker de-dups plugin contributions within a single mirror
// pass, shared by the LOCAL (mirrorPluginContributions) and CLOUD-INJECTED
// (mirrorInjectedPluginFiles) paths so their behaviour cannot drift. A copied
// guard is where two mirrors first learn to disagree — the injected path
// once carried neither the collision warning nor the `owned` de-dup that the
// local path did, and #1374 named that exact class.
//
// It tracks two things across a single mirror invocation:
//
//   - claim(kindDir, name, who): who already wrote to <destDir>/<name> this
//     pass. `who` is human-readable (a plugin name locally; empty upstream-of-me
//     in the injected path where identity has been erased on the wire). Returns
//     (previous claimant, this-is-a-duplicate).
//
//   - report(destPath): whether destPath has already been appended to the
//     owned list. Two files that resolve to one destination must be reported
//     once, or a backend hands the same path to an agent twice and its dedup
//     then decides what gets loaded.
type contribClaimTracker struct {
	claimed  map[string]string
	reported map[string]bool
}

func newContribClaimTracker() *contribClaimTracker {
	return &contribClaimTracker{
		claimed:  map[string]string{},
		reported: map[string]bool{},
	}
}

// claim records who wrote (kindDir, name) and returns the previous claimant
// (empty if none, or if the prior write was unattributed) alongside whether
// this call is a duplicate. Keyed on kindDir+"/"+name so a skill "deploy.md"
// and a command "deploy.md" — legitimately distinct destinations — never
// collide.
func (t *contribClaimTracker) claim(kindDir, name, who string) (prev string, dup bool) {
	key := kindDir + "/" + name
	prev, dup = t.claimed[key]
	t.claimed[key] = who
	return prev, dup
}

// report appends destPath to the owned list only the FIRST time it is
// mentioned in this pass. Returns true when the caller should append.
func (t *contribClaimTracker) report(destPath string) bool {
	if t.reported[destPath] {
		return false
	}
	t.reported[destPath] = true
	return true
}

// mirrorPluginContributions mirrors the markdown contributions (skills,
// commands, agents) of every enabled plugin into the workspace's
// <workDir>/.claude/<skills|commands|agents>/ directories, applying the same
// 4-branch collision policy as bundle skills (copy / no-op / refresh / shadow)
// via reconcileSkillFile. claude_code discovers all three via
// --setting-sources project; the claw backend reads the same dirs.
//
// It runs at run start and on resume, right after the bundle skills are
// mirrored, so a plugin file is shadowed by a same-named bundle/workspace file
// rather than clobbering it. A registry-load failure or a single broken plugin
// is logged and skipped — a plugin must never break a run's setup. No-op when
// workDir is empty.
//
// When inj is non-nil the payload is AUTHORITATIVE: the files it carries are
// mirrored and the local plugin registry is never consulted. That is the cloud
// path — a runner pod's iterion home is empty, so the launching instance
// resolved the enabled plugins' files for it (see Contributions).
// It returns the SKILL files iterion owns after the mirror — the same contract
// as mirrorBundleSkills, and for the same reason: a backend that must decide
// what it may hand an agent cannot recover that from the workspace, which is a
// checkout of an untrusted repository. Commands and agents are excluded; they
// are not skills.
//
// Skill files write in the DIRECTORY form <name>/SKILL.md (with a flat alias
// <name>.md for prompt-driven Reads), matching mirrorBundleSkills — the flat
// form alone is NOT discovered as a skill by claude_code's Skill tool (only
// the directory form is, per the Agent Skills spec; claw discovers both). See
// mirrorFileSkill for the two-form contract.
func mirrorPluginContributions(workDir string, inj *Contributions, logger *iterlog.Logger) ([]string, error) {
	if workDir == "" {
		return nil, nil
	}
	if inj != nil {
		return mirrorInjectedPluginFiles(workDir, inj.Plugin, logger)
	}
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("runtime: load plugins for contribution mirror: %v — skipping", err)
		}
		return nil, nil
	}
	enabled := reg.Enabled()
	if len(enabled) == 0 {
		return nil, nil
	}
	var owned []string

	// One temp dir for all kinds: content is written there then run through
	// reconcileSkillFile so plugin files reuse the exact bundle collision
	// policy. Created lazily on the first file so a run with no markdown
	// contributions touches no filesystem.
	var tmpDir string
	defer func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	// One tracker across all kinds — its key is kindDir+"/"+name, so a
	// skill and a command sharing a base name (different destinations)
	// never collide. Sharing it with mirrorInjectedPluginFiles is the
	// point: whichever way the files arrived, the same guard runs.
	tracker := newContribClaimTracker()

	for _, kind := range plugin.MirrorKinds {
		destDir := filepath.Join(workDir, ".claude", kind.Dir)
		markerDir := filepath.Join(destDir, bundleMirrorMarkerDir)
		dirsReady := false

		for _, p := range enabled {
			files, ferr := p.MirrorFiles(kind)
			if ferr != nil {
				if logger != nil {
					logger.Warn("runtime: plugin %q %ss: %v — skipping", p.Name(), kind.Name, ferr)
				}
				continue
			}
			for _, f := range files {
				if !dirsReady {
					for _, d := range []string{destDir, markerDir} {
						if err := os.MkdirAll(d, 0o755); err != nil {
							return nil, fmt.Errorf("runtime/plugin: mkdir %s: %w", d, err)
						}
					}
					dirsReady = true
				}
				if tmpDir == "" {
					t, terr := os.MkdirTemp("", "iterion-plugin-contrib-*")
					if terr != nil {
						return nil, terr
					}
					tmpDir = t
				}
				tmpPath := filepath.Join(tmpDir, f.Name)
				if werr := os.WriteFile(tmpPath, f.Content, 0o644); werr != nil {
					return nil, werr
				}
				outcome, destPath, rerr := mirrorPluginContribFile(destDir, markerDir, tmpPath, f.Name, kind, logger)
				if rerr != nil {
					// A malformed contribution name (e.g. one that
					// skillDestDirForm refuses because it has no .md
					// extension case-insensitively) must not abort the
					// whole mirror pass — that would discard every other
					// plugin's skills / commands / agents behind a single
					// WARN. Name the offender, keep going.
					if logger != nil {
						logger.Warn("runtime/plugin: skipping %s %q from %q: %v", kind.Name, f.Name, p.Name(), rerr)
					}
					continue
				}
				// Reports the COLLISION, never the winner. Two earlier
				// versions of this warning inferred which bytes landed from
				// the outcome enum and were wrong both times — a shadow can
				// equally mean a diverged workspace copy or a higher-tier
				// incumbent, and "neither landed" is false when one plugin's
				// content matched it. Who resolved the destination is stated
				// by reconcileSkillFile, on its own line, from what it did;
				// this line says only what it knows for certain. Silent when
				// the bytes are identical: nothing was lost, so there is
				// nothing to rename.
				prev, dup := tracker.claim(kind.Dir, f.Name, p.Name())
				if dup && logger != nil && outcome != skillOutcomeUpToDate {
					if prev == p.Name() {
						logger.Warn("runtime/plugin: plugin %q contributes two %ss that mirror to the same name %q — one destination, so one of them is lost; rename one",
							p.Name(), kind.Name, f.Name)
					} else {
						logger.Warn("runtime/plugin: %s %q is contributed by both %q and %q — one name, one destination at %s; which contribution survives is decided by the mirror, not by either plugin; rename one",
							kind.Name, f.Name, prev, p.Name(), destPath)
					}
				}
				if kind.Name == "skill" && outcome != skillOutcomeShadowed && tracker.report(destPath) {
					owned = append(owned, destPath)
				}
			}
		}
	}
	return owned, nil
}

// mirrorPluginContribFile places one contribution file on disk under the
// shared collision policy and returns the outcome plus the destination path
// that names the write (what the caller reports on the owned list, when the
// kind is a skill).
//
// Skills go through mirrorFileSkill so a flat "<stem>.md" source lands as
// BOTH the directory form <stem>/SKILL.md — the only shape claude_code's
// Skill tool discovers (Agent Skills spec) — and the flat alias <stem>.md
// that prompt Reads by path resolve. Commands and agents keep the flat shape,
// which is what claude_code discovers for THOSE kinds and what their contract
// has always been. The returned destPath is the directory-form file for a
// skill (the discoverable one that a backend must be handed) and the flat
// file for a command or agent.
func mirrorPluginContribFile(destDir, markerDir, tmpPath, name string, kind plugin.MirrorKind, logger *iterlog.Logger) (skillReconcileOutcome, string, error) {
	if kind.Name == "skill" {
		_, destPath, _, err := skillDestDirForm(destDir, markerDir, name)
		if err != nil {
			return skillOutcomeShadowed, "", err
		}
		outcome, err := mirrorFileSkill(destDir, markerDir, tmpPath, name, skillTierPlugin, logger)
		if err != nil {
			return outcome, "", err
		}
		return outcome, destPath, nil
	}
	destPath := filepath.Join(destDir, name)
	markerPath := filepath.Join(markerDir, name+".sha256")
	outcome, err := reconcileSkillFile(tmpPath, destPath, markerPath, skillTierPlugin, logger)
	if err != nil {
		return outcome, destPath, err
	}
	return outcome, destPath, nil
}
