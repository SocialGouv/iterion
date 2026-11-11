package llmroute

import (
	"fmt"
	"path"
	"strings"
)

// The override selector grammar (ADR-129 §3, ADR-132 §2 D21, ADR-131 §8):
//
//	selector := [ bot "/" ] target
//	target   := "*" | "agent" | "judge" | "router" | node-id | node-glob
//	          | node-id "#" route          a node's `fallbacks:` route
//	          | "supervisor:" name         a declared supervisor
//
// The bot dimension names the bot a cell reaches: the launch's bot id (a
// catalogue bot's name, a team or platform bot's slug — `review-pr-pool`),
// the manifest name of the bundle the launch serves (`review-pr`, which a
// team fork keeps), or a path.Match glob over either. Node ids are bare
// per-workflow names that collide across bots, so a cell written at a level
// every bot of a team folds (the team's record, an integration that enables
// several bots) re-pins every same-named node unless it says whose.
// Optional on a node cell, where records predate it; REQUIRED on a
// route-scoped and on a supervisor cell, the classes D21 adds — `*/` is the
// explicit spelling of "every bot", the same reach as no bot part at all.
//
// The route part is the exact site key the walk records an authored route
// under (model.ProviderResolution.Pins): the route's name, or its index when
// the author named none. The supervisor part is the supervisor's declared
// name.

// Selector separators and prefixes of the grammar above.
const (
	// SelectorBotSeparator opens a selector's target after its bot.
	SelectorBotSeparator = "/"
	// SelectorRouteSeparator joins a node id and one of its routes.
	SelectorRouteSeparator = "#"
	// SelectorSupervisorPrefix opens a supervisor selector.
	SelectorSupervisorPrefix = "supervisor:"
)

// SelectorClass is what a selector's target reaches.
type SelectorClass int

const (
	// SelectorNode reaches LLM nodes' own routes: "*", a kind keyword, an
	// exact node id or an id glob.
	SelectorNode SelectorClass = iota
	// SelectorRoute reaches one authored `fallbacks:` route of one node.
	SelectorRoute
	// SelectorSupervisor reaches one declared supervisor.
	SelectorSupervisor
)

// Selector is a parsed override selector.
type Selector struct {
	// Bot is the bot dimension: "" and "*" reach every bot, otherwise a
	// bot id or manifest name, or a path.Match glob over one (MatchBot).
	Bot string
	// Target is the selector without its bot dimension — the spelling a
	// run's folded override list carries once the bot is resolved.
	Target string
	Class  SelectorClass
	// Node and Route are a route-scoped selector's halves; Supervisor is a
	// supervisor selector's name.
	Node       string
	Route      string
	Supervisor string
}

// ParseSelector reads a selector's shape. It judges the grammar alone —
// the provenance-key characters, whitespace and the per-class field rules
// are validateOverride's.
func ParseSelector(raw string) (Selector, error) {
	s := Selector{Target: raw}
	if bot, target, scoped := strings.Cut(raw, SelectorBotSeparator); scoped {
		if bot == "" {
			return Selector{}, fmt.Errorf("selector %q names an empty bot before %q", raw, SelectorBotSeparator)
		}
		if strings.ContainsAny(bot, SelectorRouteSeparator+":") {
			return Selector{}, fmt.Errorf("selector %q: bot %q carries %q or ':' — the bot dimension is a bot name (or a glob over one), before the target", raw, bot, SelectorRouteSeparator)
		}
		if strings.ContainsAny(bot, "*?[") {
			if _, err := path.Match(bot, ""); err != nil {
				return Selector{}, fmt.Errorf("selector %q: bot %q is not a valid glob: %v", raw, bot, err)
			}
		}
		if target == "" {
			return Selector{}, fmt.Errorf("selector %q names bot %q and no target", raw, bot)
		}
		if strings.Contains(target, SelectorBotSeparator) {
			return Selector{}, fmt.Errorf("selector %q carries more than one %q — one bot dimension, then the target", raw, SelectorBotSeparator)
		}
		s.Bot, s.Target = bot, target
	}
	switch {
	case strings.HasPrefix(s.Target, SelectorSupervisorPrefix):
		name := strings.TrimPrefix(s.Target, SelectorSupervisorPrefix)
		if name == "" {
			return Selector{}, fmt.Errorf("selector %q names no supervisor after %q", raw, SelectorSupervisorPrefix)
		}
		if strings.ContainsAny(name, "*?[#:") {
			return Selector{}, fmt.Errorf("selector %q: supervisor %q — a supervisor selector names one declared supervisor exactly", raw, name)
		}
		s.Class, s.Supervisor = SelectorSupervisor, name
	case strings.Contains(s.Target, SelectorRouteSeparator):
		node, route, _ := strings.Cut(s.Target, SelectorRouteSeparator)
		if node == "" || route == "" {
			return Selector{}, fmt.Errorf("selector %q: a route-scoped selector is <node>#<route>, both named", raw)
		}
		if strings.ContainsAny(node, "*?[:") || strings.ContainsAny(route, "*?[#:") {
			return Selector{}, fmt.Errorf("selector %q: a route-scoped selector names one node and one of its routes exactly — the site key the walk records the route under", raw)
		}
		s.Class, s.Node, s.Route = SelectorRoute, node, route
	default:
		if strings.ContainsAny(s.Target, "*?[") {
			if _, err := path.Match(s.Target, ""); err != nil {
				return Selector{}, fmt.Errorf("selector %q is not a valid glob: %v", raw, err)
			}
		}
		s.Class = SelectorNode
	}
	return s, nil
}

