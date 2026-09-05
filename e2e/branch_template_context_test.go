package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// promptRecorder is a delegate backend that answers every LLM node with a
// fixed schema-valid output and keeps the prompt the executor RESOLVED for
// it. The executor renders the prompt, so what lands here is the real
// rendering — a literal `{{run.id}}` in the recording is a literal the
// model would have been asked to read.
//
// It also records the ctx run ID each dispatch carried, which is the other
// half of the contract: see assertBranchDispatchHasNoRunID.
//
// outputs overrides the canned `{"text":"ok"}` for a node whose schema needs
// something specific — an llm router will not accept anything but one of its
// own candidates.
type promptRecorder struct {
	mu      sync.Mutex
	prompts map[string][]string
	ctxRuns map[string][]string
	outputs map[string]map[string]any
	asks    map[string]bool
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{
		prompts: make(map[string][]string),
		ctxRuns: make(map[string][]string),
		outputs: make(map[string]map[string]any),
		asks:    make(map[string]bool),
	}
}

func (p *promptRecorder) answer(nodeID string, out map[string]any) *promptRecorder {
	p.outputs[nodeID] = out
	return p
}

// asksOnFirstCall makes a node request an interaction the first time it is
// dispatched. A non-empty Backend marks it as a DELEGATE pause, which is what
// routes the answer back through reInvokeBackend instead of treating it as
// the node's output.
func (p *promptRecorder) asksOnFirstCall(nodeID string) *promptRecorder {
	p.asks[nodeID] = true
	return p
}

func (p *promptRecorder) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	p.mu.Lock()
	p.prompts[task.NodeID] = append(p.prompts[task.NodeID], task.UserPrompt)
	p.ctxRuns[task.NodeID] = append(p.ctxRuns[task.NodeID], model.RunIDFromContext(ctx))
	first := len(p.prompts[task.NodeID]) == 1
	ask := p.asks[task.NodeID]
	out, ok := p.outputs[task.NodeID]
	p.mu.Unlock()
	if ask && first {
		return delegate.Result{}, &model.ErrNeedsInteraction{
			NodeID:    task.NodeID,
			Questions: map[string]any{delegate.AskUserQuestionKey: "proceed?"},
			SessionID: "probe-session",
			Backend:   "stub",
		}
	}
	if !ok {
		out = map[string]any{"text": "ok"}
	}
	return delegate.Result{
		BackendName: delegate.BackendClaudeCode,
		Output:      out,
	}, nil
}

func (p *promptRecorder) rendered(t *testing.T, nodeID string, want int) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	got := append([]string(nil), p.prompts[nodeID]...)
	if len(got) != want {
		t.Fatalf("node %q rendered %d prompts, want %d: %q", nodeID, len(got), want, got)
	}
	return got
}

func (p *promptRecorder) ctxRunIDs(nodeID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ctxRuns[nodeID]...)
}

// systemRecorder is the model-registry twin of promptRecorder. A human node
// in `llm` / `llm_or_human` mode does NOT go through a delegate backend —
// executeHumanLLM calls GenerateObjectDirect against an api.APIClient — so
// covering that dispatch path needs a client, not a backend. It answers the
// structured call with a fixed object and keeps every system prompt it was
// sent.
type systemRecorder struct {
	mu      sync.Mutex
	systems []string
	object  string // JSON for the synthetic structured-output tool call
}

func (s *systemRecorder) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	s.mu.Lock()
	s.systems = append(s.systems, req.System)
	s.mu.Unlock()

	name := "structured_output"
	if len(req.Tools) > 0 {
		name = req.Tools[0].Name
	}
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 1},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "probe", Name: name}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: s.object}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 1}},
		{Type: api.EventMessageStop},
	}
	ch := make(chan api.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (s *systemRecorder) rendered(t *testing.T, want int) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	got := append([]string(nil), s.systems...)
	if len(got) != want {
		t.Fatalf("systemRecorder saw %d calls, want %d: %q", len(got), want, got)
	}
	return got
}

