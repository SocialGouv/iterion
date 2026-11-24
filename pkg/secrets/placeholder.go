package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// The credential placeholder (ADR-162 decision 7): an untrusted run's
// container holds its LLM credential as a Layer 2 placeholder, never the
// value, and the sandbox proxy substitutes the placeholder for the real key
// in headers toward that credential's own host. Both sides — the arm that
// writes the container env and the engine that arms the proxy's rewriter —
// hold the sealed value and nothing else about each other, so the token is a
// PURE function of the value: no coordination, no registry, and two
// derivations of the same key can never disagree.

// credentialPlaceholderPrefix keeps the `__ITERION_SECRET_…__` shape every
// downstream scrubber already recognises (pkg/errtrack, pkg/knowledge, the
// secretguard's redaction family), so a placeholder that leaks into a log or
// a model payload is redacted by the existing rules rather than shipped.
const credentialPlaceholderPrefix = "__ITERION_SECRET_llm_"

// CredentialPlaceholder returns the deterministic Layer 2 placeholder for a
// credential value: sha256-derived, prefixed, closed by the secret-marker
// shape. Distinct values give distinct tokens with overwhelming
// probability; the same value always gives the same one, which is the whole
// contract — the spawn env and the proxy rewriter derive it independently.
// The empty value has no placeholder ("" answers ""); a value shorter than
// the guard's own floor is not a credential this scheme carries, and answers
// "" so no caller arms a crossing on it.
func CredentialPlaceholder(value string) string {
	if len(value) < 8 {
		return ""
	}
	sum := sha256.Sum256([]byte(credentialPlaceholderPrefix + value))
	return credentialPlaceholderPrefix + hex.EncodeToString(sum[:8])[:16] + "__"
}

// IsCredentialPlaceholder reports whether s is a token CredentialPlaceholder
// could have produced — the reader-side guard for code that wants to assert
// a value crossed as a placeholder and not raw.
func IsCredentialPlaceholder(s string) bool {
	return strings.HasPrefix(s, credentialPlaceholderPrefix) && strings.HasSuffix(s, "__") && len(s) == len(credentialPlaceholderPrefix)+18
}

// CredentialEgressHosts returns the hosts a credential of provider may be
// substituted toward — the credential's own endpoints, the proxy's whole
// allowlist for an untrusted run (ADR-162 decision 7). baseURL, when
// non-empty, is the run-held base URL paired with the key
// (Credentials.APIBaseURLs / the sealed gateway pair): its host is the
// credential's destination too. An empty answer means the provider's
// credential cannot cross as a placeholder at all — an AWS-style or
// service-account document rides an SDK signing chain the proxy cannot
// substitute, and the arm refuses it typed instead of handing the container
// a token that resolves nowhere.
func CredentialEgressHosts(provider Provider, baseURL string) []string {
	var hosts []string
	switch provider {
	case ProviderAnthropic:
		hosts = []string{"api.anthropic.com"}
	case ProviderOpenAI:
		hosts = []string{"api.openai.com"}
	case ProviderOpenRouter:
		hosts = []string{"openrouter.ai"}
	case ProviderXAI:
		hosts = []string{hostOf(XAIDefaultBaseURL)}
	case ProviderZAI, ProviderMoonshot:
		// Every endpoint the vendor publishes for the wire: the proxy judges
		// the ensemble, never one spelling of the vendor (FacadeVendorEndpoints).
		for _, endpoint := range FacadeVendorEndpoints(provider) {
			if h := hostOf(endpoint); h != "" {
				hosts = append(hosts, h)
			}
		}
	case ProviderOpenAICompatible:
		// The gateway the run's own pair names — there is no published
		// default; a pair with no URL is not a pair and seals nothing.
		if h := hostOf(baseURL); h != "" {
			hosts = []string{h}
		}
	default:
		// bedrock, vertex, azure, and any provider yet unnamed: no host this
		// package can vouch for.
		return nil
	}
	if h := hostOf(baseURL); h != "" && !containsHost(hosts, h) {
		// A run-held base URL for a known provider (an openai_compatible
		// front to it, a regional endpoint) is the run's own routing of its
		// own key: the credential travels there too.
		hosts = append(hosts, h)
	}
	return hosts
}

// hostOf reads a base URL's host, lower-cased, port-free. Unparseable or
// schemeless garbage answers "" — never a host a policy could be built on.
func hostOf(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if h == host {
			return true
		}
	}
	return false
}
