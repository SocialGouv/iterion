package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
)

// CurrentManifestSchema is the manifest schema version this build understands.
// Bumped only on breaking changes; minor additive fields use the reserved
// `Compat` map to avoid forcing a version bump on every new key.
const CurrentManifestSchema = 1

// maxIconLen caps the manifest `icon:` value. Generous enough for any
// multi-codepoint emoji sequence (ZWJ families run ~25 bytes) while
// rejecting prose.
const maxIconLen = 32

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

	// Icon is a short emoji identity for the bot (e.g. "🦉"), surfaced
	// on catalog cards, the bot home page, and the launch modal. Kept a
	// free string (an emoji may be multi-codepoint) but capped at
	// maxIconLen bytes at parse time so a manifest can't smuggle prose
	// into it. Empty falls back to the studio's persona/hash identity.
	Icon string `yaml:"icon,omitempty"`

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
	Compat map[string]any `yaml:"compat,omitempty"`

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

	// Repo declares this bot's RUNTIME repository need: whether a run
	// should target a git repository, and whether the launch surface may
	// offer to CREATE a new one on a connected forge (Appy's "new app,
	// new repo" journey). Advisory launch-surface metadata like
	// DispatchVars — the runtime only consumes the resolved
	// repo_url/repo_ref on the launch spec. It expresses a NEED ("point
	// me at a repo"), never a target-repo assumption: catalog bots stay
	// repo-agnostic.
	Repo *RepoRequirement `yaml:"repo,omitempty"`

	// ConfigShare declares this bot's SCOPED CONFIG-SHARE surface: which
	// fields of its workspace config file a non-operator may edit through a
	// scoped share URL (pkg/configshare), so a share can be minted for THIS
	// bot without the operator hand-typing the config file's JSON paths. The
	// mint DERIVES the grant's editable/visible paths and config file from
	// this block (expanding a {category} placeholder for a per-category
	// config), pinning them at mint time — a share can never be minted
	// outside the surface the bot committed to git. Advisory declaration like
	// Repo/Forge (the runtime never reads it; the config-share mint does).
	// A second bot adopts the whole config-share editor by adding this block
	// alone — no server or SPA change.
	ConfigShare *ConfigShareSpec `yaml:"config_share,omitempty"`

	// Launch opinionates the studio launch form for this bot: which
	// workflow vars are primary (surfaced top-level, in order) and which
	// are hidden (never rendered, still settable via --var). Advisory
	// launch-surface metadata like Repo/DispatchVars — the runtime never
	// reads it. Names are normalized at parse time (trim, drop empties,
	// dedupe) but NOT validated against the workflow's vars block —
	// manifests load without the DSL, so an unknown name is a soft
	// authoring mistake for the studio to surface, never a load error.
	Launch *LaunchHints `yaml:"launch,omitempty"`

	// Chat declares this bundle as a CONVERSATIONAL bot the studio hosts in
	// its assistant dock: which node speaks, which one collects the reply,
	// and what the session launcher asks first. Nil = not a chat bot, which
	// is every bot but the two that are.
	//
	// It lives in the manifest so a second chat bot is a bundle rather than
	// a studio release — the registry the studio reads is discovery, not a
	// hard-coded const with a bot id in it.
	Chat *ChatSurface `yaml:"chat,omitempty"`

	// Produces / Consumes declare the RUN-TO-RUN hand-off: what a bot leaves
	// behind that a later run can start from, and what a bot wants handed to it
	// at launch. They are matched BY KIND — a shared vocabulary, never a bot id
	// — so a reviewer and a fixer cooperate without either manifest naming the
	// other, and a second reviewer or a second fixer joins by declaring the same
	// kind. Same shape as the merge gate's `gate_context`: one agreed name,
	// filled by whoever plays the role.
	Produces []ProducedArtifact `yaml:"produces,omitempty"`
	Consumes []ConsumedArtifact `yaml:"consumes,omitempty"`

	// Retry is the bot author's opinion on what should happen when one of
	// this bot's runs dies because the provider's quota window is exhausted
	// (pkg/retrypolicy). It is the BOT layer of the retry precedence chain,
	// below a schedule's row and above the machine default — a launch
	// surface overrides only the fields it sets.
	//
	// This lives in the manifest rather than the DSL on purpose: retry
	// decides whether a NEW run is launched, which is orchestration, not
	// workflow semantics. Everything on the `workflow` block (compress,
	// permission, worktree, sandbox, budget) changes how nodes execute
	// INSIDE a run, and a DSL field would land in ir.Workflow — which the
	// engine reads, pulling it toward being retry-aware. The precedent is
	// InvocationKeepalive.StaleAfter: a schedgate field already declared
	// here and defaulted down into a Subscription.
	//
	// The relevant author knowledge is whether the output is worth having
	// late: a weekly digest is (retry it after the reset), a "what changed
	// in the last hour" report is not (usage_window: off).
	Retry *RetrySpec `yaml:"retry,omitempty"`
}

