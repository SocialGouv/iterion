package modelcatalog

import (
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
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
	case secrets.ProviderMoonshot:
		return "MOONSHOT_API_KEY"
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
	case secrets.ProviderOpenAICompatible:
		// The key's variable, as detect's local gateway row reports it and
		// as a sandboxed claw node receives the run's gateway key.
		return compatgw.APIKeyEnv
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
	// piProvider is set only by a row whose key the cloud runner hands pi —
	// the piSpends question the catalog's pi pairing asks on this surface
	// too, so one catalog's backend list and its model pairings agree. A
	// team's gateway key unlocks claw alone (ADR-131 §9), and so does an
	// azure, bedrock or vertex key: the runner forwards pi none of them.
	piProvider := false
	for _, prov := range providers {
		if !prov.Available {
			continue
		}
		anyProvider = true
		if piSpends(ReachabilityCloud, prov.Name) {
			piProvider = true
		}
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
		claw.Auth = detect.AuthOAuth
		if anyProvider {
			claw.Auth = detect.AuthAPIKey
		}
	}
	if piProvider || p.ClaudeCodeOAuth {
		pi.Available = true
		pi.Auth = detect.AuthOAuth
		if piProvider {
			pi.Auth = detect.AuthAPIKey
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

// piSpends answers, on one credential surface, whether pi can spend the
// credential a provider row stands for. On a host pi reads its own variables
// (detect.PiReadsProvider, the question detect's local probe asks); a cloud
// run hands pi only the keys the runner forwards it
// (delegate.PiForwardsRunKey), so a row pi would read on a host — azure's —
// funds no pi call in the cloud.
func piSpends(surface, row string) bool {
	if surface == ReachabilityCloud {
		return delegate.PiForwardsRunKey(keyProviderOfRow(row))
	}
	return detect.PiReadsProvider(row)
}

// keyProviderOfRow is DetectProviderName read backwards: the stored key's
// provider a cloud row stands for.
func keyProviderOfRow(row string) secrets.Provider {
	if row == DetectProviderName(secrets.ProviderAzure) {
		return secrets.ProviderAzure
	}
	return secrets.Provider(row)
}

// cloudProviderSkeleton lists the provider rows a cloud presence can light,
// each named as DetectProviderName names a stored key's provider.
//
// The gateway row carries no SuggestedModel, the rule detect's local
// gateway row states: the model id is the deployment's own naming, so
// nothing here can guess one, and an empty suggestion keeps the row out of
// every "pick a default model" consumer. A team's default gateway model is
// a routing decision — an override cell of its routing policy — never a
// suggestion of this row.
func cloudProviderSkeleton() []detect.ProviderStatus {
	return []detect.ProviderStatus{
		{Name: "anthropic", Source: "ANTHROPIC_API_KEY", SuggestedModel: "anthropic/claude-opus-5-5"},
		{Name: "zai", Source: "ZAI_API_KEY", SuggestedModel: "anthropic/glm-5.3"},
		{Name: "moonshot", Source: "MOONSHOT_API_KEY", SuggestedModel: "moonshot/kimi-k2"},
		{Name: "openai", Source: "OPENAI_API_KEY", SuggestedModel: "openai/gpt-6-sol"},
		{Name: "xai", Source: "XAI_API_KEY", SuggestedModel: "xai/grok-3"},
		{Name: "foundry", Source: "AZURE_OPENAI_API_KEY"},
		{Name: "bedrock", Source: "AWS_REGION"},
		{Name: "vertex", Source: "GOOGLE_CLOUD_PROJECT"},
		{Name: "openrouter", Source: "OPENROUTER_API_KEY"},
		{Name: modelroute.OpenAICompatible, Source: compatgw.APIKeyEnv},
	}
}
