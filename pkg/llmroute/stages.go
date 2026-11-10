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
//   - claude_code serves the anthropic-wire credential slots its delegate
//     spends (claudeCodeSpends: the Claude forfait, anthropic_key and the
//     facade keys) and reads the node's model BARE (it strips only an
//     `anthropic/` prefix). An openai-family node model is not servable,
//     and a pair naming any other slot has no mapping.
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
//
// No arm has a case for openai_compatible_key, by design: the gateway pair
// (claw's, the only one Validate admits) is in the order for the seal
// whitelist alone, a gateway route is the node's own spec and never a
// crossing's target, and a case on any arm would make the pair a ladder
// rung.
//
// teamGateway is the run's content boundary (ADR-131 §4,
// store.RunContentBoundary.TeamGateway): under it a node routed to the
// team's gateway maps on NO pair (ReasonContentBoundary) — its same-family
// rungs refuse the gateway spelling anyway, and the wholesale crossings
// (claw+openai_key, codex's) would carry the node's content to a vendor
// credential, off the gateway the run is held to. A node routed elsewhere
// maps as without the boundary, and an unstamped run (the operator's own
// environment gateway, a run launched before the stamp) keeps the
// crossing.
func StageModel(harness, credential, nodeModel string, classes ClassTable, teamGateway bool) (model string, ok bool, reason string) {
	if classes.forward == nil {
		classes = ResolveClasses(nil)
	}
	if nodeModel == "" {
		return "", false, ReasonNoMapping
	}
	if teamGateway && GatewayRoute(nodeModel) {
		return "", false, ReasonContentBoundary
	}
	family, bare := modelFamily(nodeModel)
	switch harness {
	case HarnessClaudeCode:
		if !claudeCodeSpends(credential) || family != familyAnthropicWire {
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

// claudeCodeSpends reports whether the claude_code delegate spends the
// slot. Those are the anthropic-wire slots of its precedence,
// secrets.AnthropicWireSlotOrder (held equal by test): the facade keys, the
// Anthropic key and the Claude forfait. The delegate routes on no other
// provider hint. A pair naming another slot would run the session on the
// wire's default precedence under that pair's name, and the fallback door
// would ask a donor for a credential the rung never spends.
func claudeCodeSpends(credential string) bool {
	switch credential {
	case CredClaudeForfait, "anthropic_key", "zai_key", "moonshot_key":
		return true
	}
	return false
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

// ReasonContentBoundary is the stage-skip reason for a node routed to the
// team's gateway on a run whose content boundary holds it there (ADR-131
// §4) — verbatim into the caller's drop line. No model_classes cell lifts
// it: the boundary is the run's, frozen at launch.
const ReasonContentBoundary = "is refused for a node routed to the team's gateway: the run's content boundary (team_gateway) holds its content there, and a crossing would carry it to a vendor credential"

// ReasonContentBoundaryUndecided is the stage-skip reason for a node whose
// route the screen's reading cannot decide — a `{{vars.*}}` span no vars
// view answers — on a run held to the team's gateway: the run decides the
// route at dispatch, it may be the gateway's, and a crossing mapped from
// the template text would carry the node's content to a vendor credential.
const ReasonContentBoundaryUndecided = "is refused for a node whose route this reading cannot decide: the run's content boundary (team_gateway) holds a gateway-routed node's content there, and the route dispatch resolves may be the gateway's"

// GatewayRoute reports whether a model spec is a route to the gateway: its
// routing prefix — everything before the first slash, read case-blind and
// trimmed, the walk's own reading (model.rawGatewayPrefix) — is the
// gateway's, whatever follows. Read generously on purpose: under the
// boundary, a spelling dispatch might not serve is still a node its author
// meant for the gateway, and the refusal is the safe answer.
func GatewayRoute(nodeModel string) bool {
	prefix, _, routed := strings.Cut(strings.TrimSpace(nodeModel), "/")
	return routed && strings.EqualFold(strings.TrimSpace(prefix), ProviderOpenAICompatible)
}

// shippedOpenAIDefault is the openai family's shipped crossing default
// (the class table's standard rung; per-level overrides are delivery 2).
const shippedOpenAIDefault = "gpt-6-sol"

const (
	familyAnthropicWire = "anthropic-wire"
	familyOpenAI        = "openai"
	familyNone          = ""
)

// modelFamily classifies a model spec and strips ONE provider prefix, read
// the way dispatch reads it (modelroute.Parse, mirrored here because
// llmroute is the zero-dependency leaf): the routing prefix is everything
// before the FIRST slash, and the rest is the id verbatim, slashes
// included. `openai/…` (`openai/qwen/qwen-max` is the openai wire serving
// `qwen/qwen-max` behind an OpenAI-shaped endpoint), `codex/…` and bare
// `gpt-*` ids are the openai family; `anthropic/…` (a region-prefixed
// `anthropic/eu/claude-opus-5` included) and the other bare ids (claude-*,
// glm-*, anything else the anthropic-wire endpoints answer) are the
// anthropic-wire family. The bare id is what the per-credential spellings
// re-prefix, so a same-family rung carries the node's own id.
//
// Any other prefix yields familyNone with the WHOLE spec as the bare id,
// however many slashes its id holds: the gateway's (`openai_compatible/…`,
// whose ids are often provider-first: `openai_compatible/scaleway/glm-5.3`),
// another vendor's route (`moonshot/kimi-code/kimi-for-coding`,
// `xai/grok/grok-3-mini`), a slash inside a model id
// (`scaleway/gpt-oss-120b`). Read as a bare anthropic-wire id, such a spec
// would be re-prefixed into a route no endpoint serves
// (`anthropic/moonshot/…`). The same-family mappings refuse familyNone and
// the stage drops, named; the WHOLESALE crossings do NOT — familyNone
// classifies standard and crosses to the class's openai rendering, exactly
// as any foreign model (pinned by test) — a re-prefixed invalid spec would
// dispatch nowhere, and the crossing at least spends a servable credential
// saying so. A prefix with nothing after its slash (`anthropic/`, a
// template whose variable expanded empty) names no id and is familyNone
// too: a same-family rung would carry no model, and a harness given none
// runs its own default.
func modelFamily(spec string) (family, bare string) {
	prefix, id, routed := strings.Cut(spec, "/")
	if !routed {
		if strings.HasPrefix(spec, "gpt-") {
			return familyOpenAI, spec
		}
		return familyAnthropicWire, spec
	}
	if id != "" {
		switch prefix {
		case "openai", "codex":
			return familyOpenAI, id
		case "anthropic":
			return familyAnthropicWire, id
		}
	}
	return familyNone, spec
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

// SealOnly reports whether a credential slot rides the pair order for the
// seal whitelist alone and orders no ladder rung — the gateway slot (ADR-131
// §5): a gateway route is the node's own spec, never a crossing's target, so
// StageModel maps it on no harness, and the ladder composer drops the pair
// before it becomes a stage the run message would carry.
func SealOnly(credential string) bool { return credential == gatewaySlot }

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
