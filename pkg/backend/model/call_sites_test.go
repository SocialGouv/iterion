package model

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
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

// routerTaskCapture is a backend that records the model of every task an llm
// router's dispatch hands it, and answers a route.
type routerTaskCapture struct {
	mu     sync.Mutex
	models []string
}

func (b *routerTaskCapture) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.models = append(b.models, task.Model)
	b.mu.Unlock()
	return delegate.Result{Output: map[string]any{"selected_route": "alpha", "reasoning": "routed"}}, nil
}

func (b *routerTaskCapture) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.models)
}

// An llm router's site — the call the launch's admission judges and the
// usage-cap pre-flight keys — names the model the router's dispatch sends: a
// launch override (the `router` selector, a bare `*`) over its own `model:`,
// and where it names none ITERION_DEFAULT_SUPERVISOR_MODEL or the package
// default, never the detector's pick an agent node sends. Each case runs the
// router's real dispatch (executeLLMRouterUnified) on a backend that records
// the task's model, beside the site CallSites lists under the view the
// executor hands the admission (CredentialRouteView), its base read through
// the executor's own pick. Mutants — the dispatch reading its `model:` past
// the override: the two overridden cases; the router's baseline leaving an
// unnamed router to the detector: the two unnamed cases.
func TestAnLLMRoutersSiteNamesTheModelItsDispatchSends(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	// The pod holds an OpenAI key alone, so the detector picks an OpenAI
	// id; the run's own Anthropic key passes the package default's up-front
	// funding check.
	t.Setenv("OPENAI_API_KEY", "sk-openai-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"}})
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	for _, tc := range []struct {
		name       string
		model      string
		selector   string
		envDefault string
		want       string
	}{
		{"its own model", "anthropic/claude-sonnet-4", "", "", "anthropic/claude-sonnet-4"},
		{"the router selector's override over its model", "anthropic/claude-sonnet-4", "router", "", "openai/gpt-5.5"},
		{"a bare override over its model", "anthropic/claude-sonnet-4", "*", "", "openai/gpt-5.5"},
		{"no model, the env default", "", "", "moonshot/kimi-k2", "moonshot/kimi-k2"},
		{"no model, the package default", "", "", "", defaultRouterModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", tc.envDefault)
			capture := &routerTaskCapture{}
			backends := delegate.NewRegistry()
			backends.Register(delegate.BackendClaw, capture)
			router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "rt"}, RouterMode: ir.RouterLLM, LLMFields: ir.LLMFields{Backend: "claw", Model: tc.model}}
			wf := &ir.Workflow{Name: "routed", Nodes: map[string]ir.Node{"rt": router}, Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
			var overrides ModelOverrides
			if tc.selector != "" {
				overrides.SetModel(tc.selector, "openai/gpt-5.5")
			}
			exec := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(backends), WithModelOverrides(overrides))
			if _, err := exec.executeLLMRouterUnified(ctx, router, map[string]any{"_route_candidates": []string{"alpha", "beta"}}); err != nil {
				t.Fatalf("the router's dispatch failed: %v", err)
			}
			sent := capture.sent()
			if len(sent) != 1 {
				t.Fatalf("inert bench: the dispatch handed the backend %d tasks, want one", len(sent))
			}
			if sent[0] != tc.want {
				t.Errorf("the router's dispatch sends %q, want %q", sent[0], tc.want)
			}
			var site *CallSite
			for _, s := range CallSites(exec.CredentialRouteView(wf), nil) {
				if s.ID == "rt" {
					site = &s
				}
			}
			if site == nil || site.Kind != SiteLLMRouter {
				t.Fatalf("CallSites lists no llm router site for rt: %+v", site)
			}
			pick, _ := exec.DetectedClawSpec("rt")
			if tc.model == "" && pick == sent[0] {
				t.Fatalf("inert bench: the detector picks %q, the router's own default — the case cannot tell them apart", pick)
			}
			base, err := site.Base(exec.DetectedClawSpec)
			if err != nil || site.LegSpec(0, base) != sent[0] {
				t.Errorf("the router's site names %q (%v); its dispatch sends %q — the readers judge a call it never makes", site.LegSpec(0, base), err, sent[0])
			}
		})
	}
}
