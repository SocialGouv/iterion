package modelcatalog

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestReportFromCloudPresence_EmptyIsNotTheHost(t *testing.T) {
	report := ReportFromCloudPresence(CloudPresence{})
	for _, p := range report.Providers {
		if p.Available {
			t.Errorf("empty presence marked provider %s available", p.Name)
		}
	}
	for _, b := range report.Backends {
		if b.Available {
			t.Errorf("empty presence marked backend %s available", b.Name)
		}
	}
	if report.ResolvedDefault != "" {
		t.Errorf("ResolvedDefault = %q, want empty", report.ResolvedDefault)
	}
}

func TestReportFromCloudPresence_BYOKUnlocksClawNotTheControlPlane(t *testing.T) {
	report := ReportFromCloudPresence(CloudPresence{
		ProviderSources: map[string]string{"openai": "OPENAI_API_KEY"},
	})
	openai, ok := providerStatus(report, "openai")
	if !ok || !openai.Available {
		t.Fatal("openai BYOK must mark the openai provider available")
	}
	if openai.Source != "OPENAI_API_KEY" {
		t.Errorf("Source = %q, want OPENAI_API_KEY (a name, not a value)", openai.Source)
	}
	anthropic, _ := providerStatus(report, "anthropic")
	if anthropic.Available {
		t.Error("an OpenAI key must not mark anthropic available")
	}
	claw, _ := backendStatus(report, detect.BackendClaw)
	if !claw.Available {
		t.Error("claw must be available when any provider key is present")
	}
	cc, _ := backendStatus(report, detect.BackendClaudeCode)
	if cc.Available {
		t.Error("claude_code must not be available on an OpenAI-only presence")
	}
}

func TestReportFromCloudPresence_ClaudeCodeOAuthUnlocksClaudeWithoutAnAPIKey(t *testing.T) {
	report := ReportFromCloudPresence(CloudPresence{ClaudeCodeOAuth: true})
	cat := build(t, Options{
		Specs:             []string{"anthropic/claude-opus-5", "openai/gpt-5.5", "anthropic/glm-5.2"},
		Report:            &report,
		Reachability:      ReachabilityCloud,
		UnprovenIsUnknown: true,
	})
	claude, _ := cat.Find("anthropic/claude-opus-5")
	if !claude.Usable || claude.Reachability != ReachabilityCloud {
		t.Errorf("claude-opus-5 must be cloud-proven from a forfait: %+v", claude)
	}
	if gpt, _ := cat.Find("openai/gpt-5.5"); gpt.Usable {
		t.Errorf("claude_code oauth must not make openai usable: %+v", gpt)
	}
	if glm, _ := cat.Find("anthropic/glm-5.2"); glm.Usable {
		t.Errorf("claude_code oauth must not make glm usable: %+v", glm)
	}
}

func TestUnprovenCloudModelsAreUnknownNotHostUnreachable(t *testing.T) {
	report := ReportFromCloudPresence(CloudPresence{
		ProviderSources: map[string]string{"openai": "OPENAI_API_KEY"},
	})
	cat := build(t, Options{
		Specs:             []string{"openai/gpt-5.5", "anthropic/claude-opus-5"},
		Report:            &report,
		Reachability:      ReachabilityCloud,
		UnprovenIsUnknown: true,
	})
	if cat.Reachability != ReachabilityCloud {
		t.Errorf("catalog reachability = %q, want cloud", cat.Reachability)
	}
	gpt, _ := cat.Find("openai/gpt-5.5")
	if !gpt.Usable || gpt.Reachability != ReachabilityCloud {
		t.Errorf("gpt must be cloud-proven: %+v", gpt)
	}
	claude, _ := cat.Find("anthropic/claude-opus-5")
	if claude.Usable {
		t.Errorf("claude must not be usable without a tenant anthropic credential: %+v", claude)
	}
	if claude.Reachability != ReachabilityUnknown {
		t.Errorf("claude reachability = %q, want unknown (not a host-unreachable)", claude.Reachability)
	}
	if !strings.Contains(claude.UnusableReason, "cloud") {
		t.Errorf("reason %q should describe the cloud surface, not this host", claude.UnusableReason)
	}
}

func TestDetectProviderNameAndSourceLabelNeverHoldAValue(t *testing.T) {
	if got := DetectProviderName(secrets.ProviderAzure); got != "foundry" {
		t.Errorf("azure → %q, want foundry", got)
	}
	if got := DetectProviderName(secrets.ProviderAnthropic); got != "anthropic" {
		t.Errorf("anthropic → %q", got)
	}
	if got := ProviderSourceLabel(secrets.ProviderAnthropic); got != "ANTHROPIC_API_KEY" {
		t.Errorf("source = %q", got)
	}
	if strings.Contains(ProviderSourceLabel(secrets.ProviderOpenAI), "sk-") {
		t.Fatal("source label leaked a key-shaped string")
	}
}

func TestLocalBuildStampsLocalReachability(t *testing.T) {
	cat := build(t, Options{Specs: []string{"anthropic/claude-opus-5"}, Report: bareHost()})
	if cat.Reachability != ReachabilityLocal {
		t.Errorf("catalog reachability = %q, want local", cat.Reachability)
	}
	e, _ := cat.Find("anthropic/claude-opus-5")
	if e.Reachability != ReachabilityLocal {
		t.Errorf("entry reachability = %q, want local", e.Reachability)
	}
}
