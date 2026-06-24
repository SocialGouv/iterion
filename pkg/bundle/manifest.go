package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v2"
)

// CurrentManifestSchema is the manifest schema version this build understands.
// Bumped only on breaking changes; minor additive fields use the reserved
// `Compat` map to avoid forcing a version bump on every new key.
const CurrentManifestSchema = 1

// Manifest is the parsed `manifest.yaml` shipped at the bundle root.
// All fields are optional except SchemaVersion, which defaults to 1 when
// omitted (treated as "explicit v1"). Future minor extensions add to
// Compat without changing SchemaVersion.
type Manifest struct {
	// Name is the bundle's technical id (falls back to the file stem
	// when empty). Distinct from the workflow's own name. Surfaced in
	// the studio's bundle picker, `iterion bots list`, and on the run
	// header next to the workflow name.
	Name string `yaml:"name"`

	// DisplayName is the bundle's friendly persona — the name an
	// operator actually uses in conversation (e.g. "Nexie" for the
	// whats-next bot, "Billy" for some future feature_dev variant).
	// Optional and free-form. When set, the studio's RunHeader gilds
	// the bot chip with a ✨ icon so dispatcher-spawned runs are
	// instantly recognisable by persona, not just by technical name.
	// Empty falls back to the Name + WorkflowName pair as before.
	DisplayName string `yaml:"display_name,omitempty"`

	// Version is a free-form bundle version string (semver or any
	// other scheme — the engine does not parse it).
	Version string `yaml:"version"`

	// Description is a one-line summary surfaced by `iterion inspect`
	// and the studio's bundle picker.
	Description string `yaml:"description"`

	// Author is a free-form attribution string.
	Author string `yaml:"author"`

	// SchemaVersion identifies the manifest format. Unknown values
	// produce a clear error pointing at the user's iterion build.
	SchemaVersion int `yaml:"schema_version"`

	// Compat is a forward-compatible bag for additive fields. Unknown
	// keys here are ignored without breaking loads from newer bundles.
	Compat map[string]interface{} `yaml:"compat,omitempty"`

	// Attachments declares default values for the workflow's
	// `attachments:` block: keys are attachment names, values are
	// paths inside the bundle's `attachments/` directory (relative).
	// Runtime uploads (Launch modal) override these.
	Attachments map[string]string `yaml:"attachments,omitempty"`

	// Triggers are free-form labels the orchestrator uses to match
	// issues to this bundle (e.g. "refactor", "feature_request").
	// Consumed by `iterion bots list` to build the bot catalog;
	// the runtime itself doesn't read them today.
	Triggers []string `yaml:"triggers,omitempty"`

	// Capabilities lists the host capabilities this bundle expects
	// to be granted (e.g. "board.create"). Documentation-only — the
	// runtime gates capabilities per node, not per bundle.
	Capabilities []string `yaml:"capabilities,omitempty"`

	// WhenToUse is the orchestrator-facing "use when" guidance for this
	// bot — the same role as the "when to use it" block in a Claude Code
	// skill. Free text, may be multi-line. Surfaced verbatim in the
	// generated iterion-bot-catalog "Use when" card that Nexie reads to
	// route a task to a bot. Optional; an empty value drops the card.
	// Edited via the studio Bot-metadata panel.
	WhenToUse string `yaml:"when_to_use,omitempty"`

	// DispatchVars maps the issue into THIS bot's input vars when the
	// dispatcher runs it (e.g. {"feature_prompt": "{{issue.title}}\n\n
	// {{issue.body}}"} for feature-dev, {"scope_notes": "…"} for a
	// reviewer). Values are dispatcher var templates ({{issue.*}}),
	// rendered per issue; per-ticket bot_args merge on top. This makes
	// the per-bot dispatch wiring DISCOVERY-DRIVEN — the stock
	// `iterion dispatch` no longer hardcodes a name→vars map; it reads
	// this from each discovered bot's manifest, so adding/renaming a bot
	// (shipped or custom) needs zero dispatcher-code edits. Optional;
	// empty = the bot receives only the global dispatch vars.
	DispatchVars map[string]string `yaml:"dispatch_vars,omitempty"`

	// Enabled toggles whether this bot is advertised in the catalog
	// exposed to orchestrator bots (Nexie). Tri-state on purpose:
	//   nil   → key absent → treated as enabled, so manifests authored
	//           before the toggle existed stay visible.
	//   true  → explicitly enabled.
	//   false → explicitly disabled: dropped from the generated catalog
	//           and not auto-dispatched, but still surfaced by the studio
	//           so an operator can flip it back on.
	// A workspace overlay (.iterion/bot-overrides.yaml) may override this
	// per-workspace without editing the manifest — see
	// botregistry.ResolveEnabled.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Forge declares the forge-access requirements this bot needs to be
	// auto-provisioned onto a connected repo through the studio's
	// Integrations flow. Advisory + discovery-time metadata, like
	// DispatchVars — the runtime itself does not read this; the
	// auto-provisioning orchestrator (pkg/forge) does, to compute the
	// forge webhook events, request the right token-scope subset, and
	// create the matching webhooks.Config + bot-secret binding in one
	// transaction. Nil when the bot declares no forge ambitions (the
	// Integrations "enable on this repo" picker filters those out).
	Forge *ForgeRequirements `yaml:"forge,omitempty"`

	// Invocations declare HOW this bot can be triggered (forge event,
	// /slash-command, schedule, or board pickup) and WHICH execution mode
	// each path uses (direct launch vs board-tracked dispatch). Distinct
	// from Triggers (free-form advisory catalog labels) and Forge (the
	// credential/token-scope requirements): Invocations are the typed,
	// machine-read routing contract consumed by the command router
	// (pkg/webhooks), the auto-provisioner (pkg/forge), and the cloud
	// scheduler (pkg/cloudsched). Empty = the bot is not directly
	// triggerable on a repo (orchestrators like Nexie/Evoly). A bundle that
	// declares only a legacy forge: block is treated as having the
	// synthetic set from SyntheticInvocations.
	Invocations []Invocation `yaml:"invocations,omitempty"`
}

