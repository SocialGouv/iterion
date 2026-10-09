package netproxy

import (
	"math"
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
// ("!gw.corp" is NOT in the refused list: an exclusion is valid since
// v3.7-era #2050 — it lifts the model detection for that host.)
func TestAMalformedModelHostIsRefused(t *testing.T) {
	for _, bad := range []string{"gw.corp/v1", "a*b.corp", "*.*.corp", "https://", "!", "http:gw.corp", "user:pass@gw.corp", "gw corp", "**", "*", ".llm.corp", "a..b.corp", "**..corp"} {
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

// The bound is the operator's when set (ITERION_SANDBOX_INSPECT_MAX_BODY): a
// large upload over plain HTTP — a git push to an on-prem forge — goes
// through with a bound raised, and one over it is still refused, never cut.
// A negative bound fails New.
func TestTheInspectionBoundIsTheOperatorsWhenSet(t *testing.T) {
	p := &Proxy{inspect: &inspectConfig{}, maxBody: 32}
	big := strings.Repeat("x", 100)
	req, _ := http.NewRequest(http.MethodPost, "http://git.corp/repo.git/git-receive-pack", strings.NewReader(big[:32]))
	if body, refusal := p.inspectRequest(req, "git.corp", false); refusal != nil || len(body) != 32 {
		t.Errorf("a body at the operator's bound: %d bytes, refusal %+v", len(body), refusal)
	}
	req, _ = http.NewRequest(http.MethodPost, "http://git.corp/repo.git/git-receive-pack", strings.NewReader(big[:33]))
	if _, refusal := p.inspectRequest(req, "git.corp", false); refusal == nil || refusal.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the operator's bound: refusal = %+v, want 413", refusal)
	}
	defer func(n int) { maxInspectedBody = n }(maxInspectedBody)
	maxInspectedBody = 16
	p.maxBody = 64
	req, _ = http.NewRequest(http.MethodPost, "http://git.corp/x", strings.NewReader(big[:64]))
	if body, refusal := p.inspectRequest(req, "git.corp", false); refusal != nil || len(body) != 64 {
		t.Errorf("a raised bound over the default: %d bytes, refusal %+v", len(body), refusal)
	}
	// The largest bound is no bound: the body comes through whole, never read
	// as empty.
	p.maxBody = math.MaxInt64
	req, _ = http.NewRequest(http.MethodPost, "http://git.corp/x", strings.NewReader(big))
	if body, refusal := p.inspectRequest(req, "git.corp", false); refusal != nil || string(body) != big {
		t.Errorf("the largest bound: %d bytes, refusal %+v; want the %d-byte body whole", len(body), refusal, len(big))
	}
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Policy: pol, MaxInspectedBody: -1}); err == nil {
		t.Error("a negative bound was accepted")
	}
	if px, err := New(Options{Policy: pol, MaxInspectedBody: 1 << 30}); err != nil || px.maxBody != 1<<30 {
		t.Errorf("New with a bound: %v, maxBody %d", err, px.maxBody)
	}
}

// An operator's `!host` exclusion lifts the model detection for that host:
// a tool API whose path ends like a model API's is inspected and
// substituted like any other request. A host that IS a built-in model
// provider cannot be excluded — refused when the run's proxy starts.
func TestModelHostExclusionLiftsTheModelDetection(t *testing.T) {
	pol, err := modelHostPolicy([]string{"!tools.corp"})
	if err != nil {
		t.Fatalf("an exclusions-only list is valid: %v", err)
	}
	if !pol.Unmodeled("tools.corp") || !pol.Unmodeled("TOOLS.CORP.") {
		t.Error("the excluded host is not un-modeled (case- and trailing-dot insensitive)")
	}
	if pol.Unmodeled("other.corp") {
		t.Error("an unexcluded host reads as un-modeled")
	}
	if modelRequest("tools.corp", "/v1/messages", pol) {
		t.Error("a lifted host whose path ends like a model API is still treated as a model request")
	}
	if !modelRequest("api.anthropic.com", "/v1/messages", pol) {
		t.Error("a built-in provider host must stay a model request")
	}
	if _, err := modelHostPolicy([]string{"!api.anthropic.com"}); err == nil ||
		!strings.Contains(err.Error(), "built-in model provider") {
		t.Errorf("excluding a built-in provider host: err = %v, want a refusal naming it", err)
	}
	// Mixed: positive entries keep their allow semantics beside the lift.
	pol, err = modelHostPolicy([]string{"gw.corp", "!tools.corp"})
	if err != nil {
		t.Fatal(err)
	}
	if !modelRequest("gw.corp", "/v1/messages", pol) {
		t.Error("a positive entry stopped granting model status")
	}
	if modelRequest("tools.corp", "/whatever", pol) {
		t.Error("an excluded host reads as a model request on any path")
	}
}
