package secrets

import "testing"

// A pinned key must be redacted from output like any other credential: the
// guard seeds itself from this list, and a value missing from it appears in
// node output and logs in clear.
func TestEveryKeyForRedaction_CoversBothChannels(t *testing.T) {
	c := Credentials{
		APIKeys:       map[Provider]string{ProviderAnthropic: "sk-own"},
		PinnedAPIKeys: map[Provider]string{ProviderMoonshot: "sk-pinned"},
	}
	got := c.EveryKeyForRedaction()
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	for _, want := range []string{"sk-own", "sk-pinned"} {
		if !seen[want] {
			t.Errorf("EveryKeyForRedaction() = %v, missing %q — it would appear unredacted in output", got, want)
		}
	}
}

// APIKeyForRoute answers for a NAMED provider, so it reads both channels —
// and the run's own key outranks the shared tier's.
func TestAPIKeyForRoute_OwnKeyFirstThenPinned(t *testing.T) {
	c := Credentials{
		APIKeys:       map[Provider]string{ProviderMoonshot: "sk-own"},
		PinnedAPIKeys: map[Provider]string{ProviderMoonshot: "sk-pinned", ProviderZAI: "sk-zai-pinned"},
	}
	if got := c.APIKeyForRoute(ProviderMoonshot); got != "sk-own" {
		t.Errorf("APIKeyForRoute(moonshot) = %q, want the run's own key", got)
	}
	if got := c.APIKeyForRoute(ProviderZAI); got != "sk-zai-pinned" {
		t.Errorf("APIKeyForRoute(zai) = %q, want the pinned key", got)
	}
	// APIKey stays the default-precedence answer: it must NOT see a pinned
	// key, or every unpinned route would reach it.
	if got := c.APIKey(ProviderZAI); got != "" {
		t.Errorf("APIKey(zai) = %q — the default precedence must not see a pinned key", got)
	}
	if !c.IsPinnedSlot(string(ProviderZAI)) || c.IsPinnedSlot(string(ProviderMoonshot)) {
		t.Errorf("IsPinnedSlot: zai=%v moonshot=%v, want true/false", c.IsPinnedSlot(string(ProviderZAI)), c.IsPinnedSlot(string(ProviderMoonshot)))
	}
}
