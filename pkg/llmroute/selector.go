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
// The bot dimension names the bot a cell reaches — its manifest name, the
// launch's bot id (`review-pr`) — or a path.Match glob over it. Node ids are
// bare per-workflow names that collide across bots, so a cell written at a
// level every bot of a team folds (the team's record, an integration that
// enables several bots) re-pins every same-named node unless it says whose.
// Optional on a node cell, where records predate it; REQUIRED on a
// route-scoped and on a supervisor cell, the classes D21 adds — `*/` is the
// explicit spelling of "every bot".
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
	// Bot is the bot dimension: "" reaches every bot, otherwise a bot
	// name or a path.Match glob over one.
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

// ReachesBot reports whether the selector's bot dimension reaches bot: a
// selector without one reaches every bot; a glob reaches the names it
// matches; an empty bot (a launch that names none — a plain `.bot` file) is
// reached by no bot-scoped selector.
func (s Selector) ReachesBot(bot string) bool {
	if s.Bot == "" {
		return true
	}
	if bot == "" {
		return false
	}
	if s.Bot == bot {
		return true
	}
	ok, err := path.Match(s.Bot, bot)
	return err == nil && ok
}

// NamesBot reports whether the selector's bot dimension names bot exactly —
// no glob — the cells a zero-match warning speaks for: a cell written for
// this bot by name whose target matches nothing in it is a typo, while a
// glob or an unscoped cell matching nothing in one bot is the fold's
// ordinary reach.
func (s Selector) NamesBot(bot string) bool {
	return s.Bot != "" && s.Bot == bot
}

// IsGatewayModel reports whether a model spec is a gateway route — the
// `openai_compatible/<id>` spelling dispatch reads as one
// (modelroute.Parse(spec).Gateway(); a drift test pins the two).
func IsGatewayModel(spec string) bool {
	i := strings.Index(spec, "/")
	return i > 0 && i < len(spec)-1 && spec[:i] == ProviderOpenAICompatible
}
