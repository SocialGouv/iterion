package runview

import (
	"fmt"
	"math"
	"strings"
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
// launch's bot and its policy cells meet — a subbot child's included: it
// folds its root run's snapshot again for its own bot (FoldForSubbotChild),
// through the same body. A cell `<bot>/<target>` whose bot
// part reaches neither the launch's bot id (LaunchSpec.BotID) nor the
// manifest name of the bundle it serves (LaunchSpec.BotManifestName) drops;
// one that does folds as its bare target — the spelling every reader
// downstream matches. Among the cells reaching one site, the node target's
// specificity decides first (ForNode, in the folded list), then the bot
// part — unscoped and `*/` (every bot) < a glob < the manifest name < the
// bot id (llmroute.BotMatch) — then the level: of two cells with the same
// bot match on the same target and field, the one whose provenance names
// the level nearer the launch (llmroute.SourceRank) folds and the other
// drops, the first written winning a tie of level. The folded list
// carries each bot-match class in turn, least specific first, so on one
// node score the later — more specific — class wins. The snapshot keeps
// the cells as the levels wrote them; the folded list is this bot's.

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
	bot llmroute.BotIdentity,
) []ModelOverrideEntry {
	return plainModelOverrideEntries(foldSourcedOverrides(launchRows, pol, bot))
}

// FoldForSubbotChild is the launch fold for a subbot child, the second
// caller of foldAndScreen's one body. A child is a run of its own bot: the
// routing policy its root run froze at launch folds again for the child's
// bot id and manifest name, through the fold and the screens a launch meets,
// and the zero-match warnings log once per process. The parent's folded list
// never reaches the child — its bot dimension resolved for the parent's bot —
// nor do the parent's own launch rows: a launch row applies to the run it
// launched. A nil policy folds to no row.
func FoldForSubbotChild(wf *ir.Workflow, rootPolicy *store.RunLLMRoutePolicy, botID, manifestName, permission string, logger *iterlog.Logger) ([]ModelOverrideEntry, error) {
	spec := &LaunchSpec{BotID: botID, BotManifestName: manifestName, LLMRoutePolicy: rootPolicy, Permission: permission}
	warnings, err := foldAndScreen(wf, spec)
	if err != nil {
		return nil, err
	}
	logZeroMatchOnce(logger, warnings)
	if len(spec.ModelOverrides) == 0 {
		return nil, nil
	}
	return spec.ModelOverrides, nil
}

