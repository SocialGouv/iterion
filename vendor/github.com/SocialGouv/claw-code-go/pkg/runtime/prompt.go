package runtime

import (
	clawctx "github.com/SocialGouv/claw-code-go/internal/context"
	internalrt "github.com/SocialGouv/claw-code-go/internal/runtime"
)

// PromptConfig is re-exported from internal/runtime so external hosts
// (e.g. iterion) can select which system-prompt sections claw renders.
type PromptConfig = internalrt.PromptConfig

// DefaultPromptConfig returns the all-on default (Claude Code parity).
func DefaultPromptConfig() PromptConfig {
	return internalrt.DefaultPromptConfig()
}

// MinimalPromptConfig returns the all-off preset — the small-model mode
// where only the host's own prompt is sent, with no automatic sections.
func MinimalPromptConfig() PromptConfig {
	return internalrt.MinimalPromptConfig()
}

// PromptSectionNames returns the canonical section names accepted by
// ResolvePromptSections.
func PromptSectionNames() []string {
	return internalrt.PromptSectionNames()
}

// ResolvePromptSections builds a PromptConfig from CLI-style overrides:
// a non-empty `only` list enables exclusively the listed sections (implies
// minimal); `minimal` alone disables everything; `disable` turns sections
// off, applied last. Section names match case-insensitively with "-"/"_"
// ignored. Errors on the first unknown section name.
func ResolvePromptSections(minimal bool, only, disable []string) (PromptConfig, error) {
	cfg := &internalrt.Config{}
	if err := internalrt.ApplyPromptSectionOverrides(cfg, minimal, only, disable); err != nil {
		return PromptConfig{}, err
	}
	return *cfg.Prompt, nil
}

// OperatingPosture returns claw's authored operating-posture prompt section
// so hosts can compose it into their own system prompts. Hosts that already
// ship their own posture (e.g. iterion's authored base) should NOT add this
// — use BuildSystemContext alone to avoid a double posture.
func OperatingPosture() string {
	return clawctx.OperatingPosture
}

// BuildSystemContext renders the automatic context sections (environment,
// git status, CLAUDE.md project instructions incl. walk-up/imports, auto
// memory) for workDir according to cfg. It deliberately excludes the base
// identity sentence and the operating posture — the host owns its base
// prompt and opts into each piece.
//
// Each call builds a fresh assembler, so nothing is cached between calls:
// every invocation re-walks ancestors, re-reads memory files, and re-runs
// git. Hosts calling this per model turn should cache the result themselves
// and refresh on their own cadence.
func BuildSystemContext(workDir string, cfg PromptConfig) string {
	return clawctx.NewAssemblerWithOptions(workDir, cfg.AssembleOptions()).Assemble()
}

// BuildAutoMemorySection renders ONLY the auto-memory section — the memory
// directory's path, the maintenance instructions, and the current MEMORY.md
// content — against an explicit directory.
//
// It exists for hosts that compose their own system prompt and want the
// memory half without the rest of BuildSystemContext: environment, git status
// and CLAUDE.md are things such a host typically already supplies (or
// deliberately withholds), so taking them as a package deal would duplicate
// or override its own decisions. The explicit dir is the point — see
// [context.LoadAutoMemorySectionAt] for why deriving it from the working
// directory is wrong when that directory changes per session.
//
// Returns "" for an empty dir. Read-only: the directory is never created —
// the agent's file tools do that on first write.
func BuildAutoMemorySection(dir string) string {
	return clawctx.LoadAutoMemorySectionAt(dir)
}
