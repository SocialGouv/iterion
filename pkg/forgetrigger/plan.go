// Package forgetrigger projects the bots of a forge integration onto the
// trigger spine (ADR-172). PlanIntegration derives, as a pure function, the
// subscriptions a labelled issue starts the integration's bots by; Projector
// writes that plan to the subscription store, behind the seam pkg/forge
// declares (forge.SubscriptionProjector); ManifestDigest names the manifests
// a plan was derived from, so a router can tell a stale row from a current
// one.
package forgetrigger

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// RoleBots are the deployment's role bots the legacy issue election reads.
type RoleBots struct{ Implementer, Reviewer string }

// Plan is what one integration projects: the rows its store must hold and
// the report of how each was decided.
type Plan struct {
	// Rows are the integration's forge subscriptions, sorted by bot then id.
	// A refused plan (Err) carries none.
	Rows   []trigger.Subscription
	Report forge.SubscriptionReport
	// Err is the *ProjectionError of a plan that must not be written: the
	// integration's stored rows stay as they are.
	Err error
}

// The codes of a ProjectionError.
const (
	// ProjectionOverlappingRows: one bot derives issue rows whose label sets
	// may claim the same label. It is the code the routing switch refuses a
	// lane with.
	ProjectionOverlappingRows = forge.RoutingRefusedOverlappingRows
	// ProjectionUnbuildableRow: a derived route yields no subscription — the
	// integration carries no id to bind the row to.
	ProjectionUnbuildableRow = "unbuildable_row"
)

// ErrProjection is the class of every ProjectionError.
var ErrProjection = errors.New("forgetrigger: projection refused")

// ProjectionError is a plan the projector refuses to write. Bot is the bot
// the refusal names and Rows the ids of its rows at fault; when several bots
// are at fault, Bot is the first of them in name order and Reason names them
// all.
type ProjectionError struct {
	Code   string
	Bot    string
	Rows   []string
	Reason string
}

func (e *ProjectionError) Error() string {
	return fmt.Sprintf("%v (%s): %s", ErrProjection, e.Code, e.Reason)
}

// Is places every refusal in the ErrProjection class.
func (e *ProjectionError) Is(target error) bool { return target == ErrProjection }

// The reasons of a row derived disabled, as its operator reads them.
const (
	disabledNoLabelFormat = "declares issue_labeled with no label while %d bots share %s: set its labels (bot_triggers) or enable it"
	disabledByOperator    = "disabled on the integration (bot_triggers.enabled is false): enable it there to start it again"
)

// candidate is one issue route of one bot while the plan decides it.
type candidate struct {
	bot   string
	route bundle.ForgeRoutingInvocation
	id    string
	// labels are the patterns the row claims when explicit is true; an
	// explicit empty set claims any label. A row that is not explicit has no
	// label of its own: enabled, it claims the residual.
	labels   []string
	explicit bool
	// overridden says the labels are the integration's bot_triggers, which
	// every row of the bot shares.
	overridden bool
	enabled    bool
	reason     string
	// exclude is the residual's exclusion set.
	exclude []string
}

// residual reports whether the row claims the labels no other enabled row
// declares.
func (c *candidate) residual() bool { return c.enabled && !c.explicit }

