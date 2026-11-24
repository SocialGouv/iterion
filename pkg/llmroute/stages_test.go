package llmroute

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The default order enumerates the ADR's "then the rest": every slot a
// sealed channel can serve, per the harness that reads it. Completeness is
// load-bearing — the whitelist filters slots against THIS list, and an
// unenumerated slot would silently vanish from every launch the moment
// the default answered (the slice-3 review's F2 regression).
func TestDefaultPairOrderCoversEverySealableSlot(t *testing.T) {
	sealable := map[string]bool{
		CredClaudeForfait: true, CredChatGPTForfait: true,
		"anthropic_key": true, "openai_key": true, "zai_key": true, "moonshot_key": true,
		"bedrock_key": true, "vertex_key": true, "azure_key": true, "openrouter_key": true, "xai_key": true,
		"openai_compatible_key": true,
	}
	listed := map[string]bool{}
	for _, pair := range DefaultPairOrder {
		_, c, err := ParsePair(pair)
		if err != nil {
			t.Fatalf("default order entry %q: %v", pair, err)
		}
		listed[c] = true
	}
	for slot := range sealable {
		if !listed[slot] {
			t.Errorf("sealable slot %q is missing from DefaultPairOrder — launches would lose it on deploy", slot)
		}
	}
	// The host-env harnesses are correctly absent: their credentials
	// resolve outside every store, so their pairs can never be held.
	for _, pair := range DefaultPairOrder {
		h, _, _ := ParsePair(pair)
		if h == HarnessPi || h == HarnessGrok || h == HarnessKimi || h == HarnessOpencode {
			t.Errorf("pair %q names a host-env harness — its credentials are never sealable", pair)
		}
	}
}

