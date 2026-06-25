// Package trigger is the unifying spine for event-driven runs. It defines
// one canonical Event envelope that every trigger source (forge webhook,
// schedule tick, native-board transition, run completion, custom ingest)
// maps onto, plus a Subscription registry that binds (event filter) →
// (bot launch into a target repo/workspace). The Evaluator consumes events
// from pkg/eventbus, matches them against subscriptions, and hands a
// LaunchPlan to a Launcher.
//
// Two layers compose here, deliberately:
//
//   - Capability (pkg/bundle Invocation, unchanged) — "what surfaces can
//     fire me", authored on the bot manifest. No repo/tenant/cron knowledge.
//   - Binding (trigger.Subscription, new) — "on tenant T / repo R, when an
//     event matches M, launch bot B". Generated FROM invocations at
//     provision time. Repo/tenant/cron live here, never in the manifest.
package trigger

import "time"

// Source classifies where an event originated. Closed set — a new source
// adapter adds a constant here and emits onto the bus; nothing downstream
// changes.
type Source string

const (
	// SourceForge is an inbound git-forge webhook (GitLab/GitHub/Forgejo/generic).
	SourceForge Source = "forge"
	// SourceSchedule is a cron tick (host crontab or cloudsched).
	SourceSchedule Source = "schedule"
	// SourceBoard is a native-board transition (a native.Event).
	SourceBoard Source = "board"
	// SourceRun is a run-lifecycle event (run finished/failed) — the
	// "runned by iterion" chaining source.
	SourceRun Source = "run"
	// SourceManual is an operator/CLI explicit launch (iterion run).
	SourceManual Source = "manual"
	// SourceCustom is an external integration via the signed emit ingress.
	SourceCustom Source = "custom"
)

// Board event kinds (the Kind field when Source == SourceBoard). They are a
// normalized projection of native.EventType — a card's lifecycle as the
// trigger spine sees it, independent of the board's internal audit vocabulary.
const (
	KindCardCreated = "card.created"
	KindCardMoved   = "card.moved"
	KindCardLabeled = "card.labeled"
	KindCardUpdated = "card.updated"
)

// Run lifecycle kinds (the Kind field when Source == SourceRun) — the
// "runned by iterion" chaining source.
const (
	KindRunFinished  = "run.finished"
	KindRunFailed    = "run.failed"
	KindRunCancelled = "run.cancelled"
)

// Subject is the thing an event is about (a PR, an issue/card, a run, a repo).
// Fields are best-effort per source; templating reads from here and Payload.
type Subject struct {
	Type  string `json:"type" bson:"type"` // "pull_request" | "issue" | "card" | "run" | "repo" | "comment"
	ID    string `json:"id,omitempty" bson:"id,omitempty"`
	URL   string `json:"url,omitempty" bson:"url,omitempty"`
	SHA   string `json:"sha,omitempty" bson:"sha,omitempty"`
	Ref   string `json:"ref,omitempty" bson:"ref,omitempty"`
	Title string `json:"title,omitempty" bson:"title,omitempty"`
	Body  string `json:"body,omitempty" bson:"body,omitempty"`
	// State is the subject's workflow/board state at the time of the event
	// (e.g. the board column a card entered). Matched by Matcher.SubjectStates.
	State string `json:"state,omitempty" bson:"state,omitempty"`
}

// Event is the canonical envelope every source maps onto. Storage-agnostic
// and JSON+bson tagged so the same value can ride NATS (NATSBus), sit in
// Mongo (audit), or append to a file — mirroring the dual-tagging the forge
// and cloudsched records already use.
type Event struct {
	// ID is a stable per-event identifier used for idempotency/dedup
	// (e.g. "board:<board>:<issue>:<seq>"). A source that has no natural
	// key generates a ULID/uuid.
	ID     string `json:"id" bson:"_id"`
	Source Source `json:"source" bson:"source"`
	// Kind is the source-specific event type (forge event name, board
	// card.* kind, "run_finished", "cron", ...).
	Kind string `json:"kind" bson:"kind"`
	// Action narrows Kind where the source has one (forge "opened"/"reopened").
	Action string `json:"action,omitempty" bson:"action,omitempty"`
	// TenantID scopes the event to an org in cloud mode; "" in local single-host.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// Repo is the forge slug ("group/project") or the local repo root the
	// event pertains to; "" for tenant-wide events.
	Repo    string  `json:"repo,omitempty" bson:"repo,omitempty"`
	Subject Subject `json:"subject" bson:"subject"`
	// Actor is the triggering identity (PR author login, commenter, "schedule").
	Actor string `json:"actor,omitempty" bson:"actor,omitempty"`
	// Labels are the labels on the subject at event time (board card labels,
	// issue labels). Matched by Matcher.Labels.
	Labels []string `json:"labels,omitempty" bson:"labels,omitempty"`
	// Payload carries source-specific extras used for var templating
	// (issue title/body, comment args, board from/to state, ...).
	Payload    map[string]any `json:"payload,omitempty" bson:"payload,omitempty"`
	OccurredAt time.Time      `json:"occurred_at" bson:"occurred_at"`
}
