package model

// Witnesses for the sub-agent reserve (CM-13, #3448): a scripted child
// admits against the node's armed reserve BEFORE anything resolves, settles
// the hold on every return path, cannot choose its model (ADR-165), and an
// unarmed node keeps the tool withheld. Mutation discipline: removing the
// admission, the release or the withhold override reddens the witness that
// asserts it, on its own assertion. The concurrency witnesses run in both
// completion orders; no fact is read from arrival order.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	clawapi "github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

func TestTheSubagentReserveAdmitsSettlesAndRefills(t *testing.T) {
	t.Parallel()
	s := &SubagentSpend{}
	applied := s.ArmReserve(&scripthost.SubagentReserve{Tokens: 100, TokensPerAgent: 60, Concurrent: 2})
	if applied.Tokens != 100 || applied.TokensPerAgent != 60 {
		t.Fatalf("arming changed the declared policy: %+v", applied)
	}
	h1, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("first admission refused: %v", err)
	}
	// 100 - 60 = 40 left: a second hold of 60 cannot pass.
	if _, err := s.ReserveSubagent(); err == nil {
		t.Fatal("the reserve admitted a hold past its own size")
	}
	// Settle the first child: the hold frees, the slot frees, and the
	// refill passes. Release is the settle half; Record books the usage.
	h1.Release()
	h1.Release() // idempotent: a settle path and a sweep may both reach it
	if _, _, lost := s.SubagentReserveState(); lost != 0 {
		t.Fatalf("a released hold counted as lost: %d", lost)
	}
	h2, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("the refill was refused after the settle: %v", err)
	}
	h2.Release()
}

// The reserve is finite across SEQUENTIAL children too: what a settled
// child actually burned depletes it, admission reads consumed + holds — a
// loop of children cannot outspend the declared reserve because each hold
// frees as it settles. Red on the first draft: admission compared only the
// holds in flight, so sequential spend was unbounded.
func TestTheReserveDepletesWithSettledUsage(t *testing.T) {
	t.Parallel()
	s := &SubagentSpend{}
	s.ArmReserve(&scripthost.SubagentReserve{Tokens: 100, TokensPerAgent: 60})
	h, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("first admission refused: %v", err)
	}
	h.Release()
	// The child settles its ACTUAL usage — 50 tokens, under the 60 booked.
	s.Record(context.Background(), "m", Usage{InputTokens: 30, OutputTokens: 20})
	// consumed(50) + holds(0) + 60 = 110 > 100: the next child is refused.
	if _, err := s.ReserveSubagent(); err == nil {
		t.Fatal("a settled child's usage did not deplete the reserve: sequential spend is unbounded")
	}
	if !strings.Contains(func() string { _, e := s.ReserveSubagent(); return e.Error() }(), "already settled consumed 50") {
		t.Fatal("the refusal does not name the settled consumption")
	}
}

func TestTwoConcurrentReservationsRaceOneSlot(t *testing.T) {
	t.Parallel()
	for _, holder := range []string{"a", "b"} {
		t.Run("holder-"+holder, func(t *testing.T) {
			s := &SubagentSpend{}
			s.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000, TokensPerAgent: 100, Concurrent: 1})
			// The holder keeps its slot OPEN while the other goroutine tries
			// to admit: the cap must refuse it, then admit again once the
			// slot frees. Each goroutine takes the holder seat in turn, so
			// the witness survives both identities — a lost update would
			// admit the second child past the cap.
			holdReady := make(chan *subagentHold)
			proceed := make(chan struct{})
			type outcome struct{ err error }
			tried := make(chan outcome, 1)
			go func() {
				h, err := s.ReserveSubagent()
				if err != nil {
					tried <- outcome{err}
					return
				}
				holdReady <- h
				<-proceed
				h.Release()
				tried <- outcome{nil}
			}()
			holderHold := <-holdReady

			other := make(chan error, 1)
			go func() { other <- func() error { _, err := s.ReserveSubagent(); return err }() }()
			if err := <-other; err == nil {
				t.Fatalf("goroutine %s admitted while the holder held the only slot", holder)
			}
			close(proceed)
			if res := <-tried; res.err != nil {
				t.Fatalf("the holder was refused: %v", res.err)
			}
			// The freed slot admits.
			h, err := s.ReserveSubagent()
			if err != nil {
				t.Fatalf("the freed slot did not refill: %v", err)
			}
			h.Release()
			_ = holderHold
		})
	}
}