// The stage mapping is keyed per PAIR, and the spellings protect the spend
// derivations: a zai stage on claw rides BARE (an anthropic/ prefix would
// pin "anthropic" in every derivation while dispatch spends ZAI), while
// the same crossing on claw+anthropic_key carries the true prefix.
func TestStageModelPairKeyed(t *testing.T) {
	tests := []struct {
		harness, credential, nodeModel string
		want                           string
		ok                             bool
	}{
		{HarnessClaudeCode, CredClaudeForfait, "claude-opus-5-5", "claude-opus-5-5", true},
		{HarnessClaudeCode, "anthropic_key", "claude-opus-5-5", "claude-opus-5-5", true},
		{HarnessClaudeCode, "zai_key", "claude-opus-5-5", "claude-opus-5-5", true},
		{HarnessCodex, CredChatGPTForfait, "claude-opus-5-5", "gpt-6-sol", true},
		{HarnessCodex, "openai_key", "claude-opus-5-5", "gpt-6-sol", true},
		{HarnessCodex, CredChatGPTForfait, "gpt-6-mini", "gpt-6-mini", true},
		{HarnessClaw, "anthropic_key", "claude-opus-5-5", "anthropic/claude-opus-5-5", true},
		// claw REQUIRES provider/model-id specs: the bare id would fail
		// ParseModelSpec before any call (the slice-3 review's M2).
		{HarnessClaw, "zai_key", "claude-opus-5-5", "anthropic/claude-opus-5-5", true},
		{HarnessClaw, "zai_key", "anthropic/glm-5-3", "anthropic/glm-5-3", true},
		// A foreign-family model crosses to the shipped openai default —
		// "openai/glm-5.3" would be a spec no openai endpoint serves.
		{HarnessClaw, "openai_key", "claude-opus-5-5", "openai/gpt-6-sol", true},
		{HarnessClaw, "openai_key", "openai/gpt-6-mini", "openai/gpt-6-mini", true},
		// The family bounds: an anthropic key does not serve openai ids,
		// an openai key does not serve claude ids on claude_code.
		{HarnessClaw, "anthropic_key", "openai/gpt-6-mini", "", false},
		{HarnessClaudeCode, "anthropic_key", "openai/gpt-6-mini", "", false},
		{HarnessClaw, "moonshot_key", "claude-opus-5-5", "", false},
		// An unrecognized provider prefix (the gateway spelling) maps
		// nothing — a re-prefixed invalid spec would dispatch nowhere.
		{HarnessClaudeCode, "anthropic_key", "openai_compatible/mistral", "", false},
		{HarnessClaw, "anthropic_key", "openai_compatible/mistral", "", false},
		// The gateway's own ids are provider-first, so its spelling nests:
		// the nested spec maps exactly as the one-segment one, never as an
		// anthropic-wire id re-prefixed into `anthropic/openai_compatible/…`.
		{HarnessClaw, "anthropic_key", "openai_compatible/scaleway/glm-5.3", "", false},
		{HarnessClaw, "zai_key", "openai_compatible/scaleway/glm-5.3", "", false},
		{HarnessClaudeCode, "anthropic_key", "openai_compatible/scaleway/glm-5.3", "", false},
		// The wholesale crossing answers both spellings alike: familyNone
		// classifies standard and crosses to the class's openai rendering —
		// a vendor openai key serving a node written for the gateway.
		{HarnessClaw, "openai_key", "openai_compatible/mistral", "openai/gpt-6-sol", true},
		{HarnessClaw, "openai_key", "openai_compatible/scaleway/glm-5.3", "openai/gpt-6-sol", true},
		// The trap: a provider-first id WITHOUT the gateway prefix is no
		// gateway spec and no family's own id — no same-family mapping serves
		// it, and the crossing never sends it to the vendor endpoint as
		// written (`openai/gpt-oss-120b`).
		{HarnessClaw, "anthropic_key", "scaleway/gpt-oss-120b", "", false},
		{HarnessClaw, "zai_key", "scaleway/gpt-oss-120b", "", false},
		{HarnessClaudeCode, "anthropic_key", "scaleway/gpt-oss-120b", "", false},
		{HarnessClaw, "openai_key", "scaleway/gpt-oss-120b", "openai/gpt-6-sol", true},
		// The class: dispatch reads the routing prefix at the FIRST slash and
		// sends the rest verbatim, so a nested id belongs to its prefix's
		// family, never to the bare-id reading. openai/ is the openai wire
		// whatever id it carries (`qwen/qwen-max` behind an OpenAI-shaped
		// endpoint): its rungs carry the node's own id.
		{HarnessClaw, "openai_key", "openai/qwen/qwen-max", "openai/qwen/qwen-max", true},
		{HarnessCodex, CredChatGPTForfait, "openai/qwen/qwen-max", "qwen/qwen-max", true},
		{HarnessClaw, "anthropic_key", "openai/qwen/qwen-max", "", false},
		{HarnessClaudeCode, CredClaudeForfait, "openai/qwen/qwen-max", "", false},
		// anthropic/ is the anthropic wire, a region-prefixed id included:
		// its prefix is stripped once, never doubled.
		{HarnessClaw, "anthropic_key", "anthropic/eu/claude-opus-5", "anthropic/eu/claude-opus-5", true},
		{HarnessClaudeCode, CredClaudeForfait, "anthropic/eu/claude-opus-5", "eu/claude-opus-5", true},
		{HarnessClaw, "openai_key", "anthropic/eu/claude-opus-5", "openai/gpt-6-sol", true},
		// Any other prefix is no family's, however deep its id: no
		// same-family rung serves another vendor's route, and the crossing
		// answers it as any foreign model.
		{HarnessClaw, "anthropic_key", "moonshot/kimi-code/kimi-for-coding", "", false},
		{HarnessClaw, "zai_key", "moonshot/kimi-code/kimi-for-coding", "", false},
		{HarnessClaudeCode, CredClaudeForfait, "moonshot/kimi-code/kimi-for-coding", "", false},
		{HarnessClaw, "openai_key", "moonshot/kimi-code/kimi-for-coding", "openai/gpt-6-sol", true},
		{HarnessClaw, "anthropic_key", "xai/grok/grok-3-mini", "", false},
		{HarnessClaudeCode, CredClaudeForfait, "openrouter/anthropic/claude-opus-5", "", false},
		{"pi", CredClaudeForfait, "claude-opus-5-5", "", false},
	}
	for _, tt := range tests {
		got, ok, reason := StageModel(tt.harness, tt.credential, tt.nodeModel, ResolveClasses(nil), false)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("StageModel(%s, %s, %s) = %q, %v — want %q, %v", tt.harness, tt.credential, tt.nodeModel, got, ok, tt.want, tt.ok)
		}
		// The shipped table is full, so every refusal here is structural.
		if !ok && reason != ReasonNoMapping {
			t.Errorf("StageModel(%s, %s, %s) refused with %q, want %q", tt.harness, tt.credential, tt.nodeModel, reason, ReasonNoMapping)
		}
	}

	// One reading per prefix: a nested id maps as its one-segment spelling
	// on every pair the default order lists — same family, same refusal,
	// same reason. The nesting belongs to the id, never to a second family
	// reading. Where a family's rung re-prefixes the node's own id, the two
	// models differ by that id (the rows above pin them); a prefix that is
	// no family's maps both spellings to one answer, the crossing included.
	spellings := []struct {
		one, nested string
		sameModel   bool
	}{
		{"openai_compatible/glm-5.3", "openai_compatible/scaleway/glm-5.3", true},
		{"moonshot/kimi-k2", "moonshot/kimi-code/kimi-for-coding", true},
		{"xai/grok-3-mini", "xai/grok/grok-3-mini", true},
		{"openrouter/claude-opus-5", "openrouter/anthropic/claude-opus-5", true},
		{"scaleway/gpt-oss-120b", "scaleway/oss/gpt-oss-120b", true},
		{"openai/gpt-6-mini", "openai/qwen/qwen-max", false},
		{"codex/gpt-6-mini", "codex/qwen/qwen-max", false},
		{"anthropic/claude-opus-5", "anthropic/eu/claude-opus-5", false},
	}
	for _, sp := range spellings {
		for _, pair := range DefaultPairOrder {
			h, c, err := ParsePair(pair)
			if err != nil {
				t.Fatalf("default order entry %q: %v", pair, err)
			}
			oneModel, oneOK, oneReason := StageModel(h, c, sp.one, ResolveClasses(nil), false)
			nestedModel, nestedOK, nestedReason := StageModel(h, c, sp.nested, ResolveClasses(nil), false)
			if oneOK != nestedOK || oneReason != nestedReason || (sp.sameModel && oneModel != nestedModel) {
				t.Errorf("%s: %s → (%q, %v, %q) but %s → (%q, %v, %q) — the two spellings of one prefix disagree", pair, sp.one, oneModel, oneOK, oneReason, sp.nested, nestedModel, nestedOK, nestedReason)
			}
		}
	}
	// A node with no model maps nothing: a modelless stage is never born.
	if _, ok, _ := StageModel(HarnessClaw, "anthropic_key", "", ResolveClasses(nil), false); ok {
		t.Error("StageModel with an empty node model = ok — a modelless stage would be refused downstream")
	}
	// Nor does a prefix with nothing after its slash (`anthropic/`, a
	// template whose variable expanded empty): it names no id, so no pair
	// maps it to a modelless rung or re-prefixes the empty id.
	for _, spec := range []string{"anthropic/", "openai/", "codex/"} {
		for _, pair := range DefaultPairOrder {
			h, c, err := ParsePair(pair)
			if err != nil {
				t.Fatalf("default order entry %q: %v", pair, err)
			}
			if got, ok, _ := StageModel(h, c, spec, ResolveClasses(nil), false); ok && (got == "" || strings.HasSuffix(got, "/")) {
				t.Errorf("StageModel(%s, %s) = %q — a rung with no model id", pair, spec, got)
			}
		}
	}

	// The class feature's FIRST payoff, named: the table's fast rung
	// serves a wholesale crossing for the model it names — delivery 1
	// crossed everything to gpt-6-sol. Exactly these rows differ from
	// delivery 1 (review-probed: gpt-6-luna as a NODE model never
	// crosses — openai-family models ride — so it is not a delta row).
	fast := []struct {
		harness, credential, nodeModel, want string
	}{
		{HarnessCodex, CredChatGPTForfait, "claude-haiku-4-5", "gpt-6-luna"},
		{HarnessCodex, "openai_key", "anthropic/claude-haiku-4-5", "gpt-6-luna"},
		{HarnessClaw, "openai_key", "claude-haiku-4-5", "openai/gpt-6-luna"},
		{HarnessClaw, "openai_key", "anthropic/claude-haiku-4-5", "openai/gpt-6-luna"},
	}
	for _, tt := range fast {
		got, ok, _ := StageModel(tt.harness, tt.credential, tt.nodeModel, ResolveClasses(nil), false)
		if !ok || got != tt.want {
			t.Errorf("fast crossing StageModel(%s, %s, %s) = %q, %v — want %q (the table's fast rung)", tt.harness, tt.credential, tt.nodeModel, got, ok, tt.want)
		}
	}

	// The two skip reasons are distinct: a pair that cannot serve the
	// family at all says so, and a class cell resolving nowhere (only a
	// custom table reaches it — the shipped one is full) names the class
	// and family, the fixable refusal.
	empty := ClassTable{}
	_, _, reason := StageModel("pi", CredClaudeForfait, "claude-opus-5-5", ResolveClasses(nil), false)
	if reason != ReasonNoMapping {
		t.Fatalf("structural skip reason = %q, want %q", reason, ReasonNoMapping)
	}
	custom := ResolveClasses(nil)
	custom.forward = map[string]map[string]string{ClassStandard: {familyOpenAI: "gpt-6-sol"}}
	_, ok, reason := StageModel(HarnessCodex, CredChatGPTForfait, "claude-opus-5-5", custom, false)
	if ok || reason != "class top resolves nowhere on family openai" {
		t.Fatalf("class-unresolved = %q, %v — want the named class+family reason", reason, ok)
	}
	_ = empty
}

