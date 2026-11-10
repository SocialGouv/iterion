package runview

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §4, the runner's authoritative ladder screen: the node model it
// classifies is the one dispatch resolves — the run's `{{vars.*}}` first,
// then `${…}` — and a gateway step of the node's `provider:` chain routes it
// to the gateway as much as its `model:` does. Under team_gateway such a node
// takes no policy stage, and a model the reading cannot decide is held to
// the boundary rather than read as a bare anthropic-wire id.
func TestBuildExecutor_PolicyLadderReadsTheNodeRouteDispatchServes(t *testing.T) {
	teamGateway := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "k", GatewayHost: "gw.example"}
	for _, row := range []struct {
		name     string
		model    string
		provider string
		def      string
		launch   map[string]string
		// wantRungs is the policy rungs' models the node must carry; nil
		// means the boundary refuses every stage.
		wantRungs []string
	}{
		{name: "{{vars.model}} whose declared default is a gateway route", model: "{{vars.model}}", def: "openai_compatible/scaleway/glm-5.3"},
		{name: "{{vars.model}} whose launch value is a gateway route", model: "{{vars.model}}", def: "anthropic/claude-opus-5", launch: map[string]string{"model": "openai_compatible/scaleway/glm-5.3"}},
		{name: "{{vars.model}} no reading decides", model: "{{vars.model}}"},
		{name: "a gateway step in the provider chain over a vendor model", model: "anthropic/claude-opus-5", provider: "openai_compatible:openai_compatible/scaleway/glm-5.3"},
		{name: "{{vars.model}} deciding a vendor route keeps its rungs, resolved", model: "{{vars.model}}", def: "anthropic/claude-opus-5",
			wantRungs: []string{"openai/gpt-6-sol", "anthropic/claude-opus-5"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			agent := &ir.AgentNode{}
			agent.ID = "gw"
			agent.Backend = "claw"
			agent.Model = row.model
			agent.Provider = row.provider
			agent.Tools = []string{}
			wf := &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"gw": agent}}
			if row.def != "" {
				wf.Vars = map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: row.def}}
			}
			spec := ExecutorSpec{
				Ctx:      context.Background(),
				Store:    st,
				RunID:    "run-ladder-reading",
				Workflow: wf,
				Vars:     row.launch,
				PolicyLadder: []ir.PolicyLadderStage{
					{Harness: "claw", Credential: "openai_key", On: []string{"usage_window"}},
					{Harness: "claw", Credential: "anthropic_key", On: []string{"usage_window"}},
				},
				ContentBoundary: teamGateway,
			}
			if _, err := BuildExecutor(spec); err != nil {
				t.Fatalf("BuildExecutor: %v", err)
			}
			var rungs []string
			for _, fb := range agent.Fallbacks {
				if fb.Policy {
					rungs = append(rungs, fb.Model)
				}
			}
			if !slices.Equal(rungs, row.wantRungs) {
				t.Fatalf("policy rungs on the node = %q, want %q", rungs, row.wantRungs)
			}
			evts, err := st.LoadEvents(context.Background(), spec.RunID)
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			refusals := refusalEvents(evts)
			if row.wantRungs != nil {
				if len(refusals) != 0 {
					t.Fatalf("a vendor node's landed crossings wrote refusals: %+v", refusals)
				}
				return
			}
			if len(refusals) != 2 {
				t.Fatalf("want both stages refused on the timeline, got %d", len(refusals))
			}
			for _, r := range refusals {
				if reason, _ := r.Data["reason"].(string); !strings.Contains(reason, "team_gateway") {
					t.Fatalf("refusal reason %q does not name the boundary", reason)
				}
			}
		})
	}
}

// The screen reads the message's vars at executor build; a resume message
// carries none, and the engine restores the launch's vars into the executor
// afterwards (pushExecutorVars). A launch that set `{{vars.model}}` to the
// gateway over a vendor default is screened on the default, and the rung it
// lands must not reach dispatch: under team_gateway the executor itself
// drops every policy rung of a node its dispatch vars route to the gateway.
func TestBuildExecutor_TheBoundaryHoldsAGatewayNodeTheScreenReadAsVendor(t *testing.T) {
	for _, row := range []struct {
		name     string
		boundary *store.RunContentBoundary
		want     []string
	}{
		{"team_gateway", &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "k", GatewayHost: "gw.example"}, nil},
		{"own_keys (the control)", &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}, []string{"claw"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			agent := &ir.AgentNode{}
			agent.ID = "gw"
			agent.Backend = "claude_code"
			agent.Model = "{{vars.model}}"
			agent.Tools = []string{}
			wf := &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"gw": agent},
				Vars: map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "claude-opus-5"}}}
			exec, err := BuildExecutor(ExecutorSpec{
				Ctx:             context.Background(),
				Store:           st,
				RunID:           "run-ladder-resume",
				Workflow:        wf,
				PolicyLadder:    []ir.PolicyLadderStage{{Harness: "claw", Credential: "anthropic_key", On: []string{"usage_window"}}},
				ContentBoundary: row.boundary,
			})
			if err != nil {
				t.Fatalf("BuildExecutor: %v", err)
			}
			if !slices.ContainsFunc(agent.Fallbacks, func(fb ir.Fallback) bool { return fb.Policy }) {
				evts, _ := st.LoadEvents(context.Background(), "run-ladder-resume")
				var reasons []any
				for _, e := range refusalEvents(evts) {
					reasons = append(reasons, e.Data["reason"])
				}
				t.Fatalf("fixture: the screen landed no rung on the default's vendor reading: %+v (refusals %v)", agent.Fallbacks, reasons)
			}
			exec.SetVars(map[string]any{"model": "openai_compatible/scaleway/glm-5.3"})
			if got := exec.EffectiveFallbackBackends(agent); !slices.Equal(got, row.want) {
				t.Fatalf("dispatch fallback backends = %q, want %q", got, row.want)
			}
		})
	}
}
