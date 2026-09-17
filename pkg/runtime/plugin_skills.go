package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

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

	for _, kind := range plugin.MirrorKinds {
		destDir := filepath.Join(workDir, ".claude", kind.Dir)
		markerDir := filepath.Join(destDir, bundleMirrorMarkerDir)
		dirsReady := false
		// Which plugin already claimed each mirrored name in THIS pass, and
		// which destinations were already reported. Two plugins contributing
		// one name land on one destination at the same tier, where
		// reconcileSkillFile's precedence guard does not apply: it overwrites
		// without a word, and the winner is the order Enabled() returns
		// (alphabetical by plugin name). The overwrite stays — refusing it
		// would break a workspace that relies on the incumbent — but it stops
		// being silent, and the destination is reported once.
		//
		// The cloud twin (mirrorInjectedPluginFiles) carries neither yet, and
		// is NOT covered by cloudpublisher's dedup: replaceContribution runs
		// only over locally installed plugins, while team-scoped git-hosted
		// sources are appended unconditionally — so two teams' packs claiming
		// one name still substitute silently there.
		claimed := map[string]string{}
		reported := map[string]bool{}

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
				destPath := filepath.Join(destDir, f.Name)
				markerPath := filepath.Join(markerDir, f.Name+".sha256")
				outcome, rerr := reconcileSkillFile(tmpPath, destPath, markerPath, skillTierPlugin, logger)
				if rerr != nil {
					return nil, fmt.Errorf("runtime/plugin: mirror %s %q from %q: %w", kind.Name, f.Name, p.Name(), rerr)
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
				if prev, dup := claimed[f.Name]; dup && logger != nil && outcome != skillOutcomeUpToDate {
					if prev == p.Name() {
						logger.Warn("runtime/plugin: plugin %q contributes two %ss that mirror to the same name %q — one destination, so one of them is lost; rename one",
							p.Name(), kind.Name, f.Name)
					} else {
						logger.Warn("runtime/plugin: %s %q is contributed by both %q and %q — one name, one destination at %s; which contribution survives is decided by the mirror, not by either plugin; rename one",
							kind.Name, f.Name, prev, p.Name(), destPath)
					}
				}
				claimed[f.Name] = p.Name()
				if kind.Name == "skill" && outcome != skillOutcomeShadowed && !reported[destPath] {
					owned = append(owned, destPath)
					reported[destPath] = true
				}
			}
		}
	}
	return owned, nil
}