// ADR-131 §4, the stamped counterpart of TestStageModelPairKeyed's gateway
// rows: under a team_gateway content boundary a node routed to the team's
// gateway maps on NO pair the default order lists — the wholesale crossings
// those rows pin (claw+openai_key, codex's forfait and key) included — and
// the reason names the boundary. Without the boundary (the operator's env
// gateway, an unstamped run) the crossings land exactly as the rows above
// pin. A node routed to a vendor maps the same under either answer: the
// boundary holds the gateway's content, not a mixed run's vendor nodes.
// Mutants: the boundary check dropped → the stamped rows red; the check
// blind to the flag → the unstamped crossings red; the check on every node →
// the vendor rows red.
func TestStageModelUnderTheTeamGatewayBoundary(t *testing.T) {
	classes := ResolveClasses(nil)
	crossings := []struct{ harness, credential string }{
		{HarnessClaw, "openai_key"}, {HarnessCodex, CredChatGPTForfait}, {HarnessCodex, "openai_key"},
	}
	gatewayModels := []string{"openai_compatible/scaleway/glm-5.3", "openai_compatible/mistral", " OpenAI_Compatible/scaleway/glm-5.3"}
	for _, cr := range crossings {
		for _, mdl := range gatewayModels[:2] {
			if _, ok, _ := StageModel(cr.harness, cr.credential, mdl, classes, false); !ok {
				t.Fatalf("unstamped StageModel(%s+%s, %s) refused — the control crossing is gone, so the stamped refusal below proves nothing", cr.harness, cr.credential, mdl)
			}
		}
	}
	for _, pair := range DefaultPairOrder {
		h, c, err := ParsePair(pair)
		if err != nil {
			t.Fatalf("default order entry %q: %v", pair, err)
		}
		for _, mdl := range gatewayModels {
			if got, ok, reason := StageModel(h, c, mdl, classes, true); ok || reason != ReasonContentBoundary {
				t.Errorf("stamped StageModel(%s, %s) = %q, %v, %q — want a drop with the boundary's reason: a gateway node's content never crosses to a vendor credential", pair, mdl, got, ok, reason)
			}
		}
		for _, mdl := range []string{"claude-opus-5-5", "anthropic/claude-opus-5", "openai/gpt-6-mini", "gpt-6-sol", "scaleway/gpt-oss-120b"} {
			wantModel, wantOK, wantReason := StageModel(h, c, mdl, classes, false)
			if got, ok, reason := StageModel(h, c, mdl, classes, true); got != wantModel || ok != wantOK || reason != wantReason {
				t.Errorf("stamped StageModel(%s, %s) = (%q, %v, %q), unstamped (%q, %v, %q) — the boundary moved a vendor node's rung", pair, mdl, got, ok, reason, wantModel, wantOK, wantReason)
			}
		}
	}
}

