package delegate

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// claudeAmbientSpawn is how a claude_code spawn carries the node's
// ambient-context policy (ADR-119): the settings scopes the CLI loads, and the
// memory files it must skip.
type claudeAmbientSpawn struct {
	// sources is never nil, so `--setting-sources` is always emitted: an
	// omitted flag makes the CLI load every scope, `local` included.
	sources []claudesdk.SettingSource
	// excludes feeds `claudeMdExcludes` in the flag settings layer, which
	// outranks every settings file. nil excludes nothing.
	excludes []string
	// legacy reports that ITERION_CLAUDE_CODE_SETTING_SOURCES decided.
	legacy bool
	// unknown holds the tokens of that variable that name no scope.
	unknown []string
}

// claudeAmbient translates the node's policy into Claude Code's two
// mechanisms. The project scope always loads: it carries the engine's own
// mirrored skills and plugin contributions (pkg/runtime/plugin_skills.go),
// which the CLI only discovers under it. What a policy removes is memory
// files (CLAUDE.md, CLAUDE.local.md, .claude/rules/) and the operator's user
// scope:
//
//	all        user,project
//	workspace  project, minus the memory files above the repository root
//	operator   user,project, minus the repository's memory files (its main
//	           checkout's too, for a nested worktree: ambient.OperatorBoundary)
//	none       project, minus every memory file
//
// The exclusions are needed because project memory walks up to the
// filesystem root, not to the repository root: with the workspace under
// $HOME, the project scope alone loads ~/.claude/CLAUDE.md and
// ~/.claude/rules, and a run worktree nested in the repository loads the
// primary checkout's CLAUDE.md as well (measured on CLI 2.1.282).
//
// ITERION_CLAUDE_CODE_SETTING_SOURCES, when set, replaces the scopes verbatim
// and drops the exclusions: it is the operator's raw escape hatch.
func claudeAmbient(task Task) claudeAmbientSpawn {
	if srcs, set, unknown := settingSourcesFromEnv(); set {
		return claudeAmbientSpawn{sources: srcs, legacy: true, unknown: unknown}
	}
	project := []claudesdk.SettingSource{claudesdk.SettingSourceProject}
	both := []claudesdk.SettingSource{claudesdk.SettingSourceUser, claudesdk.SettingSourceProject}
	root := ambient.RepoRoot(task.WorkDir)
	switch task.AmbientContext {
	case ambient.All:
		return claudeAmbientSpawn{sources: both}
	case ambient.Operator:
		return claudeAmbientSpawn{sources: both, excludes: excludeTree(ambient.OperatorBoundary(root))}
	case ambient.None:
		return claudeAmbientSpawn{sources: project, excludes: []string{"**"}}
	default:
		return claudeAmbientSpawn{sources: project, excludes: excludeAncestors(root)}
	}
}

// settingSourcesOption emits the scopes, an empty list included.
func (a claudeAmbientSpawn) settingSourcesOption() claudesdk.Option {
	if len(a.sources) == 0 {
		return claudesdk.WithNoSettingSources()
	}
	return claudesdk.WithSettingSources(a.sources...)
}

// excludeTree matches every memory file under dir, under each spelling of dir.
func excludeTree(dir string) []string {
	var out []string
	for _, form := range ambient.Forms(dir) {
		out = append(out, filepath.Join(form, "**"))
	}
	return out
}

// excludeAncestors matches the memory files the CLI's walk-up reads above
// root: each ancestor's CLAUDE.md and CLAUDE.local.md, and its
// .claude/CLAUDE.md and .claude/rules/ — the operator's own memory when root sits
// under $HOME. Never the whole .claude/ tree: a worktree nested in a main
// checkout's .claude/worktrees/ lives under it, and its own files would match.
func excludeAncestors(root string) []string {
	var out []string
	for _, dir := range ambient.AncestorsAbove(root) {
		for _, form := range ambient.Forms(dir) {
			out = append(out,
				filepath.Join(form, "CLAUDE.md"),
				filepath.Join(form, "CLAUDE.local.md"),
				filepath.Join(form, ".claude", "CLAUDE.md"),
				filepath.Join(form, ".claude", "rules", "**"),
			)
		}
	}
	return out
}

var (
	legacySourcesNoticeOnce  sync.Once
	unknownSourcesNoticeOnce sync.Once
)

// reportLegacySources says, once per process, that the raw variable decided
// instead of the node's policy, and names any token it could not read.
func reportLegacySources(logger *iterlog.Logger, a claudeAmbientSpawn) {
	if !a.legacy || logger == nil {
		return
	}
	legacySourcesNoticeOnce.Do(func() {
		logger.Warn("%s is set: it replaces the ambient_context policy of every claude_code node (ADR-119)", settingSourcesEnv)
	})
	if len(a.unknown) > 0 {
		unknownSourcesNoticeOnce.Do(func() {
			logger.Warn("%s: ignored token(s) %s — the known scopes are user, project and local", settingSourcesEnv, strings.Join(a.unknown, ", "))
		})
	}
}
