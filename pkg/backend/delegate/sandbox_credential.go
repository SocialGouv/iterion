package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SandboxHomeState is what the run's sandbox will actually bind of the
// operator's home (credroute.SandboxHomeState, where ADR-161's resolver keeps
// it). The probes below take it as an input rather than re-deriving it so the
// admission and the sandbox start read the SAME resolution (#1768).
type SandboxHomeState = credroute.SandboxHomeState

// claudeSandboxCredentialEnvKeys are the env entries of a claude_code spawn
// that authenticate it by themselves: a key, a bearer, the OAuth token, or
// the config dir of a forfait the runtime seeds into the container
// (secrets.ClaudeCodeSandboxConfigDir — the file channel). An EMPTY entry
// is a suppression, not a credential, and never counts.
var claudeSandboxCredentialEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CONFIG_DIR",
}

// credEnvChannel names the first credential-bearing entry of env, or "".
func credEnvChannel(env map[string]string, keys []string) string {
	for _, k := range keys {
		if env[k] != "" {
			return k
		}
	}
	return ""
}

// liveCredentialVariable names the variable a live credential set
// (ambientCredentialLive) authenticates through: its first token channel in
// claudeSandboxCredentialEnvKeys' order, else the custom-header block — the
// only other thing such a set holds — else "" for an empty set.
func liveCredentialVariable(live map[string]string) string {
	if key := credEnvChannel(live, claudeSandboxCredentialEnvKeys); key != "" {
		return key
	}
	if len(live) > 0 {
		return "ANTHROPIC_CUSTOM_HEADERS"
	}
	return ""
}

// ClaudeCodeSandboxCredential is the per-backend predicate of #1768 for
// claude_code: will a credential reach the process of a SANDBOXED
// claude_code node — answered from what the spawn actually composes, not
// assumed. The channels, in the order the spawn's own precedence takes
// them:
//
//  1. the run's resolved credentials (selectedAnthropicCredEnvForCLI with
//     sandboxed=true): a BYOK/pinned Anthropic key, a facade key of the
//     node's provider hint, or the run's Claude forfait (CLAUDE_CONFIG_DIR
//     pointed at the in-container forfait dir + CLAUDE_CODE_OAUTH_TOKEN);
//  2. the ambient Anthropic env of the LAUNCHER, forwarded by name into
//     the container (ambientAnthropicEnvForSandbox — the exec-time channel
//     a spec-level check cannot see, and the reason this is a probe of the
//     real spawn);
//  3. the operator's file-based login (`claude login`), which reaches the
//     container ONLY through the host_state mount of ~/.claude — the
//     channel a host_state: none run (and the fork lane's floor) withdraws.
//
// The returned channel names what carries the credential, for the refusal's
// message; ok is false when NO channel reaches, which for an untrusted run
// is certain to fail at the node and must refuse the launch instead.
func ClaudeCodeSandboxCredential(ctx context.Context, providerHint string, home SandboxHomeState) (channel string, ok bool) {
	// A hint chain ("anthropic,zai,…") resolves its first leg at spawn; the
	// probe reads the same FIRST leg, and a later leg's credential still
	// reaches through channel 2's ambient forwarding.
	hint := normalizeProviderHint(strings.TrimSpace(strings.Split(providerHint, ",")[0]))
	selected, inheritAmbient := selectedAnthropicCredEnvForCLI(ctx, hint, true)
	if key := credEnvChannel(selected, claudeSandboxCredentialEnvKeys); key != "" {
		return "the run's resolved credential (" + key + ")", true
	}
	if inheritAmbient {
		// The forwarded layer authenticates the spawn with the set the ambient
		// meter keys (ambientCredentialLive): every token channel, with the
		// custom-header block's credential lines applied over them. A
		// header-only pod is named, not refused as credential-less; a block of
		// other header names carries nothing the CLI authenticates with, and a
		// line whose empty value erases the only token leaves nothing either —
		// naming any of those would admit a launch that dies "Not logged in".
		// sandboxed=true: the answer is the forwarded map alone — the
		// container's env, no host fallback (spawnEnvValue).
		if channel := liveCredentialVariable(ambientCredentialLive(ambientAnthropicEnvForSandbox(), true)); channel != "" {
			return "the launcher env forwarded into the container (" + channel + ")", true
		}
	}
	if home.Mounted && home.HomeDir != "" {
		if _, err := os.Stat(filepath.Join(home.HomeDir, ".claude", ".credentials.json")); err == nil {
			return "the host_state mount of the operator's login (~/.claude)", true
		}
	}
	return "", false
}

// piSandboxCredentialSecretNames is the credential subset of
// piCredentialEnvNames — derived from it, never a second list: a name is a
// credential unless it is an endpoint/route (piSandboxEnv forwards every
// name, so the probe reads exactly what the spawn receives).
func piSandboxCredentialSecretNames() []string {
	out := make([]string, 0, len(piCredentialEnvNames))
	for _, name := range piCredentialEnvNames {
		if strings.HasSuffix(name, "_BASE_URL") || strings.HasSuffix(name, "_ENDPOINT") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// PiSandboxCredential is the #1768 predicate for pi: will a credential
// reach a sandboxed pi node's process. Channels, read from the spawn's own
// inputs:
//
//  1. the credential names piSandboxEnv forwards from the launcher's env
//     (the allowlist the in-container CLI resolves from — a host key is
//     invisible unless forwarded by name);
//  2. the run's BYOK keys piResolveEnv layers from the ctx credentials;
//  3. the ChatGPT-forfait bridge (piLoadCodexView): an `openai-codex/…`
//     node's credential is seeded into the sandbox over the state mirror —
//     it works under host_state: none too, so it is read for real, not
//     inferred from the mount.
//
// pi takes no SandboxHomeState: its file-based channel (the bridge) crosses
// the boundary without the home bind, unlike claude_code's ~/.claude mount.
func PiSandboxCredential(ctx context.Context, model string, trust store.RunTrust) (channel string, ok bool) {
	for _, name := range piSandboxCredentialSecretNames() {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return "the launcher env forwarded into the container (" + name + ")", true
		}
	}
	for key, v := range piResolveEnv(ctx) {
		if v != "" {
			return "the run's resolved credential (" + key + ")", true
		}
	}
	if provider := strings.ToLower(strings.TrimSpace(strings.Split(model, "/")[0])); provider == piCodexProvider {
		if view, err := piLoadCodexView(ctx, trust); err == nil && view.IsChatGPTMode() {
			return "the ChatGPT-forfait bridge (the codex credential seeded into the sandbox)", true
		}
	}
	return "", false
}
