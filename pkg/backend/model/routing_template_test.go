package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Every routing field the dispatch path reads — a node's model, backend and
// provider, the three on a fallbacks route, a recovery model — resolves a
// `{{vars.…}}` template before the `${VAR:-default}` environment form. Before
// resolveRoutingField the template step was missing: examples/clarify's
// `model: "{{vars.model}}"` reached claw as written and was refused as an
// invalid spec (feed-watch's bilan recorded the same on the claude CLI). The
// llm router, the human LLM interaction and the review companion read their
// model through the same helper; their executors need a registry and a
// backend to exercise and are not driven here.
func TestRoutingFieldsResolveVarsTemplates(t *testing.T) {
	t.Setenv("ROUTING_TEST_MODEL", "")
	e := &ClawExecutor{logger: iterlog.Nop(), vars: map[string]any{
		"model":     "anthropic/claude-sonnet-4-6",
		"backend":   "claw",
		"provider":  "zai",
		"alt":       "openai/gpt-5.5",
		"env_model": "${ROUTING_TEST_MODEL:-anthropic/claude-opus-5}",
	}}

	node := &ir.AgentNode{}
	node.ID = "n"
	node.Model = "{{vars.model}}"
	node.Backend = "{{vars.backend}}"
	node.Provider = "{{vars.provider}}"
	node.Fallbacks = []ir.Fallback{{Name: "alt", Backend: "{{vars.backend}}", Provider: "{{vars.provider}}", Model: "{{vars.alt}}"}}

	if got := e.baselineModel(node); got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("baselineModel = %q, want the var's value", got)
	}
	if got := e.resolveBackendName(node); got != "claw" {
		t.Errorf("resolveBackendName = %q, want claw", got)
	}
	if got := e.resolveProvider(node); got != "zai" {
		t.Errorf("resolveProvider = %q, want zai", got)
	}
	var alt *chainElement
	for i := range e.resolveChain(node) {
		el := e.resolveChain(node)[i]
		if el.Label == "alt" {
			alt = &el
		}
	}
	if alt == nil {
		t.Fatalf("fallback route missing from the chain: %+v", e.resolveChain(node))
	}
	if alt.Model != "openai/gpt-5.5" || alt.Backend != "claw" || alt.Provider != "zai" {
		t.Errorf("fallback route resolved to %+v, want model openai/gpt-5.5 on claw/zai", *alt)
	}

	task, err := e.buildTask(context.Background(), node, backendFields{id: "n", model: node.Model}, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("task.Model = %q, want the var's value", task.Model)
	}

	// The template resolves first, then the environment form inside the var.
	env := &ir.AgentNode{}
	env.ID = "e"
	env.Model = "{{vars.env_model}}"
	if got := e.baselineModel(env); got != "anthropic/claude-opus-5" {
		t.Errorf("template then env: baselineModel = %q, want the env default", got)
	}

	tool := &ir.ToolNode{}
	tool.ID = "t"
	tool.Recovery = &ir.RecoverySpec{Model: "{{vars.alt}}"}
	if got := e.recoveryModel(tool); got != "openai/gpt-5.5" {
		t.Errorf("recoveryModel = %q, want the var's value", got)
	}

	// A namespace the route cannot know stays as written: the backend refuses
	// it loudly, and the compiler refuses it first (C148). Secrets included —
	// the routing resolver carries no secret guard, so a `{{secrets.…}}` can
	// never render a value into a model id that backends log.
	for _, raw := range []string{"{{outputs.x.m}}", "{{input.m}}", "{{secrets.token}}", "{{loop.l.iteration}}"} {
		other := &ir.AgentNode{}
		other.ID = "o"
		other.Model = raw
		if got := e.baselineModel(other); got != raw {
			t.Errorf("%s in model = %q, want it kept as written", raw, got)
		}
	}
}
