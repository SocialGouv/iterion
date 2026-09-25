package delegate

import (
	"os"
	"reflect"
	"testing"
)

func TestClaudeAuxiliaryModelDefaults(t *testing.T) {
	// An unset value differs from an explicit empty override.
	keys := append(append([]string{}, claudeModelDefaultKeys...), FacadeSlotEnvKey, ForfaitSuppressedEnvKey, "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_BASE_URL")
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	got := claudeModelDefaultEnv(nil)
	for _, key := range claudeModelDefaultKeys {
		if got[key] != "claude-opus-5-5" {
			t.Errorf("%s=%q", key, got[key])
		}
	}
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "operator-haiku")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "")
	input := map[string]string{"ANTHROPIC_DEFAULT_OPUS_MODEL": "explicit-opus", "ANTHROPIC_DEFAULT_SONNET_MODEL": ""}
	want := map[string]string{"ANTHROPIC_DEFAULT_OPUS_MODEL": "explicit-opus", "ANTHROPIC_DEFAULT_SONNET_MODEL": ""}
	got = claudeModelDefaultEnv(input)
	if got["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "operator-haiku" || got["CLAUDE_CODE_SUBAGENT_MODEL"] != "" || got["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "explicit-opus" || got["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "" {
		t.Fatalf("overrides lost: %v", got)
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("mutated caller: %v", input)
	}
	for _, route := range []map[string]string{
		{FacadeSlotEnvKey: "moonshot"}, {ForfaitSuppressedEnvKey: "1"},
		{"ANTHROPIC_BASE_URL": "https://gateway.example/api"},
		{"CLAUDE_CODE_USE_BEDROCK": "1"}, {"CLAUDE_CODE_USE_VERTEX": "1"}, {"CLAUDE_CODE_USE_FOUNDRY": "1"},
	} {
		if v := claudeModelDefaultEnv(route); !reflect.DeepEqual(v, route) {
			t.Errorf("changed non-Anthropic route: %v", v)
		}
	}
}

func TestCodexDefaultAndExplicitModels(t *testing.T) {
	for in, want := range map[string]string{"": "gpt-6-sol", "openai/gpt-6-astra": "gpt-6-astra", "openai/gpt-5.5": "gpt-5.5", "operator-model": "operator-model"} {
		if got := codexTaskModel(in); got != want {
			t.Errorf("model %q => %q, want %q", in, got, want)
		}
	}
}