// RetrySpec is the manifest shape of a retrypolicy.Policy. It is declared
// as its own type (rather than embedding retrypolicy.Policy) so the YAML
// surface stays independent of the shared struct's json/bson tags, matching
// how every other host declares these fields flat and projects them.
type RetrySpec struct {
	UsageWindow string `yaml:"usage_window,omitempty" json:"usage_window,omitempty"`
	MaxAttempts int    `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	MaxWait     string `yaml:"max_wait,omitempty" json:"max_wait,omitempty"`
	Jitter      string `yaml:"jitter,omitempty" json:"jitter,omitempty"`
}

// RetryPolicy projects the manifest's retry block. A nil block yields the
// zero Policy, i.e. "this layer sets nothing" — deliberately NOT normalized,
// since filling defaults here would make the bot appear to pin fields the
// author left open and mask the machine default.
func (m *Manifest) RetryPolicy() retrypolicy.Policy {
	if m == nil || m.Retry == nil {
		return retrypolicy.Policy{}
	}
	return retrypolicy.Policy{
		UsageWindow: m.Retry.UsageWindow,
		MaxAttempts: m.Retry.MaxAttempts,
		MaxWait:     m.Retry.MaxWait,
		Jitter:      m.Retry.Jitter,
	}
}

// LaunchHints is a bot's declared launch-form opinion (manifest
// `launch:` block), carried by discovery onto the bot entry served at
// /api/v1/bots so the studio can order and prune the var form.
type LaunchHints struct {
	// Primary lists the bot inputs to surface top-level, in this order.
	Primary []string `json:"primary,omitempty" yaml:"primary,omitempty"`
	// Hidden lists the bot inputs the launch form never renders.
	Hidden []string `json:"hidden,omitempty" yaml:"hidden,omitempty"`
}

// normalized returns the hints with each list cleaned — entries
// trimmed, empties dropped, duplicates removed keeping the first
// occurrence — and collapses to nil when nothing remains, so an
// effectively-empty block serialises as an absent `launch` key.
func (l *LaunchHints) normalized() *LaunchHints {
	if l == nil {
		return nil
	}
	primary := normalizeNameList(l.Primary)
	hidden := normalizeNameList(l.Hidden)
	if len(primary) == 0 && len(hidden) == 0 {
		return nil
	}
	return &LaunchHints{Primary: primary, Hidden: hidden}
}

// normalizeNameList trims each entry, drops empties, and dedupes
// keeping first-occurrence order. Returns nil when nothing survives.
func normalizeNameList(names []string) []string {
	var out []string
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Valid RepoRequirement.Mode values.
const (
	RepoModeRequired = "required"
	RepoModeOptional = "optional"
	RepoModeNone     = "none"
)

// RepoRequirement is a bot's declared repository need, rendered by the
// launch surfaces as a "Target repository" section (active repo
// preselected → other connected repo → create new → none).
type RepoRequirement struct {
	// Mode is "required" (launch soft-blocks without a target repo),
	// "optional" (section offered, skippable), or "none" (explicit
	// repo-independence — same as omitting the block; kept so a bot can
	// document the choice).
	Mode string `yaml:"mode" json:"mode"`
	// AllowCreate offers "create a new repository" (forge RepoCreator)
	// next to "attach an existing one".
	AllowCreate bool `yaml:"allow_create,omitempty" json:"allow_create,omitempty"`
	// Purpose is a one-line operator-facing explanation of what the bot
	// does with the repo, shown under the section title.
	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty"`
	// DefaultBranch seeds a created repo's default branch name (empty =
	// the forge's default).
	DefaultBranch string `yaml:"default_branch,omitempty" json:"default_branch,omitempty"`
	// Visibility seeds a created repo's visibility: "private" (the
	// default) or "public".
	Visibility string `yaml:"visibility,omitempty" json:"visibility,omitempty"`
}