// The gateway pair is a seal-whitelist entry and never a ladder rung: the
// order lists it — once, on claw — so the whitelist can seal the gateway
// slot, and no arm of StageModel has a case for the slot, so a stage
// naming it drops on every harness and every node, whatever the node's
// model — the gateway's own spelling and a vendor's alike. The absence of
// that case on every arm IS the guarantee.
func TestStageModelNeverMapsTheGatewayPair(t *testing.T) {
	var listed []string
	for _, pair := range DefaultPairOrder {
		if _, c, err := ParsePair(pair); err == nil && c == "openai_compatible_key" {
			listed = append(listed, pair)
		}
	}
	if want := []string{Pair(HarnessClaw, "openai_compatible_key")}; !reflect.DeepEqual(listed, want) {
		t.Fatalf("DefaultPairOrder names the gateway slot as %v, want exactly %v — the whitelist seals the slot through claw's pair alone", listed, want)
	}
	for _, harness := range Harnesses {
		for _, nodeModel := range []string{
			"openai_compatible/scaleway/glm-5.3", "openai_compatible/glm-5.3",
			"claude-opus-5-5", "anthropic/claude-opus-5-5", "openai/gpt-6-mini", "gpt-6-sol",
		} {
			got, ok, reason := StageModel(harness, "openai_compatible_key", nodeModel, ResolveClasses(nil), false)
			if ok || reason != ReasonNoMapping {
				t.Errorf("StageModel(%s+openai_compatible_key, %s) = %q, %v, %q — want a drop with %q: the gateway slot is never a rung", harness, nodeModel, got, ok, reason, ReasonNoMapping)
			}
		}
	}
}

