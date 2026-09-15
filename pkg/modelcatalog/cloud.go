package modelcatalog

import (
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// unknownCloudReason is the operator-facing explanation for a model the
// cloud launch tiers did not prove. It must never read as "this host has
// no credential" — that sentence is what made the picker lie when the
// control-plane env and the run bundle disagreed.
const unknownCloudReason = "cloud run credentials are not proven for this model — a pool grant or runner fallback may still serve it"

// CloudPresence is the launch-tier credential surface for one tenant:
// which providers and OAuth kinds a cloud run would receive. Names and
// kinds only — never a credential value.
//
// Tiers match cloudpublisher.resolveAndSealCredentials (BYOK, user/org
// OAuth, platform). The mutualised pool is deliberately absent: a grant
// is opportunistic and cannot be proven without acquiring one.
type CloudPresence struct {
	// ProviderSources maps a detect provider name ("anthropic", "openai",
	// "foundry", …) to the credential *source label* the catalog may
	// echo (an env-var name, never a secret).
	ProviderSources map[string]string
	// ClaudeCodeOAuth is true when a claude_code forfait blob would be
	// injected (user, org, or platform). That unlocks the claude_code
	// backend even with no Anthropic API key.
	ClaudeCodeOAuth bool
	// CodexOAuth is true when a ChatGPT-forfait blob would be injected.
	// That unlocks the OpenAI provider the same way a host Codex login does.
	CodexOAuth bool
}

// DetectProviderName maps a secrets.Provider onto the detect.Report
// provider name availability() looks up. Azure BYOK is the one rename:
// detect calls that probe "foundry".
func DetectProviderName(p secrets.Provider) string {
	if p == secrets.ProviderAzure {
		return "foundry"
	}
	return string(p)
}

// ProviderSourceLabel is the env-var name a run would see for this
// provider — the same strings detect.Report.Source uses locally. Never a
// value, never a last4.
func ProviderSourceLabel(p secrets.Provider) string {
	switch p {
	case secrets.ProviderAnthropic:
		return "ANTHROPIC_API_KEY"
	case secrets.ProviderOpenAI:
		return "OPENAI_API_KEY"
	case secrets.ProviderZAI:
		return "ZAI_API_KEY"
	case secrets.ProviderXAI:
		return "XAI_API_KEY"
	case secrets.ProviderAzure:
		return "AZURE_OPENAI_API_KEY"
	case secrets.ProviderOpenRouter:
		return "OPENROUTER_API_KEY"
	case secrets.ProviderBedrock:
		return "AWS_REGION"
	case secrets.ProviderVertex:
		return "GOOGLE_CLOUD_PROJECT"
	default:
		return string(p)
	}
}

// ReportFromCloudPresence builds the detect.Report modelcatalog.Build
// consumes, from launch-tier *presence* instead of the server process
// environment. Backends are those a cloud runner can drive given that
// presence — never whether the control-plane host has a `claude` binary.
func ReportFromCloudPresence(p CloudPresence) detect.Report {
	providers := cloudProviderSkeleton()
	for i := range providers {
		if src, ok := p.ProviderSources[providers[i].Name]; ok && src != "" {
			providers[i].Available = true
			providers[i].Source = src
		}
	}
	if p.CodexOAuth {
		for i := range providers {
			if providers[i].Name != "openai" || providers[i].Available {
				continue
			}
			providers[i].Available = true
			providers[i].Source = "ChatGPT-OAuth"
		}
	}
	if p.ClaudeCodeOAuth {
		for i := range providers {
			if providers[i].Name != "anthropic" || providers[i].Available {
				continue
			}
			providers[i].Available = true
			providers[i].Source = "claude_code oauth"
		}
	}

	anthropicKey := false
	anyProvider := false
	for _, prov := range providers {
		if !prov.Available {
			continue
		}
		anyProvider = true
		if prov.Name == "anthropic" {
			anthropicKey = true
		}
	}

	claudeCode := detect.BackendStatus{
		Name: detect.BackendClaudeCode,
		Auth: detect.AuthNone,
	}
	if p.ClaudeCodeOAuth || anthropicKey {
		claudeCode.Available = true
		if p.ClaudeCodeOAuth {
			claudeCode.Auth = detect.AuthOAuth
			claudeCode.Sources = []string{"claude_code oauth"}
		} else {
			claudeCode.Auth = detect.AuthAPIKey
			claudeCode.Sources = []string{"ANTHROPIC_API_KEY"}
		}
	}

	claw := detect.BackendStatus{
		Name: detect.BackendClaw,
		Auth: detect.AuthNone,
	}
	pi := detect.BackendStatus{
		Name: detect.BackendPi,
		Auth: detect.AuthNone,
	}
	if anyProvider || p.ClaudeCodeOAuth {
		claw.Available = true
		pi.Available = true
		if anyProvider {
			claw.Auth = detect.AuthAPIKey
			pi.Auth = detect.AuthAPIKey
		} else {
			claw.Auth = detect.AuthOAuth
			pi.Auth = detect.AuthOAuth
		}
	}

	backends := []detect.BackendStatus{claudeCode, claw, pi}
	pref := detect.DefaultPreferenceOrder
	return detect.Report{
		PreferenceOrder: pref,
		ResolvedDefault: detect.Resolve(pref, backends),
		Backends:        backends,
		Providers:       providers,
	}
}

func cloudProviderSkeleton() []detect.ProviderStatus {
	return []detect.ProviderStatus{
		{Name: "anthropic", Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5"},
		{Name: "zai", Source: "ZAI_API_KEY", SuggestedModel: "anthropic/glm-5.2"},
		{Name: "openai", Source: "OPENAI_API_KEY", SuggestedModel: "openai/gpt-5.4-mini"},
		{Name: "xai", Source: "XAI_API_KEY", SuggestedModel: "xai/grok-3"},
		{Name: "foundry", Source: "AZURE_OPENAI_API_KEY"},
		{Name: "bedrock", Source: "AWS_REGION"},
		{Name: "vertex", Source: "GOOGLE_CLOUD_PROJECT"},
		{Name: "openrouter", Source: "OPENROUTER_API_KEY"},
	}
}
