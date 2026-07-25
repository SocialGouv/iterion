package trigger

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// Matcher is the declarative filter on an Event. It is the union of the four
// existing trigger families' allowlists (webhooks.Config.{Event,Project,
// Author,Label}Allowlist, the dispatcher's label/state selectors, the forge
// invocation Actions) so every legacy config maps onto it without losing
// fidelity. An empty slice means "match any" for that dimension; a non-empty
// slice requires at least one match (OR within a dimension, AND across
// dimensions). Labels is the exception — it requires ALL listed labels to be
// present (the board "all_labels" gate), which is what implementer triggers
// like {state: ready, labels: [feature]} need.
type Matcher struct {
	Sources       []Source `json:"sources,omitempty" bson:"sources,omitempty"`
	Kinds         []string `json:"kinds,omitempty" bson:"kinds,omitempty"`
	Actions       []string `json:"actions,omitempty" bson:"actions,omitempty"`
	Repos         []string `json:"repos,omitempty" bson:"repos,omitempty"`
	Authors       []string `json:"authors,omitempty" bson:"authors,omitempty"`
	Labels        []string `json:"labels,omitempty" bson:"labels,omitempty"`
	SubjectStates []string `json:"subject_states,omitempty" bson:"subject_states,omitempty"`
}

// Match reports whether ev satisfies every dimension of m. It is pure and
// total (no I/O, no panics) so it is cheap to call on the hot path and trivial
// to unit-test against each family's allowlist shape. All string comparisons
// are case-insensitive except Kind/Source/State, which are machine-generated
// enums compared exactly.
func (m Matcher) Match(ev Event) bool {
	if len(m.Sources) > 0 && !containsExact(m.Sources, ev.Source) {
		return false
	}
	if len(m.Kinds) > 0 && !containsStr(m.Kinds, ev.Kind, false) {
		return false
	}
	if len(m.Actions) > 0 && !containsStr(m.Actions, ev.Action, true) {
		return false
	}
	if len(m.Repos) > 0 && !containsStr(m.Repos, ev.Repo, true) {
		return false
	}
	if len(m.Authors) > 0 && !containsStr(m.Authors, ev.Actor, true) {
		return false
	}
	if len(m.SubjectStates) > 0 && !containsStr(m.SubjectStates, ev.Subject.State, false) {
		return false
	}
	// Labels: every required label must be present on the event (AND).
	for _, want := range m.Labels {
		if !containsStr(ev.Labels, want, true) {
			return false
		}
	}
	return true
}

func containsExact(set []Source, v Source) bool {
	return slices.Contains(set, v)
}

// containsStr reports whether v is in set. When fold is true the comparison is
// case-insensitive (logins, repo slugs, free-form labels); when false it is
// exact (machine enums like board kinds and states).
func containsStr(set []string, v string, fold bool) bool {
	for _, s := range set {
		if s == v || (fold && strings.EqualFold(s, v)) {
			return true
		}
	}
	return false
}

