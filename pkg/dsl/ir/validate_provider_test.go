package ir

import (
	"strings"
	"testing"
)

// providerSrc builds a minimal one-agent workflow with the given backend
// and provider field so the provider validator can be exercised in
// isolation.
func providerSrc(backend, provider string) string {
	return `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "gpt-4"
  backend: "` + backend + `"
  provider: "` + provider + `"
  system: sys
  output: empty

workflow w:
  entry: writer
  writer -> done
`
}

func TestProvider_UnknownTokenWarns(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "anthropic,zaai"))
	expectDiag(t, r, DiagUnknownProvider)
}

func TestProvider_KnownChainNoWarning(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "anthropic,zai,moonshot,openai,auto"))
	expectNoDiag(t, r, DiagUnknownProvider)
}

// The runtime routes on a FOLDED hint (delegate.normalizeProviderHint), so the
// diagnostic must accept the spellings it honours: warning that "Moonshot will
// be ignored" is false the moment the delegate routes it, and it reads as an
// instruction to delete a working route.
func TestProvider_KnownProviderIsAcceptedWhateverItsCase(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "Anthropic,ZAI,Moonshot,AUTO"))
	expectNoDiag(t, r, DiagUnknownProvider)
}

// The C087 message must name every hint the compiler accepts. A list that
// lags the set tells an author a valid hint is a typo — and the fix they then
// apply is to remove a working route.
func TestProvider_UnknownTokenMessageListsEveryKnownProvider(t *testing.T) {
	list := KnownProviderList()
	for hint := range KnownProviders {
		if !strings.Contains(list, hint) {
			t.Errorf("KnownProviderList() = %q, missing the accepted hint %q", list, hint)
		}
	}
}

// Env-ref forms resolve at run time, so the validator must not try to
// validate their literal text (and not misfire on a typo it can't yet see).
func TestProvider_EnvRefSkipsValidation(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "${RESCUE_PROVIDER:-zai},anthropic"))
	expectNoDiag(t, r, DiagUnknownProvider)
}

func TestProvider_ChainIgnoredOnClawWarns(t *testing.T) {
	r := compileFile(t, providerSrc("claw", "anthropic,zai"))
	expectDiag(t, r, DiagProviderChainIgnored)
}

func TestProvider_ChainIgnoredOnCodexWarns(t *testing.T) {
	r := compileFile(t, providerSrc("codex", "anthropic,zai"))
	expectDiag(t, r, DiagProviderChainIgnored)
}

// claude_code consumes the hint, so a chain there is meaningful — no C088.
func TestProvider_ChainOnClaudeCodeNoWarning(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "anthropic,zai"))
	expectNoDiag(t, r, DiagProviderChainIgnored)
}

// A single-value provider on claw is not a chain, so C088 must not fire.
func TestProvider_SingleValueOnClawNoWarning(t *testing.T) {
	r := compileFile(t, providerSrc("claw", "zai"))
	expectNoDiag(t, r, DiagProviderChainIgnored)
}

// Per-element `provider:model` form is valid: only the provider part is
// checked against KnownProviders, so a known provider with a model emits
// no C087 and no C172.
func TestProvider_PerElementModelNoWarning(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "zai:glm-5.2,anthropic:claude-opus-4-8"))
	expectNoDiag(t, r, DiagUnknownProvider)
	expectNoDiag(t, r, DiagMalformedProviderStep)
}

// The provider part of a `provider:model` token is still validated: a typo
// in the provider half warns C087.
func TestProvider_PerElementUnknownProviderWarns(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "zaai:glm-5.2,anthropic:claude-opus-4-8"))
	expectDiag(t, r, DiagUnknownProvider)
}

// A malformed element — colon present but an empty provider or model part —
// warns C172.
func TestProvider_MalformedStepEmptyModelWarns(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "zai:,anthropic"))
	expectDiag(t, r, DiagMalformedProviderStep)
}

func TestProvider_MalformedStepEmptyProviderWarns(t *testing.T) {
	r := compileFile(t, providerSrc("claude_code", "anthropic,:glm-5.2"))
	expectDiag(t, r, DiagMalformedProviderStep)
}
