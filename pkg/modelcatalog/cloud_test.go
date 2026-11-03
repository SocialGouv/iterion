package modelcatalog

import (
	"slices"
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
	t.Run("an openai key", func(t *testing.T) {
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
		// The control for the gateway case below: a key pi reads still
		// lights it, so the carve is per provider row, not per presence.
		if pi, _ := backendStatus(report, detect.BackendPi); !pi.Available || pi.Auth != detect.AuthAPIKey {
			t.Errorf("pi = %+v, want available on api_key — pi reads OPENAI_API_KEY", pi)
		}
		cc, _ := backendStatus(report, detect.BackendClaudeCode)
		if cc.Available {
			t.Error("claude_code must not be available on an OpenAI-only presence")
		}
	})

	// ADR-131 §9: a team's gateway key unlocks claw alone. The presence is
	// keyed and labelled through the two functions probeCloudRunPresence
	// keys and labels a stored key with, so the row it must land on is the
	// one the server actually names.
	t.Run("a gateway key unlocks claw only", func(t *testing.T) {
		gw := secrets.ProviderOpenAICompatible
		report := ReportFromCloudPresence(CloudPresence{
			ProviderSources: map[string]string{DetectProviderName(gw): ProviderSourceLabel(gw)},
		})
		if claw, _ := backendStatus(report, detect.BackendClaw); !claw.Available || claw.Auth != detect.AuthAPIKey {
			t.Errorf("claw = %+v, want available on api_key — claw is the backend that drives the gateway", claw)
		}
		if pi, _ := backendStatus(report, detect.BackendPi); pi.Available {
			t.Errorf("pi = %+v, want unavailable — pi resolves none of the OPENAI_COMPATIBLE_* variables, so it cannot spend a gateway key", pi)
		}
		if cc, _ := backendStatus(report, detect.BackendClaudeCode); cc.Available {
			t.Errorf("claude_code = %+v, want unavailable — it cannot be pointed at a gateway", cc)
		}
		if report.ResolvedDefault != detect.BackendClaw {
			t.Errorf("ResolvedDefault = %q, want claw — a node naming no backend runs the gateway on claw", report.ResolvedDefault)
		}
		row, ok := providerStatus(report, DetectProviderName(gw))
		if !ok || !row.Available {
			t.Errorf("gateway row = %+v (found %v), want available — the stored key must light its own row", row, ok)
		}
		if row.SuggestedModel != "" {
			t.Errorf("gateway row suggests %q — the id is the deployment's own naming, so no default model can be guessed for it", row.SuggestedModel)
		}
	})
}

// One stored key per case, through the two functions the server keys and
// labels a stored key with: claw lights on any of them, claude_code on the
// anthropic key alone, and pi only on a key the cloud runner hands pi. The
// verdicts are written out rather than computed, so a predicate that drifts
// from the runner's forward set — or a carve that special-cases one row —
// fails here by name.
func TestReportFromCloudPresence_PiLightsOnlyOnAKeyTheRunnerHandsIt(t *testing.T) {
	cases := []struct {
		provider secrets.Provider
		pi       bool
		why      string
	}{
		{secrets.ProviderAnthropic, true, "the runner sets ANTHROPIC_API_KEY for pi"},
		{secrets.ProviderOpenAI, true, "the runner sets OPENAI_API_KEY for pi"},
		{secrets.ProviderZAI, true, "the runner sets ZAI_API_KEY for pi"},
		{secrets.ProviderMoonshot, true, "the runner sets MOONSHOT_API_KEY for pi"},
		{secrets.ProviderXAI, true, "the runner sets XAI_API_KEY for pi"},
		{secrets.ProviderOpenRouter, true, "the runner sets OPENROUTER_API_KEY for pi"},
		{secrets.ProviderAzure, false, "pi reads AZURE_OPENAI_API_KEY on a host, but the runner never hands it a run's azure key"},
		{secrets.ProviderBedrock, false, "pi reads no AWS credential, and the runner hands it none"},
		{secrets.ProviderVertex, false, "pi reads no GCP credential, and the runner hands it none"},
		{secrets.ProviderOpenAICompatible, false, "pi resolves none of the OPENAI_COMPATIBLE_* variables, and the runner hands it no gateway key"},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		row := DetectProviderName(tc.provider)
		covered[row] = true
		t.Run(row, func(t *testing.T) {
			report := ReportFromCloudPresence(CloudPresence{
				ProviderSources: map[string]string{row: ProviderSourceLabel(tc.provider)},
			})
			if p, ok := providerStatus(report, row); !ok || !p.Available {
				t.Fatalf("row %s = %+v (found %v), want lit by its stored key", row, p, ok)
			}
			if claw, _ := backendStatus(report, detect.BackendClaw); !claw.Available || claw.Auth != detect.AuthAPIKey {
				t.Errorf("claw = %+v, want available on api_key — claw drives every stored key", claw)
			}
			pi, _ := backendStatus(report, detect.BackendPi)
			if pi.Available != tc.pi {
				t.Errorf("pi available = %v, want %v — %s", pi.Available, tc.pi, tc.why)
			}
			if tc.pi && pi.Auth != detect.AuthAPIKey {
				t.Errorf("pi auth = %q, want %q", pi.Auth, detect.AuthAPIKey)
			}
			if cc, _ := backendStatus(report, detect.BackendClaudeCode); cc.Available != (tc.provider == secrets.ProviderAnthropic) {
				t.Errorf("claude_code available = %v on a %s key — it lights on an anthropic key or a forfait only", cc.Available, row)
			}
		})
	}
	for _, p := range cloudProviderSkeleton() {
		if !covered[p.Name] {
			t.Errorf("cloud row %q has no case here — state which backends a key of that provider lights", p.Name)
		}
	}
}