// HandoffKind is the shared vocabulary a consumer matches a producer BY. It
// names a ROLE in a hand-off, never a bot: any bot that reviews declares it
// produces a review, any bot that acts on one declares it consumes a review.
// Closed set — a new kind means new render semantics in the engine.
type HandoffKind string

// HandoffKindReview is a code review: a set of findings, each carrying an
// anchor (file + line span), a severity, a category, a confidence, a
// cross-family confirmation flag, a prose detail and — when the fix is local
// and high-confidence — a literal replacement ready to apply; plus the
// reviewers' non-blocking open questions.
const HandoffKindReview HandoffKind = "review"

// HandoffKindReviewLedger is the REPLY to a review: per finding id, what became
// of it — fixed (with the commit), refused (with the argument), or deferred.
// It closes the loop in the other direction, so a later review is told which of
// its findings were contested and why, instead of re-raising them for free.
const HandoffKindReviewLedger HandoffKind = "review_ledger"

// knownHandoffKinds is the closed vocabulary produces:/consumes: match on.
var knownHandoffKinds = map[HandoffKind]bool{
	HandoffKindReview:       true,
	HandoffKindReviewLedger: true,
}

// HandoffScope says what makes an upstream run "the same work" as this launch.
type HandoffScope string

// HandoffScopePR matches an upstream run by the pull request it was launched
// against (its pr_url input). The only scope today.
const HandoffScopePR HandoffScope = "pr"

// ProducedArtifact declares an artifact this bot leaves behind for a later run
// of another bot to start from. The node names are the bot's own graph shape,
// so the engine never has to know it.
type ProducedArtifact struct {
	Kind HandoffKind `yaml:"kind" json:"kind"`
	// Node is the node whose output artifact carries the payload (for a review:
	// the merged, de-duplicated finding set + the merged questions).
	Node string `yaml:"node" json:"node"`
	// FallbackNodes are consulted in order when Node's payload is unreadable —
	// an LLM node can emit prose where an array was asked for, and an
	// un-merged set beats no set at all. Optional.
	FallbackNodes []string `yaml:"fallback_nodes,omitempty" json:"fallback_nodes,omitempty"`
	// AnchorNode carries `reviewed_sha`: the revision the payload's line anchors
	// were computed against, so a consumer can be told the branch has moved
	// under it instead of acting on stale anchors. Optional.
	AnchorNode string `yaml:"anchor_node,omitempty" json:"anchor_node,omitempty"`
}

// ConsumedArtifact declares that this bot wants a prior run's produced artifact
// rendered into one of its launch vars. An operator-pinned value always wins.
type ConsumedArtifact struct {
	Kind HandoffKind `yaml:"kind" json:"kind"`
	// Var is the workflow var the rendered digest is stamped into. The bot must
	// declare it in its `vars:` block, or the IR drops it.
	Var string `yaml:"var" json:"var"`
	// Scope selects the upstream run. Defaults to HandoffScopePR.
	Scope HandoffScope `yaml:"scope,omitempty" json:"scope,omitempty"`
}

// EffectiveScope returns the declared scope or the default.
func (c ConsumedArtifact) EffectiveScope() HandoffScope {
	if c.Scope == "" {
		return HandoffScopePR
	}
	return c.Scope
}

