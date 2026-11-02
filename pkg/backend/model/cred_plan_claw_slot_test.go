package model

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The claw slot question (#2288 revue round 2): a `provider:` hint on a
// claw route decides the ROUTE an element dispatch walks, never the SLOT
// the registry funds. The registry reads the hint at exactly one switch
// (forcing Anthropic-direct for models Anthropic serves) and the credential
// crossing forwards no hint-named slot, so the spend follows the model's
// own prefix — and a plan or an event word that says otherwise books
// nobody, or books the hint's credential for a prefix spend.

func TestServingProvider_clawReadsThePrefixOverTheHint(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name    string
		backend string
		hint    string
		model   string
		want    string
	}{
		// The hint never names a claw slot: the prefix's pin answers, GLM
		// folded. `provider: zai` over an openai spec describes a spend
		// claw never makes — the registry ignores the hint there.
		{"zai hint over an openai spec → openai", delegate.BackendClaw, "zai", "openai/gpt-5", "openai"},
		// The runner fence's exact shape (cred_plan_fine_booking_test.go):
		// the word the loop builder stamps for hint zai over the anthropic
		// prefix — the prefix's slot, the lent subscription the run funds.
		{"zai hint over a claude spec → anthropic", delegate.BackendClaw, "zai", "anthropic/claude-opus-5", "anthropic"},
		{"anthropic hint over a claude spec → anthropic", delegate.BackendClaw, "anthropic", "anthropic/claude-opus-5", "anthropic"},
		// A GLM id rides z.ai whatever the hint says (the registry's own
		// exemption); clawPinnedProvider folds it.
		{"anthropic hint over an anthropic/glm spec → zai", delegate.BackendClaw, "anthropic", "anthropic/glm-5.3", "zai"},
		{"no hint, prefixed spec → the prefix", delegate.BackendClaw, "", "openai/gpt-5", "openai"},
		// A bare spec names no slot; the hint is all there is (and the
		// registry refuses the node at Execute, so the word is inert).
		{"bare spec keeps the hint", delegate.BackendClaw, "zai", "claude-opus-5", "zai"},
		{"bare spec, no hint → empty", delegate.BackendClaw, "", "claude-opus-5", ""},
		// claude_code keeps the hint-first word: its executor really walks
		// the hint (the zai hint spends ONLY the z.ai key).
		{"claude_code hint-first, unchanged", delegate.BackendClaudeCode, "zai", "anthropic/claude-opus-4-8", "zai"},
		{"claude_code bare → empty", delegate.BackendClaudeCode, "", "claude-opus-4-8", ""},
		// The word leaves FOLDED on every backend: the plan's keys hold the
		// walk's lower-case words, so an authored route whose `provider:`
		// text is not lower-case — or a ${VAR} expanding to it — stamps a
		// word the plan can still match.
		{"claude_code folds the authored case", delegate.BackendClaudeCode, "ZAI", "anthropic/claude-opus-4-8", "zai"},
		{"claude_code folds space and case", delegate.BackendClaudeCode, " Moonshot ", "claude-opus-5", "moonshot"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := servingProvider(row.backend, &delegate.Task{ProviderHint: row.hint, Model: row.model})
			if got != row.want {
				t.Fatalf("servingProvider(%s, hint=%q, model=%q) = %q, want %q", row.backend, row.hint, row.model, got, row.want)
			}
		})
	}
}

func TestClawSpecProvider_isServingProviderWithoutAHint(t *testing.T) {
	t.Parallel()

	for _, spec := range []string{"anthropic/glm-5.3", "openai/gpt-5", "anthropic/claude-opus-5", "claude-opus-5"} {
		if got, want := clawSpecProvider(spec), servingProvider(delegate.BackendClaw, &delegate.Task{Model: spec}); got != want {
			t.Fatalf("clawSpecProvider(%q) = %q, want servingProvider's hint-less answer %q", spec, got, want)
		}
	}
}