// launchBotIdentity is the bot a launch folds the policy's cells for: its
// bot id, and the manifest name of the bundle it serves.
func launchBotIdentity(spec *LaunchSpec) llmroute.BotIdentity {
	name := strings.TrimSpace(spec.BotManifestName)
	if name == spec.BotID {
		name = ""
	}
	return llmroute.BotIdentity{ID: spec.BotID, Name: name}
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
	bot llmroute.BotIdentity,
) []sourcedOverrideEntry {
	policy := policyOverrideEntries(pol, bot)
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
// The bot dimension resolves here (see the file header): a cell whose bot
// part does not reach bot drops; the others fold as their bare targets, one
// cell per (bot match, target, field) — the level nearest the launch, then
// the first written — grouped by bot match, least specific first, each
// group in snapshot order. Each folded entry stays one written cell, with
// the fields it lost to a nearer cell blanked; an entry left with no field
// drops. A selector the grammar cannot read stays as written among the
// every-bot cells — it matches nothing; the launch chokepoint
// (llmroute.Validate at resolveRunLLMRoutePolicy) refuses such a record
// before any fold.
func policyOverrideEntries(pol *store.RunLLMRoutePolicy, bot llmroute.BotIdentity) []ModelOverrideEntry {
	if pol == nil {
		return nil
	}
	type folded struct {
		match llmroute.BotMatch
		entry ModelOverrideEntry
	}
	type cellKey struct {
		match         llmroute.BotMatch
		target, field string
	}
	type holder struct{ level, at int }
	cells := make([]folded, 0, len(pol.Overrides))
	held := map[cellKey]holder{}
	for _, o := range pol.Overrides {
		match := llmroute.BotMatchAll
		target := o.Selector
		if sel, err := llmroute.ParseSelector(o.Selector); err == nil {
			match = sel.MatchBot(bot)
			if match == llmroute.BotMatchNone {
				continue
			}
			target = sel.Target
		}
		at := len(cells)
		cells = append(cells, folded{match: match, entry: ModelOverrideEntry{
			Selector: target,
			Backend:  o.Backend,
			Model:    o.Model,
			Provider: o.Provider,
			Effort:   o.Effort,
		}})
		for _, field := range overrideEntryFields {
			if *field.of(&cells[at].entry) == "" {
				continue
			}
			level := cellLevel(pol.Sources, o.Selector, field.name)
			k := cellKey{match, target, field.name}
			if h, ok := held[k]; ok {
				if level >= h.level {
					*field.of(&cells[at].entry) = ""
					continue
				}
				*field.of(&cells[h.at].entry) = ""
			}
			held[k] = holder{level: level, at: at}
		}
	}
	out := make([]ModelOverrideEntry, 0, len(cells))
	for match := llmroute.BotMatchAll; match <= llmroute.BotMatchID; match++ {
		for _, c := range cells {
			e := c.entry
			if c.match != match || (e.Backend == "" && e.Model == "" && e.Provider == "" && e.Effort == "") {
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

// overrideEntryFields names the fields of a folded entry the way a cell's
// provenance key names them.
var overrideEntryFields = []struct {
	name string
	of   func(*ModelOverrideEntry) *string
}{
	{llmroute.OverrideFieldBackend, func(e *ModelOverrideEntry) *string { return &e.Backend }},
	{llmroute.OverrideFieldModel, func(e *ModelOverrideEntry) *string { return &e.Model }},
	{llmroute.OverrideFieldProvider, func(e *ModelOverrideEntry) *string { return &e.Provider }},
	{llmroute.OverrideFieldEffort, func(e *ModelOverrideEntry) *string { return &e.Effort }},
}

// unrankedLevel is the level of a cell whose provenance names no ranked
// layer — a snapshot built without llmroute.Resolve: after every ranked
// level, and equal among themselves, so the first written wins.
const unrankedLevel = math.MaxInt

// cellLevel reads the level that wrote one field of one cell, as a rank on
// the precedence line (llmroute.SourceRank): lower is nearer the launch.
func cellLevel(sources map[string]string, selector, field string) int {
	if rank, ok := llmroute.SourceRank(sources[llmroute.OverrideCellKey(selector, field)]); ok {
		return rank
	}
	return unrankedLevel
}

// scopedTarget reports whether a bot-free selector reaches a route or a
// supervisor rather than nodes.
func scopedTarget(selector string) bool {
	sel, err := llmroute.ParseSelector(selector)
	return err == nil && sel.Class != llmroute.SelectorNode
}

// unmatchedPolicyCells lists the policy cells written for bot BY NAME — its
// id or its manifest name — whose target reaches nothing in wf
// (model.SelectorReaches): the zero-match warning ADR-132 §2 owes, a typo'd
// selector matches nothing and nothing else would say so. A cell scoped by
// a glob, or reaching every bot, that matches nothing in this bot is the
// fold's ordinary reach, not a typo. Sorted as the policy lists them; each
// selector once.
func unmatchedPolicyCells(wf *ir.Workflow, pol *store.RunLLMRoutePolicy, bot llmroute.BotIdentity) []string {
	if wf == nil || pol == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range pol.Overrides {
		sel, err := llmroute.ParseSelector(o.Selector)
		if err != nil || sel.MatchBot(bot) < llmroute.BotMatchName || seen[o.Selector] {
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
// and the write screen returns, naming the spelling the cell reached the
// bot by.
func zeroMatchWarning(bot llmroute.BotIdentity, selector string) string {
	named := fmt.Sprintf("%q", bot.ID)
	if sel, err := llmroute.ParseSelector(selector); err == nil && sel.MatchBot(bot) == llmroute.BotMatchName {
		named = fmt.Sprintf("%q (the manifest name of %q)", bot.Name, bot.ID)
	}
	return fmt.Sprintf("routing: override cell %q names bot %s, and its target matches no node, fallbacks: route or supervisor of that bot — the cell pins nothing (a typo, or a site the bot no longer declares)", selector, named)
}

// zeroMatchWarnings is unmatchedPolicyCells rendered as warnings.
func zeroMatchWarnings(wf *ir.Workflow, pol *store.RunLLMRoutePolicy, bot llmroute.BotIdentity) []string {
	var out []string
	for _, sel := range unmatchedPolicyCells(wf, pol, bot) {
		out = append(out, zeroMatchWarning(bot, sel))
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
