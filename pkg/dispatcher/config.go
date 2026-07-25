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

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
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
	Limits            LimitsConfig              `yaml:"limits,omitempty" json:"limits,omitempty"`
	Server            ServerConfig              `yaml:"server,omitempty" json:"server,omitempty"`
	// Bots configures the registry the dispatcher consults when a
	// ticket carries a per-ticket Bot override. Paths are walked
	// using pkg/botregistry's discovery rules (single .bot files +
	// .botz bundles). Empty Paths disables the override path — a
	// ticket with iss.Bot set on it will fail to dispatch.
	Bots botregistry.Config `yaml:"bots,omitempty" json:"bots,omitempty"`

	// TicketRouter, when Enabled, routes an UNASSIGNED new issue
	// deterministically BEFORE the normal bot/assignee resolution: an issue a PR
	// already links is left to the inbound PR-webhook (which runs the
	// branch-improve bot on the PR branch) — the dispatcher steps aside to avoid
	// a second, branch-less (empty-diff) run, the ticket<->PR dedup; an issue
	// with no linked PR routes to ImplementBot (feature-dev by default) to
	// implement it. Off by default: existing dispatchers keep running the
	// default workflow for unassigned issues. See docs/dispatcher.md.
	TicketRouter TicketRouterConfig `yaml:"ticket_router,omitempty" json:"ticket_router,omitempty"`

	SourcePath string `yaml:"-" json:"-"`
}

// TicketRouterConfig gates + tunes the deterministic ticket router. It is an
// opt-in (Enabled=false by default). The PR-existence check + the visible
// bot:featurly/bot:billy label are best-effort tracker capabilities: a tracker
// that can't answer "does this issue have a linked PR?" degrades to routing
// every unassigned issue to the implement bot (never blocks, never dedups
// against a PR-webhook it can't observe).
type TicketRouterConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ImplementBot is the bot a PR-less unassigned issue routes to. Empty =
	// "feature-dev" (Featurly). It must be resolvable (bots.paths /
	// assignee_workflows) or dispatch honest-fails on it.
	ImplementBot string `yaml:"implement_bot,omitempty" json:"implement_bot,omitempty"`
}

// ImplementBotOrDefault returns the configured implement bot, or "feature-dev".
func (t TicketRouterConfig) ImplementBotOrDefault() string {
	if t.ImplementBot != "" {
		return t.ImplementBot
	}
	return "feature-dev"
}

// TrackerConfig is the discriminated tracker definition. Kind selects
// which sibling block (Native, GitHub, Forgejo) is consulted.
type TrackerConfig struct {
	Kind    TrackerKind           `yaml:"kind" json:"kind"`
	Native  *NativeTrackerConfig  `yaml:"native,omitempty" json:"native,omitempty"`
	GitHub  *GitHubTrackerConfig  `yaml:"github,omitempty" json:"github,omitempty"`
	Forgejo *ForgejoTrackerConfig `yaml:"forgejo,omitempty" json:"forgejo,omitempty"`
	GitLab  *GitLabTrackerConfig  `yaml:"gitlab,omitempty" json:"gitlab,omitempty"`
}

// TrackerKind is the typed discriminator for TrackerConfig.Kind. Using
// constants instead of raw strings catches typos at compile-time and
// makes adding a new adapter a search-and-add affair.
type TrackerKind string

