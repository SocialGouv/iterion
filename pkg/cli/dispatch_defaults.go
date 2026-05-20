package cli

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
)

// defaultBotsFS is the embedded catalogue of bots exposed as assignees
// by the zero-config `iterion dispatch` mode. The tree is regenerated
// from examples/ via `task templates:dispatch-bots`. Each top-level
// directory is one assignee — the directory name becomes the assignee
// label (`feature-dev`, `whole-improve-loop`, …) and the contents
// form a bundle (main.bot + optional skills/, prompts/, attachments/).
//
//go:embed all:templates/dispatch_bots
var defaultBotsFS embed.FS

// defaultBotsFSRoot is the prefix the //go:embed directive prepends to
// every entry in defaultBotsFS. Stripped during extraction so the
// on-disk catalogue mirrors the embedded layout, not the package path.
const defaultBotsFSRoot = "templates/dispatch_bots"

// defaultBotIssuePrompt is the standard binding mapping the kanban
// issue title + body into a single multi-line string. The bots in the
// embedded catalogue each pick which workflow var to receive this
// blob into via [defaultAssigneeDispatch].
const defaultBotIssuePrompt = "{{issue.title}}\n\n{{issue.body}}"

// defaultDispatchPort is the studio HTTP surface for the no-arg mode.
// 4892 matches the example in docs/dispatcher.md so the published port
// stays stable across iterations.
const defaultDispatchPort = 4892

// defaultDispatchMaxConcurrent is intentionally conservative — the
// zero-config mode is meant for laptop / single-developer use, not
// fleet operation. Operators who need more should write a YAML.
const defaultDispatchMaxConcurrent = 2

// defaultAssigneeDispatch wires the issue title/body into each bot's
// expected input contract. Keys must match the assignee labels in
// [defaultAssigneeWorkflows] (the validator in pkg/dispatcher/config.go
// enforces this). Every entry binds `workspace_dir` to the per-issue
// workspace path so bots that default it to `${PROJECT_DIR}` run
// against the dispatcher-allocated worktree rather than wherever the
// daemon was started.
//
// The bot-specific main prompt var (feature_prompt, improvement_prompt,
// scope_notes, user_prompt, …) was extracted from each bot's `vars:`
// block. When a bot has no obvious prompt-shaped input (sec-audit-deps,
// branch-improve-loop's diff focus), only `workspace_dir` is bound and
// `scope_notes` carries the issue text when the bot accepts it.
//
// Both kebab-case (`feature-dev`) and snake_case (`feature_dev`)
// assignee labels are mapped to the same workflow. The whats-next bot
// emits snake_case (matching the Go package convention used inside the
// examples/ tree); hand-written or git-versioned configs typically use
// kebab-case (matching the bot directory names). Supporting both makes
// auto-config robust to either origin without forcing the operator or
// the bot to pick one.
// workspace_dir is INTENTIONALLY omitted from the per-bot dispatch
// configs below — every shipped bot defaults it to "${PROJECT_DIR}",
// which the runtime expands to the worktree path the workflow's
// `worktree: auto` block produces (and remaps to the in-container
// path when sandbox is active, per pkg/runtime/engine.go:90's
// containerWorkspace logic). Overriding workspace_dir to
// {{dispatcher.workspace_path}} (the prior default) defeated both
// branches: the bot's prompt referenced an empty dir that wasn't
// bind-mounted into the sandbox, while the runtime's actual worktree
// got ignored — every Read tool call failed in cascade.
func defaultAssigneeDispatch() map[string]dispatcher.DispatchConfig {
	featureDev := dispatcher.DispatchConfig{Vars: map[string]string{
		"feature_prompt": defaultBotIssuePrompt,
	}}
	wholeImproveLoop := dispatcher.DispatchConfig{Vars: map[string]string{
		"improvement_prompt": defaultBotIssuePrompt,
		"scope_notes":        "{{issue.body}}",
	}}
	branchImproveLoop := dispatcher.DispatchConfig{Vars: map[string]string{
		"scope_notes": defaultBotIssuePrompt,
	}}
	whatsNext := dispatcher.DispatchConfig{Vars: map[string]string{
		"scope_notes": defaultBotIssuePrompt,
	}}
	docAlign := dispatcher.DispatchConfig{Vars: map[string]string{
		"scope_notes": defaultBotIssuePrompt,
	}}
	secAuditSource := dispatcher.DispatchConfig{Vars: map[string]string{
		"scope_notes": defaultBotIssuePrompt,
	}}
	secAuditDeps := dispatcher.DispatchConfig{Vars: map[string]string{}}
	securedRenovacy := dispatcher.DispatchConfig{Vars: map[string]string{
		"user_prompt": defaultBotIssuePrompt,
	}}
	return map[string]dispatcher.DispatchConfig{
		"feature-dev":         featureDev,
		"feature_dev":         featureDev,
		"whole-improve-loop":  wholeImproveLoop,
		"whole_improve_loop":  wholeImproveLoop,
		"branch-improve-loop": branchImproveLoop,
		"branch_improve_loop": branchImproveLoop,
		"whats-next":          whatsNext,
		"whats_next":          whatsNext,
		"doc-align":           docAlign,
		"doc_align":           docAlign,
		"sec-audit-source":    secAuditSource,
		"sec_audit_source":    secAuditSource,
		"sec-audit-deps":      secAuditDeps,
		"sec_audit_deps":      secAuditDeps,
		"secured-renovacy":    securedRenovacy,
		"secured_renovacy":    securedRenovacy,
	}
}

