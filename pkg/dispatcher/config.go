package dispatcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v2"
)

// Config is the parsed dispatcher configuration. Sources:
//   - YAML on disk (`iterion.dispatcher.yaml`) — for hand-edited or
//     git-versioned setups (consumed by `iterion dispatch <yaml>`).
//   - JSON via the studio SPA's PUT /api/v1/dispatcher/config endpoint
//     (persisted as <store-dir>/dispatcher/dispatcher.json).
//
// Both representations decode into the same struct — JSON and YAML
// field names match (snake_case throughout).
//
// SourcePath is the resolved absolute path of the file on disk and is
// set by the loader (not parsed from the wire format).
type Config struct {
	Name              string                    `yaml:"name,omitempty" json:"name,omitempty"`
	Workflow          string                    `yaml:"workflow" json:"workflow"`
	AssigneeWorkflows map[string]string         `yaml:"assignee_workflows,omitempty" json:"assignee_workflows,omitempty"`
	AssigneeDispatch  map[string]DispatchConfig `yaml:"assignee_dispatch,omitempty" json:"assignee_dispatch,omitempty"`
	Tracker           TrackerConfig             `yaml:"tracker" json:"tracker"`
	Dispatch          DispatchConfig            `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`
	Polling           PollingConfig             `yaml:"polling,omitempty" json:"polling,omitempty"`
	Agent             AgentConfig               `yaml:"agent,omitempty" json:"agent,omitempty"`
	Workspace         WorkspaceConfig           `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Hooks             Hooks                     `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	Stall             StallConfig               `yaml:"stall,omitempty" json:"stall,omitempty"`
	Server            ServerConfig              `yaml:"server,omitempty" json:"server,omitempty"`

	SourcePath string `yaml:"-" json:"-"`
}

// TrackerConfig is the discriminated tracker definition. Kind selects
// which sibling block (Native, GitHub, Forgejo) is consulted.
type TrackerConfig struct {
	Kind    TrackerKind           `yaml:"kind" json:"kind"`
	Native  *NativeTrackerConfig  `yaml:"native,omitempty" json:"native,omitempty"`
	GitHub  *GitHubTrackerConfig  `yaml:"github,omitempty" json:"github,omitempty"`
	Forgejo *ForgejoTrackerConfig `yaml:"forgejo,omitempty" json:"forgejo,omitempty"`
}

// TrackerKind is the typed discriminator for TrackerConfig.Kind. Using
// constants instead of raw strings catches typos at compile-time and
// makes adding a new adapter a search-and-add affair.
type TrackerKind string

const (
	TrackerKindNative  TrackerKind = "native"
	TrackerKindGitHub  TrackerKind = "github"
	TrackerKindForgejo TrackerKind = "forgejo"
)

// NativeTrackerConfig is intentionally empty in v1 — the native store
// is configured via board.json, not here.
type NativeTrackerConfig struct{}

// LabelSelector is the shared shape for label-driven state mappings.
// It mirrors tracker.LabelSelector with YAML tags so it can be parsed
// from iterion.dispatcher.yaml without the tracker package needing YAML
// tags.
type LabelSelector struct {
	LabelsInclude []string `yaml:"labels_include,omitempty" json:"labels_include,omitempty"`
	LabelsExclude []string `yaml:"labels_exclude,omitempty" json:"labels_exclude,omitempty"`
}

// GitHubTrackerConfig configures the github_issues adapter.
type GitHubTrackerConfig struct {
	Repo          string                   `yaml:"repo" json:"repo"`
	Token         string                   `yaml:"token,omitempty" json:"token,omitempty"`
	StateMapping  map[string]LabelSelector `yaml:"state_mapping,omitempty" json:"state_mapping,omitempty"`
	ClaimedLabel  string                   `yaml:"claimed_label,omitempty" json:"claimed_label,omitempty"`
	IncludeLabels []string                 `yaml:"include_labels,omitempty" json:"include_labels,omitempty"`
	ExcludeLabels []string                 `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
}

// ForgejoTrackerConfig configures the forgejo (Gitea-compatible) adapter.
type ForgejoTrackerConfig struct {
	Host          string                   `yaml:"host" json:"host"`
	Repo          string                   `yaml:"repo" json:"repo"`
	Token         string                   `yaml:"token,omitempty" json:"token,omitempty"`
	StateMapping  map[string]LabelSelector `yaml:"state_mapping,omitempty" json:"state_mapping,omitempty"`
	ClaimedLabel  string                   `yaml:"claimed_label,omitempty" json:"claimed_label,omitempty"`
	IncludeLabels []string                 `yaml:"include_labels,omitempty" json:"include_labels,omitempty"`
	ExcludeLabels []string                 `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
}

