package modelroute

import "testing"

// A spec is split at its FIRST slash: the provider routes, the rest is the
// wire id verbatim — a model id may carry slashes of its own.
func TestParseSplitsAtTheFirstSlashAndKeepsTheRestVerbatim(t *testing.T) {
	cases := []struct {
		spec, provider, wire string
	}{
		{"anthropic/claude-opus-5-5", "anthropic", "claude-opus-5-5"},
		{"openai/meta-llama/Llama-3.3-70B", "openai", "meta-llama/Llama-3.3-70B"},
		{"openai/qwen/qwen-max", "openai", "qwen/qwen-max"},
		{"openai_compatible/scaleway/gpt-oss-120b", "openai_compatible", "scaleway/gpt-oss-120b"},
		{"openai_compatible/gpt-oss-120b", "openai_compatible", "gpt-oss-120b"},
		{"moonshot/kimi-code/kimi-for-coding", "moonshot", "kimi-code/kimi-for-coding"},
		{"claude-opus-5-5", "", "claude-opus-5-5"},
		{"", "", ""},
		{"/claude-opus-5-5", "", "/claude-opus-5-5"},
		{"anthropic/", "", "anthropic/"},
	}
	for _, c := range cases {
		got := Parse(c.spec)
		if got.Spec != c.spec || got.Provider != c.provider || got.Wire != c.wire {
			t.Errorf("Parse(%q) = %+v, want provider %q wire %q", c.spec, got, c.provider, c.wire)
		}
	}
}

// A gateway id is never looked up in a vendor's capability tables: its
// capability id is empty, while a vendor route's is its wire id.
func TestCapabilityIDIsEmptyForTheGatewayOnly(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-opus-5-5":          "claude-opus-5-5",
		"openai/gpt-5.5":                     "gpt-5.5",
		"claude-opus-5-5":                    "claude-opus-5-5",
		"openai_compatible/gpt-5-mini":       "",
		"openai_compatible/scaleway/o3-mini": "",
		"openai_compatible/claude-opus-4-8":  "",
	}
	for spec, want := range cases {
		if got := Parse(spec).CapabilityID(); got != want {
			t.Errorf("Parse(%q).CapabilityID() = %q, want %q", spec, got, want)
		}
	}
}

func TestGatewayIsTheOpenAICompatiblePrefixOnly(t *testing.T) {
	for spec, want := range map[string]bool{
		"openai_compatible/gpt-oss-120b": true,
		"openai/gpt-oss-120b":            false,
		"gpt-oss-120b":                   false,
		"scaleway/gpt-oss-120b":          false,
	} {
		if got := Parse(spec).Gateway(); got != want {
			t.Errorf("Parse(%q).Gateway() = %v, want %v", spec, got, want)
		}
	}
}

func TestIsRoutingProvider(t *testing.T) {
	for p, want := range map[string]bool{
		"anthropic":         true,
		"openai":            true,
		"moonshot":          true,
		"openai_compatible": true,
		"scaleway":          false,
		"meta-llama":        false,
		"":                  false,
	} {
		if got := IsRoutingProvider(p); got != want {
			t.Errorf("IsRoutingProvider(%q) = %v, want %v", p, got, want)
		}
	}
}
