package delegate

import (
	"maps"
	"os"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

var claudeModelDefaultKeys = []string{
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// Apply after the SDK has resolved credentials and per-task overrides, at the
// actual spawn boundary shared by host/container and work/format passes. Merely
// selecting --model leaves the CLI's auxiliary calls on its built-in defaults.
// Explicit env settings (including an empty suppression) keep their precedence.
func claudeModelDefaultEnv(env map[string]string) map[string]string {
	lookup := func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return os.Getenv(key)
	}
	for _, key := range []string{FacadeSlotEnvKey, ForfaitSuppressedEnvKey, "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if lookup(key) != "" {
			return env
		}
	}
	// Gateways and cloud providers require their own deployment/model IDs.
	if !secrets.AnthropicForfaitWireOK(lookup("ANTHROPIC_BASE_URL")) {
		return env
	}
	out := maps.Clone(env)
	if out == nil {
		out = make(map[string]string)
	}
	for _, key := range claudeModelDefaultKeys {
		if _, ok := out[key]; ok {
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			out[key] = value
		} else {
			out[key] = defaultClaudeCodeModel
		}
	}
	return out
}