// DispatchConfig describes how issue fields flow into a workflow run.
// Vars maps workflow `vars:` names to template strings using
// {{issue.x}} / {{dispatcher.y}} references.
type DispatchConfig struct {
	Vars        map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	Attachments map[string]string `yaml:"attachments,omitempty" json:"attachments,omitempty"`
}

// PollingConfig sets the tick cadence.
type PollingConfig struct {
	IntervalMS int `yaml:"interval_ms,omitempty" json:"interval_ms,omitempty"`
}

// AgentConfig sets concurrency and retry caps.
type AgentConfig struct {
	MaxConcurrent        int            `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
	MaxConcurrentByState map[string]int `yaml:"max_concurrent_by_state,omitempty" json:"max_concurrent_by_state,omitempty"`
	MaxTurns             int            `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxRetryBackoffMS    int            `yaml:"max_retry_backoff_ms,omitempty" json:"max_retry_backoff_ms,omitempty"`
}

// WorkspaceConfig controls where per-issue workspaces live.
type WorkspaceConfig struct {
	Root    string                 `yaml:"root,omitempty" json:"root,omitempty"`
	Persist WorkspacePersistPolicy `yaml:"persist,omitempty" json:"persist,omitempty"`
}

// WorkspacePersistPolicy decides whether per-issue workspaces are
// cleaned up after a successful dispatch.
type WorkspacePersistPolicy string

const (
	// WorkspacePersistKeep keeps every workspace forever (the default
	// when the field is empty). Operators clean up manually.
	WorkspacePersistKeep WorkspacePersistPolicy = "keep"
	// WorkspacePersistCleanupOnDone removes the workspace on a clean
	// dispatch return (err == nil from the runner). Failed / cancelled
	// dispatches retain the workspace so retries can resume from it.
	WorkspacePersistCleanupOnDone WorkspacePersistPolicy = "cleanup_on_done"
	// WorkspacePersistCleanupOnTerminal removes the workspace on a
	// clean dispatch return when the issue has reached a terminal
	// tracker state. v1 treats it the same as cleanup_on_done; v2
	// will branch on the post-run state.
	WorkspacePersistCleanupOnTerminal WorkspacePersistPolicy = "cleanup_on_terminal"
)

// shouldCleanupOnSuccess reports whether a clean dispatch return
// should trigger workspace removal.
func (p WorkspacePersistPolicy) shouldCleanupOnSuccess() bool {
	switch p {
	case WorkspacePersistCleanupOnDone, WorkspacePersistCleanupOnTerminal:
		return true
	}
	return false
}