// The walk's claw reading: a decided hint chain records the prefix's slot
// BESIDE the hints, so the credential plan keys the node's spend on the
// slot the route can actually fund. The hint's own rows stay too — the
// launch refusals read who NAMED what, and a claw hint names a route even
// though it never names a slot.
func TestEffectiveProviders_aClawHintChainAlsoRecordsThePrefixSlot(t *testing.T) {
	t.Parallel()

	nodeOf := func(provider, mdl string) ir.Node {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: provider, Model: mdl}}
	}

	// The disagreement shape: hint zai, prefix openai. The registry spends
	// openai; the plan must carry the openai row or the observation books
	// nobody.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"impl": nodeOf("zai", "openai/gpt-5")}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened on a hint+prefixed claw route: %+v", res)
	}
	if got := res.Sites["impl"]; !slicesEqual(got, []string{"zai", "openai"}) {
		t.Fatalf("Sites[impl] = %v, want [zai openai] — the hint names the route, the prefix names the slot", got)
	}
	if !containsProviderName(res.Providers, "openai") {
		t.Fatalf("Providers = %v, want openai in it — the prefix's slot is one the route may spend", res.Providers)
	}

	// The GLM shape under a hint: the walk must carry zai (the slot the
	// registry funds), not just the hint's anthropic.
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"impl": nodeOf("anthropic", "anthropic/glm-5.3")}}
	res = EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened on an anthropic/glm claw route: %+v", res)
	}
	if got := res.Sites["impl"]; !slicesEqual(got, []string{"anthropic", "zai"}) {
		t.Fatalf("Sites[impl] = %v, want [anthropic zai] — the GLM id rides z.ai whatever the hint says", got)
	}

	// Agreement shapes stay single-rowed: hint == prefix records once.
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"impl": nodeOf("anthropic", "anthropic/claude-opus-5")}}
	res = EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if got := res.Sites["impl"]; !slicesEqual(got, []string{"anthropic"}) {
		t.Fatalf("Sites[impl] = %v, want [anthropic] once — hint and prefix agree", got)
	}

	// A bare model with a hint stays the documented narrow-safe reading:
	// the registry refuses the spec at Execute, so nothing spends and the
	// hint's rows are inert — widening it would nil the plans of runs whose
	// other routes are readable.
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"impl": nodeOf("anthropic", "claude-opus-5")}}
	res = EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if !res.NarrowSafe || !slicesEqual(res.Providers, []string{"anthropic"}) {
		t.Fatalf("bare claw spec with a hint: got narrow-safe=%v providers=%v, want the documented narrow-safe [anthropic]", res.NarrowSafe, res.Providers)
	}
}

// End to end at the producer: the hint-steered claw routes plan BOTH
// slots, and the join the runner runs hits the prefix's row — the
// credential the route actually funds.
func TestCredentialPlan_aClawHintSteeredRoutePlansThePrefixSlot(t *testing.T) {
	t.Parallel()

	nodeOf := func(id, provider, mdl string) ir.Node {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: "claw", Provider: provider, Model: mdl}}
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl":   nodeOf("impl", "zai", "openai/gpt-5"),
		"review": nodeOf("review", "anthropic", "anthropic/glm-5.3"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	plan := CredentialPlan(res, planBundle())
	want := map[string]string{
		"impl|zai":         "fp-donor-zai",
		"impl|openai":      "fp-own-openai",
		"review|anthropic": "fp-own-anthropic",
		"review|zai":       "fp-donor-zai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — every slot the route can fund carries a row", plan, want)
	}
}

// The step event carries the loop's own provider word — the delegate
// events' `provider` — omitted when the loop stamped none, the
// putDelegateModelFields convention: observers tell "unknown" from a
// measured word by the key's absence.
func TestOnLLMStepFinish_stampsTheLoopProviderWord(t *testing.T) {
	t.Parallel()

	em := &captureEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-slot", iterlog.Nop(), nil, nil)
	hooks.OnLLMStepFinish("impl", LLMStepInfo{Number: 1, InputTokens: 10, OutputTokens: 2, Provider: "openai"})
	hooks.OnLLMStepFinish("impl", LLMStepInfo{Number: 2, InputTokens: 5, OutputTokens: 1})

	if len(em.events) != 2 {
		t.Fatalf("events = %d, want 2", len(em.events))
	}
	if got := em.events[0].Data["provider"]; got != "openai" {
		t.Fatalf("llm_step_finished provider = %v, want the loop's own word openai", got)
	}
	if _, present := em.events[1].Data["provider"]; present {
		t.Fatal("a wordless step persisted a provider key — the absence IS the unknown, never an empty string")
	}
}

