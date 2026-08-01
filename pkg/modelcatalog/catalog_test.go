package modelcatalog

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// hostReport builds a detect.Report by hand so the tests never depend on the
// machine they run on (a developer with ANTHROPIC_API_KEY exported would
// otherwise see different answers than CI).
func hostReport(providers []detect.ProviderStatus, backends []detect.BackendStatus) *detect.Report {
	return &detect.Report{
		PreferenceOrder: []string{detect.BackendClaudeCode, detect.BackendClaw},
		ResolvedDefault: detect.BackendClaudeCode,
		Backends:        backends,
		Providers:       providers,
	}
}

func bareHost() *detect.Report {
	return hostReport(
		[]detect.ProviderStatus{
			{Name: "anthropic", Available: false, Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5"},
			{Name: "zai", Available: false, Source: "ZAI_API_KEY", SuggestedModel: "anthropic/glm-5.2"},
			{Name: "openai", Available: false, Source: "OPENAI_API_KEY", SuggestedModel: "openai/gpt-5.4-mini"},
		},
		[]detect.BackendStatus{
			{Name: detect.BackendClaudeCode, Available: false},
			{Name: detect.BackendClaw, Available: false},
			{Name: detect.BackendPi, Available: false},
		},
	)
}

func build(t *testing.T, opts Options) Catalog {
	t.Helper()
	cat, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cat
}

func TestBuildListsKnownSpecsByDefault(t *testing.T) {
	cat := build(t, Options{Report: bareHost()})
	if len(cat.Models) == 0 {
		t.Fatal("catalog must not be empty")
	}
	if _, ok := cat.Find("anthropic/claude-opus-5"); !ok {
		t.Errorf("expected the known-spec set to include anthropic/claude-opus-5, got %v", cat.SortedSpecs())
	}
	for _, m := range cat.Models {
		if m.Spec == "" || m.Provider == "" || m.Model == "" {
			t.Errorf("entry has empty identity fields: %+v", m)
		}
	}
}

func TestBuildRejectsMalformedSpec(t *testing.T) {
	if _, err := Build(context.Background(), Options{
		Specs:  []string{"not-a-spec"},
		Report: bareHost(),
	}); err == nil {
		t.Fatal("expected an error on a spec with no provider prefix")
	}
}

func TestNothingIsUsableOnABareHost(t *testing.T) {
	cat := build(t, Options{Report: bareHost()})
	for _, m := range cat.Models {
		if m.Usable {
			t.Errorf("%s reported usable on a host with no credentials", m.Spec)
		}
		if m.UnusableReason == "" {
			t.Errorf("%s is unusable but gives no reason", m.Spec)
		}
	}
	if cat.RecommendedSpec != "" {
		t.Errorf("RecommendedSpec = %q, want empty on a credential-less host", cat.RecommendedSpec)
	}
}

func TestClaudeCodeOAuthMakesClaudeModelsUsableWithoutAnAPIKey(t *testing.T) {
	report := hostReport(
		[]detect.ProviderStatus{
			// The common local setup: signed-in CLI, no exported API key.
			{Name: "anthropic", Available: false, Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5"},
			{Name: "openai", Available: false, Source: "OPENAI_API_KEY", SuggestedModel: "openai/gpt-5.4-mini"},
			{Name: "zai", Available: false, SuggestedModel: "anthropic/glm-5.2"},
		},
		[]detect.BackendStatus{
			{Name: detect.BackendClaudeCode, Available: true, Auth: detect.AuthOAuth},
			{Name: detect.BackendClaw, Available: false},
		},
	)
	cat := build(t, Options{
		Specs:  []string{"anthropic/claude-opus-5", "openai/gpt-5.5", "anthropic/glm-5.2"},
		Report: report,
	})

	claude, _ := cat.Find("anthropic/claude-opus-5")
	if !claude.Usable {
		t.Errorf("a signed-in claude_code CLI must make a Claude model usable: %+v", claude)
	}
	if len(claude.Backends) != 1 || claude.Backends[0] != detect.BackendClaudeCode {
		t.Errorf("backends = %v, want [claude_code]", claude.Backends)
	}

	// claude_code carries an Anthropic credential — it cannot be pointed at
	// another vendor, and it cannot serve the GLM façade either.
	gpt, _ := cat.Find("openai/gpt-5.5")
	if gpt.Usable {
		t.Errorf("claude_code must not make an OpenAI model usable: %+v", gpt)
	}
	glm, _ := cat.Find("anthropic/glm-5.2")
	if glm.Usable {
		t.Errorf("claude_code must not make a GLM model usable: %+v", glm)
	}

	if cat.RecommendedSpec != "anthropic/claude-opus-5" {
		t.Errorf("RecommendedSpec = %q, want anthropic/claude-opus-5", cat.RecommendedSpec)
	}
}

// The GLM family speaks the Anthropic API but bills to z.ai. An Anthropic key
// alone must not report it usable — that combination 401s at runtime.
func TestGLMNeedsAZAICredentialNotAnAnthropicOne(t *testing.T) {
	anthropicOnly := hostReport(
		[]detect.ProviderStatus{
			{Name: "anthropic", Available: true, Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5"},
			{Name: "zai", Available: false, Source: "ZAI_API_KEY", SuggestedModel: "anthropic/glm-5.2"},
		},
		[]detect.BackendStatus{
			{Name: detect.BackendClaudeCode, Available: false},
			{Name: detect.BackendClaw, Available: true, Auth: detect.AuthAPIKey},
		},
	)
	cat := build(t, Options{Specs: []string{"anthropic/glm-5.2", "anthropic/claude-opus-5"}, Report: anthropicOnly})
	if glm, _ := cat.Find("anthropic/glm-5.2"); glm.Usable {
		t.Errorf("glm-5.2 must not be usable on an Anthropic-only host: %+v", glm)
	}
	if claude, _ := cat.Find("anthropic/claude-opus-5"); !claude.Usable {
		t.Error("claude-opus-5 must be usable with an Anthropic key + claw")
	}

	zaiOnly := hostReport(
		[]detect.ProviderStatus{
			{Name: "anthropic", Available: false, Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5"},
			{Name: "zai", Available: true, Source: "ZAI_API_KEY", SuggestedModel: "anthropic/glm-5.2"},
		},
		[]detect.BackendStatus{
			{Name: detect.BackendClaudeCode, Available: false},
			{Name: detect.BackendClaw, Available: true, Auth: detect.AuthAPIKey},
		},
	)
	cat = build(t, Options{Specs: []string{"anthropic/glm-5.2", "anthropic/claude-opus-5"}, Report: zaiOnly})
	glm, _ := cat.Find("anthropic/glm-5.2")
	if !glm.Usable {
		t.Errorf("glm-5.2 must be usable with a z.ai key + claw: %+v", glm)
	}
	if glm.CredentialProvider != "zai" {
		t.Errorf("CredentialProvider = %q, want zai", glm.CredentialProvider)
	}
	if glm.CredentialSource != "ZAI_API_KEY" {
		t.Errorf("CredentialSource = %q, want ZAI_API_KEY", glm.CredentialSource)
	}
	if claude, _ := cat.Find("anthropic/claude-opus-5"); claude.Usable {
		t.Error("claude-opus-5 must not be usable on a z.ai-only host")
	}
}

func TestCredentialProviderFor(t *testing.T) {
	cases := []struct{ provider, model, want string }{
		{"anthropic", "claude-opus-5", "anthropic"},
		{"anthropic", "glm-5.2", "zai"},
		{"anthropic", "GLM-4.6", "zai"},
		{"openai", "gpt-5.5", "openai"},
		{"XAI", "grok-3", "xai"},
	}
	for _, tc := range cases {
		if got := CredentialProviderFor(tc.provider, tc.model); got != tc.want {
			t.Errorf("CredentialProviderFor(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestUltracodeCapableTracksTheC089Gate(t *testing.T) {
	cases := []struct {
		spec string
		want bool
	}{
		{"anthropic/claude-opus-4-8", true},
		{"claude-opus-4-8", true},
		{"anthropic/opus", true},
		{"anthropic/claude-opus-5", false},
		{"anthropic/claude-sonnet-4-6", false},
		{"openai/gpt-5.5", false},
	}
	for _, tc := range cases {
		if got := UltracodeCapable(tc.spec); got != tc.want {
			t.Errorf("UltracodeCapable(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// A zero price must never read as "free" — the picker shows "—" for unknown,
// and PriceKnown is what tells the two apart.
func TestPriceKnownDisambiguatesZero(t *testing.T) {
	cat := build(t, Options{Specs: []string{"anthropic/claude-opus-5"}, Report: bareHost()})
	e, ok := cat.Find("anthropic/claude-opus-5")
	if !ok {
		t.Fatal("missing entry")
	}
	if e.PriceKnown && e.InputCostPerM == 0 && e.OutputCostPerM == 0 {
		t.Error("PriceKnown must not be set when both rates are zero")
	}
	if !e.PriceKnown && (e.InputCostPerM != 0 || e.OutputCostPerM != 0) {
		t.Error("a rate was reported without PriceKnown")
	}
}

func TestBuildReportsBackendsAndResolvedDefault(t *testing.T) {
	report := bareHost()
	cat := build(t, Options{Specs: []string{"anthropic/claude-opus-5"}, Report: report})
	if cat.ResolvedDefaultBackend != report.ResolvedDefault {
		t.Errorf("ResolvedDefaultBackend = %q, want %q", cat.ResolvedDefaultBackend, report.ResolvedDefault)
	}
	if len(cat.Backends) != len(report.Backends) {
		t.Errorf("Backends = %d entries, want %d", len(cat.Backends), len(report.Backends))
	}
}

// A provider iterion has no probe for must say so rather than silently reading
// as "no credential" — the two are different operator problems.
func TestUnknownProviderGetsItsOwnReason(t *testing.T) {
	cat := build(t, Options{Specs: []string{"mistral/mistral-large"}, Report: bareHost()})
	e, ok := cat.Find("mistral/mistral-large")
	if !ok {
		t.Fatal("missing entry")
	}
	if e.Usable {
		t.Error("an unprobed provider cannot be usable")
	}
	if want := "no credential probe"; !strings.Contains(e.UnusableReason, want) {
		t.Errorf("UnusableReason = %q, want it to mention %q", e.UnusableReason, want)
	}
}
