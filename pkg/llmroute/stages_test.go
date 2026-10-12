package llmroute

import (
	"reflect"
	"testing"
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
		{"pi", CredClaudeForfait, "claude-opus-5-5", "", false},
	}
	for _, tt := range tests {
		got, ok, _ := StageModel(tt.harness, tt.credential, tt.nodeModel, ResolveClasses(nil))
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("StageModel(%s, %s, %s) = %q, %v — want %q, %v", tt.harness, tt.credential, tt.nodeModel, got, ok, tt.want, tt.ok)
		}
	}
	// A node with no model maps nothing: a modelless stage is never born.
	if _, ok, _ := StageModel(HarnessClaw, "anthropic_key", "", ResolveClasses(nil)); ok {
		t.Error("StageModel with an empty node model = ok — a modelless stage would be refused downstream")
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
		got, ok, _ := StageModel(tt.harness, tt.credential, tt.nodeModel, ResolveClasses(nil))
		if !ok || got != tt.want {
			t.Errorf("fast crossing StageModel(%s, %s, %s) = %q, %v — want %q (the table's fast rung)", tt.harness, tt.credential, tt.nodeModel, got, ok, tt.want)
		}
	}

	// The two skip reasons are distinct: a pair that cannot serve the
	// family at all says so, and a class cell resolving nowhere (only a
	// custom table reaches it — the shipped one is full) names the class
	// and family, the fixable refusal.
	empty := ClassTable{}
	_, _, reason := StageModel("pi", CredClaudeForfait, "claude-opus-5-5", ResolveClasses(nil))
	if reason != ReasonNoMapping {
		t.Fatalf("structural skip reason = %q, want %q", reason, ReasonNoMapping)
	}
	custom := ResolveClasses(nil)
	custom.forward = map[string]map[string]string{ClassStandard: {familyOpenAI: "gpt-6-sol"}}
	_, ok, reason := StageModel(HarnessCodex, CredChatGPTForfait, "claude-opus-5-5", custom)
	if ok || reason != "class top resolves nowhere on family openai" {
		t.Fatalf("class-unresolved = %q, %v — want the named class+family reason", reason, ok)
	}
	_ = empty
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
