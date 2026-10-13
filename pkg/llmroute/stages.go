package llmroute

import (
	"strings"
)

// The ladder-stage mapping (ADR-121 §1): for a held (harness, credential)
// pair, the model a computed fallback stage carries. The stage's model is
// the SOLE credential-routing signal at dispatch — model specs are not
// portable across backends (the C176 portability rule) — so the mapping is
// keyed per PAIR, not per family: the same node model spells differently
// for claw+anthropic_key (`anthropic/<model>`, a TRUE provider pin — the
// key IS anthropic) than for claw+zai_key (BARE `glm-*`: an
// `anthropic/` prefix here would pin "anthropic" in every derivation while
// dispatch spends ZAI, mis-metering the fingerprint and refusing the
// launch under requireLLMCredential).
//
// A pair with no mapping has no delivery-1 spelling — its stage is DROPPED
// at construction (never emitted modelless: ApplyRunFallback would refuse
// it), the honest form of the ADR's "a class that resolves nowhere leaves
// the route as written and warns".

// StageModel maps one held pair to the model spelling its fallback stage
// carries. nodeModel is the dispatching node's own model (its family is
// what the crossing serves); ok is false for pairs that cannot serve the
// node's family, or have no mapping — the caller drops the stage and says
// so, never emitting a spec dispatch would refuse. The reason names WHICH
// refusal: the structural no-mapping, or the class table's cell resolving
// nowhere on the target family — two different answers to "why was this
// rung skipped", and the caller emits both verbatim.
//
// classes is the EFFECTIVE class table (ResolveClasses over a policy's
// overrides); a zero ClassTable means the shipped one. Since delivery 2
// the wholesale-crossing target is the node model's CLASS rendering on
// the target family — delivery 1 crossed everything to the shipped openai
// default, which the table's standard cell still pins: an unknown model
// classifies standard, so every model the table is silent on crosses
// exactly where it crossed before.
//
// The rules:
//   - claude_code serves anthropic-wire credential families and reads the
//     node's model BARE (it strips only an `anthropic/` prefix); an
//     openai-family node model is not servable — no mapping.
//   - codex serves the openai family: an openai-family node model rides
//     (a `codex/` prefix strips); a foreign model crosses to its CLASS's
//     openai rendering (codex's catalog is disjoint from every other
//     family's).
//   - claw requires `provider/model-id` specs (ParseModelSpec errors on
//     bare ids) and reads its own spelling per credential: anthropic_key
//     serves anthropic-family ids (`anthropic/` + the bare id); zai_key
//     likewise (the endpoint answers claude ids with its own model — the
//     facade); openai_key serves openai-family ids (`openai/` + the bare
//     id) and crosses a foreign model to `openai/` + the class's openai
//     rendering; moonshot_key and the remaining key providers have no
//     mapping.
func StageModel(harness, credential, nodeModel string, classes ClassTable) (model string, ok bool, reason string) {
	if classes.forward == nil {
		classes = ResolveClasses(nil)
	}
	if nodeModel == "" {
		return "", false, ReasonNoMapping
	}
	family, bare := modelFamily(nodeModel)
	switch harness {
	case HarnessClaudeCode:
		if family != familyAnthropicWire {
			return "", false, ReasonNoMapping
		}
		return bare, true, ""
	case HarnessCodex:
		switch credential {
		case CredChatGPTForfait, "openai_key":
			if family == familyOpenAI {
				return bare, true, ""
			}
			target, crossed := crossToClass(classes, family, bare, familyOpenAI)
			if !crossed {
				return "", false, target
			}
			return target, true, ""
		}
	case HarnessClaw:
		switch credential {
		case "anthropic_key":
			if family != familyAnthropicWire {
				return "", false, ReasonNoMapping
			}
			return "anthropic/" + bare, true, ""
		case "zai_key":
			if family != familyAnthropicWire {
				return "", false, ReasonNoMapping
			}
			return "anthropic/" + bare, true, ""
		case "openai_key":
			if family == familyOpenAI {
				return "openai/" + bare, true, ""
			}
			target, o := crossToClass(classes, family, bare, familyOpenAI)
			if !o {
				return "", false, target
			}
			return "openai/" + target, true, ""
		}
	}
	return "", false, ReasonNoMapping
}

