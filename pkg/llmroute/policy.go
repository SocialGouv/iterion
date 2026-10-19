// Package llmroute is the adaptive-routing contract (ADR-121): the
// multi-level policy that picks which (harness, credential) pair a run may
// occupy. It is the launch-time counterpart to pkg/retrypolicy (the
// after-the-failure contract) and resolves exactly the same way — field by
// field, the first level that sets a field wins, a provenance map answers
// "which level decided this", and the platform level can only LOWER what
// the levels below may do. Like retrypolicy it is pure: no I/O, no store,
// no clock of its own. Each surface keeps its own persistence (the
// platform settings record, a bot manifest routing: block, a bot binding,
// the launch fields) and projects onto the same Policy.
//
// It exists because a run finds its credential through pieces that do not
// compose: a bot author hand-declares per-node fallbacks: chains, the
// shared tiers fill in a fixed order, and when every tier abstains the run
// parks or dies at its first LLM call — even when the run holds, sealed in
// its own bundle, a credential that could serve it. The policy is the one
// place where the operator's answer lives, at every level, with locks as
// the cost-governance primitive.
//
// The name: the ADR calls the record RoutingPolicy, but store.RoutingPolicy
// (pkg/routing, a run's launch-frozen OUTCOME contract) predates it. This
// package is the LLM (harness, credential) routing record — hence
// llmroute.
package llmroute

import (
	"fmt"
	"path"
	"strings"
)

// RefusedPinnedKey policy values. A shared-tier key refused or capped at
// launch, whose wire family is already served by another credential, would
// be restored pinned-only: the routes naming its provider then park on the
// key's own refusal. "forfait" (the default, ADR-121 § Arbitrated 1)
// leaves that key out instead — the routes naming its provider spend the
// credential that holds the family, which for claw is the forfait billed
// as extra usage. "park" is the pre-policy behavior, kept as a settable
// value.
const (
	RefusedPinnedForfait = "forfait"
	RefusedPinnedPark    = "park"
)

// Trigger is one member of the closed vocabulary of failure classes that
// may fire a policy switch. The names are the fallback machinery's own
// classifications (the route cooldown ledger's categories). budget and
// schema were never categories — they re-fail identically on every route —
// and any / unclassified are excluded by rule (ADR-087: a non-classifiable
// failure advances a hand-declared chain but never fires a policy switch),
// so validation refuses them rather than silently dropping them.
const (
	TriggerUsageWindow       = "usage_window"
	TriggerUnavailable       = "unavailable"
	TriggerTransientExhasted = "transient_exhausted"
	TriggerAuth              = "auth"
)

// Triggers is the closed vocabulary, in the platform default's order.
var Triggers = []string{TriggerUsageWindow, TriggerUnavailable, TriggerTransientExhasted, TriggerAuth}

// Credential slots a pair may name. The *_key slots name a provider's API
// key WITHOUT naming which key — the instance follows the shared tier walk,
// which is what keeps fill/restore/preview parity. claude_forfait and
// chatgpt_forfait are the platform's OAuth forfaits (OAuth kinds
// claude_code and codex). The env a runner holds is NOT a slot: a run that
// acquires no credential at all selects nothing (ADR-121 §1). The pool is
// not here either — its fallback door is trigger-driven, consulted per
// wire family during the walk, and a `pool` pair would name no family (an
// abstract slot, retired by ADR-121's resolved question 1); slice 5
// introduces its own spelling only when the door exists.
const (
	CredClaudeForfait  = "claude_forfait"
	CredChatGPTForfait = "chatgpt_forfait"
)

// Harness names a pair, an override cell's backend or a providers
// allowlist's world may name. Must stay in sync with the backend names the
// DSL accepts (pkg/dsl/ir validate_fallbacks.go) — that file is the
// authority; a backend added there joins here. Exported as a slice so the
// drift tests against the executor side iterate the SAME enumeration
// instead of a copy of it.
var Harnesses = []string{
	HarnessClaudeCode,
	HarnessClaw,
	HarnessCodex,
	HarnessPi,
	HarnessGrok,
	HarnessKimi,
	HarnessOpencode,
}

// The harness name constants, kept as constants for the call sites that
// name one harness directly.
const (
	HarnessClaudeCode = "claude_code"
	HarnessClaw       = "claw"
	HarnessCodex      = "codex"
	HarnessPi         = "pi"
	HarnessGrok       = "grok"
	HarnessKimi       = "kimi"
	HarnessOpencode   = "opencode"
)

// Pair joins a harness and a credential slot with '+' — the wire spelling
// every surface writes and reads ("claude_code+claude_forfait").
func Pair(harness, credential string) string { return harness + "+" + credential }

// ParsePair splits the wire spelling. It is the single reader: validation,
// the dials and the snapshot all go through it, so the spelling cannot
// drift between surfaces.
func ParsePair(p string) (harness, credential string, err error) {
	h, c, ok := strings.Cut(p, "+")
	if !ok || h == "" || c == "" {
		return "", "", fmt.Errorf("llmroute: pair %q is not harness+credential (e.g. %q)", p, Pair(HarnessClaudeCode, CredClaudeForfait))
	}
	return h, c, nil
}

