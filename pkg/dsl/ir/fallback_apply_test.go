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

	if refusals := ApplyRunFallback(w, []Fallback{runRoute()}, false); len(refusals) != 0 {
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

	ApplyRunFallback(w, []Fallback{runRoute()}, false)
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

	ApplyRunFallback(w, []Fallback{runRoute()}, false)
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

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "codex", Model: "gpt-5.4"}}, false)
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

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "codex", Model: "gpt-5.4"}}, false)
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

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claude_code", Model: "claude-opus-5"}}, false)
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

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "claw"}}, false)
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
	ApplyRunFallback(w, []Fallback{runRoute()}, false)

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
	if refusals := ApplyRunFallback(w, nil, false); refusals != nil {
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

	if refusals := ApplyRunFallback(w, routes, false); len(refusals) != 0 {
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

	refusals := ApplyRunFallback(w, routes, false)
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

// A codex stage is only takeable where the run is UNSANDBOXED: the codex
// CLI hard-errors on any non-noop sandbox driver at dispatch, so taking
// the stage under a sandbox would fail exactly when the chain is needed.
//
// Whether the run is sandboxed is the caller's answer (the engine's own
// runtime.WorkflowSandboxActive), never re-derived here — so the screen
// is a plain bool and the sandboxed/unsandboxed pair is all there is.
func TestApplyRunFallback_codexRefusedWhenSandboxed(t *testing.T) {
	route := []Fallback{{Backend: "codex", Model: "gpt-5.4"}}

	mk := func() *Workflow {
		agent := &AgentNode{BaseNode: BaseNode{ID: "a"}}
		return &Workflow{Nodes: map[string]Node{"a": agent}}
	}

	w := mk()
	if refusals := ApplyRunFallback(w, route, true); len(refusals) != 1 {
		t.Fatalf("sandboxed run: refusals = %v, want the codex stage refused", refusals)
	} else if len(w.Nodes["a"].(*AgentNode).Fallbacks) != 0 {
		t.Fatal("the refused stage must not be materialised")
	}

	// Unsandboxed: dispatch would allow it, so the screen must too.
	w = mk()
	if refusals := ApplyRunFallback(w, route, false); len(refusals) != 0 {
		t.Fatalf("unsandboxed run: refusals = %v, want the stage taken", refusals)
	} else if len(w.Nodes["a"].(*AgentNode).Fallbacks) != 1 {
		t.Fatal("an unsandboxed run must carry the codex stage")
	}

	// A non-codex stage is untouched by the sandbox screen.
	w = mk()
	if refusals := ApplyRunFallback(w, []Fallback{{Backend: "claw", Model: "openai/gpt-5.6"}}, true); len(refusals) != 0 {
		t.Fatalf("claw stage: refusals = %v, want none — claw runs in the sandbox", refusals)
	}
}

// The screen matches the backend a stage will DISPATCH on, not the string
// the operator typed. Dispatch expands `${...}` and reads an empty
// Backend as "inherit the node's", so a literal comparison waves through
// exactly the two wire shapes the HTTP/studio surface can express
// (runview.FallbackEntry → toRunFallback never passes ParseRunFallbackFlag).
// The env rows are the ones that fail if someone "fixes" this with a
// substring match on "codex".
func TestApplyRunFallback_codexScreenedOnTheEffectiveBackend(t *testing.T) {
	cases := []struct {
		name        string
		nodeBackend string
		wfDefault   string
		route       Fallback
		wantRefused bool
		// wantOrigin, when set, is a substring the refusal must carry —
		// the refusal has to name WHERE the backend resolved from, or it
		// sends the operator to a line that does not exist.
		wantOrigin string
	}{{
		name:        "inherited backend on a codex node",
		nodeBackend: "codex",
		route:       Fallback{Model: "gpt-5.4"},
		wantRefused: true,
		wantOrigin:  "inherited from the node's backend",
	}, {
		name:        "env ref defaulting to codex",
		nodeBackend: "claude_code",
		route:       Fallback{Backend: "${FALLBACK_BACKEND:-codex}", Model: "gpt-5.4"},
		wantRefused: true,
		wantOrigin:  "via ${FALLBACK_BACKEND:-codex}",
	}, {
		name:        "inherited from the workflow default",
		nodeBackend: "",
		wfDefault:   "codex",
		route:       Fallback{Model: "gpt-5.4"},
		wantRefused: true,
		wantOrigin:  "inherited from the workflow's default_backend",
	}, {
		// An UNSET `${VAR}` with no `:-` default expands to "", which
		// resolveChain reads as "inherit the node's resolved backend" —
		// the same route as an absent field, so it must be screened the
		// same way. Judging emptiness before expansion waved this one
		// through onto a codex node the dispatch guard then hard-errors on.
		name:        "env ref with the var unset inherits the node's codex",
		nodeBackend: "codex",
		route:       Fallback{Backend: "${FALLBACK_BACKEND}", Model: "gpt-5.4"},
		wantRefused: true,
		wantOrigin:  "${FALLBACK_BACKEND} is unset, inherited from the node's backend",
	}, {
		// Controls against over-refusal: an inherited or env-resolved
		// backend that is NOT codex keeps the operator's route.
		name:        "inherited backend on a claude_code node",
		nodeBackend: "claude_code",
		route:       Fallback{Model: "claude-opus-5"},
		wantRefused: false,
	}, {
		name:        "env ref defaulting to claw",
		nodeBackend: "claude_code",
		route:       Fallback{Backend: "${FALLBACK_BACKEND:-claw}", Model: "openai/gpt-5.6"},
		wantRefused: false,
	}, {
		name:        "env ref with the var unset on a claude_code node",
		nodeBackend: "claude_code",
		route:       Fallback{Backend: "${FALLBACK_BACKEND}", Model: "claude-opus-5"},
		wantRefused: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Hermetic: every row above reasons about FALLBACK_BACKEND
			// being unset, which an ambient export would silently invert.
			t.Setenv("FALLBACK_BACKEND", "")
			agent := &AgentNode{BaseNode: BaseNode{ID: "a"}}
			agent.Backend = tc.nodeBackend
			w := &Workflow{Nodes: map[string]Node{"a": agent}, DefaultBackend: tc.wfDefault}

			refusals := ApplyRunFallback(w, []Fallback{tc.route}, true)
			refused := len(refusals) == 1
			if refused != tc.wantRefused {
				t.Fatalf("refused = %v (%v), want %v", refused, refusals, tc.wantRefused)
			}
			if tc.wantOrigin != "" && !strings.Contains(refusals[0], tc.wantOrigin) {
				t.Errorf("refusal %q does not name where the backend resolved from (want %q)",
					refusals[0], tc.wantOrigin)
			}
			if want := 0; tc.wantRefused && len(agent.Fallbacks) != want {
				t.Fatalf("materialised %d stages, want %d", len(agent.Fallbacks), want)
			}
			if want := 1; !tc.wantRefused && len(agent.Fallbacks) != want {
				t.Fatalf("materialised %d stages, want %d", len(agent.Fallbacks), want)
			}
		})
	}

	// The env value SET at launch wins over the `:-codex` default — the
	// screen tracks resolution, not spelling.
	t.Run("env var set away from codex", func(t *testing.T) {
		t.Setenv("FALLBACK_BACKEND", "claw")
		agent := &AgentNode{BaseNode: BaseNode{ID: "a"}}
		agent.Backend = "claude_code"
		w := &Workflow{Nodes: map[string]Node{"a": agent}}

		route := Fallback{Backend: "${FALLBACK_BACKEND:-codex}", Model: "openai/gpt-5.6"}
		if refusals := ApplyRunFallback(w, []Fallback{route}, true); len(refusals) != 0 {
			t.Fatalf("refusals = %v, want the stage taken — it resolves to claw", refusals)
		}
		if len(agent.Fallbacks) != 1 {
			t.Fatal("a claw-resolving stage must be materialised")
		}
	})
}

// A malformed route is reported as malformed, whatever the sandbox: a
// bare `--fallback codex` can never work anywhere, so "no model" is the
// actionable message, not a sandbox refusal the operator would chase.
func TestApplyRunFallback_missingModelOutranksTheSandboxScreen(t *testing.T) {
	agent := applyAgent("work", "claude_code", "", nil, nil)
	w := &Workflow{Nodes: map[string]Node{"work": agent}}

	refusals := ApplyRunFallback(w, []Fallback{{Backend: "codex"}}, true)
	if len(refusals) != 1 || !strings.Contains(refusals[0], "no model") {
		t.Fatalf("refusals = %v, want the missing-model reason", refusals)
	}
}
