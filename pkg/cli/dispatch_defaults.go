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
func defaultAssigneeDispatch() map[string]dispatcher.DispatchConfig {
	return map[string]dispatcher.DispatchConfig{
		"feature-dev": {Vars: map[string]string{
			"workspace_dir":  "{{dispatcher.workspace_path}}",
			"feature_prompt": defaultBotIssuePrompt,
		}},
		"whole-improve-loop": {Vars: map[string]string{
			"workspace_dir":      "{{dispatcher.workspace_path}}",
			"improvement_prompt": defaultBotIssuePrompt,
			"scope_notes":        "{{issue.body}}",
		}},
		"branch-improve-loop": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"scope_notes":   defaultBotIssuePrompt,
		}},
		"whats-next": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"scope_notes":   defaultBotIssuePrompt,
		}},
		"doc-align": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"scope_notes":   defaultBotIssuePrompt,
		}},
		"sec-audit-source": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"scope_notes":   defaultBotIssuePrompt,
		}},
		"sec-audit-deps": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
		}},
		"secured-renovacy": {Vars: map[string]string{
			"workspace_dir": "{{dispatcher.workspace_path}}",
			"user_prompt":   defaultBotIssuePrompt,
		}},
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
// The returned Config has [Config.ApplyDefaults] already applied and
// has passed Validate, so callers can hand it straight to a Manager.
// SourcePath is left empty so downstream code (notably the
// ConfigWatcher) can tell baked-in mode apart from YAML-on-disk mode.
func BuildDefaultConfig(storeDir string) (*dispatcher.Config, error) {
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

	cfg := &dispatcher.Config{
		Name:     "iterion-default",
		Workflow: filepath.Join(botsDir, "default"),
		Tracker:  dispatcher.TrackerConfig{Kind: dispatcher.TrackerKindNative},
		Polling:  dispatcher.PollingConfig{IntervalMS: 30_000},
		Agent:    dispatcher.AgentConfig{MaxConcurrent: defaultDispatchMaxConcurrent},
		Server:   dispatcher.ServerConfig{Port: defaultDispatchPort},
		AssigneeWorkflows: map[string]string{
			"feature-dev":         filepath.Join(botsDir, "feature-dev"),
			"whole-improve-loop":  filepath.Join(botsDir, "whole-improve-loop"),
			"branch-improve-loop": filepath.Join(botsDir, "branch-improve-loop"),
			"whats-next":          filepath.Join(botsDir, "whats-next"),
			"doc-align":           filepath.Join(botsDir, "doc-align"),
			"sec-audit-source":    filepath.Join(botsDir, "sec-audit-source"),
			"sec-audit-deps":      filepath.Join(botsDir, "sec-audit-deps"),
			"secured-renovacy":    filepath.Join(botsDir, "secured-renovacy"),
		},
		AssigneeDispatch: defaultAssigneeDispatch(),
		Dispatch: dispatcher.DispatchConfig{
			Vars: map[string]string{
				"issue_title": "{{issue.title}}",
				"issue_body":  "{{issue.body}}",
				"issue_id":    "{{issue.identifier}}",
			},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dispatch defaults: validate built-in config: %w", err)
	}
	return cfg, nil
}

// DefaultAssigneeNames returns the assignee labels currently shipped
// in the embedded catalogue, sorted by name (deterministic output for
// the CLI banner). Exposed so the CLI can print the list at startup.
func DefaultAssigneeNames() []string {
	dispatch := defaultAssigneeDispatch()
	names := make([]string, 0, len(dispatch))
	for k := range dispatch {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