// DefaultPairOrder is the arbitrated default priority (ADR-121 tour 2):
// forfaits first, then keys — a run spends the subscription before the
// per-token bill, and a facade key (z.ai) serves a claude_code session
// before anything more exotic. The ADR's default ends "…then the rest":
// the enumeration below IS that rest, keyed by the compatibility matrix —
// every slot a sealed channel can serve, per the harness that reads it.
// The host-env harnesses (pi, grok, kimi, opencode) appear nowhere: their
// credentials resolve outside every store, so no pair of theirs is ever
// sealable and listing one would be a dial that cannot act.
// Completeness is load-bearing: the whitelist filters slots against THIS
// list, so an unenumerated slot would silently vanish from every launch
// the moment the default answered (the F2 regression the slice-3 review
// caught).
var DefaultPairOrder = []string{
	// The five the ADR names, in arbitrated order.
	Pair(HarnessClaudeCode, CredClaudeForfait),
	Pair(HarnessCodex, CredChatGPTForfait),
	Pair(HarnessClaw, "anthropic_key"),
	Pair(HarnessClaw, "openai_key"),
	Pair(HarnessClaudeCode, "zai_key"),
	// Then the rest: the same slots under the other harness that reads
	// them, then the remaining key providers by wire family.
	Pair(HarnessClaudeCode, "anthropic_key"),
	Pair(HarnessClaudeCode, "moonshot_key"),
	Pair(HarnessCodex, "openai_key"),
	Pair(HarnessClaw, "zai_key"),
	Pair(HarnessClaw, "moonshot_key"),
	Pair(HarnessClaw, "bedrock_key"),
	Pair(HarnessClaw, "vertex_key"),
	Pair(HarnessClaw, "azure_key"),
	Pair(HarnessClaw, "openrouter_key"),
	Pair(HarnessClaw, "xai_key"),
}

// Override is one per-node routing pin: the node selector (an exact node id,
// an id glob, a kind keyword agent|judge|router, or "*") plus whatever it
// pins. Empty fields leave that dimension to the lower layers. Every value
// is a literal — Validate refuses a template, because a pin the launch
// pre-flights cannot read is a pin the tenancy gate cannot certify.
type Override struct {
	Selector string `yaml:"selector" json:"selector" bson:"selector"`
	Backend  string `yaml:"backend,omitempty" json:"backend,omitempty" bson:"backend,omitempty"`
	// Model is a model id, verbatim (ADR-122: a gateway model id travels
	// exactly as written).
	Model string `yaml:"model,omitempty" json:"model,omitempty" bson:"model,omitempty"`
	// Provider is a credential-routing hint chain — a literal, never "auto".
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty" bson:"provider,omitempty"`
	// Effort re-targets reasoning_effort — the one slot no var can reach
	// (reasoning_effort: resolves ${…} only).
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty" bson:"effort,omitempty"`
}

// Policy is the routing contract shared by every level that can own one.
// Zero value is legal (the defaults); Normalize promotes it to explicit
// values. All fields are additive on their host schemas — old records,
// manifests and bindings load unchanged.
type Policy struct {
	// PairOrder is the ordered list of (harness, credential) pairs a run
	// may occupy, in wire spelling. A level REPLACES it — a reordered
	// prefix of a longer list is a semantics trap, so lists never merge.
	// Empty = unset (inherit / default).
	PairOrder []string `yaml:"pair_order,omitempty" json:"pair_order,omitempty" bson:"pair_order,omitempty"`
	// Triggers is the subset of the trigger vocabulary that may fire a
	// switch in-run. The platform level can only PRUNE it (Ceiling), never
	// extend it. Nil = unset (inherit / the default, the whole vocabulary).
	// There is deliberately no "none" value a level can write: a level
	// that must not switch locks the field instead (Validate refuses an
	// explicit empty list); the ONE legitimate empty is the ceiling's —
	// when the platform forbids every trigger the winner named, the honest
	// answer is "never switch", and Clamp produces it as an empty list.
	Triggers []string `yaml:"triggers,omitempty" json:"triggers,omitempty" bson:"triggers,omitempty"`
	// RefusedPinnedKey says what happens to a shared-tier key refused or
	// capped at launch whose family another credential holds: "forfait"
	// (default) leaves it out — routes naming its provider spend the
	// family's holder; "park" restores it pinned-only, and those routes
	// park on the key's own refusal with a durable retry. Empty = unset
	// (inherit / default). Facade keys (z.ai, Moonshot) are out of the
	// knob's scope in both readings: no forfait alternative exists for
	// them, so they always come back.
	RefusedPinnedKey string `yaml:"refused_pinned_key,omitempty" json:"refused_pinned_key,omitempty" bson:"refused_pinned_key,omitempty"`
	// Strict marks every backend pin a REQUIREMENT instead of a
	// preference: a node whose backend cannot be served fails by name
	// rather than being re-targeted. A level may set it where the author
	// did not; NO level unsets it — the fold is monotone (any level's
	// `true` beats every level's `false`), and only the launcher's
	// explicit lock reopens the question (a locked field is decided at or
	// below the lock, so a run-level lock + false wins over a bot's true).
	// Nil = unset (inherit / false).
	Strict *bool `yaml:"strict,omitempty" json:"strict,omitempty" bson:"strict,omitempty"`
	// Locks names fields this level owns: every setter ABOVE it is
	// vetoed, and the field is decided by the highest setter at or below
	// the lock (the author's lock stops the binding; the platform's lock
	// binds every tenant-configurable level). A lock holds even when this
	// level does not SET the field — it then pins the default against the
	// levels above, under a "<level>_lock" provenance. Locks never bind
	// the level that declares them.
	Locks []string `yaml:"locks,omitempty" json:"locks,omitempty" bson:"locks,omitempty"`
	// ModelClasses carries a level's OVERRIDES of the model class table
	// (ADR-121 § Delivery 2): class → family → bare model id. The
	// vocabulary (top/standard/fast), the shipped table and the
	// out-of-routing consumers live with the table itself (classes.go) —
	// the policy carries only what a level may tune. The fold merges
	// ENTRY-WISE per class×family (a map of keyed cells merges like
	// settings, where pair_order replaces); a cell naming an unknown
	// family is accepted (families grow) and one naming an unknown model
	// id too (consumed verbatim; dispatch refuses a bogus one, named).
	// Nil = unset (inherit / the shipped table).
	ModelClasses map[string]map[string]string `yaml:"model_classes,omitempty" json:"model_classes,omitempty" bson:"model_classes,omitempty"`
	// Overrides pins the route of individual NODES — the typed, per-level
	// twin of the launch's model_overrides. The fold is ENTRY-WISE per
	// (selector, field) cell (a cell is independent: ForNode resolves per
	// field), the first setter within the range the locks allow winning
	// each cell; values are LITERALS (Validate refuses a template), which
	// is what makes "the launch pre-flights can read this pin" a property
	// of the type. Nil = unset (inherit / no cells).
	Overrides []Override `yaml:"overrides,omitempty" json:"overrides,omitempty" bson:"overrides,omitempty"`
	// Providers is the tenant's provider ALLOWLIST: the credential-routing
	// providers a run of this level may spend. A route resolving outside it
	// is not this tenant's spend. Nil = unset (inherit / no allowlist).
	// Lockable; first-setter-wins like pair_order — a list never merges.
	Providers []string `yaml:"providers,omitempty" json:"providers,omitempty" bson:"providers,omitempty"`
	// CrossHarness says whether a computed route may move a node onto a
	// DIFFERENT HARNESS than it started on (ADR-121 § Delivery 2, the
	// session contract's third state): "off" (the default) keeps the
	// delivery-1 refusal — a cross-backend stage on a session-bearing
	// node never lands; "restart" lets it land and the node relaunches
	// from its original prompt, the failed attempt's transcript riding
	// as reference; "reuse" lets it land and the fresh harness CONTINUES
	// the node mid-work from the sealed transcript. A cross-harness
	// switch REPLACES the session — said on the model_fallback line with
	// the handoff state, neither kept (one provider's turns are never
	// replayed into a harness that never issued them) nor silently
	// evicted. First-setter-wins like refused_pinned_key; lockable; NOT
	// ceilinged — the operator's veto here is a lock, not a prune.
	// Empty = unset (Normalize answers "off").
	CrossHarness string `yaml:"cross_harness,omitempty" json:"cross_harness,omitempty" bson:"cross_harness,omitempty"`
}

