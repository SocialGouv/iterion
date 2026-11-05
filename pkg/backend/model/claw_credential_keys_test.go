package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// What a claw call spent travels with its usage events (ADR-161 decision 8):
// the process that built the call writes the arm's Spend as the events'
// credential keys, and the spend ledger and the fine table read them, never a
// re-derivation from the model and the provider word.

// lockedEmitter records events under a mutex: the loop's hooks may fire from
// more than one goroutine.
type lockedEmitter struct {
	mu     sync.Mutex
	events []store.Event
}

func (c *lockedEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
	return &evt, nil
}

func (c *lockedEmitter) snapshot() []store.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.Event(nil), c.events...)
}

// An in-process claw call stamps the arm's Spend — here the pod's key, its
// account named — on every step event and on its delegate event: the
// dispatch's own resolution, the one its client was built from.
func TestAnInProcessClawCallsEventsCarryTheArmsSpend(t *testing.T) {
	blankAmbientEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	reg := NewRegistry()
	mock := newMockClient(textEvents("done", 60, 5))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	em := &lockedEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-keys", iterlog.Nop(), nil, nil)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: "Say done."}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithWorkDir(t.TempDir()), WithEventHooks(hooks))
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "impl"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "u"},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	want := credroute.Spend{Channel: credroute.ChannelPodEnv, Fingerprint: secrets.FingerprintSHA256("sk-pod")}
	seen := map[store.EventType]int{}
	for _, e := range em.snapshot() {
		if e.Type != store.EventLLMStepFinished && e.Type != store.EventDelegateFinished {
			continue
		}
		seen[e.Type]++
		got, ok := credroute.SpendFromEvent(e.Data)
		if !ok || got.Channel != want.Channel || got.Slot != "" || got.Fingerprint != want.Fingerprint {
			t.Errorf("%s carries the credential keys %+v (present %v), want the arm's Spend %+v", e.Type, got, ok, want)
		}
	}
	if seen[store.EventLLMStepFinished] == 0 || seen[store.EventDelegateFinished] == 0 {
		t.Fatalf("inert bench: the call left %v — no step or no delegate event to judge", seen)
	}
}

// The loop builders' bridge: GenerationOptions.Credential reaches every step
// info applyHooks converts, and the step event writes it as its credential
// keys; a step whose loop resolved none writes no key at all.
func TestApplyHooks_carriesTheCredentialOntoEveryStep(t *testing.T) {
	spend := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "anthropic", Fingerprint: "fp-run"}
	var got []LLMStepInfo
	h := EventHooks{OnLLMStepFinish: func(_ string, step LLMStepInfo) { got = append(got, step) }}
	opts := GenerationOptions{Provider: "anthropic", Credential: spend}
	applyHooks("impl", 0, h, &opts)
	opts.OnStepFinish(StepResult{Number: 1})
	if len(got) != 1 || got[0].Credential != spend {
		t.Fatalf("steps = %+v, want one carrying %+v — the bridge must carry the credential", got, spend)
	}

	em := &lockedEmitter{}
	sh := NewStoreEventHooks(context.Background(), em, "run-keys", iterlog.Nop(), nil, nil)
	sh.OnLLMStepFinish("impl", got[0])
	sh.OnLLMStepFinish("impl", LLMStepInfo{Number: 2})
	events := em.snapshot()
	if s, ok := credroute.SpendFromEvent(events[0].Data); !ok || s.Channel != spend.Channel || s.Slot != spend.Slot || s.Fingerprint != spend.Fingerprint {
		t.Errorf("the step event carries %+v (present %v), want %+v", s, ok, spend)
	}
	for _, key := range []string{credroute.EventKeyChannel, credroute.EventKeySlot, credroute.EventKeyFingerprint} {
		if _, present := events[1].Data[key]; present {
			t.Errorf("a step with no resolved credential wrote %q — the absence is the reader's cue to derive", key)
		}
	}
}