// PlanIntegration derives the forge subscriptions of one integration from
// its bots' manifests, its per-bot operator state (BotTriggers) and the
// legacy election of its webhook config. It is pure: the same input gives
// the same plan, whatever the order of in.Bots.
//
// Each issue route of each bot (bundle.ForgeIssueInvocations) derives one
// row. Its labels are the integration's bot_triggers labels for the bot when
// set, else none for a bot pinned on the residual, else the route's own. The
// row is enabled when bot_triggers says so; else when it has labels; else —
// a row with no label — when the bot is pinned on the residual, is the bot
// the legacy router elects for a labelled issue (webhooks.LegacyIssueWinner,
// its reviewer fallback left out), or is the only bot of the integration
// that claims the event. An enabled row with no label claims the residual:
// any added label except the patterns the other enabled rows declare. Every
// other row is derived disabled, with the reason its operator reads.
//
// The first projection of an integration (SubscriptionsAdoptedAt unset)
// reports, as adoption pins, the derived state of each bot the integration
// holds no bot_triggers entry for: on the residual, or disabled. Stored,
// the pins keep a label a manifest gains later from narrowing the one or
// enabling the other.
//
// A bot whose issue rows may claim the same label refuses the whole plan
// with a *ProjectionError (Err): a bot claims an event through one row.
func PlanIntegration(in forge.SubscriptionInput, roles RoleBots, now time.Time) Plan {
	ri := in.Integration
	bots := sortedBots(in.Bots)
	plan := Plan{Report: forge.SubscriptionReport{
		Digest:     ManifestDigest(DigestBots(ri), in.InvByBot, in.FrByBot, roles),
		HandPinned: handPinnedLaunchVars(in.Config.LaunchVars, bots, in.FrByBot),
	}}
	warn := func(format string, args ...any) {
		plan.Report.Warnings = append(plan.Report.Warnings, fmt.Sprintf(format, args...))
	}

	// Claimers: one candidate per issue route, its labels resolved.
	var cands []*candidate
	claimers := map[string]bool{}
	for _, bot := range bots {
		routes, warnings := bundle.ForgeIssueInvocations(in.FrByBot[bot], in.InvByBot[bot])
		for _, w := range warnings {
			warn("bot %s: %s", bot, w)
		}
		entry, pinned := ri.BotTriggers[bot]
		if pinned {
			for _, w := range botTriggerWarnings(entry, len(routes)) {
				warn("bot %s: %s", bot, w)
			}
		}
		for _, route := range routes {
			c := &candidate{
				bot:   bot,
				route: route,
				id:    trigger.DerivedForgeSubscriptionID(ri.TenantID, bot, route.Key, ri.ID),
			}
			switch {
			case pinned && entry.Labels != nil:
				c.labels, c.explicit, c.overridden = slices.Clone(entry.Labels), true, true
			case pinned && entry.Residual:
				// The residual pin outranks the manifest: a label the
				// manifest gains does not narrow what the bot serves.
			case len(route.Labels) > 0:
				c.labels, c.explicit = slices.Clone(route.Labels), true
			}
			cands = append(cands, c)
			claimers[bot] = true
		}
	}

	// First pass: which rows are enabled. It reads no other row's labels, so
	// the second pass can read every enabled row whatever the bot order.
	winner, reviewerFallback := legacyWinner(in.Config, roles)
	for _, c := range cands {
		entry, pinned := ri.BotTriggers[c.bot]
		derived := c.explicit || (pinned && entry.Residual) || c.bot == winner || len(claimers) == 1
		c.enabled = derived
		if pinned && entry.Enabled != nil {
			c.enabled = *entry.Enabled
		}
		switch {
		case c.enabled:
		case derived:
			c.reason = disabledByOperator
		default:
			c.reason = fmt.Sprintf(disabledNoLabelFormat, len(bots), ri.RepoFullName)
		}
	}

	// Second pass: a residual row excludes the patterns every other enabled
	// row declares, so a label a bot declares goes to that bot.
	for _, c := range cands {
		if !c.residual() {
			continue
		}
		declared := map[string]bool{}
		for _, other := range cands {
			if other == c || !other.enabled || !other.explicit {
				continue
			}
			for _, pattern := range other.labels {
				if key := labelKey(pattern); key != "" {
					declared[key] = true
				}
			}
		}
		c.exclude = slices.Sorted(maps.Keys(declared))
		if declared["*"] {
			warn("bot %s: its residual row claims no label: another enabled row of the integration claims every label (*)", c.bot)
		}
	}

	enabledBots := map[string]bool{}
	for _, c := range cands {
		if c.enabled {
			enabledBots[c.bot] = true
			if plan.Report.Enabled == nil {
				plan.Report.Enabled = map[string]map[string][]string{webhooks.LaneIssues: {}}
			}
			lane := plan.Report.Enabled[webhooks.LaneIssues]
			lane[c.bot] = append(lane[c.bot], c.claimedLabels()...)
			continue
		}
		if plan.Report.Disabled == nil {
			plan.Report.Disabled = map[string]string{}
		}
		if _, named := plan.Report.Disabled[c.bot]; !named {
			plan.Report.Disabled[c.bot] = c.reason
		}
	}
	if reviewerFallback != "" && !enabledBots[reviewerFallback] {
		warn("bot %s: the legacy route starts it on a labelled issue as the reviewer fallback (no default bot, no implementer in scope); no subscription is derived from that fallback: set its labels (bot_triggers) to keep it", reviewerFallback)
	}

	// One bot claims an event through one row.
	if overlaps := overlappingRows(cands); len(overlaps) > 0 {
		first := overlaps[0]
		refusal := &ProjectionError{Code: ProjectionOverlappingRows, Bot: first.bot}
		for _, o := range overlaps {
			plan.Report.Overlaps = append(plan.Report.Overlaps, o.String())
			if o.bot == first.bot {
				refusal.Rows = append(refusal.Rows, o.a.id, o.b.id)
			}
		}
		slices.Sort(refusal.Rows)
		refusal.Rows = slices.Compact(refusal.Rows)
		refusal.Reason = "a bot claims an added label through one row, and these rows may claim the same one: " +
			strings.Join(plan.Report.Overlaps, "; ")
		plan.Err = refusal
		return plan
	}

	target := trigger.ForgeTarget{TenantID: ri.TenantID, IntegrationID: ri.ID, Repo: ri.RepoFullName}
	for _, c := range cands {
		var labels, exclude []string
		switch {
		case c.explicit:
			labels = c.labels
		case c.enabled:
			exclude = c.exclude
		}
		vars, dropped := routeLaunchVars(in.FrByBot[c.bot], c.route)
		if len(dropped) > 0 {
			warn("bot %s: route %q: launch vars %s are server launch context, never a manifest's: dropped from its subscription",
				c.bot, c.route.Key, strings.Join(dropped, ", "))
		}
		row, ok := trigger.FromForgeIssueInvocation(c.id, target, c.bot, c.route, labels, exclude,
			vars, c.enabled, c.reason, plan.Report.Digest, now)
		if !ok {
			plan.Rows = nil
			plan.Err = &ProjectionError{Code: ProjectionUnbuildableRow, Bot: c.bot, Rows: []string{c.id},
				Reason: fmt.Sprintf("the %s route %q of bot %s derives no subscription on integration %q (tenant %q): a row is bound to the id of its integration",
					bundle.ForgeEventIssueLabeled, c.route.Key, c.bot, ri.ID, ri.TenantID)}
			return plan
		}
		plan.Rows = append(plan.Rows, row)
	}
	slices.SortFunc(plan.Rows, func(a, b trigger.Subscription) int {
		return cmp.Or(cmp.Compare(a.BotID, b.BotID), cmp.Compare(a.ID, b.ID))
	})

	if ri.SubscriptionsAdoptedAt == nil {
		plan.Report.AdoptionPins = adoptionPins(cands, ri.BotTriggers)
	}
	return plan
}