// CrossHarnessActive is the ONE gate for "may this stage cross harnesses
// on a session-bearing node": the ACTIVE modes only. NEVER test
// emptiness — Normalize promotes "" to "off", so every resolved policy
// and every stamped wire entry carries a non-empty "off", and an
// emptiness gate would lift the session refusal for every cloud run
// under the default (the plan-review critique, probe-proven).
func CrossHarnessActive(mode string) bool {
	return mode == CrossHarnessReuse || mode == CrossHarnessRestart
}

// The fold's field names, as the provenance map keys them (the wire field
// names, same convention as retrypolicy).
const (
	FieldPairOrder        = "pair_order"
	FieldTriggers         = "triggers"
	FieldRefusedPinnedKey = "refused_pinned_key"
	FieldStrict           = "strict"
	FieldModelClasses     = "model_classes"
	FieldCrossHarness     = "cross_harness"
	FieldOverrides        = "overrides"
	FieldProviders        = "providers"
)

// The cross_harness vocabulary (ADR-121 § Delivery 2).
const (
	CrossHarnessOff     = "off"
	CrossHarnessReuse   = "reuse"
	CrossHarnessRestart = "restart"
)

// Fields lists the fold's fields — the vocabulary Locks validates against
// and the provenance map completes.
var Fields = []string{FieldPairOrder, FieldTriggers, FieldRefusedPinnedKey, FieldStrict, FieldModelClasses, FieldCrossHarness, FieldOverrides, FieldProviders}

// Normalize returns p with defaults applied. Idempotent; never returns a
// Policy with a nil PairOrder, Triggers or empty RefusedPinnedKey. Only NIL
// lists inherit: an explicitly EMPTY trigger list is an answer (the ceiling
// pruned everything the winner named — Clamp produces it), never an
// absence, and must survive a re-Normalize unchanged. Write surfaces never
// see one — Validate refuses it — so only the fold's post-Clamp policies
// can carry it, and consumers of a RESOLVED policy must not Normalize it.
func Normalize(p Policy) Policy {
	if p.PairOrder == nil {
		p.PairOrder = append([]string(nil), DefaultPairOrder...)
	}
	if p.Triggers == nil {
		p.Triggers = append([]string(nil), Triggers...)
	}
	if p.RefusedPinnedKey == "" {
		p.RefusedPinnedKey = RefusedPinnedForfait
	}
	if p.CrossHarness == "" {
		p.CrossHarness = CrossHarnessOff
	}
	return p
}

// ValidTrigger reports whether t belongs to the closed vocabulary.
func ValidTrigger(t string) bool {
	for _, v := range Triggers {
		if v == t {
			return true
		}
	}
	return false
}

func validHarness(h string) bool {
	for _, v := range Harnesses {
		if v == h {
			return true
		}
	}
	return false
}

