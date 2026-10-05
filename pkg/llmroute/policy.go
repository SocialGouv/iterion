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

// Harnesses a pair may name. Must stay in sync with the backend names the
// DSL accepts (pkg/dsl/ir validate_fallbacks.go) — that file is the
// authority; a backend added there joins here.
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
}

// The fold's field names, as the provenance map keys them (the wire field
// names, same convention as retrypolicy).
const (
	FieldPairOrder        = "pair_order"
	FieldTriggers         = "triggers"
	FieldRefusedPinnedKey = "refused_pinned_key"
	FieldStrict           = "strict"
	FieldModelClasses     = "model_classes"
)

// Fields lists the fold's fields — the vocabulary Locks validates against
// and the provenance map completes.
var Fields = []string{FieldPairOrder, FieldTriggers, FieldRefusedPinnedKey, FieldStrict, FieldModelClasses}

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
	switch h {
	case HarnessClaudeCode, HarnessClaw, HarnessCodex, HarnessPi, HarnessGrok, HarnessKimi, HarnessOpencode:
		return true
	}
	return false
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
	out = Normalize(out)
	for _, f := range Fields {
		if f == FieldModelClasses {
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