// nodeOutputsFor returns every output a node published on a node_finished
// event, in order. A fan-out node publishes one per branch.
func nodeOutputsFor(events []*store.Event, nodeID string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e.Type != store.EventNodeFinished || e.NodeID != nodeID {
			continue
		}
		if o, ok := e.Data["output"].(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// TestTemplateContextReachesFanOutBranches is the #763 regression: the
// identity + template snapshot the executor renders prompts and tool
// commands from was attached on the TRUNK dispatch path only, so the same
// probe rendered a run id on the trunk and a silent empty constant one edge
// later, inside a fan-out branch.
//
// The fixture runs ONE probe body through all five call sites of
// executor.Execute in a single run — the trunk main loop (engine_exec), the
// branch dispatch (branch, exercised by both fan-out kinds: `fan_out_each`
// once per item and a `fan_out_all` branch), the llm router (routing), and
// the two resume-file sites, `execAutoOrPauseHuman` and `reInvokeBackend`.
// Each must render exactly what the trunk rendered, modulo the live elapsed
// reading; the re-invocation is checked by containment because the engine
// prepends a prior-interaction block to that one.
//
// The per-item agent additionally reads `{{outputs.spread.it}}`, which
// exists only in the branch's own outputs view, so it also pins whose view
// the snapshot was taken from.
func TestTemplateContextReachesFanOutBranches(t *testing.T) {
	wf := compileFixture(t, "branch_template_context_mini.bot")
	s := tmpStore(t)
	const runID = "e2e-branch-template-ctx"

	recorder := newPromptRecorder()
	// The llm router validates its own output against a candidate enum, so
	// its answer cannot be the canned {"text":"ok"}.
	recorder.answer("route_probe", map[string]any{"selected_route": "gate_probe", "reasoning": "probe"})
	recorder.asksOnFirstCall("reinvoke_probe")
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaudeCode, recorder)

	// The two direct-generation clients. gate_probe is `llm_or_human`: its
	// schema is the node's own, wrapped with needs_human_input, and
	// answering false is what keeps the run walking forward instead of
	// parking on a human. The interaction client answers reinvoke_probe's
	// question under the synthetic schema built from the question keys.
	// Separate model specs so each recorder counts only its own calls.
	gate := &systemRecorder{object: `{"text":"ok","needs_human_input":false}`}
	interaction := &systemRecorder{object: `{"` + delegate.AskUserQuestionKey + `":"yes"}`}
	models := model.NewRegistry()
	models.Register("stub", func(modelID string) (api.APIClient, error) {
		switch modelID {
		case "gate":
			return gate, nil
		case "interaction":
			return interaction, nil
		}
		return nil, fmt.Errorf("unexpected stub model %q", modelID)
	})

	exec := model.NewClawExecutor(models, wf,
		model.WithBackendRegistry(backends),
		model.WithWorkDir(t.TempDir()),
	)

	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished", r.Status, r.Error)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// --- tool `command:` — the silent-constant half -----------------------
	//
	// A `{{run.id}}` a tool node cannot resolve does not survive as visible
	// braces: resolveRunRefs substitutes it with the empty string, so the
	// command runs with an argument nobody notices is missing.
	for _, probe := range []struct {
		node  string
		calls int
	}{
		{"trunk_probe", 1}, // the reference: the trunk always had the snapshot
		{"each_probe", 2},  // fan_out_each body, once per item
		{"all_probe", 1},   // fan_out_all branch
	} {
		outs := nodeOutputsFor(events, probe.node)
		if len(outs) != probe.calls {
			t.Fatalf("tool %q produced %d outputs, want %d", probe.node, len(outs), probe.calls)
		}
		for i, out := range outs {
			if got := out["run_id"]; got != runID {
				t.Errorf("tool %q call %d: command rendered run.id = %q, want %q", probe.node, i, got, runID)
			}
			assertElapsed(t, probe.node, out["elapsed"])
		}
	}

	// --- agent `user:` prompt — the literal-braces half -------------------
	trunk := recorder.rendered(t, "trunk_agent", 1)[0]
	assertPromptResolved(t, "trunk_agent", trunk, runID)
	if !strings.Contains(trunk, "seed=SEEDED") {
		t.Errorf("trunk_agent prompt did not resolve {{outputs.seed.value}}: %q", trunk)
	}

	// trunk_agent and all_agent share ONE prompt body, so everything but
	// the live elapsed reading must render identically across the trunk /
	// fan_out_all boundary.
	branch := recorder.rendered(t, "all_agent", 1)[0]
	assertPromptResolved(t, "all_agent", branch, runID)
	if !strings.Contains(branch, "seed=SEEDED") {
		t.Errorf("all_agent prompt did not resolve {{outputs.seed.value}} inside a fan_out_all branch: %q", branch)
	}
	if dropElapsed(trunk) != dropElapsed(branch) {
		t.Errorf("one prompt body rendered two ways:\n  trunk:  %q\n  branch: %q", trunk, branch)
	}

	// The per-item ref: `{{outputs.spread.it}}` is stamped into the BRANCH's
	// outputs view by the fan_out_each router, never into the trunk's. It
	// resolves only if the snapshot was built from the branch scope — the
	// same scope the expr path already resolved `outputs.*` against.
	items := map[string]bool{}
	for _, p := range recorder.rendered(t, "each_agent", 2) {
		assertPromptResolved(t, "each_agent", p, runID)
		if !strings.Contains(p, "seed=SEEDED") {
			t.Errorf("each_agent prompt did not resolve {{outputs.seed.value}}: %q", p)
		}
		switch {
		case strings.Contains(p, "item=alpha"):
			items["alpha"] = true
		case strings.Contains(p, "item=beta"):
			items["beta"] = true
		default:
			t.Errorf("each_agent prompt resolved no per-item {{outputs.spread.it}}: %q", p)
		}
	}
	if len(items) != 2 {
		t.Errorf("the two fan_out_each bodies rendered %v, want one prompt per item", items)
	}

	// --- the three hand-wired dispatch sites -----------------------------
	//
	// The llm router (routing.go) and the two resume.go sites build their
	// context beside their own iteration wiring rather than through the main
	// loop's, so nothing else in the suite would notice if a refactor
	// dropped the snapshot from one of them. All three carry probe_prompt.
	router := recorder.rendered(t, "route_probe", 1)[0]
	assertPromptResolved(t, "route_probe", router, runID)
	if dropElapsed(trunk) != dropElapsed(router) {
		t.Errorf("the llm router rendered the shared body differently:\n  trunk:  %q\n  router: %q", trunk, router)
	}

	gated := gate.rendered(t, 1)[0]
	assertPromptResolved(t, "gate_probe", gated, runID)
	if dropElapsed(trunk) != dropElapsed(gated) {
		t.Errorf("the llm_or_human gate rendered the shared body differently:\n  trunk: %q\n  gate:  %q", trunk, gated)
	}

	// reInvokeBackend: the node asked once, the interaction model answered,
	// and the engine re-dispatched it. Both renderings must resolve — the
	// re-invocation builds its own context and could silently lose the
	// snapshot while the first dispatch stayed green. The engine prepends a
	// prior-interaction block to the second user prompt, so the assertion is
	// containment, not equality.
	interaction.rendered(t, 1)
	redispatch := recorder.rendered(t, "reinvoke_probe", 2)
	for i, p := range redispatch {
		assertPromptResolved(t, "reinvoke_probe", p, runID)
		if !strings.Contains(p, "seed=SEEDED") {
			t.Errorf("reinvoke_probe dispatch %d did not resolve {{outputs.seed.value}}: %q", i, p)
		}
	}

	assertBranchDispatchHasNoRunID(t, recorder, runID)
}

// assertBranchDispatchHasNoRunID pins the other half of the contract: the
// branch dispatch carries the template snapshot but NOT the ctx run ID,
// while every trunk site carries both.
//
// The two are not interchangeable. A run ID on the executor's context is a
// capability switch — it arms the per-node claw session store, the operator
// inbox drain and the async-ask binder, all keyed `(runID, nodeID)` with no
// branch discriminator. A `fan_out_each` runs N branches concurrently under
// ONE node id, so a run ID there makes item N's generation inherit item M's
// stored messages and lets one arbitrary item swallow a steering message.
// The `run=<id>` assertions above prove the snapshot is enough on its own,
// so nothing is lost by keeping the ID on the trunk.
//
// Both directions are load-bearing, because execContext is now defined as
// execContextBranch plus the run ID: dropping the branch rows would let
// someone "simplify" the branch path back onto execContext, and dropping the
// trunk rows would let the composition be inverted — silently disabling
// session compaction and operator steering for the whole run.
func assertBranchDispatchHasNoRunID(t *testing.T, recorder *promptRecorder, runID string) {
	t.Helper()
	for _, tc := range []struct {
		node string
		want string
	}{
		{"trunk_agent", runID},    // trunk main loop (engine_exec)
		{"route_probe", runID},    // trunk llm router (routing)
		{"reinvoke_probe", runID}, // trunk re-dispatch (resume)
		{"all_agent", ""},         // fan_out_all branch (branch)
		{"each_agent", ""},        // fan_out_each body, same node id per item
	} {
		got := recorder.ctxRunIDs(tc.node)
		if len(got) == 0 {
			t.Fatalf("node %q never dispatched, cannot check its ctx run id", tc.node)
		}
		for i, id := range got {
			if id != tc.want {
				t.Errorf("node %q dispatch %d carried ctx run id %q, want %q", tc.node, i, id, tc.want)
			}
		}
	}
}

// dropElapsed blanks the one field that legitimately differs between two
// renderings of the same body: `run.elapsed_seconds` is read when the node
// starts, so two nodes never see the same number.
func dropElapsed(prompt string) string {
	head, rest, ok := strings.Cut(prompt, "elapsed=")
	if !ok {
		return prompt
	}
	_, tail, _ := strings.Cut(rest, " ")
	return head + "elapsed=<n> " + tail
}

func assertPromptResolved(t *testing.T, nodeID, prompt, runID string) {
	t.Helper()
	if strings.Contains(prompt, "{{") {
		t.Errorf("%s prompt kept an unresolved reference: %q", nodeID, prompt)
	}
	if !strings.Contains(prompt, "run="+runID) {
		t.Errorf("%s prompt did not resolve {{run.id}}: %q", nodeID, prompt)
	}
	if _, _, ok := strings.Cut(prompt, "elapsed="); !ok {
		t.Fatalf("%s prompt lost the elapsed field entirely: %q", nodeID, prompt)
	}
	_, rest, _ := strings.Cut(prompt, "elapsed=")
	field, _, _ := strings.Cut(rest, " ")
	assertElapsed(t, nodeID, field)
}

// assertElapsed accepts any non-negative number: `run.elapsed_seconds` is a
// live measurement, so the assertion is that it was MEASURED, not that it
// equals a value.
func assertElapsed(t *testing.T, nodeID string, v any) {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Errorf("%s: run.elapsed_seconds rendered as %T (%v), want a string", nodeID, v, v)
		return
	}
	if s == "" {
		t.Errorf("%s: run.elapsed_seconds rendered empty — the template snapshot never reached this dispatch path", nodeID)
		return
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Errorf("%s: run.elapsed_seconds = %q, want a number", nodeID, s)
		return
	}
	if f < 0 {
		t.Errorf("%s: run.elapsed_seconds = %v, want >= 0", nodeID, f)
	}
}
