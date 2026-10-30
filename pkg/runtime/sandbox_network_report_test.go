package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The fail-closed rule expansion (#2099): a reference that resolves to
// nothing refuses ITS RULE, not merely when the whole rule comes out empty.
// "api.${REGION}.example.com" with REGION unset expands to a pattern no host
// matches — silently dropped, which in denylist mode (and for an allowlist's
// `!` exclusions) fails OPEN: the denied host allowed.
func TestResolveNetworkPolicy_RefusesARuleWhoseReferenceDies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  sandbox.NetworkMode
		rules []string
	}{
		{name: "denylist, literal around the reference", mode: sandbox.NetworkModeDenylist, rules: []string{"api.${TEST_UNSET_NEVER_SET}.example.com"}},
		{name: "allowlist exclusion around the reference", mode: sandbox.NetworkModeAllowlist, rules: []string{"**.corp.example", "!metadata.${TEST_UNSET_NEVER_SET}.corp.example"}},
		{name: "bare reference still refuses", mode: sandbox.NetworkModeDenylist, rules: []string{"${TEST_UNSET_NEVER_SET}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &sandbox.Spec{Network: &sandbox.Network{Mode: tc.mode, Rules: tc.rules}}
			_, rules, err := ResolveNetworkPolicy(spec)
			if err == nil {
				t.Fatalf("ResolveNetworkPolicy = %v, want a refusal — a rule whose reference resolves to nothing matches no host, a dropped rule that fails open", rules)
			}
			// The bare reference refused before this fix; the guard this
			// test adds is the two MIXED rules — assert only that every
			// case still refuses.
			_ = err
		})
	}
}

// A rule every reference explicitly defaults is the author's "no rule when
// unset" — dropped, not refused: the one greppable opt-out.
func TestResolveNetworkPolicy_ExplicitEmptyDefaultIsNoRule(t *testing.T) {
	spec := &sandbox.Spec{Network: &sandbox.Network{
		Mode:  sandbox.NetworkModeAllowlist,
		Rules: []string{"${TEST_UNSET_EGRESS:-}", "**.anthropic.com"},
	}}
	_, rules, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v — an explicitly defaulted rule is the author's opt-out, not a mistake to refuse", err)
	}
	if len(rules) != 1 || rules[0] != "**.anthropic.com" {
		t.Errorf("rules = %v, want the optional rule dropped and the literal kept", rules)
	}
}

// A defaulted reference with a real default fills the rule; the rule is
// kept expanded.
func TestResolveNetworkPolicy_DefaultedReferenceFillsTheRule(t *testing.T) {
	spec := &sandbox.Spec{Network: &sandbox.Network{
		Mode:  sandbox.NetworkModeDenylist,
		Rules: []string{"api.${TEST_UNSET_REGION:-us}.example.com"},
	}}
	_, rules, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v", err)
	}
	if len(rules) != 1 || rules[0] != "api.us.example.com" {
		t.Errorf("rules = %v, want [api.us.example.com]", rules)
	}
}