// crossToClass renders the node model's CLASS on the target family — the
// wholesale crossing's target. ok=false carries the class-unresolved
// reason verbatim (the caller composes the drop line from it); ok=true
// carries the bare target spelling.
func crossToClass(classes ClassTable, family, bare, targetFamily string) (string, bool) {
	class := classes.ClassOf(family, bare)
	target, ok := classes.Target(class, targetFamily)
	if !ok {
		return "class " + class + " resolves nowhere on family " + targetFamily, false
	}
	return target, true
}

// ReasonNoMapping is the stage-skip reason for a pair that cannot serve
// the family at all — verbatim into the caller's drop line. The other
// skip reason (a class cell resolving nowhere) composes its own phrase,
// carrying the class and family: a level can fix THAT one with a
// model_classes cell, never this one.
const ReasonNoMapping = "has no delivery-1 model mapping"

// shippedOpenAIDefault is the openai family's shipped crossing default
// (the class table's standard rung; per-level overrides are delivery 2).
const shippedOpenAIDefault = "gpt-6-sol"

const (
	familyAnthropicWire = "anthropic-wire"
	familyOpenAI        = "openai"
	familyNone          = ""
)

// modelFamily classifies a model spec and strips ONE provider prefix:
// `openai/…`, `gpt-*` and `codex/…` are the openai family; `anthropic/…`
// and bare ids (claude-*, glm-*, anything else the anthropic-wire
// endpoints answer) are the anthropic-wire family. The bare id is what
// the per-credential spellings re-prefix. An UNRECOGNIZED single-segment
// prefix (`openai_compatible/<id>` — the gateway spelling, whose
// credential no sealed channel serves) yields familyNone. The
// same-family mappings refuse it and the stage drops, named; the
// WHOLESALE crossings do NOT — familyNone classifies standard and
// crosses to the class's openai rendering, exactly as any foreign model
// (the pre-class behavior for that spelling, pinned by test) — a
// re-prefixed invalid spec would dispatch nowhere, and the crossing at
// least spends a servable credential saying so.
func modelFamily(spec string) (family, bare string) {
	if before, after, found := strings.Cut(spec, "/"); found && !strings.Contains(after, "/") {
		switch before {
		case "openai", "codex":
			return familyOpenAI, after
		case "anthropic":
			return familyAnthropicWire, after
		default:
			return familyNone, spec
		}
	}
	if strings.HasPrefix(spec, "gpt-") {
		return familyOpenAI, spec
	}
	return familyAnthropicWire, spec
}

// SlotSealable reports whether any listed pair names the credential slot —
// the whitelist's granularity. A pair names a SLOT, not a sealed channel:
// the seal serves the slot ("the instance follows the tier walk"), the
// ladder's harness dimension picks the occupancies. An empty order (never
// post-Normalize) seals nothing.
func SlotSealable(order []string, credential string) bool {
	for _, pair := range order {
		if _, c, err := ParsePair(pair); err == nil && c == credential {
			return true
		}
	}
	return false
}

// HeldPairs returns the policy's pair order filtered to pairs the SEALED
// BUNDLE serves — a sealNone outcome is not held: no channel, no rung.
// The bundle reading is injected so the function stays pure:
// serves(harness, credential) answers "does the sealed bundle hold a
// channel this pair's routes can spend".
func HeldPairs(order []string, serves func(harness, credential string) bool) []string {
	var held []string
	for _, pair := range order {
		h, c, err := ParsePair(pair)
		if err != nil {
			continue
		}
		if serves(h, c) {
			held = append(held, pair)
		}
	}
	return held
}

// Stage is one computed run-fallback ladder rung. The MODEL is computed
// PER NODE at materialization (each node's own model is what the crossing
// maps — a run-level model would be wrong for every node but one), so a
// Stage carries the pair only; the materializer calls StageModel with the
// node's effective model and DROPS the rung for that node when the
// mapping is missing (named in the drops), never modelless.
type Stage struct {
	Harness    string
	Credential string
}