// ConfigShareSpec is a bot's declared scoped config-share surface — the
// contract that lets an operator mint a share for the bot (pkg/configshare)
// without knowing its config file's JSON structure, and that a share can never
// exceed. A second bot adopts the config-share editor by adding this block
// alone: no server or SPA change.
type ConfigShareSpec struct {
	// ConfigPath is the config file inside the target repo a share edits
	// (e.g. "feed-watch.json"). Repo-relative; normalized + guarded at mint
	// (no traversal, no .git/.github, inside the workspace).
	ConfigPath string `yaml:"config_path" json:"config_path"`
	// EditablePaths are the dotted leaf paths a share may WRITE. A
	// "{category}" placeholder is expanded to the concrete category at mint
	// (e.g. "categories.{category}.feeds"), so ONE declaration covers every
	// category; a config with no categories lists literal paths. No globs —
	// every entry is a full leaf path.
	EditablePaths []string `yaml:"editable_paths" json:"editable_paths"`
	// VisiblePaths are extra READ-ONLY dotted paths a share may read back as
	// context (e.g. "categories.{category}.digest_title"). The GET projection
	// returns EditablePaths ∪ VisiblePaths and nothing else. Same {category}
	// expansion. Optional.
	VisiblePaths []string `yaml:"visible_paths,omitempty" json:"visible_paths,omitempty"`
	// EditorTitle overrides the generic "Config editor" heading in the
	// signed-in config_editor shell with a bot-specific name (e.g. "Éditeur de
	// veilles"). Optional; empty falls back to the generic title.
	EditorTitle string `yaml:"editor_title,omitempty" json:"editor_title,omitempty"`
	// EditorDescription is a one-line subtitle under EditorTitle explaining
	// what the editor edits ("Sources et éditorial de vos veilles"). Optional.
	EditorDescription string `yaml:"editor_description,omitempty" json:"editor_description,omitempty"`
}

// Normalized forge event vocabulary used in a manifest `forge.events`
// block. The auto-provisioner (pkg/forge) maps each entry to the
// per-provider native event when it creates the forge-side hook:
//
//	pull_request          -> gitlab "merge_requests_events",
//	                         github / forgejo "pull_request"
//	pull_request_comment  -> gitlab "note_events",
//	                         github / forgejo "issue_comment"
//	issue_labeled         -> github / forgejo "issues"
//	                         (gitlab "issues_events" — not yet wired inbound)
const (
	ForgeEventPullRequest        = "pull_request"
	ForgeEventPullRequestComment = "pull_request_comment"
	// ForgeEventIssueLabeled subscribes the repo hook to the forge-native
	// "issues" event; labeling an issue launches an implementer bot that
	// opens a PR back-linked to the issue (see the GitHub issues handler).
	ForgeEventIssueLabeled = "issue_labeled"
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
	ForgeEventIssueLabeled:       true,
}