// legacyWinner returns the bot the legacy router elects for a labelled issue
// of cfg, when a row may stand for that election: the webhook's bot scope
// permits the bot, and the election did not reach its reviewer fallback.
// reviewerFallback names the reviewer that fallback starts, which no row is
// derived for.
func legacyWinner(cfg webhooks.Config, roles RoleBots) (winner, reviewerFallback string) {
	pick, permitted := webhooks.LegacyIssueWinner(cfg, roles.Implementer, roles.Reviewer)
	if pick == "" || !permitted {
		return "", ""
	}
	// Withholding the reviewer tells its fallback from a reviewer the config
	// names itself (a pinned default, the only bot in scope): the election
	// names nobody only when the fallback is what named the pick.
	if named, _ := webhooks.LegacyIssueWinner(cfg, roles.Implementer, ""); named == "" {
		return "", pick
	}
	return pick, ""
}

// claimedLabels renders, for the report, the labels an enabled row claims:
// its patterns, "*" for any label, or "*" followed by one "!pattern" per
// label the residual leaves to another row.
func (c *candidate) claimedLabels() []string {
	if c.explicit {
		if len(c.labels) == 0 {
			return []string{"*"}
		}
		return slices.Clone(c.labels)
	}
	out := []string{"*"}
	for _, pattern := range c.exclude {
		out = append(out, "!"+pattern)
	}
	return out
}