// BotIdentity is a launch's bot as the bot dimension reads it.
type BotIdentity struct {
	// ID is the launch's bot id: a catalogue bot's name, a stored bot's
	// slug. "" for a launch that names no bot (a plain `.bot` file).
	ID string
	// Name is the manifest name of the bundle the launch serves — the
	// second spelling a cell may name the bot by (a team fork keeps its
	// catalogue bot's name under its own slug). "" when the manifest names
	// none or names the ID itself.
	Name string
}

// BotMatch is how a selector's bot dimension reaches a launch's bot,
// ordered from no reach to the most specific: a cell of a higher match
// wins a site over a cell of a lower one on the same target.
type BotMatch int

const (
	// BotMatchNone: the cell is another bot's.
	BotMatchNone BotMatch = iota
	// BotMatchAll: no bot part, or `*/` — every bot, a launch naming none
	// included.
	BotMatchAll
	// BotMatchGlob: a glob over the bot id or the manifest name.
	BotMatchGlob
	// BotMatchName: the manifest name, exactly.
	BotMatchName
	// BotMatchID: the bot id, exactly.
	BotMatchID
)

// MatchBot reports how the selector's bot dimension reaches b. A launch
// naming no bot is reached by the every-bot spellings alone; a glob that
// matches the empty name reaches no such launch.
func (s Selector) MatchBot(b BotIdentity) BotMatch {
	switch {
	case s.Bot == "" || s.Bot == "*":
		return BotMatchAll
	case b.ID == "" && b.Name == "":
		return BotMatchNone
	case s.Bot == b.ID:
		return BotMatchID
	case b.Name != "" && s.Bot == b.Name:
		return BotMatchName
	case globMatches(s.Bot, b.ID) || (b.Name != "" && globMatches(s.Bot, b.Name)):
		return BotMatchGlob
	}
	return BotMatchNone
}

