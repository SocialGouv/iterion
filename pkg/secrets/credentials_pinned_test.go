package secrets

import (
	"errors"
	"testing"
)

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

// A gateway credential is read as ONE pair from ONE map: the run's own
// key with its own URL first, the pinned key with the pinned URL next —
// never a key from one map with the URL of the other. A key met without
// its URL is a typed error, not a key to send to the environment's
// gateway; a vendor key needs no URL.
func TestGatewayCredentialForRoute_PairsKeyAndURLFromOneMap(t *testing.T) {
	gw := ProviderOpenAICompatible
	c := Credentials{
		APIKeys: map[Provider]string{gw: "sk-own", ProviderAnthropic: "sk-ant"}, APIBaseURLs: map[Provider]string{gw: "https://own.example.test"},
		PinnedAPIKeys: map[Provider]string{gw: "sk-pinned"}, PinnedAPIBaseURLs: map[Provider]string{gw: "https://pinned.example.test"},
	}
	if k, u, err := c.GatewayCredentialForRoute(gw); err != nil || k != "sk-own" || u != "https://own.example.test" {
		t.Fatalf("own pair = %q %q %v", k, u, err)
	}
	delete(c.APIKeys, gw)
	if k, u, err := c.GatewayCredentialForRoute(gw); err != nil || k != "sk-pinned" || u != "https://pinned.example.test" {
		t.Fatalf("pinned pair = %q %q %v", k, u, err)
	}
	// The own URL survives in APIBaseURLs but the own key is gone: the
	// pinned key must not be paired with it.
	c.APIBaseURLs[gw] = "https://stale-own.example.test"
	if _, u, _ := c.GatewayCredentialForRoute(gw); u != "https://pinned.example.test" {
		t.Fatalf("the pinned key was paired with another map's URL: %q", u)
	}
	delete(c.PinnedAPIBaseURLs, gw)
	if _, _, err := c.GatewayCredentialForRoute(gw); !errors.Is(err, ErrGatewayKeyWithoutURL) {
		t.Fatalf("a key without its URL = %v, want ErrGatewayKeyWithoutURL", err)
	}
	c.APIBaseURLs[ProviderAnthropic] = "https://stray.example.test"
	if k, u, err := c.GatewayCredentialForRoute(ProviderAnthropic); err != nil || k != "sk-ant" || u != "" {
		t.Fatalf("a vendor key never carries a URL, even a stray one: %q %q %v", k, u, err)
	}
	if k, u, err := (Credentials{}).GatewayCredentialForRoute(gw); err != nil || k != "" || u != "" {
		t.Fatalf("no key is no error: %q %q %v", k, u, err)
	}
}