// ReasoningEfforts is the closed effort vocabulary an override cell may pin.
// Kept local because pkg/llmroute imports nothing of iterion by design; the
// drift test against ir.ValidReasoningEfforts lives in projections_test.go
// (package llmroute_test — the in-package test cannot import ir, which
// imports llmroute).
var ReasoningEfforts = []string{"none", "low", "medium", "high", "xhigh", "max", "ultracode"}

// ValidEffort reports whether e belongs to the closed effort vocabulary.
func ValidEffort(e string) bool {
	for _, v := range ReasoningEfforts {
		if v == e {
			return true
		}
	}
	return false
}

// The provider names an override hint or a providers allowlist may speak.
// The <provider>_key slots' wire identifiers (validKeyProvider) plus the
// gateway, whose credential the runner's environment funds.
const ProviderOpenAICompatible = "openai_compatible"

// ProviderHints is the closed provider vocabulary of an override's provider
// chain and of the providers allowlist.
var ProviderHints = append(append([]string(nil), "anthropic", "openai", "bedrock", "vertex", "azure", "openrouter", "xai", "zai", "moonshot"), ProviderOpenAICompatible)

// ValidProviderHint reports whether p belongs to the provider vocabulary.
func ValidProviderHint(p string) bool {
	for _, v := range ProviderHints {
		if v == p {
			return true
		}
	}
	return false
}

// ConsumesProviderHint is the local twin of
// model.ConsumesProviderHint: how a backend consumes the per-node provider
// hint. chains=true walks a multi-step chain (claude_code); single=true
// honours one step and derives the rest from the model prefix (claw); both
// false = the backend ignores the hint. Kept local for the same import
// reason as the effort list; the drift test lives in projections_test.go.
func ConsumesProviderHint(backend string) (chains, single bool) {
	switch backend {
	case HarnessClaudeCode:
		return true, false
	case HarnessClaw:
		return false, true
	default:
		return false, false
	}
}

// HonoursProviderHintValue is the local twin of
// model.HonoursProviderHintValue: whether the backend honours THIS
// provider value as a pin at its dispatch chokepoint — the value-aware
// half of ConsumesProviderHint. claude_code walks every step and refuses
// an unfunded hint loudly at dispatch, so any validated value is
// honoured; claw acts on the hint at exactly one switch (its registry's
// force-Anthropic-direct branch, wired to the model twin) and ignores
// every other value there — the spend then follows the default
// precedence, the silent misroute a policy pin must not certify. Kept
// local for the same import reason as the classes; the drift test lives
// in projections_test.go.
func HonoursProviderHintValue(backend, provider string) bool {
	switch backend {
	case HarnessClaudeCode:
		return true
	case HarnessClaw:
		return strings.ToLower(strings.TrimSpace(provider)) == "anthropic"
	default:
		return false
	}
}

// validCredential checks the slot against the closed vocabulary. The
// <provider>_key slots follow pkg/secrets.Provider's stable wire
// identifiers (the provider list is guarded against drift by a test).
func validCredential(c string) bool {
	switch c {
	case CredClaudeForfait, CredChatGPTForfait:
		return true
	}
	if strings.HasSuffix(c, "_key") && validKeyProvider(strings.TrimSuffix(c, "_key")) {
		return true
	}
	return false
}

// validKeyProvider mirrors secrets.Provider's values. Kept as its own list
// because this package must stay importable from everywhere the policy
// travels (platformcfg, store-adjacent surfaces) without dragging a store
// dependency; the llmroute test imports secrets and asserts the two lists
// agree.
func validKeyProvider(p string) bool {
	switch p {
	case "anthropic", "openai", "bedrock", "vertex", "azure", "openrouter", "xai", "zai", "moonshot":
		return true
	}
	return false
}

// validateOverrideCells lists the four routing fields an Override may pin,
// in the order the resolved entries carry them.
type overrideCell struct {
	name string
	get  func(Override) string
	set  func(*Override, string)
}

var overrideCells = []overrideCell{
	{"backend", func(o Override) string { return o.Backend }, func(o *Override, v string) { o.Backend = v }},
	{"model", func(o Override) string { return o.Model }, func(o *Override, v string) { o.Model = v }},
	{"provider", func(o Override) string { return o.Provider }, func(o *Override, v string) { o.Provider = v }},
	{"effort", func(o Override) string { return o.Effort }, func(o *Override, v string) { o.Effort = v }},
}

// providerHintTokens splits a provider cell into its tokens, keeping the
// raw hint beside the folded step: the ONE split of the cell, so the
// shape, case and vocabulary checks read the same parse the executor's
// dispatch and the credential preview read (split on ",", then the
// `provider:model` step form, hints folded to lower-case). "auto" and an
// empty step come back named — they are refused, not skipped: an override
// exists to PIN.
type providerHintToken struct {
	raw      string // the hint as the cell spells it, pre-case-fold
	step     string // the folded form the vocabulary checks
	stepRaw  string // the whole step as the cell spells it, `provider:model` form included
	modelRaw string // the model half, trimmed; empty when absent or blank
	hasModel bool   // a colon is present: the step carries the provider:model form
}

func providerHintTokens(raw string) []providerHintToken {
	var tokens []providerHintToken
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue // stray, leading or trailing comma
		}
		hint, model, hasModel := strings.Cut(token, ":")
		hint = strings.TrimSpace(hint)
		tokens = append(tokens, providerHintToken{
			raw:      hint,
			step:     strings.ToLower(hint),
			stepRaw:  token,
			modelRaw: strings.TrimSpace(model),
			hasModel: hasModel,
		})
	}
	return tokens
}

