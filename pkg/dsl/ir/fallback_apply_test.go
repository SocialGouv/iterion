package ir

import (
	"strings"
	"testing"
)

func applyAgent(id, backend, permission string, tools []string, fbs []Fallback) *AgentNode {
	n := &AgentNode{}
	n.ID = id
	n.Backend = backend
	n.Permission = permission
	n.Tools = tools
	n.Fallbacks = fbs
	return n
}

func applyJudge(id, backend string) *JudgeNode {
	n := &JudgeNode{}
	n.ID = id
	n.Backend = backend
	return n
}

func runRoute() Fallback {
	return Fallback{Backend: "claw", Model: "openai/gpt-5.5"}
}

// TestApplyRunFallback_ReachesEligibleAgent is the operator surface's
// whole point: a bot that declares nothing survives a forfait wall when
// the operator asks for it at launch.
func TestApplyRunFallback_ReachesEligibleAgent(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	if refusals := ApplyRunFallback(w, []Fallback{runRoute()}); len(refusals) != 0 {
		t.Fatalf("unexpected refusals: %v", refusals)
	}
	if len(agent.Fallbacks) != 1 || agent.Fallbacks[0].Name != RunFallbackName {
		t.Fatalf("route not materialised onto the node: %+v", agent.Fallbacks)
	}
	if agent.Fallbacks[0].Backend != "claw" {
		t.Errorf("route backend = %q", agent.Fallbacks[0].Backend)
	}
}

// TestApplyRunFallback_NeverReachesJudges: a judge's verdict is
// load-bearing — a weaker model still emits a well-formed verdict and
// only the finding COUNT changes, which a deterministic merge gate
// reads. A blanket launch setting must not reach one.
func TestApplyRunFallback_NeverReachesJudges(t *testing.T) {
	judge := applyJudge("gate", "claude_code")
	w := &Workflow{Nodes: map[string]Node{"gate": judge}}

	ApplyRunFallback(w, []Fallback{runRoute()})
	if len(judge.Fallbacks) != 0 {
		t.Errorf("the run-level route reached a judge: %+v", judge.Fallbacks)
	}
}

// TestApplyRunFallback_AuthoredRoutesWin: an author who wrote a chain
// vetted where it may go; appending past their last route would extend
// a deliberate stop.
func TestApplyRunFallback_AuthoredRoutesWin(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, []Fallback{
		{Name: "api", Backend: "claw", Model: "anthropic/claude-opus-5"},
	})
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	ApplyRunFallback(w, []Fallback{runRoute()})
	if len(agent.Fallbacks) != 1 {
		t.Errorf("run-level route appended past an authored chain: %+v", agent.Fallbacks)
	}
}

// TestApplyRunFallback_RefusesUngatedRoute is the security half of the
// screen: without it, `--fallback codex:…` would run a `permission: deny`
// node UNGATED on fall-through — the precise crossing the compiler
// refuses in the .bot, reached through a flag. codex is the witness: it has
// no enforcement seam at all, where grok and kimi now hold a proven deny gate.
func TestApplyRunFallback_RefusesUngatedRoute(t *testing.T) {
	agent := applyAgent("work", "claude_code", "deny", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "codex", Model: "gpt-5.4"}})
	if len(refusals) != 1 || !strings.Contains(refusals[0], "UNGATED") {
		t.Fatalf("expected an ungated-crossing refusal, got %v", refusals)
	}
	if len(agent.Fallbacks) != 0 {
		t.Error("a refused route must not be attached")
	}
}

// TestApplyRunFallback_RefusesUngatedRouteFromWorkflowGate: the
// workflow block is the DOCUMENTED place to declare the mode, and a
// node's own field means "inherit". A screen reading only the node
// field misses the common shape entirely.
func TestApplyRunFallback_RefusesUngatedRouteFromWorkflowGate(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, nil)
	w := &Workflow{
		Permission: "deny",
		Nodes:      map[string]Node{"work": agent},
	}

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "codex", Model: "gpt-5.4"}})
	if len(refusals) != 1 {
		t.Fatalf("a workflow-level gate must refuse the same crossing, got %v", refusals)
	}
}

// TestApplyRunFallback_RefusesToolsInversion: an empty tools list means
// ZERO tools on claw and the FULL native toolset on a CLI backend, so
// the crossing silently changes what the node can DO — and the node was
// already admitted as a read-only parallel branch on the claw reading.
func TestApplyRunFallback_RefusesToolsInversion(t *testing.T) {
	agent := applyAgent("work", "claw", "", nil, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}})
	if len(refusals) != 1 || !strings.Contains(refusals[0], "un-restricts") {
		t.Fatalf("expected a tools-inversion refusal, got %v", refusals)
	}
	if len(agent.Fallbacks) != 0 {
		t.Error("a refused route must not be attached")
	}
}