func TestTheReserveSaturationRefusesTheSecondChild(t *testing.T) {
	t.Parallel()
	s := &SubagentSpend{}
	s.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000, TokensPerAgent: 100, Concurrent: 1})
	h1, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("first admission refused: %v", err)
	}
	defer h1.Release()
	_, err = s.ReserveSubagent()
	if err == nil {
		t.Fatal("a second in-flight child passed a one-slot reserve")
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("the saturation refusal does not name the concurrency bound: %v", err)
	}
}

func TestTheSweepReleasesAndNamesUnsettledChildren(t *testing.T) {
	t.Parallel()
	s := &SubagentSpend{}
	s.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000, TokensPerAgent: 100, Concurrent: 4})
	h1, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("admission refused: %v", err)
	}
	h2, err := s.ReserveSubagent()
	if err != nil {
		t.Fatalf("second admission refused: %v", err)
	}
	_ = h2
	if got := s.SweepUnsettled(); got != 2 {
		t.Fatalf("the sweep reported %d unsettled children, want 2", got)
	}
	// After the sweep the reserve is empty: a refill passes, and the lost
	// count is named.
	if _, _, lost := s.SubagentReserveState(); lost != 2 {
		t.Fatalf("the lost holds were not named: %d", lost)
	}
	if _, err := s.ReserveSubagent(); err != nil {
		t.Fatalf("a refilled admission was refused after the sweep: %v", err)
	}
	// The swept hold releases idempotently — a late settle of a child whose
	// goroutine DID come back must not drive the counters negative.
	h1.Release()
	if holds, inFlight, lost := s.SubagentReserveState(); holds != 0 || inFlight < 0 || lost != 2 {
		t.Fatalf("a late release after the sweep corrupted the counters: holds=%d inFlight=%d lost=%d", holds, inFlight, lost)
	}
}

func TestAnArmedReserveClampsToTheCeiling(t *testing.T) {
	t.Parallel()
	s := &SubagentSpend{}
	applied := s.ArmReserve(&scripthost.SubagentReserve{Tokens: 1 << 40, TokensPerAgent: 1 << 40, Concurrent: 1 << 10, StepsPerAgent: 1 << 10})
	ceil := scripthost.SubagentCeiling()
	if applied.Tokens > ceil.Tokens || applied.TokensPerAgent > ceil.TokensPerAgent || applied.Concurrent > ceil.Concurrent || applied.StepsPerAgent > ceil.StepsPerAgent {
		t.Fatalf("arming served a policy above the platform ceiling: %+v (ceiling %+v)", applied, ceil)
	}
}

// The scripted route: a reserve with no room refuses the child before the
// model resolves — typed, naming the reserve, and the provider is never
// called. Red if the admission moved after resolution (the child would cost
// a provider call before being refused).
func TestASpentReserveRefusesTheScriptedChildBeforeAnyModelCall(t *testing.T) {
	t.Parallel()
	client := &subagentPolicyClient{}
	modelReg := NewRegistry()
	modelReg.Register("anthropic", func(string) (clawapi.APIClient, error) { return client, nil })
	toolReg := tool.NewRegistry()

	run := NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, "anthropic/claude-opus-4-6")
	spend := &SubagentSpend{}
	spend.ArmReserve(&scripthost.SubagentReserve{Tokens: 1, TokensPerAgent: 1, Concurrent: 1})
	// Spend the only slot outside the runner: the runner's admission must
	// refuse before it resolves a model.
	h, err := spend.ReserveSubagent()
	if err != nil {
		t.Fatalf("pre-booking refused: %v", err)
	}
	ctx := WithSubagentScriptAdmission(context.Background(), spend)
	_, runnerErr := run(ctx, map[string]any{"description": "probe", "prompt": "go"})
	if runnerErr == nil {
		t.Fatal("a spent reserve admitted a scripted child")
	}
	if !strings.Contains(runnerErr.Error(), "subagent reserve") {
		t.Fatalf("the refusal is not the reserve's typed one: %v", runnerErr)
	}
	if calls := client.calls; calls != 0 {
		t.Fatalf("the refused child cost %d provider call(s); admission must precede resolution", calls)
	}
	h.Release()
}