// claude_code maps exactly the slots its delegate spends — the
// anthropic-wire slots, secrets.AnthropicWireSlotOrder (the OAuth kind is
// the Claude forfait). A pair naming any other slot hands the delegate a
// provider hint it does not route on: the session would spend the wire's
// default precedence (the forfait, the Anthropic key) under that pair's
// name, and the fallback door would ask a donor for a credential the rung
// never spends. Every slot of the vocabulary is asked, the gateway's
// included.
func TestStageModelClaudeCodeMapsTheSlotsItSpends(t *testing.T) {
	spends := map[string]bool{}
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if slot == string(secrets.OAuthKindClaudeCode) {
			slot = CredClaudeForfait
		} else {
			slot += "_key"
		}
		if !validCredential(slot) {
			t.Errorf("secrets.AnthropicWireSlotOrder names %q, a slot the policy vocabulary does not know", slot)
		}
		spends[slot] = true
	}
	slots := []string{CredClaudeForfait, CredChatGPTForfait}
	for _, p := range ProviderHints {
		slots = append(slots, p+"_key")
	}
	for _, slot := range slots {
		got, ok, reason := StageModel(HarnessClaudeCode, slot, "claude-opus-5-5", ResolveClasses(nil), false)
		if spends[slot] {
			if !ok || got != "claude-opus-5-5" {
				t.Errorf("StageModel(claude_code+%s, claude-opus-5-5) = %q, %v — the delegate spends this slot, want the node's id", slot, got, ok)
			}
			continue
		}
		if ok || reason != ReasonNoMapping {
			t.Errorf("StageModel(claude_code+%s, claude-opus-5-5) = %q, %v, %q — the delegate never spends this slot, want a drop with %q", slot, got, ok, reason, ReasonNoMapping)
		}
	}
}