// Subscription binds an event filter to a bot launch into a target. One row
// per (tenant, repo, bot, invocation-kind). This is the unit the "by repo /
// by bot" studio surfaces and the Evaluator's hot-path query hit, and the
// projection the forge orchestrator generates from a bot's Invocations.
type Subscription struct {
	ID       string `json:"id" bson:"_id"`
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// Repo scopes the subscription to one repo ("group/project"); "" makes it
	// tenant-wide (e.g. a board-wide trigger on the local single-host board).
	Repo  string `json:"repo,omitempty" bson:"repo,omitempty"`
	BotID string `json:"bot_id" bson:"bot_id"`
	// Invocation names which bot capability this subscription activates, so a
	// deprovision/regeneration can rebuild exactly the rows derived from a
	// given invocation kind.
	Invocation bundle.InvocationKind `json:"invocation" bson:"invocation"`
	// Mode is direct (launch now) vs board (materialise a card the dispatcher
	// claims). Empty inherits the invocation's EffectiveMode at evaluation.
	Mode  bundle.ExecutionMode `json:"mode,omitempty" bson:"mode,omitempty"`
	Match Matcher              `json:"match" bson:"match"`
	// ConsumeLabels (board-source, direct-mode only) strips the Match.Labels
	// set from the card before launching, making the labels a one-shot
	// trigger: duplicate card events can't double-launch, and re-adding the
	// label re-arms it. Mirrors bundle.InvocationBoard.ConsumeLabels.
	ConsumeLabels bool `json:"consume_labels,omitempty" bson:"consume_labels,omitempty"`
	// Vars are launch-var overrides stamped on the run (ContextVars +
	// operator LaunchVars merged at provision time; operator wins).
	Vars map[string]string `json:"vars,omitempty" bson:"vars,omitempty"`
	// ArgsVar names the workflow input var that receives the event's free-text
	// payload (issue title+body, comment args). Empty injects no payload.
	ArgsVar string `json:"args_var,omitempty" bson:"args_var,omitempty"`
	// Cron is set only for Invocation == schedule (the timer source matches it).
	Cron string `json:"cron,omitempty" bson:"cron,omitempty"`
	// IntervalSeconds is set only for Invocation == keepalive (the sub-minute
	// counterpart of Cron): the always-on relaunch cadence in seconds.
	IntervalSeconds int               `json:"interval_seconds,omitempty" bson:"interval_seconds,omitempty"`
	KeyOverrides    map[string]string `json:"key_overrides,omitempty" bson:"key_overrides,omitempty"`
	SecretOverrides map[string]string `json:"secret_overrides,omitempty" bson:"secret_overrides,omitempty"`
	// Overlap policy + pre-launch guard for schedule-kind subscriptions
	// (pkg/schedgate). Overlap "" normalizes to "skip": a cron tick
	// whose previous run is still live is skipped and audited instead
	// of piling up. Guard is an optional `sh -lc` gate: exit 0 fires
	// (stdout → vars[GuardVar]), non-zero skips the tick.
	Overlap       string `json:"overlap,omitempty" bson:"overlap,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty" bson:"max_concurrent,omitempty"`
	Guard         string `json:"guard,omitempty" bson:"guard,omitempty"`
	GuardTimeout  string `json:"guard_timeout,omitempty" bson:"guard_timeout,omitempty"`
	GuardVar      string `json:"guard_var,omitempty" bson:"guard_var,omitempty"`
	// StaleAfter is the keepalive silence cutoff (Go duration); empty
	// normalizes to schedgate.DefaultStaleAfter. Only meaningful with
	// Overlap == keepalive.
	StaleAfter string `json:"stale_after,omitempty" bson:"stale_after,omitempty"`
	// Origin records where this subscription came from so dedup and cleanup
	// are possible: "forge:<repo_integration_id>" (orchestrator-generated,
	// deleted by Origin on deprovision), "operator" (studio), "schedule.yaml"
	// / "dispatcher.yaml" (local config load).
	Origin    string    `json:"origin,omitempty" bson:"origin,omitempty"`
	Enabled   bool      `json:"enabled" bson:"enabled"`
	CreatedBy string    `json:"created_by,omitempty" bson:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// NextFire computes the subscription's next-fire instant after `after`,
// the single seam that resolves the cron-vs-interval cadence (mirrors
// cloudsched.NextFireForBot for the resident scheduler). ok=false means
// this subscription has no timer cadence (not a schedule/keepalive kind,
// or missing cron/interval) and the scheduler skips it; a non-nil err is a
// malformed cron on an otherwise-eligible schedule subscription.
func (s Subscription) NextFire(after time.Time) (next time.Time, ok bool, err error) {
	switch {
	case s.Invocation == bundle.InvocationKindSchedule && s.Cron != "":
		sched, perr := cron.ParseStandard(s.Cron)
		if perr != nil {
			return time.Time{}, true, perr
		}
		return sched.Next(after), true, nil
	case s.Invocation == bundle.InvocationKindKeepalive && s.IntervalSeconds > 0:
		return after.Add(time.Duration(s.IntervalSeconds) * time.Second), true, nil
	default:
		return time.Time{}, false, nil
	}
}

// Policy projects the subscription's schedgate fields into a
// normalized overlap/guard policy.
func (s Subscription) Policy() schedgate.Policy {
	return schedgate.Normalize(schedgate.Policy{
		Overlap:       s.Overlap,
		MaxConcurrent: s.MaxConcurrent,
		Guard:         s.Guard,
		GuardTimeout:  s.GuardTimeout,
		GuardVar:      s.GuardVar,
		StaleAfter:    s.StaleAfter,
	})
}

// EffectiveMode returns the subscription's execution mode, defaulting an empty
// value to ExecutionDirect (mirrors bundle.Invocation.EffectiveMode).
func (s Subscription) EffectiveMode() bundle.ExecutionMode {
	if s.Mode == bundle.ExecutionBoard {
		return bundle.ExecutionBoard
	}
	return bundle.ExecutionDirect
}

// FromBoardInvocation derives a board-trigger Subscription from a bot's
// kind=board invocation that carries a board: block. A plain kind=board
// invocation with no board: block stays poll-only (the legacy dispatcher
// target) and yields no subscription — opting into a board: block is what
// activates event-driven promotion. Returns ok=false for any other invocation.
//
// The caller supplies id (a uuid), tenant, and repo scope; the board block's
// On/ToStates/AllLabels become the Matcher. The default mode is board
// (promote the card so the dispatcher claims it); an explicit mode: direct
// launches the bot itself on the matching card event (with the card id in
// vars["issue_id"]) — the triage-style "run a bot ON the card without
// routing the card TO it" shape.
func FromBoardInvocation(id, tenantID, repo, botID, origin string, inv bundle.Invocation, now time.Time) (Subscription, bool) {
	if inv.Kind != bundle.InvocationKindBoard || inv.Board == nil {
		return Subscription{}, false
	}
	sub := baseSubscription(id, tenantID, repo, botID, origin, inv.Kind, now)
	sub.Mode = bundle.ExecutionBoard
	if inv.Mode == bundle.ExecutionDirect {
		sub.Mode = bundle.ExecutionDirect
		sub.ConsumeLabels = inv.Board.ConsumeLabels
	}
	sub.Match = Matcher{
		Sources:       []Source{SourceBoard},
		Kinds:         append([]string(nil), inv.Board.On...),
		SubjectStates: append([]string(nil), inv.Board.ToStates...),
		Labels:        append([]string(nil), inv.Board.AllLabels...),
	}
	sub.ArgsVar = inv.ArgsVar
	sub.Vars = copyStrMap(inv.ContextVars)
	return sub, true
}

// FromScheduleInvocation derives a schedule-trigger Subscription from a bot's
// kind=schedule invocation that carries a suggested_cron. The cron is advisory
// (the operator may retune it via the Triggers REST), but seeding it enabled
// makes the bot's recurring run work out of the box. Returns ok=false for any
// other invocation or a missing cron.
func FromScheduleInvocation(id, tenantID, repo, botID, origin string, inv bundle.Invocation, now time.Time) (Subscription, bool) {
	if inv.Kind != bundle.InvocationKindSchedule || inv.Schedule == nil {
		return Subscription{}, false
	}
	cronExpr := strings.TrimSpace(inv.Schedule.SuggestedCron)
	if cronExpr == "" {
		return Subscription{}, false
	}
	vars := copyStrMap(inv.Schedule.DefaultVars)
	if vars == nil {
		vars = copyStrMap(inv.ContextVars)
	}
	sub := baseSubscription(id, tenantID, repo, botID, origin, inv.Kind, now)
	sub.Mode = bundle.ExecutionDirect
	sub.Match = Matcher{Sources: []Source{SourceSchedule}}
	sub.Cron = cronExpr
	sub.Vars = vars
	sub.ArgsVar = inv.ArgsVar
	return sub, true
}

// FromKeepaliveInvocation derives an always-on Subscription from a bot's
// kind=keepalive invocation. The interval (validated >= KeepaliveMinInterval at
// manifest parse) becomes IntervalSeconds; the overlap policy is keepalive so
// the scheduler relaunches a fresh run each tick with at-most-one-live +
// staleness reaping. Returns ok=false for any other invocation or a
// missing/unparseable interval.
func FromKeepaliveInvocation(id, tenantID, repo, botID, origin string, inv bundle.Invocation, now time.Time) (Subscription, bool) {
	if inv.Kind != bundle.InvocationKindKeepalive || inv.Keepalive == nil {
		return Subscription{}, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(inv.Keepalive.Interval))
	if err != nil || d < bundle.KeepaliveMinInterval {
		return Subscription{}, false
	}
	vars := copyStrMap(inv.Keepalive.DefaultVars)
	if vars == nil {
		vars = copyStrMap(inv.ContextVars)
	}
	sub := baseSubscription(id, tenantID, repo, botID, origin, inv.Kind, now)
	sub.Mode = bundle.ExecutionDirect
	sub.Match = Matcher{Sources: []Source{SourceSchedule}}
	sub.IntervalSeconds = int(d.Round(time.Second) / time.Second)
	sub.Overlap = schedgate.OverlapKeepalive
	sub.StaleAfter = strings.TrimSpace(inv.Keepalive.StaleAfter)
	sub.Vars = vars
	sub.ArgsVar = inv.ArgsVar
	return sub, true
}

// baseSubscription fills the fields every manifest-derived subscription shares
// (identity, scope, lifecycle); each From* constructor sets the source-specific
// Mode/Match/Vars on top.
func baseSubscription(id, tenantID, repo, botID, origin string, kind bundle.InvocationKind, now time.Time) Subscription {
	return Subscription{
		ID:         id,
		TenantID:   tenantID,
		Repo:       repo,
		BotID:      botID,
		Invocation: kind,
		Origin:     origin,
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func copyStrMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}