const (
	TrackerKindNative  TrackerKind = "native"
	TrackerKindGitHub  TrackerKind = "github"
	TrackerKindForgejo TrackerKind = "forgejo"
	TrackerKindGitLab  TrackerKind = "gitlab"
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
	// AuthorAllowlist restricts dispatch to issues OPENED BY one of these
	// logins (case-insensitive; e.g. the repo's trusted team members). Empty =
	// any author. The anti-abuse scope for auto-dispatch on a public repo — a
	// drive-by issue from an untrusted author is never auto-processed.
	AuthorAllowlist []string `yaml:"author_allowlist,omitempty" json:"author_allowlist,omitempty"`
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

// GitLabTrackerConfig configures the gitlab (v4 REST API) adapter.
type GitLabTrackerConfig struct {
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
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// Attachments is retained ONLY so Config.Validate can detect it and
	// fail fast with a clear message — the dispatcher has no path to
	// inject per-issue attachments (see unsupportedAttachmentsErr and
	// docs/adr/013-dispatcher-attachments-unsupported.md). The YAML
	// decoder is non-strict (Load uses yaml.Unmarshal, not strict
	// decoding), so the field MUST exist to be detectable: removing it
	// would make a stray `attachments:` key parse-and-vanish silently,
	// reintroducing the very context-loss bug this guards against.
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

	// MaxAttempts caps the TOTAL number of dispatch attempts (the initial
	// run plus retries) for one issue. When a run fails and this many
	// attempts have been made, the dispatcher stops retrying and moves the
	// issue to FailedState instead of rescheduling forever. The previous
	// behaviour retried indefinitely (bounded only by the optional daily
	// spend cap — disabled by default), silently bouncing a doomed ticket
	// between its source and running states and burning model spend, with
	// the failure visible only in the dispatcher dashboard. Default
	// DefaultMaxAttempts; set a negative value to retry forever (the legacy
	// unbounded behaviour).
	MaxAttempts int `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`

	// RunningState, when non-empty, is the kanban state the dispatcher
	// transitions a claimed issue to after a successful Claim. Empty
	// disables the transition (escape hatch for boards without an
	// in-flight column). On cancel or pre-run failure the dispatcher
	// reverts to the issue's original state. On normal finish the
	// workflow itself sets the next state (review/done/etc).
	RunningState string `yaml:"running_state,omitempty" json:"running_state,omitempty"`

	// CompletedState is the kanban state the dispatcher moves a
	// cleanly-finished issue into when the workflow itself didn't move
	// it out of RunningState. Without this auto-transition, a workflow
	// that lacks board.move capability (the catch-all dispatcher_default
	// fallback when an issue has no assignee, for example) finishes,
	// the dispatcher releases the claim, and the issue stays in
	// RunningState. With RunningState marked eligible:true on the
	// board (the default, needed for crash-recovery re-dispatch), the
	// next poll picks the same issue back up and the workflow runs in
	// a tight loop — burning model spend.
	//
	// Default: "review" — matches the default board's review column.
	// Set to "none" (or any state name the board doesn't define) to
	// disable; on an unknown state the dispatcher logs a warning and
	// leaves the issue in RunningState (back to pre-fix behaviour).
	CompletedState string `yaml:"completed_state,omitempty" json:"completed_state,omitempty"`

	// MergedState is the kanban state the dispatcher moves an issue
	// into when the operator squash/merges the run's commits via the
	// studio (POST /api/runs/{id}/merge). The transition fires only
	// when the run has a Source.IssueID (= dispatcher-spawned) and
	// the merge succeeds. Analog of GitHub's "close issue on PR merge"
	// — review queues can flow review → done without the operator
	// touching the kanban after every merge click.
	//
	// Empty in YAML → defaults to "done" (see DefaultMergedState) so a
	// merged run closes its ticket automatically. "none" is the explicit
	// opt-out (issue stays in CompletedState — typically "review" — until
	// manually moved). An unknown state name logs a warning and is a no-op.
	MergedState string `yaml:"merged_state,omitempty" json:"merged_state,omitempty"`

	// FailedState is the kanban state an issue moves to when its retries
	// are exhausted (see MaxAttempts). Default "blocked" (a terminal column
	// on the default board) so a give-up is visible on the board and the
	// issue stops being eligible for re-dispatch. "none" disables the move;
	// when the move is unavailable (board lacks the state, tracker rejects
	// it) the dispatcher logs and falls back to the legacy retry behaviour
	// rather than stranding the issue, so the cap never silently drops work.
	FailedState string `yaml:"failed_state,omitempty" json:"failed_state,omitempty"`
}

// LimitsConfig holds operator-facing spend guardrails. Currently just
// the per-(store, UTC-day) LLM spend cap; structured as a block so
// future limits (per-issue cost, monthly cap) slot in without a config
// migration.
type LimitsConfig struct {
	// MaxCostPerDayUSD caps cumulative LLM spend across all dispatcher
	// runs for a UTC calendar day. When the day's spend reaches the cap,
	// the dispatcher stops launching new work and every running run
	// pauses (resumable) at its next node boundary. 0 (the default)
	// disables the cap. Overridable for the current day from the studio;
	// auto-resets at the next UTC day.
	MaxCostPerDayUSD float64 `yaml:"max_cost_per_day_usd,omitempty" json:"max_cost_per_day_usd,omitempty"`
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
	// WorkspacePersistCleanupOnDone requests verified workspace cleanup on a
	// clean dispatch return (err == nil from the runner). An inconclusive
	// durability/ownership proof preserves it. Failed / cancelled dispatches
	// retain the workspace so retries can resume from it.
	WorkspacePersistCleanupOnDone WorkspacePersistPolicy = "cleanup_on_done"
	// WorkspacePersistCleanupOnTerminal requests verified cleanup on a clean
	// dispatch return when the issue has reached a terminal tracker state. v1
	// treats it the same as cleanup_on_done; v2 will branch on post-run state.
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
	DefaultGitLabClaimedLabel  = "iterion-claimed"
	// DefaultRunningState mirrors native.StateInProgress; duplicated here
	// as a string literal to keep pkg/dispatcher/config.go free of the
	// native package import (config.go is loaded before the native store
	// is constructed).
	DefaultRunningState = "in_progress"
	// DefaultCompletedState matches the default board's review column.
	// The auto-transition only fires when the workflow finished cleanly
	// without moving the issue out of RunningState (typically because
	// the workflow lacks board.move capability — the catch-all
	// dispatcher_default fallback is the prime culprit) and an
	// unrecognised target is treated as "leave the issue alone" so
	// operators with custom boards aren't forced to opt out explicitly.
	DefaultCompletedState = "review"

	// DefaultMergedState is the kanban state a dispatcher-spawned issue
	// moves to when its run is merged from the studio (review → merged →
	// done is the canonical lifecycle). Without a default, merged issues
	// would sit in CompletedState ("review") forever, forcing the
	// operator to close each one by hand. "done" is assumed to exist as a
	// terminal column; UpdateState failures are logged + non-fatal, so
	// boards without it degrade gracefully. Opt out with
	// `merged_state: none`.
	DefaultMergedState = "done"

	// DefaultMaxAttempts bounds retries so a deterministically-failing
	// issue (bad/missing bot, schema mismatch, missing dependency) can't
	// retry forever. With the default 5-minute backoff cap this is on the
	// order of tens of minutes of retries before giving up — enough to ride
	// out a transient provider outage, bounded enough to stop burning spend
	// on a doomed ticket.
	DefaultMaxAttempts = 10

	// DefaultFailedState is the terminal column a give-up moves an issue
	// into. "blocked" exists on the default board (native.StateBlocked,
	// duplicated as a literal to keep this file free of the native import —
	// same convention as DefaultRunningState).
	DefaultFailedState = "blocked"
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
	// RunningState/CompletedState/MergedState/FailedState share one
	// convention via defaultOrNone: empty in YAML/JSON → the field's
	// default; the literal "none" → "" (the explicit opt-out the rest of
	// the dispatcher reads as the disabled zero value).
	//   - RunningState   → in_progress: moves claimed issues out of "ready".
	//   - CompletedState → review:      post-success auto-transition target.
	//   - MergedState    → done:        studio-merged run auto-closes its ticket.
	//   - FailedState    → blocked:     give-up target once retries exhaust.
	c.Agent.RunningState = defaultOrNone(c.Agent.RunningState, DefaultRunningState)
	c.Agent.CompletedState = defaultOrNone(c.Agent.CompletedState, DefaultCompletedState)
	c.Agent.MergedState = defaultOrNone(c.Agent.MergedState, DefaultMergedState)
	c.Agent.FailedState = defaultOrNone(c.Agent.FailedState, DefaultFailedState)
	// MaxAttempts: 0 (unset) → finite default so retries can't run forever;
	// a negative value is the explicit "retry indefinitely" escape hatch,
	// mapped to 0 which the exhausted() give-up gate reads as "no cap".
	if c.Agent.MaxAttempts == 0 {
		c.Agent.MaxAttempts = DefaultMaxAttempts
	} else if c.Agent.MaxAttempts < 0 {
		c.Agent.MaxAttempts = 0
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
	if c.Tracker.Kind == TrackerKindGitLab && c.Tracker.GitLab != nil && c.Tracker.GitLab.ClaimedLabel == "" {
		c.Tracker.GitLab.ClaimedLabel = DefaultGitLabClaimedLabel
	}
}

// defaultOrNone applies the shared kanban-state convention used by
// RunningState/CompletedState/MergedState/FailedState: an empty value
// takes def; the literal "none" is the explicit opt-out mapped to ""
// (the disabled zero value the rest of the dispatcher reads).
func defaultOrNone(v, def string) string {
	switch v {
	case "":
		return def
	case "none":
		return ""
	default:
		return v
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
	if c.Tracker.GitLab != nil {
		c.Tracker.GitLab.Token = expandEnv(c.Tracker.GitLab.Token)
	}
}

// Validate checks fields are coherent. Workflow file existence is
// checked here; deeper compile-time validation of the workflow is the
// caller's responsibility (typically performed by Dispatcher.Start).
func (c *Config) Validate() error {
	if c.Workflow == "" {
		return errors.New("config: workflow is required")
	}
	if _, err := bundle.Detect(c.Workflow); err != nil {
		return fmt.Errorf("config: workflow %s: %w", c.Workflow, err)
	}
	for assignee, wfPath := range c.AssigneeWorkflows {
		if assignee == "" {
			return errors.New("config: assignee_workflows contains an empty key")
		}
		if wfPath == "" {
			return fmt.Errorf("config: assignee_workflows[%q] is empty", assignee)
		}
		if _, err := bundle.Detect(wfPath); err != nil {
			return fmt.Errorf("config: assignee_workflows[%q] %s: %w", assignee, wfPath, err)
		}
	}
	switch c.Tracker.Kind {
	case "":
		return errors.New("config: tracker.kind is required (native | github | forgejo | gitlab)")
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
	case TrackerKindGitLab:
		if c.Tracker.GitLab == nil {
			return errors.New("config: tracker.kind=gitlab requires tracker.gitlab block")
		}
		if c.Tracker.GitLab.Host == "" || c.Tracker.GitLab.Repo == "" {
			return errors.New("config: tracker.gitlab requires host and repo")
		}
	default:
		return fmt.Errorf("config: tracker.kind %q not supported (native | github | forgejo | gitlab)", c.Tracker.Kind)
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
	if len(c.Dispatch.Attachments) > 0 {
		return errors.New(unsupportedAttachmentsErr("dispatch.attachments"))
	}
	// Pre-walk discovery ONCE so each assignee_dispatch resolvability check
	// below is an O(1) set lookup, not an O(N) re-walk of the bots tree per
	// entry (botregistry.ResolveBotPath does a full List per call).
	var discoverable map[string]struct{}
	if len(c.Bots.Paths) > 0 {
		if entries, derr := botregistry.List(botregistry.ListOptions{Paths: c.Bots.Paths}); derr == nil {
			discoverable = make(map[string]struct{}, len(entries))
			for _, e := range entries {
				discoverable[botregistry.NormalizeName(e.Name)] = struct{}{}
			}
		}
	}
	for assignee, dc := range c.AssigneeDispatch {
		if assignee == "" {
			return errors.New("config: assignee_dispatch contains an empty key")
		}
		if _, ok := c.AssigneeWorkflows[assignee]; !ok {
			// With discovery configured, an assignee_dispatch entry is valid
			// when the bot resolves via the registry (Bots.Paths) — the stock
			// config derives BOTH routing and these vars from discovery, so
			// there is no assignee_workflows literal to match against. Only
			// when neither a static route NOR a discoverable bot exists is
			// the entry dangling.
			if _, ok := discoverable[botregistry.NormalizeName(assignee)]; !ok {
				return fmt.Errorf("config: assignee_dispatch[%q] has no matching assignee_workflows entry and is not a discoverable bot under bots.paths", assignee)
			}
		}
		for k, v := range dc.Vars {
			if _, err := ParseTemplate(v); err != nil {
				return fmt.Errorf("config: assignee_dispatch[%q].vars[%s]: %w", assignee, k, err)
			}
		}
		if len(dc.Attachments) > 0 {
			return errors.New(unsupportedAttachmentsErr(fmt.Sprintf("assignee_dispatch[%q].attachments", assignee)))
		}
	}
	return nil
}

// unsupportedAttachmentsErr builds the fail-fast message used when a
// dispatcher config declares attachments. The dispatcher runner has no
// path to inject per-issue attachments: attachments are binary files
// referenced as {{attachments.<name>.path}} (see pkg/dsl/ir.Attachment —
// kinds are file/image, never a template-rendered string), and there is
// no defined mapping from a rendered string to an attachment's bytes
// (content? a path to open? an upload id?). Rather than silently drop the
// block (the prior behaviour — rendered then never wired into the engine)
// or guess a semantic and risk feeding the bot the wrong bytes, we refuse
// the config outright so the operator gets an honest signal at load time.
// See docs/adr/013-dispatcher-attachments-unsupported.md.
func unsupportedAttachmentsErr(field string) string {
	return "config: " + field + " is not supported — the dispatcher cannot inject " +
		"per-issue attachments (attachments are binary files referenced as " +
		"{{attachments.<name>.path}}, not template-rendered strings). Remove the " +
		"block and pass per-issue context through vars / a ticket's bot_args " +
		"instead. See docs/adr/013-dispatcher-attachments-unsupported.md."
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