// ADR-165, same sentence as the reserve: a script cannot choose a child's
// model. The refusal is typed and the provider is never called.
func TestAScriptedChildCannotChooseItsModel(t *testing.T) {
	t.Parallel()
	client := &subagentPolicyClient{}
	modelReg := NewRegistry()
	modelReg.Register("anthropic", func(string) (clawapi.APIClient, error) { return client, nil })
	run := NewSubagentRunner(modelReg, tool.NewRegistry(), EventHooks{}, nil, "anthropic/claude-opus-4-6")
	spend := &SubagentSpend{}
	spend.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000})
	ctx := WithSubagentScriptAdmission(context.Background(), spend)
	_, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "model": "openai/gpt-x"})
	if err == nil {
		t.Fatal("a scripted call chose a model")
	}
	if !strings.Contains(err.Error(), "cannot choose a child's model") {
		t.Fatalf("the refusal does not name the rule: %v", err)
	}
	if client.calls != 0 {
		t.Fatal("the refused call reached the provider")
	}
	// The DIRECT route is unchanged ON AN ARMED NODE TOO: the model's own
	// turn runs under the node's Execute context, which carries the spend
	// (and the armed policy) but never the script admission key — that key
	// lives only on the script server's nested dispatches. Red on the first
	// draft, which armed it on the whole Execute context: the direct call
	// then hit the ADR-165 refusal and booked reserve holds.
	armedNode := WithSubagentSpend(context.Background(), spend)
	_, directErr := run(armedNode, map[string]any{"description": "probe", "prompt": "go", "model": "openai/gpt-x"})
	if directErr != nil && strings.Contains(directErr.Error(), "cannot choose a child's model") {
		t.Fatalf("the direct route picked up the scripted refusal on an armed node: %v", directErr)
	}
	if holds, _, _ := spend.SubagentReserveState(); holds != 0 {
		t.Fatalf("the direct call booked a reserve hold: %d", holds)
	}
}

// The happy scripted path: admit, run, settle, release — and the freed slot
// admits the next child. The hold never enters the usage: the total is what
// Record booked, not the hold's estimate.
func TestAScriptedChildAdmitsRunsAndReleases(t *testing.T) {
	t.Parallel()
	client := &subagentPolicyClient{}
	modelReg := NewRegistry()
	modelReg.Register("anthropic", func(string) (clawapi.APIClient, error) { return client, nil })
	toolReg := tool.NewRegistry()
	if err := toolReg.RegisterBuiltin("bash", "test shell", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) { return "ran", nil }); err != nil {
		t.Fatalf("register bash: %v", err)
	}
	run := NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, "anthropic/claude-opus-4-6")
	spend := &SubagentSpend{}
	spend.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000, TokensPerAgent: 400, Concurrent: 1})
	ctx := WithSubagentSpend(context.Background(), spend)
	ctx = WithSubagentScriptAdmission(ctx, spend)
	ctx = WithParentLoop(ctx, ParentLoop{Model: "anthropic/claude-opus-4-6", Tools: nil, MaxSteps: 5})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "run the command"}); err != nil {
		t.Fatalf("the admitted child failed: %v", err)
	}
	holds, inFlight, lost := spend.SubagentReserveState()
	if holds != 0 || inFlight != 0 || lost != 0 {
		t.Fatalf("the settled child left state behind: holds=%d inFlight=%d lost=%d", holds, inFlight, lost)
	}
	usage, _ := spend.Total()
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		t.Fatal("the settled child recorded no usage — the reserve replaced the accounting instead of booking beside it")
	}
	// The freed slot admits again: two sequential children through a
	// one-slot reserve.
	if _, err := spend.ReserveSubagent(); err != nil {
		t.Fatalf("the freed slot did not refill: %v", err)
	}
}

// The door: on an armed node the tool is served, named concurrent-safe, and
// in the catalogue; on an unarmed node everything stays as before.
func TestTheDoorServesAgentOnlyOnAnArmedNode(t *testing.T) {
	t.Parallel()
	var ran bool
	_, unarmed := newToolDispatcher(context.Background(), bashTool(&ran), nil, nil, nil, nil, allowAllPolicy(t))
	unarmedSrv, err := newScriptToolServer(unarmed, ScriptHostToolName)
	if err != nil {
		t.Fatal(err)
	}
	if refusal, withheld := unarmedSrv.withhold("agent"); !withheld || !strings.Contains(refusal, "reserve") {
		t.Fatalf("an unarmed node's door does not withhold agent with the reserve refusal: %q %v", refusal, withheld)
	}
	if unarmedSrv.ConcurrentSafe("agent") {
		t.Fatal("agent answered concurrent-safe on an unarmed node")
	}

	spend := &SubagentSpend{}
	spend.ArmReserve(&scripthost.SubagentReserve{Tokens: 1000, Concurrent: 2})
	// The node's Execute context carries the SPEND (as production arms it);
	// the server detects the armed policy from it and arms the script
	// admission on ITS OWN nested-dispatch context.
	armed := WithSubagentSpend(unarmed, spend)
	armedSrv, err := newScriptToolServer(armed, ScriptHostToolName)
	if err != nil {
		t.Fatal(err)
	}
	if refusal, withheld := armedSrv.withhold("agent"); withheld {
		t.Fatalf("an armed node's door withheld agent: %q", refusal)
	}
	if !armedSrv.ConcurrentSafe("agent") {
		t.Fatal("agent did not answer concurrent-safe on an armed node")
	}
	// The ask family stays withheld on an armed node: the reserve arms one
	// tool's script route, not the door's other classes.
	if _, withheld := armedSrv.withhold("ask_user"); !withheld {
		t.Fatal("an armed node's door served the ask family")
	}
}