// validateOverride checks one entry: the selector's syntax, each pinned
// value's vocabulary, and that the entry pins SOMETHING — ModelOverrides.add
// drops an empty entry silently, which at a write surface is a typo that
// looks like it landed.
func validateOverride(o Override) error {
	sel := strings.TrimSpace(o.Selector)
	if sel == "" {
		return fmt.Errorf("selector is empty")
	}
	if sel != o.Selector {
		return fmt.Errorf("selector %q carries surrounding whitespace — it would never match a node id", o.Selector)
	}
	if strings.ContainsAny(sel, ".$") {
		return fmt.Errorf("selector %q — a '.' or '$' would poison the provenance key overrides.<selector>.<field>, persisted as a BSON document", sel)
	}
	if strings.ContainsAny(sel, "*?[") {
		if _, err := path.Match(sel, ""); err != nil {
			return fmt.Errorf("selector %q is not a valid glob: %v", sel, err)
		}
	}
	pinned := 0
	for _, c := range overrideCells {
		v := c.get(o)
		if v == "" {
			continue
		}
		pinned++
		if strings.Contains(v, "{{") || strings.Contains(v, "${") {
			return fmt.Errorf("%s %q — an override value is a LITERAL (a template would be unreadable at the launch pre-flights, so the gate could certify nothing); omit the cell to let the lower layers decide", c.name, v)
		}
	}
	if o.Backend != "" && !validHarness(o.Backend) {
		return fmt.Errorf("backend %q is not a harness (want one of claude_code, claw, codex, pi, grok, kimi, opencode)", o.Backend)
	}
	if o.Effort != "" && !ValidEffort(o.Effort) {
		return fmt.Errorf("effort %q is not in the vocabulary (want one of %s)", o.Effort, strings.Join(ReasoningEfforts, ", "))
	}
	if o.Provider != "" {
		tokens := providerHintTokens(o.Provider)
		if len(tokens) == 0 {
			return fmt.Errorf("provider %q names no step", o.Provider)
		}
		for _, t := range tokens {
			if strings.ToLower(t.raw) != t.raw {
				return fmt.Errorf("provider %q names %q — the provider vocabulary is lower-case; write %q", o.Provider, t.raw, t.step)
			}
		}
		for _, t := range tokens {
			s := t.step
			if s == "auto" {
				return fmt.Errorf("provider %q names \"auto\" — an override pins, it never defers; omit the cell to let the process decide", o.Provider)
			}
			if s == "" {
				return fmt.Errorf("provider %q names an empty step", o.Provider)
			}
			if t.hasModel && t.modelRaw == "" {
				return fmt.Errorf("provider %q names %q — a step with an empty model part is a typo the runtime would trim to the bare hint; write %q", o.Provider, t.stepRaw, t.raw)
			}
			if !ValidProviderHint(s) {
				return fmt.Errorf("provider %q names %q, which is not a known provider (want one of %s)", o.Provider, s, strings.Join(ProviderHints, ", "))
			}
			if o.Backend != "" {
				chains, single := ConsumesProviderHint(o.Backend)
				if !chains && !single {
					return fmt.Errorf("provider %q names %q on backend %q — this harness does not consume the provider hint; the gate would certify a pin the harness ignores", o.Provider, s, o.Backend)
				}
				if single && !chains && len(tokens) > 1 {
					return fmt.Errorf("provider %q names %q on backend %q — this harness honours a single-value hint and derives the rest from the model prefix; it does not walk chains", o.Provider, s, o.Backend)
				}
				if !HonoursProviderHintValue(o.Backend, s) {
					return fmt.Errorf("provider %q names %q on backend %q — this harness acts on none of this value at its routing chokepoint, so the gate would certify a pin the harness silently ignores; the spend would follow the default precedence", o.Provider, s, o.Backend)
				}
			}
		}
	}
	if pinned == 0 {
		return fmt.Errorf("selector %q pins nothing — an entry that pins no field would be dropped silently at the fold, so the typo would look like it landed", sel)
	}
	return nil
}