// Normalized forge event vocabulary used in a manifest `forge.events`
// block. The auto-provisioner (pkg/forge) maps each entry to the
// per-provider native event when it creates the forge-side hook:
//
//	pull_request          -> gitlab "merge_requests_events",
//	                         github / forgejo "pull_request"
//	pull_request_comment  -> gitlab "note_events",
//	                         github / forgejo "issue_comment"
const (
	ForgeEventPullRequest        = "pull_request"
	ForgeEventPullRequestComment = "pull_request_comment"
)

// DefaultForgeSecretName is the workflow-secret name an integration
// binds the connection's forge token under when a manifest's
// forge.secret is empty. Matches the name review-pr / revi-converse
// declare in their .bot `secrets:` block.
const DefaultForgeSecretName = "forge_token"

// KnownForgeEvents is the closed set of normalized event names a
// manifest may declare in forge.events. decodeManifest rejects anything
// else so a typo fails fast at parse time (same bar as attachments:).
var KnownForgeEvents = map[string]bool{
	ForgeEventPullRequest:        true,
	ForgeEventPullRequestComment: true,
}

// knownForgeScopeKeys / knownForgeScopeLevels constrain a manifest
// forge.token_scopes block. The provisioner unions the keys across the
// bots co-enabled on a repo and translates them to the tightest OAuth
// scope / GitHub-App permission that satisfies the union.
var (
	knownForgeScopeKeys = map[string]bool{
		"pull_requests": true,
		"repository":    true,
		"issues":        true,
		"webhooks":      true,
	}
	knownForgeScopeLevels = map[string]bool{
		"read":  true,
		"write": true,
		"admin": true,
	}
)