// A sandboxed call's steps are relayed by the container, which never holds
// the run's slots: the launcher writes their keys from the arm's resolution
// of the crossing (withStepCredential, the multiplexer handler's relay), and
// the container's envelope carries none of its own.
func TestARelayedStepCarriesTheLaunchersSpend(t *testing.T) {
	spend := credroute.Spend{Channel: credroute.ChannelRunForfait, Slot: "claude_code", Fingerprint: "fp-forfait"}

	var sent delegate.Envelope
	container := SandboxRelayHooks(func(env delegate.Envelope) error { sent = env; return nil }, nil)
	container.OnLLMStepFinish("container-node", LLMStepInfo{Number: 1, InputTokens: 3, Credential: credroute.Spend{Channel: credroute.ChannelPodEnv}})
	var wire delegate.EventData
	if err := json.Unmarshal(sent.Data, &wire); err != nil {
		t.Fatalf("decode EventData: %v", err)
	}
	if _, ok := credroute.SpendFromEvent(wire.Payload); ok {
		t.Fatalf("the container's envelope carries credential keys %v — the container names no slot of the run's", wire.Payload)
	}

	host := &lockedEmitter{}
	b := NewClawBackend(NewRegistry(), NewStoreEventHooks(context.Background(), host, "run-relay", iterlog.Nop(), nil, nil), RetryPolicy{})
	handler, _ := b.multiplexerHandler(withCrossingSpend(context.Background(), spend), delegate.Task{NodeID: "impl", Iteration: 1}, sessionSeed{})
	handler.OnEvent(wire.Type, wire.Payload)
	events := host.snapshot()
	if len(events) != 1 {
		t.Fatalf("re-fired events = %d, want 1", len(events))
	}
	if got, ok := credroute.SpendFromEvent(events[0].Data); !ok || got.Channel != spend.Channel || got.Slot != spend.Slot || got.Fingerprint != spend.Fingerprint {
		t.Errorf("the relayed step carries %+v (present %v), want the launcher's Spend %+v", got, ok, spend)
	}
}

// The delegate events and their folded slices carry each route's own Spend:
// the result's, carried through the executor's DelegateInfo and the chain's
// abandoned attempts.
func TestDelegateEventsAndFoldedSlicesCarryTheirCredentials(t *testing.T) {
	served := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "zai", Fingerprint: "fp-zai"}
	abandoned := credroute.Spend{Channel: credroute.ChannelRunForfait, Slot: "claude_code", Fingerprint: "fp-forfait"}

	var spent chainSpend
	task := delegate.Task{Model: "anthropic/claude-sonnet-4"}
	spent.add(delegate.Result{Tokens: 10, CostUSD: 0.1, Credential: abandoned}, attemptLabel(delegate.BackendClaw, &task, delegate.Result{Credential: abandoned}))
	info := delegateInfoFromResult(delegate.BackendClaw, delegate.Result{Tokens: 20, Credential: served})
	info.FoldedSpend = spent.slices
	data := map[string]any{}
	putDelegateModelFields(data, info)

	if got, ok := credroute.SpendFromEvent(data); !ok || got.Slot != served.Slot || got.Fingerprint != served.Fingerprint {
		t.Errorf("the delegate event carries %+v (present %v), want the served route's %+v", got, ok, served)
	}
	folded, _ := data["folded_spend"].([]any)
	if len(folded) != 1 {
		t.Fatalf("folded_spend = %v, want the abandoned attempt's slice", data["folded_spend"])
	}
	entry, _ := folded[0].(map[string]any)
	if got, ok := credroute.SpendFromEvent(entry); !ok || got.Slot != abandoned.Slot || got.Fingerprint != abandoned.Fingerprint {
		t.Errorf("the folded slice carries %+v (present %v), want the abandoned attempt's %+v", got, ok, abandoned)
	}
}

