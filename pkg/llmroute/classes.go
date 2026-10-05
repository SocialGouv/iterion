package llmroute

// The model class table (ADR-121 § Delivery 2): classes → families →
// bare model ids, with both directions built once — the forward direction
// answers "what does class C render as on family F" (the wholesale
// crossing's target), the reverse "which class names this model" (the
// node's own classification). The table lives in llmroute, beside its
// only consumer (shippedOpenAIDefault's precedent): llmroute is the
// zero-dependency leaf, and modelcatalog imports the backend stack.

// The class vocabulary is CLOSED (like the trigger vocabulary): a policy
// naming another class refuses at validation.
const (
	ClassTop      = "top"
	ClassStandard = "standard"
	ClassFast     = "fast"
)

// classVocabulary order is load-bearing for the reverse index: the FIRST
// class naming a bare id wins, so gpt-6-sol (named by top AND standard)
// classifies top.
var classVocabulary = []string{ClassTop, ClassStandard, ClassFast}

// ClassTable is the effective class table after folding a policy's
// model_classes overrides over the shipped cells. The zero value is the
// EMPTY table — StageModel substitutes the shipped one, so call sites
// never construct one by hand.
type ClassTable struct {
	forward map[string]map[string]string // class → family → bare id
	reverse map[string]map[string]string // family → bare id → class
}

// shippedClasses is the registry's default table (ADR-121; the ticket §4
// proposal, with the operator-flagged astra-as-top cell LEFT at sol — one
// model_classes.top.openai override moves it, no behavior taken on the
// operator's behalf). Cells are BARE ids: the per-credential spellings
// re-prefix (anthropic/ + bare, openai/ + bare, bare for
// claude_code/codex). The ticket's third (glm/z.ai) column does not
// exist here: glm-* ids ARE anthropic-wire family under modelFamily.
//
// standard/openai is load-bearing for byte-identity: delivery 1 crosses
// EVERY foreign model to shippedOpenAIDefault, and an unknown model
// classifies standard — the cell must equal it (pinned by test).
func shippedClasses() map[string]map[string]string {
	return map[string]map[string]string{
		ClassTop: {
			familyAnthropicWire: "claude-opus-5-5",
			familyOpenAI:        shippedOpenAIDefault,
		},
		ClassStandard: {
			familyAnthropicWire: "claude-sonnet-5",
			familyOpenAI:        shippedOpenAIDefault,
		},
		ClassFast: {
			familyAnthropicWire: "claude-haiku-4-5",
			familyOpenAI:        "gpt-6-luna",
		},
	}
}

// ResolveClasses folds the overrides ENTRY-WISE per class×family over the
// shipped cells (the model_classes merge rule — a map of keyed cells
// merges like settings, where pair_order replaces) and builds the reverse
// index: first class in vocabulary order naming a bare id wins.
func ResolveClasses(overrides map[string]map[string]string) ClassTable {
	fwd := map[string]map[string]string{}
	for class, fams := range shippedClasses() {
		fwd[class] = map[string]string{}
		for fam, spec := range fams {
			fwd[class][fam] = spec
		}
	}
	for class, fams := range overrides {
		if fwd[class] == nil {
			fwd[class] = map[string]string{}
		}
		for fam, spec := range fams {
			fwd[class][fam] = spec
		}
	}
	rev := map[string]map[string]string{}
	for _, class := range classVocabulary {
		for fam, spec := range fwd[class] {
			if rev[fam] == nil {
				rev[fam] = map[string]string{}
			}
			if _, taken := rev[fam][spec]; !taken {
				rev[fam][spec] = class
			}
		}
	}
	return ClassTable{forward: fwd, reverse: rev}
}

// ClassOf classifies a model by the PAIR (family, bare) — never bare
// alone: the same bare id classifies differently under a different
// family prefix (gpt-6-luna is the fast cell on the openai family, but
// anthropic/gpt-6-luna is ANTHROPIC-WIRE family by modelFamily's own
// rule, where the fast cell names claude-haiku-4-5 — so it is unnamed
// there and classifies standard). A model the table does not name — and
// the empty family — classifies standard, the rule that keeps delivery
// 1's crossing answer for everything the table is silent on.
func (t ClassTable) ClassOf(family, bare string) string {
	if c, ok := t.reverse[family][bare]; ok {
		return c
	}
	return ClassStandard
}

// Target answers the class's rendering on the family. ok=false for a
// cell that resolves nowhere — the stage drops for that node, named
// ("class <c> resolves nowhere on family <f>"), the honest form of the
// ADR's "a class that resolves nowhere leaves the route as written and
// warns". The shipped table is full; only a custom table (tests) or a
// future vocabulary extension reaches it.
func (t ClassTable) Target(class, family string) (string, bool) {
	spec, ok := t.forward[class][family]
	return spec, ok && spec != ""
}