// StallConfig is the inactivity timeout for running issues.
type StallConfig struct {
	TimeoutMS int `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
}

// ServerConfig opens the dispatcher's HTTP/WS surface.
type ServerConfig struct {
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
}

// Defaults applied to optional fields after parse.
const (
	DefaultPollingInterval     = 30_000
	DefaultMaxConcurrent       = 4
	DefaultMaxTurns            = 20
	DefaultMaxRetryBackoffMS   = 300_000
	DefaultStallTimeoutMS      = 600_000
	DefaultGitHubClaimedLabel  = "iterion-claimed"
	DefaultForgejoClaimedLabel = "iterion-claimed"
)

// Load reads and parses a dispatcher config from path. Relative paths
// inside the config (workflow, workspace.root, hooks.path) are resolved
// against the config file's directory. Environment variable references
// like "$GITHUB_TOKEN" or "${GITHUB_TOKEN}" are expanded in string
// fields that accept secrets. The returned config has defaults applied.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", abs, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", abs, err)
	}
	cfg.SourcePath = abs
	cfg.applyEnvAndPaths()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Polling.IntervalMS <= 0 {
		// Treat 0 and negative the same — both would later trip
		// ticker.Reset's "non-positive interval" panic. Hot-reload
		// callers depend on this floor.
		c.Polling.IntervalMS = DefaultPollingInterval
	}
	if c.Agent.MaxConcurrent == 0 {
		c.Agent.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.Agent.MaxTurns == 0 {
		c.Agent.MaxTurns = DefaultMaxTurns
	}
	if c.Agent.MaxRetryBackoffMS == 0 {
		c.Agent.MaxRetryBackoffMS = DefaultMaxRetryBackoffMS
	}
	if c.Stall.TimeoutMS == 0 {
		c.Stall.TimeoutMS = DefaultStallTimeoutMS
	}
	if c.Tracker.Kind == TrackerKindGitHub && c.Tracker.GitHub != nil && c.Tracker.GitHub.ClaimedLabel == "" {
		c.Tracker.GitHub.ClaimedLabel = DefaultGitHubClaimedLabel
	}
	if c.Tracker.Kind == TrackerKindForgejo && c.Tracker.Forgejo != nil && c.Tracker.Forgejo.ClaimedLabel == "" {
		c.Tracker.Forgejo.ClaimedLabel = DefaultForgejoClaimedLabel
	}
}

func (c *Config) applyEnvAndPaths() {
	dir := filepath.Dir(c.SourcePath)
	c.Workflow = resolveRelPath(dir, expandEnv(c.Workflow))
	for assignee, wfPath := range c.AssigneeWorkflows {
		c.AssigneeWorkflows[assignee] = resolveRelPath(dir, expandEnv(wfPath))
	}
	if c.Workspace.Root != "" {
		c.Workspace.Root = expandHome(expandEnv(c.Workspace.Root))
		if !filepath.IsAbs(c.Workspace.Root) {
			c.Workspace.Root = filepath.Join(dir, c.Workspace.Root)
		}
	}
	c.Hooks.AfterCreate = expandHookPaths(dir, c.Hooks.AfterCreate)
	c.Hooks.BeforeRun = expandHookPaths(dir, c.Hooks.BeforeRun)
	c.Hooks.AfterRun = expandHookPaths(dir, c.Hooks.AfterRun)
	c.Hooks.BeforeRemove = expandHookPaths(dir, c.Hooks.BeforeRemove)

	if c.Tracker.GitHub != nil {
		c.Tracker.GitHub.Token = expandEnv(c.Tracker.GitHub.Token)
	}
	if c.Tracker.Forgejo != nil {
		c.Tracker.Forgejo.Token = expandEnv(c.Tracker.Forgejo.Token)
	}
}

// Validate checks fields are coherent. Workflow file existence is
// checked here; deeper compile-time validation of the .iter is the
// caller's responsibility (typically performed by Dispatcher.Start).
func (c *Config) Validate() error {
	if c.Workflow == "" {
		return errors.New("config: workflow is required")
	}
	if _, err := os.Stat(c.Workflow); err != nil {
		return fmt.Errorf("config: workflow %s: %w", c.Workflow, err)
	}
	for assignee, wfPath := range c.AssigneeWorkflows {
		if assignee == "" {
			return errors.New("config: assignee_workflows contains an empty key")
		}
		if wfPath == "" {
			return fmt.Errorf("config: assignee_workflows[%q] is empty", assignee)
		}
		if _, err := os.Stat(wfPath); err != nil {
			return fmt.Errorf("config: assignee_workflows[%q] %s: %w", assignee, wfPath, err)
		}
	}
	switch c.Tracker.Kind {
	case "":
		return errors.New("config: tracker.kind is required (native | github | forgejo)")
	case TrackerKindNative:
		// nothing to validate beyond presence
	case TrackerKindGitHub:
		if c.Tracker.GitHub == nil {
			return errors.New("config: tracker.kind=github requires tracker.github block")
		}
		if c.Tracker.GitHub.Repo == "" {
			return errors.New("config: tracker.github.repo is required")
		}
		if !githubRepoRe.MatchString(c.Tracker.GitHub.Repo) {
			return fmt.Errorf("config: tracker.github.repo %q must be owner/repo", c.Tracker.GitHub.Repo)
		}
	case TrackerKindForgejo:
		if c.Tracker.Forgejo == nil {
			return errors.New("config: tracker.kind=forgejo requires tracker.forgejo block")
		}
		if c.Tracker.Forgejo.Host == "" || c.Tracker.Forgejo.Repo == "" {
			return errors.New("config: tracker.forgejo requires host and repo")
		}
	default:
		return fmt.Errorf("config: tracker.kind %q not supported (native | github | forgejo)", c.Tracker.Kind)
	}
	if err := c.Hooks.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if c.Workspace.Persist != "" {
		switch c.Workspace.Persist {
		case WorkspacePersistKeep, WorkspacePersistCleanupOnDone, WorkspacePersistCleanupOnTerminal:
		default:
			return fmt.Errorf("config: workspace.persist %q invalid", c.Workspace.Persist)
		}
	}
	if c.Agent.MaxConcurrent < 1 {
		return errors.New("config: agent.max_concurrent must be >= 1")
	}
	for state, cap := range c.Agent.MaxConcurrentByState {
		if cap < 0 {
			return fmt.Errorf("config: agent.max_concurrent_by_state[%s] must be >= 0", state)
		}
	}
	for k, v := range c.Dispatch.Vars {
		if _, err := ParseTemplate(v); err != nil {
			return fmt.Errorf("config: dispatch.vars[%s]: %w", k, err)
		}
	}
	for k, v := range c.Dispatch.Attachments {
		if _, err := ParseTemplate(v); err != nil {
			return fmt.Errorf("config: dispatch.attachments[%s]: %w", k, err)
		}
	}
	for assignee, dc := range c.AssigneeDispatch {
		if assignee == "" {
			return errors.New("config: assignee_dispatch contains an empty key")
		}
		if _, ok := c.AssigneeWorkflows[assignee]; !ok {
			return fmt.Errorf("config: assignee_dispatch[%q] has no matching assignee_workflows entry", assignee)
		}
		for k, v := range dc.Vars {
			if _, err := ParseTemplate(v); err != nil {
				return fmt.Errorf("config: assignee_dispatch[%q].vars[%s]: %w", assignee, k, err)
			}
		}
		for k, v := range dc.Attachments {
			if _, err := ParseTemplate(v); err != nil {
				return fmt.Errorf("config: assignee_dispatch[%q].attachments[%s]: %w", assignee, k, err)
			}
		}
	}
	return nil
}

// ApplyDefaults exposes the package-private default application so
// callers that build a Config in code (e.g. cli.BuildDefaultConfig for
// the no-arg `iterion dispatch` mode) can normalise zero fields the
// same way Load does on disk.
func (c *Config) ApplyDefaults() { c.applyDefaults() }

// PollingInterval returns the polling cadence as a time.Duration after
// defaults are applied.
func (c *Config) PollingInterval() time.Duration {
	return time.Duration(c.Polling.IntervalMS) * time.Millisecond
}

// MaxRetryBackoff returns the retry backoff cap as a time.Duration.
func (c *Config) MaxRetryBackoff() time.Duration {
	return time.Duration(c.Agent.MaxRetryBackoffMS) * time.Millisecond
}

// StallTimeout returns the stall timeout as a time.Duration. Zero
// disables stall detection.
func (c *Config) StallTimeout() time.Duration {
	if c.Stall.TimeoutMS <= 0 {
		return 0
	}
	return time.Duration(c.Stall.TimeoutMS) * time.Millisecond
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func expandEnv(s string) string {
	if s == "" {
		return s
	}
	return os.Expand(s, func(name string) string {
		return os.Getenv(name)
	})
}

func expandHome(s string) string {
	if !strings.HasPrefix(s, "~") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, strings.TrimPrefix(s, "~"))
}

func resolveRelPath(dir, p string) string {
	if p == "" {
		return p
	}
	p = expandHome(p)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func expandHookPaths(dir string, h *Hook) *Hook {
	if h == nil || h.Path == "" {
		return h
	}
	out := *h
	out.Path = resolveRelPath(dir, h.Path)
	return &out
}