// The catalog's pi pairing asks the cloud question too: a team holding an
// openai key and an azure key has pi available (the openai key reaches it),
// yet an azure-served spec pairs with claw alone, since the runner hands pi
// no azure key. On a host, pi reads AZURE_OPENAI_API_KEY itself, so the local
// pairing keeps it.
func TestCloudPiPairingAsksWhatTheRunnerHandsPi(t *testing.T) {
	const azureSpec, openaiSpec = "foundry/gpt-5.5", "openai/gpt-5.5"
	t.Run("cloud", func(t *testing.T) {
		report := ReportFromCloudPresence(CloudPresence{ProviderSources: map[string]string{
			DetectProviderName(secrets.ProviderOpenAI): ProviderSourceLabel(secrets.ProviderOpenAI),
			DetectProviderName(secrets.ProviderAzure):  ProviderSourceLabel(secrets.ProviderAzure),
		}})
		cat := build(t, Options{
			Specs:             []string{azureSpec, openaiSpec},
			Report:            &report,
			Reachability:      ReachabilityCloud,
			UnprovenIsUnknown: true,
		})
		azure, ok := cat.Find(azureSpec)
		if !ok || !azure.Usable {
			t.Fatalf("%s = %+v (found %v), want usable through claw", azureSpec, azure, ok)
		}
		if slices.Contains(azure.Backends, detect.BackendPi) {
			t.Errorf("%s backends = %v — pi paired with a team azure key the runner never hands it", azureSpec, azure.Backends)
		}
		if !slices.Contains(azure.Backends, detect.BackendClaw) {
			t.Errorf("%s backends = %v, want claw", azureSpec, azure.Backends)
		}
		if openai, _ := cat.Find(openaiSpec); !slices.Contains(openai.Backends, detect.BackendPi) {
			t.Errorf("%s backends = %v, want pi — the runner hands pi the openai key", openaiSpec, openai.Backends)
		}
	})
	t.Run("local", func(t *testing.T) {
		report := hostReport(
			[]detect.ProviderStatus{{Name: "foundry", Available: true, Source: "AZURE_OPENAI_API_KEY"}},
			[]detect.BackendStatus{
				{Name: detect.BackendClaw, Available: true},
				{Name: detect.BackendPi, Available: true},
			},
		)
		cat := build(t, Options{Specs: []string{azureSpec}, Report: report})
		if azure, _ := cat.Find(azureSpec); !slices.Contains(azure.Backends, detect.BackendPi) {
			t.Errorf("%s backends = %v, want pi — on a host pi reads AZURE_OPENAI_API_KEY itself", azureSpec, azure.Backends)
		}
	})
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
	// The gateway's label is its key's variable name — the same string
	// detect's local gateway row reports.
	if got := DetectProviderName(secrets.ProviderOpenAICompatible); got != "openai_compatible" {
		t.Errorf("openai_compatible → %q", got)
	}
	if got := ProviderSourceLabel(secrets.ProviderOpenAICompatible); got != "OPENAI_COMPATIBLE_API_KEY" {
		t.Errorf("gateway source = %q, want OPENAI_COMPATIBLE_API_KEY", got)
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
