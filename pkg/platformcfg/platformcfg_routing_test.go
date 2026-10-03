package platformcfg

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The platform record's Routing field is the platform level of the
// adaptive-routing policy. nil = the level says nothing (the env dials and
// the built-in defaults answer); set, every value must be one the fold can
// resolve — a value the record carries but the fold cannot read would
// decide the opposite of what the operator wrote.
func TestPlatformCredentials_RoutingValidate(t *testing.T) {
	err := (PlatformCredentials{Routing: &llmroute.Policy{
		PairOrder: []string{"claude_code"},
	}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "not harness+credential") {
		t.Fatalf("Validate = %v, want the pair-spelling refusal", err)
	}

	err = (PlatformCredentials{Routing: &llmroute.Policy{
		PairOrder: []string{"claude_code+harness_typo"},
	}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown credential") {
		t.Fatalf("Validate = %v, want the unknown-credential refusal", err)
	}

	err = (PlatformCredentials{Routing: &llmroute.Policy{
		Triggers: []string{"budget"},
	}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("Validate = %v, want the closed-vocabulary refusal", err)
	}

	if err := (PlatformCredentials{Routing: &llmroute.Policy{
		RefusedPinnedKey: llmroute.RefusedPinnedPark,
	}}).Validate(); err != nil {
		t.Fatalf("Validate(park): %v", err)
	}
	if err := (PlatformCredentials{}.Validate()); err != nil {
		t.Fatalf("Validate(nil routing): %v", err)
	}
}

// The reader's precedence: the record's routing block, then the env dial,
// then the arbitrated default. Both must be readable from the snapshot's
// answer — an env-configured deployment and a record-configured one get
// different provenance for the same value.
func TestPlatformCredentials_RefusedPinnedPrecedence(t *testing.T) {
	t.Setenv(llmroute.EnvRefusedPinnedKey, llmroute.RefusedPinnedPark)
	if got := (&PlatformCredentials{}).RefusedPinned(); got != llmroute.RefusedPinnedPark {
		t.Fatalf("env dial ignored: %q, want park", got)
	}
	rec := &PlatformCredentials{Routing: &llmroute.Policy{RefusedPinnedKey: llmroute.RefusedPinnedForfait}}
	if got := rec.RefusedPinned(); got != llmroute.RefusedPinnedForfait {
		t.Fatalf("record outranked by env: %q, want the record's forfait", got)
	}
	t.Setenv(llmroute.EnvRefusedPinnedKey, "")
	if got := (&PlatformCredentials{}).RefusedPinned(); got != llmroute.RefusedPinnedForfait {
		t.Fatalf("default lost: %q, want the arbitrated forfait", got)
	}
	// A nil record reads like an absent one.
	var nilRec *PlatformCredentials
	if got := nilRec.RefusedPinned(); got != llmroute.RefusedPinnedForfait {
		t.Fatalf("nil record: %q, want forfait", got)
	}
}

// The env dials are the platform record's deployment defaults
// (ITERION_PLATFORM_*): a value the policy cannot read refuses the boot —
// the same discipline as an unreadable facade_default.
func TestValidateEnv_RoutingDials(t *testing.T) {
	t.Setenv(llmroute.EnvPairOrder, "claude_code+claude_forfait_typo")
	if err := ValidateEnv(); err == nil || !strings.Contains(err.Error(), llmroute.EnvPairOrder) {
		t.Fatalf("ValidateEnv = %v, want a refusal naming %s", err, llmroute.EnvPairOrder)
	}

	t.Setenv(llmroute.EnvPairOrder, "")
	t.Setenv(llmroute.EnvTriggers, "budget")
	if err := ValidateEnv(); err == nil || !strings.Contains(err.Error(), llmroute.EnvTriggers) {
		t.Fatalf("ValidateEnv = %v, want a refusal named for %s", err, llmroute.EnvTriggers)
	}

	for _, e := range []string{llmroute.EnvPairOrder, llmroute.EnvTriggers, llmroute.EnvRefusedPinnedKey, llmroute.EnvStrict} {
		t.Setenv(e, "")
	}
	if err := ValidateEnv(); err != nil {
		t.Fatalf("ValidateEnv clean env: %v", err)
	}
}