// botTriggerWarnings names what an operator set on a bot that the plan does
// not act on as written.
func botTriggerWarnings(entry forge.BotTrigger, routes int) []string {
	var out []string
	if routes == 0 {
		out = append(out, "bot_triggers is set, and the bot's manifest claims no issue_labeled route: no row derives from it")
	}
	if len(entry.LaunchVars) > 0 {
		out = append(out, "bot_triggers.launch_vars is stored and applied to no row yet: pin the vars on the integration's launch_vars")
	}
	if entry.Labels != nil && entry.Residual {
		out = append(out, "bot_triggers sets both labels and residual: the labels win")
	}
	if slices.ContainsFunc(entry.Labels, func(pattern string) bool { return labelKey(pattern) == "" }) {
		out = append(out, "bot_triggers.labels holds a blank pattern, which matches no label")
	}
	return out
}

// adoptionPins is the state a first projection freezes for each bot the
// integration holds no bot_triggers entry for: on the residual when one of
// its rows claims it, disabled when none of its rows is enabled. A bot
// enabled by its own labels is not pinned: its manifest keeps deciding them.
func adoptionPins(cands []*candidate, stored map[string]forge.BotTrigger) map[string]forge.BotTrigger {
	var residual, enabled, claims = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range cands {
		claims[c.bot] = true
		residual[c.bot] = residual[c.bot] || c.residual()
		enabled[c.bot] = enabled[c.bot] || c.enabled
	}
	pins := map[string]forge.BotTrigger{}
	for bot := range claims {
		if _, held := stored[bot]; held {
			continue
		}
		switch {
		case residual[bot]:
			pins[bot] = forge.BotTrigger{Residual: true}
		case !enabled[bot]:
			off := false
			pins[bot] = forge.BotTrigger{Enabled: &off}
		}
	}
	if len(pins) == 0 {
		return nil
	}
	return pins
}

// rowOverlap is two rows of one bot whose label sets may claim one label.
type rowOverlap struct {
	bot  string
	a, b *candidate
}

// String names the overlap as "bot: row a ∩ row b".
func (o rowOverlap) String() string {
	return fmt.Sprintf("%s: %s ∩ %s", o.bot, o.a.describe(), o.b.describe())
}

// describe names a row by its id and the labels it is compared on.
func (c *candidate) describe() string {
	labels := "[no label]"
	switch {
	case c.explicit && len(c.labels) == 0:
		labels = "[any label]"
	case c.explicit:
		labels = fmt.Sprint(c.labels)
	}
	if c.overridden {
		labels += " (bot_triggers, shared by every row of the bot)"
	}
	return c.id + " " + labels
}

// overlappingRows compares the issue rows of each bot pairwise, on the
// labels the plan resolved for them, and returns the pairs that may claim
// one label, by bot then by row id. Two rows that both carry the
// integration's bot_triggers labels always overlap: the override is one set
// for every row of the bot, whatever patterns it holds.
func overlappingRows(cands []*candidate) []rowOverlap {
	byBot := map[string][]*candidate{}
	for _, c := range cands {
		byBot[c.bot] = append(byBot[c.bot], c)
	}
	var out []rowOverlap
	for _, bot := range slices.Sorted(maps.Keys(byBot)) {
		rows := byBot[bot]
		slices.SortFunc(rows, func(a, b *candidate) int { return cmp.Compare(a.id, b.id) })
		for i, a := range rows {
			for _, b := range rows[i+1:] {
				if (a.overridden && b.overridden) || labelSetsMayIntersect(a.labels, b.labels) {
					out = append(out, rowOverlap{bot: bot, a: a, b: b})
				}
			}
		}
	}
	return out
}

