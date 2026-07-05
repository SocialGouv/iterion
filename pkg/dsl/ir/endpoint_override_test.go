package ir

import "testing"

// endpointSrc builds a minimal one-agent workflow with the given backend
// and per-node endpoint override fields so the endpoint-override compile +
// validation path can be exercised in isolation. Empty base_url/api_key_env
// values are omitted so the "unset" case can be tested too.
func endpointSrc(backend, baseURL, apiKeyEnv string) string {
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "openai/kimi-k2"
  backend: "` + backend + `"
`
	if baseURL != "" {
		src += `  base_url: "` + baseURL + `"` + "\n"
	}
	if apiKeyEnv != "" {
		src += `  api_key_env: "` + apiKeyEnv + `"` + "\n"
	}
	src += `  system: sys
  output: empty

workflow w:
  entry: writer
  writer -> done
`
	return src
}

// TestEndpointOverride_PopulatesLLMFields verifies base_url/api_key_env flow
// from the DSL through the parser and compiler onto the node's LLMFields.
func TestEndpointOverride_PopulatesLLMFields(t *testing.T) {
	w := mustCompile(t, endpointSrc("claw", "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"))
	n, ok := w.Nodes["writer"].(*AgentNode)
	if !ok {
		t.Fatalf("writer node = %T, want *AgentNode", w.Nodes["writer"])
	}
	if n.BaseURL != "https://api.moonshot.ai/v1" {
		t.Errorf("BaseURL = %q, want %q", n.BaseURL, "https://api.moonshot.ai/v1")
	}
	if n.APIKeyEnv != "MOONSHOT_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want %q", n.APIKeyEnv, "MOONSHOT_API_KEY")
	}
}

// TestEndpointOverride_NoFieldsLeavesEmpty verifies the fields default to
// empty when the DSL omits them (so the claw backend takes the normal
// env-fallback resolve path).
func TestEndpointOverride_NoFieldsLeavesEmpty(t *testing.T) {
	w := mustCompile(t, endpointSrc("claw", "", ""))
	n := w.Nodes["writer"].(*AgentNode)
	if n.BaseURL != "" || n.APIKeyEnv != "" {
		t.Errorf("expected empty override fields, got BaseURL=%q APIKeyEnv=%q", n.BaseURL, n.APIKeyEnv)
	}
}

// TestEndpointOverride_OnClawNoWarning verifies claw honours the override, so
// no C173 is emitted.
func TestEndpointOverride_OnClawNoWarning(t *testing.T) {
	r := compileFile(t, endpointSrc("claw", "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"))
	expectNoDiag(t, r, DiagEndpointOverrideIgnored)
}

// TestEndpointOverride_UnsetBackendNoWarning verifies an unset backend
// (defaults to claw at runtime) does not warn.
func TestEndpointOverride_UnsetBackendNoWarning(t *testing.T) {
	// An unset backend can't be expressed via endpointSrc (it always emits
	// backend:), so build the source inline without a backend line.
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "openai/kimi-k2"
  base_url: "https://api.moonshot.ai/v1"
  api_key_env: "MOONSHOT_API_KEY"
  system: sys
  output: empty

workflow w:
  entry: writer
  writer -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagEndpointOverrideIgnored)
}

// TestEndpointOverride_OnClaudeCodeWarns verifies C173 fires when the fields
// are set on a backend (claude_code) that ignores them.
func TestEndpointOverride_OnClaudeCodeWarns(t *testing.T) {
	r := compileFile(t, endpointSrc("claude_code", "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"))
	expectDiag(t, r, DiagEndpointOverrideIgnored)
}

// TestEndpointOverride_ApiKeyEnvAloneWarnsOnCodex verifies the warning fires
// even when only api_key_env (no base_url) is set on an ignoring backend.
func TestEndpointOverride_ApiKeyEnvAloneWarnsOnCodex(t *testing.T) {
	r := compileFile(t, endpointSrc("codex", "", "MOONSHOT_API_KEY"))
	expectDiag(t, r, DiagEndpointOverrideIgnored)
}

// TestEndpointOverride_EnvRefBackendSkipsValidation verifies a backend given
// as a ${VAR} env ref is not judged at compile time (no false C173).
func TestEndpointOverride_EnvRefBackendSkipsValidation(t *testing.T) {
	r := compileFile(t, endpointSrc("${BACKEND:-claw}", "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"))
	expectNoDiag(t, r, DiagEndpointOverrideIgnored)
}