// The catalogue: an armed node's programs discover the agent tool, with the
// reserve in the description. installCodemodeOn is the exact wiring
// production passes.
func TestTheCatalogueCarriesAgentOnAnArmedNode(t *testing.T) {
	t.Parallel()
	var opts GenerationOptions
	if err := installCodemodeOn(&opts, childHost(), false, &scripthost.Limits{Memory: 1 << 30}, &scripthost.SubagentReserve{Tokens: 5000, TokensPerAgent: 1000, Concurrent: 2}); err != nil {
		t.Fatal(err)
	}
	var codemode *GenerationTool
	for i := range opts.Tools {
		if opts.Tools[i].Name == ScriptHostToolName {
			codemode = &opts.Tools[i]
		}
	}
	if codemode == nil {
		t.Fatal("the codemode tool is missing")
	}
	// The description tells the program the bounds it draws against.
	for _, want := range []string{"reserve of 5000", "books 1000", "at most 2"} {
		if !strings.Contains(codemode.Description, want) {
			t.Fatalf("the description does not name %q:\n%s", want, codemode.Description)
		}
	}
	// Disarming keeps the agent tool out of the catalogue and the bounds out
	// of the description.
	var plain GenerationOptions
	if err := installCodemodeOn(&plain, childHost(), false, &scripthost.Limits{Memory: 1 << 30}, nil); err != nil {
		t.Fatal(err)
	}
	for i := range plain.Tools {
		if plain.Tools[i].Name == ScriptHostToolName && strings.Contains(plain.Tools[i].Description, "agent:") {
			t.Fatal("an unarmed node's description offers the agent tool")
		}
	}
	catalog := scriptCatalog(plain.Tools, ScriptHostToolName, false)
	for _, tool := range catalog {
		if isAgentTool(tool.Name) {
			t.Fatal("an unarmed node's catalogue carries the agent tool")
		}
	}
	// The ARMED catalogue keeps it — scriptWithheld refuses the name
	// unconditionally, so the armed case must bypass that check, or
	// call.search/call.describe would never list a tool the door serves.
	withAgent := []GenerationTool{{Name: "agent", Description: "start a child"}, {Name: "bash"}}
	armedCatalog := scriptCatalog(withAgent, ScriptHostToolName, true)
	found := false
	for _, tool := range armedCatalog {
		if isAgentTool(tool.Name) {
			found = true
		}
	}
	if !found {
		t.Fatal("an armed node's catalogue drops the agent tool the door serves")
	}
	// The unarmed catalogue drops it from the same list.
	unarmedCatalog := scriptCatalog(withAgent, ScriptHostToolName, false)
	for _, tool := range unarmedCatalog {
		if isAgentTool(tool.Name) {
			t.Fatal("an unarmed node's catalogue carries the agent tool")
		}
	}
}

// The envelope: the reserve crosses to the runner and back, both JSON
// directions, under its own wire name beside the bounds.
func TestTheSubagentsReserveCrossesTheEnvelope(t *testing.T) {
	t.Parallel()
	want := &scripthost.SubagentReserve{Tokens: 5000, TokensPerAgent: 1000, Concurrent: 2, StepsPerAgent: 8}
	task := delegate.Task{SubagentReserve: want}
	wire, err := json.Marshal(delegate.ToIOTask(task))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"codemode_subagents"`) {
		t.Fatalf("the wire does not carry the reserve under its own name: %s", wire)
	}
	var ioTask delegate.IOTask
	if err := json.Unmarshal(wire, &ioTask); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ioTask.SubagentReserve
	if got == nil || got.Tokens != 5000 || got.TokensPerAgent != 1000 || got.Concurrent != 2 || got.StepsPerAgent != 8 {
		t.Fatalf("the reserve did not survive the crossing: %+v", got)
	}
	// Absent stays absent: a runner that predates the field reads nil.
	var bare delegate.IOTask
	if err := json.Unmarshal([]byte(`{"node_id":"n"}`), &bare); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	if bare.SubagentReserve != nil {
		t.Fatal("an absent reserve decoded non-nil")
	}
}
