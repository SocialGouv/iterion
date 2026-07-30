package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Contributions carries PRE-RESOLVED plugin markdown contributions (skills /
// commands / agents) and skill-library skills so a run executing where the
// operator's iterion home is NOT on disk mirrors exactly what the instance that
// launched it had.
//
// Why it exists: mirrorPluginContributions and mirrorLibrarySkills resolve from
// the local filesystem (<iterion-home>/plugins, <iterion-home>/skills). That is
// right for a local CLI/studio run, but a cloud runner pod gets an ephemeral,
// EMPTY iterion home — so an operator-installed plugin's skill (or a DSL
// `skills:` library reference) silently never reached the workspace there, and
// only the compiled-in builtins did. The launching instance, which does have
// the operator's home, resolves the payload and ships it on the queue message;
// the runner hands it to the engine through WithContributions.
//
// Semantics:
//   - nil          → resolve from the local filesystem (the local path, unchanged).
//   - non-nil      → AUTHORITATIVE: mirror exactly this set and never consult the
//     local registry/store. An empty payload legitimately means
//     "nothing enabled on the launching instance".
type Contributions struct {
	// Plugin holds enabled-plugin markdown files, already read.
	Plugin []ContributionFile
	// Library holds skill-library skills the workflow references by name.
	Library []LibrarySkillFile
}

// ContributionFile is one plugin markdown file bound for
// <workDir>/.claude/<Kind>/<Name>.
type ContributionFile struct {
	// Kind is the .claude/ leaf dir: "skills" | "commands" | "agents"
	// (plugin.MirrorKind.Dir).
	Kind string
	// Name is the destination file name, e.g. "deploy-target.md".
	Name string
	// Content is the file body.
	Content []byte
}

// LibrarySkillFile is one resolved skill-library skill. Description is the
// frontmatter description, carried so the runner reproduces the "## Skills"
// prompt hint without re-reading a store it does not have.
type LibrarySkillFile struct {
	Name        string
	Description string
	Content     []byte
}

// IsEmpty reports whether the payload would mirror nothing.
func (c *Contributions) IsEmpty() bool {
	return c == nil || (len(c.Plugin) == 0 && len(c.Library) == 0)
}

// mirrorInjectedPluginFiles mirrors pre-resolved plugin markdown into
// <workDir>/.claude/<kind>/, applying the SAME 4-branch collision policy as the
// local path (reconcileSkillFile) so precedence stays identical whichever way
// the files arrived. Content is staged through a temp file because
// reconcileSkillFile compares on-disk sources.
func mirrorInjectedPluginFiles(workDir string, files []ContributionFile, logger *iterlog.Logger) ([]string, error) {
	if workDir == "" || len(files) == 0 {
		return nil, nil
	}
	tmpDir, err := os.MkdirTemp("", "iterion-injected-contrib-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	var owned []string
	ready := map[string]bool{}
	for _, f := range files {
		if f.Kind == "" || f.Name == "" {
			continue
		}
		destDir := filepath.Join(workDir, ".claude", f.Kind)
		markerDir := filepath.Join(destDir, bundleMirrorMarkerDir)
		if !ready[f.Kind] {
			for _, d := range []string{destDir, markerDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return nil, fmt.Errorf("runtime/contrib: mkdir %s: %w", d, err)
				}
			}
			ready[f.Kind] = true
		}
		tmpPath := filepath.Join(tmpDir, f.Kind+"__"+f.Name)
		if err := os.WriteFile(tmpPath, f.Content, 0o644); err != nil {
			return nil, err
		}
		destPath := filepath.Join(destDir, f.Name)
		markerPath := filepath.Join(markerDir, f.Name+".sha256")
		outcome, err := reconcileSkillFile(tmpPath, destPath, markerPath, logger)
		if err != nil {
			return nil, fmt.Errorf("runtime/contrib: mirror %s %q: %w", f.Kind, f.Name, err)
		}
		// Skills only — commands and agents are mirrored for claude_code's
		// discovery, and are not skills a backend may pass as one.
		if f.Kind == "skills" && outcome != skillOutcomeShadowed {
			owned = append(owned, destPath)
		}
	}
	if logger != nil {
		logger.Info("contributions: %d injected plugin file(s) mirrored into %s", len(files), filepath.Join(workDir, ".claude"))
	}
	return owned, nil
}

// mirrorInjectedLibrarySkills mirrors pre-resolved library skills as
// <workDir>/.claude/skills/<name>/SKILL.md (the directory form — the only shape
// claude_code's Skill tool discovers) and returns the name→description hints,
// matching mirrorLibrarySkills' contract exactly.
func mirrorInjectedLibrarySkills(workDir string, skills []LibrarySkillFile, logger *iterlog.Logger) (map[string]string, []string, error) {
	if workDir == "" || len(skills) == 0 {
		return nil, nil, nil
	}
	tmpDir, err := os.MkdirTemp("", "iterion-injected-libskill-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	dest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	for _, d := range []string{dest, markerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, nil, fmt.Errorf("runtime/contrib: mkdir %s: %w", d, err)
		}
	}

	var owned []string
	hints := make(map[string]string, len(skills))
	for _, s := range skills {
		if s.Name == "" {
			continue
		}
		tmpPath := filepath.Join(tmpDir, s.Name+".SKILL.md")
		if err := os.WriteFile(tmpPath, s.Content, 0o644); err != nil {
			return nil, nil, err
		}
		skillDir := filepath.Join(dest, s.Name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("runtime/contrib: mkdir %s: %w", skillDir, err)
		}
		destPath := filepath.Join(skillDir, "SKILL.md")
		markerPath := filepath.Join(markerDir, s.Name+".SKILL.md.sha256")
		outcome, err := reconcileSkillFile(tmpPath, destPath, markerPath, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime/contrib: mirror library skill %q: %w", s.Name, err)
		}
		if outcome != skillOutcomeShadowed {
			owned = append(owned, skillDir)
		}
		hints[s.Name] = s.Description
	}
	if logger != nil && len(hints) > 0 {
		logger.Info("contributions: %d injected library skill(s) mirrored into %s", len(hints), dest)
	}
	return hints, owned, nil
}