// Every in-process claw call beside a node's own dispatch — a subagent the
// node spawns, a human node's LLM half, the review companion — resolves
// through the claw arm (resolveBeside) and stamps its Spend on its step
// events, so the ledger and the fine table book the credential the call
// spent, never a derivation. Here the pod's key, its account named. Mutant —
// a site's GenerationOptions without the arm's Spend: its steps carry no
// credential keys.
func TestACallBesideTheDispatchCarriesTheArmsSpend(t *testing.T) {
	want := credroute.Spend{Channel: credroute.ChannelPodEnv, Fingerprint: secrets.FingerprintSHA256("sk-pod")}
	verdict := map[string]*ir.Schema{"verdict": {Name: "verdict", Fields: []*ir.SchemaField{{Name: "decision", Type: ir.FieldTypeString}}}}
	for _, tc := range []struct {
		name string
		// node is the step events' node id.
		node string
		run  func(t *testing.T, reg *Registry, hooks EventHooks)
	}{
		{"a subagent the node spawns", "subagent:", func(t *testing.T, reg *Registry, hooks EventHooks) {
			input := `{"description":"summarise","prompt":"Summarise Go interfaces","subagent_type":"general-purpose","model":"anthropic/claude-haiku-4-5"}`
			// One mock serves both conversations, its scripts consumed in
			// call order.
			mock := newMockClient(
				toolUseEvents("tu_1", "agent", input, 100, 30),
				textEvents("the child's own answer", 200, 60),
				textEvents("the node's answer", 10, 5),
			)
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			runner := NewSubagentRunner(reg, tool.NewRegistry(), hooks, nil, "")
			if _, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1}).Execute(context.Background(), delegate.Task{
				NodeID: "delegator", Model: "anthropic/claude-sonnet-4-6", UserPrompt: "Delegate the summary.",
				HasTools: true, ToolDefs: []delegate.ToolDef{agentToolDefFor(t, runner)}, ToolMaxSteps: 5,
			}); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
		}},
		{"a human node's LLM half", "gate", func(t *testing.T, reg *Registry, hooks EventHooks) {
			reg.Register("anthropic", func(string) (api.APIClient, error) {
				return newMockClient(textEvents(`{"decision":"approve"}`, 40, 8)), nil
			})
			node := &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLMOrHuman},
				Model:             "anthropic/claude-sonnet-4-6",
			}
			node.OutputSchema = "verdict"
			wf := &ir.Workflow{Name: "gate", Nodes: map[string]ir.Node{"gate": node}, Schemas: verdict, Prompts: map[string]*ir.Prompt{}}
			e := NewClawExecutor(reg, wf, WithEventHooks(hooks))
			_, _ = e.executeHumanLLM(context.Background(), node, map[string]any{"ctx": "the change"}, nil)
		}},
		{"the review companion", "rev", func(t *testing.T, reg *Registry, hooks EventHooks) {
			reg.Register("anthropic", func(string) (api.APIClient, error) {
				return newMockClient(textEvents(`{"decision":"approve","needs_human_input":false,"message":"done"}`, 40, 8)), nil
			})
			node := &ir.HumanNode{Model: "anthropic/claude-haiku-4-5"}
			node.ID = "rev"
			node.OutputSchema = "verdict"
			wf := &ir.Workflow{Name: "rev", Nodes: map[string]ir.Node{"rev": node}, Schemas: verdict, Prompts: map[string]*ir.Prompt{}}
			e := NewClawExecutor(reg, wf, WithEventHooks(hooks))
			_, _ = e.ExecuteReviewCompanion(context.Background(), node, "system", "user")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankAmbientEnv(t)
			t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
			em := &lockedEmitter{}
			tc.run(t, NewRegistry(), NewStoreEventHooks(context.Background(), em, "run-beside", iterlog.Nop(), nil, nil))
			steps := 0
			for _, e := range em.snapshot() {
				if e.Type != store.EventLLMStepFinished || !strings.HasPrefix(e.NodeID, tc.node) {
					continue
				}
				steps++
				got, ok := credroute.SpendFromEvent(e.Data)
				if !ok || got.Channel != want.Channel || got.Slot != "" || got.Fingerprint != want.Fingerprint {
					t.Errorf("%s's step carries the credential keys %+v (present %v), want the arm's Spend %+v", e.NodeID, got, ok, want)
				}
			}
			if steps == 0 {
				t.Fatalf("inert bench: no step event of %q to judge (%d events)", tc.node, len(em.snapshot()))
			}
		})
	}
}