func globMatches(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// classFieldRule refuses a pin on a field the selector's class does not
// carry: a `fallbacks:` route runs at its node's reasoning effort and
// takes one provider hint, a supervisor names a model only. A routing cell
// (validateOverride) and a launch's own row (ValidateLaunchRow) pin the
// same sites, so they share the rule.
func classFieldRule(parsed Selector, sel, backend, provider, effort string) error {
	switch parsed.Class {
	case SelectorRoute:
		if effort != "" {
			return fmt.Errorf("selector %q pins effort %q on a fallbacks: route — a route runs at its node's reasoning effort and carries none of its own, so the pin would do nothing; pin the node instead", sel, effort)
		}
		if err := routeProviderIsOneHint(sel, provider); err != nil {
			return err
		}
	case SelectorSupervisor:
		// A supervisor's evaluator runs in process on claw and routes on
		// the spec's own prefix.
		for _, f := range []struct{ name, v string }{{"backend", backend}, {"provider", provider}, {"effort", effort}} {
			if f.v != "" {
				return fmt.Errorf("selector %q pins %s %q on a supervisor — a supervisor names a model only (its evaluator runs in process on claw and routes on the spec's prefix), so the pin would do nothing", sel, f.name, f.v)
			}
		}
	}
	return nil
}

// routeProviderIsOneHint refuses a route pin whose provider is not one
// hint. Dispatch sends a `fallbacks:` route's provider whole (the
// executor's resolveChainUnder): a `,` chain is never walked — the node's
// `fallbacks:` list is the route chain — and a `hint:model` step is never
// split, so no harness acts on such a value — a stray `,` included:
// "zai," reaches dispatch as "zai,", a hint no harness knows and the
// allowlist drop compares whole. A value carrying a `${…}` or `{{…}}`
// reference is left to the literal rule of its surface (validateOverride
// for a cell, ValidateLaunchRow for a row): a `${P:-x}` holds a ':' that is
// no step.
func routeProviderIsOneHint(sel, provider string) error {
	if strings.Contains(provider, "${") || strings.Contains(provider, "{{") {
		return nil
	}
	tokens := providerHintTokens(provider)
	switch {
	case len(tokens) > 1 || (len(tokens) == 0 && strings.Contains(provider, ",")):
		return fmt.Errorf("selector %q pins provider %q on a fallbacks: route — a route's provider is one hint, sent whole at dispatch and never walked as a chain (the node's fallbacks: list is the route chain); pin one hint", sel, provider)
	case len(tokens) == 1 && tokens[0].hasModel && tokens[0].modelRaw != "":
		return fmt.Errorf("selector %q pins provider %q on a fallbacks: route — a route's provider is one hint, sent whole at dispatch and never split into a hint:model step; write \"provider\": %q and \"model\": %q", sel, provider, tokens[0].raw, tokens[0].modelRaw)
	case len(tokens) == 1 && tokens[0].hasModel:
		return fmt.Errorf("selector %q pins provider %q on a fallbacks: route — a route's provider is one hint, sent whole at dispatch and never split into a hint:model step; write \"provider\": %q", sel, provider, tokens[0].raw)
	case len(tokens) == 1 && strings.Contains(provider, ","):
		return fmt.Errorf("selector %q pins provider %q on a fallbacks: route — a route's provider is one hint, sent whole at dispatch, where a ',' is part of the hint and separates nothing; write \"provider\": %q", sel, provider, tokens[0].raw)
	}
	return nil
}

// ValidateLaunchRow judges one of a launch's own override rows (the API
// body's model_overrides, the CLI's --model/--backend/--effort-for) by the
// selector grammar: a row reaches its own launch's run, so it carries no bot
// dimension, and it obeys the per-class field rules a routing cell obeys.
// The rest of a cell's checks stay the routing write surfaces': a launch's
// rows compose with the folded cells, and the launch judges the composed
// pairs on the folded list.
func ValidateLaunchRow(selector, backend, provider, effort string) error {
	parsed, err := ParseSelector(selector)
	if err != nil {
		return err
	}
	if parsed.Bot != "" {
		return fmt.Errorf("selector %q names bot %q — a launch's own rows reach its own run; drop the prefix (a bot-scoped cell belongs in a routing record or llm_routing)", selector, parsed.Bot)
	}
	// A route row's provider is a literal, as a cell's is (validateOverride):
	// dispatch sends a route's expanded provider whole, so a template would
	// reach it as a value no launch judged. A node row keeps its template:
	// dispatch splits a node chain step by step.
	if parsed.Class == SelectorRoute && (strings.Contains(provider, "${") || strings.Contains(provider, "{{")) {
		return fmt.Errorf("selector %q pins provider %q on a fallbacks: route — a route row's provider is a LITERAL: the runner expands a template and sends the result whole, so the launch could certify no one hint; write the hint", selector, provider)
	}
	return classFieldRule(parsed, selector, backend, provider, effort)
}

// IsGatewayModel reports whether a model spec is a gateway route — the
// `openai_compatible/<id>` spelling dispatch reads as one
// (modelroute.Parse(spec).Gateway(); a drift test pins the two).
func IsGatewayModel(spec string) bool {
	i := strings.Index(spec, "/")
	return i > 0 && i < len(spec)-1 && spec[:i] == ProviderOpenAICompatible
}