// SlotSealable is the whitelist's granularity: a slot is sealable when ANY
// listed pair names it (the harness dimension constrains the LADDER, not
// the seal — the instance follows the tier walk).
func TestSlotSealable(t *testing.T) {
	order := []string{Pair(HarnessClaw, "anthropic_key"), Pair(HarnessClaudeCode, CredClaudeForfait)}
	if !SlotSealable(order, "anthropic_key") || !SlotSealable(order, CredClaudeForfait) {
		t.Fatal("listed slots refused")
	}
	if SlotSealable(order, "zai_key") {
		t.Fatal("an unlisted slot admitted")
	}
	// An EMPTY order (no resolved policy) seals nothing — the caller gates
	// on the policy's presence before asking.
	if SlotSealable(nil, CredClaudeForfait) {
		t.Fatal("a nil order admitted everything — the caller must gate on the policy, not the list")
	}
}

// ADR-131 §5: the gateway slot joins the shipped default and the policy
// vocabulary as a SEAL-WHITELIST entry, never a ladder rung. Sealable under
// the default — a run launched today seals its team's gateway key — and
// SealOnly, the predicate the ladder composer drops the pair on, true for
// that slot alone: every other slot the default names orders a rung. A run
// frozen BEFORE the entry existed replays its list as stored and says so
// once; that half is witnessed at the owner site, where the whitelist is
// built from the run document's frozen pair order
// (pkg/server/cloudpublisher, TestGatewayKey_AFrozenPreUpgradeOrderReplaysAsStoredAndSaysSoOnce).
// Mutants: SealOnly answering false for the gateway → the first assertion
// reds; answering true for every slot → the loop reds on anthropic_key.
func TestGatewayPairIsASealWhitelistEntryNeverARung(t *testing.T) {
	if !SlotSealable(DefaultPairOrder, gatewaySlot) {
		t.Fatalf("SlotSealable(DefaultPairOrder, %q) = false: the shipped default must name the gateway slot, or no launch ever seals a team's gateway key", gatewaySlot)
	}
	if !validCredential(gatewaySlot) {
		t.Fatalf("validCredential(%q) = false: the policy vocabulary refuses the slot the default names", gatewaySlot)
	}
	if !SealOnly(gatewaySlot) {
		t.Fatalf("SealOnly(%q) = false: the gateway pair would become a ladder stage the moment a run seals its key", gatewaySlot)
	}
	for _, pair := range DefaultPairOrder {
		_, c, err := ParsePair(pair)
		if err != nil {
			t.Fatalf("default order entry %q: %v", pair, err)
		}
		if c != gatewaySlot && SealOnly(c) {
			t.Errorf("SealOnly(%q) = true: a rung slot of the default would never order a stage", c)
		}
	}
	// The structural half the composer relies on: no harness maps a node
	// model onto the gateway slot, so even a stage that slipped through
	// would drop on every node.
	for _, h := range []string{HarnessClaw, HarnessClaudeCode, HarnessCodex} {
		for _, mdl := range []string{"openai_compatible/scaleway/glm-5.3", "anthropic/claude-opus-5", "gpt-6-sol"} {
			if got, ok, _ := StageModel(h, gatewaySlot, mdl, ClassTable{}, false); ok {
				t.Errorf("StageModel(%s, %s, %s) = %q: the gateway slot must map no stage", h, gatewaySlot, mdl, got)
			}
		}
	}
}

// HeldPairs filters to pairs the sealed bundle serves; a sealNone slot
// (no channel) is not held.
func TestHeldPairs(t *testing.T) {
	order := []string{
		Pair(HarnessClaudeCode, CredClaudeForfait),
		Pair(HarnessClaw, "anthropic_key"),
		Pair(HarnessClaudeCode, "zai_key"),
	}
	held := HeldPairs(order, func(harness, credential string) bool {
		return credential == CredClaudeForfait || credential == "zai_key"
	})
	want := []string{Pair(HarnessClaudeCode, CredClaudeForfait), Pair(HarnessClaudeCode, "zai_key")}
	if !reflect.DeepEqual(held, want) {
		t.Fatalf("held = %v, want %v (policy order preserved, unheld dropped)", held, want)
	}
}
