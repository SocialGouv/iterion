package runtime

import (
	"testing"
)

func TestSelectProviderXAI(t *testing.T) {
	p := SelectProvider("xai")
	if p == nil {
		t.Fatal("SelectProvider(\"xai\") returned nil")
	}
	if got := p.Name(); got != "openai" {
		t.Errorf("SelectProvider(\"xai\").Name() = %q, want \"openai\"", got)
	}
}

func TestSelectProviderDashScope(t *testing.T) {
	p := SelectProvider("dashscope")
	if p == nil {
		t.Fatal("SelectProvider(\"dashscope\") returned nil")
	}
	if got := p.Name(); got != "openai" {
		t.Errorf("SelectProvider(\"dashscope\").Name() = %q, want \"openai\"", got)
	}
}

func TestSelectProviderOpenAI(t *testing.T) {
	p := SelectProvider("openai")
	if p == nil {
		t.Fatal("SelectProvider(\"openai\") returned nil")
	}
	if got := p.Name(); got != "openai" {
		t.Errorf("SelectProvider(\"openai\").Name() = %q, want \"openai\"", got)
	}
}

func TestSelectProviderAnthropic(t *testing.T) {
	p := SelectProvider("anthropic")
	if p == nil {
		t.Fatal("SelectProvider(\"anthropic\") returned nil")
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("SelectProvider(\"anthropic\").Name() = %q, want \"anthropic\"", got)
	}
}

// Moonshot must select its OWN provider, not fall through to the anthropic
// default: the default answers on api.anthropic.com, so a route named
// "moonshot" would silently spend an Anthropic credential.
func TestSelectProviderMoonshot(t *testing.T) {
	p := SelectProvider("moonshot")
	if p == nil {
		t.Fatal("SelectProvider(\"moonshot\") returned nil")
	}
	if got := p.Name(); got != "moonshot" {
		t.Errorf("SelectProvider(\"moonshot\").Name() = %q, want \"moonshot\"", got)
	}
}

func TestSelectProviderDefaultIsAnthropic(t *testing.T) {
	p := SelectProvider("unknown-provider")
	if p == nil {
		t.Fatal("SelectProvider(\"unknown-provider\") returned nil")
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("SelectProvider(\"unknown-provider\").Name() = %q, want \"anthropic\" (default)", got)
	}
}
