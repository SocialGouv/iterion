package secrets

import "testing"

// The Layer 2 placeholder (ADR-162 decision 7): a pure function of the value
// — the arm that writes the spawn env and the engine that arms the proxy's
// rewriter derive it independently, so determinism IS the contract. Shape
// matters as much: the token wears the `__ITERION_SECRET_…__` marker every
// downstream scrubber already recognises, so a placeholder that leaks into a
// log or a payload is redacted by the existing rules rather than shipped.
func TestCredentialPlaceholderIsThePureCrossingToken(t *testing.T) {
	key := "sk-ant-the-run-key-witness"
	first, second := CredentialPlaceholder(key), CredentialPlaceholder(key)
	if first == "" || first != second {
		t.Fatalf("placeholder = %q then %q, want a stable non-empty token — the spawn env and the proxy rewriter must derive the same one", first, second)
	}
	if other := CredentialPlaceholder(key + "-x"); other == first {
		t.Fatalf("distinct values %q and %q share the placeholder %q — two run keys would cross as one", key, key+"-x", first)
	}
	if !IsCredentialPlaceholder(first) {
		t.Fatalf("IsCredentialPlaceholder(%q) = false, want true — the reader-side guard rejects the arm's own token", first)
	}
	if got := CredentialPlaceholder(""); got != "" {
		t.Fatalf("the empty value's placeholder = %q, want \"\"", got)
	}
	// The guard's own floor: a value too short to be a credential answers ""
	// so no caller arms a crossing — or a DLP match — on a common word.
	for _, short := range []string{"a", "sk-run", "sevench"} {
		if got := CredentialPlaceholder(short); got != "" {
			t.Fatalf("placeholder(%q) = %q, want \"\" — too short to carry", short, got)
		}
		if IsCredentialPlaceholder(short) {
			t.Fatalf("IsCredentialPlaceholder(%q) = true, want false", short)
		}
	}
}

// The egress table (ADR-162 decision 7): the hosts a credential may be
// substituted toward are the credential's OWN endpoints — and a provider
// whose credential rides an SDK signing chain names none, which is the arm's
// cue to refuse the crossing typed rather than hand the container a token
// that resolves nowhere.
func TestCredentialEgressHostsNameTheCredentialsOwnEndpoints(t *testing.T) {
	for _, row := range []struct {
		provider Provider
		baseURL  string
		want     string
	}{
		{ProviderAnthropic, "", "api.anthropic.com"},
		{ProviderOpenAI, "", "api.openai.com"},
		{ProviderOpenRouter, "", "openrouter.ai"},
		{ProviderZAI, "", "api.z.ai"},
		{ProviderMoonshot, "", "api.moonshot.ai"},
		// A run-held base URL is the run's own routing of its own key: the
		// credential travels there too, beside the vendor's endpoint.
		{ProviderAnthropic, "https://llm-gw.example.com", "llm-gw.example.com"},
		{ProviderOpenAICompatible, "https://gw.internal:8443", "gw.internal"},
	} {
		hosts := CredentialEgressHosts(row.provider, row.baseURL)
		found := false
		for _, h := range hosts {
			if h == row.want {
				found = true
			}
		}
		if !found {
			t.Errorf("CredentialEgressHosts(%s, %q) = %v, want %s among them", row.provider, row.baseURL, hosts, row.want)
		}
	}
	// A gateway pair with no URL seals nothing and names no host.
	if hosts := CredentialEgressHosts(ProviderOpenAICompatible, ""); len(hosts) != 0 {
		t.Errorf("a pair with no URL names %v, want none", hosts)
	}
	// bedrock/vertex/azure: the signing-chain providers the proxy cannot
	// substitute — no host, the arm's typed refusal.
	for _, provider := range []Provider{ProviderBedrock, ProviderVertex, ProviderAzure} {
		if hosts := CredentialEgressHosts(provider, ""); len(hosts) != 0 {
			t.Errorf("CredentialEgressHosts(%s) = %v, want none — an SDK signing chain cannot cross as a placeholder", provider, hosts)
		}
	}
}