// ForgeRequirements is the `forge:` block of a bundle manifest. All
// fields are optional; a bundle with no forge: block has Forge == nil.
type ForgeRequirements struct {
	// Events is the normalized event vocabulary this bot wants the
	// auto-created webhook to subscribe to (see KnownForgeEvents).
	Events []string `yaml:"events,omitempty"`

	// TokenScopes is a normalized permission map (key -> "read" |
	// "write" | "admin"); keys ∈ {pull_requests, repository, issues,
	// webhooks}. The provisioner always needs webhook-admin regardless
	// of this map — declaring it is informational. Unioned across
	// co-enabled bots to size the requested OAuth scope.
	TokenScopes map[string]string `yaml:"token_scopes,omitempty"`

	// Secret is the workflow-secret name the bundle's main.bot
	// `secrets:` block expects (e.g. "forge_token"). Empty defaults to
	// DefaultForgeSecretName. The orchestrator binds the connection's
	// managed forge token under this name; botregistry cross-references
	// it against the parsed .bot secret names.
	Secret string `yaml:"secret,omitempty"`

	// Webhook carries the launch-side knobs the orchestrator copies into
	// the auto-created webhooks.Config.
	Webhook *ForgeWebhookHints `yaml:"webhook,omitempty"`

	// Rationale is free text shown verbatim in the Integrations enable
	// dialog so the operator understands why each scope is requested.
	Rationale string `yaml:"rationale,omitempty"`
}

// ForgeWebhookHints are the webhook-launch knobs an auto-provisioned
// integration copies into webhooks.Config.
type ForgeWebhookHints struct {
	// LaunchVars are default vars the auto-created webhook stamps onto
	// every run it launches (merged with the handler defaults; operator
	// overrides still win).
	LaunchVars map[string]string `yaml:"launch_vars,omitempty"`

	// MinReplierRole mirrors webhooks.Config.MinReplierRole — the
	// minimum forge role a commenter must have to trigger the bot via a
	// note. Empty inherits the webhook default.
	MinReplierRole string `yaml:"min_replier_role,omitempty"`

	// AuthorAllowlist mirrors webhooks.Config.AuthorAllowlist — restrict the
	// auto-created webhook to PRs/MRs opened by these author logins (empty =
	// any author). A dependency-PR bot sets ["dependabot[bot]",
	// "renovate[bot]"] so it reacts only to the dependency bots, not humans.
	AuthorAllowlist []string `yaml:"author_allowlist,omitempty"`
}

// SecretName returns the workflow-secret name this bot binds its forge
// token under, applying DefaultForgeSecretName when unset.
func (f *ForgeRequirements) SecretName() string {
	if f == nil || strings.TrimSpace(f.Secret) == "" {
		return DefaultForgeSecretName
	}
	return f.Secret
}

// Invocation vocabulary -----------------------------------------------------

// InvocationKind classifies the surface that can fire a bot. Closed set,
// validated at manifest parse time (same bar as KnownForgeEvents) so a typo
// fails fast.
type InvocationKind string

const (
	// InvocationKindForge fires on a forge webhook event (PR/MR open, push).
	InvocationKindForge InvocationKind = "forge"
	// InvocationKindCommand fires on a /slash-command in a PR/MR/issue comment.
	InvocationKindCommand InvocationKind = "command"
	// InvocationKindSchedule fires on a cron tick (advisory suggested_cron the
	// Integrations UI proposes; iterion's cloud scheduler owns firing).
	InvocationKindSchedule InvocationKind = "schedule"
	// InvocationKindBoard marks the bot as a dispatcher target: an issue whose
	// Bot == this bot's name is picked up and run. No payload.
	InvocationKindBoard InvocationKind = "board"
)

var knownInvocationKinds = map[InvocationKind]bool{
	InvocationKindForge:    true,
	InvocationKindCommand:  true,
	InvocationKindSchedule: true,
	InvocationKindBoard:    true,
}

// ExecutionMode controls how a fired invocation becomes a run.
//
//	direct → launch the run immediately (the Revi path:
//	         insertAndLaunchWebhook → publisher → queue → runner). For
//	         fast, read-only, PR-bound work.
//	board  → materialise a kanban issue assigned to the bot; the dispatcher
//	         claims and runs it (tracked, retryable, supports human gates).
//	         For long, mutating, to-be-tracked work.
type ExecutionMode string

const (
	ExecutionDirect ExecutionMode = "direct"
	ExecutionBoard  ExecutionMode = "board"
)

var knownExecutionModes = map[ExecutionMode]bool{
	"":              true,
	ExecutionDirect: true,
	ExecutionBoard:  true,
}