// knownForgeEventNames lists the accepted events, sorted for a stable
// message. Derived from KnownForgeEvents rather than spelled out at the
// error site: the hardcoded version named only two of the three and stayed
// wrong while validation correctly accepted all three, so a bot author
// declaring the valid `issue_labeled` was told it was unknown.
func knownForgeEventNames() []string {
	names := make([]string, 0, len(KnownForgeEvents))
	for name := range KnownForgeEvents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// knownForgeScopeKeys / knownForgeScopeLevels constrain a manifest
// forge.token_scopes block. The provisioner unions the keys across the
// bots co-enabled on a repo and translates them to the tightest OAuth
// scope / GitHub-App permission that satisfies the union.
// ForgeScopeStatuses is the token scope a bot declares when it posts a commit
// status. Declaring it means the bot can be a REQUIRED check, which the
// provisioner has to account for beyond the token itself — a gate that does
// not follow the head SHA blocks the PR it guards.
const ForgeScopeStatuses = "statuses"

var (
	knownForgeScopeKeys = map[string]bool{
		"pull_requests":    true,
		"repository":       true,
		"issues":           true,
		"webhooks":         true,
		ForgeScopeStatuses: true, // commit statuses (the merge gate)
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

	// AuthorScope declares how AuthorAllowlist interacts with the OTHER bots
	// co-enabled on the same repo webhook:
	//
	//   "shared" (default/empty) — other bots also react to these authors.
	//   "exclusive"              — the authors I claim are MINE: provisioning
	//                              adds them to every other co-enabled bot's
	//                              author denylist, so a general reviewer
	//                              stops double-reviewing the PRs this bot
	//                              owns. The reviewer's own manifest stays
	//                              free of any knowledge that this bot exists.
	//
	// Only meaningful together with a non-empty AuthorAllowlist.
	AuthorScope string `yaml:"author_scope,omitempty"`
}

// Author-scope vocabulary for ForgeWebhookHints.AuthorScope.
const (
	AuthorScopeShared    = "shared"
	AuthorScopeExclusive = "exclusive"
)

// IsExclusiveAuthors reports whether this bot claims its allowlisted authors
// exclusively against the other bots sharing the repo webhook.
func (h *ForgeWebhookHints) IsExclusiveAuthors() bool {
	return h != nil && len(h.AuthorAllowlist) > 0 &&
		strings.EqualFold(strings.TrimSpace(h.AuthorScope), AuthorScopeExclusive)
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
	// InvocationKindKeepalive runs the bot always-on: a fresh, individually
	// budgeted run is relaunched every `interval` with at-most-one-live
	// semantics (a stale run is reaped, not stacked). Sub-minute cadence
	// requires the resident in-process scheduler (host crontab floors at 1m).
	// The bot's own supervisor: block, if any, attaches per launched run.
	InvocationKindKeepalive InvocationKind = "keepalive"
)

var knownInvocationKinds = map[InvocationKind]bool{
	InvocationKindForge:     true,
	InvocationKindCommand:   true,
	InvocationKindSchedule:  true,
	InvocationKindBoard:     true,
	InvocationKindKeepalive: true,
}

// KeepaliveMinInterval is the floor on a keepalive invocation's interval: a
// guardrail against a launch storm (each tick is a fresh budgeted run).
const KeepaliveMinInterval = 5 * time.Second

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

	Forge     *InvocationForge     `yaml:"forge,omitempty" json:"forge,omitempty"`
	Command   *InvocationCommand   `yaml:"command,omitempty" json:"command,omitempty"`
	Schedule  *InvocationSchedule  `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Board     *InvocationBoard     `yaml:"board,omitempty" json:"board,omitempty"`
	Keepalive *InvocationKeepalive `yaml:"keepalive,omitempty" json:"keepalive,omitempty"`
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

// InvocationKeepalive is the payload of a kind=keepalive invocation.
type InvocationKeepalive struct {
	// Interval is how often to relaunch the bot (Go duration, e.g. "30s",
	// "5m"). Required, must be >= KeepaliveMinInterval. Sub-minute values
	// need the resident in-process scheduler.
	Interval string `yaml:"interval" json:"interval"`
	// StaleAfter is the silence cutoff: a running run whose last progress is
	// older than this is treated as dead, so a fresh run relaunches and the
	// zombie is reaped. Empty defaults to schedgate.DefaultStaleAfter.
	StaleAfter string `yaml:"stale_after,omitempty" json:"stale_after,omitempty"`
	// DefaultVars are vars stamped on each relaunched run.
	DefaultVars map[string]string `yaml:"default_vars,omitempty" json:"default_vars,omitempty"`
}

// Board card-event kinds a kind=board invocation may filter on. These mirror
// the trigger package's Kind* constants; bundle can't import trigger (trigger
// imports bundle), so they are duplicated here and kept in sync — the closed
// set is enforced at parse time so a typo fails fast.
const (
	BoardKindCardCreated = "card.created"
	BoardKindCardMoved   = "card.moved"
	BoardKindCardLabeled = "card.labeled"
	BoardKindCardUpdated = "card.updated"
)

var knownBoardKinds = map[string]bool{
	BoardKindCardCreated: true,
	BoardKindCardMoved:   true,
	BoardKindCardLabeled: true,
	BoardKindCardUpdated: true,
}

// InvocationBoard is the optional payload of a kind=board invocation. It
// declares which native-board transitions fire this bot: the card-event
// kinds (On), the board states the card must have entered (ToStates), and the
// labels the card must ALL carry (AllLabels). An empty block keeps the legacy
// behaviour — the bot is a plain dispatcher target picked up when an issue's
// Bot == this bot. With a block, the orchestrator/operator derives a
// trigger.Subscription whose Matcher fires the bot the moment a matching card
// transition lands, instead of waiting for the dispatcher poll.
type InvocationBoard struct {
	// On filters by card-event kind (subset of knownBoardKinds). Empty = any.
	On []string `yaml:"on,omitempty" json:"on,omitempty"`
	// ToStates fires only when the card enters one of these board states.
	// Empty = any state.
	ToStates []string `yaml:"to_states,omitempty" json:"to_states,omitempty"`
	// AllLabels requires the card to carry every listed label (AND). Empty =
	// no label constraint.
	AllLabels []string `yaml:"all_labels,omitempty" json:"all_labels,omitempty"`
	// ConsumeLabels strips the AllLabels set from the card atomically before
	// firing, so the labels act as a one-shot trigger: a card-event storm
	// (forge re-syncs, edits) cannot re-fire, and re-adding the label re-arms
	// the trigger. Only meaningful with mode=direct (the promote path is
	// already idempotent); requires a non-empty AllLabels.
	ConsumeLabels bool `yaml:"consume_labels,omitempty" json:"consume_labels,omitempty"`
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
			return fmt.Errorf("invocations[%d]: unknown kind %q (known: forge, command, schedule, board, keepalive)", idx, inv.Kind)
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
				return fmt.Errorf("invocations[%d].forge: unknown event %q (known: %s)", idx, inv.Forge.Event, strings.Join(knownForgeEventNames(), ", "))
			}
			if inv.Command != nil || inv.Schedule != nil || inv.Keepalive != nil {
				return fmt.Errorf("invocations[%d]: kind=forge must not set a command:/schedule:/keepalive: block", idx)
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
			if inv.Forge != nil || inv.Schedule != nil || inv.Keepalive != nil {
				return fmt.Errorf("invocations[%d]: kind=command must not set a forge:/schedule:/keepalive: block", idx)
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
			if inv.Forge != nil || inv.Command != nil || inv.Keepalive != nil {
				return fmt.Errorf("invocations[%d]: kind=schedule must not set a forge:/command:/keepalive: block", idx)
			}
		case InvocationKindBoard:
			if inv.Forge != nil || inv.Command != nil || inv.Schedule != nil || inv.Keepalive != nil {
				return fmt.Errorf("invocations[%d]: kind=board takes only an optional board: block (not forge:/command:/schedule:/keepalive:)", idx)
			}
			if inv.Board != nil {
				for _, k := range inv.Board.On {
					if !knownBoardKinds[k] {
						return fmt.Errorf("invocations[%d].board: unknown on %q (known: %s, %s, %s, %s)", idx, k, BoardKindCardCreated, BoardKindCardMoved, BoardKindCardLabeled, BoardKindCardUpdated)
					}
				}
				if inv.Board.ConsumeLabels && len(inv.Board.AllLabels) == 0 {
					return fmt.Errorf("invocations[%d].board: consume_labels requires a non-empty all_labels", idx)
				}
			}
		case InvocationKindKeepalive:
			if inv.Keepalive == nil {
				return fmt.Errorf("invocations[%d]: kind=keepalive requires a keepalive: block", idx)
			}
			iv := strings.TrimSpace(inv.Keepalive.Interval)
			if iv == "" {
				return fmt.Errorf("invocations[%d].keepalive: interval is required", idx)
			}
			d, err := time.ParseDuration(iv)
			if err != nil {
				return fmt.Errorf("invocations[%d].keepalive: invalid interval %q: %w", idx, iv, err)
			}
			if d < KeepaliveMinInterval {
				return fmt.Errorf("invocations[%d].keepalive: interval %q is below the %s floor", idx, iv, KeepaliveMinInterval)
			}
			if sa := strings.TrimSpace(inv.Keepalive.StaleAfter); sa != "" {
				sd, err := time.ParseDuration(sa)
				if err != nil {
					return fmt.Errorf("invocations[%d].keepalive: invalid stale_after %q: %w", idx, sa, err)
				}
				if sd <= 0 {
					return fmt.Errorf("invocations[%d].keepalive: stale_after must be > 0 (got %q)", idx, sa)
				}
			}
			if inv.Forge != nil || inv.Command != nil || inv.Schedule != nil || inv.Board != nil {
				return fmt.Errorf("invocations[%d]: kind=keepalive must not set a forge:/command:/schedule:/board: block", idx)
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

// DecodeManifest parses + validates manifest bytes without touching the
// filesystem — the seam generators (pkg/botscaffold) use to hold their
// rendered manifest to the same bar as a loaded one before writing it.
func DecodeManifest(body []byte, srcLabel string) (*Manifest, error) {
	return decodeManifest(body, srcLabel)
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
	m.Icon = strings.TrimSpace(m.Icon)
	if len(m.Icon) > maxIconLen {
		return nil, fmt.Errorf("bundle: manifest %s: icon %q exceeds %d bytes — expected a short emoji", srcLabel, m.Icon, maxIconLen)
	}
	// Soft-normalize only — launch names may reference workflow vars,
	// which the manifest loader cannot see, so nothing here hard-fails.
	m.Launch = m.Launch.normalized()
	m.Chat = m.Chat.normalized()
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
	if err := validateChatSurface(m.Chat); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	if err := validateRepoRequirement(m.Repo); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	if err := validateHandoff(m.Produces, m.Consumes); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	// A typo in retry: must fail at parse time, next to its source. Left
	// unvalidated it would surface days later as a silently-defaulted
	// policy on a run nobody is watching.
	if err := retrypolicy.Validate(m.RetryPolicy()); err != nil {
		return nil, fmt.Errorf("bundle: manifest %s: %w", srcLabel, err)
	}
	return &m, nil
}

// validateHandoff rejects an unusable produces:/consumes: declaration at parse
// time. Left unchecked, every one of these degrades to silence: an unknown kind
// matches no counterpart, a producer with no node has nothing to load, and a
// consumer with no var has nowhere to put the result — and the hand-off is
// best-effort by design, so nothing downstream would ever report the mistake.
func validateHandoff(produces []ProducedArtifact, consumes []ConsumedArtifact) error {
	for _, p := range produces {
		if !knownHandoffKinds[p.Kind] {
			return fmt.Errorf("produces: unknown kind %q (known: %s, %s)", p.Kind, HandoffKindReview, HandoffKindReviewLedger)
		}
		if strings.TrimSpace(p.Node) == "" {
			return fmt.Errorf("produces: kind %q declares no node to read the artifact from", p.Kind)
		}
	}
	for _, c := range consumes {
		if !knownHandoffKinds[c.Kind] {
			return fmt.Errorf("consumes: unknown kind %q (known: %s, %s)", c.Kind, HandoffKindReview, HandoffKindReviewLedger)
		}
		if strings.TrimSpace(c.Var) == "" {
			return fmt.Errorf("consumes: kind %q declares no var to stamp the result into", c.Kind)
		}
		if c.EffectiveScope() != HandoffScopePR {
			return fmt.Errorf("consumes: unknown scope %q (known: %s)", c.Scope, HandoffScopePR)
		}
	}
	return nil
}

// validateRepoRequirement rejects unknown mode/visibility values at parse
// time so a typo fails fast (same bar as forge: and invocations:).
func validateRepoRequirement(r *RepoRequirement) error {
	if r == nil {
		return nil
	}
	switch r.Mode {
	case RepoModeRequired, RepoModeOptional, RepoModeNone:
	default:
		return fmt.Errorf("repo: unknown mode %q (known: %s, %s, %s)", r.Mode, RepoModeRequired, RepoModeOptional, RepoModeNone)
	}
	switch r.Visibility {
	case "", "private", "public":
	default:
		return fmt.Errorf("repo: unknown visibility %q (known: private, public)", r.Visibility)
	}
	return nil
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
			return fmt.Errorf("forge.token_scopes: unknown scope %q (known: pull_requests, repository, issues, webhooks, statuses)", key)
		}
		if !knownForgeScopeLevels[level] {
			return fmt.Errorf("forge.token_scopes[%s]: invalid level %q (want read, write, or admin)", key, level)
		}
	}
	if f.Webhook != nil {
		switch scope := strings.ToLower(strings.TrimSpace(f.Webhook.AuthorScope)); scope {
		case "", AuthorScopeShared, AuthorScopeExclusive:
		default:
			return fmt.Errorf("forge.webhook.author_scope: unknown value %q (known: %s, %s)", scope, AuthorScopeShared, AuthorScopeExclusive)
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
