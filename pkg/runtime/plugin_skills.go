package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// mirrorPluginSkills mirrors the skills contributed by every enabled plugin
// into <workDir>/.claude/skills/, applying the same 4-branch collision policy
// as bundle skills (copy / no-op / refresh / shadow) via reconcileSkillFile.
// This is the runtime half of the plugin "skill" contribution kind — it runs
// at run start and on resume, right after the bundle skills are mirrored, so a
// plugin's skill is shadowed by a same-named bundle/workspace skill rather than
// clobbering it.
//
// A registry-load failure or a single broken plugin is logged and skipped — a
// plugin must never break a run's skill setup. No-op when workDir is empty.
func mirrorPluginSkills(workDir string, logger *iterlog.Logger) error {
	if workDir == "" {
		return nil
	}
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("runtime: load plugins for skill mirror: %v — skipping", err)
		}
		return nil
	}

	// dest/marker/tmp dirs are created lazily on the first skill, so a run with
	// no skill-contributing plugin enabled (the common case — only rtk on)
	// touches no filesystem. The content is written to a temp file then run
	// through reconcileSkillFile so plugin skills reuse the exact same
	// collision policy (copy / no-op / refresh / shadow) as bundle skills.
	var dest, markerDir, tmpDir string
	ensureDirs := func() error {
		if dest != "" {
			return nil
		}
		dest = filepath.Join(workDir, ".claude", "skills")
		markerDir = filepath.Join(dest, bundleMirrorMarkerDir)
		for _, d := range []string{dest, markerDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return fmt.Errorf("runtime/plugin: mkdir %s: %w", d, err)
			}
		}
		t, err := os.MkdirTemp("", "iterion-plugin-skills-*")
		if err != nil {
			return err
		}
		tmpDir = t
		return nil
	}
	defer func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	for _, p := range reg.Enabled() {
		files, ferr := p.SkillFiles()
		if ferr != nil {
			if logger != nil {
				logger.Warn("runtime: plugin %q skills: %v — skipping", p.Name(), ferr)
			}
			continue
		}
		for _, sk := range files {
			if err := ensureDirs(); err != nil {
				return err
			}
			tmpPath := filepath.Join(tmpDir, sk.Name)
			if werr := os.WriteFile(tmpPath, sk.Content, 0o644); werr != nil {
				return werr
			}
			destPath := filepath.Join(dest, sk.Name)
			markerPath := filepath.Join(markerDir, sk.Name+".sha256")
			if _, rerr := reconcileSkillFile(tmpPath, destPath, markerPath, logger); rerr != nil {
				return fmt.Errorf("runtime/plugin: mirror skill %q from %q: %w", sk.Name, p.Name(), rerr)
			}
		}
	}
	return nil
}