// The bridge the loop builders feed: GenerationOptions.Provider reaches
// every step info applyHooks converts, beside the iteration — the chain
// between `opts.Provider = servingProvider(...)` (claw_backend.go) and the
// event stamp above. A bridge that drops the word leaves every step
// wordless while the unit tests above stay green.
func TestApplyHooks_carriesTheLoopProviderOntoEveryStep(t *testing.T) {
	t.Parallel()

	var got []LLMStepInfo
	h := EventHooks{OnLLMStepFinish: func(_ string, step LLMStepInfo) { got = append(got, step) }}
	opts := GenerationOptions{Provider: "openai"}
	applyHooks("impl", 2, h, &opts)
	opts.OnStepFinish(StepResult{Number: 1})
	if len(got) != 1 {
		t.Fatalf("steps = %d, want 1", len(got))
	}
	if got[0].Provider != "openai" {
		t.Fatalf("step provider = %q, want the loop's word openai — the bridge must carry it", got[0].Provider)
	}
	if got[0].Iteration != 2 {
		t.Fatalf("step iteration = %d, want 2 — the word rides beside it, not instead of it", got[0].Iteration)
	}
}

// The relay round-trip: the container's step envelope carries the word and
// the host re-fires it; an envelope predating the field decodes to "".
func TestRelayedLLMStep_carriesTheProviderWord(t *testing.T) {
	t.Parallel()

	// Encode through the relay's own path: capture the envelope it sends,
	// decode its EventData payload the way the multiplexer does.
	var sent delegate.Envelope
	capturing := SandboxRelayHooks(func(env delegate.Envelope) error { sent = env; return nil }, nil)
	capturing.OnLLMStepFinish("container-node", LLMStepInfo{Number: 1, InputTokens: 3, Provider: "zai"})
	if sent.Type != delegate.EnvelopeEvent {
		t.Fatalf("envelope type = %q, want the event envelope", sent.Type)
	}
	var wire delegate.EventData
	if err := json.Unmarshal(sent.Data, &wire); err != nil {
		t.Fatalf("decode EventData: %v", err)
	}
	if wire.Type != string(store.EventLLMStepFinished) {
		t.Fatalf("event type = %q, want llm_step_finished", wire.Type)
	}

	host := &captureEmitter{}
	target := NewStoreEventHooks(context.Background(), host, "run-relay", iterlog.Nop(), nil, nil)
	handled, err := ApplyRelayedEvent(target, "impl", wire.Type, wire.Payload)
	if err != nil || !handled {
		t.Fatalf("ApplyRelayedEvent = (%v, %v), want handled with no error", handled, err)
	}
	if len(host.events) != 1 {
		t.Fatalf("re-fired events = %d, want 1", len(host.events))
	}
	if got := host.events[0].Data["provider"]; got != "zai" {
		t.Fatalf("relayed provider = %v, want zai — the word crosses whole", got)
	}

	// An OLD container's envelope: no provider key. The host reads "" and
	// the event carries no word — its own derivation answers, the same skew
	// tolerance as every capability an older sandbox image lacks.
	var old map[string]any
	if err := json.Unmarshal([]byte(`{"step":1,"input_tokens":3,"output_tokens":1}`), &old); err != nil {
		t.Fatalf("seed decode: %v", err)
	}
	handled, err = ApplyRelayedEvent(target, "impl", string(store.EventLLMStepFinished), old)
	if err != nil || !handled {
		t.Fatalf("ApplyRelayedEvent(old) = (%v, %v), want handled", handled, err)
	}
	if _, present := host.events[1].Data["provider"]; present {
		t.Fatal("an old envelope re-fired with a provider key — absence must stay absence")
	}
}

// containsProviderName reports whether list names s — the walk's Providers
// reading, asserted by membership.
func containsProviderName(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The override arm of the claw slot reading (revi round 3): a launch
// override cell whose provider hint decides the route still leaves claw's
// SLOT on the model's prefix — the same registry reading the node-route arm
// above serves. Without the prefix's row here, a hint-steered override
// books nobody on exactly the nodes an operator re-pointed at launch.
func TestEffectiveProviders_aClawProviderOverrideAlsoRecordsThePrefixSlot(t *testing.T) {
	t.Parallel()

	nodeOf := func() ir.Node {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "zai", Model: "anthropic/claude-opus-5"}}
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"impl": nodeOf()}}
	var ovs ModelOverrides
	ovs.SetProvider("impl", "openai")
	res := EffectiveProviders(RouteView{Workflow: wf, Overrides: ovs}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened on an override-steered claw route: %+v", res)
	}
	if got := res.Sites["impl"]; !slicesEqual(got, []string{"openai", "anthropic"}) {
		t.Fatalf("Sites[impl] = %v, want [openai anthropic] — the override names the route, the prefix names the slot claw funds", got)
	}
	plan := CredentialPlan(res, planBundle())
	if got := plan["impl|anthropic"]; got != "fp-own-anthropic" {
		t.Fatalf("plan[impl|anthropic] = %q, want %q — the prefix's slot plans the credential the route funds (the override's openai hint names no claw slot)", got, "fp-own-anthropic")
	}
}