// TestApplyRunFallback_RefusesBackendWithoutModel: model specs are not
// portable across backends, so the route could only fail at dispatch.
func TestApplyRunFallback_RefusesBackendWithoutModel(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claw"}})
	if len(refusals) != 1 || !strings.Contains(refusals[0], "no model") {
		t.Fatalf("expected a missing-model refusal, got %v", refusals)
	}
}

// TestApplyRunFallback_VisibleToPreRunAnalyses is the reason the route
// is written into the IR at all: the sandbox bind-mount and the
// workspace-safety admission both read a node's routes, and a route
// resolved privately downstream is invisible to them.
func TestApplyRunFallback_VisibleToPreRunAnalyses(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}
	ApplyRunFallback(w, []Fallback{runRoute()})

	llm, ok := w.Nodes["work"].(LLMNode)
	if !ok {
		t.Fatal("agent is not an LLMNode")
	}
	if len(llm.GetFallbacks()) != 1 {
		t.Error("GetFallbacks — the accessor every pre-run analysis reads — does not see the route")
	}
}

// TestApplyRunFallback_NoRouteIsANoOp guards the blast radius.
func TestApplyRunFallback_NoRouteIsANoOp(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", nil, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}
	if refusals := ApplyRunFallback(w, nil); refusals != nil {
		t.Errorf("an empty route must do nothing, got %v", refusals)
	}
	if len(agent.Fallbacks) != 0 {
		t.Error("an empty route attached something")
	}
}

func TestApplyRunFallback_PreservesStageOrder(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}
	routes := []Fallback{
		{Backend: "claw", Model: "openai/gpt-5.5"},
		{Backend: "claw", Model: "anthropic/claude-opus-5"},
	}

	if refusals := ApplyRunFallback(w, routes); len(refusals) != 0 {
		t.Fatalf("unexpected refusals: %v", refusals)
	}
	if len(agent.Fallbacks) != 2 {
		t.Fatalf("fallbacks = %+v, want two stages", agent.Fallbacks)
	}
	if agent.Fallbacks[0].Model != routes[0].Model || agent.Fallbacks[1].Model != routes[1].Model {
		t.Fatalf("fallback order = %+v, want %+v", agent.Fallbacks, routes)
	}
	if !agent.Fallbacks[0].RunStageSet || agent.Fallbacks[0].RunStage != 0 || agent.Fallbacks[1].RunStage != 1 {
		t.Fatalf("fallback stage indexes = %+v, want 0 then 1", agent.Fallbacks)
	}
}

func TestApplyRunFallback_RefusedStageDoesNotStopChain(t *testing.T) {
	agent := applyAgent("work", "claude_code", "deny", []string{"read_file"}, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}
	routes := []Fallback{
		{Backend: "codex", Model: "gpt-5.4"},
		{Backend: "claw", Model: "openai/gpt-5.5"},
	}

	refusals := ApplyRunFallback(w, routes)
	if len(refusals) != 1 || !strings.Contains(refusals[0], "stage 1") || !strings.Contains(refusals[0], "UNGATED") {
		t.Fatalf("refusals = %v, want the first stage refused explicitly", refusals)
	}
	if len(agent.Fallbacks) != 1 || agent.Fallbacks[0].Model != routes[1].Model {
		t.Fatalf("fallbacks = %+v, want the accepted second stage", agent.Fallbacks)
	}
	if !agent.Fallbacks[0].RunStageSet || agent.Fallbacks[0].RunStage != 1 {
		t.Fatalf("accepted stage index = %+v, want original index 1", agent.Fallbacks[0])
	}
}

func TestParseRunFallbackFlag(t *testing.T) {
	tests := []struct {
		arg           string
		wantBackend   string
		wantModel     string
		wantErr       bool
		wantEmptyName bool
	}{
		{arg: "", wantEmptyName: true},
		{arg: "claw:openai/gpt-5.5", wantBackend: "claw", wantModel: "openai/gpt-5.5"},
		// A model id containing a colon survives the first-colon split.
		{arg: "claw:openai/gpt-5.5:preview", wantBackend: "claw", wantModel: "openai/gpt-5.5:preview"},
		{arg: "  claw : openai/gpt-5.5  ", wantBackend: "claw", wantModel: "openai/gpt-5.5"},
		{arg: ":openai/gpt-5.5", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseRunFallbackFlag(tc.arg)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.arg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.arg, err)
			continue
		}
		if got.Backend != tc.wantBackend || got.Model != tc.wantModel {
			t.Errorf("%q: got backend=%q model=%q", tc.arg, got.Backend, got.Model)
		}
	}
}
