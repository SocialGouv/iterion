package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// callSiteKinds is every ir node kind with the nodes that stand for it — each
// shape dispatch can give the kind — and the sites CallSites must enumerate
// for each. A kind is classified as calling a model (one shape or more lists
// sites) or not (every shape lists none).
var callSiteKinds = map[ir.NodeKind][]struct {
	name string
	node ir.Node
	want []SiteKind
}{
	ir.NodeAgent: {
		{"an agent node", &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4"}}, []SiteKind{SiteNode}},
		{"an agent with a fallback and an interaction model", &ir.AgentNode{
			BaseNode:          ir.BaseNode{ID: "n"},
			LLMFields:         ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4"},
			InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionModel: "anthropic/claude-haiku-4-5"},
			Fallbacks:         []ir.Fallback{{Name: "rescue", Model: "openai/gpt-5.5"}},
		}, []SiteKind{SiteNode, SiteFallback, SiteInteraction}},
	},
	ir.NodeJudge: {
		{"a judge node", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-sonnet-4"}}, []SiteKind{SiteNode}},
	},
	ir.NodeRouter: {
		{"an llm router", &ir.RouterNode{BaseNode: ir.BaseNode{ID: "n"}, RouterMode: ir.RouterLLM, LLMFields: ir.LLMFields{Model: "anthropic/claude-sonnet-4"}}, []SiteKind{SiteLLMRouter}},
		{"a fan-out router", &ir.RouterNode{BaseNode: ir.BaseNode{ID: "n"}, RouterMode: ir.RouterFanOutAll}, nil},
		{"a condition router", &ir.RouterNode{BaseNode: ir.BaseNode{ID: "n"}, RouterMode: ir.RouterCondition}, nil},
	},
	ir.NodeHuman: {
		{"an llm human", &ir.HumanNode{BaseNode: ir.BaseNode{ID: "n"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM}, Model: "anthropic/claude-sonnet-4"}, []SiteKind{SiteHumanLLM}},
		{"an llm-or-human human", &ir.HumanNode{BaseNode: ir.BaseNode{ID: "n"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLMOrHuman}, Model: "anthropic/claude-sonnet-4"}, []SiteKind{SiteHumanLLM}},
		{"a review gate", &ir.HumanNode{BaseNode: ir.BaseNode{ID: "n"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionReview}, Model: "anthropic/claude-sonnet-4"}, []SiteKind{SiteReviewCompanion}},
		{"a pausing human", &ir.HumanNode{BaseNode: ir.BaseNode{ID: "n"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}, nil},
	},
	ir.NodeTool: {
		{"a recovering tool", &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "n"}, Command: "make build", Goal: "the build succeeds", Postcondition: "test -f build.ok",
			Policy: ir.PolicyRecover, Recovery: &ir.RecoverySpec{MaxRepairAttempts: 1, MaxAgentAttempts: 1, Model: "anthropic/claude-sonnet-4"},
		}, []SiteKind{SiteAgentRecovery, SiteSelfRepair}},
		{"a plain tool", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, Command: "make build"}, nil},
	},
	ir.NodeCompute:      {{"a compute node", &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
	ir.NodeEmit:         {{"an emit node", &ir.EmitNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
	ir.NodeWait:         {{"a wait node", &ir.WaitNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
	ir.NodeAwaitAnswers: {{"an await-answers node", &ir.AwaitAnswersNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
	ir.NodeSubbot:       {{"a subbot", &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "n"}}, []SiteKind{SiteUnread}}},
	ir.NodeDone:         {{"a done node", &ir.DoneNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
	ir.NodeFail:         {{"a fail node", &ir.FailNode{BaseNode: ir.BaseNode{ID: "n"}}, nil}},
}

// Every ir node kind is classified, by the one enumeration the admission and
// the pre-flight iterate (CallSites): a kind dispatch can turn into a model
// call lists its sites, every other none. A node kind added to the IR fails
// here until it has a row — and a shape dispatch counts as model-calling
// (ir.NodeUsesLLM, the predicate dispatch's own walks read) never lists no
// site. Mutant — CallSites without its router (or human, or tool) arm: the
// llm router (the human's halves, the rungs) lists the unread site, or none.
func TestEveryNodeKindIsClassifiedAsCallingOrNot(t *testing.T) {
	kinds := 0
	for k := ir.NodeKind(0); k.String() != "unknown"; k++ {
		kinds++
		if _, ok := callSiteKinds[k]; !ok {
			t.Errorf("node kind %q has no row: say which sites, if any, CallSites lists for it", k)
		}
	}
	if kinds != len(callSiteKinds) {
		t.Errorf("the IR names %d node kinds, the table %d", kinds, len(callSiteKinds))
	}
	for kind, shapes := range callSiteKinds {
		for _, shape := range shapes {
			if shape.node.NodeKind() != kind {
				t.Fatalf("%s is a %s node, listed under %s", shape.name, shape.node.NodeKind(), kind)
			}
			wf := &ir.Workflow{Name: "kinds", Nodes: map[string]ir.Node{"n": shape.node}}
			var got []SiteKind
			for _, s := range CallSites(RouteView{Workflow: wf}, nil) {
				if s.NodeID != "n" {
					t.Errorf("%s: a site of node %q", shape.name, s.NodeID)
				}
				got = append(got, s.Kind)
			}
			slices.Sort(got)
			want := slices.Clone(shape.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: CallSites lists %v, want %v", shape.name, got, want)
			}
			if ir.NodeUsesLLM(shape.node) && len(got) == 0 {
				t.Errorf("%s: dispatch calls a model there (ir.NodeUsesLLM), CallSites lists no site", shape.name)
			}
		}
	}
}

// Each site is asked where and as whom dispatch makes its call: a node
// dispatch on the node's backend, as the node, in the run's sandbox; a
// direct generation and a supervisor on claw, in this process, as an
// auxiliary or the supervisor — never with the hint a dispatch carries.
func TestEachSiteIsAskedWhereItsCallIsMade(t *testing.T) {
	wf := &ir.Workflow{
		Name: "placed",
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4", Provider: "anthropic"}},
			"gate": &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM}, Model: "openai/gpt-5.5"},
		},
		Supervisors: []*ir.Supervisor{{Name: "watch", Model: "anthropic/claude-haiku-4-5"}},
	}
	sandbox := credroute.Sandbox{Contained: true}
	want := map[string]credroute.Call{
		"impl":             {Backend: "claw", Spec: "anthropic/claude-sonnet-4", Leg: credroute.Leg{Provider: "anthropic"}, Role: credroute.RoleNode, Sandbox: sandbox},
		"gate":             {Backend: "claw", Spec: "openai/gpt-5.5", Role: credroute.RoleAuxiliary},
		"supervisor:watch": {Backend: "claw", Spec: "anthropic/claude-haiku-4-5", Role: credroute.RoleSupervisor},
	}
	sites := CallSites(RouteView{Workflow: wf}, nil)
	if len(sites) != len(want) {
		t.Fatalf("CallSites lists %d sites, want %d: %+v", len(sites), len(want), sites)
	}
	for _, s := range sites {
		base, err := s.Base(nil)
		if err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
		if got := s.Call(0, s.LegSpec(0, base), sandbox); got != want[s.ID] {
			t.Errorf("%s is asked %+v, want %+v", s.ID, got, want[s.ID])
		}
	}
}

// A claw dispatch naming no model is asked about the detector's pick, as its
// dispatch builds the task on it; the pick the run's allowlist refuses is the
// whole site's refusal. Mutant — Base ignoring the detector: the spec stays
// empty.
func TestASiteNamingNoModelIsAskedTheDetectorsPick(t *testing.T) {
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw"}}
	wf := &ir.Workflow{Name: "bare", Nodes: map[string]ir.Node{"impl": node}}
	sites := CallSites(RouteView{Workflow: wf}, nil)
	if len(sites) != 1 {
		t.Fatalf("CallSites lists %+v, want the node's own site", sites)
	}
	base, err := sites[0].Base(func(string) (string, error) { return "openai/gpt-5.5", nil })
	if err != nil || base != "openai/gpt-5.5" {
		t.Fatalf("the bare claw site's base = %q, %v — want the detector's pick", base, err)
	}
	refusal := DetectedClawSpecUnder([]string{"anthropic"})
	t.Setenv("OPENAI_API_KEY", "sk-detect")
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY", "AZURE_OPENAI_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := sites[0].Base(refusal); err == nil {
		t.Fatal("a pick outside the run's allowlist must refuse the site, as the dispatch refuses the node")
	}
}