// extractDefaultBots materialises the embedded catalogue under
// <storeDir>/dispatcher/bots/. Existing files are left untouched so an
// operator can hand-edit a bot in place without `iterion dispatch`
// overwriting it on restart. Returns the absolute path of the
// catalogue root + the number of top-level assignee directories
// observed in the embed (callers use the count to detect a binary
// that was built before `task templates:dispatch-bots` ran).
func extractDefaultBots(storeDir string) (string, int, error) {
	botsDir := filepath.Join(storeDir, "dispatcher", "bots")
	if err := os.MkdirAll(botsDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("dispatch defaults: mkdir %s: %w", botsDir, err)
	}

	rootPrefix := defaultBotsFSRoot + "/"
	assigneeDirs := 0
	walkErr := fs.WalkDir(defaultBotsFS, defaultBotsFSRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == defaultBotsFSRoot {
			return err
		}
		// Strip the embed root prefix so the on-disk layout matches
		// what BuildDefaultConfig expects: <botsDir>/<assignee>/...,
		// not <botsDir>/templates/dispatch_bots/<assignee>/...
		rel := strings.TrimPrefix(path, rootPrefix)
		if d.IsDir() {
			if !strings.ContainsRune(rel, '/') {
				assigneeDirs++
			}
			return os.MkdirAll(filepath.Join(botsDir, filepath.FromSlash(rel)), 0o755)
		}
		target := filepath.Join(botsDir, filepath.FromSlash(rel))
		if _, statErr := os.Stat(target); statErr == nil {
			// write-if-absent: preserve user edits.
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		data, readErr := defaultBotsFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("dispatch defaults: read embed %s: %w", path, readErr)
		}
		return os.WriteFile(target, data, 0o644)
	})
	if walkErr != nil {
		return "", 0, fmt.Errorf("dispatch defaults: extract: %w", walkErr)
	}
	return botsDir, assigneeDirs, nil
}

