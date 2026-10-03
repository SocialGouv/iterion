package ir

import (
	"strings"
	"testing"
)

// gatewayAgentSrc renders a one-agent workflow whose agent carries the
// given extra properties, so each test states only what it is about.
func gatewayAgentSrc(nodeProps string) string {
	return `
agent gw:
  model: "openai_compatible/gpt-5x"
` + nodeProps + `
workflow w:
  entry: gw
  gw -> done
`
}

// gatewayRouterSrc renders a workflow whose `mode: llm` router carries the
// given extra properties; the two edges keep the llm-router edge count
// above its diagnostic floor.
func gatewayRouterSrc(routerProps string) string {
	return `
router pick:
  mode: llm
  model: "openai_compatible/m"
` + routerProps + `
agent t1:
  model: "m1"

agent t2:
  model: "m2"

workflow w:
  entry: pick
  pick -> t1
  pick -> t2
`
}

func countC184(r *CompileResult) int {
	n := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagGatewayCrossing {
			n++
		}
	}
	return n
}

func c184Messages(r *CompileResult) []string {
	var out []string
	for _, d := range r.Diagnostics {
		if d.Code == DiagGatewayCrossing {
			out = append(out, d.Message)
		}
	}
	return out
}

// TestC184_FiresOnAnExplicitNonClawBackendWithAGatewayModel — the shape the
// diagnostic exists for: the model is a gateway route as written and the
// node pins a CLI backend that cannot serve it. Guarded by muting the
// emission in validate_gateway.go (the whole suite reddens there); this
// test must red on ITS assertion — one C186 naming the node, the model and
// the backend — when only the backend arm is muted (its `return` to a
// no-op).
func TestC184_FiresOnAnExplicitNonClawBackendWithAGatewayModel(t *testing.T) {
	r := compileFile(t, gatewayAgentSrc("  backend: \"claude_code\"\n"))
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a gateway model beside backend claude_code (%v)", got, r.Diagnostics)
	}
	msg := c184Messages(r)[0]
	for _, want := range []string{`gw`, `openai_compatible/gpt-5x`, `claude_code`, `claw backend`} {
		if !strings.Contains(msg, want) {
			t.Errorf("C186 message %q does not name %q", msg, want)
		}
	}
	if d := r.Diagnostics[findC184(r)]; d.Severity != SeverityWarning {
		t.Errorf("C186 severity = %v, want warning", d.Severity)
	}
}

// TestC184_FiresOnTheWorkflowDefaultBackend — a node that names no backend
// inherits the workflow's `default_backend:`, and an explicit non-claw
// default is as explicit as a node's own: the executor reads the same
// effective backend (effectiveNodeBackend mirrors resolveBackendName).
func TestC184_FiresOnTheWorkflowDefaultBackend(t *testing.T) {
	src := `
agent gw:
  model: "openai_compatible/gpt-5x"

workflow w:
  entry: gw
  default_backend: codex
  gw -> done
`
	r := compileFile(t, src)
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a gateway model under default_backend codex (%v)", got, r.Diagnostics)
	}
	msg := c184Messages(r)[0]
	for _, want := range []string{`gw`, `openai_compatible/gpt-5x`, `codex`, `claw backend`} {
		if !strings.Contains(msg, want) {
			t.Errorf("C186 message %q does not name %q", msg, want)
		}
	}
}

// TestC184_StaysSilentWhenTheBackendIsInferred — the launch's claw-default
// path: no backend named anywhere, so there is nothing the author pinned
// against the gateway route and nothing the executor would refuse.
func TestC184_StaysSilentWhenTheBackendIsInferred(t *testing.T) {
	r := compileFile(t, gatewayAgentSrc(""))
	if got := countC184(r); got != 0 {
		t.Errorf("C186 count = %d, want 0 when no backend is named anywhere (%v)", got, r.Diagnostics)
	}
}

// TestC184_StaysSilentOnATemplatedModel — a `{{vars.…}}` or `${VAR}` model
// is no route at compile time; whatever it expands to is judged at
// dispatch, where the value is known. The templates sit AFTER the gateway
// prefix on purpose: a bare `{{vars.mid}}` parses as a bare id either way,
// and the guard this test holds is the one that keeps a templated gateway
// route from being judged on its prefix alone.
func TestC184_StaysSilentOnATemplatedModel(t *testing.T) {
	for _, model := range []string{`"openai_compatible/{{vars.mid}}"`, `"openai_compatible/${GW_MODEL}"`} {
		// The var is declared on purpose: the silence under test is the
		// template rule, not a broken reference (C033 is another code).
		src := "vars:\n  mid: string = \"m\"\n" +
			strings.Replace(gatewayAgentSrc("  backend: \"claude_code\"\n"),
				`"openai_compatible/gpt-5x"`, model, 1)
		r := compileFile(t, src)
		if got := countC184(r); got != 0 {
			t.Errorf("C186 count = %d, want 0 for templated model %s (%v)", got, model, r.Diagnostics)
		}
	}
}