// labelSetsMayIntersect reports whether two rows' label sets may claim the
// same added label: either holds no pattern (any label, or the residual),
// either holds a wildcard ("*", or a "*suffix"), or both hold one literal,
// compared as webhooks.MatchLabel compares it.
func labelSetsMayIntersect(a, b []string) bool {
	if holdsOpenPattern(a) || holdsOpenPattern(b) {
		return true
	}
	held := map[string]bool{}
	for _, pattern := range a {
		held[labelKey(pattern)] = true
	}
	for _, pattern := range b {
		if key := labelKey(pattern); key != "" && held[key] {
			return true
		}
	}
	return false
}

// holdsOpenPattern reports whether a label set claims labels it does not
// spell: it is empty, or one of its patterns is a wildcard.
func holdsOpenPattern(labels []string) bool {
	if len(labels) == 0 {
		return true
	}
	return slices.ContainsFunc(labels, func(pattern string) bool {
		return strings.HasPrefix(strings.TrimSpace(pattern), "*")
	})
}

// labelKey is a label pattern as webhooks.MatchLabel reads it: trimmed and
// case-insensitive.
func labelKey(pattern string) string {
	return strings.ToLower(strings.TrimSpace(pattern))
}

// routeLaunchVars are the launch vars a row stamps on its run: the bot's
// manifest launch vars, under the context vars of the invocation the route
// comes from. A manifest never writes what the server stamps or mints — the
// reserved launch context (native.ReservedLaunchArgs: the repo to clone, the
// webhook's key and secret overrides, …) and the minted publish grant — so
// those keys are dropped here and named in dropped, for the plan to report.
func routeLaunchVars(fr *bundle.ForgeRequirements, route bundle.ForgeRoutingInvocation) (vars map[string]string, dropped []string) {
	all := map[string]string{}
	maps.Copy(all, manifestLaunchVars(fr))
	maps.Copy(all, route.Inv.ContextVars)
	vars = native.DropServerMintedLaunchVars(all)
	for k := range all {
		if _, kept := vars[k]; !kept {
			dropped = append(dropped, k)
		}
	}
	slices.Sort(dropped)
	if len(vars) == 0 {
		return nil, dropped
	}
	return vars, dropped
}

// manifestLaunchVars are the launch vars a bot's manifest declares for the
// runs a forge event starts.
func manifestLaunchVars(fr *bundle.ForgeRequirements) map[string]string {
	if fr == nil || fr.Webhook == nil {
		return nil
	}
	return fr.Webhook.LaunchVars
}

// handPinnedLaunchVars returns, sorted, the keys on which the webhook
// config's launch vars differ from the union of the bots' manifest launch
// vars — held by one side alone, or by both with different values. The union
// is the one a provision writes on the config: bots in name order, a later
// bot's value over an earlier one's.
func handPinnedLaunchVars(configVars map[string]string, bots []string, frByBot map[string]*bundle.ForgeRequirements) []string {
	union := map[string]string{}
	for _, bot := range bots {
		maps.Copy(union, manifestLaunchVars(frByBot[bot]))
	}
	differ := map[string]bool{}
	for key, value := range configVars {
		if declared, ok := union[key]; !ok || declared != value {
			differ[key] = true
		}
	}
	for key := range union {
		if _, ok := configVars[key]; !ok {
			differ[key] = true
		}
	}
	if len(differ) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(differ))
}

// sortedBots returns the bot ids sorted, each once.
func sortedBots(bots []string) []string {
	out := slices.Clone(bots)
	slices.Sort(out)
	return slices.Compact(out)
}