// BuildDefaultConfig returns the in-memory dispatcher configuration
// used by `iterion dispatch` when invoked without a YAML argument:
// native tracker, HTTP on [defaultDispatchPort], polling every 30 s,
// the embedded bot catalogue extracted under <storeDir>/dispatcher/
// bots, and the `default` fallback bot bound for unassigned issues.
//
// projectDir is the host git repository the dispatcher should seed
// per-issue workspaces from. When non-empty, an `after_create` hook
// is wired that runs `git worktree add` from projectDir into the
// freshly-created issue workspace, so bots see a populated checkout
// matching the host repo's HEAD instead of an empty directory. When
// empty (out-of-tree CLI invocations), the hook is omitted and the
// operator is expected to populate workspaces through a different
// mechanism (a custom hook, a bind-mount, or a workflow-level
// worktree: auto block).
//
// The returned Config has [Config.ApplyDefaults] already applied and
// has passed Validate, so callers can hand it straight to a Manager.
// SourcePath is left empty so downstream code (notably the
// ConfigWatcher) can tell baked-in mode apart from YAML-on-disk mode.
func BuildDefaultConfig(storeDir, projectDir string) (*dispatcher.Config, error) {
	if storeDir == "" {
		return nil, errors.New("dispatch defaults: storeDir is required")
	}

	botsDir, assigneeDirs, err := extractDefaultBots(storeDir)
	if err != nil {
		return nil, err
	}
	if assigneeDirs == 0 {
		return nil, errors.New("dispatch defaults: embedded bot catalogue is empty — this binary was compiled before `task templates:dispatch-bots` populated pkg/cli/templates/dispatch_bots/. Rebuild via `task build` (which depends on it) or run the sync task directly")
	}

	var hooks dispatcher.Hooks
	if projectDir != "" {
		// Resolve the project dir to an absolute path so the hook
		// is unaffected by the dispatcher daemon's cwd at the time
		// it fires (workers cd into the freshly-created workspace
		// before invoking the hook, so a relative path would be
		// relative to the empty workspace — wrong source).
		absProject, absErr := filepath.Abs(projectDir)
		if absErr == nil {
			projectDir = absProject
		}
		// after_create runs once per workspace, the first time the
		// dispatcher claims an issue. The dispatcher passes
		// ITERION_WORKSPACE in the env and sets cwd to it; the
		// shell hook seeds it as a git worktree of projectDir@HEAD
		// (or copies the tree when projectDir is not a git repo)
		// so the bot's workspace_dir input lands on a real
		// checkout matching the host state at dispatch time.
		hooks.AfterCreate = &dispatcher.Hook{
			Script: fmt.Sprintf(`set -e
PROJECT_DIR=%q
# If the workspace already has content (e.g. a previous failed
# attempt populated it before crashing), don't re-seed — let the
# operator clean up manually rather than silently overwrite.
if [ "$(ls -A "$ITERION_WORKSPACE" 2>/dev/null | head -c1)" != "" ]; then
  echo "workspace $ITERION_WORKSPACE non-empty — skipping seed"
  exit 0
fi
# Prefer a git worktree (cheap, shares object store with the host
# repo, isolates branches). Fall back to a recursive copy when
# PROJECT_DIR is not a git repository so out-of-tree projects
# still work.
if git -C "$PROJECT_DIR" rev-parse --git-dir >/dev/null 2>&1; then
  # Use detached HEAD so two parallel issues don't fight over the
  # same branch name. Bots that need a branch can create one with
  # workflow-level worktree: auto on top.
  git -C "$PROJECT_DIR" worktree add --detach "$ITERION_WORKSPACE" HEAD
  echo "seeded git worktree from $PROJECT_DIR@HEAD"
else
  cp -a "$PROJECT_DIR/." "$ITERION_WORKSPACE/"
  echo "seeded copy from $PROJECT_DIR"
fi
`, projectDir),
			TimeoutMS: 120_000,
		}
		// before_remove: clean up the git worktree registration on
		// the host repo before the workspace directory is deleted.
		// Without this, `git -C $PROJECT_DIR worktree list` would
		// accumulate stale entries pointing at gone directories
		// (the dispatcher's Workspaces.Remove only deletes the
		// directory, it doesn't talk to git).
		hooks.BeforeRemove = &dispatcher.Hook{
			Script: fmt.Sprintf(`set -e
PROJECT_DIR=%q
if git -C "$PROJECT_DIR" rev-parse --git-dir >/dev/null 2>&1; then
  git -C "$PROJECT_DIR" worktree remove --force "$ITERION_WORKSPACE" 2>/dev/null || true
fi
`, projectDir),
			TimeoutMS: 30_000,
		}
	}

	cfg := &dispatcher.Config{
		Name:     "iterion-default",
		Workflow: filepath.Join(botsDir, "default"),
		Tracker:  dispatcher.TrackerConfig{Kind: dispatcher.TrackerKindNative},
		Polling:  dispatcher.PollingConfig{IntervalMS: 30_000},
		Agent:    dispatcher.AgentConfig{MaxConcurrent: defaultDispatchMaxConcurrent},
		Server:   dispatcher.ServerConfig{Port: defaultDispatchPort},
		AssigneeWorkflows: map[string]string{
			// kebab-case canonical (matches the bot directory names
			// + the names a hand-written iterion.dispatcher.yaml
			// typically uses).
			"feature-dev":         filepath.Join(botsDir, "feature-dev"),
			"whole-improve-loop":  filepath.Join(botsDir, "whole-improve-loop"),
			"branch-improve-loop": filepath.Join(botsDir, "branch-improve-loop"),
			"whats-next":          filepath.Join(botsDir, "whats-next"),
			"doc-align":           filepath.Join(botsDir, "doc-align"),
			"sec-audit-source":    filepath.Join(botsDir, "sec-audit-source"),
			"sec-audit-deps":      filepath.Join(botsDir, "sec-audit-deps"),
			"secured-renovacy":    filepath.Join(botsDir, "secured-renovacy"),
			// snake_case aliases — what whats-next' assign_to_bots
			// emits (matching the Go pkg naming convention used in
			// examples/feature_dev/, examples/whole_improve_loop/).
			// Pre-aliased here so the dispatcher routes existing
			// snake_case assignees without the operator needing to
			// rename tickets or edit the config.
			"feature_dev":         filepath.Join(botsDir, "feature-dev"),
			"whole_improve_loop":  filepath.Join(botsDir, "whole-improve-loop"),
			"branch_improve_loop": filepath.Join(botsDir, "branch-improve-loop"),
			"whats_next":          filepath.Join(botsDir, "whats-next"),
			"doc_align":           filepath.Join(botsDir, "doc-align"),
			"sec_audit_source":    filepath.Join(botsDir, "sec-audit-source"),
			"sec_audit_deps":      filepath.Join(botsDir, "sec-audit-deps"),
			"secured_renovacy":    filepath.Join(botsDir, "secured-renovacy"),
		},
		AssigneeDispatch: defaultAssigneeDispatch(),
		Dispatch: dispatcher.DispatchConfig{
			Vars: map[string]string{
				"issue_title": "{{issue.title}}",
				"issue_body":  "{{issue.body}}",
				"issue_id":    "{{issue.identifier}}",
			},
		},
		Hooks: hooks,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dispatch defaults: validate built-in config: %w", err)
	}
	return cfg, nil
}

// DefaultAssigneeNames returns the canonical (kebab-case) assignee
// labels shipped in the embedded catalogue, sorted by name
// (deterministic output for the CLI banner). Snake_case aliases are
// folded out so the human-readable list stays one entry per bot.
// Exposed so the CLI can print the list at startup.
func DefaultAssigneeNames() []string {
	dispatch := defaultAssigneeDispatch()
	seen := make(map[string]struct{}, len(dispatch))
	names := make([]string, 0, len(dispatch)/2)
	for k := range dispatch {
		// Skip snake_case aliases — they share the same DispatchConfig
		// value as the kebab-case canonical entry.
		if strings.ContainsRune(k, '_') {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
