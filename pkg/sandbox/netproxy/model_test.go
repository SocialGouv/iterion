package netproxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestAModelRequestIsRecognisedByItsHostOrItsPath(t *testing.T) {
	extra, err := modelHostPolicy([]string{" GW.Example.NET. ", "", "https://llm.corp:8443/v1", "gw2.example.net:9443", "**.ai.internal", "[fd00::1]", "10.1.0.0/16", "пример.рф", "gw3.corp.."})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, path string
		want       bool
	}{
		// the providers' own hosts, whatever the path
		{"api.anthropic.com", "/v1/messages", true},
		{"api.anthropic.com", "/api/oauth/token", true},
		{"API.OPENAI.COM.", "/v1/files", true},
		{"openrouter.ai", "/api/v1/models", true},
		{"api.x.ai", "/v1/models", true},
		{"api.mistral.ai", "/v1/files", true},
		{"api.z.ai", "/api/paas/v4/files", true},
		{"api.moonshot.ai", "/v1/files", true},
		{"api.moonshot.cn", "/v1/files", true},
		{"generativelanguage.googleapis.com", "/v1beta/files", true},
		{"aiplatform.googleapis.com", "/v1/projects/p/locations/global/endpoints", true},
		{"europe-west1-aiplatform.googleapis.com", "/v1/x", true},
		{"acme.openai.azure.com", "/openai/files", true},
		{"bedrock-runtime.eu-west-3.amazonaws.com", "/x", true},
		{"radius.pi.dev", "/v1/models", true},
		{"llm.corp.example", "/v1/messages/batches", true},
		{"gateway.ai.cloudflare.com", "/v1/acct/gw/google/v1/models/gemini:countTokens", true},
		{"ollama.internal.example", "/api/embed", true},
		{"bedrock.internal.example", "/model/m/count-tokens", true},
		{"chatgpt.com", "/backend-api/codex/responses", true},
		{"chatgpt.com", "/backend-api/codex/models", true},
		{"chatgpt.com", "/backend-api/codex/images/generations", true},
		// the operator's own model hosts, in the network rules' syntax
		{"gw.example.net", "/generate-text", true},
		{"llm.corp", "/generate-text", true},
		{"gw2.example.net", "/generate-text", true},
		{"a.b.ai.internal", "/generate-text", true},
		{"fd00::1", "/generate-text", true},
		{"10.1.2.3", "/generate-text", true},
		{"xn--e1afmkfd.xn--p1ai", "/generate-text", true},
		{"gw3.corp", "/generate-text", true},
		// a model API's path, whatever the host
		{"llm.internal.example", "/anthropic/v1/messages", true},
		{"llm.internal.example", "/v1/messages/count_tokens", true},
		{"llm.internal.example", "/v1/chat/completions/", true},
		{"llm.internal.example", "/v1/completions", true},
		{"llm.internal.example", "/v1/responses", true},
		{"api.individual.githubcopilot.com", "/responses", true},
		{"gateway.ai.cloudflare.com", "/v1/acct/gw/openai/responses", true},
		{"llm.internal.example", "/v1/images/generations", true},
		{"llm.internal.example", "/v1/embeddings", true},
		{"llm.internal.example", "/v1/models/gemini:generateContent", true},
		{"llm.internal.example", "/v1/models/gemini:streamGenerateContent", true},
		{"llm.internal.example", "/v1/models/claude:rawPredict", true},
		{"llm.internal.example", "/v1/models/claude:streamRawPredict", true},
		{"ollama.internal.example", "/api/chat", true},
		{"ollama.internal.example", "/api/generate", true},
		{"bedrock.internal.example", "/model/anthropic.claude-v2/invoke", true},
		{"bedrock.internal.example", "/model/anthropic.claude-v2/invoke-with-response-stream", true},
		{"bedrock.internal.example", "/model/m/converse", true},
		{"bedrock.internal.example", "/model/m/converse-stream", true},
		// not a model API: substitution applies
		{"github.com", "/o/r.git/git-receive-pack", false},
		{"api.github.com", "/repos/o/r/dispatches", false},
		{"oauth2.googleapis.com", "/token", false},
		{"storage.googleapis.com", "/upload/storage/v1/b/bucket/o", false},
		{"bedrock.eu-west-3.amazonaws.com", "/guardrails", false},
		{"discord.com", "/api/v10/channels/1/messages", false},
		{"hooks.example.com", "/actions/invoke", false},
		{"gw.example.org", "/generate-text", false},
		{"ai.internal.example", "/generate-text", false},
		{"10.2.0.1", "/generate-text", false},
	} {
		if got := modelRequest(c.host, c.path, extra); got != c.want {
			t.Errorf("modelRequest(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}

// An operator's model host that is not a host pattern is refused, never
// ignored: an ignored gateway would have its conversation substituted.
func TestAMalformedModelHostIsRefused(t *testing.T) {
	for _, bad := range []string{"gw.corp/v1", "a*b.corp", "*.*.corp", "https://", "!", "http:gw.corp", "user:pass@gw.corp", "gw corp", "**", "*", "!gw.corp", ".llm.corp", "a..b.corp", "**..corp"} {
		if _, err := modelHostPolicy([]string{bad}); err == nil {
			t.Errorf("modelHostPolicy(%q) accepted it", bad)
		}
		pol, _ := Compile(ModeOpen, nil)
		if _, err := New(Options{Policy: pol, InspectCA: &EphemeralCA{}, ModelHosts: []string{bad}}); err == nil {
			t.Errorf("New with model host %q: no error", bad)
		}
	}
}

// A refused operator host is named by its position: a URL's userinfo may hold
// a credential, and the error reaches the run's start and its logs.
func TestARefusedModelHostIsNamedByItsPosition(t *testing.T) {
	for _, entry := range []string{"https://user:s3cr3t-pass@/v1", "gw.corp/key=s3cr3t-pass"} {
		_, err := modelHostPolicy([]string{"gw.corp", entry})
		if err == nil {
			t.Fatalf("scenario broken: %q was accepted", entry)
		}
		if strings.Contains(err.Error(), "s3cr3t-pass") || !strings.Contains(err.Error(), "#2") {
			t.Errorf("error = %q, want the entry's position and none of its text", err)
		}
	}
}

// A body over the inspection bound is refused (413), never cut: the proxy
// scans and substitutes the whole body, and a truncated upload is a corrupt
// one.
func TestABodyOverTheInspectionBoundIsRefused(t *testing.T) {
	defer func(n int) { maxInspectedBody = n }(maxInspectedBody)
	maxInspectedBody = 16
	p := &Proxy{inspect: &inspectConfig{}}
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/x", strings.NewReader("0123456789abcdefXYZ"))
	if _, refusal := p.inspectRequest(req, "api.github.com", true); refusal == nil || refusal.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the bound: refusal = %+v, want 413", refusal)
	}
	req, _ = http.NewRequest(http.MethodPost, "https://api.github.com/x", strings.NewReader("0123456789abcdef"))
	if body, refusal := p.inspectRequest(req, "api.github.com", true); refusal != nil || string(body) != "0123456789abcdef" {
		t.Errorf("a body at the bound: body %q, refusal %+v", body, refusal)
	}
}