// Validate reports whether p is coherent. Errors are user-facing and name
// the offending field. An UNSET field (empty) is legal at any single level
// — inherit is the norm; only values that are set must be well-formed.
func Validate(p Policy) error {
	// An explicit empty list is refused, not honored: it would read as
	// "the run may occupy nothing" / "no trigger may fire" on one read and
	// as "inherit" after a record round-trip (omitempty erases it) — two
	// opposite answers from one write. A level that must not switch locks
	// the field instead; a level that must not route omits the field.
	if p.PairOrder != nil && len(p.PairOrder) == 0 {
		return fmt.Errorf("llmroute: pair_order is empty — omit it to inherit the default (an empty list would disable routing outright)")
	}
	if p.Triggers != nil && len(p.Triggers) == 0 {
		return fmt.Errorf("llmroute: triggers is empty — omit it to inherit the vocabulary; a level that must not switch locks the field instead")
	}
	seenPair := map[string]bool{}
	for _, pair := range p.PairOrder {
		h, c, err := ParsePair(pair)
		if err != nil {
			return err
		}
		if !validHarness(h) {
			return fmt.Errorf("llmroute: pair_order %q — unknown harness %q (want one of claude_code, claw, codex, pi, grok, kimi, opencode)", pair, h)
		}
		if !validCredential(c) {
			return fmt.Errorf("llmroute: pair_order %q — unknown credential %q (want claude_forfait, chatgpt_forfait, pool, or <provider>_key like anthropic_key)", pair, c)
		}
		if seenPair[pair] {
			return fmt.Errorf("llmroute: pair_order names %q twice — a repeated pair is an ordering mistake", pair)
		}
		seenPair[pair] = true
	}
	seenTrigger := map[string]bool{}
	for _, t := range p.Triggers {
		if !ValidTrigger(t) {
			return fmt.Errorf("llmroute: triggers %q is not in the vocabulary (want a subset of %s) — budget and schema never were categories, and any/unclassified are excluded by rule: a non-classifiable failure never fires a policy switch", t, strings.Join(Triggers, ", "))
		}
		if seenTrigger[t] {
			return fmt.Errorf("llmroute: triggers names %q twice", t)
		}
		seenTrigger[t] = true
	}
	switch p.RefusedPinnedKey {
	case "", RefusedPinnedForfait, RefusedPinnedPark:
	default:
		return fmt.Errorf("llmroute: refused_pinned_key %q — want %q or %q", p.RefusedPinnedKey, RefusedPinnedForfait, RefusedPinnedPark)
	}
	for class, fams := range p.ModelClasses {
		if !validClass(class) {
			return fmt.Errorf("llmroute: model_classes %q is not a class (want one of %s)", class, strings.Join(classVocabulary, ", "))
		}
		if len(fams) == 0 {
			return fmt.Errorf("llmroute: model_classes %q is empty — omit it to inherit the class", class)
		}
		for fam, spec := range fams {
			if fam == "" {
				return fmt.Errorf("llmroute: model_classes %s names an empty family", class)
			}
			if spec == "" {
				return fmt.Errorf("llmroute: model_classes %s.%s is empty — omit the cell to inherit the shipped rendering", class, fam)
			}
			if strings.Contains(spec, "/") {
				return fmt.Errorf("llmroute: model_classes %s.%s = %q — cells are BARE ids (the per-credential spellings re-prefix; a %s/ prefix here would double it)", class, fam, spec, fam)
			}
		}
	}
	switch p.CrossHarness {
	case "", CrossHarnessOff, CrossHarnessReuse, CrossHarnessRestart:
	default:
		return fmt.Errorf("llmroute: cross_harness %q — want %q, %q or %q", p.CrossHarness, CrossHarnessOff, CrossHarnessReuse, CrossHarnessRestart)
	}
	for _, o := range p.Overrides {
		if err := validateOverride(o); err != nil {
			return fmt.Errorf("llmroute: overrides: %w", err)
		}
	}
	seenCell := map[string]bool{}
	for _, o := range p.Overrides {
		for _, cell := range overrideCells {
			if cell.get(o) == "" {
				continue
			}
			key := o.Selector + "\x00" + cell.name
			if seenCell[key] {
				return fmt.Errorf("llmroute: overrides pins %s.%s twice — the later would silently win", o.Selector, cell.name)
			}
			seenCell[key] = true
		}
	}
	if p.Providers != nil && len(p.Providers) == 0 {
		return fmt.Errorf("llmroute: providers is empty — omit it to inherit (a level that must forbid everything has no run to make; the honest spelling refuses the bot)")
	}
	seenProv := map[string]bool{}
	for _, pr := range p.Providers {
		if !ValidProviderHint(pr) {
			return fmt.Errorf("llmroute: providers %q is not a known provider (want one of %s)", pr, strings.Join(ProviderHints, ", "))
		}
		if seenProv[pr] {
			return fmt.Errorf("llmroute: providers names %q twice", pr)
		}
		seenProv[pr] = true
	}
	for _, f := range p.Locks {
		if !validField(f) {
			return fmt.Errorf("llmroute: locks %q is not a policy field (want a subset of %s)", f, strings.Join(Fields, ", "))
		}
	}
	return nil
}

func validClass(c string) bool {
	for _, v := range classVocabulary {
		if v == c {
			return true
		}
	}
	return false
}

func validField(f string) bool {
	for _, v := range Fields {
		if v == f {
			return true
		}
	}
	return false
}

// Layer is one contributor to a resolved Policy, tagged with a label used
// for provenance reporting.
type Layer struct {
	// Source names the layer for provenance ("bot", "schedule",
	// "trigger", "webhook", "platform", "env", "default").
	Source string
	Policy Policy
	// Launcher marks THE launcher layer (the run level, delivery-1
	// slice 3): the one seat whose explicit lock may reopen strict
	// (ADR-121: "no level unsets it — only the launcher, for their own
	// run, through an explicit lock"). No layer marks it before the run
	// level exists, so strict is monotone across every layer slices 1–2
	// ship — a binding lock cannot launder an unset past the author.
	Launcher bool
}

// Provenance labels for the layers Resolve is normally called with —
// per-surface for the levels above the platform, matching the retrypolicy
// chain's convention (a snapshot that says only "binding" stops where the
// webhook case needs it most: one ingress serves many bots).
const (
	// SourceRun names the launcher's own layer — the HEAD of the chain,
	// the seat whose explicit lock may reopen strict.
	SourceRun      = "run"
	SourceBot      = "bot"
	SourceSchedule = "schedule"
	SourceTrigger  = "trigger"
	SourceWebhook  = "webhook"
	SourceTeam     = "team"
	SourceOrg      = "org"
	SourcePlatform = "platform"
	SourceEnv      = "env"
	SourceDefault  = "default"
	SourceCeiling  = "platform_ceiling"
)

