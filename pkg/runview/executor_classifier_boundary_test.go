package runview

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §4: the tool-policy LLM classifier reads every tool call's
// arguments — a gateway node's included — so under team_gateway a VENDOR
// classifier spec is refused at executor build, whatever key the run holds.
// A gateway-only run already met that refusal (its vendor spec found no
// credential and the ambient refusal); a MIXED run's bundle holds the team's
// vendor key, so the classifier resolved on it and sent the gateway nodes'
// tool arguments to the vendor. A gateway classifier spec stays admitted —
// the run's sealed pair serves it — and an own_keys run keeps its vendor
// classifier.
func TestBuildExecutor_TheContentBoundaryRefusesAVendorClassifier(t *testing.T) {
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	gw := secrets.ProviderOpenAICompatible
	mixed := secrets.Credentials{
		APIKeys:        map[secrets.Provider]string{gw: "sk-gw-team", secrets.ProviderAnthropic: "sk-ant-team"},
		APIBaseURLs:    map[secrets.Provider]string{gw: "https://gw.example"},
		AmbientRefused: true,
	}
	teamGateway := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "k", GatewayHost: "gw.example"}
	for _, row := range []struct {
		name     string
		spec     string
		boundary *store.RunContentBoundary
		refused  bool
	}{
		{"a vendor classifier on a mixed team_gateway run", "anthropic/claude-haiku-4-5", teamGateway, true},
		{"a gateway classifier on the same run (admitted)", "openai_compatible/haiku", teamGateway, false},
		{"a vendor classifier on an own_keys run (the control)", "anthropic/claude-haiku-4-5", &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("ITERION_LLM_CLASSIFIER_MODEL", row.spec)
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			agent := &ir.AgentNode{}
			agent.ID = "gw"
			agent.Backend = "claw"
			agent.Model = "openai_compatible/scaleway/glm-5.3"
			agent.Tools = []string{}
			_, err = BuildExecutor(ExecutorSpec{
				Ctx:             secrets.WithCredentials(context.Background(), mixed),
				Store:           st,
				RunID:           "run-classifier-boundary",
				Workflow:        &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"gw": agent}},
				ContentBoundary: row.boundary,
			})
			if !row.refused {
				if err != nil {
					t.Fatalf("BuildExecutor: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrClassifierBoundaryRefused) {
				t.Fatalf("BuildExecutor err = %v, want %v — a vendor classifier must not read a team_gateway run's tool calls", err, ErrClassifierBoundaryRefused)
			}
		})
	}
}
