package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// SkillFrontmatter mirrors the YAML header used by Claude Code skills.
type SkillFrontmatter = intl.SkillFrontmatter

// SkillResolution describes a resolved skill (filesystem or bundled).
type SkillResolution = intl.SkillResolution

// SkillSource identifies where a resolved skill lives.
type SkillSource = intl.SkillSource

// SkillInvocation is the parsed result of executing a skill.
type SkillInvocation = intl.SkillInvocation

// InstallOptions controls SkillInstall behavior.
type InstallOptions = intl.InstallOptions

const (
	SkillSourceFilesystem = intl.SkillSourceFilesystem
	SkillSourceBundled    = intl.SkillSourceBundled
)

// SkillTool returns the api.Tool registration for the "skill" tool.
func SkillTool() api.Tool { return intl.SkillTool() }

// ExecuteSkill loads a skill (LLM-invoked path). Honors
// disable-model-invocation: true. Pass an empty workDir to use the process
// CWD.
func ExecuteSkill(ctx context.Context, input map[string]any, workDir string) (string, error) {
	return intl.ExecuteSkill(input, workDir)
}

// ExecuteSkillUserInvoked loads a skill on behalf of the user. Bypasses the
// disable-model-invocation gate.
func ExecuteSkillUserInvoked(ctx context.Context, input map[string]any, workDir string) (string, error) {
	return intl.ExecuteSkillUserInvoked(input, workDir)
}

// ExecuteSkillPinned loads a skill (LLM-invoked path) resolved through an
// explicit name → path allowlist instead of the workspace walk — the
// shape a host uses when the workspace is not trusted (iterion#1719). The
// map is the whole servable set; a nil or empty map refuses every name.
func ExecuteSkillPinned(ctx context.Context, input map[string]any, allowed map[string]string) (string, error) {
	return intl.ExecuteSkillPinned(input, allowed)
}

// ResolveSkillPinned resolves a skill name against an explicit allowlist,
// without walking any directory.
func ResolveSkillPinned(name string, allowed map[string]string) (SkillResolution, error) {
	return intl.ResolveSkillPinned(name, allowed)
}

// ResolveSkill turns a user-supplied skill identifier into a concrete path.
func ResolveSkill(name, workDir string) (SkillResolution, error) {
	return intl.ResolveSkill(name, workDir)
}

// ResolveSkillFrontmatter returns the parsed frontmatter for a skill along
// with the resolution metadata, without executing the skill.
func ResolveSkillFrontmatter(name, workDir string) (SkillFrontmatter, SkillResolution, error) {
	return intl.ResolveSkillFrontmatter(name, workDir)
}

// ListBundledSkills returns the canonical "<plugin>:<skill>" names of every
// embedded skill, sorted.
func ListBundledSkills() []string { return intl.ListBundledSkills() }

// InstallSkillFromMarketplace fetches a skill from the official Anthropic
// marketplace into ~/.claude/skills/ (or InstallOptions.Destination).
func InstallSkillFromMarketplace(name string, opts InstallOptions) (string, error) {
	return intl.InstallSkillFromMarketplace(name, opts)
}
