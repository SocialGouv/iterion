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
func mirrorPluginContributions(workDir string, inj *Contributions, logger *iterlog.Logger) error {
	if workDir == "" {
		return nil
	}
	if inj != nil {
		return mirrorInjectedPluginFiles(workDir, inj.Plugin, logger)
	}
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("runtime: load plugins for contribution mirror: %v — skipping", err)
		}
		return nil
	}
	enabled := reg.Enabled()
	if len(enabled) == 0 {
		return nil
	}

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
							return fmt.Errorf("runtime/plugin: mkdir %s: %w", d, err)
						}
					}
					dirsReady = true
				}
				if tmpDir == "" {
					t, terr := os.MkdirTemp("", "iterion-plugin-contrib-*")
					if terr != nil {
						return terr
					}
					tmpDir = t
				}
				tmpPath := filepath.Join(tmpDir, f.Name)
				if werr := os.WriteFile(tmpPath, f.Content, 0o644); werr != nil {
					return werr
				}
				destPath := filepath.Join(destDir, f.Name)
				markerPath := filepath.Join(markerDir, f.Name+".sha256")
				if _, rerr := reconcileSkillFile(tmpPath, destPath, markerPath, logger); rerr != nil {
					return fmt.Errorf("runtime/plugin: mirror %s %q from %q: %w", kind.Name, f.Name, p.Name(), rerr)
				}
			}
		}
	}
	return nil
}