// Resolve overlays layers FIELD BY FIELD, highest priority first, and
// reports which layer won each field.
//
// Per-field (rather than whole-struct) overlay is what makes the chain
// usable: a bot that only pins pair_order must not silently discard the
// platform's refused_pinned_key choice. It mirrors retrypolicy.Resolve
// exactly (ADR-121 §0: the fold is "on the retrypolicy pattern") — except
// for the lock, which is the one primitive cost governance has and reads
// the other way round:
//
// A lock on a field at level L vetoes every setter ABOVE L. The field is
// then decided by the highest-priority setter AT OR BELOW L — or the
// default when none of them sets it, with the provenance naming the lock
// ("<level>_lock") so an operator can tell "nobody spoke" from "the author
// locked this". That is the ADR's contract read literally: the author's
// lock is what stops the binding (a level ABOVE the author), and the
// platform's lock binds every level the tenant could configure. Several
// locks on one field: the lowest-priority one governs (the most
// restrictive range). A level's own sets are never blocked by its own
// locks, and a level above is never blocked by declaring a lock — it was
// already answered before the lock's range began.
//
// The returned map is keyed by the wire field name so an API can show an
// operator why an effective value is what it is — an undebuggable policy
// is an unauditable one.
func Resolve(layers ...Layer) (Policy, map[string]string) {
	var out Policy
	src := make(map[string]string, len(Fields))
	// Pre-pass: for each field, the lowest-priority lock (the LAST one in
	// the slice) decides where the field's answer may come from.
	lockedFrom := map[string]int{}
	lockSource := map[string]string{}
	for i, l := range layers {
		for _, f := range l.Policy.Locks {
			lockedFrom[f] = i
			lockSource[f] = l.Source
		}
	}
	mayAnswer := func(i int, field string) bool {
		from, locked := lockedFrom[field]
		return !locked || i >= from
	}
	for i, l := range layers {
		// FIRST setter wins, within the range the locks leave this layer.
		if mayAnswer(i, FieldPairOrder) && out.PairOrder == nil && len(l.Policy.PairOrder) > 0 {
			out.PairOrder = l.Policy.PairOrder
			src[FieldPairOrder] = l.Source
		}
		if mayAnswer(i, FieldTriggers) && out.Triggers == nil && len(l.Policy.Triggers) > 0 {
			out.Triggers = l.Policy.Triggers
			src[FieldTriggers] = l.Source
		}
		if mayAnswer(i, FieldRefusedPinnedKey) && out.RefusedPinnedKey == "" && l.Policy.RefusedPinnedKey != "" {
			out.RefusedPinnedKey = l.Policy.RefusedPinnedKey
			src[FieldRefusedPinnedKey] = l.Source
		}
		if mayAnswer(i, FieldStrict) && out.Strict == nil && l.Policy.Strict != nil {
			out.Strict = l.Policy.Strict
			src[FieldStrict] = l.Source
		}
		if mayAnswer(i, FieldCrossHarness) && out.CrossHarness == "" && l.Policy.CrossHarness != "" {
			out.CrossHarness = l.Policy.CrossHarness
			src[FieldCrossHarness] = l.Source
		}
		if mayAnswer(i, FieldProviders) && out.Providers == nil && len(l.Policy.Providers) > 0 {
			out.Providers = l.Policy.Providers
			src[FieldProviders] = l.Source
		}
	}
	// Strict is monotone unless the LAUNCHER layer locked it (ADR-121:
	// "no level unsets it — only the launcher, for their own run, through
	// an explicit lock"): a true anywhere wins over a false anywhere — a
	// bot's `strict: true` survives a binding's plain `false` AND a
	// binding's lock. The seat is marked, not positional: before the run
	// level exists no layer marks it, so the monotone lift runs
	// everywhere. When the launcher's lock reopens the question, the
	// locked range decides (first setter at or below the lock).
	// Provenance names the highest setter of the value that won.
	headLocksStrict := false
	for _, l := range layers {
		if !l.Launcher {
			continue
		}
		for _, f := range l.Policy.Locks {
			if f == FieldStrict {
				headLocksStrict = true
			}
		}
	}
	if !headLocksStrict {
		if out.Strict == nil || !*out.Strict {
			// The lift scans only the range the locks allow — a true
			// ABOVE a lock was vetoed and must not come back through the
			// monotone pass.
			start := 0
			if from, locked := lockedFrom[FieldStrict]; locked {
				start = from
			}
			for _, l := range layers[start:] {
				if l.Policy.Strict != nil && *l.Policy.Strict {
					t := true
					out.Strict = &t
					src[FieldStrict] = l.Source
					break
				}
			}
		}
	}
	// A lock that pinned a field to the default says so: "default" alone
	// would hide the veto.
	for f := range lockedFrom {
		if _, ok := src[f]; !ok {
			src[f] = lockSource[f] + "_lock"
		}
	}
	// model_classes folds ENTRY-WISE (the merge rule is the field's own,
	// stated in the ADR: a map of keyed cells merges like settings, where
	// pair_order replaces). First setter wins PER CELL within the range
	// the locks leave; each cell's provenance names its level as
	// model_classes.<class>.<family>. The BLOCK key answers only when no
	// cell spoke — "default", or "<level>_lock" for a lock that pinned
	// the block without setting cells; when cells spoke the block key is
	// OMITTED, because a "default" beside named cells would lie about who
	// decided.
	var mcCells int
	for i, l := range layers {
		if !mayAnswer(i, FieldModelClasses) || len(l.Policy.ModelClasses) == 0 {
			continue
		}
		for class, fams := range l.Policy.ModelClasses {
			if len(fams) == 0 {
				continue
			}
			if out.ModelClasses == nil {
				out.ModelClasses = map[string]map[string]string{}
			}
			if out.ModelClasses[class] == nil {
				out.ModelClasses[class] = map[string]string{}
			}
			for fam, spec := range fams {
				if _, spoken := out.ModelClasses[class][fam]; spoken {
					continue
				}
				out.ModelClasses[class][fam] = spec
				src[FieldModelClasses+"."+class+"."+fam] = l.Source
				mcCells++
			}
		}
	}
	if _, locked := lockedFrom[FieldModelClasses]; locked && mcCells == 0 {
		src[FieldModelClasses] = lockSource[FieldModelClasses] + "_lock"
	} else if mcCells == 0 {
		src[FieldModelClasses] = SourceDefault
	}
	// overrides folds ENTRY-WISE per (selector, field) cell (the merge rule
	// is the field's own, stated in the ADR: a cell is independent by
	// construction — ForNode resolves per field — so merging cells cannot
	// produce the "reordered prefix" trap that made pair_order replace).
	// First setter wins PER CELL within the range the locks leave; each
	// cell's provenance names its level as overrides.<selector>.<field>.
	// The BLOCK key answers only when no cell spoke — "default", or
	// "<level>_lock" for a lock that pinned the block without setting
	// cells; when cells spoke the block key keeps the lock's stamp, the
	// same convention model_classes follows.
	type ovCell struct{ selector, field string }
	cells := map[ovCell]string{}
	cellSrc := map[ovCell]string{}
	var selOrder []string
	selSeen := map[string]bool{}
	ovCells := 0
	for i, l := range layers {
		if !mayAnswer(i, FieldOverrides) {
			continue
		}
		for _, o := range l.Policy.Overrides {
			for _, cell := range overrideCells {
				v := cell.get(o)
				if v == "" {
					continue
				}
				key := ovCell{o.Selector, cell.name}
				if _, spoken := cells[key]; spoken {
					continue
				}
				cells[key] = v
				cellSrc[key] = l.Source
				if !selSeen[o.Selector] {
					selSeen[o.Selector] = true
					selOrder = append(selOrder, o.Selector)
				}
				ovCells++
			}
		}
	}
	if len(cells) > 0 {
		out.Overrides = make([]Override, 0, len(selOrder))
		for _, sel := range selOrder {
			var ov Override
			ov.Selector = sel
			for _, cell := range overrideCells {
				if v, ok := cells[ovCell{sel, cell.name}]; ok {
					cell.set(&ov, v)
				}
			}
			out.Overrides = append(out.Overrides, ov)
		}
		for k, s := range cellSrc {
			src[FieldOverrides+"."+k.selector+"."+k.field] = s
		}
	}
	if _, locked := lockedFrom[FieldOverrides]; locked && ovCells == 0 {
		src[FieldOverrides] = lockSource[FieldOverrides] + "_lock"
	} else if ovCells == 0 {
		src[FieldOverrides] = SourceDefault
	}
	out = Normalize(out)
	for _, f := range Fields {
		if f == FieldModelClasses || f == FieldOverrides {
			continue // answered above: per-cell keys, or the block key
		}
		if _, ok := src[f]; !ok {
			src[f] = SourceDefault
		}
	}
	return out, src
}