// Invocation declares one way this bot can be triggered, plus the execution
// mode that path uses. The payload field that applies is selected by Kind
// (Forge for kind=forge, Command for kind=command, Schedule for
// kind=schedule; kind=board needs none).
type Invocation struct {
	Kind InvocationKind `yaml:"kind" json:"kind"`

	// Mode is the execution mode for this path. Empty defaults to "direct"
	// (see EffectiveMode).
	Mode ExecutionMode `yaml:"mode,omitempty" json:"mode,omitempty"`

	// ArgsVar names the workflow input var that receives the trigger's
	// free-text payload (the comment args after the command, etc.). Empty
	// injects no payload. Cross-checked against the bot's declared vars by
	// botregistry.ListWithSchema (a warning, not a hard error).
	ArgsVar string `yaml:"args_var,omitempty" json:"args_var,omitempty"`

	// ContextVars are extra launch vars stamped on every run from this
	// invocation, merged BEFORE the operator's webhook LaunchVars (operator
	// still wins).
	ContextVars map[string]string `yaml:"context_vars,omitempty" json:"context_vars,omitempty"`

	Forge    *InvocationForge    `yaml:"forge,omitempty" json:"forge,omitempty"`
	Command  *InvocationCommand  `yaml:"command,omitempty" json:"command,omitempty"`
	Schedule *InvocationSchedule `yaml:"schedule,omitempty" json:"schedule,omitempty"`
}

// EffectiveMode returns the execution mode, defaulting an empty value to
// ExecutionDirect (the safe, PR-bound behaviour).
func (i Invocation) EffectiveMode() ExecutionMode {
	if i.Mode == ExecutionBoard {
		return ExecutionBoard
	}
	return ExecutionDirect
}

// InvocationForge is the payload of a kind=forge invocation.
type InvocationForge struct {
	// Event is one of KnownForgeEvents.
	Event string `yaml:"event" json:"event"`
	// Actions narrows the trigger to specific provider actions (e.g.
	// "opened","reopened" for a PR). Empty applies the handler's default
	// reviewable-action filter.
	Actions []string `yaml:"actions,omitempty" json:"actions,omitempty"`
}

// InvocationCommand is the payload of a kind=command invocation.
type InvocationCommand struct {
	// Name is the slash-command id WITHOUT the leading "/" (e.g. "revi",
	// "featurly"). Lowercase ^[a-z][a-z0-9_-]*$.
	Name string `yaml:"name" json:"name"`
	// Aliases are additional command ids that route to this bot (e.g. the
	// technical name "feature-dev" aliasing the persona "featurly").
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Scope restricts where the command is honoured: "pr" (default),
	// "issue", or "any".
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
	// MinReplierRole overrides the webhook's MinReplierRole for THIS
	// command — a mutating bot can demand "maintainer" while a reviewer
	// stays "developer". Empty inherits the webhook default.
	MinReplierRole string `yaml:"min_replier_role,omitempty" json:"min_replier_role,omitempty"`
	// Disambiguator resolves a same-name command shared by two co-enabled
	// bots (the review-pr vs revi-converse pattern): "when_args_empty"
	// claims a bare "/cmd", "when_args_present" claims "/cmd <args>". Empty
	// claims the command unconditionally.
	Disambiguator string `yaml:"disambiguator,omitempty" json:"disambiguator,omitempty"`

	// OpensMR marks this command as one whose bot opens a merge/pull request
	// AND should back-link the original issue the human commented on. When set
	// and the command fires in board mode, the dispatch layer stamps
	// open_mr="true" + source_issue_ref=<subject URL/ref> into the materialised
	// card's bot_args, so the routed bot (a code-improvement bot that declares
	// the matching open_mr / source_issue_ref vars) opens the MR and links the
	// issue. Off for read-only commands (e.g. /revi) so unrelated board
	// commands aren't stamped.
	OpensMR bool `yaml:"opens_mr,omitempty" json:"opens_mr,omitempty"`
}

