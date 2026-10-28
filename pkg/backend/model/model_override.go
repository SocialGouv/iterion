package model

import (
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// NodeModelOverride is the resolved launch-time override for a single node:
// which backend / model / provider / reasoning effort to use instead of the
// node's DSL values. Empty fields mean "leave the node's resolved value
// unchanged".
type NodeModelOverride struct {
	Backend  string
	Model    string
	Provider string
	// Effort re-targets reasoning_effort. It travels with model and backend
	// because the three are one decision: a model, the backend that drives it
	// and how hard it is asked to think. Splitting them left the studio able
	// to retarget a node's model but not the effort that model is worth.
	Effort string
}

// Empty reports whether the override carries no directive (all fields blank).
func (o NodeModelOverride) Empty() bool {
	return o.Backend == "" && o.Model == "" && o.Provider == "" && o.Effort == ""
}

// modelOverrideRule is one selector→override directive, in insertion order.
type modelOverrideRule struct {
	selector string
	// rank is the layer the rule belongs to. Rules MergeOver stamps land
	// one rank above the base's highest, and ForNode compares
	// (rank, score) lexicographically: the layer decides BEFORE selector
	// specificity, so the launcher's least-specific gesture — `--backend
	// claw` and the studio dropdown both record "*" — still outranks a
	// more specific rule from a lower layer. The precedence line it makes
	// true at every selector pair: node DSL field < policy-resolved
	// overrides < the launch's own model_overrides.
	rank int
	NodeModelOverride
}

// ModelOverrides is an ordered set of launch-time per-node/per-group
// backend+model+provider overrides, populated from the studio Launch
// dropdowns or the CLI --model / --backend flags. The zero value applies
// nothing, so a run with no overrides behaves exactly as before.
//
// A selector matches a node by (most specific first):
//   - exact node id            ("reviewer_claude")
//   - id glob                  ("reviewer_*", "fix_*")   — path.Match syntax
//   - node-kind keyword        ("agent", "judge", "router")
//   - "*"                      every LLM node (run-level default)
//
// For each field (backend/model/provider) the most specific matching rule
// that sets a non-empty value wins within a layer; across layers the
// highest rank wins first (see MergeOver), and ties resolve to the later
// rule. Because
// resolution is per-field, `--model 'reviewer_*=X'` and `--backend '*=claw'`
// compose (the reviewer gets model X on backend claw). Overrides sit at the
// TOP of the resolution chain — above the node's DSL backend:/model: — because
// the operator is explicitly re-targeting the bot at launch without editing
// the .bot.
type ModelOverrides struct {
	rules []modelOverrideRule
}

// Empty reports whether no rules are configured.
func (o ModelOverrides) Empty() bool { return len(o.rules) == 0 }

// SetModel adds a model directive for the selector.
func (o *ModelOverrides) SetModel(selector, model string) {
	o.add(selector, NodeModelOverride{Model: model})
}

// SetBackend adds a backend directive for the selector.
func (o *ModelOverrides) SetBackend(selector, backend string) {
	o.add(selector, NodeModelOverride{Backend: backend})
}

// SetProvider adds a provider directive for the selector.
func (o *ModelOverrides) SetProvider(selector, provider string) {
	o.add(selector, NodeModelOverride{Provider: provider})
}

// SetEffort adds a reasoning-effort directive for the selector. The value is
// validated by the caller (HTTP handler / CLI flag parser) against
// ir.ValidReasoningEfforts — an unknown level here would reach the provider.
func (o *ModelOverrides) SetEffort(selector, effort string) {
	o.add(selector, NodeModelOverride{Effort: effort})
}

// Row is one selector→override directive as parsed, in insertion order.
// It is the shape a launch surface persists on the run document so a later
// resume can read the choice back.
type Row struct {
	Selector string
	NodeModelOverride
}

// Rows returns the directives in insertion order.
//
// Exported so a launch path can PERSIST what it parsed. Folding to
// ModelOverrides is lossy for that purpose — resolution is per-field and
// per-selector, so there is no way back to "what the operator typed" from
// the folded set, and a launch that cannot record its choice cannot have it
// replayed on resume.
func (o ModelOverrides) Rows() []Row {
	if len(o.rules) == 0 {
		return nil
	}
	out := make([]Row, 0, len(o.rules))
	for _, r := range o.rules {
		out = append(out, Row{Selector: r.selector, NodeModelOverride: r.NodeModelOverride})
	}
	return out
}

// MergeOver layers o's rules ON TOP of base: every rule of o lands above
// base's highest rank, so a directive in o wins ANY rule of base — not
// only an equally-specific one — while still inheriting the fields base
// sets and o leaves blank. o's own inner order is preserved (each rule is
// stamped relative to its own rank), so layering the output of an earlier
// MergeOver keeps its layers stacked.
//
// This is what lets a resume keep the model the run was launched with while
// still honouring a flag the operator typed for this attempt: pass the run's
// persisted rows as base and the freshly parsed flags as o. Resolution is
// per-field (see ForNode), so `--effort-for '*=high'` alone re-targets the
// effort without discarding the launch's model.
func (o ModelOverrides) MergeOver(base ModelOverrides) ModelOverrides {
	if len(o.rules) == 0 {
		return base
	}
	if len(base.rules) == 0 {
		return o
	}
	top := 0
	for _, r := range base.rules {
		if r.rank > top {
			top = r.rank
		}
	}
	merged := ModelOverrides{rules: make([]modelOverrideRule, 0, len(base.rules)+len(o.rules))}
	merged.rules = append(merged.rules, base.rules...)
	for _, r := range o.rules {
		// Stamped relative to o's own rank: a single-layer o (every rule
		// at rank 0) lands at top+1 exactly as before, and a LAYERED o
		// (the output of an earlier MergeOver) keeps its inner precedence
		// instead of collapsing onto one rank — where base's most
		// specific rule would beat it on score alone.
		r.rank = top + 1 + r.rank
		merged.rules = append(merged.rules, r)
	}
	return merged
}

func (o *ModelOverrides) add(selector string, ov NodeModelOverride) {
	selector = strings.TrimSpace(selector)
	if selector == "" || ov.Empty() {
		return
	}
	o.rules = append(o.rules, modelOverrideRule{selector: selector, NodeModelOverride: ov})
}

// FieldSource names the rule that won a field for a node: the selector
// the winning rule was written under. Zero value = the field stayed
// unset. A caller matches it against the entries it folded to name where
// a composed field came from.
type FieldSource struct {
	Selector string
}

// ForNode resolves the effective override for a node id + kind. Returns an
// empty override when nothing matches (the caller then keeps the node's DSL
// values). Resolution is per-field: the (rank, score) pair decides, compared
// lexicographically — the layer first, then selector specificity within it —
// with the later rule winning a full tie. A rule of a higher rank therefore
// beats a more specific rule of a lower one (the precedence line), while
// rules of the same layer keep the specificity ordering.
func (o ModelOverrides) ForNode(id string, kind ir.NodeKind) NodeModelOverride {
	out, _, _ := o.ForNodeFieldSources(id, kind)
	return out
}

// ForNodeFieldSources resolves exactly as ForNode and additionally names,
// for the fields the composed-pair check reads (backend, provider), the
// rule that won each. ONE walk: ForNode delegates here, so the pair's
// provenance and the resolution it names cannot drift (#2637).
func (o ModelOverrides) ForNodeFieldSources(id string, kind ir.NodeKind) (NodeModelOverride, FieldSource, FieldSource) {
	var out NodeModelOverride
	var backendSrc, providerSrc FieldSource
	type best struct{ rank, score int }
	// Seeded below every rank: a rule's rank is a layer, and a producer
	// BELOW the DSL layer stamps -1 — a rule is never dropped for its
	// layer alone.
	const neverBeaten = math.MinInt
	bb, bm, bp, be := best{rank: neverBeaten}, best{rank: neverBeaten}, best{rank: neverBeaten}, best{rank: neverBeaten}
	takes := func(rank, score int, cur best) bool {
		return rank > cur.rank || (rank == cur.rank && score >= cur.score)
	}
	for _, r := range o.rules {
		score, ok := selectorScore(r.selector, id, kind)
		if !ok {
			continue
		}
		if r.Backend != "" && takes(r.rank, score, bb) {
			out.Backend, bb = r.Backend, best{r.rank, score}
			backendSrc = FieldSource{Selector: r.selector}
		}
		if r.Model != "" && takes(r.rank, score, bm) {
			out.Model, bm = r.Model, best{r.rank, score}
		}
		if r.Provider != "" && takes(r.rank, score, bp) {
			out.Provider, bp = r.Provider, best{r.rank, score}
			providerSrc = FieldSource{Selector: r.selector}
		}
		if r.Effort != "" && takes(r.rank, score, be) {
			out.Effort, be = r.Effort, best{r.rank, score}
		}
	}
	return out, backendSrc, providerSrc
}

// ConsumesProviderHint reports how a backend consumes the per-node provider
// hint: chains=true walks a multi-step chain (claude_code); single=true
// honours one step and derives the rest from the model prefix (claw); both
// false = the backend does not consume the hint (codex, pi, grok, kimi,
// opencode). The predicate behind providerFallbackEligible's three classes,
// exported so the routing-policy vocabulary (pkg/llmroute, which imports
// nothing of iterion) can carry a local twin with a drift test against THIS
// function instead of against a hope.
func ConsumesProviderHint(backend string) (chains, single bool) {
	switch backend {
	case delegate.BackendClaudeCode:
		return true, false
	case delegate.BackendClaw:
		return false, true
	default:
		return false, false
	}
}

// HonoursProviderHintValue reports whether a backend honours the given
// provider value as a pin at its dispatch chokepoint — the value-aware
// half of ConsumesProviderHint. claude_code walks every step of the chain
// it is given and refuses an unfunded hint loudly at dispatch
// (facadeHintRefusal), so any validated value is honoured. claw acts on
// the hint at exactly one switch — the registry's force-Anthropic-direct
// branch, wired to THIS predicate so the gate and the runtime read one
// function — and ignores every other value there, letting the spend
// follow the default precedence: the silent misroute a policy pin must
// not certify. The routing policy's local twin (pkg/llmroute)
// drift-tests against this function.
func HonoursProviderHintValue(backend, provider string) bool {
	switch backend {
	case delegate.BackendClaudeCode:
		return true
	case delegate.BackendClaw:
		return strings.ToLower(strings.TrimSpace(provider)) == "anthropic"
	default:
		return false
	}
}

// selectorScore returns a specificity score (higher = more specific) and
// whether the selector matches the node. Ordering: "*" < kind keyword < glob
// (by literal length) < exact id.
func selectorScore(selector, id string, kind ir.NodeKind) (int, bool) {
	switch selector {
	case "*":
		return 1, true
	case "agent":
		return 10, kind == ir.NodeAgent
	case "judge":
		return 10, kind == ir.NodeJudge
	case "router":
		return 10, kind == ir.NodeRouter
	}
	if selector == id {
		return 100000, true
	}
	if strings.ContainsAny(selector, "*?[") {
		if ok, err := path.Match(selector, id); err == nil && ok {
			return 100 + literalLen(selector), true
		}
		return 0, false
	}
	return 0, false
}

// literalLen counts the non-wildcard characters of a glob, used to rank two
// matching globs (the more literal one is more specific).
func literalLen(pat string) int {
	n := 0
	for _, r := range pat {
		if r != '*' && r != '?' && r != '[' && r != ']' {
			n++
		}
	}
	return n
}

// splitSelectorValue parses a "selector=value" flag argument. A bare value
// with no '=' targets every LLM node (selector "*"), so `--model X` sets X
// run-wide while `--model 'fix_*=X'` targets a group.
func splitSelectorValue(arg string) (selector, value string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("empty override")
	}
	i := strings.Index(arg, "=")
	if i < 0 {
		return "*", arg, nil
	}
	selector = strings.TrimSpace(arg[:i])
	value = strings.TrimSpace(arg[i+1:])
	if selector == "" {
		selector = "*"
	}
	if value == "" {
		return "", "", fmt.Errorf("override %q has empty value", arg)
	}
	return selector, value, nil
}