// TestC184_StaysSilentOnAClawResolvingBackend — `claw` and `auto` both
// resolve to claw at dispatch, which is where a gateway route runs; a
// gateway provider hint beside a gateway model names one wire, not two.
func TestC184_StaysSilentOnAClawResolvingBackend(t *testing.T) {
	for _, props := range []string{
		"  backend: \"claw\"\n",
		"  backend: auto\n",
		"  backend: \"claw\"\n  provider: openai_compatible\n",
		"  provider: auto\n",
	} {
		r := compileFile(t, gatewayAgentSrc(props))
		if got := countC184(r); got != 0 {
			t.Errorf("C186 count = %d, want 0 for props %q (%v)", got, props, r.Diagnostics)
		}
	}
}

// TestC184_FiresOnAVendorProviderHintWithAGatewayModel — the hint arm of
// the parity rule: a gateway model borrows no vendor's wire, so a literal
// `provider:` naming one is refused at dispatch whatever the backend says.
func TestC184_FiresOnAVendorProviderHintWithAGatewayModel(t *testing.T) {
	r := compileFile(t, gatewayAgentSrc("  provider: anthropic\n"))
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a gateway model beside provider anthropic (%v)", got, r.Diagnostics)
	}
	msg := c184Messages(r)[0]
	for _, want := range []string{`gw`, `openai_compatible/gpt-5x`, `anthropic`, `wire`} {
		if !strings.Contains(msg, want) {
			t.Errorf("C186 message %q does not name %q", msg, want)
		}
	}
}

// TestC184_StaysSilentOnATemplatedBackend — a `${VAR}` backend is decided
// by the launch environment, not the source; a gateway model beside it is
// a crossing only on the machines where the env names a CLI backend.
func TestC184_StaysSilentOnATemplatedBackend(t *testing.T) {
	r := compileFile(t, gatewayAgentSrc("  backend: \"${GW_BACKEND}\"\n"))
	if got := countC184(r); got != 0 {
		t.Errorf("C186 count = %d, want 0 for a ${VAR} backend (%v)", got, r.Diagnostics)
	}
}

// TestC184_FiresOnAFallbackRoute — a `fallbacks:` entry is a chain element
// the executor judges per element like the primary: a gateway model on the
// route's own non-claw backend fires, and so does one that INHERITS the
// node's non-claw backend by leaving the route's backend empty.
func TestC184_FiresOnAFallbackRoute(t *testing.T) {
	src := `
agent gw:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  fallbacks:
    rescue:
      model: "openai_compatible/m"
      backend: "codex"

workflow w:
  entry: gw
  gw -> done
`
	r := compileFile(t, src)
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a gateway model on a fallback route naming backend codex (%v)", got, r.Diagnostics)
	}
	msg := c184Messages(r)[0]
	for _, want := range []string{`gw`, `fallback route "rescue"`, `openai_compatible/m`, `codex`} {
		if !strings.Contains(msg, want) {
			t.Errorf("C186 message %q does not name %q", msg, want)
		}
	}
}

// TestC184_FiresOnAFallbackRouteInheritingTheNodeBackend — the route names
// only a model; the element's backend is the node's, and claude_code
// cannot serve the gateway model.
func TestC184_FiresOnAFallbackRouteInheritingTheNodeBackend(t *testing.T) {
	src := `
agent gw:
  backend: "claude_code"
  model: "anthropic/claude-opus-5"
  fallbacks:
    rescue:
      model: "openai_compatible/m"

workflow w:
  entry: gw
  gw -> done
`
	r := compileFile(t, src)
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a fallback route inheriting backend claude_code (%v)", got, r.Diagnostics)
	}
}

// TestC184_FiresOnAnLLMRouterPinnedToANonClawBackend — a `mode: llm`
// router's model is a route element the executor judges like an agent's.
func TestC184_FiresOnAnLLMRouterPinnedToANonClawBackend(t *testing.T) {
	r := compileFile(t, gatewayRouterSrc("  backend: kimi\n"))
	if got := countC184(r); got != 1 {
		t.Fatalf("C186 count = %d, want 1 for a gateway model on an llm router naming backend kimi (%v)", got, r.Diagnostics)
	}
	msg := c184Messages(r)[0]
	for _, want := range []string{`pick`, `router`, `openai_compatible/m`, `kimi`, `claw backend`} {
		if !strings.Contains(msg, want) {
			t.Errorf("C186 message %q does not name %q", msg, want)
		}
	}
}

// TestC184_StaysSilentOnATemplatedRouteBackend — a `${VAR}` backend on the
// route itself is undecided at compile time: the launch environment may
// name claw, and the node's own backend does not serve the element then.
func TestC184_StaysSilentOnATemplatedRouteBackend(t *testing.T) {
	src := `
agent gw:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  fallbacks:
    rescue:
      model: "openai_compatible/m"
      backend: "${GW_BACKEND}"

workflow w:
  entry: gw
  gw -> done
`
	r := compileFile(t, src)
	if got := countC184(r); got != 0 {
		t.Errorf("C186 count = %d, want 0 for a fallback route whose backend is ${VAR} (%v)", got, r.Diagnostics)
	}
}

func findC184(r *CompileResult) int {
	for i, d := range r.Diagnostics {
		if d.Code == DiagGatewayCrossing {
			return i
		}
	}
	return -1
}