// InvocationSchedule is the payload of a kind=schedule invocation.
type InvocationSchedule struct {
	// SuggestedCron is a 5-field cron expression the Integrations UI
	// proposes as a default. Advisory — the operator picks the final
	// schedule; iterion's cloud scheduler (pkg/cloudsched) owns firing.
	SuggestedCron string `yaml:"suggested_cron,omitempty" json:"suggested_cron,omitempty"`
	// DefaultVars are vars stamped on each scheduled run.
	DefaultVars map[string]string `yaml:"default_vars,omitempty" json:"default_vars,omitempty"`
}

var (
	commandNameRe       = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	knownCommandScopes  = map[string]bool{"": true, "pr": true, "issue": true, "any": true}
	knownDisambiguators = map[string]bool{"": true, "when_args_empty": true, "when_args_present": true}
)

// validateInvocations rejects malformed invocations at parse time so a typo
// fails fast (same bar as forge.events). It checks the kind/mode enums, the
// per-kind required payload + payload mutual-exclusivity, the command-name
// shape, and intra-bot command-name uniqueness. Cross-bot command collisions
// are a provisioning-time concern (pkg/forge), not a manifest one.
func validateInvocations(invs []Invocation) error {
	seenCmd := map[string]bool{}
	for idx, inv := range invs {
		if !knownInvocationKinds[inv.Kind] {
			return fmt.Errorf("invocations[%d]: unknown kind %q (known: forge, command, schedule, board)", idx, inv.Kind)
		}
		if !knownExecutionModes[inv.Mode] {
			return fmt.Errorf("invocations[%d]: invalid mode %q (want direct or board)", idx, inv.Mode)
		}
		switch inv.Kind {
		case InvocationKindForge:
			if inv.Forge == nil {
				return fmt.Errorf("invocations[%d]: kind=forge requires a forge: block", idx)
			}
			if !KnownForgeEvents[inv.Forge.Event] {
				return fmt.Errorf("invocations[%d].forge: unknown event %q (known: %s, %s)", idx, inv.Forge.Event, ForgeEventPullRequest, ForgeEventPullRequestComment)
			}
			if inv.Command != nil || inv.Schedule != nil {
				return fmt.Errorf("invocations[%d]: kind=forge must not set command:/schedule:", idx)
			}
		case InvocationKindCommand:
			if inv.Command == nil {
				return fmt.Errorf("invocations[%d]: kind=command requires a command: block", idx)
			}
			if !knownCommandScopes[inv.Command.Scope] {
				return fmt.Errorf("invocations[%d].command: invalid scope %q (want pr, issue, or any)", idx, inv.Command.Scope)
			}
			if !knownDisambiguators[inv.Command.Disambiguator] {
				return fmt.Errorf("invocations[%d].command: invalid disambiguator %q (want when_args_empty or when_args_present)", idx, inv.Command.Disambiguator)
			}
			for _, nm := range append([]string{inv.Command.Name}, inv.Command.Aliases...) {
				lc := strings.ToLower(nm)
				if !commandNameRe.MatchString(lc) {
					return fmt.Errorf("invocations[%d].command: invalid name %q (want ^[a-z][a-z0-9_-]*$)", idx, nm)
				}
				if seenCmd[lc] {
					return fmt.Errorf("invocations[%d].command: duplicate command name %q within this bot", idx, lc)
				}
				seenCmd[lc] = true
			}
			if inv.Forge != nil || inv.Schedule != nil {
				return fmt.Errorf("invocations[%d]: kind=command must not set forge:/schedule:", idx)
			}
		case InvocationKindSchedule:
			if inv.Schedule == nil {
				return fmt.Errorf("invocations[%d]: kind=schedule requires a schedule: block", idx)
			}
			if c := strings.TrimSpace(inv.Schedule.SuggestedCron); c != "" {
				if fields := strings.Fields(c); len(fields) != 5 {
					return fmt.Errorf("invocations[%d].schedule: suggested_cron %q must be a 5-field cron expression", idx, c)
				}
			}
			if inv.Forge != nil || inv.Command != nil {
				return fmt.Errorf("invocations[%d]: kind=schedule must not set forge:/command:", idx)
			}
		case InvocationKindBoard:
			if inv.Forge != nil || inv.Command != nil || inv.Schedule != nil {
				return fmt.Errorf("invocations[%d]: kind=board takes no payload", idx)
			}
		}
	}
	return nil
}

