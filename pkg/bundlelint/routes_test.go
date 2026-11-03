package bundlelint_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestNodeBackendsKeepsConditionalEnvironmentRoutes(t *testing.T) {
	for _, kind := range []string{"agent", "judge"} {
		for _, route := range []struct {
			name, env, backend, action string
			want                       []string
		}{
			{"environment replaces source default", "pi", "${ITERION_TEST_AUDIT_BACKEND:-claude_code}", "", []string{"claude_code", "pi"}},
			{"source default is not an extra route", "claude_code", "${ITERION_TEST_AUDIT_BACKEND:-pi}", "", []string{"claude_code"}},
			{"skip", "pi", "${ITERION_TEST_AUDIT_BACKEND:-claude_code}", ir.FallbackActionSkip, []string{"claude_code"}},
			{"inherit node rather than workflow", "", "", "", []string{"claude_code"}},
		} {
			t.Run(kind+"/"+route.name, func(t *testing.T) {
				t.Setenv("ITERION_TEST_AUDIT_BACKEND", route.env)
				fallbacks := []ir.Fallback{{Backend: route.backend, Model: "test/model", Action: route.action, When: "vars.rescue == true"}}
				original := slices.Clone(fallbacks)
				fields := ir.LLMFields{Backend: "claude_code"}
				var node ir.LLMNode = &ir.AgentNode{LLMFields: fields, Fallbacks: fallbacks}
				if kind == "judge" {
					node = &ir.JudgeNode{LLMFields: fields, Fallbacks: fallbacks}
				}
				wf := &ir.Workflow{DefaultBackend: "pi"}
				run := model.NewProgramExecutor(wf)
				run.SetVars(map[string]any{"rescue": true})
				active := append([]string{run.EffectiveBackendName(node)}, run.EffectiveFallbackBackends(node)...)
				if !slices.Equal(active, route.want) {
					t.Fatalf("fixture dispatch routes = %v, want %v", active, route.want)
				}
				if got := bundlelint.NodeBackends(node, wf); !slices.Equal(got, active) {
					t.Errorf("audited routes = %v, dispatch with condition active = %v", got, active)
				}
				if !reflect.DeepEqual(node.GetFallbacks(), original) {
					t.Error("capability audit changed the node's fallback conditions")
				}
			})
		}
	}
}
