package runview

import (
	"fmt"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The ONE launch fold of the routing policy's override cells under the
// launch's own model_overrides (ADR-129 §6). The ADR's precedence line:
//
//	node DSL field < policy-resolved overrides (platform < org < team < bot
//	< binding < run routing layer) < the launch's own model_overrides
//
// The policy levels resolve through the routing snapshot
// (resolveRunLLMRoutePolicy → store.RunLLMRoutePolicy), whose Overrides
// carry the winning cell per (selector, field). The launch folds them ONCE,
// through this helper, and the folded list is what the launch validates
// against, executes with, stamped on the run document and published on the
// queue message — so a resume replays what the launch resolved instead of
// re-deriving the pins from a policy record that may have changed since.
//
// Specificity is NOT precedence. Within one layer, ForNode resolves per
// field by selector specificity; across layers, the fold implements the
// launch's win at the two shapes the flat list can express: a launch row
// wins a policy cell on the SAME selector (the launch's rows sit after the
// policy's, and the later rule wins a tie), and a launch row pinning a
// field on "*" shadows every policy cell on that field outright. The
// residual: a policy cell on a MORE SPECIFIC selector than any launch row
// pinning the same field still wins that field on specificity inside the
// folded list — carrying the layer across the wire would take ranks on the
// wire twins (the queue schema), so until then a launcher states a
// field-wide gesture with "*" or repeats the exact selector.
//
// A local (CLI) launch resolves no policy at all — its other levels are
// server records (ADR-129 §8, the stated limit) — so its overrides stay
// flags-only and the helper is a no-op there (nil policy).
//
// The bot dimension (ADR-132 §2, D21) resolves HERE, the one place a
// launch's bot and its policy cells meet: a cell `<bot>/<target>` whose bot
// does not reach the launch's (LaunchSpec.BotID) drops, one that does folds
// as its bare target — the spelling every reader downstream matches — after
// every unscoped cell, so it wins a tie of equal node specificity (a more
// specific selector wins wherever it is written). The snapshot keeps the
// cells as the levels wrote them; the folded list is this bot's.

// overrideOrigin names the layer that wrote a folded override entry —
// the level half of the composed-pair refusal's naming (#2637).
type overrideOrigin string

const (
	// originPolicy marks a cell the resolved routing policy pinned.
	originPolicy overrideOrigin = "the routing policy's override cell"
	// originLaunch marks a row the launch body itself carried.
	originLaunch overrideOrigin = "the launch's own override row"
)

// sourcedOverrideEntry is one folded entry beside the layer that wrote
// it. The flat []ModelOverrideEntry list stays every wire shape; the
// origin lives only inside the launch's validation walk.
type sourcedOverrideEntry struct {
	ModelOverrideEntry
	origin overrideOrigin
}

// EffectiveModelOverrides folds the resolved routing policy's override
// cells under the launch's own rows and returns the ONE list a launch
// carries onward: validated against, executed with, stamped on the run
// document, published on the queue message. A resume replays it.
func EffectiveModelOverrides(
	launchRows []ModelOverrideEntry,
	pol *store.RunLLMRoutePolicy,
	botID string,
) []ModelOverrideEntry {
	return plainModelOverrideEntries(foldSourcedOverrides(launchRows, pol, botID))
}

// plainModelOverrideEntries drops the origins: the wire shape the launch
// carries onward.
func plainModelOverrideEntries(sourced []sourcedOverrideEntry) []ModelOverrideEntry {
	out := make([]ModelOverrideEntry, len(sourced))
	for i, e := range sourced {
		out[i] = e.ModelOverrideEntry
	}
	return out
}

// foldSourcedOverrides is EffectiveModelOverrides' single body, keeping
// each folded entry's origin beside it. The composed-pair refusal names
// the two entries (and their levels) that produced a pair, so the fold
// must remember where each entry came from — one body, or the naming and
// the fold would drift.
func foldSourcedOverrides(
	launchRows []ModelOverrideEntry,
	pol *store.RunLLMRoutePolicy,
	botID string,
) []sourcedOverrideEntry {
	policy := policyOverrideEntries(pol, botID)
	// A launch row pinning a field on "*" shadows every policy cell on
	// that field: the launcher's canonical gesture (a dropdown / bare flag
	// records "*") must beat a more specific policy selector on score
	// alone, and "*" matches every node any policy selector can, so the
	// shadowed cell can never decide — dropping it here is what makes the
	// flat folded list resolve, on the runner and on resume, exactly as
	// the launch resolved it.
	var shadow struct{ backend, model, provider, effort bool }
	for _, r := range launchRows {
		if r.Selector != "*" {
			continue
		}
		shadow.backend = shadow.backend || r.Backend != ""
		shadow.model = shadow.model || r.Model != ""
		shadow.provider = shadow.provider || r.Provider != ""
		shadow.effort = shadow.effort || r.Effort != ""
	}
	hadShadow := shadow.backend || shadow.model || shadow.provider || shadow.effort
	out := make([]sourcedOverrideEntry, 0, len(policy)+len(launchRows))
	for _, o := range policy {
		if scopedTarget(o.Selector) {
			// A route-scoped or supervisor cell pins a site no node row
			// reaches: the launch's "*" re-targets nodes, never a route's
			// own fields nor a supervisor's model, so it shadows nothing
			// here. A launch row on the same scoped selector still wins
			// the tie (it sits after).
			out = append(out, sourcedOverrideEntry{ModelOverrideEntry: o, origin: originPolicy})
			continue
		}
		if shadow.backend {
			o.Backend = ""
		}
		if shadow.model {
			o.Model = ""
		}
		if shadow.provider {
			o.Provider = ""
		}
		if shadow.effort {
			o.Effort = ""
		}
		if hadShadow && o.Backend == "" && o.Model == "" && o.Provider == "" && o.Effort == "" {
			continue // the cell is fully shadowed
		}
		out = append(out, sourcedOverrideEntry{ModelOverrideEntry: o, origin: originPolicy})
	}
	for _, r := range launchRows {
		out = append(out, sourcedOverrideEntry{ModelOverrideEntry: r, origin: originLaunch})
	}
	return out
}

// policyOverrideEntries adapts the resolved snapshot's cells onto the
// launch-entry shape — the "a tiny adapter per site" doctrine
// (pkg/backend/model/override_fold.go's OverrideEntry).
//
// The bot dimension resolves here: a cell scoped to another bot drops, one
// scoped to botID folds as its bare target, after the unscoped cells (see
// the file header). A selector the grammar cannot read stays as written —
// it matches nothing; the launch chokepoint (llmroute.Validate at
// resolveRunLLMRoutePolicy) refuses such a record before any fold.
func policyOverrideEntries(pol *store.RunLLMRoutePolicy, botID string) []ModelOverrideEntry {
	if pol == nil {
		return nil
	}
	out := make([]ModelOverrideEntry, 0, len(pol.Overrides))
	var scoped []ModelOverrideEntry
	for _, o := range pol.Overrides {
		e := ModelOverrideEntry{
			Selector: o.Selector,
			Backend:  o.Backend,
			Model:    o.Model,
			Provider: o.Provider,
			Effort:   o.Effort,
		}
		sel, err := llmroute.ParseSelector(o.Selector)
		if err != nil || sel.Bot == "" {
			out = append(out, e)
			continue
		}
		if !sel.ReachesBot(botID) {
			continue
		}
		e.Selector = sel.Target
		scoped = append(scoped, e)
	}
	return append(out, scoped...)
}

// scopedTarget reports whether a bot-free selector reaches a route or a
// supervisor rather than nodes.
func scopedTarget(selector string) bool {
	sel, err := llmroute.ParseSelector(selector)
	return err == nil && sel.Class != llmroute.SelectorNode
}

// unmatchedPolicyCells lists the policy cells written for botID BY NAME
// whose target reaches nothing in wf (model.SelectorReaches) — the
// zero-match warning ADR-132 §2 owes: a typo'd selector matches nothing and
// nothing else would say so. A cell scoped by a glob, or unscoped, that
// matches nothing in this bot is the fold's ordinary reach, not a typo.
// Sorted as the policy lists them; each selector once.
func unmatchedPolicyCells(wf *ir.Workflow, pol *store.RunLLMRoutePolicy, botID string) []string {
	if wf == nil || pol == nil || botID == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range pol.Overrides {
		sel, err := llmroute.ParseSelector(o.Selector)
		if err != nil || !sel.NamesBot(botID) || seen[o.Selector] {
			continue
		}
		seen[o.Selector] = true
		if !model.SelectorReaches(wf, sel.Target) {
			out = append(out, o.Selector)
		}
	}
	return out
}

// zeroMatchWarning renders one unmatched cell as the warning a launch logs
// and the write screen returns.
func zeroMatchWarning(botID, selector string) string {
	return fmt.Sprintf("routing: override cell %q names bot %q, and its target matches no node, fallbacks: route or supervisor of that bot — the cell pins nothing (a typo, or a site the bot no longer declares)", selector, botID)
}

// zeroMatchWarnings is unmatchedPolicyCells rendered as warnings.
func zeroMatchWarnings(wf *ir.Workflow, pol *store.RunLLMRoutePolicy, botID string) []string {
	var out []string
	for _, sel := range unmatchedPolicyCells(wf, pol, botID) {
		out = append(out, zeroMatchWarning(botID, sel))
	}
	return out
}

// zeroMatchWarned holds the warnings a launch already logged in this
// process — each names its bot and selector, so the warning is once per
// name, not once per run.
var zeroMatchWarned sync.Map

// logZeroMatchOnce logs each zero-match warning the first time this
// process meets it.
func logZeroMatchOnce(logger *iterlog.Logger, warnings []string) {
	for _, w := range warnings {
		if _, dup := zeroMatchWarned.LoadOrStore(w, true); dup || logger == nil {
			continue
		}
		logger.Warn("%s", w)
	}
}