// ParseModelOverrides builds a ModelOverrides from repeatable CLI flag values.
// Each element of modelFlags/backendFlags/effortFlags is a "selector=value" (or
// a bare "value" targeting every node). Returns a descriptive error on a
// malformed element so the run fails fast instead of silently ignoring a typo.
//
// An effort value is checked against ir.ValidReasoningEfforts here: unlike a
// model or backend name (host state this process cannot enumerate), the effort
// levels are a closed set that reaches the provider verbatim, so a typo has to
// die at the flag rather than on the run's first node.
func ParseModelOverrides(modelFlags, backendFlags, effortFlags []string) (ModelOverrides, error) {
	var o ModelOverrides
	for _, m := range modelFlags {
		sel, val, err := splitSelectorValue(m)
		if err != nil {
			return o, fmt.Errorf("--model: %w", err)
		}
		o.SetModel(sel, val)
	}
	for _, b := range backendFlags {
		sel, val, err := splitSelectorValue(b)
		if err != nil {
			return o, fmt.Errorf("--backend: %w", err)
		}
		o.SetBackend(sel, val)
	}
	for _, ef := range effortFlags {
		sel, val, err := splitSelectorValue(ef)
		if err != nil {
			return o, fmt.Errorf("--effort-for: %w", err)
		}
		if !ir.ValidReasoningEfforts[val] {
			return o, fmt.Errorf("--effort-for: %q is not a reasoning effort", val)
		}
		o.SetEffort(sel, val)
	}
	return o, nil
}