// Widenings compares two plans of one integration — before and after a
// change of its bots or bot_triggers — and names, sorted, every row of the
// after plan that may start its bot on a label the before plan did not
// start it on: a row enabled that was not (or did not exist), a residual
// row that excludes a pattern less, a row that claims any label where it
// claimed a set, or an explicit set that gains a pattern. Patterns compare
// as webhooks.MatchLabel reads them (trimmed, case-insensitive); a pattern
// not literally held counts as new, so the comparison errs toward naming a
// widening. The residual couples the bots: narrowing, disabling or removing
// one bot widens the residual winner, which shows here as the winner's
// exclusions shrinking.
func Widenings(before, after Plan) []string {
	prev := map[string]trigger.Subscription{}
	for _, row := range before.Rows {
		prev[row.ID] = row
	}
	var out []string
	for _, row := range after.Rows {
		if !row.Enabled {
			continue
		}
		old, found := prev[row.ID]
		if !found || !old.Enabled {
			out = append(out, fmt.Sprintf("bot %s: row %s starts on %s", row.BotID, row.ID, describeClaim(row.Match)))
			continue
		}
		if gained := claimGains(old.Match, row.Match); len(gained) > 0 {
			out = append(out, fmt.Sprintf("bot %s: row %s now also claims %s", row.BotID, row.ID, strings.Join(gained, ", ")))
		}
	}
	slices.Sort(out)
	return out
}

// claimGains names what next's added-label dimension may claim that prev's
// did not. An empty AddedLabels claims any label not excluded.
func claimGains(prev, next trigger.Matcher) []string {
	prevAny, nextAny := len(prev.AddedLabels) == 0, len(next.AddedLabels) == 0
	prevExcl := keySet(prev.ExcludeAddedLabels)
	switch {
	case nextAny && !prevAny:
		return []string{describeClaim(next)}
	case nextAny && prevAny:
		nextExcl := keySet(next.ExcludeAddedLabels)
		var gained []string
		for _, p := range slices.Sorted(maps.Keys(prevExcl)) {
			if !nextExcl[p] {
				gained = append(gained, p)
			}
		}
		return gained
	case !nextAny && prevAny:
		var gained []string
		for _, p := range next.AddedLabels {
			if k := labelKey(p); prevExcl[k] || (k == "*" && len(prevExcl) > 0) {
				gained = append(gained, k)
			}
		}
		return gained
	}
	held := keySet(prev.AddedLabels)
	if held["*"] {
		return nil
	}
	var gained []string
	for _, p := range next.AddedLabels {
		if k := labelKey(p); k != "" && !held[k] {
			gained = append(gained, k)
		}
	}
	return gained
}

// describeClaim renders a row's added-label claim for a widening's message.
func describeClaim(m trigger.Matcher) string {
	if len(m.AddedLabels) > 0 {
		return "[" + strings.Join(m.AddedLabels, ", ") + "]"
	}
	if len(m.ExcludeAddedLabels) > 0 {
		return "any label except [" + strings.Join(m.ExcludeAddedLabels, ", ") + "]"
	}
	return "any label"
}

// keySet is the set of patterns as labelKey reads them, blanks dropped.
func keySet(patterns []string) map[string]bool {
	out := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		if k := labelKey(p); k != "" {
			out[k] = true
		}
	}
	return out
}

// ResidualTransfers names, sorted, every label a plan takes from a residual
// row of the stored projection and hands to a bot that declares it: the
// residual row stays enabled and excludes the label now, and an enabled row
// of another bot claims it by its own pattern. One warning per label and
// pair of bots, naming the three.
func ResidualTransfers(stored []trigger.Subscription, plan Plan) []string {
	prev := map[string]trigger.Subscription{}
	for _, row := range stored {
		prev[row.ID] = row
	}
	seen := map[string]bool{}
	var out []string
	for _, row := range plan.Rows {
		old, found := prev[row.ID]
		if !found || !old.Enabled || !row.Enabled || len(old.Match.AddedLabels) > 0 || len(row.Match.AddedLabels) > 0 {
			continue
		}
		before := keySet(old.Match.ExcludeAddedLabels)
		for _, label := range slices.Sorted(maps.Keys(keySet(row.Match.ExcludeAddedLabels))) {
			if before[label] {
				continue
			}
			for _, to := range plan.Rows {
				if to.BotID == row.BotID || !to.Enabled || !keySet(to.Match.AddedLabels)[label] {
					continue
				}
				msg := fmt.Sprintf("label %q moves from bot %s (the residual) to bot %s, whose manifest now declares it: a label a bot declares goes to that bot — set %s's labels (bot_triggers) to give it back",
					label, row.BotID, to.BotID, to.BotID)
				if !seen[msg] {
					seen[msg] = true
					out = append(out, msg)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}