// IsEnabled reports whether this bot should be advertised in the
// orchestrator-facing catalog by default. A nil Enabled (key absent
// from the manifest) is treated as enabled, so bots authored before the
// toggle existed remain visible. A workspace overlay may still override
// this — see botregistry.ResolveEnabled.
func (m *Manifest) IsEnabled() bool {
	if m == nil || m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

// LoadManifest reads and parses a manifest.yaml file. Missing file
// is not an error (returns nil, nil); only parse or schema errors fail.
func LoadManifest(path string) (*Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bundle: read manifest %s: %w", path, err)
	}
	return decodeManifest(body, path)
}

// decodeManifest parses + validates manifest bytes (strict unmarshal,
// schema version, attachment-path safety). srcLabel names the source in
// errors. Shared by LoadManifest and WriteManifest's pre-write
// validation so a rewritten manifest is held to exactly the same bar as
// a loaded one.
func decodeManifest(body []byte, srcLabel string) (*Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalStrict(body, &m); err != nil {
		return nil, fmt.Errorf("bundle: parse manifest %s: %w", srcLabel, err)
	}
	if m.SchemaVersion == 0 {
		m.SchemaVersion = CurrentManifestSchema
	}
	if m.SchemaVersion != CurrentManifestSchema {
		return nil, fmt.Errorf(
			"bundle: manifest schema_version %d not supported by this iterion build (expected %d) — upgrade iterion or downgrade the bundle",
			m.SchemaVersion, CurrentManifestSchema,
		)
	}
	// Every attachment value is later joined to the bundle's attachments/
	// directory and opened as a file by the runtime. Reject absolute or
	// "../"-escaping values at parse time so a hostile bundle can't turn
	// that join into an arbitrary host-file read. This mirrors the tar
	// extractor's guardEntry — both untrusted path sources in a .botz are
	// validated identically.
	for name, rel := range m.Attachments {
		if err := validateAttachmentRelPath(name, rel); err != nil {
			return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
		}
	}
	if err := validateForgeRequirements(m.Forge); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	if err := validateInvocations(m.Invocations); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	return &m, nil
}

// validateForgeRequirements rejects an unknown event name or a malformed
// token-scope entry in a manifest `forge:` block at parse time, so a typo
// fails fast (same bar as attachments:). The forge.secret cross-reference
// against the bundle's main.bot `secrets:` block is a soft check surfaced
// by botregistry, not enforced here — decodeManifest does not see main.bot.
func validateForgeRequirements(f *ForgeRequirements) error {
	if f == nil {
		return nil
	}
	for _, ev := range f.Events {
		if !KnownForgeEvents[ev] {
			return fmt.Errorf("forge: unknown event %q (known: %s, %s)", ev, ForgeEventPullRequest, ForgeEventPullRequestComment)
		}
	}
	for key, level := range f.TokenScopes {
		if !knownForgeScopeKeys[key] {
			return fmt.Errorf("forge.token_scopes: unknown scope %q (known: pull_requests, repository, issues, webhooks)", key)
		}
		if !knownForgeScopeLevels[level] {
			return fmt.Errorf("forge.token_scopes[%s]: invalid level %q (want read, write, or admin)", key, level)
		}
	}
	return nil
}

// validateAttachmentRelPath rejects a manifest `attachments:` value that
// is absolute or escapes the bundle root via "..". The downstream
// consumer builds the on-disk path with a bare
// filepath.Join(AttachmentsDir, value) followed by os.Open, so an
// unvalidated value such as "../../../../etc/passwd" would read an
// arbitrary host file. Keep this in lock-step with tar.go's guardEntry.
func validateAttachmentRelPath(name, rel string) error {
	if strings.TrimSpace(rel) == "" {
		return fmt.Errorf("attachment %q has an empty path", name)
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("attachment %q path must be relative, got absolute %q", name, rel)
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." {
		return fmt.Errorf("attachment %q has an empty path", name)
	}
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("attachment %q path must be relative, got absolute %q", name, rel)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("attachment %q path escapes the bundle (%q)", name, rel)
		}
	}
	return nil
}