// Ceiling is the platform-imposed bound on the trigger vocabulary: it can
// only PRUNE what the levels below resolved, never extend it. A nil/empty
// ceiling constrains nothing (the platform did not speak on triggers).
// Other fields have no ceiling: pair_order is replaced, not narrowed;
// refused_pinned_key and strict are single values.
type Ceiling struct {
	// Triggers prunes the resolved triggers to this subset. nil/empty =
	// no ceiling on triggers.
	Triggers []string
}

// IsZero reports whether the ceiling constrains nothing.
func (c Ceiling) IsZero() bool { return len(c.Triggers) == 0 }

// Clamp lowers p to fit c, recording the prune in src (which may be nil)
// under SourceCeiling. It never fires a trigger the ceiling refused, and
// never restores one the policy already dropped. Pruning to an EMPTY list
// is a legitimate answer ("never switch"), not an absence — the caller
// stores it as-is (the snapshot's triggers field is not omitempty) and
// must not re-Normalize the resolved policy. The platform level sits
// BELOW org and team (ADR-121's chain runs platform < org < team < bot <
// binding < run), so its triggers are the ceiling those levels may only
// narrow — a live semantic since the tenant levels landed, wired from
// delivery 1's slice 1 so no level retrofitted the ordering.
func Clamp(p Policy, c Ceiling, src map[string]string) Policy {
	if c.IsZero() || len(p.Triggers) == 0 {
		return p
	}
	keep := make([]string, 0, len(p.Triggers))
	pruned := false
	for _, t := range p.Triggers {
		if ceilingAllows(c.Triggers, t) {
			keep = append(keep, t)
		} else {
			pruned = true
		}
	}
	if !pruned {
		return p
	}
	p.Triggers = keep
	if src != nil {
		src[FieldTriggers] = SourceCeiling
	}
	return p
}

func ceilingAllows(allowed []string, t string) bool {
	for _, a := range allowed {
		if a == t {
			return true
		}
	}
	return false
}
