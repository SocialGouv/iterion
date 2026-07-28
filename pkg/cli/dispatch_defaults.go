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

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
)

// defaultBotsFS is the embedded catalogue of bots exposed as assignees
// by the zero-config `iterion dispatch` mode. The tree is regenerated
// from bots/ via `task templates:dispatch-bots`. Each top-level
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

// defaultDispatchPort is the studio HTTP surface for the no-arg mode.
// 4892 matches the example in docs/dispatcher.md so the published port
// stays stable across iterations.
const defaultDispatchPort = 4892

// defaultDispatchMaxConcurrent is intentionally conservative — the
// zero-config mode is meant for laptop / single-developer use, not
// fleet operation. Operators who need more should write a YAML.
const defaultDispatchMaxConcurrent = 2

// discoverAssigneeDispatch builds the per-assignee dispatch-var map by
// DISCOVERING the bots reachable via paths and reading each one's manifest
// `dispatch_vars` (botregistry.Entry.DispatchVars). It replaces the former
// hardcoded name→vars table: routing is handled by RoutingRunner via
// Bots.Paths, and the per-bot var wiring now travels with each bot's
// manifest — so adding/renaming a bot (shipped OR a custom one the operator
// drops in <projectDir>/bots) needs no edit here. Bots with no dispatch_vars
// receive only the global dispatch vars (issue_title/body/id).
//
// workspace_dir is INTENTIONALLY never bound — every bot defaults it to
// "${PROJECT_DIR}", which the runtime expands to the worktree the run's
// `worktree: auto` produces (and remaps for sandbox); overriding it to the
// pre-seed workspace path made the bot's prompt reference an unmounted dir
// and every Read tool call failed in cascade.
func discoverAssigneeDispatch(paths []string) (map[string]dispatcher.DispatchConfig, error) {
	entries, err := botregistry.List(botregistry.ListOptions{Paths: paths})
	if err != nil {
		return nil, fmt.Errorf("dispatch defaults: discover bots for dispatch vars: %w", err)
	}
	out := make(map[string]dispatcher.DispatchConfig, len(entries))
	for _, e := range entries {
		if len(e.DispatchVars) == 0 {
			continue
		}
		out[e.Name] = dispatcher.DispatchConfig{Vars: e.DispatchVars}
	}
	return out, nil
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
		// No destructive before_remove default: after removing an owned,
		// clean workspace the dispatcher deregisters that exact linked
		// worktree through the surviving host-repository metadata. A
		// shell-level `git worktree remove --force` before deletion would
		// reopen a commit-vs-remove race.
	}

	// Discover the bots so routing + per-bot dispatch vars come from each
	// bot's MANIFEST (dispatch_vars), not a hardcoded name→workflow/vars
	// map. Adding/renaming a bot — shipped (materialized at botsDir) OR a
	// custom one the operator drops in the project — is then a manifest
	// change with ZERO dispatcher-code edits. The project roots use the
	// canonical botregistry.DefaultPaths (bots/ + examples/ + .botz) — the
	// SAME set studio/server discover, so the CLI dispatcher and studio see
	// one catalog. botsDir (the embedded shipped catalogue) is prepended so
	// a shipped bot is never shadowed by a partial user override; missing
	// project roots are skipped by discovery (botregistry.List stats each).
	botsPaths := []string{botsDir}
	if projectDir != "" {
		botsPaths = append(botsPaths, botregistry.DefaultPaths(projectDir)...)
	}
	assigneeDispatch, err := discoverAssigneeDispatch(botsPaths)
	if err != nil {
		return nil, err
	}

	cfg := &dispatcher.Config{
		Name:     "iterion-default",
		Workflow: filepath.Join(botsDir, "default"),
		Tracker:  dispatcher.TrackerConfig{Kind: dispatcher.TrackerKindNative},
		Polling:  dispatcher.PollingConfig{IntervalMS: 30_000},
		Agent: dispatcher.AgentConfig{
			MaxConcurrent: defaultDispatchMaxConcurrent,
			// Move claimed issues to `in_progress` so the kanban
			// surfaces in-flight work. Matches native.StateInProgress;
			// duplicated as a literal to avoid importing native here
			// (Config.ApplyDefaults would also fill this in, but
			// stating it explicitly keeps the zero-config contract
			// obvious at the call site).
			RunningState: dispatcher.DefaultRunningState,
		},
		Server: dispatcher.ServerConfig{Port: defaultDispatchPort},
		// Discovery-driven routing: RoutingRunner resolves any ENABLED
		// discovered bot by name against Bots.Paths (HasRoute consults the
		// registry), so there is no hardcoded assignee_workflows map —
		// shipped + custom bots route identically. snake_case is no longer
		// special-cased: the catalog is kebab and Nexie emits kebab names.
		Bots:             botregistry.Config{Paths: botsPaths},
		AssigneeDispatch: assigneeDispatch,
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

// DefaultAssigneeNames returns the enabled bots discovered under paths,
// by name, sorted — the routable assignees for the CLI startup banner.
// Discovery-driven (no hardcoded list), so it reflects custom bots too.
func DefaultAssigneeNames(paths []string) []string {
	entries, err := botregistry.List(botregistry.ListOptions{Paths: paths})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}