// The rescue-route arm of the claw slot reading (revi round 3): an authored
// `fallbacks:` route on claw reads through resolveRoute like the primary,
// and its hint decides the route its model's prefix never does. Without the
// prefix's row, the fired rescue books nobody — the donor's rung spent, the
// plan silent.
func TestEffectiveProviders_aClawFallbackHintAlsoRecordsThePrefixSlot(t *testing.T) {
	t.Parallel()

	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "impl"},
			LLMFields: ir.LLMFields{Backend: "claw", Provider: "anthropic", Model: "anthropic/claude-opus-5"},
			Fallbacks: []ir.Fallback{{Name: "donor", Backend: "claw", Provider: "zai", Model: "openai/gpt-5"}},
		},
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened on a hint-steered claw fallback: %+v", res)
	}
	if got := res.Sites["impl#donor"]; !slicesEqual(got, []string{"zai", "openai"}) {
		t.Fatalf("Sites[impl#donor] = %v, want [zai openai] — the fallback's hint names the route, its model's prefix names the slot", got)
	}
	plan := CredentialPlan(res, planBundle())
	if got := plan["impl|openai"]; got != "fp-own-openai" {
		t.Fatalf("plan[impl|openai] = %q, want %q — the fired rescue's prefix slot plans its credential", got, "fp-own-openai")
	}
}

// The run-level chain's claw stage reads through the same rescue-route arm,
// and its providers spread onto every node: the stage's hint names the
// route claw walks, its model's prefix names the slot the registry funds.
// Without the prefix in the chain's spread, the fired stage books nobody on
// every node it rescues.
func TestCredentialPlan_aClawRunFallbackStagePlansItsPrefixSlot(t *testing.T) {
	t.Parallel()

	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl":   agentWith("impl", "anthropic", "anthropic/claude-opus-5"),
		"review": agentWith("review", "openai", "openai/gpt-5"),
	}}
	view := RouteView{Workflow: wf, RunFallbacks: []FallbackEntry{{Backend: "claw", Provider: "zai", Model: "openai/gpt-5"}}}
	res := EffectiveProviders(view, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened on a hint-steered run fallback: %+v", res)
	}
	plan := CredentialPlan(res, planBundle())
	for _, node := range []string{"impl", "review"} {
		if got := plan[node+"|openai"]; got != "fp-own-openai" {
			t.Fatalf("plan[%s|openai] = %q, want %q — the chain stage's prefix slot plans on every node", node, got, "fp-own-openai")
		}
		if got := plan[node+"|zai"]; got != "fp-donor-zai" {
			t.Fatalf("plan[%s|zai] = %q, want %q — the stage's hint stays recorded beside the prefix", node, got, "fp-donor-zai")
		}
	}
}

// The wiring witness (revi round 3): ClawBackend.Execute stamps
// GenerationOptions.Provider from servingProvider before applyHooks copies
// it onto every step — the chain the runner's plan join reads. A wiring
// that drops the word leaves every step wordless while the unit tables
// above stay green, and a hint-steered claw route then books nobody.
func TestClawBackendExecute_stampsTheServingSlotOnEveryStep(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	mock := &captureRequestClient{stream: mockStreamEvents("ok", "end_turn")}
	reg.Register("openai", func(string) (api.APIClient, error) { return mock, nil })
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })

	var steps []LLMStepInfo
	hooks := EventHooks{OnLLMStepFinish: func(_ string, step LLMStepInfo) { steps = append(steps, step) }}
	backend := NewClawBackend(reg, hooks, RetryPolicy{})
	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "impl",
		ProviderHint: "zai",
		Model:        "anthropic/claude-opus-5",
		UserPrompt:   "ping",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("the loop produced no step event — the witness premise is broken")
	}
	for _, step := range steps {
		if step.Provider != "anthropic" {
			t.Fatalf("step provider = %q, want \"anthropic\" — the slot the client was resolved from (the prefix, never the zai hint) must ride every step", step.Provider)
		}
	}
}
