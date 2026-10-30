package model

import (
	"context"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// clawSandboxCredentialEnvKeys are the entries of the env
// forwardableProviderEnv hands a sandboxed claw node that authenticate it:
// the provider keys, the bearer, the gateway key, and the two secret-FILE
// channels (CODEX_HOME / CLAUDE_CONFIG_DIR point at a forfait the runtime
// seeded into the container). Endpoint and routing names
// (providerRoutingEnvVars) never authenticate by themselves and are not
// here.
var clawSandboxCredentialEnvKeys = []string{
	"OPENAI_API_KEY",
	"AZURE_OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ZAI_API_KEY",
	"MOONSHOT_API_KEY",
	"XAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GROQ_API_KEY",
	"DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY",
	"AWS_ACCESS_KEY_ID",
	compatgw.APIKeyEnv,
	"CODEX_HOME",
	"CLAUDE_CONFIG_DIR",
}

// ClawSandboxCredential is the #1768 predicate for the claw sandboxed
// runner: will a credential reach the node's process — answered from what
// the dispatch actually hands the container, not assumed:
//
//  1. the env forwardableProviderEnv composes for THIS node's model (the
//     launcher's forwarded provider keys, the run's BYOK/pinned keys, the
//     gateway key, the forfait file channels) — the exec-time channel a
//     spec-level check cannot see;
//  2. the host's ChatGPT forfait, which deliverHostCodexForfait copies into
//     the container at dispatch — hostCodexForfait is the real read,
//     switch and planted-home refusals included;
//  3. the operator's file-based ChatGPT login (~/.codex/auth.json), which
//     reaches the container ONLY through the host_state mount — the
//     channel a host_state: none run (and the fork lane's floor) withdraws.
//
// A gateway-env misconfiguration (err) answers "no channel": the node
// would fail at dispatch on that error anyway, and a refusal naming the
// credential gap is the more honest failure for an untrusted run.
func ClawSandboxCredential(ctx context.Context, model string, home delegate.SandboxHomeState) (channel string, ok bool) {
	env, _, err := forwardableProviderEnv(ctx, model)
	if err == nil {
		for _, key := range clawSandboxCredentialEnvKeys {
			if env[key] != "" {
				return "the env the dispatch forwards into the container (" + key + ")", true
			}
		}
	}
	if payload, _ := hostCodexForfait(ctx, model); payload != nil {
		return "the host's ChatGPT forfait (copied into the sandbox at dispatch)", true
	}
	if home.Mounted && home.HomeDir != "" {
		if _, err := os.Stat(filepath.Join(home.HomeDir, ".codex", "auth.json")); err == nil {
			return "the host_state mount of the operator's login (~/.codex)", true
		}
	}
	return "", false
}
